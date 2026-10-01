// Package discovery turns user-supplied paths into canonical project roots, and finds candidate
// repositories under a parent directory.
package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Canonical returns the absolute, symlink-resolved path. Surrounding quotes from a pasted path
// are stripped.
func Canonical(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return filepath.Clean(abs), nil
}

// Key is the comparison form of a canonical path (case-insensitive on Windows).
func Key(canonical string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(canonical)
	}
	return canonical
}

// ID is 8 hex chars identifying the project by canonical path.
func ID(canonical string) string {
	sum := sha256.Sum256([]byte(Key(canonical)))
	return hex.EncodeToString(sum[:])[:8]
}

// Inside reports whether p is equal to or below root (both canonical).
func Inside(p, root string) bool {
	kp, kr := Key(p), Key(root)
	if kp == kr {
		return true
	}
	return strings.HasPrefix(kp, strings.TrimSuffix(kr, string(filepath.Separator))+string(filepath.Separator))
}

// projectMarkers identify a directory as a candidate repository.
var projectMarkers = []string{".git", "build.xml", "pom.xml", "build.gradle", "build.gradle.kts", ".project", "nbproject"}

// Candidates lists immediate and second-level subdirectories of parent that look like projects.
// It stops descending at the first match so a repo's modules are not listed separately.
func Candidates(parent string) ([]string, error) {
	root, err := Canonical(parent)
	if err != nil {
		return nil, err
	}
	var out []string
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if looksLikeProject(sub) {
				out = append(out, sub)
			} else if depth < 2 {
				visit(sub, depth+1)
			}
		}
	}
	if looksLikeProject(root) {
		return []string{root}, nil
	}
	visit(root, 1)
	return out, nil
}

func looksLikeProject(dir string) bool {
	for _, m := range projectMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}
