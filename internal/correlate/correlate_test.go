package correlate_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/dyammarcano/anamnesis/internal/correlate"
	"github.com/dyammarcano/anamnesis/internal/model"
)

func ev(id, kind, subject string, vals map[string]string) model.Evidence {
	return model.Evidence{ID: id, Analyzer: "test", Kind: kind, Subject: subject, Finding: kind + " " + subject,
		Status: model.Info, Confidence: model.Observed, Values: vals}
}

func buildSystemAnt() model.Evidence {
	return ev("bs-ant", model.KindBuildSystem, "ant", map[string]string{"at_root": "true", "count": "1"})
}

func conclusion(t *testing.T, cs []model.Conclusion, topic string) model.Conclusion {
	t.Helper()
	for _, c := range cs {
		if c.Topic == topic {
			return c
		}
	}
	t.Fatalf("no conclusion with topic %q; have %v", topic, topics(cs))
	return model.Conclusion{}
}

func topics(cs []model.Conclusion) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Topic)
	}
	return out
}

func TestBuildabilitySemantics(t *testing.T) {
	tests := []struct {
		name      string
		evidence  []model.Evidence
		wantLocal string // prefix
		wantExact string // exact local value when non-empty
		wantSrc   string
		wantState string
	}{
		{
			name: "a: tool not available, not attempted",
			evidence: []model.Evidence{buildSystemAnt(),
				ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "false", "class": model.BuildToolNotAvailable, "tool": "ant"})},
			wantLocal: "NOT_ATTEMPTED", wantSrc: "NOT_DISPROVEN", wantState: "NOT_PROVEN",
		},
		{
			name: "b: partial success, not the full build",
			evidence: []model.Evidence{buildSystemAnt(),
				ev("bl", model.KindBuildLocal, "compile", map[string]string{"attempted": "true", "class": model.BuildSuccess, "full_build": "false", "tool": "ant", "target": "compile", "default_target": "dist"})},
			wantExact: "PARTIAL:compile", wantSrc: "NOT_DISPROVEN", wantState: "NOT_PROVEN",
		},
		{
			name: "c: full build succeeded",
			evidence: []model.Evidence{buildSystemAnt(),
				ev("bl", model.KindBuildLocal, "dist", map[string]string{"attempted": "true", "class": model.BuildSuccess, "full_build": "true", "tool": "ant", "target": "dist"})},
			wantExact: "REPRODUCED", wantSrc: "PROVEN_LOCALLY", wantState: "LOCALLY_BUILDABLE",
		},
		{
			name: "d: CI succeeded on the same SHA, local build failed",
			evidence: []model.Evidence{buildSystemAnt(),
				ev("ci", model.KindCISameSHA, "ci", map[string]string{"state": "SUCCESS_SAME_SHA", "run_url": "https://example.invalid/run/1"}),
				ev("bl", model.KindBuildLocal, "dist", map[string]string{"attempted": "true", "class": model.BuildSourceOrBuildError, "exit_code": "1", "tool": "ant"})},
			wantLocal: "FAILED:", wantSrc: "PROVEN_IN_CI_SAME_SHA", wantState: "KNOWN_BUILDABLE_IN_CI_LOCAL_NOT_REPRODUCED",
		},
		{
			name: "environmental failure class keeps the source not disproven",
			evidence: []model.Evidence{buildSystemAnt(),
				ev("bl", model.KindBuildLocal, "dist", map[string]string{"attempted": "true", "class": model.BuildMissingAppServer, "exit_code": "1", "tool": "ant"})},
			wantLocal: "FAILED:MISSING_APP_SERVER_OR_RUNTIME", wantSrc: "NOT_DISPROVEN", wantState: "BUILD_FAILED_NO_SAME_SHA_CI_PROOF",
		},
		{
			name:      "no build evidence at all",
			evidence:  []model.Evidence{buildSystemAnt()},
			wantExact: "INCOMPLETE", wantSrc: "NOT_DISPROVEN", wantState: "NOT_PROVEN",
		},
		{
			name:      "nothing at all",
			evidence:  nil,
			wantExact: "INCOMPLETE", wantSrc: "NOT_PROVEN", wantState: "NOT_PROVEN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := correlate.Correlate(tc.evidence)
			local := conclusion(t, cs, "buildability.local-reproducibility")
			src := conclusion(t, cs, "buildability.source")
			state := conclusion(t, cs, "buildability.state")
			switch {
			case tc.wantExact != "":
				if local.Value != tc.wantExact {
					t.Errorf("local-reproducibility = %q, want %q", local.Value, tc.wantExact)
				}
			default:
				if !strings.HasPrefix(local.Value, tc.wantLocal) {
					t.Errorf("local-reproducibility = %q, want prefix %q", local.Value, tc.wantLocal)
				}
			}
			if src.Value != tc.wantSrc {
				t.Errorf("source = %q, want %q", src.Value, tc.wantSrc)
			}
			if state.Value != tc.wantState {
				t.Errorf("state = %q, want %q", state.Value, tc.wantState)
			}
		})
	}
}

// negated reports whether a "source is not/unbuildable" match sits inside a negation, e.g.
// "This is not a finding that the source is unbuildable".
var negation = regexp.MustCompile(`(?i)\b(not|never|no|neither|nor|cannot|does not|doesn't)\b`)

var positiveClaim = regexp.MustCompile(`(?i)source is (not\s+|un)buildable`)

// unnegatedClaims returns the "source is not/unbuildable" matches that are not inside a negation.
func unnegatedClaims(text string) []string {
	var out []string
	for _, loc := range positiveClaim.FindAllStringIndex(text, -1) {
		start := max(0, loc[0]-80)
		if !negation.MatchString(text[start:loc[0]]) {
			out = append(out, text[loc[0]:loc[1]])
		}
	}
	return out
}

func assertNoPositiveUnbuildableClaim(t *testing.T, where, text string) {
	t.Helper()
	for _, c := range unnegatedClaims(text) {
		t.Errorf("%s makes an un-negated claim %q in: %s", where, c, text)
	}
	for _, bad := range []string{"NOT_BUILDABLE", "UNBUILDABLE", "NOT BUILDABLE"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s contains the machine claim %q: %s", where, bad, text)
		}
	}
}

func TestNoConclusionClaimsSourceIsUnbuildable(t *testing.T) {
	scenarios := map[string][]model.Evidence{
		"nothing": nil,
		"tool missing": {buildSystemAnt(),
			ev("pr", model.KindPrereqTool, "ant", map[string]string{"requirement": "REQUIRED_FOR_CAPABILITY", "state": "MISSING"}),
			ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "false", "class": model.BuildToolNotAvailable})},
		"side effect skipped": {buildSystemAnt(),
			ev("sk", model.KindAnalyzerSkipped, "localbuild", map[string]string{})},
		"source error": {buildSystemAnt(),
			ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "true", "class": model.BuildSourceOrBuildError, "exit_code": "1"})},
		"missing dependency": {buildSystemAnt(),
			ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "true", "class": model.BuildMissingDependency, "exit_code": "1"})},
		"unknown failure": {buildSystemAnt(),
			ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "true", "class": model.BuildUnknown, "exit_code": "2"})},
		"ci failed same sha": {buildSystemAnt(),
			ev("ci", model.KindCISameSHA, "ci", map[string]string{"state": "FAILED_SAME_SHA"})},
		"partial": {buildSystemAnt(),
			ev("bl", model.KindBuildLocal, "c", map[string]string{"attempted": "true", "class": model.BuildSuccess, "full_build": "false", "target": "c"})},
		"no build system": {
			ev("bl", model.KindBuildLocal, "x", map[string]string{"attempted": "false", "class": model.BuildNotAttemptedNoDecision})},
	}
	for name, evidence := range scenarios {
		t.Run(name, func(t *testing.T) {
			for _, c := range correlate.Correlate(evidence) {
				assertNoPositiveUnbuildableClaim(t, c.Topic+" statement", c.Statement)
				for _, l := range c.Limitations {
					assertNoPositiveUnbuildableClaim(t, c.Topic+" limitation", l)
				}
				if c.Topic == "buildability.source" {
					switch c.Value {
					case "NOT_DISPROVEN", "NOT_PROVEN", "PROVEN_LOCALLY", "PROVEN_IN_CI_SAME_SHA":
					default:
						t.Errorf("buildability.source has unexpected value %q", c.Value)
					}
				}
			}
		})
	}
}

func TestNegationGuardCatchesAPositiveClaim(t *testing.T) {
	// guards the guard: the helper must flag a bare claim and accept a negated one
	if got := unnegatedClaims("The source is unbuildable."); len(got) != 1 {
		t.Errorf("bare claim not flagged: %v", got)
	}
	if got := unnegatedClaims("The source is not buildable here."); len(got) != 1 {
		t.Errorf("bare 'not buildable' claim not flagged: %v", got)
	}
	if got := unnegatedClaims("This is not a finding that the source is unbuildable."); len(got) != 0 {
		t.Errorf("negated claim wrongly flagged: %v", got)
	}
}

func TestJavaGenerationNeverDerivedFromLocalRuntimeAlone(t *testing.T) {
	runtime21 := ev("rt", model.KindJavaLocalRuntime, "java", map[string]string{"major": "21", "version": "21.0.1", "java_home": "C:\\jdk"})
	runtime21.Category = model.CatEnvironment
	tests := []struct {
		name      string
		evidence  []model.Evidence
		wantKind  model.ConclusionKind
		wantValue string
	}{
		{"local runtime only", []model.Evidence{runtime21}, model.KindUnknown, ""},
		{"local runtime and a build system", []model.Evidence{runtime21, buildSystemAnt()}, model.KindUnknown, ""},
		{"no evidence", nil, model.KindUnknown, ""},
		{
			"declared target 1.6 next to a 21 runtime",
			[]model.Evidence{runtime21, ev("jt", model.KindJavaTarget, "1.6", map[string]string{"resolved": "1.6", "origin": "ant", "defined_at": "p.xml:3"})},
			model.KindInference, "LEGACY_JAVA_6",
		},
		{
			"class files only (major 50) next to a 21 runtime",
			[]model.Evidence{runtime21, ev("cm", model.KindJavaClassMajor, "binaries", map[string]string{"max_major": "50"})},
			model.KindInference, "LEGACY_JAVA_6",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := conclusion(t, correlate.Correlate(tc.evidence), "java.generation")
			if c.Kind != tc.wantKind || c.Value != tc.wantValue {
				t.Errorf("java.generation kind=%s value=%q, want kind=%s value=%q (%s)", c.Kind, c.Value, tc.wantKind, tc.wantValue, c.Statement)
			}
			if strings.Contains(c.Statement, "21") && c.Kind != model.KindUnknown {
				t.Errorf("statement leaks the local runtime major: %s", c.Statement)
			}
			for _, id := range c.EvidenceIDs {
				if id == "rt" {
					t.Error("java.generation cites the local-runtime evidence as a basis")
				}
			}
			if c.Kind == model.KindUnknown && c.Confidence != model.Unknown {
				t.Errorf("unknown conclusion carries confidence %s", c.Confidence)
			}
		})
	}
}

func fullEvidenceSet() []model.Evidence {
	clsMajor := ev("cm", model.KindJavaClassMajor, "binaries", map[string]string{"max_major": "50"})
	derived := ev("jt", model.KindJavaTarget, "1.6", map[string]string{"resolved": "1.6", "origin": "ant", "defined_at": "modules/p.xml:97"})
	derived.Confidence = model.Derived
	inferredJEE := ev("jee", model.KindEnterpriseJavaEE, "javaee", map[string]string{"detected": "true"})
	inferredJEE.Confidence = model.Inferred
	unknownConf := ev("unk", model.KindAntTargetSafety, "build", map[string]string{"class": "UNKNOWN"})
	unknownConf.Confidence = model.Unknown
	pred := ev("mp", model.KindMTAPrediction, "java-provider", map[string]string{"java_provider": "NOT_APPLICABLE", "reason": "no pom"})
	pred.Confidence = model.Inferred
	return []model.Evidence{
		ev("id", model.KindIdentity, "proj", map[string]string{"files": "10", "java_files": "3", "java_lines": "99", "jars": "1", "wars": "0", "ears": "0"}),
		ev("git", model.KindGitRepository, "git", map[string]string{"state": "none"}),
		buildSystemAnt(),
		ev("bs-mvn", model.KindBuildSystemAbsent, "maven", nil),
		derived, clsMajor,
		ev("rt", model.KindJavaLocalRuntime, "java", map[string]string{"major": "21", "version": "21"}),
		ev("ds", model.KindEnterpriseDescriptor, "web.xml", map[string]string{"count": "1"}),
		ev("as", model.KindEnterpriseAppServer, "jboss", nil),
		inferredJEE,
		ev("tu", model.KindTechUsage, "servlet", map[string]string{"files": "1", "occurrences": "2"}),
		ev("di", model.KindDepsInventory, "deps", map[string]string{"jars": "1", "wars": "0", "ears": "0", "total_bytes": "10"}),
		ev("cw", model.KindCIWorkflow, ".github/workflows/x.yml", map[string]string{"purpose": "BUILD"}),
		ev("ci", model.KindCISameSHA, "ci", map[string]string{"state": "NO_RUN_SAME_SHA"}),
		ev("bl", model.KindBuildLocal, "build", map[string]string{"attempted": "false", "class": model.BuildToolNotAvailable, "tool": "ant"}),
		ev("pt", model.KindPrereqTool, "ant", map[string]string{"requirement": "REQUIRED_FOR_CAPABILITY", "state": "MISSING"}),
		ev("sf", model.KindAntTargetSafety, "deploy", map[string]string{"class": "DANGEROUS"}),
		unknownConf, pred,
		ev("af", model.KindAnalyzerFailed, "mta", nil),
		ev("sk", model.KindAnalyzerSkipped, "localbuild", nil),
	}
}

func TestEveryConclusionTracesToInputEvidence(t *testing.T) {
	in := fullEvidenceSet()
	ids := map[string]model.Evidence{}
	for _, e := range in {
		ids[e.ID] = e
	}
	cs := correlate.Correlate(in)
	if len(cs) < 10 {
		t.Fatalf("suspiciously few conclusions (%d): %v", len(cs), topics(cs))
	}
	rank := map[model.Confidence]int{model.Observed: 3, model.Derived: 2, model.Inferred: 1, model.Unknown: 0}
	for _, c := range cs {
		for _, id := range c.EvidenceIDs {
			e, ok := ids[id]
			if !ok {
				t.Errorf("conclusion %s cites evidence ID %q that is not in the input", c.Topic, id)
				continue
			}
			if c.Kind != model.KindUnknown && rank[c.Confidence] > rank[e.Confidence] {
				t.Errorf("conclusion %s claims %s but rests on %s evidence %s", c.Topic, c.Confidence, e.Confidence, id)
			}
		}
		if c.Kind != model.KindUnknown && c.Statement == "" {
			t.Errorf("conclusion %s has an empty statement", c.Topic)
		}
	}
	// conclusions that assert something about the project must actually cite something
	for _, topic := range []string{"build.systems", "java.declared", "java.generation", "java.binaries", "buildability.source", "buildability.state", "enterprise"} {
		c := conclusion(t, cs, topic)
		if len(c.EvidenceIDs) == 0 {
			t.Errorf("conclusion %s cites no evidence", topic)
		}
	}
}

func TestConclusionsAreDeterministic(t *testing.T) {
	a := correlate.Correlate(fullEvidenceSet())
	rev := fullEvidenceSet()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	b := correlate.Correlate(rev)
	if len(a) != len(b) {
		t.Fatalf("different conclusion counts for permuted input: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Topic != b[i].Topic || a[i].Value != b[i].Value || strings.Join(a[i].EvidenceIDs, ",") != strings.Join(b[i].EvidenceIDs, ",") {
			t.Errorf("conclusion %s differs for permuted input:\n  %+v\n  %+v", a[i].Topic, a[i], b[i])
		}
	}
}
