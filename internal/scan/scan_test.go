package scan_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/dyammarcano/anamnesis/internal/scan"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rels(files []scan.File) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Rel)
	}
	return out
}

func TestWalkPathContainingSpaces(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "unit", "dir with spaces"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root, " ") {
		t.Fatalf("fixture path has no space: %q", root)
	}
	ix, err := scan.Walk(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.WalkErrors) != 0 {
		t.Errorf("walk errors: %v", ix.WalkErrors)
	}
	want := []string{"readme notes.txt", "src/My App/Hello World.java"}
	got := rels(ix.Files)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("files = %q, want %q", got, want)
	}
	if ix.JavaFiles != 1 || ix.JavaLines != 4 {
		t.Errorf("JavaFiles=%d JavaLines=%d, want 1 and 4", ix.JavaFiles, ix.JavaLines)
	}
	f := ix.WithExt(".java")[0]
	if _, err := os.Stat(ix.Abs(f)); err != nil {
		t.Errorf("Abs of a file with spaces does not resolve: %v", err)
	}
	if got := ix.Under("src/My App"); len(got) != 1 {
		t.Errorf("Under with a spaced dir returned %v", rels(got))
	}
	if got := ix.Named("HELLO WORLD.java"); len(got) != 1 {
		t.Errorf("Named is not case-insensitive for spaced names: %v", rels(got))
	}
	if strings.Join(ix.Dirs, "|") != "src|src/My App" {
		t.Errorf("Dirs = %q", ix.Dirs)
	}
}

func TestWalkSkipsVCSDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/A.java", "class A {}\n")
	write(t, root, ".git/HEAD", "ref: refs/heads/main\n")
	write(t, root, ".git/objects/Evil.java", "class Evil {}\n")
	write(t, root, ".svn/entries", "x")
	write(t, root, ".hg/store/data.java", "class H {}\n")
	write(t, root, "sub/.git/config", "[core]\n") // nested VCS dir: skipped, but not the root marker
	write(t, root, "sub/B.java", "class B {}\n")
	ix, err := scan.Walk(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !ix.HasGitDir {
		t.Error("HasGitDir should be true for a .git directory at the root")
	}
	got := strings.Join(rels(ix.Files), "|")
	if got != "src/A.java|sub/B.java" {
		t.Errorf("files = %q; VCS content leaked into the index or real files are missing", got)
	}
	for _, d := range ix.Dirs {
		if strings.Contains(d, ".git") || strings.Contains(d, ".svn") || strings.Contains(d, ".hg") {
			t.Errorf("VCS dir %q listed", d)
		}
	}
	if ix.JavaFiles != 2 {
		t.Errorf("JavaFiles = %d, want 2 (files under VCS dirs must not count)", ix.JavaFiles)
	}
}

func TestWalkGitMarkerVariants(t *testing.T) {
	tests := []struct {
		name  string
		setup func(root string)
		want  bool
	}{
		{"no git", func(root string) { write(t, root, "a.txt", "x") }, false},
		{"git directory at root", func(root string) { write(t, root, ".git/HEAD", "x") }, true},
		{"git file at root (worktree or submodule)", func(root string) { write(t, root, ".git", "gitdir: ../elsewhere\n") }, true},
		{"git directory only in a subfolder", func(root string) { write(t, root, "sub/.git/HEAD", "x") }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(root)
			ix, err := scan.Walk(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if ix.HasGitDir != tc.want {
				t.Errorf("HasGitDir = %v, want %v", ix.HasGitDir, tc.want)
			}
			for _, f := range ix.Files {
				if f.Base == ".git" {
					t.Errorf("the .git marker file was indexed as project content: %s", f.Rel)
				}
			}
		})
	}
}

func TestWalkIndexQueries(t *testing.T) {
	root := t.TempDir()
	write(t, root, "build.xml", "<project/>")
	write(t, root, "mods/Build.XML", "<project/>")
	write(t, root, "mods/build-extra.xml", "<project/>")
	write(t, root, "lib/Foo.JAR", "x")
	write(t, root, "lib/bar.jar", "x")
	write(t, root, "noext", "x")
	write(t, root, "src/Main.java", "a\nb\nc") // 3 lines, no trailing newline
	ix, err := scan.Walk(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(ix.Named("build.xml")); strings.Join(got, "|") != "build.xml|mods/Build.XML" {
		t.Errorf("Named(build.xml) = %v", got)
	}
	if got := len(ix.WithExt(".jar")); got != 2 {
		t.Errorf("WithExt(.jar) = %d, want 2 (extension match is case-insensitive)", got)
	}
	if got := rels(ix.Match("build*.xml")); strings.Join(got, "|") != "build.xml|mods/Build.XML|mods/build-extra.xml" {
		t.Errorf("Match = %v", got)
	}
	if got := rels(ix.Under("mods")); len(got) != 2 {
		t.Errorf("Under(mods) = %v", got)
	}
	if got := rels(ix.Under("mod")); len(got) != 0 {
		t.Errorf("Under must match whole path segments, got %v", got)
	}
	if !ix.HasRoot("BUILD.XML") || ix.HasRoot("build-extra.xml") {
		t.Error("HasRoot must only report files at the root")
	}
	if ix.ByExt[""] != 1 {
		t.Errorf("files without extension = %d, want 1", ix.ByExt[""])
	}
	if ix.JavaLines != 3 {
		t.Errorf("JavaLines = %d, want 3 (last line without newline counts)", ix.JavaLines)
	}
	if ix.TotalBytes == 0 {
		t.Error("TotalBytes not accumulated")
	}
	// the index is sorted
	for i := 1; i < len(ix.Files); i++ {
		if ix.Files[i-1].Rel > ix.Files[i].Rel {
			t.Fatalf("files not sorted: %s > %s", ix.Files[i-1].Rel, ix.Files[i].Rel)
		}
	}
}

func TestWalkCancelledContext(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scan.Walk(ctx, root); err == nil {
		t.Error("a cancelled walk must return an error")
	}
}

func TestWalkNeverWrites(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/A.java", "class A {}\n")
	before := snapshot(t, root)
	if _, err := scan.Walk(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, root); after != before {
		t.Errorf("Walk changed the tree:\n before %q\n after  %q", before, after)
	}
}

func snapshot(t *testing.T, root string) string {
	t.Helper()
	var parts []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		parts = append(parts, rel+"|"+info.ModTime().String()+"|"+string(rune('0'+info.Size()%10)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, ";")
}

func TestCountNewlines(t *testing.T) {
	const bufSize = 64 * 1024
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"empty", "", 0},
		{"single newline", "\n", 1},
		{"single char no newline", "a", 1},
		{"one line with newline", "a\n", 1},
		{"two lines no trailing newline", "a\nb", 2},
		{"two lines trailing newline", "a\nb\n", 2},
		{"blank lines", "\n\n\n", 3},
		{"leading blank line", "\na", 2},
		{"CRLF counts once per line", "a\r\nb\r\n", 2},
		{"lone CR is not a line break", "a\rb", 1},
		{"no newline longer than the buffer", strings.Repeat("a", 3*bufSize+17), 1},
		{"many lines across buffer boundaries", strings.Repeat("x\n", 5*bufSize), 5 * bufSize},
		{"newline exactly on the buffer boundary", strings.Repeat("a", bufSize-1) + "\n" + "tail", 2},
		{"buffer-sized content ending in newline", strings.Repeat("a", bufSize-1) + "\n", 1},
		{"unicode content", "café\n世界", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := scan.CountNewlines(strings.NewReader(tc.in)); got != tc.want {
				t.Errorf("CountNewlines = %d, want %d", got, tc.want)
			}
			// the count must not depend on how the reader chunks the data
			if got := scan.CountNewlines(iotest.OneByteReader(strings.NewReader(tc.in))); got != tc.want {
				t.Errorf("OneByteReader: CountNewlines = %d, want %d", got, tc.want)
			}
			if got := scan.CountNewlines(iotest.DataErrReader(strings.NewReader(tc.in))); got != tc.want {
				t.Errorf("DataErrReader: CountNewlines = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCountNewlinesStopsAtReadError(t *testing.T) {
	// data followed by an error: the lines read so far are counted, nothing panics
	r := io.MultiReader(strings.NewReader("a\nb\n"), iotest.ErrReader(io.ErrUnexpectedEOF))
	if got := scan.CountNewlines(r); got != 2 {
		t.Errorf("CountNewlines = %d, want 2", got)
	}
}
