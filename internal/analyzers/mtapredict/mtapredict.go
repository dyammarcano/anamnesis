// Package mtapredict predicts, without running MTA, whether the MTA/Kantra Java provider can start on
// a project, and checks the prerequisites of the configured MTA installation. The prediction rests on
// the build-tool detection documented in docs/kantra-internals.md section 5.2; it is an inference.
package mtapredict

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/proc"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

const (
	maxLocations = 50
	kantraError  = "failed to start providers: unable to initialize providers: unable to init: unable to get build tool"
)

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "mtapredict" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	if err := os.MkdirAll(p.RunDir, 0o755); err != nil {
		return err
	}
	predict(p, sink)
	prereqs(ctx, p, sink)
	profiles(p, sink)
	return ctx.Err()
}

func predict(p *engine.Project, sink engine.Sink) {
	ix := p.Files
	pom := ix.HasRoot("pom.xml")
	groovy := ix.HasRoot("build.gradle")
	kts := ix.HasRoot("build.gradle.kts")
	assume := []string{
		"The Java provider looks for build.gradle, then a .jar/.war/.ear input, then pom.xml, at the input root only (docs/kantra-internals.md §5.2, J:bldtool/tool.go:145-163).",
		"build.gradle.kts is not detected (docs/kantra-internals.md §5.2).",
		"Kantra/MTA version 8.3.0 behaviour as documented; a different version may differ.",
	}
	vals := map[string]string{"root_pom_xml": strconv.FormatBool(pom), "root_build_gradle": strconv.FormatBool(groovy), "root_build_gradle_kts": strconv.FormatBool(kts)}
	ev := model.Evidence{
		Category: model.CatMigration, Kind: model.KindMTAPrediction, Subject: "java-provider",
		Confidence: model.Inferred, Assumptions: assume, Values: vals,
		Locations: rootLocs(ix, "pom.xml", "build.gradle", "build.gradle.kts"),
	}
	switch {
	case pom || groovy:
		which := "pom.xml"
		if groovy {
			which = "build.gradle"
		}
		vals["java_provider"], vals["reason"] = "APPLICABLE", which+" exists at the project root"
		ev.Status = model.Info
		ev.Finding = "The MTA Java provider is predicted to find a build tool: " + which + " exists at the project root."
		if groovy && !ix.HasRoot("gradlew") && !ix.HasRoot("gradlew.bat") {
			ev.Limitations = append(ev.Limitations, "Gradle dependency work additionally requires a gradle wrapper in the project (docs/kantra-internals.md §5.2); none was found at the root.")
		}
		ev.Limitations = append(ev.Limitations, "Detecting a build tool does not predict that the rest of the analysis succeeds.")
	default:
		reason := "no pom.xml and no build.gradle at the project root"
		if kts {
			reason += "; build.gradle.kts alone does not count because the Java provider only detects the Groovy DSL file build.gradle"
		}
		vals["java_provider"], vals["reason"], vals["expected_error"] = "NOT_APPLICABLE", reason, kantraError
		ev.Status = model.Warn
		ev.Finding = "The MTA Java provider is predicted to fail with \"" + kantraError + "\" because there is " + reason + "."
		ev.Limitations = append(ev.Limitations, "Only the project root is examined by the provider; a pom.xml in a subdirectory does not help.")
	}
	sink.Emit(ev)
}

func rootLocs(ix *scan.Index, names ...string) []model.Location {
	var locs []model.Location
	for _, n := range names {
		for _, f := range ix.Named(n) {
			if !strings.Contains(f.Rel, "/") && len(locs) < maxLocations {
				locs = append(locs, model.Location{Path: f.Rel})
			}
		}
	}
	return locs
}

func exeNames(base string) []string {
	if runtime.GOOS == "windows" {
		return []string{base + ".exe", base + ".cmd", base + ".bat"}
	}
	return []string{base}
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

type item struct {
	subject string
	state   string // OK | MISSING | OBSERVED | WARN
	detail  string
	from    string
	cmd     *model.CommandRecord
	extra   map[string]string
}

func emitItem(sink engine.Sink, it item) {
	vals := map[string]string{"state": it.state, "detail": it.detail}
	if it.from != "" {
		vals["resolved_from"] = it.from
	}
	maps.Copy(vals, it.extra)
	status := model.Pass
	switch it.state {
	case "MISSING":
		status = model.Fail
	case "WARN":
		status = model.Warn
	case "OBSERVED":
		status = model.Info
	}
	sink.Emit(model.Evidence{
		Category: model.CatMigration, Kind: model.KindMTAPrereq, Subject: it.subject,
		Finding: it.detail, Status: status, Confidence: model.Observed, Values: vals, Command: it.cmd,
	})
}

var javaVersionRe = regexp.MustCompile(`version\s+"([^"]+)"`)

func javaMajor(v string) int {
	v = strings.TrimPrefix(v, "1.")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(v[:end])
	return n
}

func prereqs(ctx context.Context, p *engine.Project, sink engine.Sink) {
	var mta config
	if p.Params != nil {
		mta = config{install: p.Params.MTA.InstallDir, exe: p.Params.MTA.Executable, jdk: p.Params.MTA.JDK, jdkHome: p.Params.JDKHome(p.Params.MTA.JDK), mvn: p.Params.Tool("mvn")}
	}

	// MTA CLI executable.
	{
		it := item{subject: "mta-cli", state: "MISSING"}
		switch {
		case mta.exe != "":
			it.from = "parameters.yaml (mta.executable)"
			if isFile(mta.exe) {
				it.state, it.detail = "OK", "MTA CLI executable found at "+mta.exe+"."
			} else {
				it.detail = "mta.executable points to " + mta.exe + ", which does not exist."
			}
		case mta.install != "":
			it.from = "parameters.yaml (mta.install_dir)"
			for _, base := range []string{"windows-mta-cli", "mta-cli", "kantra"} {
				for _, n := range exeNames(base) {
					if c := filepath.Join(mta.install, n); isFile(c) && it.state != "OK" {
						it.state, it.detail = "OK", "MTA CLI executable found at "+c+"."
					}
				}
			}
			if it.state != "OK" {
				it.detail = "No windows-mta-cli, mta-cli or kantra executable exists in mta.install_dir " + mta.install + "."
			}
		default:
			it.from = "PATH (global tool)"
			for _, base := range []string{"mta-cli", "windows-mta-cli", "kantra"} {
				if c, err := exec.LookPath(base); err == nil && it.state != "OK" {
					it.state, it.detail = "OK", "MTA CLI found on PATH at "+c+" (global tool)."
				}
			}
			if it.state != "OK" {
				it.detail = "No MTA CLI (mta-cli, windows-mta-cli, kantra) is configured in parameters.yaml (mta.install_dir / mta.executable) or on PATH."
			}
		}
		emitItem(sink, it)
	}

	// MTA installation layout.
	{
		it := item{subject: "mta-install-dir", from: "parameters.yaml (mta.install_dir)"}
		if mta.install == "" {
			it.state, it.detail, it.from = "MISSING", "mta.install_dir is not set in parameters.yaml; rulesets/, jdtls/ and static-report/ could not be located.", "parameters.yaml (unset)"
		} else {
			var miss []string
			for _, d := range []string{"rulesets", "jdtls", "static-report"} {
				if !isDir(filepath.Join(mta.install, d)) {
					miss = append(miss, d+"/")
				}
			}
			if len(miss) == 0 {
				it.state, it.detail = "OK", mta.install+" contains rulesets/, jdtls/ and static-report/."
			} else {
				it.state, it.detail = "MISSING", mta.install+" is missing "+strings.Join(miss, ", ")+"."
			}
		}
		emitItem(sink, it)
	}

	// mvn: required by kantra containerless mode even for non-Maven projects.
	{
		it := item{subject: "mvn", extra: map[string]string{"why": "required by kantra containerless mode even for non-Maven projects (docs/kantra-internals.md §4)"}}
		path := ""
		if mta.mvn != "" {
			it.from = "parameters.yaml"
			if c, err := exec.LookPath(mta.mvn); err == nil {
				path = c
			}
		} else {
			it.from = "PATH (global tool)"
			if c, err := exec.LookPath("mvn"); err == nil {
				path = c
			}
		}
		if path != "" {
			it.state, it.detail = "OK", "mvn found at "+path+"; required by kantra containerless mode even for non-Maven projects."
		} else {
			it.state, it.detail = "MISSING", "mvn was not found; required by kantra containerless mode even for non-Maven projects (kantra stops with \"cannot find requirement maven\")."
		}
		emitItem(sink, it)
	}

	// MTA JDK.
	{
		it := item{subject: "mta-jdk", from: "parameters.yaml (mta.jdk)"}
		javaExe := ""
		if mta.jdkHome != "" {
			for _, n := range exeNames("java") {
				if c := filepath.Join(mta.jdkHome, "bin", n); isFile(c) {
					javaExe = c
					break
				}
			}
		}
		switch {
		case mta.jdk == "":
			it.state, it.detail, it.from = "MISSING", "mta.jdk is not set in parameters.yaml; MTA needs a Java 17+ JDK.", "parameters.yaml (unset)"
		case javaExe == "":
			it.state, it.detail = "MISSING", "JDK "+mta.jdk+" (home "+mta.jdkHome+") has no bin/java executable."
		default:
			res, err := proc.Run(ctx, proc.Spec{Name: javaExe, Args: []string{"-version"}, Dir: p.RunDir, Timeout: 20 * time.Second, RunDir: p.RunDir})
			if err == nil {
				it.cmd = &res.Record
			}
			m := javaVersionRe.FindStringSubmatch(res.Stderr + "\n" + res.Stdout)
			switch {
			case m == nil:
				it.state, it.detail = "WARN", "JDK "+mta.jdk+" did not report a parsable version from `java -version`; Java 17+ could not be confirmed."
			case javaMajor(m[1]) >= 17:
				it.state, it.detail = "OK", "MTA JDK "+mta.jdk+" is Java "+strconv.Itoa(javaMajor(m[1]))+" ("+m[1]+"), satisfying the 17+ requirement."
				it.extra = map[string]string{"major": strconv.Itoa(javaMajor(m[1])), "version": m[1]}
			default:
				it.state, it.detail = "MISSING", "MTA JDK "+mta.jdk+" is Java "+strconv.Itoa(javaMajor(m[1]))+" ("+m[1]+"); MTA needs 17 or newer."
				it.extra = map[string]string{"major": strconv.Itoa(javaMajor(m[1])), "version": m[1]}
			}
		}
		emitItem(sink, it)
	}

	// Observed environment: reported, never used by Anamnesis.
	for _, name := range []string{"KANTRA_DIR", "JAVA_HOME"} {
		v, set := os.LookupEnv(name)
		d := name + " is unset in the process environment (observed only; not used by Anamnesis)."
		if set {
			d = name + " is set to " + v + " in the process environment (observed only; not used by Anamnesis)."
		}
		emitItem(sink, item{subject: "env:" + name, state: "OBSERVED", detail: d, from: "process environment, observed only"})
	}
}

type config struct{ install, exe, jdk, jdkHome, mvn string }

func profiles(p *engine.Project, sink engine.Sink) {
	dir := filepath.Join(p.Root, ".konveyor", "profiles")
	files := p.Files.Under(".konveyor/profiles")
	if !isDir(dir) && len(files) == 0 {
		emitItem(sink, item{subject: ".konveyor/profiles", state: "OBSERVED", detail: "No .konveyor/profiles directory exists in the project."})
		return
	}
	var locs []model.Location
	for i, f := range files {
		if i >= maxLocations {
			break
		}
		locs = append(locs, model.Location{Path: f.Rel})
	}
	if len(locs) == 0 {
		locs = []model.Location{{Path: ".konveyor/profiles"}}
	}
	sink.Emit(model.Evidence{
		Category: model.CatMigration, Kind: model.KindMTAPrereq, Subject: ".konveyor/profiles",
		Finding: "The project carries an MTA profile under .konveyor/profiles; MTA auto-loads it and it can change the analysis input, mode, rules, label selector and default-ruleset use.",
		Status:  model.Warn, Confidence: model.Observed, Locations: locs,
		Values: map[string]string{
			"state": "WARN", "detail": "repository-carried MTA profile present", "files": strconv.Itoa(len(files)),
			"locations_total": strconv.Itoa(len(files)),
		},
		Assumptions: []string{"Auto-loading of <input>/.konveyor/profiles follows docs/kantra-internals.md §9 for MTA 8.3.0."},
		Limitations: []string{"The profile contents were not interpreted."},
	})
}
