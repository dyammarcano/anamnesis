package javaver

import (
	"regexp"
	"strings"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

var (
	gradleCompatRe  = regexp.MustCompile(`\b(sourceCompatibility|targetCompatibility)\b\s*=?\s*(\S.*?)\s*$`)
	gradleReleaseRe = regexp.MustCompile(`\b(?:options\.)?release(?:\.set\(\s*|\s*=\s*)(\d+)`)
	gradleToolchain = regexp.MustCompile(`JavaLanguageVersion\.of\(\s*(\d+)\s*\)`)
	javaVersionRe   = regexp.MustCompile(`JavaVersion\.VERSION_([0-9_]+)`)
	numericRe       = regexp.MustCompile(`^["']?([0-9]+(?:\.[0-9]+)?)["']?$`)
)

const gradleLimit = "Gradle files are matched with text patterns, not evaluated; conditional logic, variables and plugins are not followed."

func analyzeGradle(ix *scan.Index, f scan.File, sink engine.Sink) {
	data, err := readCapped(ix.Abs(f))
	if err != nil {
		warnFile(sink, model.KindJavaSource, f.Rel, "could not read Gradle build file: "+err.Error())
		return
	}
	for i, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		if j := strings.Index(t, " //"); j >= 0 {
			t = strings.TrimSpace(t[:j])
		}
		n := i + 1
		if m := gradleCompatRe.FindStringSubmatch(t); m != nil {
			kind := model.KindJavaSource
			if m[1] == "targetCompatibility" {
				kind = model.KindJavaTarget
			}
			raw := m[2]
			v := javaVal{kind: kind, subject: f.Rel, line: n, raw: raw, origin: "gradle", via: m[1], conf: model.Derived, limitation: gradleLimit}
			switch {
			case javaVersionRe.MatchString(raw):
				v.resolved = strings.ReplaceAll(javaVersionRe.FindStringSubmatch(raw)[1], "_", ".")
			case numericRe.MatchString(raw):
				v.resolved = numericRe.FindStringSubmatch(raw)[1]
			default:
				v.conf = model.Unknown
			}
			if v.resolved != "" {
				v.finding = "Gradle build file sets " + m[1] + " to " + v.resolved + "."
			} else {
				v.finding = "Gradle build file sets " + m[1] + " to a non-literal expression (" + raw + ")."
			}
			emitVal(sink, v)
		}
		if m := gradleReleaseRe.FindStringSubmatch(t); m != nil {
			emitVal(sink, javaVal{kind: model.KindJavaRelease, subject: f.Rel, line: n, raw: m[0], resolved: m[1], origin: "gradle",
				via: "options.release", conf: model.Derived, limitation: gradleLimit,
				finding: "Gradle build file sets the javac release to " + m[1] + "."})
		}
		if m := gradleToolchain.FindStringSubmatch(t); m != nil && strings.Contains(t, "languageVersion") {
			note := "Gradle toolchain languageVersion; source and target default to this level unless overridden"
			for _, kind := range []string{model.KindJavaSource, model.KindJavaTarget} {
				emitVal(sink, javaVal{kind: kind, subject: f.Rel, line: n, raw: m[0], resolved: m[1], origin: "gradle",
					via: "toolchain languageVersion", note: note, conf: model.Derived, limitation: gradleLimit,
					finding: "Gradle toolchain requests Java language version " + m[1] + "."})
			}
		}
	}
}
