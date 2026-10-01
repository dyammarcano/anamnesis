package buildrun

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"anamnesis/internal/model"
	"anamnesis/internal/redact"
)

// Facts is what the caller knows about the build attempt besides the log.
type Facts struct {
	ToolFound       bool
	ToolName        string
	ExitCode        int
	TimedOut        bool
	LocalJavaMajor  int      // 0 when unknown
	DeclaredTargets []string // e.g. "1.6", "8"
	EnvMissing      []string // env var names the build references that are unset locally
}

type rule struct {
	re  *regexp.Regexp
	why string
}

func ci(p string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + p) }

var jdkRules = []rule{
	{ci(`(source|target) option \d+(\.\d+)? is (no longer supported|obsolete)`), "javac rejects the declared source/target level"},
	{ci(`invalid (target|source) release[: ]`), "javac rejects the declared source/target level"},
	{ci(`release version \S+ not supported`), "javac does not support the requested release"},
	{ci(`class file has wrong version`), "class files were built for a different JDK level than the one running"},
	{ci(`UnsupportedClassVersionError`), "class files were built for a different JDK level than the one running"},
	{ci(`Unsupported major\.minor version`), "class files were built for a different JDK level than the one running"},
	{ci(`Unsupported class file major version`), "a tool in the build cannot read class files of the running JDK"},
	{ci(`compiled by a more recent version of the Java Runtime`), "class files were built for a newer JDK than the one running"},
	{ci(`Could not determine java version from`), "the build tool does not recognise the running JDK version"},
	{ci(`sun\.misc\.BASE64(Encoder|Decoder)`), "uses JDK-internal API removed from the JDK"},
}

var removedJEE = ci(`package (javax\.xml\.bind|javax\.annotation|javax\.activation|javax\.jws|javax\.xml\.ws|javax\.transaction|javax\.xml\.soap|com\.sun\.xml\.internal)[\w.]* does not exist`)

var envRules = []rule{
	{ci(`\$\{env\.[A-Za-z0-9_.]+\}`), "an unexpanded ${env.X} reference reached the output, so that environment variable is not set"},
	{ci(`JAVA_HOME (is not defined|is not set|is set to an invalid|does not point)`), "JAVA_HOME is not usable"},
	{ci(`(environment variable|\b\w+_HOME\b|\benv\.\w+).*\b(is )?not (set|defined)\b`), "a required environment variable is not set"},
	{ci(`\b(set|define|missing)\b.*\benvironment variable\b`), "the build asks for an environment variable"},
	{ci(`\b(is )?not (set|defined)\b.*\b(environment|home|variable)\b`), "the build reports an unset environment/home setting"},
	{ci(`Unable to find a javac compiler`), "no JDK compiler reachable (JAVA_HOME likely points at a JRE)"},
}

var serverWord = ci(`(app(lication)?[ _-]?server|appserver|jboss|wildfly|weblogic|websphere|glassfish|tomcat|geronimo|jetty|jonas|resin)`)
var serverFail = ci(`(could not be detected|not detected|could not (find|locate)|not found|not set|not defined|missing|is required|must be (set|specified|defined)|does not exist|cannot find|unable to|no (such )?(application )?server)`)
var notClassRef = ci(`(package \S+ does not exist|cannot find symbol|symbol:|import \S+;)`)

var repoRules = []rule{
	{ci(`Could not resolve dependencies`), "dependency resolution failed"},
	{ci(`Could not transfer artifact`), "an artifact could not be downloaded"},
	{ci(`Could not resolve all (files|dependencies) for configuration`), "dependency resolution failed"},
	{ci(`Failed to (read artifact descriptor|collect dependencies)`), "dependency resolution failed"},
	{ci(`UnknownHostException|unknown host|nodename nor servname`), "a repository host could not be resolved"},
	{ci(`Connection (refused|timed out|reset)|connect timed out|Network is unreachable`), "a repository was unreachable"},
	{ci(`PKIX path building failed|unable to find valid certification path`), "a repository's TLS certificate is not trusted locally"},
	{ci(`(status code|return code is|HTTP/\d\.\d|response code)[: ]+\W*40[13]\b|\b40[13] (Unauthorized|Forbidden)`), "a repository answered 401/403"},
	{ci(`unresolved dependency`), "an Ivy dependency could not be resolved"},
}

var depRules = []rule{
	{ci(`package [\w.$]+ does not exist`), "a referenced package is not on the classpath"},
	{ci(`cannot find symbol`), "a referenced symbol is not on the classpath"},
	{ci(`Could not find file`), "a file the build expects is absent"},
	{ci(`(jar|lib|library|directory|dir|folder|file|path|basedir)\b.*\bdoes not exist`), "a file or directory the build expects is absent"},
	{ci(`taskdef class \S+ cannot be found|Problem: failed to create task or type|Could not load definitions from resource`), "an Ant task definition's class/jar is absent"},
	{ci(`Cannot find \S+ imported from`), "an imported build file is absent"},
	{ci(`Reference \S+ not found`), "an Ant reference (usually a classpath) is undefined"},
	{ci(`(Could not find|Unable to find) (artifact|\S+\.jar)`), "a required jar is absent"},
	{ci(`Warning: Could not find file`), "a file the build expects is absent"},
}

var compileErr = ci(`(COMPILATION ERROR|Compilation failure|\.java:\d+: error|\[javac\].*\berror\b|error: )`)
var buildFailed = ci(`(BUILD FAILED|BUILD FAILURE|FAILURE: Build failed|\[ERROR\])`)

const axisNote = "This classifies why the local build did not succeed; it does not by itself show the source is unbuildable."

// Classify maps a build outcome to a model.Build* class with ordered heuristics. Environment
// explanations are tested first; SOURCE_OR_BUILD_ERROR is only returned when compile errors exist
// and none of them applies. Reasons quote matching (redacted) log lines.
func Classify(log string, f Facts) (class string, reasons []string) {
	lines := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")

	if !f.ToolFound {
		name := f.ToolName
		if name == "" {
			name = "build tool"
		}
		return model.BuildToolNotAvailable, []string{
			fmt.Sprintf("%s was not found on PATH; the build was not attempted.", name),
			"Nothing was learned about the source from this.",
		}
	}
	if f.TimedOut {
		return model.BuildTimeout, []string{
			"The build exceeded its time limit and was stopped; it neither succeeded nor failed on its own.",
			"Last output may show where it was: " + lastLine(lines),
		}
	}
	if f.ExitCode == 0 && !anyMatch(lines, buildFailed) {
		return model.BuildSuccess, []string{"The tool exited 0 and the log shows no failure marker."}
	}

	// 1. JDK incompatibility.
	if hits := collect(lines, jdkRules, 4); len(hits) > 0 {
		r := []string{fmt.Sprintf("%s: the running JDK%s and the project's declared level disagree.", model.BuildJDKIncompatible, javaNote(f))}
		return model.BuildJDKIncompatible, append(append(r, hits...), notSource("the failure is a JDK/level mismatch, not a defect in the source"), axisNote)
	}
	if ls := matchLines(lines, removedJEE, 4); len(ls) > 0 && removedFromJDK(f) {
		r := []string{fmt.Sprintf("%s: packages removed from the JDK in 11 are missing while the project declares target %s%s.",
			model.BuildJDKIncompatible, strings.Join(f.DeclaredTargets, ", "), javaNote(f))}
		return model.BuildJDKIncompatible, append(append(r, quoteAll(ls)...), notSource("those packages exist on the JDK level the project declares"), axisNote)
	}

	// 2. Missing environment variable.
	envHits := collect(lines, envRules, 4)
	for _, n := range f.EnvMissing {
		for _, l := range lines {
			if n != "" && strings.Contains(l, n) && len(envHits) < 6 {
				envHits = append(envHits, quote(l))
				break
			}
		}
	}
	if len(envHits) > 0 {
		r := []string{model.BuildMissingEnvVar + ": the build refers to an environment variable or home directory that is not set locally."}
		if len(f.EnvMissing) > 0 {
			r = append(r, "Environment names known to be unset locally: "+strings.Join(f.EnvMissing, ", "))
		}
		return model.BuildMissingEnvVar, append(append(r, envHits...), notSource("the build stopped on missing local configuration"), axisNote)
	}

	// 3. Missing application server / runtime.
	var srv []string
	for _, l := range lines {
		if serverWord.MatchString(l) && serverFail.MatchString(l) && !notClassRef.MatchString(l) {
			srv = append(srv, quote(l))
			if len(srv) == 4 {
				break
			}
		}
	}
	if len(srv) > 0 {
		r := []string{model.BuildMissingAppServer + ": the build reports an application server or runtime it could not find."}
		return model.BuildMissingAppServer, append(append(r, srv...), notSource("the build needs a server installation that this machine lacks"), axisNote)
	}

	// 4. Repository.
	if hits := collect(lines, repoRules, 4); len(hits) > 0 {
		r := []string{model.BuildMissingRepository + ": a dependency repository could not be reached, resolved against, or authenticated to."}
		return model.BuildMissingRepository, append(append(r, hits...), notSource("artifacts could not be obtained; that depends on network, credentials or repository availability"), axisNote)
	}

	// 5. Missing dependency or source file.
	if hits := collect(lines, depRules, 5); len(hits) > 0 {
		r := []string{model.BuildMissingDependency + ": the build references libraries or files that are not present in the tree."}
		if ns := serverNamespaces(lines); ns != "" {
			r = append(r, "The missing packages are in an application-server namespace ("+ns+"); they are usually provided by a server installation or its libraries.")
		}
		r = append(r, hits...)
		r = append(r, notSource("missing inputs are not proof the code itself is wrong; a complete dependency set may compile it"))
		return model.BuildMissingDependency, append(r, axisNote)
	}

	// 6. Genuine compile errors, none of the above applying.
	if ls := matchLines(lines, compileErr, 5); len(ls) > 0 {
		r := []string{model.BuildSourceOrBuildError + ": compile errors exist and no environment, JDK, repository or missing-input explanation matched."}
		r = append(r, quoteAll(ls)...)
		return model.BuildSourceOrBuildError, append(r, "This is the only class that points at the source or build script themselves; it is still a local result, not proof of CI behavior.")
	}

	return model.BuildUnknown, []string{
		"The build failed (exit " + strconv.Itoa(f.ExitCode) + ") but no known pattern matched; cause not established.",
		"Last output: " + lastLine(lines),
	}
}

func notSource(why string) string {
	return "Not " + model.BuildSourceOrBuildError + ": " + why + "."
}

func javaNote(f Facts) string {
	if f.LocalJavaMajor > 0 {
		return " (local JDK " + strconv.Itoa(f.LocalJavaMajor) + ")"
	}
	return " (local JDK version unknown)"
}

func removedFromJDK(f Facts) bool {
	if f.LocalJavaMajor != 0 && f.LocalJavaMajor < 11 {
		return false
	}
	for _, t := range f.DeclaredTargets {
		if m := JavaMajor(t); m > 0 && m <= 8 {
			return true
		}
	}
	return false
}

// JavaMajor converts "1.6", "6", "1.8", "11" to the Java major version; 0 if unparseable.
func JavaMajor(s string) int {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	s = strings.TrimPrefix(s, "1.")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func collect(lines []string, rules []rule, max int) []string {
	var out []string
	for _, r := range rules {
		for _, l := range lines {
			if r.re.MatchString(l) {
				out = append(out, quote(l)+"  ["+r.why+"]")
				break
			}
		}
		if len(out) >= max {
			break
		}
	}
	return out
}

func matchLines(lines []string, re *regexp.Regexp, max int) []string {
	var out []string
	for _, l := range lines {
		if re.MatchString(l) {
			out = append(out, l)
			if len(out) == max {
				break
			}
		}
	}
	return out
}

func anyMatch(lines []string, re *regexp.Regexp) bool {
	return slices.ContainsFunc(lines, re.MatchString)
}

func quoteAll(ls []string) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = quote(l)
	}
	return out
}

func quote(l string) string {
	l = strings.TrimSpace(redact.Redact(l))
	if len(l) > 300 {
		l = l[:300] + "..."
	}
	return "log: \"" + l + "\""
}

func lastLine(lines []string) string {
	for _, line := range slices.Backward(lines) {
		if t := strings.TrimSpace(line); t != "" {
			return quote(t)
		}
	}
	return "(no output)"
}

var serverPkg = regexp.MustCompile(`package (org\.jboss|org\.wildfly|weblogic|com\.ibm\.websphere|org\.apache\.geronimo|org\.glassfish|com\.sun\.enterprise)[\w.]* does not exist`)

func serverNamespaces(lines []string) string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if m := serverPkg.FindStringSubmatch(l); m != nil && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return strings.Join(out, ", ")
}
