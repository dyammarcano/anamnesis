package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"anamnesis/internal/config"
)

func writeParams(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mention string
	}{
		{"misspelled top-level key", "output_dr: out\nprojects: []\n", "output_dr"},
		{"misspelled nested key", "output_dir: out\nbuild:\n  alow: true\n", "alow"},
		{"misspelled project key", "output_dir: out\nprojects:\n  - pth: x\n", "pth"},
		{"misspelled mta key", "output_dir: out\nmta:\n  timeout_minuts: 5\n", "timeout_minuts"},
		{"unknown section", "output_dir: out\nenvironmnt: {}\n", "environmnt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeParams(t, t.TempDir(), tc.content)
			_, err := config.Load(path)
			if err == nil {
				t.Fatal("a misspelled key was accepted")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q should name the offending key %q", err, tc.mention)
			}
		})
	}
}

func TestLoadResolvesRelativePathsAgainstFileDir(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(t.TempDir(), "absolute-project")
	path := writeParams(t, dir, `
output_dir: out/assess
projects:
  - path: proj/one
  - path: '`+abs+`'
  - path: '"quoted/proj"'
discover_roots: [roots/a]
jdks:
  - name: j21
    home: jdks/21
mta:
  install_dir: tools/mta
  executable: tools/mta/mta-cli.exe
`)
	p, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	checks := []struct{ name, got, want string }{
		{"output_dir", p.OutputDir, want("out/assess")},
		{"relative project", p.Projects[0].Path, want("proj/one")},
		{"absolute project untouched", p.Projects[1].Path, abs},
		{"quoted project", p.Projects[2].Path, want("quoted/proj")},
		{"discover root", p.DiscoverRoots[0], want("roots/a")},
		{"jdk home", p.JDKs[0].Home, want("jdks/21")},
		{"mta install_dir", p.MTA.InstallDir, want("tools/mta")},
		{"mta executable", p.MTA.Executable, want("tools/mta/mta-cli.exe")},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !filepath.IsAbs(p.Path) || filepath.Dir(p.Path) != dir {
		t.Errorf("Path = %q, want an absolute path in %q", p.Path, dir)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeParams(t, t.TempDir(), "output_dir: out\nprojects:\n  - path: p\n")
	p, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Build.TimeoutMinutes != 20 || p.MTA.TimeoutMinutes != 60 || p.MTA.Mode != "source-only" ||
		p.Analysis.Concurrency != 4 || p.Analysis.AnalyzerTimeoutMinutes != 30 {
		t.Errorf("defaults not applied: build=%+v mta=%+v analysis=%+v", p.Build, p.MTA, p.Analysis)
	}
	if !p.Projects[0].IsEnabled() {
		t.Error("a project without 'enabled' must default to enabled")
	}
	if p.Build.Allow || p.MTA.Allow || p.Network.Allow {
		t.Error("side effects must default to disallowed")
	}
}

func TestProjectEnabledFlag(t *testing.T) {
	path := writeParams(t, t.TempDir(), "output_dir: out\nprojects:\n  - path: a\n    enabled: false\n  - path: b\n    enabled: true\n")
	p, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Projects[0].IsEnabled() || !p.Projects[1].IsEnabled() {
		t.Errorf("enabled flags wrong: %+v", p.Projects)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mention string
	}{
		{"build.jdk not in jdks", "output_dir: out\njdks:\n  - {name: a, home: h}\nbuild:\n  jdk: missing\n", `build.jdk "missing"`},
		{"build.jdk with no jdks at all", "output_dir: out\nbuild:\n  jdk: j21\n", "build.jdk"},
		{"mta.jdk not in jdks", "output_dir: out\nmta:\n  jdk: nope\n", `mta.jdk "nope"`},
		{"missing output_dir", "projects: []\n", "output_dir is required"},
		{"bad mta mode", "output_dir: out\nmta:\n  mode: turbo\n", "mta.mode"},
		{"jdk without home", "output_dir: out\njdks:\n  - {name: a}\n", "name and home"},
		{"empty file", "", "output_dir is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeParams(t, t.TempDir(), tc.content))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %q", err, tc.mention)
			}
		})
	}
}

func TestValidateAcceptsConsistentFile(t *testing.T) {
	path := writeParams(t, t.TempDir(), "output_dir: out\njdks:\n  - {name: j17, home: jdk17}\nbuild:\n  jdk: j17\nmta:\n  jdk: j17\n  mode: full\n")
	p, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.JDKHome("j17"); got != filepath.Join(filepath.Dir(p.Path), "jdk17") {
		t.Errorf("JDKHome = %q", got)
	}
	if p.JDKHome("none") != "" {
		t.Error("unknown JDK should have no home")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("loading a missing file must fail")
	}
}

func TestFindExplicitPath(t *testing.T) {
	dir := t.TempDir()
	got, err := config.Find(filepath.Join(dir, "custom.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "custom.yaml") {
		t.Errorf("Find = %q", got)
	}
}

func TestAddJDKIsIdempotentAndCaseInsensitive(t *testing.T) {
	p := &config.Parameters{}
	p.AddJDK("a", filepath.Join("C:", "Jdks", "A"))
	p.AddJDK("b", filepath.Join("C:", "Jdks", "A"))
	if len(p.JDKs) != 1 {
		t.Fatalf("same home added twice: %+v", p.JDKs)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := writeParams(t, dir, "output_dir: out\nprojects:\n  - path: p\n    name: demo\nbuild:\n  allow: true\n")
	p, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	q, err := config.Load(path)
	if err != nil {
		t.Fatalf("a saved file must load again: %v", err)
	}
	if q.OutputDir != p.OutputDir || len(q.Projects) != 1 || q.Projects[0].Name != "demo" || !q.Build.Allow {
		t.Errorf("round trip changed the parameters: %+v", q)
	}
}
