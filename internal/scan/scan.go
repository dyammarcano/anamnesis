// Package scan performs the single streaming walk of a project. Every analyzer reads this index
// instead of walking the tree again.
package scan

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// File is one indexed file. Rel uses forward slashes, relative to the project root.
type File struct {
	Rel  string
	Size int64
	Ext  string // lower-case, with dot; "" when none
	Base string // lower-case base name
}

// Index is the immutable result of one walk.
type Index struct {
	Root       string
	Files      []File // sorted by Rel
	Dirs       []string
	HasGitDir  bool
	TotalBytes int64
	ByExt      map[string]int
	JavaFiles  int
	JavaLines  int64 // newline count over .java files (approximate LOC, includes blanks/comments)
	WalkErrors []string
	byBase     map[string][]int
	byExt      map[string][]int
}

// skipDirs are never descended into: VCS metadata and tool caches that are not project content.
var skipDirs = map[string]bool{".git": true, ".svn": true, ".hg": true}

// Walk indexes root. It never writes anything.
func Walk(ctx context.Context, root string) (*Index, error) {
	ix := &Index{Root: root, ByExt: map[string]int{}, byBase: map[string][]int{}, byExt: map[string][]int{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			ix.WalkErrors = append(ix.WalkErrors, p+": "+err.Error())
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == root {
				return nil
			}
			if skipDirs[d.Name()] {
				if d.Name() == ".git" && filepath.Dir(p) == root {
					ix.HasGitDir = true
				}
				return filepath.SkipDir
			}
			ix.Dirs = append(ix.Dirs, rel)
			return nil
		}
		if d.Name() == ".git" && filepath.Dir(p) == root { // worktree/submodule .git file
			ix.HasGitDir = true
			return nil
		}
		info, err := d.Info()
		if err != nil {
			ix.WalkErrors = append(ix.WalkErrors, p+": "+err.Error())
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		ix.Files = append(ix.Files, File{Rel: rel, Size: info.Size(), Ext: ext, Base: strings.ToLower(d.Name())})
		ix.TotalBytes += info.Size()
		ix.ByExt[ext]++
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ix.Files, func(i, j int) bool { return ix.Files[i].Rel < ix.Files[j].Rel })
	sort.Strings(ix.Dirs)
	for i, f := range ix.Files {
		ix.byBase[f.Base] = append(ix.byBase[f.Base], i)
		ix.byExt[f.Ext] = append(ix.byExt[f.Ext], i)
	}
	java := ix.WithExt(".java")
	ix.JavaFiles = len(java)
	ix.JavaLines = countLines(ctx, root, java)
	return ix, nil
}

// Abs returns the absolute OS path of an indexed file.
func (ix *Index) Abs(f File) string { return filepath.Join(ix.Root, filepath.FromSlash(f.Rel)) }

// WithExt returns files with the given lower-case extension (e.g. ".jar").
func (ix *Index) WithExt(ext string) []File { return ix.pick(ix.byExt[strings.ToLower(ext)]) }

// Named returns files whose base name equals name (case-insensitive).
func (ix *Index) Named(name string) []File { return ix.pick(ix.byBase[strings.ToLower(name)]) }

// Match returns files whose base name matches the glob (case-insensitive, filepath.Match syntax).
func (ix *Index) Match(glob string) []File {
	glob = strings.ToLower(glob)
	var out []File
	for _, f := range ix.Files {
		if ok, _ := filepath.Match(glob, f.Base); ok {
			out = append(out, f)
		}
	}
	return out
}

// Under returns files whose Rel starts with dir + "/".
func (ix *Index) Under(dir string) []File {
	prefix := strings.TrimSuffix(filepath.ToSlash(dir), "/") + "/"
	var out []File
	for _, f := range ix.Files {
		if strings.HasPrefix(f.Rel, prefix) {
			out = append(out, f)
		}
	}
	return out
}

// HasRoot reports whether a file with this base name exists at the project root.
func (ix *Index) HasRoot(name string) bool {
	for _, f := range ix.Named(name) {
		if !strings.Contains(f.Rel, "/") {
			return true
		}
	}
	return false
}

func (ix *Index) pick(idx []int) []File {
	out := make([]File, len(idx))
	for i, j := range idx {
		out[i] = ix.Files[j]
	}
	return out
}

// ForEach runs fn over files with a bounded worker pool. fn must be safe for concurrent use.
func ForEach(ctx context.Context, files []File, fn func(File)) {
	workers := runtime.GOMAXPROCS(0)
	ch := make(chan File)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for f := range ch {
				fn(f)
			}
		})
	}
	for _, f := range files {
		select {
		case ch <- f:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(ch)
	wg.Wait()
}

func countLines(ctx context.Context, root string, files []File) int64 {
	var total atomic.Int64
	ForEach(ctx, files, func(f File) {
		fh, err := os.Open(filepath.Join(root, filepath.FromSlash(f.Rel)))
		if err != nil {
			return
		}
		defer func() { _ = fh.Close() }()
		total.Add(CountNewlines(fh))
	})
	return total.Load()
}

// CountNewlines streams r and counts lines (a trailing line without newline counts as one).
func CountNewlines(r io.Reader) int64 {
	br := bufio.NewReaderSize(r, 64*1024)
	buf := make([]byte, 64*1024)
	var n int64
	var last byte = '\n'
	for {
		k, err := br.Read(buf)
		if k > 0 {
			n += int64(bytes.Count(buf[:k], []byte{'\n'}))
			last = buf[k-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		n++
	}
	return n
}
