package mta

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/config"
)

// Install describes an existing MTA installation. Anamnesis only reads it, never creates or alters it.
type Install struct {
	Exe     string // absolute path of the CLI executable
	Dir     string // value passed to the child as KANTRA_DIR: contains rulesets/, jdtls/, static-report/
	Version string // first line of `<exe> version`, best effort; "" when it could not be read
}

// exeNames are the executable names MTA / kantra distributions use, in preference order.
func exeNames() []string {
	base := []string{"windows-mta-cli", "mta-cli", "kantra"}
	if runtime.GOOS == "windows" {
		out := make([]string, 0, len(base))
		for _, b := range base {
			out = append(out, b+".exe")
		}
		return out
	}
	return base
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// MissingParts lists the entries kantra requires inside dir (P-3) that are absent.
func MissingParts(dir string) []string {
	var missing []string
	for _, sub := range []string{"rulesets", "jdtls", "static-report"} {
		if !isDir(filepath.Join(dir, sub)) {
			missing = append(missing, sub+"/")
		}
	}
	return missing
}

// Locate resolves the installation from parameters only: mta.install_dir and mta.executable.
// Anamnesis never consults KANTRA_DIR, PATH or the user profile for this. When there is no usable
// installation, reason says what to fix (it becomes the Finding text).
func Locate(ctx context.Context, cfg config.MTA) (inst Install, reason string) {
	if cfg.InstallDir == "" {
		return Install{}, "set mta.install_dir in parameters.yaml"
	}
	if !isDir(cfg.InstallDir) {
		return Install{}, "mta.install_dir does not exist: " + cfg.InstallDir
	}
	if miss := MissingParts(cfg.InstallDir); len(miss) > 0 {
		return Install{}, "mta.install_dir is incomplete (missing " + strings.Join(miss, ", ") + "): " + cfg.InstallDir
	}
	exe := cfg.Executable
	if exe == "" {
		for _, n := range exeNames() {
			if p := filepath.Join(cfg.InstallDir, n); isFile(p) {
				exe = p
				break
			}
		}
		if exe == "" {
			return Install{}, "no MTA executable (" + strings.Join(exeNames(), ", ") + ") inside mta.install_dir; set mta.executable"
		}
	} else if !isFile(exe) {
		return Install{}, "mta.executable does not exist: " + exe
	}
	inst = Install{Exe: exe, Dir: cfg.InstallDir}
	inst.Version = probeVersion(ctx, exe, cfg.InstallDir)
	return inst, ""
}

func probeVersion(ctx context.Context, exe, installDir string) string {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, "version")
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "KANTRA_DIR="+installDir)
	out, _ := cmd.CombinedOutput()
	for line := range strings.SplitSeq(string(out), "\n") {
		if l := strings.TrimSpace(line); l != "" {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return ""
}
