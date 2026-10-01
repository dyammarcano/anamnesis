package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dyammarcano/anamnesis/internal/model"
)

// treeDigest hashes a directory tree: fileDigest is computed exactly like the repository's
// tools/treedigest (relative path, size, sha256 of every file), dirDigest additionally
// covers the directory names so a stray empty directory is noticed too.
func treeDigest(t *testing.T, root string) (fileDigest, dirDigest string, files int) {
	t.Helper()
	type entry struct {
		rel  string
		size int64
		sum  string
	}
	var entries []entry
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, rel)
			}
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel, n, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		t.Fatalf("hashing %s: %v", root, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	sort.Strings(dirs)
	tree := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(tree, "%s  %d  %s\n", e.sum, e.size, e.rel)
	}
	dtree := sha256.New()
	_, _ = dtree.Write(tree.Sum(nil))
	_, _ = dtree.Write([]byte(strings.Join(dirs, "\n")))
	return hex.EncodeToString(tree.Sum(nil)), hex.EncodeToString(dtree.Sum(nil)), len(entries)
}

// copyTree copies src to dst (dst is created).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func ofKind(ev []model.Evidence, kind string) []model.Evidence {
	var out []model.Evidence
	for _, e := range ev {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func confRank(c model.Confidence) int {
	switch c {
	case model.Observed:
		return 3
	case model.Derived:
		return 2
	case model.Inferred:
		return 1
	}
	return 0
}
