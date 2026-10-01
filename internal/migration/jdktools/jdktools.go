// Package jdktools runs the JDK's own jdeps and jdeprscan over a project's jars and compiled class
// directories. They complement MTA, which reasons over source and decompiled code with rules:
// these two answer which JDK-internal and removed-for-deprecation APIs the compiled artifacts
// really reference. They do not duplicate MTA rules and are not merged with them; correlating by
// API name is left to the report.
package jdktools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/proc"
)

const (
	jdepsBatchSize   = 40
	jdepsTotalBudget = 10 * time.Minute
	jdeprscanMaxJars = 200
	jdeprscanBudget  = 15 * time.Minute
	maxClassDirs     = 200
	maxLocations     = 50
	maxListed        = 20
	minJDKMajor      = 11
)

// Analyzers returns the jdeps and jdeprscan analyzers.
func Analyzers() []engine.Analyzer {
	return []engine.Analyzer{tool{name: "jdk-jdeps"}, tool{name: "jdk-jdeprscan"}}
}

type tool struct{ name string }

func (t tool) Name() string                  { return t.name }
func (t tool) Phase() engine.Phase           { return engine.Deep }
func (t tool) Requires() []engine.Capability { return nil }

func (t tool) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	emit := func(e model.Evidence) {
		e.Analyzer = t.name
		e.Category = model.CatMigration
		e.Assumptions = append(e.Assumptions,
			"complements MTA: reports what the compiled artifacts reference, does not duplicate MTA rules; correlate with MTA findings by API name",
			"the JDK used is the one selected from parameters.yaml (mta.jdk, else the first jdks entry with Java 11+), not the project's own JDK")
		sink.Emit(e)
	}
	exeName := "jdeps"
	if t.name == "jdk-jdeprscan" {
		exeName = "jdeprscan"
	}
	j, reason := findJDK(ctx, p, exeName)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if reason != "" {
		emit(model.Evidence{
			Kind: model.KindPrereqTool, Subject: exeName, Status: model.Warn, Confidence: model.Observed,
			Finding: exeName + " was not run: " + reason,
			Values: map[string]string{
				"requirement": "REQUIRED_FOR_CAPABILITY", "state": "MISSING", "capability": t.name,
				"suggestion": "configure jdks in parameters.yaml with a JDK 11 or newer", "reason": reason,
			},
		})
		return nil
	}
	capDir := p.CapabilityDir(t.name)
	work := filepath.Join(capDir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		emit(model.Evidence{
			Kind: model.KindPrereqTool, Subject: exeName, Status: model.Warn, Confidence: model.Observed,
			Finding: exeName + " was not run: cannot create " + work + ": " + err.Error(),
			Values:  map[string]string{"state": "MISSING", "capability": t.name, "reason": err.Error()},
		})
		return nil
	}
	in := collectInputs(p)
	r := &runner{p: p, j: j, capDir: capDir, work: work, emit: emit, in: in, useMR: true}
	if t.name == "jdk-jdeps" {
		return r.jdeps(ctx)
	}
	return r.jdeprscan(ctx)
}

// ---- JDK selection --------------------------------------------------------------------------

type jdk struct {
	Home    string
	Bin     string // directory holding the tools
	Major   int
	Version string
	Exe     string // absolute path of the requested tool
}

func exeFile(name string) string {
	if filepath.Separator == '\\' {
		return name + ".exe"
	}
	return name
}

// findJDK picks the JDK that has the requested tool: mta.jdk first, then the first jdks entry,
// requiring Java 11 or newer. It consults no environment variable and never searches PATH.
func findJDK(ctx context.Context, p *engine.Project, toolName string) (jdk, string) {
	if p.Params == nil || len(p.Params.JDKs) == 0 {
		return jdk{}, "no JDK is configured; configure jdks in parameters.yaml"
	}
	var homes []string
	if h := p.Params.JDKHome(p.Params.MTA.JDK); p.Params.MTA.JDK != "" && h != "" {
		homes = append(homes, h)
	}
	for _, d := range p.Params.JDKs {
		homes = append(homes, d.Home)
	}
	var notes []string
	seen := map[string]bool{}
	for _, h := range homes {
		if seen[h] {
			continue
		}
		seen[h] = true
		bin := filepath.Join(h, "bin")
		exe := filepath.Join(bin, exeFile(toolName))
		javaExe := filepath.Join(bin, exeFile("java"))
		if st, err := os.Stat(exe); err != nil || st.IsDir() {
			notes = append(notes, h+": no bin/"+exeFile(toolName))
			continue
		}
		major, ver := javaVersion(ctx, javaExe)
		if major < minJDKMajor {
			notes = append(notes, fmt.Sprintf("%s: Java %s is older than %d or its version could not be read", h, orDash(ver), minJDKMajor))
			continue
		}
		return jdk{Home: h, Bin: bin, Major: major, Version: ver, Exe: exe}, ""
	}
	return jdk{}, "no configured JDK provides a usable " + toolName + " (" + strings.Join(notes, "; ") + ")"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var reJavaVersion = regexp.MustCompile(`version "([^"]+)"`)

func javaVersion(ctx context.Context, javaExe string) (int, string) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, javaExe, "-version")
	cmd.Dir = os.TempDir()
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	_ = cmd.Run()
	m := reJavaVersion.FindStringSubmatch(buf.String())
	if m == nil {
		return 0, ""
	}
	return parseMajor(m[1]), m[1]
}

func parseMajor(v string) int {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '_' || r == '+' })
	if len(parts) == 0 {
		return 0
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	if n == 1 && len(parts) > 1 {
		n, _ = strconv.Atoi(parts[1])
	}
	return n
}

// ---- inputs ---------------------------------------------------------------------------------

type inputs struct {
	jars        []string          // repo-relative, sorted
	classDirs   []string          // repo-relative class roots, sorted
	unrooted    int               // class files whose class root could not be derived
	dirsDropped int               // class roots beyond maxClassDirs
	rels        map[string]string // absolute path -> repo-relative
}

func collectInputs(p *engine.Project) inputs {
	in := inputs{rels: map[string]string{}}
	for _, f := range p.Files.WithExt(".jar") {
		if f.Size > 0 {
			in.jars = append(in.jars, f.Rel)
		}
	}
	dirs := map[string]bool{}
	for _, f := range p.Files.WithExt(".class") {
		if root, ok := classRoot(f.Rel); ok {
			dirs[root] = true
		} else {
			in.unrooted++
		}
	}
	for d := range dirs {
		in.classDirs = append(in.classDirs, d)
	}
	sort.Strings(in.jars)
	sort.Strings(in.classDirs)
	if len(in.classDirs) > maxClassDirs {
		in.dirsDropped = len(in.classDirs) - maxClassDirs
		in.classDirs = in.classDirs[:maxClassDirs]
	}
	for _, r := range in.jars {
		in.rels[filepath.Join(p.Root, filepath.FromSlash(r))] = r
	}
	for _, r := range in.classDirs {
		in.rels[filepath.Join(p.Root, filepath.FromSlash(r))] = r
	}
	return in
}

// classRoot derives the root of a package tree from a class file's repo-relative path: the first
// directory named like a class output directory, plus the language and source-set subdirectories
// Gradle adds below build/classes.
func classRoot(rel string) (string, bool) {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 {
		return "", false
	}
	segs = segs[:len(segs)-1]
	for i, s := range segs {
		low := strings.ToLower(s)
		if low != "bin" && !strings.HasSuffix(low, "classes") {
			continue
		}
		end := i + 1
		if low == "classes" && end+1 < len(segs) {
			switch strings.ToLower(segs[end]) {
			case "java", "kotlin", "groovy", "scala":
				end += 2
			}
		}
		return strings.Join(segs[:end], "/"), true
	}
	return "", false
}

// ---- shared runner --------------------------------------------------------------------------

type runner struct {
	p      *engine.Project
	j      jdk
	capDir string
	work   string
	emit   func(model.Evidence)
	in     inputs
	useMR  bool
	n      int // invocation counter, for log names
}

func (r *runner) call(ctx context.Context, logStem string, timeout time.Duration, args []string) (proc.Result, string) {
	r.n++
	res, _ := proc.Run(ctx, proc.Spec{
		Name: r.j.Exe, Args: args, Dir: r.work, Timeout: timeout,
		LogDir: r.capDir, LogName: fmt.Sprintf("%s-%03d", logStem, r.n), RunDir: r.p.RunDir,
	})
	out := res.Stdout
	if res.Record.LogRef != "" { // the full stdout is on disk; the in-memory tail keeps only 64 KiB
		if b, err := os.ReadFile(filepath.Join(r.p.RunDir, filepath.FromSlash(res.Record.LogRef))); err == nil {
			out = string(b)
		}
	}
	return res, out
}

func failed(res proc.Result) bool { return res.Record.ExitCode != 0 || res.Record.Err != "" }

func firstProblem(res proc.Result, out string) string {
	if res.Record.Err != "" {
		return res.Record.Err
	}
	if res.Record.TimedOut {
		return "timed out"
	}
	for _, text := range []string{res.Stderr, out} {
		for l := range strings.SplitSeq(text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				if len(l) > 300 {
					l = l[:300] + "..."
				}
				return l
			}
		}
	}
	return "exit code " + strconv.Itoa(res.Record.ExitCode)
}

func (r *runner) abs(rel string) string { return filepath.Join(r.p.Root, filepath.FromSlash(rel)) }

func limitJoin(ss []string, n int) string {
	if len(ss) > n {
		return strings.Join(ss[:n], ";") + fmt.Sprintf(";...(+%d)", len(ss)-n)
	}
	return strings.Join(ss, ";")
}

func locsOf(rels []string) []model.Location {
	rels = uniqSorted(rels)
	if len(rels) > maxLocations {
		rels = rels[:maxLocations]
	}
	out := make([]model.Location, len(rels))
	for i, r := range rels {
		out[i] = model.Location{Path: r}
	}
	return out
}

func uniqSorted(ss []string) []string {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func setKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- jdeps ----------------------------------------------------------------------------------

type jdepsAPI struct {
	archives    map[string]bool // archive names as jdeps printed them
	users       map[string]bool
	removed     bool
	module      string
	archiveOnly bool
}

var (
	reHeader = regexp.MustCompile(`^(\S.*?)\s+->\s+(.+?)\s*$`)
	reDetail = regexp.MustCompile(`^\s+(\S+)\s+->\s+(\S+)\s+(.*?)\s*$`)
	reRepl   = regexp.MustCompile(`^(\S+)\s{2,}(.+?)\s*$`)
	reParen  = regexp.MustCompile(`\(([^)]*)\)\s*$`)
)

type jdepsParse struct {
	apis        map[string]*jdepsAPI
	replacement map[string]string
}

func newParse() *jdepsParse {
	return &jdepsParse{apis: map[string]*jdepsAPI{}, replacement: map[string]string{}}
}

func (jp *jdepsParse) api(name string) *jdepsAPI {
	a := jp.apis[name]
	if a == nil {
		a = &jdepsAPI{archives: map[string]bool{}, users: map[string]bool{}}
		jp.apis[name] = a
	}
	return a
}

// parse reads jdeps --jdk-internals output. Header lines look like "x.jar -> jdk.unsupported" or
// "x.jar -> JDK removed internal API"; indented lines are "user.Class -> sun.misc.Unsafe  JDK
// internal API (jdk.unsupported)"; a trailing table maps internal APIs to suggested replacements.
func (jp *jdepsParse) parse(out string) {
	var curArchive, curTarget string
	detail := map[string]bool{}
	type hdr struct{ archive, target string }
	var flagged []hdr
	inTable := false
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			m := reDetail.FindStringSubmatch(line)
			if m == nil || curArchive == "" || !strings.Contains(strings.ToLower(m[3]), "internal api") {
				continue
			}
			a := jp.api(m[2])
			a.archives[curArchive] = true
			a.users[m[1]] = true
			low := strings.ToLower(m[3] + " " + curTarget)
			if strings.Contains(low, "removed") {
				a.removed = true
			}
			if pm := reParen.FindStringSubmatch(m[3]); pm != nil {
				a.module = pm[1]
			} else {
				a.module = curTarget
			}
			detail[curArchive] = true
			continue
		}
		if strings.HasPrefix(line, "JDK Internal API") {
			inTable = true
			continue
		}
		if inTable {
			if strings.HasPrefix(line, "---") {
				continue
			}
			if m := reRepl.FindStringSubmatch(line); m != nil {
				jp.replacement[m[1]] = m[2]
			}
			continue
		}
		if m := reHeader.FindStringSubmatch(line); m != nil {
			curArchive, curTarget = m[1], m[2]
			low := strings.ToLower(curTarget)
			if strings.Contains(low, "internal api") || low == "jdk.unsupported" {
				flagged = append(flagged, hdr{curArchive, curTarget})
			}
		}
	}
	// archive-level only (no per-class lines were printed, e.g. summary mode)
	for _, h := range flagged {
		if detail[h.archive] {
			continue
		}
		a := jp.api(h.target)
		a.archiveOnly = true
		a.module = h.target
		a.archives[h.archive] = true
		if strings.Contains(strings.ToLower(h.target), "removed") {
			a.removed = true
		}
	}
}

func (jp *jdepsParse) replacementFor(api string) string {
	if s, ok := jp.replacement[api]; ok {
		return s
	}
	if i := strings.LastIndexByte(api, '.'); i > 0 {
		return jp.replacement[api[:i]]
	}
	return ""
}

func (r *runner) jdeps(ctx context.Context) error {
	deadline := time.Now().Add(jdepsTotalBudget)
	var files []string
	for _, rel := range r.in.jars {
		files = append(files, r.abs(rel))
	}
	for _, rel := range r.in.classDirs {
		files = append(files, r.abs(rel))
	}
	jp := newParse()
	failures := map[string]string{} // repo-relative -> reason
	budgetHit := false
	batches := 0

	var usageErr string
	var process func(batch []string)
	process = func(batch []string) {
		if ctx.Err() != nil || usageErr != "" {
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			budgetHit = true
			return
		}
		if left > 5*time.Minute {
			left = 5 * time.Minute
		}
		run := func(mr bool) (proc.Result, string) {
			// --jdk-internals cannot be combined with -summary/-verbose (jdeps rejects the call).
			args := []string{"-q"}
			if mr {
				args = append(args, "--multi-release", "base")
			}
			args = append(args, "--jdk-internals")
			args = append(args, batch...)
			return r.call(ctx, "jdeps", left, args)
		}
		res, out := run(r.useMR)
		// jdeps rejects --multi-release when an archive is not a multi-release jar; drop the flag.
		if failed(res) && r.useMR && strings.Contains(strings.ToLower(res.Stderr+out), "multi-release") {
			r.useMR = false
			res, out = run(false)
		}
		if !failed(res) {
			jp.parse(out)
			return
		}
		// A rejected command line is Anamnesis's error, not a property of the archives: stop and report
		// the analyzer as failed instead of bisecting and blaming every jar.
		if msg := usageError(res, out); msg != "" {
			usageErr = msg
			return
		}
		if len(batch) == 1 {
			rel := r.in.rels[batch[0]]
			if rel == "" {
				rel = filepath.ToSlash(batch[0])
			}
			failures[rel] = firstProblem(res, out)
			return
		}
		mid := len(batch) / 2
		process(batch[:mid])
		process(batch[mid:])
	}

	for i := 0; i < len(files); i += jdepsBatchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := min(i+jdepsBatchSize, len(files))
		batches++
		process(files[i:end])
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if usageErr != "" {
		return fmt.Errorf("jdeps rejected the command line Anamnesis built (a Anamnesis defect, not a property of the project): %s", usageErr)
	}

	// resolve archive names back to repository paths
	byBase := map[string][]string{}
	for abs, rel := range r.in.rels {
		byBase[filepath.Base(abs)] = append(byBase[filepath.Base(abs)], rel)
	}
	resolve := func(name string) []string {
		if rel, ok := r.in.rels[name]; ok {
			return []string{rel}
		}
		if rel, ok := r.in.rels[filepath.FromSlash(name)]; ok {
			return []string{rel}
		}
		if rels := byBase[filepath.Base(name)]; len(rels) > 0 {
			return rels
		}
		return nil
	}

	names := make([]string, 0, len(jp.apis))
	for n := range jp.apis {
		names = append(names, n)
	}
	sort.Strings(names)
	removedN := 0
	for _, n := range names {
		a := jp.apis[n]
		var rels []string
		for arch := range a.archives {
			rels = append(rels, resolve(arch)...)
		}
		rels = uniqSorted(rels)
		vals := map[string]string{
			"archives":      limitJoin(rels, maxListed),
			"archive_count": strconv.Itoa(len(rels)),
			"removed":       strconv.FormatBool(a.removed),
			"module":        a.module,
		}
		if len(a.users) > 0 {
			vals["referencing_classes"] = strconv.Itoa(len(a.users))
		}
		if rep := jp.replacementFor(n); rep != "" {
			vals["suggested_replacement"] = rep
		}
		if a.archiveOnly {
			vals["level"] = "archive"
		}
		ev := model.Evidence{
			Kind: model.KindJdeps, Subject: n, Status: model.Warn, Confidence: model.Observed,
			Severity: model.SevMedium, Locations: locsOf(rels), Values: vals,
			Limitations: []string{"jdeps reads bytecode: it shows a reference exists in an archive, not that the code path runs"},
		}
		if a.removed {
			ev.Severity = model.SevHigh
			removedN++
			ev.Finding = fmt.Sprintf("%s is a JDK removed internal API, referenced from %d archive(s)", n, len(rels))
		} else {
			ev.Finding = fmt.Sprintf("%s is a JDK internal API, referenced from %d archive(s)", n, len(rels))
		}
		if a.archiveOnly {
			ev.Finding = fmt.Sprintf("%d archive(s) depend on %s", len(rels), n)
		}
		r.emit(ev)
	}

	// summary item: always emitted, so "nothing found" and "could not scan" are both visible
	failRels := make([]string, 0, len(failures))
	for rel := range failures {
		failRels = append(failRels, rel)
	}
	sort.Strings(failRels)
	sum := model.Evidence{
		Kind: model.KindJdeps, Subject: "jdeps:summary", Confidence: model.Observed,
		Status: model.Pass, Locations: locsOf(failRels),
		Values: map[string]string{
			"summary": "true", "jdk_home": r.j.Home, "jdk_version": r.j.Version,
			"jars": strconv.Itoa(len(r.in.jars)), "class_dirs": strconv.Itoa(len(r.in.classDirs)),
			"internal_apis": strconv.Itoa(len(names)), "removed_apis": strconv.Itoa(removedN),
			"failed_archives": strconv.Itoa(len(failures)), "batches": strconv.Itoa(batches),
			"multi_release_flag": strconv.FormatBool(r.useMR),
		},
	}
	switch {
	case len(files) == 0:
		sum.Status = model.Info
		sum.Finding = "no jars or compiled class directories were found, so jdeps had nothing to scan"
	default:
		sum.Finding = fmt.Sprintf("jdeps scanned %d jar(s) and %d class dir(s): %d internal API(s), %d removed", len(r.in.jars), len(r.in.classDirs), len(names), removedN)
	}
	if len(failures) > 0 {
		sum.Status = model.Warn
		sum.Finding += fmt.Sprintf("; %d archive(s) could not be analysed (e.g. class files newer than the JDK, or unreadable); see Locations", len(failures))
		for _, rel := range failRels[:min(len(failRels), 5)] {
			sum.Limitations = append(sum.Limitations, rel+": "+failures[rel])
		}
	}
	if budgetHit {
		sum.Status = model.Warn
		sum.Values["truncated"] = "time budget"
		sum.Limitations = append(sum.Limitations, fmt.Sprintf("the %s total jdeps time budget ran out; later archives were not scanned", jdepsTotalBudget))
	}
	if r.in.dirsDropped > 0 {
		sum.Limitations = append(sum.Limitations, fmt.Sprintf("%d class directories beyond the first %d were not scanned", r.in.dirsDropped, maxClassDirs))
	}
	if r.in.unrooted > 0 {
		sum.Limitations = append(sum.Limitations, fmt.Sprintf("%d class file(s) outside a recognisable class output directory were not scanned", r.in.unrooted))
	}
	r.emit(sum)
	return nil
}

// ---- jdeprscan ------------------------------------------------------------------------------

type depAPI struct {
	archives map[string]bool
	users    map[string]bool
	since    string
}

var (
	reSince   = regexp.MustCompile(`(?i)since[ =:]*([0-9][0-9.]*)`)
	depWords  = map[string]bool{"deprecated": true, "class": true, "method": true, "field": true, "interface": true, "enum": true, "annotation": true, "constructor": true}
	reJarHead = regexp.MustCompile(`^(?i)jar file\b`)
)

// parseJdeprscan reads jdeprscan output: "class a/B uses deprecated method java/lang/Thread.stop()V".
func parseJdeprscan(out string, apis map[string]*depAPI, jar string) (found int) {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || reJarHead.MatchString(line) {
			continue
		}
		i := strings.Index(line, " uses ")
		if i < 0 {
			continue
		}
		userFields := strings.Fields(line[:i])
		if len(userFields) == 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len(" uses "):])
		words := strings.Fields(rest)
		for len(words) > 0 && depWords[strings.ToLower(words[0])] {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		api := words[0]
		a := apis[api]
		if a == nil {
			a = &depAPI{archives: map[string]bool{}, users: map[string]bool{}}
			apis[api] = a
		}
		a.archives[jar] = true
		a.users[userFields[len(userFields)-1]] = true
		if m := reSince.FindStringSubmatch(rest); m != nil && a.since == "" {
			a.since = m[1]
		}
		found++
	}
	return found
}

func (r *runner) jdeprscan(ctx context.Context) error {
	deadline := time.Now().Add(jdeprscanBudget)
	jars := r.in.jars
	truncatedJars := 0
	if len(jars) > jdeprscanMaxJars {
		truncatedJars = len(jars) - jdeprscanMaxJars
		jars = jars[:jdeprscanMaxJars]
	}
	apis := map[string]*depAPI{}
	failures := map[string]string{}
	unresolved := map[string]bool{} // jars scanned, but with references jdeprscan could not resolve
	scanned := 0
	budgetHit := false
	// The project's other jars are the classpath, so references between them resolve. Windows caps a
	// command line near 32k characters; beyond the cap the scan runs without a classpath and says so.
	var allAbs []string
	for _, rel := range r.in.jars {
		allAbs = append(allAbs, r.abs(rel))
	}
	classpath := strings.Join(allAbs, string(os.PathListSeparator))
	cpTooLong := len(classpath) > jdeprscanClasspathMax
	for _, rel := range jars {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		left := time.Until(deadline)
		if left <= 0 {
			budgetHit = true
			break
		}
		if left > 3*time.Minute {
			left = 3 * time.Minute
		}
		args := []string{"--for-removal"}
		if r.j.Major > 0 {
			args = append(args, "--release", strconv.Itoa(r.j.Major))
		}
		if !cpTooLong && len(allAbs) > 1 {
			args = append(args, "--class-path", classpath)
		}
		args = append(args, r.abs(rel))
		res, out := r.call(ctx, "jdeprscan", left, args)
		scanned++
		found := parseJdeprscan(out, apis, rel)
		if msg := usageError(res, out); msg != "" {
			return fmt.Errorf("jdeprscan rejected the command line Anamnesis built (a Anamnesis defect, not a property of the project): %s", msg)
		}
		text := res.Stderr + "\n" + out
		switch {
		case failed(res) && found == 0 && !onlyResolutionErrors(text):
			failures[rel] = firstProblem(res, out)
		case strings.Contains(strings.ToLower(text), "error:"):
			unresolved[rel] = true
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	names := make([]string, 0, len(apis))
	for n := range apis {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := apis[n]
		rels := setKeys(a.archives)
		vals := map[string]string{
			"for_removal":         "true",
			"archives":            limitJoin(rels, maxListed),
			"archive_count":       strconv.Itoa(len(rels)),
			"referencing_classes": strconv.Itoa(len(a.users)),
			"jdk_release":         strconv.Itoa(r.j.Major),
		}
		if a.since != "" {
			vals["since"] = a.since
		}
		r.emit(model.Evidence{
			Kind: model.KindJdeprscan, Subject: n, Status: model.Warn, Confidence: model.Observed,
			Severity:  model.SevHigh,
			Finding:   fmt.Sprintf("%s is deprecated for removal in JDK %d, used by %d class(es) in %d jar(s)", n, r.j.Major, len(a.users), len(rels)),
			Locations: locsOf(rels), Values: vals,
			Limitations: []string{"jdeprscan reports what is marked for removal in the scanning JDK's release, which may not be the release the project will target"},
		})
	}

	failRels := make([]string, 0, len(failures))
	for rel := range failures {
		failRels = append(failRels, rel)
	}
	sort.Strings(failRels)
	sum := model.Evidence{
		Kind: model.KindJdeprscan, Subject: "jdeprscan:summary", Confidence: model.Observed,
		Status: model.Pass, Locations: locsOf(failRels),
		Values: map[string]string{
			"summary": "true", "jdk_home": r.j.Home, "jdk_version": r.j.Version, "release": strconv.Itoa(r.j.Major),
			"jars_total": strconv.Itoa(len(r.in.jars)), "jars_scanned": strconv.Itoa(scanned),
			"apis": strconv.Itoa(len(names)), "failed_jars": strconv.Itoa(len(failures)),
		},
	}
	if len(r.in.jars) == 0 {
		sum.Status = model.Info
		sum.Finding = "no jars were found, so jdeprscan had nothing to scan"
	} else {
		sum.Finding = fmt.Sprintf("jdeprscan --for-removal --release %d scanned %d of %d jar(s): %d API(s) deprecated for removal", r.j.Major, scanned, len(r.in.jars), len(names))
	}
	if len(unresolved) > 0 {
		sum.Status = model.Warn
		sum.Values["jars_with_unresolved_references"] = strconv.Itoa(len(unresolved))
		sum.Finding += fmt.Sprintf("; %d jar(s) reference classes that are neither in the project's jars nor in the JDK (cannot find class), so their results may be incomplete", len(unresolved))
		sum.Limitations = append(sum.Limitations, "classes missing from the scan classpath usually come from libraries the application server or build provides at runtime")
	}
	if cpTooLong {
		sum.Limitations = append(sum.Limitations, "the jar classpath exceeded the Windows command-line limit, so each jar was scanned without the others on its classpath")
	}
	if len(failures) > 0 {
		sum.Status = model.Warn
		sum.Finding += fmt.Sprintf("; %d jar(s) could not be analysed; see Locations and the logs", len(failures))
		for _, rel := range failRels[:min(len(failRels), 5)] {
			sum.Limitations = append(sum.Limitations, rel+": "+failures[rel])
		}
	}
	if truncatedJars > 0 {
		sum.Status = model.Warn
		sum.Values["truncated"] = "jar limit"
		sum.Limitations = append(sum.Limitations, fmt.Sprintf("only the first %d jars (sorted by path) were scanned; %d were not", jdeprscanMaxJars, truncatedJars))
	}
	if budgetHit {
		sum.Status = model.Warn
		sum.Values["truncated"] = "time budget"
		sum.Limitations = append(sum.Limitations, fmt.Sprintf("the %s time budget ran out; later jars were not scanned", jdeprscanBudget))
	}
	sum.Limitations = append(sum.Limitations, "compiled class directories are not scanned by jdeprscan here, only jars")
	r.emit(sum)
	return nil
}

// usageError recognises a jdeps/jdeprscan rejection of its own arguments.
func usageError(res proc.Result, out string) string {
	text := strings.TrimSpace(res.Stderr + "\n" + out)
	low := strings.ToLower(text)
	for _, marker := range []string{"cannot be used with", "invalid option", "unknown option", "error: option", "usage: jdeps", "usage: jdeprscan"} {
		if strings.Contains(low, marker) {
			if i := strings.IndexByte(text, '\n'); i > 0 {
				text = text[:i]
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// jdeprscanClasspathMax keeps the command line under the Windows limit (~32k characters).
const jdeprscanClasspathMax = 24000

// onlyResolutionErrors reports whether every "error:" line is a class or member resolution miss.
func onlyResolutionErrors(text string) bool {
	seen := false
	for line := range strings.SplitSeq(text, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(l, "error:") {
			continue
		}
		seen = true
		if !strings.Contains(l, "cannot find class") && !strings.Contains(l, "cannot resolve") {
			return false
		}
	}
	return seen
}
