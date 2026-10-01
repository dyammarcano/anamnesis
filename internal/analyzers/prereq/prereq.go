// Package prereq infers which external tools the project would need for each capability, purely from
// the file index, and probes whether they are available. It never installs anything; for a missing
// tool it only suggests a command and the parameters.yaml key to set afterwards.
package prereq

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/proc"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

const maxLocations = 50

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "prereq" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

type need struct {
	tool        string // evidence subject
	key         string // parameters.yaml tools.<key>; "" for jdk
	exe         string // executable name for PATH lookup
	versionArgs []string
	requirement string
	capability  string
	reason      string
	suggestion  string
	files       []scan.File
}

func locations(files []scan.File) ([]model.Location, int) {
	var locs []model.Location
	for i, f := range files {
		if i >= maxLocations {
			break
		}
		locs = append(locs, model.Location{Path: f.Rel})
	}
	return locs, len(files)
}

func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	if err := os.MkdirAll(p.RunDir, 0o755); err != nil {
		return err
	}
	ix := p.Files
	hasRoot := func(names ...string) bool {
		return slices.ContainsFunc(names, ix.HasRoot)
	}
	named := func(names ...string) []scan.File {
		var out []scan.File
		for _, n := range names {
			out = append(out, ix.Named(n)...)
		}
		return out
	}

	var needs []need
	ant := named("build.xml")
	mvn := named("pom.xml")
	gradle := named("build.gradle", "build.gradle.kts")
	if len(ant) > 0 {
		needs = append(needs, need{tool: "ant", key: "ant", exe: "ant", versionArgs: []string{"-version"}, requirement: "REQUIRED_FOR_CAPABILITY",
			capability: "local build (Ant)", reason: "build.xml present", suggestion: "scoop install ant (Anamnesis installs it automatically when environment.install_missing is true)", files: ant})
	}
	if len(mvn) > 0 && !hasRoot("mvnw", "mvnw.cmd") {
		needs = append(needs, need{tool: "mvn", key: "mvn", exe: "mvn", versionArgs: []string{"-v"}, requirement: "REQUIRED_FOR_CAPABILITY",
			capability: "local build (Maven)", reason: "pom.xml present and no Maven wrapper (mvnw) at the project root", suggestion: "scoop install maven (Anamnesis installs it automatically when environment.install_missing is true)", files: mvn})
	}
	if len(gradle) > 0 && !hasRoot("gradlew", "gradlew.bat") {
		needs = append(needs, need{tool: "gradle", key: "gradle", exe: "gradle", versionArgs: []string{"-v"}, requirement: "REQUIRED_FOR_CAPABILITY",
			capability: "local build (Gradle)", reason: "Gradle build file present and no Gradle wrapper (gradlew) at the project root", suggestion: "scoop install gradle (Anamnesis installs it automatically when environment.install_missing is true)", files: gradle})
	}
	jdkFiles := append(append(append(append([]scan.File{}, ix.WithExt(".java")...), ant...), mvn...), gradle...)
	if len(jdkFiles) > 0 {
		needs = append(needs, need{tool: "jdk", exe: "javac", versionArgs: []string{"-version"}, requirement: "REQUIRED_FOR_CAPABILITY",
			capability: "compilation", reason: "not required for static discovery; Java sources or build files are present",
			suggestion: "scoop search java — choose a JDK matching the project's declared target (see java.* evidence), then add it under jdks in parameters.yaml",
			files:      jdkFiles})
	}
	var workflows []scan.File
	for _, f := range ix.Under(".github/workflows") {
		if f.Ext == ".yml" || f.Ext == ".yaml" {
			workflows = append(workflows, f)
		}
	}
	if len(workflows) > 0 {
		needs = append(needs, need{tool: "gh", key: "gh", exe: "gh", versionArgs: []string{"--version"}, requirement: "OPTIONAL",
			capability: "CI evidence", reason: ".github/workflows present", suggestion: "scoop install gh (Anamnesis installs it automatically when environment.install_missing is true)", files: workflows})
	}

	for _, n := range needs {
		if ctx.Err() != nil {
			break
		}
		if n.tool == "jdk" {
			probeJDK(ctx, p, sink, n)
			continue
		}
		probeTool(ctx, p, sink, n)
	}
	return ctx.Err()
}

// resolve returns the executable to run and where that choice came from; path is "" when not found.
func resolve(p *engine.Project, n need) (path, from string) {
	if p.Params != nil && n.key != "" {
		if t := p.Params.Tool(n.key); t != "" {
			if found, err := exec.LookPath(t); err == nil {
				return found, "parameters.yaml"
			}
			return "", "parameters.yaml"
		}
	}
	if found, err := exec.LookPath(n.exe); err == nil {
		return found, "PATH (global tool)"
	}
	return "", "PATH (global tool)"
}

func version(ctx context.Context, p *engine.Project, name, path string, args []string) (string, model.CommandRecord, string) {
	res, err := proc.Run(ctx, proc.Spec{
		Name: path, Args: args, Dir: p.RunDir, Timeout: 20 * time.Second,
		LogDir: p.CapabilityDir("prereq"), LogName: name + "-version", RunDir: p.RunDir,
	})
	if err != nil {
		return "", res.Record, err.Error()
	}
	v := firstLine(res.Stdout)
	if v == "" {
		v = firstLine(res.Stderr)
	}
	problem := ""
	if res.Record.Err != "" {
		problem = res.Record.Err
	} else if res.Record.TimedOut {
		problem = "version command timed out after 20s"
	} else if res.Record.ExitCode != 0 {
		problem = "version command exited with code " + itoa(res.Record.ExitCode) + ": " + firstLine(res.Stderr)
	}
	return v, res.Record, problem
}

func probeTool(ctx context.Context, p *engine.Project, sink engine.Sink, n need) {
	path, from := resolve(p, n)
	locs, total := locations(n.files)
	vals := map[string]string{
		"requirement": n.requirement, "capability": n.capability, "reason": n.reason,
		"resolved_from": from, "locations_total": itoa(total),
	}
	ev := model.Evidence{
		Category: model.CatEnvironment, Kind: model.KindPrereqTool, Subject: n.tool, Locations: locs, Values: vals,
		Confidence:  model.Inferred,
		Assumptions: []string{"The requirement is inferred from files present (" + n.reason + "); the tool state below was probed directly."},
	}
	if path == "" {
		vals["state"] = "MISSING"
		vals["suggestion"] = n.suggestion
		ev.Finding = n.tool + " is not available (" + from + "); it is needed for " + n.capability + "."
		ev.Status, ev.Severity = model.Info, model.SevInfo
		if n.requirement != "OPTIONAL" {
			ev.Status, ev.Severity = model.Fail, model.SevLow
		}
		sink.Emit(ev)
		return
	}
	vals["state"] = "AVAILABLE"
	vals["path"] = path
	ver, rec, problem := version(ctx, p, n.tool, path, n.versionArgs)
	ev.Command = &rec
	ev.Status = model.Pass
	if ver != "" {
		vals["version"] = ver
	}
	ev.Finding = n.tool + " is available" + verSuffix(ver) + "; needed for " + n.capability + "."
	if problem != "" {
		ev.Status = model.Warn
		ev.Limitations = append(ev.Limitations, "The executable was found but its version probe did not succeed: "+problem)
	}
	sink.Emit(ev)
}

// probeJDK reports whether a javac is available, preferring the JDKs the operator configured.
func probeJDK(ctx context.Context, p *engine.Project, sink engine.Sink, n need) {
	locs, total := locations(n.files)
	vals := map[string]string{
		"requirement": n.requirement, "capability": n.capability, "reason": n.reason, "locations_total": itoa(total),
	}
	ev := model.Evidence{
		Category: model.CatEnvironment, Kind: model.KindPrereqTool, Subject: "jdk", Locations: locs, Values: vals,
		Confidence:  model.Inferred,
		Assumptions: []string{"The requirement is inferred from files present; the project's own Java target is reported separately by the java.* evidence."},
	}
	javac := "javac"
	if runtime.GOOS == "windows" {
		javac = "javac.exe"
	}
	var found []string
	var rec *model.CommandRecord
	if p.Params != nil {
		for _, j := range p.Params.JDKs {
			if ctx.Err() != nil {
				return
			}
			exe := filepath.Join(j.Home, "bin", javac)
			if _, err := os.Stat(exe); err != nil {
				continue
			}
			ver, r, _ := version(ctx, p, "javac-"+j.Name, exe, n.versionArgs)
			rec = &r
			found = append(found, j.Name+": "+ver)
		}
	}
	switch {
	case len(found) > 0:
		vals["state"], vals["resolved_from"], vals["version"] = "AVAILABLE", "parameters.yaml", strings.Join(found, "; ")
		ev.Status, ev.Command = model.Pass, rec
		ev.Finding = "A JDK with javac is configured in parameters.yaml (" + vals["version"] + "); needed for " + n.capability + "."
	default:
		if path, err := exec.LookPath("javac"); err == nil {
			ver, r, _ := version(ctx, p, "javac-path", path, n.versionArgs)
			vals["state"], vals["resolved_from"], vals["path"] = "AVAILABLE", "PATH (global tool)", path
			if ver != "" {
				vals["version"] = ver
			}
			ev.Status, ev.Command = model.Pass, &r
			ev.Finding = "javac was found on PATH" + verSuffix(ver) + " but no JDK is configured in parameters.yaml; needed for " + n.capability + "."
			ev.Limitations = append(ev.Limitations, "Anamnesis only uses JDKs listed under jdks in parameters.yaml; the PATH javac was observed, not selected.")
		} else {
			vals["state"], vals["resolved_from"], vals["suggestion"] = "MISSING", "parameters.yaml jdks and PATH", n.suggestion
			ev.Status, ev.Severity = model.Fail, model.SevLow
			ev.Finding = "No javac is available (none under parameters.yaml jdks, none on PATH); needed for " + n.capability + "."
		}
	}
	sink.Emit(ev)
}

func verSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " (" + v + ")"
}

func itoa(i int) string { return strconv.Itoa(i) }
