// Command treedigest writes a deterministic manifest (relpath, size, sha256) of a directory tree
// and prints a single digest over it. Used to prove an analyzed fixture was not modified:
// run before and after an Anamnesis run and compare digests.
//
//	go run ./tools/treedigest <dir> <manifest-out>
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type entry struct {
	rel  string
	size int64
	sum  string
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: treedigest <dir> <manifest-out>")
		os.Exit(2)
	}
	root, out := os.Args[1], os.Args[2]
	var entries []entry
	var dirs, failures int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			failures++
			fmt.Fprintln(os.Stderr, "walk:", p, err)
			return nil
		}
		if d.IsDir() {
			dirs++
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		sum, size, err := hashFile(p)
		if err != nil {
			failures++
			fmt.Fprintln(os.Stderr, "hash:", p, err)
			return nil
		}
		entries = append(entries, entry{filepath.ToSlash(rel), size, sum})
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "walk:", err)
		os.Exit(1)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	w := bufio.NewWriter(f)
	tree := sha256.New()
	var total int64
	for _, e := range entries {
		line := fmt.Sprintf("%s  %d  %s\n", e.sum, e.size, e.rel)
		if _, err := w.WriteString(line); err != nil {
			fmt.Fprintln(os.Stderr, "write manifest:", err)
			os.Exit(1)
		}
		tree.Write([]byte(line))
		total += e.size
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "write manifest:", err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close manifest:", err)
		os.Exit(1)
	}

	fmt.Printf("files: %d  dirs: %d  bytes: %d  failures: %d\n", len(entries), dirs, total, failures)
	fmt.Printf("tree-sha256: %s\n", hex.EncodeToString(tree.Sum(nil)))
	fmt.Printf("manifest: %s\n", out)
	if failures > 0 {
		os.Exit(1)
	}
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
