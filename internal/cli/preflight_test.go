package cli

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"anamnesis/internal/config"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/workspace"
)

// writeLegacyJar creates a jar holding one class file whose header says Java 6 (major 50). It is
// generated at test time so no binary is committed to the repository.
func writeLegacyJar(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("Manifest-Version: 1.0\r\nCreated-By: 1.6.0_45 (Sun Microsystems Inc.)\r\n\r\n"))
	w, err = zw.Create("legacy/Old.class")
	if err != nil {
		t.Fatal(err)
	}
	// CAFEBABE, minor 0, major 50
	_, _ = w.Write([]byte{0xCA, 0xFE, 0xBA, 0xBE, 0x00, 0x00, 0x00, 0x32, 0x00, 0x00})
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeParameters(t *testing.T, dir string, projects []string, outputDir string) *config.Parameters {
	t.Helper()
	var b strings.Builder
	b.WriteString("output_dir: '" + outputDir + "'\nprojects:\n")
	for _, p := range projects {
		b.WriteString("  - path: '" + p + "'\n")
	}
	path := filepath.Join(dir, "parameters.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	params, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return params
}

func TestPreflightFixtureAnt(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "unit", "ant-java6"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "build.xml")); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}

	work := t.TempDir()
	proj := filepath.Join(work, "ant-java6")
	copyTree(t, fixture, proj)
	writeLegacyJar(t, filepath.Join(proj, "lib", "legacy.jar"))

	out := filepath.Join(t.TempDir(), "assessments")
	params := writeParameters(t, work, []string{proj}, out)

	beforeFiles, beforeDirs, nFiles := treeDigest(t, proj)

	outs, err := Assess(context.Background(), nil, Options{Params: params, Phases: []engine.Phase{engine.Preflight}})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("want 1 outcome, got %d", len(outs))
	}
	o := outs[0]
	if o.Err != nil {
		t.Fatalf("outcome error: %v", o.Err)
	}
	if o.Deep {
		t.Error("a Preflight-only run is marked as deep")
	}

	// reports
	if !strings.HasPrefix(o.RunDir, out) {
		t.Errorf("run dir %s is not under the output dir %s", o.RunDir, out)
	}
	for _, name := range []string{"report.json", "report.html", "evidence.jsonl", "run.json"} {
		st, err := os.Stat(filepath.Join(o.RunDir, name))
		if err != nil {
			t.Errorf("%s not written: %v", name, err)
		} else if st.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	// the evidence on disk is the evidence in memory
	onDisk, err := workspace.ReadEvidence(o.RunDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(o.Evidence) {
		t.Errorf("evidence.jsonl has %d items, outcome has %d", len(onDisk), len(o.Evidence))
	}

	// no analyzer may fail on a clean synthetic repository
	if failed := o.Failed(); len(failed) > 0 {
		t.Errorf("analyzers failed: %v", failed)
	}
	for _, e := range ofKind(o.Evidence, model.KindAnalyzerFailed) {
		t.Errorf("analyzer.failed evidence: %s", e.Finding)
	}

	// build systems
	var ant *model.Evidence
	for _, e := range ofKind(o.Evidence, model.KindBuildSystem) {
		if e.Subject == "ant" {
			e := e
			ant = &e
		}
		if e.Subject == "maven" || e.Subject == "gradle" {
			t.Errorf("%s reported present in a repository that has none", e.Subject)
		}
	}
	if ant == nil {
		t.Error("build.system ant not reported")
	} else if ant.Values["at_root"] != "true" || ant.Confidence != model.Observed {
		t.Errorf("ant evidence wrong: at_root=%q confidence=%s", ant.Values["at_root"], ant.Confidence)
	}
	absent := map[string]bool{}
	for _, e := range ofKind(o.Evidence, model.KindBuildSystemAbsent) {
		absent[e.Subject] = true
	}
	for _, want := range []string{"maven", "gradle"} {
		if !absent[want] {
			t.Errorf("build.system.absent %s not reported (have %v)", want, absent)
		}
	}

	// java target through the imported property file
	wantLine := lineOf(t, filepath.Join(proj, "build-properties.xml"), `name="java.target.version"`)
	var target *model.Evidence
	for _, e := range ofKind(o.Evidence, model.KindJavaTarget) {
		if e.Values["resolved"] == "1.6" {
			e := e
			target = &e
		}
	}
	if target == nil {
		t.Error("java.target 1.6 not reported")
	} else {
		if target.Confidence != model.Derived {
			t.Errorf("java.target confidence = %s, want DERIVED", target.Confidence)
		}
		if want := "build-properties.xml:" + strconv.Itoa(wantLine); target.Values["defined_at"] != want {
			t.Errorf("defined_at = %q, want %q", target.Values["defined_at"], want)
		}
		if target.Values["raw"] != "${java.target.version}" {
			t.Errorf("raw = %q", target.Values["raw"])
		}
	}

	// class files from the generated jar
	var sawMajor bool
	for _, e := range ofKind(o.Evidence, model.KindJavaClassMajor) {
		if e.Values["max_major"] == "50" {
			sawMajor = true
		}
	}
	if !sawMajor {
		t.Error("java.binary.class-major max_major=50 not reported for lib/legacy.jar")
	}

	// the Java generation conclusion follows the declared target, never the local runtime
	gen := o.Conclusion("java.generation")
	if gen == nil || gen.Value != "LEGACY_JAVA_6" {
		t.Errorf("java.generation = %+v, want LEGACY_JAVA_6", gen)
	}

	// descriptor
	var sawWebXML bool
	for _, e := range ofKind(o.Evidence, model.KindEnterpriseDescriptor) {
		if e.Subject == "web.xml" {
			sawWebXML = true
		}
	}
	if !sawWebXML {
		t.Error("WEB-INF/web.xml not reported as an enterprise descriptor")
	}

	// preflight must not run the build or MTA
	for _, e := range o.Evidence {
		if e.Kind == model.KindBuildLocal && e.Values["attempted"] == "true" {
			t.Errorf("a build was attempted during Preflight: %s", e.Finding)
		}
	}
	if len(ofKind(o.Evidence, model.KindMTARun)) != 0 {
		t.Error("MTA ran during Preflight")
	}

	// the analyzed tree is untouched
	afterFiles, afterDirs, afterN := treeDigest(t, proj)
	if afterFiles != beforeFiles || afterDirs != beforeDirs || afterN != nFiles {
		t.Errorf("the analyzed repository changed: files %s -> %s (%d -> %d), dirs %s -> %s", beforeFiles, afterFiles, nFiles, afterN, beforeDirs, afterDirs)
	}
}

func TestPreflightRefusesOutputInsideProject(t *testing.T) {
	work := t.TempDir()
	proj := filepath.Join(work, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	params := writeParameters(t, work, []string{proj}, filepath.Join(proj, "out"))
	before, _, _ := treeDigest(t, proj)
	_, err := Assess(context.Background(), nil, Options{Params: params, Phases: []engine.Phase{engine.Preflight}})
	if err == nil {
		t.Fatal("Assess accepted an output_dir inside the analyzed project")
	}
	if after, _, _ := treeDigest(t, proj); after != before {
		t.Error("project was modified although the run was refused")
	}
	if _, statErr := os.Stat(filepath.Join(proj, "out")); statErr == nil {
		t.Error("the output directory was created inside the project")
	}
}

func TestAssessWithoutProjectsFails(t *testing.T) {
	work := t.TempDir()
	params := writeParameters(t, work, nil, filepath.Join(work, "out"))
	if _, err := Assess(context.Background(), nil, Options{Params: params}); err == nil {
		t.Error("no projects to assess must be an error")
	}
	if _, err := Assess(context.Background(), nil, Options{}); err == nil {
		t.Error("missing parameters must be an error")
	}
}

func TestAssessMissingProjectPathIsReportedPerProject(t *testing.T) {
	work := t.TempDir()
	params := writeParameters(t, work, []string{filepath.Join(work, "does-not-exist")}, filepath.Join(work, "out"))
	outs, err := Assess(context.Background(), nil, Options{Params: params, Phases: []engine.Phase{engine.Preflight}})
	if err != nil {
		t.Fatalf("a missing project must not abort the call: %v", err)
	}
	if len(outs) != 1 || outs[0].Err == nil {
		t.Fatalf("want one outcome carrying an error, got %+v", outs)
	}
}

func lineOf(t *testing.T, file, needle string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	t.Fatalf("%q not found in %s", needle, file)
	return 0
}
