package workspace_test

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/workspace"
)

func TestNewRefusesOutputInsideProject(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "Proj")
	other := filepath.Join(base, "Other")
	tests := []struct {
		name    string
		out     string
		roots   []string
		refused bool
	}{
		{"output equals project root", proj, []string{proj}, true},
		{"output below project root", filepath.Join(proj, "out", "deep"), []string{proj}, true},
		{"output inside the second of two projects", filepath.Join(other, "o"), []string{proj, other}, true},
		{"sibling directory sharing a name prefix is allowed", proj + "-out", []string{proj}, false},
		{"unrelated directory", filepath.Join(base, "elsewhere"), []string{proj}, false},
		{"parent of the project is allowed", base, []string{proj}, false},
		{"no projects", filepath.Join(base, "x"), nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := workspace.New(tc.out, tc.roots)
			if tc.refused {
				if err == nil {
					t.Fatalf("output %s inside %v was accepted", tc.out, tc.roots)
				}
				if !strings.Contains(err.Error(), "inside analyzed project") {
					t.Errorf("error does not explain the refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if ws.Root != tc.out {
				t.Errorf("Root = %q, want %q", ws.Root, tc.out)
			}
		})
	}
}

func TestNewRefusesOutputInsideProjectCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive path comparison is a Windows rule")
	}
	proj := filepath.Join(t.TempDir(), "MyProject")
	variants := []string{
		strings.ToUpper(proj),
		strings.ToLower(proj),
		filepath.Join(strings.ToUpper(proj), "OUT"),
		filepath.Join(strings.ToLower(proj), "Out", "Sub"),
	}
	for _, out := range variants {
		if _, err := workspace.New(out, []string{proj}); err == nil {
			t.Errorf("output %q was accepted although it is inside %q (case differs only)", out, proj)
		}
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := workspace.New("", nil); err == nil {
		t.Fatal("empty output root accepted")
	}
}

func TestNewLayout(t *testing.T) {
	out := filepath.Join(t.TempDir(), "o")
	ws, err := workspace.New(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}Z$`).MatchString(ws.RunID) {
		t.Errorf("RunID %q is not a UTC timestamp", ws.RunID)
	}
	if got, want := ws.ProjectRunDir("demo", "abcd1234"), filepath.Join(out, "demo-abcd1234", ws.RunID); got != want {
		t.Errorf("ProjectRunDir = %q, want %q", got, want)
	}
	if got, want := ws.ConsolidatedDir(), filepath.Join(out, "consolidated", ws.RunID); got != want {
		t.Errorf("ConsolidatedDir = %q, want %q", got, want)
	}
	if time.Since(ws.Start) > time.Minute {
		t.Error("Start is not the creation time")
	}
}

func sampleEvidence() []model.Evidence {
	evs := []model.Evidence{
		{Analyzer: "b-analyzer", Category: model.CatBuild, Kind: model.KindBuildSystem, Subject: "ant", Finding: "Ant <found> & \"quoted\"",
			Status: model.Info, Confidence: model.Observed, Locations: []model.Location{{Path: "build.xml", Line: 3}, {Path: "x/build.xml"}},
			Values: map[string]string{"at_root": "true", "count": "2", "z": "last", "a": "first"}, Assumptions: []string{"a1"}, Limitations: []string{"l1", "l2"}},
		{Analyzer: "a-analyzer", Category: model.CatJavaVersion, Kind: model.KindJavaTarget, Subject: "1.6", Finding: "target 1.6",
			Status: model.Warn, Confidence: model.Derived, Severity: model.SevMedium, Locations: []model.Location{{Path: "p.xml", Line: 97}},
			Values: map[string]string{"resolved": "1.6", "origin": "ant"}, RuleID: "r1", Target: "t",
			Effort:  &model.Effort{Points: 3, Incidents: 2},
			Command: &model.CommandRecord{Argv: []string{"ant", "-version"}, Dir: "d", ExitCode: 0, DurationMs: 12, EnvNames: []string{"JAVA_HOME"}}},
		{Analyzer: "a-analyzer", Category: model.CatGit, Kind: model.KindGitRepository, Subject: "git", Finding: "unicode café 世界",
			Status: model.Info, Confidence: model.Observed, Values: map[string]string{"state": "none"}},
	}
	for i := range evs {
		evs[i].Finalize()
	}
	return evs
}

func TestEvidenceRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	in := sampleEvidence()
	if err := workspace.WriteEvidence(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := workspace.ReadEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("read %d evidence items, wrote %d", len(out), len(in))
	}
	for i := range in {
		want := in[i]
		want.ObservedAt = time.Time{} // timing is deliberately not stored in evidence.jsonl
		if !reflect.DeepEqual(want, out[i]) {
			t.Errorf("item %d changed in the round trip:\n  wrote %+v\n  read  %+v", i, want, out[i])
		}
	}
}

func TestEvidenceFileIsJSONLinesWithoutHTMLEscaping(t *testing.T) {
	dir := t.TempDir()
	if err := workspace.WriteEvidence(dir, sampleEvidence()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "evidence.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("want one line per item (3), got %d", len(lines))
	}
	if strings.Contains(string(b), string(rune(92))+"u003c") || !strings.Contains(string(b), "<found> &") {
		t.Errorf("evidence.jsonl must not HTML-escape < > &: %s", b)
	}
	if strings.Contains(string(b), "ObservedAt") || strings.Contains(string(b), "observed") {
		t.Error("timing must not leak into evidence.jsonl")
	}
}

func TestEvidenceOutputIsDeterministic(t *testing.T) {
	write := func(evs []model.Evidence) []byte {
		dir := t.TempDir()
		if err := workspace.WriteEvidence(dir, evs); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "evidence.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := sampleEvidence()
	engine.SortEvidence(first)
	want := write(first)

	// same facts observed at different times and delivered in a different order
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 5; round++ {
		again := sampleEvidence()
		for i := range again {
			again[i].ObservedAt = time.Now().Add(time.Duration(rng.Intn(1000)) * time.Hour)
		}
		rng.Shuffle(len(again), func(i, j int) { again[i], again[j] = again[j], again[i] })
		engine.SortEvidence(again)
		if got := write(again); !bytes.Equal(got, want) {
			t.Fatalf("round %d: same evidence produced different bytes:\n%s\n--- vs ---\n%s", round, got, want)
		}
	}
}

func TestFinalizeIDIsStableAndTimeIndependent(t *testing.T) {
	a := model.Evidence{Analyzer: "x", Kind: "k", Subject: "s", Locations: []model.Location{{Path: "f", Line: 2}}}
	b := a
	b.ObservedAt = time.Now().Add(-48 * time.Hour)
	a.Finalize()
	b.Finalize()
	if a.ID == "" || a.ID != b.ID {
		t.Errorf("IDs differ or empty: %q vs %q", a.ID, b.ID)
	}
	c := a
	c.ID = ""
	c.Locations = []model.Location{{Path: "f", Line: 3}}
	c.Finalize()
	if c.ID == a.ID {
		t.Error("a different location must give a different ID")
	}
}

func TestReadEvidenceErrors(t *testing.T) {
	if _, err := workspace.ReadEvidence(t.TempDir()); err == nil {
		t.Error("reading a directory without evidence.jsonl must fail")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "evidence.jsonl"), []byte("{\"id\":\"a\"}\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadEvidence(dir); err == nil {
		t.Error("a corrupt line must be an error, not silently skipped")
	}
}

func TestWriteEvidenceEmptyCreatesEmptyFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "run")
	if err := workspace.WriteEvidence(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := workspace.ReadEvidence(dir)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestWriteJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := workspace.WriteJSON(dir, "x.json", map[string]int{"b": 2, "a": 1}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "}\n") || strings.Index(string(b), `"a"`) > strings.Index(string(b), `"b"`) {
		t.Errorf("unexpected JSON layout: %q", b)
	}
}
