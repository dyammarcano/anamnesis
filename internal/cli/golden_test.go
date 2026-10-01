package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"anamnesis/internal/config"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
)

const goldenID = "ejbca-4.0.16"

type goldenFile struct {
	Fixture struct {
		ID         string `yaml:"id"`
		TreeSHA256 string `yaml:"tree_sha256"`
	} `yaml:"fixture"`
	Invariants []goldenInvariant `yaml:"invariants"`
}

type goldenInvariant struct {
	ID     string         `yaml:"id"`
	Stage  string         `yaml:"stage"`
	Claim  string         `yaml:"claim"`
	Expect map[string]any `yaml:"expect"`
}

// enforcedStages are the stages of expected.yaml this test enforces. Invariants of any other stage
// (build, migration, report-of-a-deep-run) are logged as PENDING and never counted as passing,
// except the ones with an explicit evaluator below because their evidence exists after Preflight.
var enforcedStages = map[string]bool{"discovery": true, "java-version": true, "ant-safety": true}

// goldenRun is everything one assessment of the golden fixture produced.
type goldenRun struct {
	out          Outcome
	ev           []model.Evidence
	root         string
	beforeFiles  string
	afterFiles   string
	beforeDirs   string
	afterDirs    string
	pristineWant string
}

// check evaluates one invariant. ok=false carries a detail that says what was observed.
type check func(g *goldenRun, inv goldenInvariant) (ok bool, detail string)

func expectString(inv goldenInvariant, key string) string {
	if v, ok := inv.Expect[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func expectInt(inv goldenInvariant, key string) int {
	n, _ := strconv.Atoi(expectString(inv, key))
	return n
}

func expectList(inv goldenInvariant, key string) []string {
	var out []string
	if l, ok := inv.Expect[key].([]any); ok {
		for _, v := range l {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

func hasPath(e model.Evidence, sub string) bool {
	for _, l := range e.Locations {
		if strings.Contains(strings.ReplaceAll(l.Path, "\\", "/"), sub) {
			return true
		}
	}
	return false
}

func safetyOf(ev []model.Evidence, names ...string) (model.Evidence, bool) {
	for _, e := range ofKind(ev, model.KindAntTargetSafety) {
		for _, n := range names {
			if e.Subject == n {
				return e, true
			}
		}
	}
	return model.Evidence{}, false
}

func classIn(c string, set ...string) bool {
	for _, s := range set {
		if c == s {
			return true
		}
	}
	return false
}

var goldenChecks = map[string]check{
	"repo.untouched": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		if g.beforeFiles != g.afterFiles || g.beforeDirs != g.afterDirs {
			return false, fmt.Sprintf("digest changed: files %s -> %s, dirs %s -> %s", g.beforeFiles, g.afterFiles, g.beforeDirs, g.afterDirs)
		}
		return true, "tree digest identical before and after (" + g.afterFiles[:12] + ")"
	},
	"repo.no-vcs": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		gits := ofKind(g.ev, model.KindGitRepository)
		if len(gits) == 0 {
			return false, "no git.repository evidence"
		}
		for _, e := range gits {
			if e.Values["state"] != "none" {
				return false, "git state = " + e.Values["state"] + " (" + e.Finding + ")"
			}
		}
		return true, "git state none"
	},
	"project.language": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		for _, e := range ofKind(g.ev, model.KindIdentity) {
			if e.Values["java_files"] == "" {
				continue
			}
			n, _ := strconv.Atoi(e.Values["java_files"])
			min := expectInt(inv, "java_files_at_least")
			if n < min {
				return false, fmt.Sprintf("java_files = %d, want >= %d", n, min)
			}
			return true, fmt.Sprintf("java_files = %d", n)
		}
		return false, "no identity.summary with java_files"
	},
	"build.system": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		present := map[string]model.Evidence{}
		for _, e := range ofKind(g.ev, model.KindBuildSystem) {
			present[e.Subject] = e
		}
		absent := map[string]bool{}
		for _, e := range ofKind(g.ev, model.KindBuildSystemAbsent) {
			absent[e.Subject] = true
		}
		want := expectString(inv, "systems_contains")
		e, ok := present[want]
		if !ok {
			return false, want + " not detected"
		}
		if e.Values["at_root"] != "true" || !hasPath(e, "build.xml") {
			return false, "ant evidence not anchored at root build.xml"
		}
		for _, ex := range expectList(inv, "systems_excludes") {
			if _, found := present[ex]; found {
				return false, ex + " reported present"
			}
			if !absent[ex] {
				return false, ex + " not reported as absent"
			}
		}
		return true, "ant at root; maven and gradle absent"
	},
	"build.ide-metadata": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		for _, e := range ofKind(g.ev, model.KindBuildSystem) {
			if e.Subject == "netbeans" {
				if hasPath(e, "modules/batchenrollment-gui") {
					return true, "netbeans at modules/batchenrollment-gui"
				}
				return false, "netbeans detected elsewhere: " + fmt.Sprint(e.Locations)
			}
		}
		return false, "netbeans metadata not detected"
	},
	"java.target-version": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		want := expectString(inv, "target_version")
		minConf := model.Confidence(expectString(inv, "confidence_at_least"))
		for _, e := range ofKind(g.ev, model.KindJavaTarget) {
			if e.Values["resolved"] != want {
				continue
			}
			if confRank(e.Confidence) < confRank(minConf) {
				return false, fmt.Sprintf("confidence %s below %s", e.Confidence, minConf)
			}
			if !strings.Contains(e.Values["defined_at"], "modules/build-properties.xml:97") {
				return false, "defined_at = " + e.Values["defined_at"]
			}
			return true, "resolved " + want + " (" + string(e.Confidence) + ") defined_at " + e.Values["defined_at"]
		}
		return false, "no java.target evidence resolving to " + want
	},
	"java.source-version-mostly-implicit": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		if evs := ofKind(g.ev, model.KindJavaImplicit); len(evs) > 0 {
			e := evs[0]
			total, _ := strconv.Atoi(e.Values["javac_total"])
			noSrc, _ := strconv.Atoi(e.Values["no_source"])
			if total == 0 || noSrc*2 <= total {
				return false, fmt.Sprintf("no_source %d of %d javac tasks", noSrc, total)
			}
			return true, fmt.Sprintf("%d of %d javac tasks set no source level", noSrc, total)
		}
		return false, "no java.javac.implicit evidence"
	},
	"java.not-modern": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		c := g.out.Conclusion("java.generation")
		if c == nil {
			return false, "no java.generation conclusion"
		}
		if !strings.Contains(c.Statement, "Java 6") || strings.Contains(c.Statement, "Java 21") || c.Value != "LEGACY_JAVA_6" {
			return false, fmt.Sprintf("java.generation = %q (%s)", c.Statement, c.Value)
		}
		for _, id := range c.EvidenceIDs {
			for _, e := range g.ev {
				if e.ID == id && e.Kind == model.KindJavaLocalRuntime {
					return false, "java.generation rests on the local runtime"
				}
			}
		}
		return true, c.Value
	},
	"java.class-majors": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		best := 0
		for _, e := range ofKind(g.ev, model.KindJavaClassMajor) {
			if n, err := strconv.Atoi(e.Values["max_major"]); err == nil && n > best {
				best = n
			}
		}
		if want := expectInt(inv, "jar_class_major_max"); best != want {
			return false, fmt.Sprintf("max_major = %d, want %d", best, want)
		}
		return true, "max_major = " + strconv.Itoa(best)
	},
	"enterprise.java-ee": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		for _, e := range ofKind(g.ev, model.KindEnterpriseJavaEE) {
			if e.Values["detected"] == "true" {
				return true, "detected=true"
			}
		}
		return false, "enterprise.javaee detected=true not found"
	},
	"enterprise.appserver-coupling": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		want := expectString(inv, "appservers_contains")
		var seen []string
		for _, e := range ofKind(g.ev, model.KindEnterpriseAppServer) {
			seen = append(seen, e.Subject)
			if strings.EqualFold(e.Subject, want) {
				return true, "servers: " + strings.Join(seen, ",")
			}
		}
		return false, want + " not among " + fmt.Sprint(seen)
	},
	"ant.dynamic-import": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		for _, e := range ofKind(g.ev, model.KindAntImportUnresolved) {
			if e.Values["raw"] == "bin/${appserver.type}.xml" {
				return true, "reported at " + fmt.Sprint(e.Locations)
			}
		}
		return false, "ant.import.unresolved with raw bin/${appserver.type}.xml not found"
	},
	"ant.deploy-not-safe": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		var parts []string
		for _, name := range []string{"deploy", "install"} {
			e, ok := safetyOf(g.ev, name, "build.xml#"+name)
			if !ok {
				return false, "no ant.target.safety for " + name
			}
			c := e.Values["class"]
			parts = append(parts, name+"="+c)
			if !classIn(c, "DANGEROUS", "REVIEW_REQUIRED") {
				return false, name + " classified " + c
			}
		}
		for _, e := range ofKind(g.ev, model.KindBuildLocal) {
			if e.Values["attempted"] == "true" {
				return false, "a build was executed: " + e.Finding
			}
		}
		return true, strings.Join(parts, " ") + "; nothing executed"
	},
	"ant.default-not-trusted-by-name": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		var def string
		for _, e := range ofKind(g.ev, model.KindAntProject) {
			if e.Subject == "build.xml" {
				def = e.Values["default"]
			}
		}
		if def != "build" {
			return false, "root default target = " + strconv.Quote(def)
		}
		e, ok := safetyOf(g.ev, "build")
		if !ok {
			return false, "no ant.target.safety for build"
		}
		if e.Values["class"] == "SAFE" {
			return false, "default target build classified SAFE"
		}
		closure := strings.Split(e.Values["closure"], ",")
		if len(closure) < 2 || !strings.Contains(e.Values["closure"], "fail-unless-appserver-detected") {
			return false, "closure does not reach app-server detection: " + e.Values["closure"]
		}
		return true, fmt.Sprintf("build is %s via a %d-target closure", e.Values["class"], len(closure))
	},
	"migration.mta-predicted": func(g *goldenRun, _ goldenInvariant) (bool, string) {
		if evs := ofKind(g.ev, model.KindMTAPrediction); len(evs) > 0 {
			e := evs[0]
			if e.Values["java_provider"] == "NOT_APPLICABLE" {
				return true, e.Values["reason"]
			}
			return false, "java_provider = " + e.Values["java_provider"]
		}
		return false, "no mta.prediction evidence"
	},
	"report.always": func(g *goldenRun, inv goldenInvariant) (bool, string) {
		for _, f := range expectList(inv, "files") {
			st, err := os.Stat(filepath.Join(g.out.RunDir, f))
			if err != nil || st.Size() == 0 {
				return false, f + " missing or empty in " + g.out.RunDir
			}
		}
		return true, strings.Join(expectList(inv, "files"), ", ")
	},
}

func loadGoldenFixtureRoot(t *testing.T) (*config.Parameters, string) {
	t.Helper()
	paramsFile := filepath.Join("..", "..", "parameters.yaml")
	if _, err := os.Stat(paramsFile); err != nil {
		t.Skip("golden fixture not configured")
	}
	params, err := config.Load(paramsFile)
	if err != nil {
		t.Skipf("golden fixture not configured (parameters.yaml unusable: %v)", err)
	}
	for _, p := range params.Projects {
		if p.Name == goldenID {
			if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
				t.Skip("golden fixture not configured")
			}
			return params, p.Path
		}
	}
	t.Skip("golden fixture not configured")
	return nil, ""
}

func TestGoldenEJBCA(t *testing.T) {
	params, root := loadGoldenFixtureRoot(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "golden", goldenID, "expected.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var gf goldenFile
	if err := yaml.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("expected.yaml: %v", err)
	}
	if gf.Fixture.ID != goldenID || len(gf.Invariants) == 0 {
		t.Fatalf("expected.yaml is not the %s file or has no invariants", goldenID)
	}

	g := &goldenRun{root: root, pristineWant: gf.Fixture.TreeSHA256}
	g.beforeFiles, g.beforeDirs, _ = treeDigest(t, root)

	// side effects are explicitly off; Only keeps prepare, build and MTA from ever running.
	prm := *params
	prm.OutputDir = filepath.Join(t.TempDir(), "assessments")
	prm.Build.Allow, prm.MTA.Allow, prm.Network.Allow = false, false, false
	prm.Environment.InstallMissing, prm.Environment.InstallJDKs = false, false

	registerOnce.Do(registerAll)
	only := map[string]bool{"ant-safety": true}
	for _, a := range engine.Registered() {
		if a.Phase() == engine.Preflight {
			only[a.Name()] = true
		}
	}
	outs, err := Assess(context.Background(), []string{root}, Options{
		Params: &prm,
		Phases: []engine.Phase{engine.Preflight, engine.Deep},
		Only:   only,
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if len(outs) != 1 || outs[0].Err != nil {
		t.Fatalf("unexpected outcome: %+v", outs)
	}
	g.out, g.ev = outs[0], outs[0].Evidence
	g.afterFiles, g.afterDirs, _ = treeDigest(t, root)
	if g.beforeFiles != gf.Fixture.TreeSHA256 {
		t.Logf("NOTE: the fixture digest %s differs from the pristine digest %s recorded in expected.yaml (digest taken before the run)", g.beforeFiles, gf.Fixture.TreeSHA256)
	}
	if len(g.out.Failed()) > 0 {
		t.Errorf("analyzers failed: %v", g.out.Failed())
	}

	type row struct{ id, stage, state, detail string }
	var rows []row
	for _, inv := range gf.Invariants {
		inv := inv
		chk, evaluated := goldenChecks[inv.ID]
		switch {
		case evaluated:
			var ok bool
			var detail string
			t.Run(inv.ID, func(t *testing.T) {
				ok, detail = chk(g, inv)
				if !ok {
					t.Errorf("%s: %s\n  claim: %s", inv.ID, detail, inv.Claim)
				}
			})
			state := "PASS"
			if !ok {
				state = "FAIL"
			}
			rows = append(rows, row{inv.ID, inv.Stage, state, detail})
		case enforcedStages[inv.Stage]:
			// an invariant in an enforced stage with no evaluator is a gap in this test, not a pass
			t.Run(inv.ID, func(t *testing.T) {
				t.Errorf("%s: stage %s is enforced but this test has no evaluator for it", inv.ID, inv.Stage)
			})
			rows = append(rows, row{inv.ID, inv.Stage, "FAIL", "no evaluator"})
		default:
			t.Run(inv.ID, func(t *testing.T) {
				t.Skipf("PENDING: stage %q needs a run Anamnesis capability that this Preflight+ant-safety run does not exercise", inv.Stage)
			})
			rows = append(rows, row{inv.ID, inv.Stage, "PENDING", "stage " + inv.Stage})
		}
	}

	var b strings.Builder
	b.WriteString("\ngolden invariants (" + goldenID + ")\n")
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.state]++
		fmt.Fprintf(&b, "  %-8s %-40s %-13s %s\n", r.state, r.id, r.stage, r.detail)
	}
	fmt.Fprintf(&b, "  PASS=%d FAIL=%d PENDING=%d\n", counts["PASS"], counts["FAIL"], counts["PENDING"])
	t.Log(b.String())
}
