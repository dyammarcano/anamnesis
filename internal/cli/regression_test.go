package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression (EJBCA validation): `--only antsafety` matched no analyzer, ran nothing and reported
// success. An unknown analyzer name must be a usage error.
func TestOnlyRejectsUnknownAnalyzer(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	params := filepath.Join(dir, "parameters.yaml")
	body := "output_dir: " + filepath.Join(dir, "out") + "\nprojects:\n  - path: " + proj + "\n"
	if err := os.WriteFile(params, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"preflight", "--parameters", params, "--only", "no-such-analyzer"}); code != 1 {
		t.Fatalf("exit code = %d, want 1 for an unknown analyzer name", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Fatal("an assessment was written although --only named no known analyzer")
	}
}
