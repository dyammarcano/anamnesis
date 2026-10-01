// Package prepare readies the machine for deep analysis: it installs missing global tools with
// scoop and selects a JDK matching the project's declared Java target. It runs AFTER preflight, so
// the report keeps both the environment as found and the environment as prepared (ADR-0005).
package prepare

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/config"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/proc"
)

const analyzerName = "prepare"

// toolPackages maps a prereq.tool Subject to its scoop package.
var toolPackages = map[string]string{
	"git":    "git",
	"ant":    "ant",
	"mvn":    "maven",
	"maven":  "maven",
	"gradle": "gradle",
	"gh":     "gh",
}

// notInstallable are requirements scoop cannot satisfy; they are reported, never attempted.
var notInstallable = []string{"jboss", "weblogic", "websphere", "glassfish", "geronimo", "appserver", "application server"}

// Run inspects preflight evidence, installs what is missing (when the parameters allow it), and
// returns evidence of everything it did. It mutates params in memory only: a JDK it installed or
// found is added to params.JDKs, and params.Build.JDK is set when it was empty. parameters.yaml on
// disk is not rewritten.
func Run(ctx context.Context, params *config.Parameters, runDir string, preflight []model.Evidence) []model.Evidence {
	var out []model.Evidence
	emit := func(e model.Evidence) {
		e.Analyzer = analyzerName
		e.Category = model.CatEnvironment
		e.Finalize()
		out = append(out, e)
	}
	logDir := filepath.Join(runDir, "capabilities", analyzerName)

	missing := missingTools(preflight)
	if params.MTA.Allow && !onPath("mvn") {
		missing["mvn"] = "MTA (kantra containerless mode requires mvn on PATH)"
	}
	targets := declaredTargets(preflight)
	needJDK := params.Environment.InstallJDKs && params.Build.JDK == "" && len(targets) > 0

	if len(missing) == 0 && !needJDK {
		emit(model.Evidence{Kind: model.KindEnvInstall, Subject: "none", Status: model.Pass, Confidence: model.Observed,
			Finding: "No missing tools to install; environment left unchanged."})
		return out
	}
	if !params.Environment.InstallMissing {
		for tool, why := range missing {
			emit(model.Evidence{Kind: model.KindEnvInstall, Subject: tool, Status: model.Skipped, Confidence: model.Observed,
				Finding: fmt.Sprintf("%s is missing (%s); not installed because environment.install_missing is false in parameters.yaml.", tool, why),
				Values:  map[string]string{"package": toolPackages[tool], "state": "SKIPPED"}})
		}
		return out
	}

	shell, err := powershell()
	if err != nil {
		emit(model.Evidence{Kind: model.KindEnvInstall, Subject: "scoop", Status: model.Failed, Confidence: model.Observed,
			Finding: "Cannot prepare the environment: no PowerShell found to run scoop.", Values: map[string]string{"state": "FAILED"}})
		return out
	}
	if r := scoop(ctx, shell, runDir, logDir, "scoop-which", "--version", time.Minute); r.Record.ExitCode != 0 {
		emit(model.Evidence{Kind: model.KindEnvInstall, Subject: "scoop", Status: model.Failed, Confidence: model.Observed, Command: &r.Record,
			Finding: "scoop is not available, so missing tools cannot be installed. Install scoop (https://scoop.sh) and rerun.",
			Values:  map[string]string{"state": "FAILED"}})
		return out
	}
	for _, b := range params.Environment.ScoopBuckets {
		scoop(ctx, shell, runDir, logDir, "bucket-"+b, "bucket add "+b, 5*time.Minute) // "already exists" is fine
	}

	tools := make([]string, 0, len(missing))
	for t := range missing {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		why := missing[tool]
		pkg, ok := toolPackages[tool]
		if !ok || isNotInstallable(tool) {
			emit(model.Evidence{Kind: model.KindEnvInstall, Subject: tool, Status: model.Fail, Confidence: model.Observed, Severity: model.SevMedium,
				Finding: fmt.Sprintf("%s is required (%s) but cannot be installed automatically with scoop.", tool, why),
				Values:  map[string]string{"state": "NOT_INSTALLABLE"}})
			continue
		}
		emit(install(ctx, shell, runDir, logDir, tool, pkg, why))
	}

	if needJDK {
		out = append(out, prepareJDK(ctx, shell, params, runDir, logDir, targets, emit)...)
	}
	return out
}

func install(ctx context.Context, shell, runDir, logDir, tool, pkg, why string) model.Evidence {
	r := scoop(ctx, shell, runDir, logDir, "install-"+pkg, "install "+pkg, 20*time.Minute)
	e := model.Evidence{Kind: model.KindEnvInstall, Subject: tool, Confidence: model.Observed, Command: &r.Record,
		Values: map[string]string{"package": pkg, "reason": why}}
	isJDK := strings.HasSuffix(pkg, "-jdk")
	prefix := scoopPrefix(ctx, shell, runDir, logDir, pkg)
	if prefix != "" {
		e.Values["prefix"] = prefix
		// scoop adds <prefix>in to the USER path in the registry, which this already-running
		// process does not see. Make the tool visible to Anamnesis and to the children it starts.
		// JDKs are excluded: they are passed explicitly as JAVA_HOME, and putting their bin first
		// would change which java other tools (e.g. MTA, which needs 17+) pick up.
		if !isJDK {
			prependProcessPath(filepath.Join(prefix, "bin"))
		}
	}
	present := prefix != ""
	if !isJDK {
		present = onPath(tool)
	}
	switch {
	case r.Record.ExitCode == 0 && present:
		e.Status, e.Values["state"] = model.Pass, "INSTALLED"
		e.Finding = fmt.Sprintf("Installed %s with `scoop install %s` (needed for: %s).", tool, pkg, why)
	case strings.Contains(strings.ToLower(r.Stdout+r.Stderr), "already installed") && present:
		e.Status, e.Values["state"] = model.Pass, "ALREADY_PRESENT"
		e.Finding = fmt.Sprintf("%s was already installed by scoop.", pkg)
	default:
		e.Status, e.Values["state"], e.Severity = model.Failed, "FAILED", model.SevMedium
		e.Finding = fmt.Sprintf("`scoop install %s` did not make %s available (exit %d); %s remains missing.", pkg, tool, r.Record.ExitCode, tool)
	}
	return e
}

// prependProcessPath puts dir first on this process's PATH (children inherit it). It changes
// nothing outside the Anamnesis process.
func prependProcessPath(dir string) {
	cur := os.Getenv("PATH")
	for _, d := range filepath.SplitList(cur) {
		if strings.EqualFold(filepath.Clean(d), filepath.Clean(dir)) {
			return
		}
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+cur)
}

// prepareJDK picks a JDK able to compile the project's declared target, installing it if needed.
func prepareJDK(ctx context.Context, shell string, params *config.Parameters, runDir, logDir string, targets []int, emit func(model.Evidence)) []model.Evidence {
	minTarget := targets[0]
	pkg := jdkPackage(minTarget)
	name := strings.TrimSuffix(pkg, "-jdk")
	reason := fmt.Sprintf("lowest declared Java target is %s; %s can still compile it (JDK 12+ cannot target 6, JDK 20+ cannot target 7)", javaLabel(minTarget), pkg)

	home := scoopPrefix(ctx, shell, runDir, logDir, pkg)
	if home == "" {
		// scoop JDK packages set the user-level JAVA_HOME and prepend their bin to the user PATH.
		watched := []string{"JAVA_HOME", "Path"}
		before := map[string]string{}
		readable := map[string]bool{}
		for _, v := range watched {
			before[v], readable[v] = userEnv(ctx, shell, runDir, logDir, v)
		}
		emit(install(ctx, shell, runDir, logDir, pkg, pkg, reason))
		home = scoopPrefix(ctx, shell, runDir, logDir, pkg)
		for _, v := range watched {
			after, okAfter := userEnv(ctx, shell, runDir, logDir, v)
			if !readable[v] || !okAfter {
				emit(model.Evidence{Kind: model.KindEnvJavaHome, Subject: v + " (user)", Status: model.Warn, Confidence: model.Unknown, Severity: model.SevHigh,
					Finding: fmt.Sprintf("Could not read the user-level %s before and after installing %s, so Anamnesis cannot tell whether it changed and did not touch it; check it manually.", v, pkg)})
				continue
			}
			if after == before[v] {
				continue
			}
			ev := model.Evidence{Kind: model.KindEnvJavaHome, Subject: v + " (user)", Confidence: model.Observed, Severity: model.SevMedium,
				Values: map[string]string{"user_before": before[v], "user_after_install": after}}
			if !params.Environment.RestoreUserEnv {
				ev.Status = model.Warn
				ev.Finding = fmt.Sprintf("Installing %s changed the user-level %s; it was left changed (environment.restore_user_env is false). Anamnesis passes JAVA_HOME explicitly and does not rely on it.", pkg, v)
				emit(ev)
				continue
			}
			wrote := setUserEnv(ctx, shell, runDir, logDir, v, before[v])
			restored, okRestored := userEnv(ctx, shell, runDir, logDir, v)
			ev.Values["user_after_restore"] = restored
			if wrote && okRestored && restored == before[v] {
				ev.Status = model.Pass
				ev.Finding = fmt.Sprintf("Installing %s changed the user-level %s; Anamnesis restored the previous value (environment.restore_user_env).", pkg, v)
			} else {
				ev.Status, ev.Severity = model.Fail, model.SevHigh
				ev.Finding = fmt.Sprintf("Installing %s changed the user-level %s and restoring the previous value FAILED; check it manually (previous value recorded in this evidence).", pkg, v)
			}
			emit(ev)
		}
	}
	if home == "" {
		emit(model.Evidence{Kind: model.KindEnvBuildJDK, Subject: pkg, Status: model.Fail, Confidence: model.Observed,
			Finding: fmt.Sprintf("No JDK suitable for target %s is available; the local build will run without a matching JDK.", javaLabel(minTarget)),
			Values:  map[string]string{"reason": reason}})
		return nil
	}
	params.AddJDK(name, home)
	params.Build.JDK = name
	emit(model.Evidence{Kind: model.KindEnvBuildJDK, Subject: name, Status: model.Pass, Confidence: model.Derived,
		Finding: fmt.Sprintf("Local build will use %s at %s (%s).", name, home, reason),
		Values:  map[string]string{"name": name, "home": home, "reason": reason}})
	return nil
}

// jdkPackage returns the scoop (java bucket) package able to compile the given target major.
func jdkPackage(target int) string {
	switch {
	case target <= 8:
		return "temurin8-jdk"
	case target <= 11:
		return "temurin11-jdk"
	case target <= 17:
		return "temurin17-jdk"
	case target <= 21:
		return "temurin21-jdk"
	default:
		return fmt.Sprintf("temurin%d-jdk", target)
	}
}

func javaLabel(major int) string {
	if major <= 8 {
		return "1." + strconv.Itoa(major)
	}
	return strconv.Itoa(major)
}

// missingTools reads prereq.tool evidence with state MISSING.
func missingTools(ev []model.Evidence) map[string]string {
	out := map[string]string{}
	for _, e := range ev {
		if e.Kind != model.KindPrereqTool || e.Values["state"] != "MISSING" {
			continue
		}
		tool := strings.ToLower(e.Subject)
		if tool == "jdk" || tool == "java" || tool == "javac" {
			continue // JDKs are handled by prepareJDK, never by name
		}
		why := e.Values["capability"]
		if why == "" {
			why = e.Values["requirement"]
		}
		out[tool] = why
	}
	return out
}

// declaredTargets returns distinct declared Java targets (major numbers), lowest first. Only
// project declarations count — never the local runtime.
func declaredTargets(ev []model.Evidence) []int {
	seen := map[int]bool{}
	for _, e := range ev {
		if e.Kind != model.KindJavaTarget && e.Kind != model.KindJavaRelease && e.Kind != model.KindJavaSource {
			continue
		}
		if e.Confidence == model.Unknown {
			continue
		}
		if m := major(e.Values["resolved"]); m > 0 {
			seen[m] = true
		}
	}
	var out []int
	for m := range seen {
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

func major(v string) int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "1.")
	if i := strings.IndexAny(v, ".-_ "); i > 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 99 {
		return 0
	}
	return n
}

func isNotInstallable(tool string) bool {
	t := strings.ToLower(tool)
	for _, s := range notInstallable {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func onPath(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

func powershell() (string, error) {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p, nil
	}
	return exec.LookPath("powershell")
}

// scoop runs `scoop <args>` through PowerShell (scoop is a PowerShell script shim).
func scoop(ctx context.Context, shell, runDir, logDir, logName, args string, timeout time.Duration) proc.Result {
	r, _ := proc.Run(ctx, proc.Spec{
		Name: shell, Args: []string{"-NoProfile", "-NonInteractive", "-Command", "scoop " + args},
		Dir: runDir, Timeout: timeout, LogDir: logDir, LogName: logName, RunDir: runDir,
	})
	return r
}

func scoopPrefix(ctx context.Context, shell, runDir, logDir, pkg string) string {
	r := scoop(ctx, shell, runDir, logDir, "prefix-"+pkg, "prefix "+pkg, time.Minute)
	if r.Record.ExitCode != 0 {
		return ""
	}
	line := strings.TrimSpace(lastLine(r.Stdout))
	if line == "" || strings.Contains(strings.ToLower(line), "not installed") {
		return ""
	}
	return line
}

// userEnv reads a user-level environment variable from the registry (observation only).
func userEnv(ctx context.Context, shell, runDir, logDir, name string) (string, bool) {
	r, _ := proc.Run(ctx, proc.Spec{
		Name: shell, Args: []string{"-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("[Environment]::GetEnvironmentVariable(%s,'User')", psQuote(name))},
		Dir: runDir, Timeout: 30 * time.Second, LogDir: logDir, LogName: "user-env-get-" + strings.ToLower(name), RunDir: runDir,
	})
	if r.Record.ExitCode != 0 || r.Record.TimedOut || r.Record.Err != "" {
		return "", false // a failed read is not an empty value
	}
	return strings.TrimSpace(lastLine(r.Stdout)), true
}

// setUserEnv writes a user-level environment variable back (restore after a JDK install). An empty
// value removes the variable, which is what "unset before" means.
func setUserEnv(ctx context.Context, shell, runDir, logDir, name, value string) bool {
	v := "$null"
	if value != "" {
		v = psQuote(value)
	}
	r, _ := proc.Run(ctx, proc.Spec{
		Name: shell, Args: []string{"-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("[Environment]::SetEnvironmentVariable(%s,%s,'User')", psQuote(name), v)},
		Dir: runDir, Timeout: 30 * time.Second, LogDir: logDir, LogName: "user-env-set-" + strings.ToLower(name), RunDir: runDir,
	})
	return r.Record.ExitCode == 0 && !r.Record.TimedOut && r.Record.Err == ""
}

// psQuote makes a PowerShell single-quoted literal.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n ")
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
