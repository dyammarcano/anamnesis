// Package antfacts emits facts read directly from Ant build files: the build files themselves,
// the Java language levels their javac tasks request, unresolved imports, packaging tasks and
// environment-variable references. It never executes Ant.
package antfacts

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/ant"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
)

const maxLocations = 50

// Analyzers returns this package's analyzers.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

type analyzer struct{}

func (analyzer) Name() string                  { return "ant-facts" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

type locSet struct {
	seen map[model.Location]bool
	list []model.Location
}

func (s *locSet) add(l ant.Location) {
	ml := model.Location{Path: l.File, Line: l.Line}
	if s.seen == nil {
		s.seen = map[model.Location]bool{}
	}
	if !s.seen[ml] {
		s.seen[ml] = true
		s.list = append(s.list, ml)
	}
}

// capped returns the locations sorted and cut to maxLocations, plus the total.
func (s *locSet) capped() ([]model.Location, int) {
	out := append([]model.Location(nil), s.list...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	total := len(out)
	if total > maxLocations {
		out = out[:maxLocations]
	}
	return out, total
}

type verAgg struct {
	kind, resolved string
	known          bool
	raws           []string
	conf           model.Confidence
	locs           locSet
	definedAt      string
	limits         []string
	javacCount     int
}

func addUnique(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

func refsOf(raw string) []string {
	var out []string
	for i := 0; i+1 < len(raw); i++ {
		if (raw[i] == '$' || raw[i] == '@') && raw[i+1] == '{' {
			if end := strings.IndexByte(raw[i+2:], '}'); end >= 0 {
				out = append(out, raw[i+2:i+2+end])
				i += 2 + end
			}
		}
	}
	return out
}

func weaker(a, b model.Confidence) model.Confidence {
	rank := func(c model.Confidence) int {
		switch c {
		case model.Observed:
			return 0
		case model.Derived:
			return 1
		case model.Inferred:
			return 2
		}
		return 3
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

type packAgg struct {
	locs  locSet
	dests []string
	count int
}

type envAgg struct {
	locs     locSet
	prefixes []string
	uses     int
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	projects, errs := ant.LoadAll(p)
	for _, e := range errs {
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindAntProject, Subject: e.File,
			Finding: fmt.Sprintf("Ant build file %s could not be parsed: %v", e.File, e.Err),
			Status:  model.Warn, Confidence: model.Observed, Severity: model.SevMedium,
			Locations:   []model.Location{{Path: e.File}},
			Limitations: []string{"nothing was learned from this build file; targets, properties and javac settings in it are unknown"},
		})
	}

	versions := map[string]*verAgg{}
	var javacTotal, noSource, noTarget int
	var noLevelLocs locSet
	var antBuildSource, antBuildTarget bool
	packs := map[string]*packAgg{}
	envs := map[string]*envAgg{}
	seenImports := map[string]bool{}

	for _, ap := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		emitProject(sink, ap)
		if _, ok := ap.Props["ant.build.javac.source"]; ok {
			antBuildSource = true
		}
		if _, ok := ap.Props["ant.build.javac.target"]; ok {
			antBuildTarget = true
		}

		ap.EachTask(func(t *ant.Task) {
			if !t.TaskPos || len(t.Expansion) > 0 {
				return
			}
			switch name := strings.ToLower(t.Name); name {
			case "javac":
				javacTotal++
				_, hasSrc := t.Raw["source"]
				_, hasTgt := t.Raw["target"]
				_, hasRel := t.Raw["release"]
				if !hasSrc && !hasRel {
					noSource++
				}
				if !hasTgt && !hasRel {
					noTarget++
				}
				if (!hasSrc && !hasRel) || (!hasTgt && !hasRel) {
					noLevelLocs.add(t.Loc)
				}
				for _, kind := range []struct{ attr, kind string }{
					{"source", model.KindJavaSource}, {"target", model.KindJavaTarget}, {"release", model.KindJavaRelease},
				} {
					if _, present := t.Raw[kind.attr]; present {
						recordVersion(versions, ap, t, kind.attr, kind.kind)
					}
				}
			case "jar", "war", "ear", "ejbjar":
				pa := packs[name]
				if pa == nil {
					pa = &packAgg{}
					packs[name] = pa
				}
				pa.count++
				pa.locs.add(t.Loc)
				for _, a := range []string{"destfile", "destdir", "jarfile", "warfile", "earfile"} {
					if v, raw, present, ok := t.Value(a); present {
						if ok {
							pa.dests = addUnique(pa.dests, v)
						} else {
							pa.dests = addUnique(pa.dests, raw+" (unresolved)")
						}
						break
					}
				}
			}
		})

		for _, ref := range ap.EnvRefs {
			ea := envs[ref.Var]
			if ea == nil {
				ea = &envAgg{}
				envs[ref.Var] = ea
			}
			ea.locs.add(ref.Loc)
			ea.uses++
			ea.prefixes = addUnique(ea.prefixes, ref.Prefix)
		}

		for _, u := range ap.Unresolved {
			k := u.Raw + "|" + u.Loc.String()
			if seenImports[k] {
				continue
			}
			seenImports[k] = true
			subject := u.Raw
			if subject == "" {
				subject = "<no file attribute>"
			}
			sink.Emit(model.Evidence{
				Category: model.CatBuild, Kind: model.KindAntImportUnresolved, Subject: subject,
				Finding: fmt.Sprintf("Ant <%s> of %q could not be followed: %s", u.Kind, subject, u.Reason),
				Status:  model.Warn, Confidence: model.Observed, Severity: model.SevMedium,
				Locations: []model.Location{{Path: u.Loc.File, Line: u.Loc.Line}},
				Values: map[string]string{
					"raw": u.Raw, "reason": u.Reason, "kind": u.Kind, "optional": strconv.FormatBool(u.Optional),
				},
				Limitations: []string{"targets, properties and tasks defined by the unresolved file are not visible to the analysis"},
			})
		}
	}

	emitVersions(sink, versions)

	if javacTotal > 0 {
		locs, total := noLevelLocs.capped()
		vals := map[string]string{
			"javac_total": strconv.Itoa(javacTotal), "no_source": strconv.Itoa(noSource), "no_target": strconv.Itoa(noTarget),
			"locations_total": strconv.Itoa(total),
		}
		var limits []string
		if antBuildSource || antBuildTarget {
			vals["ant_build_javac_source_defined"] = strconv.FormatBool(antBuildSource)
			vals["ant_build_javac_target_defined"] = strconv.FormatBool(antBuildTarget)
			limits = append(limits, "ant.build.javac.source/target are defined and supply the default level for javac tasks that omit source/target")
		} else {
			limits = append(limits, "a javac without source/target compiles at the default level of the JDK that runs the build, which is not known statically")
		}
		st := model.Info
		if noSource > 0 || noTarget > 0 {
			st = model.Warn
		}
		sink.Emit(model.Evidence{
			Category: model.CatJavaVersion, Kind: model.KindJavaImplicit, Subject: "javac",
			Finding: fmt.Sprintf("%d javac task(s) in total; %d set no source level and %d set no target level (release counts as both)", javacTotal, noSource, noTarget),
			Status:  st, Confidence: model.Derived, Locations: locs, Values: vals, Limitations: limits,
		})
	}

	var pnames []string
	for n := range packs {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	for _, n := range pnames {
		pa := packs[n]
		locs, total := pa.locs.capped()
		sort.Strings(pa.dests)
		shown := pa.dests
		if len(shown) > 10 {
			shown = shown[:10]
		}
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindAntPackaging, Subject: n,
			Finding: fmt.Sprintf("Ant build files define %d %s packaging task(s) at %d location(s)", pa.count, n, total),
			Status:  model.Info, Confidence: model.Observed, Locations: locs,
			Values: map[string]string{
				"count": strconv.Itoa(pa.count), "destinations": strings.Join(shown, ", "), "locations_total": strconv.Itoa(total),
			},
		})
	}

	var enames []string
	for n := range envs {
		enames = append(enames, n)
	}
	sort.Strings(enames)
	for _, n := range enames {
		ea := envs[n]
		locs, total := ea.locs.capped()
		sort.Strings(ea.prefixes)
		sink.Emit(model.Evidence{
			Category: model.CatEnvironment, Kind: model.KindAntEnvRef, Subject: n,
			Finding: fmt.Sprintf("Ant build files read environment variable %s (%d reference(s)); its value is not known at analysis time", n, ea.uses),
			Status:  model.Info, Confidence: model.Observed, Severity: model.SevLow, Locations: locs,
			Values: map[string]string{
				"var": n, "prefix": strings.Join(ea.prefixes, ","), "uses": strconv.Itoa(ea.uses), "locations_total": strconv.Itoa(total),
			},
			Limitations: []string{"the value depends on the environment of whoever runs the build"},
		})
	}
	return nil
}

func emitProject(sink engine.Sink, ap *ant.Project) {
	own := 0
	for _, n := range ap.TargetOrder {
		if t := ap.Targets[n]; t != nil && t.File == ap.File {
			own++
		}
	}
	vals := map[string]string{
		"default": ap.Default, "targets": strconv.Itoa(own), "targets_total": strconv.Itoa(len(ap.TargetOrder)),
		"imports": strconv.Itoa(len(ap.Imports)), "unresolved_imports": strconv.Itoa(len(ap.Unresolved)),
		"name": ap.Name, "files_parsed": strconv.Itoa(len(ap.Files)),
	}
	if n := len(ap.MissingPropertyFiles); n > 0 {
		vals["missing_property_files"] = strconv.Itoa(n)
	}
	if n := len(ap.EnvPrefixes); n > 0 {
		var ps []string
		for k := range ap.EnvPrefixes {
			ps = append(ps, k)
		}
		sort.Strings(ps)
		vals["env_prefixes"] = strings.Join(ps, ",")
	}
	var limits []string
	limits = append(limits, ap.Warnings...)
	for i, m := range ap.MissingPropertyFiles {
		if i == 5 {
			limits = append(limits, fmt.Sprintf("... and %d more property files not read", len(ap.MissingPropertyFiles)-i))
			break
		}
		limits = append(limits, fmt.Sprintf("property file %q (%s) was not read: %s; properties it defines are unknown", m.Raw, m.Loc, m.Reason))
	}
	st := model.Info
	if len(ap.Warnings) > 0 {
		st = model.Warn
	}
	def := fmt.Sprintf("default target %q", ap.Default)
	if ap.Default == "" {
		def = "no default target"
	} else if _, ok := ap.Targets[ap.Default]; !ok {
		def += " (not defined in the loaded files)"
	}
	sink.Emit(model.Evidence{
		Category: model.CatBuild, Kind: model.KindAntProject, Subject: ap.File,
		Finding: fmt.Sprintf("Ant build file %s declares %d target(s) (%d including imports), %s, and follows %d import(s)", ap.File, own, len(ap.TargetOrder), def, len(ap.Imports)),
		Status:  st, Confidence: model.Observed, Locations: []model.Location{{Path: ap.File}},
		Values: vals, Limitations: limits,
	})
}

func recordVersion(versions map[string]*verAgg, ap *ant.Project, t *ant.Task, attr, kind string) {
	val, raw, _, ok := t.Value(attr)
	literal := !strings.Contains(raw, "${") && !strings.Contains(raw, "@{")
	var conf model.Confidence
	switch {
	case literal:
		conf = model.Observed
	case ok:
		conf = model.Derived
	default:
		conf = model.Unknown
	}
	resolved := val
	key := kind + "|" + resolved
	if !ok {
		key = kind + "|?" + raw
	}
	va := versions[key]
	if va == nil {
		va = &verAgg{kind: kind, resolved: resolved, known: ok, conf: conf}
		if !ok {
			va.resolved = raw
		}
		versions[key] = va
	}
	va.conf = weaker(va.conf, conf)
	va.raws = addUnique(va.raws, raw)
	va.locs.add(t.Loc)
	va.javacCount++
	if va.definedAt == "" {
		if literal {
			va.definedAt = t.Loc.String()
		} else {
			var where []string
			for _, ref := range refsOf(raw) {
				if pr := ap.Props[ref]; pr != nil && !pr.Loc.IsZero() {
					where = addUnique(where, pr.Loc.String())
				}
			}
			va.definedAt = strings.Join(where, ",")
		}
	}
	if !ok {
		for _, ref := range refsOf(val) {
			pr := ap.Props[ref]
			switch {
			case pr != nil && pr.Conditional:
				lim := fmt.Sprintf("property %s is set conditionally (%s at %s) so its value is not known statically", ref, pr.Origin, pr.Loc)
				if len(pr.Alternatives) > 0 {
					lim += "; later definitions that apply if the condition fails: " + strings.Join(pr.Alternatives, ", ")
				}
				va.limits = addUnique(va.limits, lim)
			case ref == "java.version" || ref == "ant.java.version" || strings.HasPrefix(ref, "java."):
				va.limits = addUnique(va.limits, fmt.Sprintf("${%s} depends on the JDK that runs Ant", ref))
			case ap.EnvPrefixes != nil && strings.HasPrefix(ref, "env."):
				va.limits = addUnique(va.limits, fmt.Sprintf("${%s} depends on the environment of whoever runs the build", ref))
			default:
				va.limits = addUnique(va.limits, fmt.Sprintf("property ${%s} is not defined in any loaded build file or property file (it may come from -D, the environment or a missing file)", ref))
			}
		}
	}
}

func emitVersions(sink engine.Sink, versions map[string]*verAgg) {
	keys := make([]string, 0, len(versions))
	for k := range versions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	label := map[string]string{model.KindJavaSource: "source", model.KindJavaTarget: "target", model.KindJavaRelease: "release"}
	for _, k := range keys {
		va := versions[k]
		locs, total := va.locs.capped()
		raw := va.raws
		if len(raw) > 5 {
			raw = raw[:5]
		}
		vals := map[string]string{
			"raw": strings.Join(raw, ", "), "origin": "ant", "defined_at": va.definedAt,
			"locations_total": strconv.Itoa(total), "javac_tasks": strconv.Itoa(va.javacCount),
		}
		ev := model.Evidence{
			Category: model.CatJavaVersion, Kind: va.kind, Subject: va.resolved,
			Confidence: va.conf, Locations: locs, Values: vals, Limitations: va.limits,
		}
		if va.known {
			vals["resolved"] = va.resolved
			ev.Status = model.Info
			how := "set literally"
			if va.conf == model.Derived {
				how = "resolved through Ant properties"
			}
			ev.Finding = fmt.Sprintf("javac %s level %s is requested by %d javac task(s) in %d location(s) (%s)", label[va.kind], va.resolved, va.javacCount, total, how)
		} else {
			vals["resolved"] = ""
			ev.Status = model.Warn
			ev.Finding = fmt.Sprintf("javac %s level %q could not be resolved statically (%d javac task(s))", label[va.kind], va.resolved, va.javacCount)
		}
		sink.Emit(ev)
	}
}
