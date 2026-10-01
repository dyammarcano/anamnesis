package javaver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/proc"
)

const notProject = " This is the local environment, not the project's Java version."

var javaVersionOutRe = regexp.MustCompile(`version\s+"([^"]+)"`)

// parseJavaMajor extracts the feature release from a `java -version` version string ("1.8.0_391" -> 8, "17.0.9" -> 17).
func parseJavaMajor(version string) int {
	v := version
	v = strings.TrimPrefix(v, "1.")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(v[:end])
	if err != nil {
		return 0
	}
	return n
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// probeJava runs `<java> -version` (output goes to stderr) with the run directory as cwd.
func probeJava(ctx context.Context, p *engine.Project, javaPath string) (version string, major int, rec model.CommandRecord) {
	res, err := proc.Run(ctx, proc.Spec{Name: javaPath, Args: []string{"-version"}, Dir: p.RunDir, Timeout: 20 * time.Second, RunDir: p.RunDir})
	if err != nil {
		return "", 0, res.Record
	}
	if m := javaVersionOutRe.FindStringSubmatch(res.Stderr + "\n" + res.Stdout); m != nil {
		return m[1], parseJavaMajor(m[1]), res.Record
	}
	return "", 0, res.Record
}

func analyzeLocal(ctx context.Context, p *engine.Project, sink engine.Sink) {
	if err := os.MkdirAll(p.RunDir, 0o755); err != nil {
		return
	}
	emit := func(subject, finding string, status model.Status, conf model.Confidence, vals map[string]string, rec *model.CommandRecord, limits ...string) {
		sink.Emit(model.Evidence{
			Category: model.CatEnvironment, Kind: model.KindJavaLocalRuntime, Subject: subject,
			Finding: finding + notProject, Status: status, Confidence: conf, Values: vals, Command: rec, Limitations: limits,
		})
	}

	// Configured JDKs: the only ones Anamnesis uses.
	if p.Params == nil || len(p.Params.JDKs) == 0 {
		emit("configured-jdks", "No JDKs are configured in parameters.yaml (jdks).", model.Warn, model.Observed,
			map[string]string{"count": "0", "source": "parameters.yaml"}, nil)
	} else {
		for _, j := range p.Params.JDKs {
			if ctx.Err() != nil {
				return
			}
			javaExe := filepath.Join(j.Home, "bin", exeName("java"))
			vals := map[string]string{"name": j.Name, "home": j.Home, "source": "parameters.yaml"}
			if _, err := os.Stat(javaExe); err != nil {
				emit("jdk:"+j.Name, "Configured JDK "+j.Name+" has no "+javaExe+".", model.Warn, model.Observed, vals, nil)
				continue
			}
			ver, major, rec := probeJava(ctx, p, javaExe)
			if _, err := os.Stat(filepath.Join(j.Home, "bin", exeName("javac"))); err == nil {
				vals["javac"] = "present"
			} else {
				vals["javac"] = "absent"
			}
			if major == 0 {
				emit("jdk:"+j.Name, "Configured JDK "+j.Name+" did not report a parsable version from `java -version`.", model.Warn, model.Unknown, vals, &rec)
				continue
			}
			vals["version"], vals["major"] = ver, strconv.Itoa(major)
			emit("jdk:"+j.Name, "Configured JDK "+j.Name+" is Java "+strconv.Itoa(major)+" ("+ver+").", model.Info, model.Observed, vals, &rec)
		}
	}

	// Process environment: observed and reported, never used by Anamnesis.
	vals := map[string]string{"source": "process environment, observed only; not used by Anamnesis"}
	home := os.Getenv("JAVA_HOME")
	if home == "" {
		vals["java_home"] = "unset"
	} else {
		vals["java_home"] = home
		if _, err := os.Stat(filepath.Join(home, "bin", exeName("java"))); err != nil {
			vals["java_home_valid"] = "false"
		} else {
			vals["java_home_valid"] = "true"
		}
	}
	if jc, err := exec.LookPath("javac"); err == nil {
		vals["javac"] = jc
	} else {
		vals["javac"] = "not on PATH"
	}
	finding := "Observed in the process environment, not used by Anamnesis: JAVA_HOME " + vals["java_home"] + "; javac " + vals["javac"] + "."
	var rec *model.CommandRecord
	status, conf := model.Info, model.Observed
	if jp, err := exec.LookPath("java"); err == nil {
		ver, major, r := probeJava(ctx, p, jp)
		rec = &r
		vals["java"] = jp
		if major > 0 {
			vals["version"], vals["major"] = ver, strconv.Itoa(major)
			finding += " `java -version` on PATH reports Java " + strconv.Itoa(major) + " (" + ver + ")."
		} else {
			finding += " `java -version` on PATH produced no parsable version."
			status = model.Warn
		}
	} else {
		vals["java"] = "not on PATH"
		finding += " No java on PATH."
	}
	emit("process-environment", finding, status, conf, vals, rec)
}
