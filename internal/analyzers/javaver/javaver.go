// Package javaver collects Java language-level facts from every non-Ant source: Maven, Gradle,
// Eclipse and NetBeans settings, class-file versions inside archives and loose classes, manifest JDK
// attributes, and the Java runtimes known to the operator. The source/target/release kinds are never merged.
package javaver

import (
	"context"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

const (
	maxLocations = 50
	maxTextBytes = 8 << 20
)

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "javaver" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	ix := p.Files

	scan.ForEach(ctx, ix.Named("pom.xml"), func(f scan.File) { analyzeMaven(ix, f, sink) })
	var gradle []scan.File
	gradle = append(gradle, ix.Named("build.gradle")...)
	gradle = append(gradle, ix.Named("build.gradle.kts")...)
	scan.ForEach(ctx, gradle, func(f scan.File) { analyzeGradle(ix, f, sink) })
	scan.ForEach(ctx, ix.Named("org.eclipse.jdt.core.prefs"), func(f scan.File) { analyzeEclipse(ix, f, sink) })
	var nb []scan.File
	for _, f := range ix.Named("project.properties") {
		if path.Base(path.Dir(f.Rel)) == "nbproject" {
			nb = append(nb, f)
		}
	}
	scan.ForEach(ctx, nb, func(f scan.File) { analyzeNetBeans(ix, f, sink) })

	analyzeBinaries(ctx, p, sink)
	analyzeLocal(ctx, p, sink)
	return ctx.Err()
}

// javaVal describes one declared source/target/release value.
type javaVal struct {
	kind       string // model.KindJavaSource / Target / Release
	subject    string // repo-relative file
	line       int
	raw        string
	resolved   string // "" when unknown
	origin     string
	conf       model.Confidence
	via        string // how it was declared
	note       string
	finding    string
	limitation string
}

func emitVal(sink engine.Sink, v javaVal) {
	vals := map[string]string{
		"raw": v.raw, "resolved": v.resolved, "origin": v.origin,
		"defined_at": v.subject + ":" + strconv.Itoa(v.line),
	}
	if v.via != "" {
		vals["via"] = v.via
	}
	if v.note != "" {
		vals["note"] = v.note
	}
	var lim []string
	if v.limitation != "" {
		lim = append(lim, v.limitation)
	}
	status := model.Info
	if v.conf == model.Unknown {
		status = model.Warn
	}
	sink.Emit(model.Evidence{
		Category: model.CatJavaVersion, Kind: v.kind, Subject: v.subject, Finding: v.finding,
		Status: status, Confidence: v.conf,
		Locations:   []model.Location{{Path: v.subject, Line: v.line}},
		Values:      vals,
		Limitations: lim,
	})
}

func warnFile(sink engine.Sink, kind, rel, msg string) {
	sink.Emit(model.Evidence{
		Category: model.CatJavaVersion, Kind: kind, Subject: rel, Finding: msg,
		Status: model.Warn, Confidence: model.Unknown,
		Locations: []model.Location{{Path: rel}},
	})
}

func readCapped(abs string) ([]byte, error) {
	fh, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	return io.ReadAll(io.LimitReader(fh, maxTextBytes))
}

var placeholderRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// substitute resolves ${name} placeholders from props, repeatedly. ok is false when a placeholder
// has no definition; missing names the first one. changed reports whether any substitution happened.
func substitute(raw string, props map[string]string) (out string, ok, changed bool, missing string) {
	out = raw
	for range 8 {
		m := placeholderRe.FindStringSubmatch(out)
		if m == nil {
			return out, true, changed, ""
		}
		v, found := props[m[1]]
		if !found {
			return "", false, changed, m[1]
		}
		out = strings.Replace(out, m[0], v, 1)
		changed = true
	}
	return "", false, changed, "(recursive)"
}

type kv struct {
	key, val string
	line     int
}

// parseProps reads a Java .properties style file.
func parseProps(data []byte) []kv {
	var out []kv
	for i, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == '!' {
			continue
		}
		idx := strings.IndexAny(l, "=:")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(l[:idx])
		v := strings.TrimSpace(l[idx+1:])
		v = strings.NewReplacer(`\:`, ":", `\=`, "=", `\\`, `\`).Replace(v)
		out = append(out, kv{k, v, i + 1})
	}
	return out
}

func analyzeEclipse(ix *scan.Index, f scan.File, sink engine.Sink) {
	data, err := readCapped(ix.Abs(f))
	if err != nil {
		warnFile(sink, model.KindJavaSource, f.Rel, "could not read Eclipse JDT settings: "+err.Error())
		return
	}
	for _, e := range parseProps(data) {
		var kind, note string
		switch e.key {
		case "org.eclipse.jdt.core.compiler.source":
			kind = model.KindJavaSource
		case "org.eclipse.jdt.core.compiler.compliance":
			kind, note = model.KindJavaTarget, "compiler compliance level; stands in for the target level and is not a separate -target setting"
		case "org.eclipse.jdt.core.compiler.codegen.targetPlatform":
			kind = model.KindJavaTarget
		default:
			continue
		}
		emitVal(sink, javaVal{
			kind: kind, subject: f.Rel, line: e.line, raw: e.val, resolved: e.val, origin: "eclipse",
			conf: model.Observed, via: e.key, note: note,
			finding: "Eclipse JDT settings declare " + e.key + " = " + e.val + ".",
		})
	}
}

func analyzeNetBeans(ix *scan.Index, f scan.File, sink engine.Sink) {
	data, err := readCapped(ix.Abs(f))
	if err != nil {
		warnFile(sink, model.KindJavaSource, f.Rel, "could not read NetBeans project properties: "+err.Error())
		return
	}
	entries := parseProps(data)
	props := map[string]string{}
	for _, e := range entries {
		props[e.key] = e.val
	}
	for _, e := range entries {
		var kind string
		switch e.key {
		case "javac.source":
			kind = model.KindJavaSource
		case "javac.target":
			kind = model.KindJavaTarget
		default:
			continue
		}
		v := javaVal{kind: kind, subject: f.Rel, line: e.line, raw: e.val, origin: "netbeans", via: e.key, conf: model.Observed}
		if out, ok, changed, missing := substitute(e.val, props); ok {
			v.resolved = out
			if changed {
				v.conf = model.Derived
			}
			v.finding = "NetBeans project properties declare " + e.key + " = " + out + "."
		} else {
			v.conf = model.Unknown
			v.finding = "NetBeans project properties declare " + e.key + " = " + e.val + ", which could not be resolved."
			v.limitation = "property " + missing + " is not defined in this file"
		}
		emitVal(sink, v)
	}
}

// distributionString renders "major:count" pairs sorted by major ascending.
func distributionString(d map[int]int) string {
	keys := make([]int, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Itoa(k) + ":" + strconv.Itoa(d[k])
	}
	return strings.Join(parts, ",")
}

// javaFromMajor maps a class-file major version to the Java release that produces it.
func javaFromMajor(major int) string {
	switch major {
	case 45:
		return "1.1"
	case 46:
		return "1.2"
	case 47:
		return "1.3"
	case 48:
		return "1.4"
	case 49:
		return "5"
	case 50:
		return "6"
	case 51:
		return "7"
	case 52:
		return "8"
	}
	if major < 45 {
		return "unknown"
	}
	return strconv.Itoa(major - 44)
}
