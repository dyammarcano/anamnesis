// Package localbuild runs the project's own build in a staged copy and classifies the outcome.
// It keeps two axes apart: "the local build was not reproduced here" says nothing about whether
// the source is buildable elsewhere.
package localbuild

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"anamnesis/internal/buildrun"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/proc"
	"anamnesis/internal/redact"
)

// AntTargetChooser is set by integration code. It returns the build file (repo-relative, slash
// separated) and a target proven safe to execute, plus the reason. localbuild never parses Ant.
var AntTargetChooser func(p *engine.Project) (buildFile, target, reason string, ok bool)

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

type analyzer struct{}

func (analyzer) Name() string                  { return "localbuild" }
func (analyzer) Phase() engine.Phase           { return engine.Deep }
func (analyzer) Requires() []engine.Capability { return []engine.Capability{engine.SideEffectBuild} }

// Timeout gives the engine build.timeout_minutes plus staging time (engine.Timeouter).
func (analyzer) Timeout(p *engine.Project) time.Duration {
	if p.Params == nil {
		return 0
	}
	return time.Duration(p.Params.Build.TimeoutMinutes+10) * time.Minute
}

const stagedName = "staged-src"

type system struct {
	name string // ant|maven|gradle
	file string // root build file, repo-relative
}

func detect(p *engine.Project) (primary system, others []string) {
	var all []system
	if p.Files.HasRoot("build.xml") {
		all = append(all, system{"ant", "build.xml"})
	}
	if p.Files.HasRoot("pom.xml") {
		all = append(all, system{"maven", "pom.xml"})
	}
	for _, g := range []string{"build.gradle", "build.gradle.kts"} {
		if p.Files.HasRoot(g) {
			all = append(all, system{"gradle", g})
			break
		}
	}
	if len(all) == 0 {
		return system{}, nil
	}
	for _, s := range all[1:] {
		others = append(others, s.name)
	}
	return all[0], others
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	sys, others := detect(p)
	if sys.name == "" {
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindBuildLocal, Subject: "local-build",
			Finding: "No root build.xml, pom.xml or build.gradle(.kts): no local build was attempted.",
			Status:  model.Skipped, Confidence: model.Observed,
			Values: map[string]string{"attempted": "false", "class": model.BuildUnknown},
		})
		return nil
	}

	ev := model.Evidence{
		Category: model.CatBuild, Kind: model.KindBuildLocal, Subject: sys.name,
		Locations: []model.Location{{Path: sys.file}},
		Values:    map[string]string{"attempted": "false", "tool": sys.name},
	}
	if len(others) > 0 {
		ev.Limitations = append(ev.Limitations, "Other root build systems were also present and not attempted: "+strings.Join(others, ", "))
	}

	// Resolve the tool.
	toolPath, resolvedFrom, found := resolveTool(p, sys.name)
	ev.Values["tool_resolved_from"] = resolvedFrom
	ev.Values["resolved_from"] = resolvedFrom
	if !found {
		ev.Values["class"] = model.BuildToolNotAvailable
		ev.Status, ev.Confidence, ev.Severity = model.Fail, model.Observed, model.SevMedium
		ev.Finding = fmt.Sprintf("Local %s build was NOT attempted: %s was not found (%s). This says nothing about the source.", sys.name, toolLabel(sys.name), resolvedFrom)
		ev.Limitations = append(ev.Limitations, "Install "+toolKey(sys.name)+" globally (Anamnesis does this with scoop when environment.install_missing is true) to obtain a local build result.")
		sink.Emit(ev)
		return nil
	}

	// Choose the invocation.
	var (
		buildFile, target string
		chooseReason      string
	)
	if sys.name == "ant" {
		ok := false
		if AntTargetChooser != nil {
			buildFile, target, chooseReason, ok = AntTargetChooser(p)
		}
		if !ok {
			ev.Values["class"] = model.BuildUnsafeTarget
			ev.Status, ev.Confidence = model.Warn, model.Observed
			ev.Finding = "Local Ant build NOT attempted: no target could be proven safe; not executed."
			if chooseReason != "" {
				ev.Finding += " " + redact.Redact(chooseReason)
			}
			ev.Limitations = append(ev.Limitations, "Not executing is a safety decision; it says nothing about whether the source builds.")
			sink.Emit(ev)
			return nil
		}
		ev.Values["target"] = target
		ev.Values["build_file"] = buildFile
		ev.Values["target_reason"] = chooseReason
		// A build only stands for the application when it is the project's own default target.
		// A SAFE auxiliary target (a doc tool, a bundle) succeeding proves nothing about the app.
		def := antDefaultTarget(filepath.Join(p.Root, filepath.FromSlash(buildFile)))
		ev.Values["default_target"] = def
		ev.Values["full_build"] = strconv.FormatBool(def != "" && def == target)
	} else {
		ev.Values["full_build"] = "true" // mvn package / gradle assemble build the whole project
	}

	// Stage a copy; the build never runs in the analyzed repository.
	staged := filepath.Join(p.RunDir, stagedName)
	if err := os.RemoveAll(staged); err != nil {
		return fmt.Errorf("clear staged copy: %w", err)
	}
	if err := buildrun.Stage(ctx, p.Root, staged); err != nil {
		return fmt.Errorf("stage project copy: %w", err)
	}
	ev.Values["staged_copy"] = stagedName

	var cmd buildrun.Command
	switch sys.name {
	case "ant":
		cmd = buildrun.PlanAnt(staged, buildFile, target)
	case "maven":
		cmd = buildrun.PlanMaven(staged)
	case "gradle":
		c, ok := buildrun.PlanGradle(staged)
		if !ok {
			ev.Values["class"] = model.BuildToolNotAvailable
			ev.Status, ev.Confidence = model.Fail, model.Observed
			ev.Finding = "Local Gradle build was NOT attempted: no gradlew wrapper in the project and only the wrapper is used. This says nothing about the source."
			sink.Emit(ev)
			return nil
		}
		cmd = c
	}
	cmd.Name = toolPath
	if sys.name == "gradle" {
		cmd.Name = filepath.Join(staged, filepath.Base(toolPath))
	}
	cmd.Timeout = time.Duration(timeoutMinutes(p)) * time.Minute

	// Build environment: JDK only from parameters.yaml.
	env, jdkHome := buildEnv(p)
	if jdkHome != "" {
		ev.Values["java_home"] = "configured"
		ev.Values["java_home_path"] = jdkHome
		ev.Values["jdk_name"] = p.Params.Build.JDK
	} else {
		ev.Values["java_home"] = "not configured (child inherits process environment)"
	}

	logDir := p.CapabilityDir("localbuild")
	javaMajor := probeJava(ctx, p, staged, jdkHome, logDir)
	if javaMajor > 0 {
		ev.Values["java_local_major"] = strconv.Itoa(javaMajor)
	}
	ev.Values["tool_version"] = toolVersion(ctx, p, sys.name, toolPath, staged, env, logDir)

	res, err := proc.Run(ctx, proc.Spec{
		Name: cmd.Name, Args: cmd.Args, Dir: cmd.Dir, Timeout: cmd.Timeout, Env: env,
		LogDir: logDir, LogName: "build", RunDir: p.RunDir,
	})
	if err != nil {
		return err
	}
	rec := res.Record
	ev.Command = &rec
	ev.Values["attempted"] = "true"
	ev.Values["exit_code"] = strconv.Itoa(rec.ExitCode)
	if rec.LogRef != "" {
		ev.Artifacts = append(ev.Artifacts, rec.LogRef, strings.TrimSuffix(rec.LogRef, ".stdout.log")+".stderr.log")
	}

	var declared []string
	switch sys.name {
	case "ant":
		declared = declaredLevels(filepath.Join(p.Root, filepath.FromSlash(buildFile)))
	case "maven":
		declared = declaredLevels(filepath.Join(p.Root, "pom.xml"))
	}
	if len(declared) > 0 {
		ev.Values["declared_levels"] = strings.Join(declared, ",")
	}
	toolFound := rec.Err == "" || rec.ExitCode >= 0
	class, reasons := buildrun.Classify(res.Stdout+"\n"+res.Stderr, buildrun.Facts{
		ToolFound: toolFound, ToolName: sys.name, ExitCode: rec.ExitCode, TimedOut: rec.TimedOut,
		LocalJavaMajor: javaMajor, DeclaredTargets: declared,
	})
	ev.Values["class"] = class
	ev.Values["reasons"] = strings.Join(reasons, "\n")

	desc := sys.name
	if target != "" {
		desc += " " + target
	}
	switch class {
	case model.BuildSuccess:
		ev.Status, ev.Confidence = model.Pass, model.Observed
		ev.Finding = fmt.Sprintf("Local build (%s) in a staged copy exited 0 with no failure marker in the log.", desc)
	case model.BuildSourceOrBuildError:
		ev.Status, ev.Confidence, ev.Severity = model.Fail, model.Derived, model.SevMedium
		ev.Finding = fmt.Sprintf("Local build (%s) failed with compile errors that no environment explanation covers (%s); a local result only.", desc, class)
	case model.BuildUnknown:
		ev.Status, ev.Confidence, ev.Severity = model.Fail, model.Unknown, model.SevMedium
		ev.Finding = fmt.Sprintf("Local build (%s) failed (exit %d); the cause was not established (%s).", desc, rec.ExitCode, class)
	case model.BuildTimeout:
		ev.Status, ev.Confidence, ev.Severity = model.Fail, model.Observed, model.SevMedium
		ev.Finding = fmt.Sprintf("Local build (%s) did not finish within %d minutes and was stopped (%s); this does not show the source is unbuildable.", desc, timeoutMinutes(p), class)
	default:
		ev.Status, ev.Confidence, ev.Severity = model.Fail, model.Derived, model.SevMedium
		ev.Finding = fmt.Sprintf("Local build NOT reproduced (%s); this does not show the source is unbuildable.", class)
	}
	ev.Assumptions = append(ev.Assumptions, "The class is derived from log patterns, not from a tool-reported cause.")
	ev.Limitations = append(ev.Limitations, "A local result: it depends on this machine's JDK, tools, network and environment.")
	sink.Emit(ev)
	return nil
}

func toolKey(system string) string {
	if system == "maven" {
		return "mvn"
	}
	return system
}

func toolLabel(system string) string {
	switch system {
	case "ant":
		return "ant"
	case "maven":
		return "mvn"
	}
	return "gradlew wrapper"
}

func timeoutMinutes(p *engine.Project) int {
	if p.Params != nil && p.Params.Build.TimeoutMinutes > 0 {
		return p.Params.Build.TimeoutMinutes
	}
	return int(buildrun.DefaultTimeout / time.Minute)
}

// resolveTool returns the executable for the build system. Gradle uses the repository wrapper only.
func resolveTool(p *engine.Project, sys string) (path, from string, found bool) {
	if sys == "gradle" {
		name := "gradlew"
		if p.Files.HasRoot("gradlew.bat") && isWindows() {
			name = "gradlew.bat"
		}
		if isWindows() && name == "gradlew" {
			name = "gradlew.bat"
		}
		if p.Files.HasRoot(name) {
			return filepath.Join(p.Root, name), "repository wrapper (repository code; runs from the staged copy)", true
		}
		return "", "no gradlew wrapper in the project", false
	}
	key := toolKey(sys)
	if p.Params != nil {
		if cfg := p.Params.Tool(key); cfg != "" {
			if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
				return cfg, "parameters.yaml tools." + key, true
			}
			return "", "parameters.yaml tools." + key + " is set but " + cfg + " does not exist", false
		}
	}
	if lp, err := exec.LookPath(key); err == nil {
		return lp, "PATH (global tool)", true
	}
	return "", "not configured and not on PATH", false
}

func isWindows() bool { return os.PathSeparator == '\\' }

// buildEnv returns KEY=VALUE pairs for the child. JAVA_HOME and PATH are set only from the JDK
// named in parameters.yaml build.jdk; with none configured the child inherits the process env.
func buildEnv(p *engine.Project) (env []string, jdkHome string) {
	if p.Params == nil || p.Params.Build.JDK == "" {
		return nil, ""
	}
	home := p.Params.JDKHome(p.Params.Build.JDK)
	if home == "" {
		return nil, ""
	}
	// PATH is read only to prepend the JDK to the child's own PATH; it configures nothing.
	path := filepath.Join(home, "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	return []string{"JAVA_HOME=" + home, "PATH=" + path}, home
}

var javaVer = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

// probeJava runs `java -version` the way the build child would see it; 0 when unknown.
func probeJava(ctx context.Context, p *engine.Project, dir, jdkHome, logDir string) int {
	name := "java"
	var env []string
	if jdkHome != "" {
		name = filepath.Join(jdkHome, "bin", "java")
		env = []string{"JAVA_HOME=" + jdkHome}
	}
	r, err := proc.Run(ctx, proc.Spec{Name: name, Args: []string{"-version"}, Dir: dir, Timeout: 30 * time.Second, Env: env, LogDir: logDir, LogName: "java-version", RunDir: p.RunDir})
	if err != nil || r.Record.ExitCode != 0 {
		return 0
	}
	m := javaVer.FindStringSubmatch(r.Stderr + "\n" + r.Stdout)
	if m == nil {
		return 0
	}
	major, _ := strconv.Atoi(m[1])
	if major == 1 && m[2] != "" {
		major, _ = strconv.Atoi(m[2])
	}
	return major
}

func toolVersion(ctx context.Context, p *engine.Project, sys, toolPath, dir string, env []string, logDir string) string {
	args := []string{"-version"}
	switch sys {
	case "maven":
		args = []string{"-v"}
	case "gradle":
		return "not captured (wrapper downloads its own distribution)"
	}
	r, err := proc.Run(ctx, proc.Spec{Name: toolPath, Args: args, Dir: dir, Timeout: time.Minute, Env: env, LogDir: logDir, LogName: sys + "-version", RunDir: p.RunDir})
	if err != nil || r.Record.Err != "" {
		return "unknown"
	}
	out := strings.TrimSpace(r.Stdout)
	if out == "" {
		out = strings.TrimSpace(r.Stderr)
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return redact.Redact(strings.TrimSpace(out))
}

var levelAttr = regexp.MustCompile(`(?i)\b(?:source|target|release)\s*=\s*["']?(1\.[0-9]|[0-9]{1,2})["']?`)
var levelElem = regexp.MustCompile(`(?i)<(?:maven\.compiler\.)?(?:source|target|release)>\s*(1\.[0-9]|[0-9]{1,2})\s*<`)

// declaredLevels is a lexical scan of the build file for source/target levels. It is a hint for
// classification only; the java-version analyzers own the real resolution.
func declaredLevels(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 2<<20)
	n, _ := f.Read(buf)
	seen := map[string]bool{}
	var out []string
	for _, re := range []*regexp.Regexp{levelAttr, levelElem} {
		for _, m := range re.FindAllStringSubmatch(string(buf[:n]), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

// antDefaultTarget returns the default attribute of the build file's <project> element, or "".
// It reads only up to the first start element.
func antDefaultTarget(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	dec := xml.NewDecoder(f)
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "project" {
				return ""
			}
			for _, a := range se.Attr {
				if a.Name.Local == "default" {
					return strings.TrimSpace(a.Value)
				}
			}
			return ""
		}
	}
}
