package mta

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/buildrun"
	"github.com/dyammarcano/anamnesis/internal/config"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/proc"
)

// Run states for KindMTARun Values["state"].
const (
	StateSucceeded = "SUCCEEDED"
	StateFailed    = "FAILED"
	StateNotRun    = "NOT_RUN"
)

// Routes for KindMTARun Values["route"].
const (
	RouteSource       = "source"
	RouteBinary       = "binary"
	RouteSourceForced = "source-forced" // run although the failure was predicted, by parameter
	RouteSyntheticPOM = "synthetic-pom" // staged copy reshaped with a generated pom.xml (no Maven/Gradle build)
)

const (
	maxLocations   = 50
	analysisLogMax = 256 * 1024
)

// DefaultTargets are used when mta.targets is empty.
var DefaultTargets = []string{"openjdk17", "eap8", "jakarta-ee"}

// Analyzers returns the MTA analyzer.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

type analyzer struct{}

func (analyzer) Name() string                  { return "mta" }
func (analyzer) Phase() engine.Phase           { return engine.Deep }
func (analyzer) Requires() []engine.Capability { return []engine.Capability{engine.SideEffectMTA} }

// Timeout gives the engine mta.timeout_minutes from parameters.yaml (engine.Timeouter).
func (analyzer) Timeout(p *engine.Project) time.Duration {
	if p.Params == nil {
		return 0
	}
	return time.Duration(p.Params.MTA.TimeoutMinutes) * time.Minute
}

// run collects what the single KindMTARun evidence item reports.
type run struct {
	state       string
	route       string
	class       string
	line        string
	finding     string
	duration    time.Duration
	cmd         *model.CommandRecord
	artifacts   []string
	values      map[string]string
	assumptions []string
	limitations []string
}

func (a analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	var cfg config.MTA
	if p.Params != nil {
		cfg = p.Params.MTA
	}
	emit := func(e model.Evidence) {
		e.Analyzer = "mta"
		e.Category = model.CatMigration
		sink.Emit(e)
	}

	emitProfiles(p, emit)

	inst, reason := Locate(ctx, cfg)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if reason != "" {
		cls := FailMTANotInstalled
		if cfg.InstallDir != "" {
			cls = FailKantraDirIncomplete
			if !isDir(cfg.InstallDir) || MissingParts(cfg.InstallDir) == nil {
				cls = FailMTANotInstalled
			}
		}
		emitRun(emit, run{state: StateNotRun, class: cls, finding: "MTA was not run: " + reason})
		return nil
	}

	emitToolConfig(p, cfg, emit)

	// Prediction: kantra 8.3.0 only recognises a root pom.xml or build.gradle (kantra-internals 5.2).
	applicable := p.Files.HasRoot("pom.xml") || p.Files.HasRoot("build.gradle")
	predicted := ""
	if !applicable {
		predicted = "unable to get build tool: the project root has no pom.xml and no build.gradle"
		if p.Files.HasRoot("build.gradle.kts") {
			predicted += " (build.gradle.kts alone is not detected by the Java provider)"
		}
	}

	r := run{values: map[string]string{
		"mta_version": inst.Version, "mta_dir_source": "parameters",
		"java_provider": map[bool]string{true: "APPLICABLE", false: "NOT_APPLICABLE"}[applicable],
	}}
	if predicted != "" {
		r.values["predicted_error"] = predicted
	}

	var input string
	switch {
	case applicable:
		r.route, input = RouteSource, p.Root
	case !cfg.DisableSyntheticPOM && p.Files.JavaFiles > 0:
		// No pom.xml/build.gradle: reshape a staged copy so the Java provider can start (H-1).
		r.route = RouteSyntheticPOM
		r.values["java_provider"] = "APPLICABLE_WITH_SYNTHETIC_POM"
	default:
		if arch, ok := pickArchive(p); ok {
			copied, err := copyArchive(p, arch.Rel)
			if err != nil {
				r.state, r.class = StateNotRun, FailUnknown
				r.finding = "MTA was not run: could not copy " + arch.Rel + " to the run directory: " + err.Error()
				r.values["binary_candidate"] = arch.Rel
				emitRun(emit, r)
				a.emitCoverage(ctx, inst, Providers{}, emit)
				return nil
			}
			r.route, input = RouteBinary, copied
			r.values["binary_source"] = arch.Rel
			r.assumptions = append(r.assumptions,
				"binary route: a copy of "+arch.Rel+" was analysed so MTA's java-project/ output stays outside the repository; incident paths are decompiled artifact paths, not repository paths")
		} else if cfg.ForceWhenPredictedToFail {
			r.route, input = RouteSourceForced, p.Root
			r.assumptions = append(r.assumptions,
				"mta.force_when_predicted_to_fail is set: MTA was run on the repository root although "+predicted)
		} else {
			r.state, r.class, r.line = StateNotRun, FailJavaNoBuildTool, predicted
			r.finding = "MTA was not run: predicted failure '" + predicted + "'; no .war/.ear found to use as binary input. " +
				"Set mta.force_when_predicted_to_fail to run it anyway and capture the real failure"
			emitRun(emit, r)
			a.emitCoverage(ctx, inst, Providers{}, emit)
			return nil
		}
	}

	// Source routes never hand the analyzed repository to MTA: its Java provider runs mvn or the
	// project's gradlew inside the input, and the analyzer writes under it. Analyze a staged copy.
	var stagedDir string
	var relocated map[string]string
	if r.route == RouteSource || r.route == RouteSourceForced || r.route == RouteSyntheticPOM {
		stagedDir = filepath.Join(p.RunDir, "mta-src", p.Name)
		if err := buildrun.Stage(ctx, p.Root, stagedDir); err != nil {
			r.state, r.class = StateNotRun, FailUnknown
			r.finding = "MTA was not run: could not stage a copy of the project for analysis: " + err.Error()
			emitRun(emit, r)
			return nil
		}
		input = stagedDir
		r.values["staged_copy"] = filepath.ToSlash(filepath.Join("mta-src", p.Name))
		r.assumptions = append(r.assumptions,
			"MTA analysed a staged copy of the repository (VCS metadata excluded) so build tools it runs cannot modify the repository; incident paths are mapped back to repository paths")
		if r.route == RouteSyntheticPOM {
			rel, info, err := synthesizePOM(ctx, p, stagedDir)
			if err != nil {
				r.state, r.class = StateNotRun, FailUnknown
				r.finding = "MTA was not run: the synthetic-POM copy could not be prepared: " + err.Error()
				emitRun(emit, r)
				return nil
			}
			relocated = rel
			r.values["synthetic_pom"] = "pom.xml generated in the staged copy (see artifacts)"
			r.values["java_files"] = strconv.Itoa(info.JavaFiles)
			r.values["java_files_relocated"] = strconv.Itoa(info.Moved)
			r.values["java_collisions"] = strconv.Itoa(info.Collisions)
			r.values["source_roots"] = strings.Join(limitStrings(info.Roots, 20), ",")
			r.values["system_jars"] = strconv.Itoa(info.Jars)
			r.values["synthetic_source_level"] = SyntheticSourceLevel
			r.assumptions = append(r.assumptions,
				"no Maven/Gradle build exists, so MTA ran on a staged copy reshaped with a generated pom.xml: .java files moved to src/main/java by package, project jars declared as system dependencies",
				"the synthetic pom declares Java "+SyntheticSourceLevel+" because JDT no longer accepts lower compliance levels; Java 5-7 source parses as Java 8",
				"results reflect the code MTA could resolve with the project's own jars; classes from the application server or the original build classpath are unresolved")
			if info.Collisions > 0 {
				r.limitations = append(r.limitations, fmt.Sprintf("%d .java file(s) duplicate a package+class already placed and were not analysed as Java (builtin rules still see them)", info.Collisions))
			}
			if b, err := os.ReadFile(filepath.Join(stagedDir, "pom.xml")); err == nil {
				capDir := p.CapabilityDir("mta")
				if os.MkdirAll(capDir, 0o755) == nil && os.WriteFile(filepath.Join(capDir, "synthetic-pom.xml"), b, 0o644) == nil {
					r.artifacts = append(r.artifacts, filepath.ToSlash(filepath.Join("capabilities", "mta", "synthetic-pom.xml")))
				}
			}
		}
	}

	targets := cfg.Targets
	if len(targets) == 0 {
		targets = DefaultTargets
	}
	r.values["mode"] = cfg.Mode
	r.values["targets"] = strings.Join(targets, ",")

	timeout := time.Duration(cfg.TimeoutMinutes) * time.Minute
	if timeout <= 0 {
		timeout = 60 * time.Minute
	}

	capDir := p.CapabilityDir("mta")
	scratch := filepath.Join(p.RunDir, "mta-scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		r.state, r.class = StateNotRun, FailUnknown
		r.finding = "MTA was not run: cannot create scratch directory: " + err.Error()
		emitRun(emit, r)
		return nil
	}
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		r.state, r.class = StateNotRun, FailUnknown
		r.finding = "MTA was not run: cannot create capability directory: " + err.Error()
		emitRun(emit, r)
		return nil
	}

	env := childEnv(p, cfg, inst)
	res, outDir := invoke(ctx, p, inst, env, scratch, capDir, input, targets, cfg.Mode, timeout, "analyze")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	text := failureText(res, outDir)
	if res.Record.ExitCode != 0 && len(targets) > 0 && strings.Contains(strings.ToLower(text), "unknown target") {
		_, line := firstLineContaining(text, "unknown target")
		r.values["retried_without_target"] = "true"
		r.values["unknown_target_message"] = line
		r.assumptions = append(r.assumptions, "MTA rejected a target ("+line+"); the run was repeated once without --target, so all rulesets were eligible")
		r.values["targets"] = ""
		res, outDir = invoke(ctx, p, inst, env, scratch, capDir, input, nil, cfg.Mode, timeout, "analyze-retry")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		text = failureText(res, outDir)
	}

	rec := res.Record
	r.cmd = &rec
	r.duration = time.Duration(rec.DurationMs) * time.Millisecond
	r.values["exit_code"] = strconv.Itoa(rec.ExitCode)
	r.artifacts = collectArtifacts(p, outDir, rec)

	outYaml := filepath.Join(outDir, "output.yaml")
	haveOutput := isFile(outYaml)
	ok := rec.ExitCode == 0 && rec.Err == "" && !rec.TimedOut && haveOutput

	switch {
	case ok:
		r.state = StateSucceeded
	default:
		r.state = StateFailed
		switch {
		case rec.Err != "" && strings.Contains(strings.ToLower(rec.Err), "not found"):
			r.class, r.line = FailMTANotInstalled, rec.Err
		case rec.TimedOut:
			r.class = FailTimeout
			r.line = "timed out after " + timeout.String()
		default:
			r.class, r.line = ClassifyFailure(text)
			if r.class == FailUnknown && rec.ExitCode == 0 && !haveOutput {
				r.line = "exit code 0 but no output.yaml was written"
			}
		}
	}

	provs := Providers{}
	if ok {
		provs = Providers{Java: true, Builtin: true}
	}

	var rulesets []RuleSet
	if haveOutput {
		var err error
		rulesets, err = ParseOutputFile(outYaml)
		if err != nil {
			r.limitations = append(r.limitations, "output.yaml could not be fully parsed: "+err.Error())
			if r.state == StateSucceeded {
				r.state, r.class, r.line = StateFailed, FailUnknown, err.Error()
			}
		}
		if r.state != StateSucceeded {
			r.limitations = append(r.limitations, "MTA did not finish cleanly; output.yaml, where present, may be incomplete")
		}
	}

	mapper := newPathMapper(p)
	if stagedDir != "" {
		mapper.staged = strings.TrimRight(filepath.ToSlash(stagedDir), "/")
		mapper.relocated = relocated
	}
	results, stats := emitResults(emit, rulesets, mapper)
	rep := Aggregate(results)
	maps.Copy(r.values, stats)
	if haveOutput {
		r.values["effort_points_derived"] = strconv.Itoa(rep.TotalPoints)
		r.values["effort_label"] = rep.Label
	}

	switch r.state {
	case StateSucceeded:
		r.finding = fmt.Sprintf("MTA analysed the project via the %s route in %s: %s violation rule(s), %s insight(s), %s rule error(s)",
			r.route, r.duration.Round(time.Second), r.values["violation_rules"], r.values["insight_rules"], r.values["rule_errors"])
	default:
		r.finding = fmt.Sprintf("MTA run failed (%s): %s", r.class, r.line)
		if haveOutput {
			r.finding += "; partial output.yaml was read"
		}
	}
	emitRun(emit, r)
	a.emitCoverage(ctx, inst, provs, emit)
	return nil
}

func emitRun(emit func(model.Evidence), r run) {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values["state"] = r.state
	if r.route != "" {
		r.values["route"] = r.route
	}
	r.values["duration_ms"] = strconv.FormatInt(r.duration.Milliseconds(), 10)
	if r.class != "" {
		r.values["failure_class"] = r.class
	}
	if r.line != "" {
		r.values["failure_line"] = r.line
	}
	for k, v := range r.values {
		if v == "" {
			delete(r.values, k)
		}
	}
	ev := model.Evidence{
		Kind: model.KindMTARun, Subject: "mta", Finding: r.finding, Confidence: model.Observed,
		Command: r.cmd, Artifacts: r.artifacts, Values: r.values,
		Assumptions: r.assumptions, Limitations: r.limitations,
	}
	switch r.state {
	case StateSucceeded:
		ev.Status = model.Pass
	case StateFailed:
		ev.Status, ev.Severity = model.Fail, model.SevMedium
	default:
		ev.Status, ev.Severity = model.Skipped, model.SevLow
		if r.class == FailMTANotInstalled || r.class == FailKantraDirIncomplete {
			ev.Status = model.Fail
		}
	}
	emit(ev)
}

func (analyzer) emitCoverage(ctx context.Context, inst Install, ran Providers, emit func(model.Evidence)) {
	st, err := ScanRulesets(ctx, inst.Dir)
	emit(CoverageEvidence(st, err, ran))
}

// emitProfiles records a repo-carried .konveyor/profiles directory: MTA auto-loads it and it can
// change input, mode, rules and label selector (kantra-internals section 9).
func emitProfiles(p *engine.Project, emit func(model.Evidence)) {
	files := p.Files.Under(".konveyor/profiles")
	if len(files) == 0 {
		return
	}
	var locs []model.Location
	for i, f := range files {
		if i == maxLocations {
			break
		}
		locs = append(locs, model.Location{Path: f.Rel})
	}
	emit(model.Evidence{
		Kind: model.KindMTAPrereq, Subject: ".konveyor/profiles", Status: model.Warn, Confidence: model.Observed,
		Severity:    model.SevMedium,
		Finding:     fmt.Sprintf("the repository carries %d MTA profile file(s) under .konveyor/profiles; MTA auto-loads them and they can change the analysis settings", len(files)),
		Locations:   locs,
		Values:      map[string]string{"state": "PRESENT", "detail": strconv.Itoa(len(files)) + " file(s)"},
		Limitations: []string{"Anamnesis did not inspect what the profile changes; the effective MTA settings may differ from the parameters"},
	})
}

func emitToolConfig(p *engine.Project, cfg config.MTA, emit func(model.Evidence)) {
	if cfg.JDK == "" {
		emit(model.Evidence{
			Kind: model.KindMTAPrereq, Subject: "mta.jdk", Status: model.Warn, Confidence: model.Observed,
			Finding: "mta.jdk is not set: JAVA_HOME is not passed to MTA, which needs JDK 17+ (JDT LS)",
			Values:  map[string]string{"state": "NOT_CONFIGURED", "detail": "set mta.jdk to a jdks[].name"},
		})
	}
	if p.Params != nil && p.Params.Tool("mvn") == "" {
		emit(model.Evidence{
			Kind: model.KindMTAPrereq, Subject: "mvn", Status: model.Info, Confidence: model.Observed,
			Finding: "tools.mvn is not set: MTA's Java provider requires mvn and will use whatever is on the inherited PATH",
			Values:  map[string]string{"state": "NOT_CONFIGURED", "detail": "MTA 8.3.0 checks mvn for every run that uses the Java provider"},
		})
	}
}

// childEnv builds the explicit environment additions for the MTA child. Only names reach evidence.
func childEnv(p *engine.Project, cfg config.MTA, inst Install) []string {
	env := []string{"KANTRA_DIR=" + inst.Dir}
	var prepend []string
	if p.Params != nil {
		if home := p.Params.JDKHome(cfg.JDK); cfg.JDK != "" && home != "" {
			env = append(env, "JAVA_HOME="+home)
			prepend = append(prepend, filepath.Join(home, "bin"))
		}
		if mvn := p.Params.Tool("mvn"); mvn != "" {
			prepend = append(prepend, filepath.Dir(mvn))
		}
	}
	if len(prepend) > 0 {
		sep := string(os.PathListSeparator)
		env = append(env, "PATH="+strings.Join(prepend, sep)+sep+os.Getenv("PATH"))
	}
	if cfg.JvmMaxMem != "" {
		env = append(env, "JVM_MAX_MEM="+cfg.JvmMaxMem)
	}
	return env
}

// invoke runs one MTA analysis into a fresh output directory (never --overwrite).
func invoke(ctx context.Context, p *engine.Project, inst Install, env []string, scratch, capDir, input string,
	targets []string, mode string, timeout time.Duration, logName string) (proc.Result, string) {
	outDir := freshDir(filepath.Join(p.RunDir, "mta-output"))
	args := []string{"analyze", "--input", input, "--output", outDir}
	for _, t := range targets {
		args = append(args, "--target", t)
	}
	args = append(args, "--mode", mode, "--run-local=true", "--disable-maven-search", "--no-progress")
	res, _ := proc.Run(ctx, proc.Spec{
		Name: inst.Exe, Args: args, Dir: scratch, Timeout: timeout, Env: env,
		LogDir: capDir, LogName: logName, RunDir: p.RunDir,
	})
	return res, outDir
}

// freshDir returns base, or base-2, base-3 ... whichever does not exist yet.
func freshDir(base string) string {
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for i := 2; ; i++ {
		c := base + "-" + strconv.Itoa(i)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// failureText joins stderr, stdout and the tail of analysis.log for classification.
func failureText(res proc.Result, outDir string) string {
	return res.Stderr + "\n" + res.Stdout + "\n" + readTail(filepath.Join(outDir, "analysis.log"), analysisLogMax)
}

func readTail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.Size() > max {
		_, _ = f.Seek(st.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

func firstLineContaining(text, sub string) (int, string) {
	for i, l := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(l), sub) {
			return i, boundLine(l)
		}
	}
	return -1, ""
}

func collectArtifacts(p *engine.Project, outDir string, rec model.CommandRecord) []string {
	rel := func(abs string) string {
		r, err := filepath.Rel(p.RunDir, abs)
		if err != nil {
			return filepath.ToSlash(abs)
		}
		return filepath.ToSlash(r)
	}
	var out []string
	for _, n := range []string{"output.yaml", "dependencies.yaml", "analysis.log"} {
		if isFile(filepath.Join(outDir, n)) {
			out = append(out, rel(filepath.Join(outDir, n)))
		}
	}
	if isDir(filepath.Join(outDir, "static-report")) {
		out = append(out, rel(filepath.Join(outDir, "static-report")))
	}
	if rec.LogRef != "" {
		out = append(out, rec.LogRef)
		if before, ok := strings.CutSuffix(rec.LogRef, ".stdout.log"); ok {
			errRef := before + ".stderr.log"
			if isFile(filepath.Join(p.RunDir, filepath.FromSlash(errRef))) {
				out = append(out, errRef)
			}
		}
	}
	return out
}

// pickArchive chooses the .war/.ear to analyse: largest at the repo top level or under dist/,
// otherwise the largest anywhere.
func pickArchive(p *engine.Project) (best struct {
	Rel  string
	Size int64
}, ok bool) {
	bestTier := 2
	consider := func(rel string, size int64) {
		if size <= 0 {
			return
		}
		tier := 1
		if !strings.Contains(rel, "/") || strings.HasPrefix(rel, "dist/") {
			tier = 0
		}
		if tier < bestTier || (tier == bestTier && (size > best.Size || (size == best.Size && rel < best.Rel))) {
			bestTier, best.Rel, best.Size, ok = tier, rel, size, true
		}
	}
	for _, f := range p.Files.WithExt(".war") {
		consider(f.Rel, f.Size)
	}
	for _, f := range p.Files.WithExt(".ear") {
		consider(f.Rel, f.Size)
	}
	return best, ok
}

// copyArchive copies the in-repo archive to <run>/mta-input/ and returns the copy's path.
func copyArchive(p *engine.Project, rel string) (string, error) {
	dstDir := filepath.Join(p.RunDir, "mta-input")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(filepath.Join(p.Root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	dst := filepath.Join(dstDir, path.Base(rel))
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return "", err
	}
	return dst, out.Close()
}

// ---- result mapping -------------------------------------------------------------------------

const (
	spaceRepo     = "repo"
	spaceArtifact = "artifact"
	spaceExternal = "external"
)

type pathMapper struct {
	repo, runDir string
	staged       string            // staged copy of the repository analysed by MTA; maps back to repo paths
	relocated    map[string]string // synthetic-POM route: staged path -> original repo path
	fold         bool
}

func newPathMapper(p *engine.Project) pathMapper {
	return pathMapper{
		repo:   strings.TrimRight(filepath.ToSlash(p.Root), "/"),
		runDir: strings.TrimRight(filepath.ToSlash(p.RunDir), "/"),
		fold:   runtime.GOOS == "windows",
	}
}

func (m pathMapper) cut(s, prefix string) (string, bool) {
	prefix += "/"
	if len(s) < len(prefix) {
		return "", false
	}
	head := s[:len(prefix)]
	if head == prefix || (m.fold && strings.EqualFold(head, prefix)) {
		return s[len(prefix):], true
	}
	return "", false
}

// mapURI turns an incident URI into a repo-relative path where possible. Paths under the run
// directory (decompiled binary input, copies) are returned relative to it as artifact paths.
func (m pathMapper) mapURI(uri string) (p, space string) {
	raw, ok := URIToPath(uri)
	if !ok {
		return "", ""
	}
	raw = path.Clean(raw)
	if m.staged != "" {
		if rel, ok := m.cut(raw, m.staged); ok {
			if orig, moved := m.relocated[rel]; moved {
				return orig, spaceRepo
			}
			return rel, spaceRepo
		}
	}
	if rel, ok := m.cut(raw, m.repo); ok {
		return rel, spaceRepo
	}
	if rel, ok := m.cut(raw, m.runDir); ok {
		return rel, spaceArtifact
	}
	if !path.IsAbs(raw) && (len(raw) <= 1 || raw[1] != ':') {
		return strings.TrimPrefix(raw, "./"), spaceRepo
	}
	return raw, spaceExternal
}

func targetsOf(labels []string) []string {
	const pre = "konveyor.io/target="
	var out []string
	for _, l := range labels {
		if after, ok := strings.CutPrefix(l, pre); ok {
			out = append(out, after)
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

type mappedIncidents struct {
	refs      []IncidentRef
	locs      []model.Location
	files     int
	repoN     int
	otherN    int
	truncated bool
}

func mapIncidents(m pathMapper, incs []Incident) mappedIncidents {
	var mi mappedIncidents
	type key struct {
		p string
		l int
	}
	seen := map[key]bool{}
	files := map[string]bool{}
	for _, inc := range incs {
		pth, space := m.mapURI(inc.URI)
		line := 0
		if inc.LineNumber != nil {
			line = *inc.LineNumber
		}
		mi.refs = append(mi.refs, IncidentRef{Path: pth, Line: line})
		if pth == "" {
			continue
		}
		files[pth] = true
		if space == spaceRepo {
			mi.repoN++
		} else {
			mi.otherN++
		}
		if k := (key{pth, line}); !seen[k] {
			seen[k] = true
			mi.locs = append(mi.locs, model.Location{Path: pth, Line: line})
		}
	}
	mi.files = len(files)
	sort.Slice(mi.locs, func(i, j int) bool {
		if mi.locs[i].Path != mi.locs[j].Path {
			return mi.locs[i].Path < mi.locs[j].Path
		}
		return mi.locs[i].Line < mi.locs[j].Line
	})
	if len(mi.locs) > maxLocations {
		mi.locs, mi.truncated = mi.locs[:maxLocations], true
	}
	return mi
}

func (mi mappedIncidents) space() string {
	switch {
	case mi.otherN == 0:
		return spaceRepo
	case mi.repoN == 0:
		return spaceArtifact
	}
	return "mixed"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func severityOf(category string) model.Severity {
	switch strings.ToLower(category) {
	case "mandatory":
		return model.SevMedium
	case "potential":
		return model.SevLow
	}
	return model.SevInfo
}

// emitResults emits violations, insights and rule errors, and returns the full RuleResults (all
// incidents, for Aggregate) plus counters for the run evidence.
func emitResults(emit func(model.Evidence), sets []RuleSet, m pathMapper) ([]RuleResult, map[string]string) {
	var results []RuleResult
	nViol, nIns, nErr, nInc, nSkipped, nUnmatched := 0, 0, 0, 0, 0, 0
	for _, rs := range sets {
		nSkipped += len(rs.Skipped)
		nUnmatched += len(rs.Unmatched)
		for _, id := range sortedKeys(rs.Violations) {
			v := rs.Violations[id]
			mi := mapIncidents(m, v.Incidents)
			nViol++
			nInc += len(v.Incidents)
			effort := 0
			if v.Effort != nil {
				effort = *v.Effort
			}
			results = append(results, RuleResult{
				RuleSet: rs.Name, RuleID: id, Category: v.Category, Labels: v.Labels,
				Targets: targetsOf(v.Labels), Effort: effort, Incidents: mi.refs,
			})
			emit(violationEvidence(rs.Name, id, v, mi, model.KindMTAViolation))
		}
		for _, id := range sortedKeys(rs.Insights) {
			v := rs.Insights[id]
			mi := mapIncidents(m, v.Incidents)
			nIns++
			ev := violationEvidence(rs.Name, id, v, mi, model.KindMTAInsight)
			ev.Effort = nil
			ev.Status, ev.Severity = model.Info, model.SevInfo
			emit(ev)
		}
		for _, id := range sortedKeys(rs.Errors) {
			nErr++
			emit(model.Evidence{
				Kind: model.KindMTARuleError, Subject: rs.Name + "/" + id, RuleID: id,
				Finding: "MTA could not evaluate rule " + id + ": " + firstLine(rs.Errors[id]),
				Status:  model.Warn, Confidence: model.Observed, Severity: model.SevLow,
				Values:      map[string]string{"ruleset": rs.Name},
				Limitations: []string{"the rule's results are absent from this assessment"},
			})
		}
	}
	return results, map[string]string{
		"violation_rules": strconv.Itoa(nViol), "insight_rules": strconv.Itoa(nIns),
		"rule_errors": strconv.Itoa(nErr), "incidents_total": strconv.Itoa(nInc),
		"skipped_rules": strconv.Itoa(nSkipped), "unmatched_rules": strconv.Itoa(nUnmatched),
	}
}

func violationEvidence(ruleset, id string, v Violation, mi mappedIncidents, kind string) model.Evidence {
	n := len(v.Incidents)
	desc := firstLine(v.Description)
	if desc == "" {
		desc = id
	}
	vals := map[string]string{
		"category":       v.Category,
		"labels":         strings.Join(v.Labels, ";"),
		"files":          strconv.Itoa(mi.files),
		"ruleset":        ruleset,
		"incidents":      strconv.Itoa(n),
		"location_space": mi.space(),
	}
	if mi.truncated {
		vals["locations_truncated"] = "true"
	}
	ev := model.Evidence{
		Kind: kind, Subject: ruleset + "/" + id, RuleID: id, Target: strings.Join(targetsOf(v.Labels), ","),
		Confidence: model.Observed, Status: model.Warn, Severity: severityOf(v.Category),
		Locations: mi.locs, Values: vals,
	}
	if v.Effort != nil {
		ev.Effort = &model.Effort{Points: *v.Effort, Incidents: n}
		ev.Finding = fmt.Sprintf("%s: %d incident(s) in %d file(s), effort %d per incident", desc, n, mi.files, *v.Effort)
	} else {
		ev.Finding = fmt.Sprintf("%s: %d incident(s) in %d file(s)", desc, n, mi.files)
	}
	if mi.truncated {
		ev.Limitations = append(ev.Limitations, fmt.Sprintf("locations list is limited to %d of the distinct incident locations", maxLocations))
	}
	if mi.otherN > 0 {
		ev.Limitations = append(ev.Limitations, "some incident paths are artifact or external paths (decompiled input or outside the repository), not repository paths")
	}
	return ev
}
