package mta

import "strings"

// Failure classes (P-2). The match patterns are exact lower-case substrings taken from kantra and
// analyzer-lsp messages, see docs/kantra-internals.md section 4 and 5.3.
const (
	FailMTANotInstalled      = "MTA_NOT_INSTALLED"
	FailMavenRequiredMissing = "MAVEN_REQUIRED_MISSING"
	FailJavaHomeMissing      = "JAVA_HOME_MISSING"
	FailJDKTooOld            = "JDK_TOO_OLD"
	FailKantraDirIncomplete  = "KANTRA_DIR_INCOMPLETE"
	FailJavaNoBuildTool      = "JAVA_PROVIDER_NO_BUILD_TOOL"
	FailProviderInitTimeout  = "PROVIDER_INIT_TIMEOUT"
	FailProviderInitFailed   = "PROVIDER_INIT_FAILED"
	FailRuleParseFailed      = "RULE_PARSE_FAILED"
	FailStaticReportFailed   = "STATIC_REPORT_FAILED"
	FailTimeout              = "TIMEOUT"
	FailUnknown              = "UNKNOWN"
)

type failPattern struct {
	class string
	subs  []string
}

// Order matters: specific causes come before the generic "failed to start providers" wrapper that
// kantra puts around them.
var failPatterns = []failPattern{
	{FailMavenRequiredMissing, []string{"cannot find requirement maven"}},
	{FailJavaHomeMissing, []string{"java_home is not set"}},
	{FailJDKTooOld, []string{"openjdk17+", "jdtls requires at least java 17"}},
	{FailKantraDirIncomplete, []string{"missing kantra dependency", "installation directory not found"}},
	{FailJavaNoBuildTool, []string{"unable to get build tool"}},
	{FailProviderInitTimeout, []string{"timed out starting providers"}},
	{FailProviderInitFailed, []string{"failed to start providers", "unable to initialize providers", "unable to init provider"}},
	{FailRuleParseFailed, []string{"failed to parse rules"}},
	{FailStaticReportFailed, []string{"static report", "static-report"}},
}

// ClassifyFailure maps the combined stderr and analysis.log text of a failed MTA run to a failure
// class and returns the line that matched (trimmed, bounded). FailUnknown with an empty line means
// nothing matched; that is not evidence of any particular cause.
func ClassifyFailure(text string) (class, line string) {
	lines := strings.Split(text, "\n")
	lower := make([]string, len(lines))
	for i, l := range lines {
		lower[i] = strings.ToLower(l)
	}
	for _, fp := range failPatterns {
		for i, l := range lower {
			for _, s := range fp.subs {
				if !strings.Contains(l, s) {
					continue
				}
				// the static-report patterns only mean failure when the line also says so
				if fp.class == FailStaticReportFailed &&
					!strings.Contains(l, "fail") && !strings.Contains(l, "error") && !strings.Contains(l, "unable") {
					continue
				}
				return fp.class, boundLine(lines[i])
			}
		}
	}
	return FailUnknown, ""
}

func boundLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
