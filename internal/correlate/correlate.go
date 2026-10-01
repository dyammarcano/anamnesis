// Package correlate turns normalized evidence into reportable conclusions. Every conclusion lists
// the evidence IDs it rests on and never claims more confidence than its weakest input.
// Semantics are generic: nothing here knows about any particular project.
package correlate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"anamnesis/internal/model"
)

func rank(c model.Confidence) int {
	switch c {
	case model.Observed:
		return 3
	case model.Derived:
		return 2
	case model.Inferred:
		return 1
	default:
		return 0
	}
}

// weakest returns the lowest confidence among evs, starting from limit (the most the conclusion may claim).
func weakest(limit model.Confidence, evs []model.Evidence) model.Confidence {
	cur := limit
	for _, e := range evs {
		if rank(e.Confidence) < rank(cur) {
			cur = e.Confidence
		}
	}
	return cur
}

func uniqIDs(evs []model.Evidence) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, e := range evs {
		if e.ID != "" && !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e.ID)
		}
	}
	sort.Strings(out)
	return out
}

type builder struct{ out []model.Conclusion }

func (b *builder) add(topic string, kind model.ConclusionKind, limit model.Confidence, statement, value string, evs []model.Evidence, assumptions, limitations []string) {
	conf := weakest(limit, evs)
	if kind == model.KindUnknown {
		conf = model.Unknown
	}
	b.out = append(b.out, model.Conclusion{
		ID: "C-" + topic, Topic: topic, Statement: statement, Value: value, Kind: kind, Confidence: conf,
		EvidenceIDs: uniqIDs(evs), Assumptions: assumptions, Limitations: limitations,
	})
}

func val(e model.Evidence, k string) string { return strings.TrimSpace(e.Values[k]) }

func loc(e model.Evidence) string {
	if len(e.Locations) == 0 {
		return ""
	}
	l := e.Locations[0]
	if l.Line > 0 {
		return l.Path + ":" + strconv.Itoa(l.Line)
	}
	return l.Path
}

func sortBySubject(evs []model.Evidence) []model.Evidence {
	out := append([]model.Evidence(nil), evs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return loc(out[i]) < loc(out[j])
	})
	return out
}

func cat(lists ...[]model.Evidence) []model.Evidence {
	var out []model.Evidence
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// latest keeps only the "prepared" snapshot when evidence from both snapshots is present, so
// conclusions describe the environment as prepared; the "found" items stay in the evidence file.
func latest(evs []model.Evidence) []model.Evidence {
	prepared := false
	for _, e := range evs {
		if e.Values["snapshot"] == "prepared" {
			prepared = true
			break
		}
	}
	if !prepared {
		return evs
	}
	var out []model.Evidence
	for _, e := range evs {
		if e.Values["snapshot"] != "found" {
			out = append(out, e)
		}
	}
	return out
}

func dflt(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// Correlate computes the conclusions for one project from its evidence.
func Correlate(ev []model.Evidence) []model.Conclusion {
	by := map[string][]model.Evidence{}
	for _, e := range ev {
		by[e.Kind] = append(by[e.Kind], e)
	}
	b := &builder{}
	projectIdentity(b, by)
	buildSystems(b, by)
	javaDeclared(b, by)
	javaBinaries(b, by)
	javaLocalRuntime(b, by)
	javaGeneration(b, by)
	enterprise(b, by)
	technologies(b, by)
	dependencies(b, by)
	ciSummary(b, by)
	buildability(b, by)
	environmentMissing(b, by)
	environmentPrepared(b, by)
	safety(b, by)
	migrationMTA(b, by)
	migrationEffort(b, by)
	analyzersFailed(b, by)
	unknowns(b, ev)
	return b.out
}

// ---- identity -------------------------------------------------------------------------------

func projectIdentity(b *builder, by map[string][]model.Evidence) {
	id, git := by[model.KindIdentity], by[model.KindGitRepository]
	evs := cat(id, git)
	if len(evs) == 0 {
		b.add("project.identity", model.KindUnknown, model.Unknown, "No project identity or git evidence was produced.", "", nil, nil, nil)
		return
	}
	var parts []string
	for _, e := range id {
		parts = append(parts, fmt.Sprintf("%s files, %s Java files (%s lines), %s jars, %s wars, %s ears",
			dflt(val(e, "files")), dflt(val(e, "java_files")), dflt(val(e, "java_lines")), dflt(val(e, "jars")), dflt(val(e, "wars")), dflt(val(e, "ears"))))
	}
	for _, e := range git {
		if val(e, "state") == "none" {
			parts = append(parts, "not a git repository")
			continue
		}
		s := "git (" + dflt(val(e, "state")) + ")"
		if v := val(e, "sha"); v != "" {
			s += " at " + v
		}
		if v := val(e, "branch"); v != "" {
			s += " on " + v
		}
		if v := val(e, "dirty_count"); v != "" && v != "0" {
			s += ", " + v + " uncommitted changes"
		}
		parts = append(parts, s)
	}
	b.add("project.identity", model.KindFact, model.Observed, "Project identity: "+strings.Join(parts, "; ")+".", "", evs, nil, nil)
}

// ---- build systems --------------------------------------------------------------------------

func buildSystems(b *builder, by map[string][]model.Evidence) {
	sys, abs := sortBySubject(by[model.KindBuildSystem]), sortBySubject(by[model.KindBuildSystemAbsent])
	evs := cat(sys, abs)
	if len(evs) == 0 {
		b.add("build.systems", model.KindUnknown, model.Unknown, "No build-system evidence was produced (the detection analyzer did not run or failed).", "", nil, nil,
			[]string{"absence of evidence here is not evidence that no build system exists"})
		return
	}
	var found, names []string
	for _, e := range sys {
		s := e.Subject
		if val(e, "at_root") == "true" {
			s += " (at repository root"
		} else {
			s += " (not at root"
		}
		if c := val(e, "count"); c != "" {
			s += ", " + c + " file(s)"
		}
		found = append(found, s+")")
		names = append(names, e.Subject)
	}
	var absent []string
	for _, e := range abs {
		absent = append(absent, e.Subject)
	}
	st := "Build systems found: "
	if len(found) == 0 {
		st += "none"
	} else {
		st += strings.Join(found, ", ")
	}
	st += "."
	if len(absent) > 0 {
		st += " Not found: " + strings.Join(absent, ", ") + "."
	}
	b.add("build.systems", model.KindFact, model.Observed, st, strings.Join(names, ","), evs, nil, nil)
}

// ---- java versions --------------------------------------------------------------------------

type decl struct {
	kind, resolved, origin, at string
	ev                         model.Evidence
}

func declared(by map[string][]model.Evidence) []decl {
	var out []decl
	for _, k := range []struct{ kind, label string }{{model.KindJavaSource, "source"}, {model.KindJavaTarget, "target"}, {model.KindJavaRelease, "release"}} {
		for _, e := range sortBySubject(by[k.kind]) {
			r := val(e, "resolved")
			if r == "" {
				r = val(e, "raw")
			}
			at := val(e, "defined_at")
			if at == "" {
				at = loc(e)
			}
			out = append(out, decl{kind: k.label, resolved: r, origin: val(e, "origin"), at: at, ev: e})
		}
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	var vs []string
	for v := range m {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

func javaDeclared(b *builder, by map[string][]model.Evidence) {
	ds := declared(by)
	impl := by[model.KindJavaImplicit]
	var evs []model.Evidence
	for _, d := range ds {
		evs = append(evs, d.ev)
	}
	evs = append(evs, impl...)
	if len(evs) == 0 {
		b.add("java.declared", model.KindUnknown, model.Unknown, "No declared Java source/target/release value was found in any build file or IDE metadata.", "", nil, nil, nil)
		return
	}
	var lines, vals []string
	perKind := map[string]map[string][]string{}
	for _, d := range ds {
		lines = append(lines, fmt.Sprintf("%s %s [%s %s]", d.kind, dflt(d.resolved), dflt(d.origin), dflt(d.at)))
		if perKind[d.kind] == nil {
			perKind[d.kind] = map[string][]string{}
		}
		perKind[d.kind][d.resolved] = append(perKind[d.kind][d.resolved], dflt(d.origin)+" "+dflt(d.at))
	}
	var conflicts []string
	for _, k := range []string{"source", "target", "release"} {
		m := perKind[k]
		if len(m) == 0 {
			continue
		}
		vs := sortedKeys(m)
		if len(vs) > 1 {
			var parts []string
			for _, v := range vs {
				parts = append(parts, fmt.Sprintf("%s (%s)", v, strings.Join(m[v], "; ")))
			}
			conflicts = append(conflicts, fmt.Sprintf("CONFLICT in %s: %s", k, strings.Join(parts, " vs ")))
		}
		vals = append(vals, k+"="+strings.Join(vs, "|"))
	}
	st := "Declared Java levels (as written in project files): " + strings.Join(lines, "; ") + "."
	if len(lines) == 0 {
		st = "No explicit source/target/release is declared."
	}
	for _, e := range impl {
		st += fmt.Sprintf(" javac invocations: %s total, %s without explicit source, %s without explicit target.", dflt(val(e, "javac_total")), dflt(val(e, "no_source")), dflt(val(e, "no_target")))
	}
	var lim []string
	if len(conflicts) > 0 {
		st += " " + strings.Join(conflicts, ". ") + "."
		lim = append(lim, "declared values conflict; they are listed side by side and not merged")
	}
	b.add("java.declared", model.KindEvidence, model.Observed, st, strings.Join(vals, ";"), evs, nil, lim)
}

func majorToJava(m int) int { return m - 44 }

func maxMajor(evs []model.Evidence) int {
	best := 0
	for _, e := range evs {
		if n, err := strconv.Atoi(val(e, "max_major")); err == nil && n > best {
			best = n
		}
	}
	return best
}

func javaBinaries(b *builder, by map[string][]model.Evidence) {
	cm, mf := by[model.KindJavaClassMajor], by[model.KindJavaManifestJDK]
	evs := cat(cm, mf)
	if len(evs) == 0 {
		b.add("java.binaries", model.KindUnknown, model.Unknown, "No class-file evidence was produced (no binaries found or inspector did not run).", "", nil, nil, nil)
		return
	}
	st, value := "Class-file majors were not established.", ""
	if m := maxMajor(cm); m > 0 {
		st = fmt.Sprintf("Highest class-file major version found in project binaries: %d (Java %d).", m, majorToJava(m))
		value = strconv.Itoa(m)
	}
	for _, e := range mf {
		if d := val(e, "distribution"); d != "" {
			st += " Build-JDK values in jar manifests: " + d + "."
		}
	}
	b.add("java.binaries", model.KindEvidence, model.Observed, st, value, evs, nil,
		[]string{"binaries may be third-party libraries; they bound what the project can run on, not what it declares"})
}

func javaLocalRuntime(b *builder, by map[string][]model.Evidence) {
	rt := latest(by[model.KindJavaLocalRuntime])
	if len(rt) == 0 {
		b.add("java.local-runtime", model.KindUnknown, model.Unknown, "LOCAL ENVIRONMENT: no Java runtime was detected or probed on this machine.", "", nil, nil,
			[]string{"describes this machine only, not the project"})
		return
	}
	var parts, vs []string
	for _, e := range rt {
		s := "JDK " + dflt(val(e, "version"))
		if v := val(e, "java_home"); v != "" {
			s += " (JAVA_HOME " + v + ")"
		}
		parts = append(parts, s)
		vs = append(vs, dflt(val(e, "major")))
	}
	b.add("java.local-runtime", model.KindFact, model.Observed,
		"LOCAL ENVIRONMENT (this machine, not a property of the project): "+strings.Join(parts, "; ")+".", strings.Join(vs, ","), rt, nil,
		[]string{"describes this machine only; it is never used to infer the project's Java generation"})
}

// parseJava reads "1.6", "6", "1.8", "11" as a Java feature version; 0 when unparsable.
func parseJava(s string) int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "1.")
	if i := strings.IndexAny(s, ".-_"); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 99 {
		return 0
	}
	return n
}

func javaGeneration(b *builder, by map[string][]model.Evidence) {
	ds := declared(by)
	pick := func(kind string) (int, []model.Evidence) {
		best := 0
		var evs []model.Evidence
		for _, d := range ds {
			if d.kind != kind {
				continue
			}
			if n := parseJava(d.resolved); n > 0 {
				evs = append(evs, d.ev)
				if n > best {
					best = n
				}
			}
		}
		return best, evs
	}
	dec, decLabel := 0, ""
	var evs []model.Evidence
	for _, k := range []string{"target", "release", "source"} {
		if n, e := pick(k); n > 0 {
			dec, evs, decLabel = n, e, k
			break
		}
	}
	bin := 0
	cm := by[model.KindJavaClassMajor]
	if m := maxMajor(cm); m >= 45 {
		bin = majorToJava(m)
		evs = append(evs, cm...)
	}
	lim := []string{"the local JDK is deliberately not used for this inference"}
	if dec == 0 && bin == 0 {
		b.add("java.generation", model.KindUnknown, model.Unknown,
			"Java generation of the project cannot be inferred: there is no declared target and no binary class-file evidence.", "", nil, nil, lim)
		return
	}
	level := dec
	var sig []string
	if dec > 0 {
		sig = append(sig, fmt.Sprintf("declared %s Java %d", decLabel, dec))
	} else {
		level = bin
	}
	if bin > 0 {
		sig = append(sig, fmt.Sprintf("binaries up to Java %d", bin))
	}
	if dec > 0 && bin > 0 && dec != bin {
		lim = append(lim, fmt.Sprintf("declared (Java %d) and binary (Java %d) evidence differ; the declared value is used", dec, bin))
	}
	class, vclass := "legacy", "LEGACY"
	if level >= 9 {
		class, vclass = "modern", "MODERN"
	}
	var assum []string
	if dec == 0 {
		assum = append(assum, "no declared level exists; binaries are taken as indicative of the project")
	}
	b.add("java.generation", model.KindInference, model.Inferred,
		fmt.Sprintf("Inferred %s Java generation: Java %d (%s).", class, level, strings.Join(sig, "; ")),
		fmt.Sprintf("%s_JAVA_%d", vclass, level), evs, assum, lim)
}

// ---- enterprise / technologies / deps / ci ----------------------------------------------------

func enterprise(b *builder, by map[string][]model.Evidence) {
	desc, srv := sortBySubject(by[model.KindEnterpriseDescriptor]), sortBySubject(by[model.KindEnterpriseAppServer])
	jee, tech := by[model.KindEnterpriseJavaEE], by[model.KindTechUsage]
	evs := cat(desc, srv, jee, tech)
	if len(evs) == 0 {
		b.add("enterprise", model.KindUnknown, model.Unknown, "No enterprise-Java evidence was produced.", "", nil, nil, nil)
		return
	}
	var parts []string
	detected := ""
	for _, e := range jee {
		detected = val(e, "detected")
		parts = append(parts, "Java EE detected="+dflt(detected))
	}
	if len(srv) > 0 {
		var s []string
		for _, e := range srv {
			s = append(s, e.Subject)
		}
		parts = append(parts, "application-server families: "+strings.Join(s, ", "))
	}
	if len(desc) > 0 {
		var s []string
		for _, e := range desc {
			s = append(s, e.Subject)
		}
		parts = append(parts, "descriptors: "+strings.Join(s, ", "))
	}
	if len(tech) > 0 {
		parts = append(parts, fmt.Sprintf("%d technologies in use", len(tech)))
	}
	b.add("enterprise", model.KindInference, model.Inferred, "Enterprise Java indicators: "+strings.Join(parts, "; ")+".", detected, evs,
		[]string{"descriptor and server-specific files indicate historical targets, not the current runtime"}, nil)
}

func technologies(b *builder, by map[string][]model.Evidence) {
	tech := by[model.KindTechUsage]
	if len(tech) == 0 {
		b.add("technologies", model.KindUnknown, model.Unknown, "No technology-usage evidence was produced.", "", nil, nil, nil)
		return
	}
	occ := func(e model.Evidence) int { n, _ := strconv.Atoi(val(e, "occurrences")); return n }
	ts := append([]model.Evidence(nil), tech...)
	sort.SliceStable(ts, func(i, j int) bool {
		if occ(ts[i]) != occ(ts[j]) {
			return occ(ts[i]) > occ(ts[j])
		}
		return ts[i].Subject < ts[j].Subject
	})
	var items, names []string
	for _, e := range ts {
		items = append(items, fmt.Sprintf("%s (%s occurrences in %s files)", e.Subject, dflt(val(e, "occurrences")), dflt(val(e, "files"))))
		names = append(names, e.Subject)
	}
	b.add("technologies", model.KindEvidence, model.Observed, "Technologies referenced by project sources: "+strings.Join(items, "; ")+".", strings.Join(names, ","), ts, nil,
		[]string{"counted from imports and descriptors; a reference does not prove the technology is exercised at runtime"})
}

func dependencies(b *builder, by map[string][]model.Evidence) {
	inv, dup, old := by[model.KindDepsInventory], sortBySubject(by[model.KindDepsDuplicate]), sortBySubject(by[model.KindDepsOld])
	evs := cat(inv, dup, old)
	if len(evs) == 0 {
		b.add("dependencies", model.KindUnknown, model.Unknown, "No dependency-inventory evidence was produced.", "", nil, nil, nil)
		return
	}
	var parts []string
	for _, e := range inv {
		parts = append(parts, fmt.Sprintf("%s jars, %s wars, %s ears (%s bytes)", dflt(val(e, "jars")), dflt(val(e, "wars")), dflt(val(e, "ears")), dflt(val(e, "total_bytes"))))
	}
	if len(dup) > 0 {
		parts = append(parts, fmt.Sprintf("%d artifacts present in several versions", len(dup)))
	}
	if len(old) > 0 {
		parts = append(parts, fmt.Sprintf("%d artifacts flagged old", len(old)))
	}
	b.add("dependencies", model.KindFact, model.Observed, "Bundled binaries: "+strings.Join(parts, "; ")+".", "", evs, nil, nil)
}

func ciSummary(b *builder, by map[string][]model.Evidence) {
	wf, same := sortBySubject(by[model.KindCIWorkflow]), by[model.KindCISameSHA]
	evs := cat(wf, same)
	if len(evs) == 0 {
		b.add("ci", model.KindUnknown, model.Unknown, "No CI evidence was produced.", "", nil, nil, nil)
		return
	}
	var parts []string
	for _, e := range wf {
		parts = append(parts, fmt.Sprintf("%s (purpose %s)", e.Subject, dflt(val(e, "purpose"))))
	}
	var st strings.Builder
	st.WriteString("CI workflows: ")
	if len(parts) == 0 {
		st.WriteString("none found")
	} else {
		st.WriteString(strings.Join(parts, ", "))
	}
	st.WriteString(".")
	var states []string
	for _, e := range same {
		states = append(states, dflt(val(e, "state")))
		if u := val(e, "run_url"); u != "" {
			st.WriteString(" Run: " + u + ".")
		}
	}
	if len(states) > 0 {
		st.WriteString(" Same-SHA CI state: " + strings.Join(states, ", ") + ".")
	}
	b.add("ci", model.KindFact, model.Observed, st.String(), strings.Join(states, ","), evs, nil, nil)
}

// ---- buildability ---------------------------------------------------------------------------

var environmentalClass = map[string]bool{
	model.BuildJDKIncompatible: true, model.BuildMissingEnvVar: true, model.BuildMissingRepository: true,
	model.BuildMissingAppServer: true, model.BuildToolNotAvailable: true, model.BuildUnsafeTarget: true,
	model.BuildTimeout: true, model.BuildNotAttemptedNoDecision: true,
}

func buildability(b *builder, by map[string][]model.Evidence) {
	var notAttempted, success, partial, failedLocal []model.Evidence
	for _, e := range by[model.KindBuildLocal] {
		switch {
		case val(e, "attempted") != "true":
			notAttempted = append(notAttempted, e)
		case val(e, "class") == model.BuildSuccess && val(e, "full_build") == "true":
			success = append(success, e)
		case val(e, "class") == model.BuildSuccess:
			// an auxiliary target succeeded; it says nothing about the application as a whole
			partial = append(partial, e)
		default:
			failedLocal = append(failedLocal, e)
		}
	}
	var ciSuccess, ciFailed, ciAll []model.Evidence
	for _, e := range by[model.KindCISameSHA] {
		ciAll = append(ciAll, e)
		switch val(e, "state") {
		case "SUCCESS_SAME_SHA":
			ciSuccess = append(ciSuccess, e)
		case "FAILED_SAME_SHA":
			ciFailed = append(ciFailed, e)
		}
	}
	hasBuildSystem := len(by[model.KindBuildSystem]) > 0 || len(by[model.KindAntProject]) > 0
	var skipped []model.Evidence
	for _, e := range by[model.KindAnalyzerSkipped] {
		if strings.Contains(e.Subject, "localbuild") || strings.Contains(e.Subject, "buildrun") {
			skipped = append(skipped, e)
		}
	}

	// local reproducibility axis
	var localValue, localStmt string
	var localEvs []model.Evidence
	switch {
	case len(success) > 0:
		e := success[0]
		localValue = "REPRODUCED"
		localStmt = fmt.Sprintf("Local build reproduced: %s %s succeeded.", dflt(val(e, "tool")), val(e, "target"))
		localEvs = success
	case len(failedLocal) > 0:
		e := failedLocal[0]
		class := dflt(val(e, "class"))
		localValue = "FAILED:" + class
		tool := ""
		if t := val(e, "tool"); t != "" {
			tool = ": " + t
		}
		localStmt = fmt.Sprintf("Local build attempted and not reproduced (%s%s, exit %s).", class, tool, dflt(val(e, "exit_code")))
		if environmentalClass[class] {
			localStmt += " This failure class is environmental and does not show the source is unbuildable."
		} else {
			localStmt += " The failure may originate in the source or in the local environment; the failure class and log decide which."
		}
		localEvs = failedLocal
	case len(partial) > 0:
		e := partial[0]
		localValue = "PARTIAL:" + dflt(val(e, "target"))
		localStmt = fmt.Sprintf("Only a partial local build was reproduced: target %q succeeded, but it is not the project's default target (%s), so the application as a whole was not built. This does not show the source is unbuildable.",
			val(e, "target"), dflt(val(e, "default_target")))
		localEvs = partial
	case len(notAttempted) > 0:
		e := notAttempted[0]
		reason := val(e, "class")
		if reason == "" {
			reason = "UNSPECIFIED"
		}
		tool := ""
		if t := val(e, "tool"); t != "" {
			tool = ": " + t
		}
		localValue = "NOT_ATTEMPTED:" + reason
		localStmt = fmt.Sprintf("Local build not reproduced (%s%s). This does not show the source is unbuildable.", reason, tool)
		localEvs = notAttempted
	case len(skipped) > 0:
		localValue = "NOT_ATTEMPTED:SIDE_EFFECT_NOT_ENABLED"
		localStmt = "Local build not reproduced (SIDE_EFFECT_NOT_ENABLED: the build capability was not enabled for this run). This does not show the source is unbuildable."
		localEvs = skipped
	default:
		localValue = "INCOMPLETE"
		localStmt = "No local build evidence was produced, so local reproducibility is incomplete. This does not show the source is unbuildable."
	}
	var missing []model.Evidence
	for _, e := range latest(by[model.KindPrereqTool]) {
		if s := val(e, "state"); s == "MISSING" || s == "INCOMPATIBLE" {
			missing = append(missing, e)
		}
	}
	if localValue != "REPRODUCED" && len(missing) > 0 {
		var names []string
		for _, e := range sortBySubject(missing) {
			names = append(names, e.Subject+" "+strings.ToLower(val(e, "state")))
		}
		localStmt += " Local prerequisites: " + strings.Join(names, ", ") + "."
		localEvs = cat(localEvs, missing)
	}
	lkind := model.KindFact
	if localValue == "INCOMPLETE" {
		lkind = model.KindUnknown
	}
	b.add("buildability.local-reproducibility", lkind, model.Observed, localStmt, localValue, localEvs, nil,
		[]string{"describes this machine's ability to build, not the source's"})

	// source axis
	var srcValue, srcStmt string
	var srcEvs []model.Evidence
	srcKind := model.KindInference
	switch {
	case len(success) > 0:
		srcValue, srcKind = "PROVEN_LOCALLY", model.KindFact
		srcStmt = "Source buildability proven locally: a build of the project succeeded on this machine."
		srcEvs = success
	case len(ciSuccess) > 0:
		srcValue, srcKind = "PROVEN_IN_CI_SAME_SHA", model.KindFact
		srcStmt = "Source buildability proven in CI: a CI run on the same commit succeeded."
		srcEvs = ciSuccess
	case len(ciFailed) > 0 || hasSourceFailure(failedLocal):
		srcValue = "NOT_PROVEN"
		srcStmt = "Source buildability is not proven: a build failed in a way that may involve the source. This is not a finding that the source is unbuildable; the failure class and log must be inspected."
		srcEvs = cat(ciFailed, failedLocal)
	case len(partial) > 0:
		srcValue = "NOT_DISPROVEN"
		srcStmt = fmt.Sprintf("Source buildability is not disproven: only an auxiliary target (%s) was built locally, which says nothing about the application as a whole, and no build was shown to fail because of the source.", val(partial[0], "target"))
		srcEvs = cat(by[model.KindBuildSystem], localEvs)
	case hasBuildSystem:
		srcValue = "NOT_DISPROVEN"
		srcStmt = "Source buildability is not disproven: no build was shown to fail because of the source, and none was shown to succeed either. A missing local tool or a build that was not attempted never counts against the source."
		srcEvs = cat(by[model.KindBuildSystem], localEvs)
	default:
		srcValue = "NOT_PROVEN"
		srcStmt = "Source buildability is not proven: no build system was detected and no build was run. This is not a finding that the source is unbuildable."
		srcEvs = localEvs
	}
	b.add("buildability.source", srcKind, model.Observed, srcStmt, srcValue, srcEvs, nil, nil)

	// combined state
	var state, stStmt string
	var stEvs []model.Evidence
	stKind := model.KindInference
	switch {
	case len(success) > 0:
		state, stKind = "LOCALLY_BUILDABLE", model.KindFact
		stStmt = "Locally buildable: " + localStmt
		stEvs = success
	case len(ciSuccess) > 0:
		state, stKind = "KNOWN_BUILDABLE_IN_CI_LOCAL_NOT_REPRODUCED", model.KindFact
		stStmt = "Known buildable in CI on the same commit; " + lowerFirst(localStmt)
		stEvs = cat(ciSuccess, localEvs)
	case len(failedLocal) > 0:
		state = "BUILD_FAILED_NO_SAME_SHA_CI_PROOF"
		stStmt = "A local build was attempted and failed, and no same-commit CI success exists. " + localStmt
		stEvs = cat(failedLocal, ciAll)
	default:
		state = "NOT_PROVEN"
		stStmt = "Buildability not proven. " + localStmt + " Neither proven nor disproven."
		stEvs = cat(localEvs, ciAll)
	}
	b.add("buildability.state", stKind, model.Observed, stStmt, state, stEvs, nil,
		[]string{"derived from the two axes; a local failure or non-attempt is never reported as 'not buildable'"})
}

func hasSourceFailure(failed []model.Evidence) bool {
	for _, e := range failed {
		switch val(e, "class") {
		case model.BuildSourceOrBuildError, model.BuildMissingDependency, model.BuildUnknown, "":
			return true
		}
	}
	return false
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---- environment / safety ---------------------------------------------------------------------

func environmentMissing(b *builder, by map[string][]model.Evidence) {
	all := sortBySubject(latest(by[model.KindPrereqTool]))
	if len(all) == 0 {
		b.add("environment.missing", model.KindUnknown, model.Unknown, "No prerequisite evidence was produced.", "", nil, nil, nil)
		return
	}
	var miss []model.Evidence
	var lines []string
	for _, e := range all {
		s := val(e, "state")
		if s != "MISSING" && s != "INCOMPATIBLE" {
			continue
		}
		miss = append(miss, e)
		l := fmt.Sprintf("%s %s", e.Subject, s)
		if r := val(e, "requirement"); r != "" {
			l += " (" + strings.ToLower(r)
			if c := val(e, "capability"); c != "" {
				l += " for " + c
			}
			l += ")"
		}
		if r := val(e, "reason"); r != "" {
			l += ": " + r
		}
		if sg := val(e, "suggestion"); sg != "" {
			l += " Suggestion: " + sg
		}
		lines = append(lines, l)
	}
	inst := installedBy(by)
	snap := ""
	for _, e := range all {
		if e.Values["snapshot"] == "prepared" {
			snap = " (environment as prepared)"
			break
		}
	}
	if len(miss) == 0 {
		st := fmt.Sprintf("No missing or incompatible prerequisite among %d probed%s.", len(all), snap)
		if t := inst.text; t != "" {
			st += " " + t
		}
		b.add("environment.missing", model.KindFact, model.Observed, st, "0", cat(all, inst.evs), nil, nil)
		return
	}
	st := "Missing or incompatible prerequisites on this machine" + snap + ": " + strings.Join(lines, "; ")
	if t := inst.text; t != "" {
		st += ". " + t
	}
	b.add("environment.missing", model.KindFact, model.Observed, st, strconv.Itoa(len(miss)), cat(miss, inst.evs), nil,
		[]string{"Anamnesis installs missing tools only when parameters.yaml enables it; otherwise it only suggests"})
}

type installInfo struct {
	text string
	evs  []model.Evidence
}

// installedBy summarizes what the prepare step did (env.install evidence).
func installedBy(by map[string][]model.Evidence) installInfo {
	var parts []string
	var evs []model.Evidence
	for _, e := range sortBySubject(by[model.KindEnvInstall]) {
		st := val(e, "state")
		if e.Subject == "none" {
			continue
		}
		evs = append(evs, e)
		parts = append(parts, fmt.Sprintf("%s %s", e.Subject, dflt(st)))
	}
	if len(parts) == 0 {
		return installInfo{}
	}
	return installInfo{text: "Anamnesis install step: " + strings.Join(parts, ", "), evs: evs}
}

func environmentPrepared(b *builder, by map[string][]model.Evidence) {
	inst, jh, bj := by[model.KindEnvInstall], by[model.KindEnvJavaHome], by[model.KindEnvBuildJDK]
	evs := cat(inst, jh, bj)
	if len(evs) == 0 {
		b.add("environment.prepared", model.KindFact, model.Derived,
			"The environment was not prepared in this run (preflight only, or no preparation was needed or enabled); the environment is reported as found.", "NOT_PREPARED", nil, nil,
			[]string{"derived from the absence of preparation evidence"})
		return
	}
	var parts []string
	for _, e := range sortBySubject(inst) {
		l := fmt.Sprintf("%s: %s", e.Subject, dflt(val(e, "state")))
		if p := val(e, "package"); p != "" {
			l += " (scoop package " + p + ")"
		}
		if e.Command != nil {
			l += fmt.Sprintf(" [exit %d]", e.Command.ExitCode)
		}
		parts = append(parts, l)
	}
	for _, e := range jh {
		parts = append(parts, e.Subject+": "+strings.TrimSpace(e.Finding))
	}
	for _, e := range bj {
		parts = append(parts, "Build JDK: "+strings.TrimSpace(e.Finding))
	}
	// found -> prepared differences of prerequisite tools
	found, prep := map[string]string{}, map[string]string{}
	for _, e := range by[model.KindPrereqTool] {
		switch e.Values["snapshot"] {
		case "found":
			found[e.Subject] = val(e, "state")
		case "prepared":
			prep[e.Subject] = val(e, "state")
		}
	}
	var diffs []string
	for _, t := range sortedKeysS(found) {
		if p, ok := prep[t]; ok && p != found[t] {
			diffs = append(diffs, fmt.Sprintf("%s %s -> %s", t, found[t], p))
		}
	}
	st := "Environment preparation: " + strings.Join(parts, "; ") + "."
	if len(diffs) > 0 {
		st += " Prerequisites found vs prepared: " + strings.Join(diffs, ", ") + "."
	}
	b.add("environment.prepared", model.KindFact, model.Observed, st, "PREPARED", evs, nil,
		[]string{"preparation changes this machine, never the analyzed repository"})
}

func sortedKeysS(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func safety(b *builder, by map[string][]model.Evidence) {
	saf := sortBySubject(by[model.KindAntTargetSafety])
	locals := by[model.KindBuildLocal]
	if len(saf) == 0 && len(locals) == 0 {
		b.add("safety", model.KindUnknown, model.Unknown, "No build-safety or build-execution evidence was produced.", "", nil, nil, nil)
		return
	}
	cnt := map[string]int{}
	names := map[string][]string{}
	var flagged, defaults []model.Evidence
	for _, e := range saf {
		c := val(e, "class")
		cnt[c]++
		// only the root build file's default target goes in the statement; module defaults stay in evidence
		if val(e, "default") == "true" && !strings.Contains(e.Subject, "#") {
			defaults = append(defaults, e)
		}
		if c == "DANGEROUS" || c == "REVIEW_REQUIRED" {
			names[c] = append(names[c], e.Subject)
			flagged = append(flagged, e)
		}
	}
	var parts []string
	if len(saf) > 0 {
		parts = append(parts, fmt.Sprintf("%d Ant targets classified statically from their transitive closure: DANGEROUS %d, REVIEW_REQUIRED %d, SAFE %d, UNKNOWN %d (target names are never evidence of safety)",
			len(saf), cnt["DANGEROUS"], cnt["REVIEW_REQUIRED"], cnt["SAFE"], cnt["UNKNOWN"]))
		for _, d := range defaults {
			reason := val(d, "reasons")
			if i := strings.Index(reason, "; "); i > 0 {
				reason = reason[:i]
			}
			if len(reason) > 300 {
				reason = reason[:300] + "..."
			}
			line := fmt.Sprintf("Default target %s is %s", d.Subject, dflt(val(d, "class")))
			if reason != "" {
				line += ": " + reason
			}
			parts = append(parts, line)
		}
		if l := names["DANGEROUS"]; len(l) > 0 {
			parts = append(parts, "DANGEROUS targets include "+firstN(l, 8))
		}
	}
	var attempted []model.Evidence
	for _, e := range locals {
		if val(e, "attempted") == "true" {
			attempted = append(attempted, e)
		}
	}
	if len(attempted) > 0 {
		var a []string
		for _, e := range attempted {
			a = append(a, fmt.Sprintf("%s %s (%s)", dflt(val(e, "tool")), dflt(val(e, "target")), dflt(val(e, "class"))))
		}
		parts = append(parts, "A local build WAS attempted: "+strings.Join(a, "; "))
	} else {
		parts = append(parts, "No build target was executed automatically in this run")
	}
	b.add("safety", model.KindFact, model.Observed, strings.Join(parts, ". ")+".",
		fmt.Sprintf("dangerous=%d;review=%d;executed=%d", cnt["DANGEROUS"], cnt["REVIEW_REQUIRED"], len(attempted)), cat(defaults, flagged, locals), nil,
		[]string{"per-target classes and full reasons are in the ant.target.safety evidence (evidence.jsonl)"})
}

// ---- migration ------------------------------------------------------------------------------

func migrationMTA(b *builder, by map[string][]model.Evidence) {
	pred, run, cov := by[model.KindMTAPrediction], by[model.KindMTARun], by[model.KindMTACoverage]
	prereq, ruleErr := sortBySubject(by[model.KindMTAPrereq]), by[model.KindMTARuleError]
	evs := cat(pred, run, cov, prereq, ruleErr)
	if len(evs) == 0 {
		b.add("migration.mta", model.KindUnknown, model.Unknown, "No MTA evidence was produced (the migration analyzer did not run or failed).", "", nil, nil, nil)
		return
	}
	var st, value string
	switch {
	case len(run) > 0 && val(run[0], "state") == "SUCCEEDED":
		value = "SUCCEEDED"
		st = fmt.Sprintf("MTA ran successfully (route %s).", dflt(val(run[0], "route")))
	case len(run) > 0 && val(run[0], "state") == "FAILED":
		e := run[0]
		fc := dflt(val(e, "failure_class"))
		value = "FAILED:" + fc
		st = fmt.Sprintf("MTA run FAILED (failure class %s, route %s): %s The failure is preserved as evidence and does not affect other conclusions.", fc, dflt(val(e, "route")), strings.TrimSpace(e.Finding))
	case len(pred) > 0 && val(pred[0], "java_provider") == "NOT_APPLICABLE":
		value = "PREDICTED_NOT_APPLICABLE"
		st = "MTA Java provider is predicted NOT APPLICABLE: " + dflt(val(pred[0], "reason")) + ". MTA was not run."
	case len(pred) > 0 && val(pred[0], "java_provider") == "APPLICABLE":
		value = "NOT_RUN"
		st = "MTA Java provider is predicted applicable; MTA has not been run."
	default:
		value = "NOT_RUN"
		st = "MTA has not been run."
	}
	var lim []string
	for _, e := range cov {
		st += fmt.Sprintf(" Rule coverage: %s of %s rules evaluable; %s of %s effort-bearing rules evaluable.",
			dflt(val(e, "rules_evaluable")), dflt(val(e, "rules_total")), dflt(val(e, "effort_rules_evaluable")), dflt(val(e, "effort_rules_total")))
		lim = append(lim, "results cover only evaluable rules; rules dropped for missing providers are not counted")
	}
	if len(ruleErr) > 0 {
		st += fmt.Sprintf(" %d rule evaluation error(s) were recorded.", len(ruleErr))
	}
	var bad []string
	for _, e := range prereq {
		// OBSERVED items are environment observations (e.g. KANTRA_DIR unset), not requirements.
		if s := val(e, "state"); s == "MISSING" || s == "INCOMPATIBLE" || s == "FAIL" || s == "FAILED" {
			bad = append(bad, e.Subject+"="+s)
		}
	}
	if len(bad) > 0 {
		st += " MTA prerequisites not satisfied: " + strings.Join(bad, ", ") + "."
	}
	b.add("migration.mta", model.KindEvidence, model.Observed, st, value, evs, nil, lim)
}

// EffortSummary aggregates raw MTA effort points. Points are MTA's own unit; they are never hours.
type EffortSummary struct {
	TotalPoints int            `json:"total_points"`
	Rules       int            `json:"rules"`
	Incidents   int            `json:"incidents"`
	ByRule      map[string]int `json:"by_rule"`
	ByTarget    map[string]int `json:"by_target"`
	ByCategory  map[string]int `json:"by_category"`
	ByLabel     map[string]int `json:"by_label"`
	Formula     string         `json:"formula"`
	Unit        string         `json:"unit"`
}

// MTAEffort sums effort x incidents over mta.violation evidence.
func MTAEffort(ev []model.Evidence) EffortSummary {
	s := EffortSummary{ByRule: map[string]int{}, ByTarget: map[string]int{}, ByCategory: map[string]int{}, ByLabel: map[string]int{},
		Formula: "sum over rules of (rule effort points x incident count)", Unit: "MTA effort points (DERIVED; not hours)"}
	for _, e := range ev {
		if e.Kind != model.KindMTAViolation || e.Effort == nil {
			continue
		}
		p := e.Effort.Points * e.Effort.Incidents
		s.TotalPoints += p
		s.Rules++
		s.Incidents += e.Effort.Incidents
		s.ByRule[dflt(e.RuleID)] += p
		t := e.Target
		if t == "" {
			t = "(no target)"
		}
		s.ByTarget[t] += p
		c := val(e, "category")
		if c == "" {
			c = "(none)"
		}
		s.ByCategory[c] += p
		for l := range strings.SplitSeq(val(e, "labels"), ",") {
			if l = strings.TrimSpace(l); l != "" {
				s.ByLabel[l] += p
			}
		}
	}
	return s
}

func sortedPairs(m map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].v != l[j].v {
			return l[i].v > l[j].v
		}
		return l[i].k < l[j].k
	})
	var parts []string
	for _, p := range l {
		parts = append(parts, fmt.Sprintf("%s=%d", p.k, p.v))
	}
	return strings.Join(parts, ", ")
}

func migrationEffort(b *builder, by map[string][]model.Evidence) {
	var viol []model.Evidence
	for _, e := range by[model.KindMTAViolation] {
		if e.Effort != nil {
			viol = append(viol, e)
		}
	}
	hoursLim := []string{"engineering hours are UNKNOWN: no calibration (hours per effort point) has been measured"}
	if len(viol) == 0 {
		b.add("migration.effort", model.KindUnknown, model.Unknown, "No MTA effort was computed: there are no MTA violations with effort in the evidence (see migration.mta for why).", "", nil, nil, hoursLim)
		b.add("migration.hours", model.KindUnknown, model.Unknown, "Engineering hours: UNKNOWN. No calibration exists.", "", nil, nil, nil)
		return
	}
	s := MTAEffort(viol)
	st := fmt.Sprintf("MTA effort: %d effort points (formula: %s; %d rules, %d incidents). By target: %s. By category: %s. Points are MTA's own unit, not hours.",
		s.TotalPoints, s.Formula, s.Rules, s.Incidents, sortedPairs(s.ByTarget), sortedPairs(s.ByCategory))
	b.add("migration.effort", model.KindEvidence, model.Derived, st, strconv.Itoa(s.TotalPoints), viol, nil,
		append(hoursLim, "points cover only rules MTA could evaluate"))
	b.add("migration.hours", model.KindUnknown, model.Unknown, "Engineering hours: UNKNOWN. Effort points are not converted to hours until a measured calibration exists.", "", nil, nil, nil)
}

// ---- analyzer failures and unknowns ------------------------------------------------------------

func analyzersFailed(b *builder, by map[string][]model.Evidence) {
	f, s := sortBySubject(by[model.KindAnalyzerFailed]), sortBySubject(by[model.KindAnalyzerSkipped])
	evs := cat(f, s)
	if len(evs) == 0 {
		b.add("analyzers.failed", model.KindFact, model.Derived, "No analyzer failed or was skipped.", "failed=0;skipped=0", nil, nil,
			[]string{"derived from the absence of failure evidence"})
		return
	}
	var parts []string
	for _, e := range f {
		parts = append(parts, "FAILED "+e.Subject+": "+strings.TrimSpace(e.Finding))
	}
	for _, e := range s {
		parts = append(parts, "SKIPPED "+e.Subject+": "+strings.TrimSpace(e.Finding))
	}
	b.add("analyzers.failed", model.KindFact, model.Observed, strings.Join(parts, " | "), fmt.Sprintf("failed=%d;skipped=%d", len(f), len(s)), evs, nil,
		[]string{"evidence from failed analyzers may be partial or absent; conclusions that depend on them say so"})
}

func unknowns(b *builder, ev []model.Evidence) {
	var items []string
	var evs []model.Evidence
	for _, c := range b.out {
		if c.Kind == model.KindUnknown {
			items = append(items, c.Topic+": "+c.Statement)
		}
	}
	for _, e := range ev {
		if e.Confidence == model.Unknown && e.Kind != model.KindAnalyzerFailed && e.Kind != model.KindAnalyzerSkipped {
			evs = append(evs, e)
			items = append(items, fmt.Sprintf("%s %s: %s", e.Kind, e.Subject, strings.TrimSpace(e.Finding)))
		}
	}
	if len(items) == 0 {
		b.add("unknowns", model.KindFact, model.Derived, "No unknown items were recorded.", "0", nil, nil, nil)
		return
	}
	b.add("unknowns", model.KindUnknown, model.Unknown, strconv.Itoa(len(items))+" unknown item(s): "+strings.Join(items, " | "), strconv.Itoa(len(items)), evs, nil, nil)
}

// firstN joins up to n items and says how many were left out.
func firstN(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}
