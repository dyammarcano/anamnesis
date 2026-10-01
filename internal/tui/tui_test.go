package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/dyammarcano/anamnesis/internal/config"
)

// newTestModel builds the TUI model over a temp parameters.yaml with no projects.
func newTestModel(t *testing.T) (*model_, string) {
	t.Helper()
	dir := t.TempDir()
	pfile := filepath.Join(dir, "parameters.yaml")
	if err := os.WriteFile(pfile, []byte("output_dir: "+filepath.Join(dir, "out")+"\nprojects: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := config.Load(pfile)
	if err != nil {
		t.Fatal(err)
	}
	return newModel(p), dir
}

func press(m *model_, msgs ...tea.Msg) {
	for _, msg := range msgs {
		m.Update(msg)
	}
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// Every rendered line must fit the terminal, and the whole screen must fit its height; otherwise the
// terminal wraps or scrolls and the bottom lines (the "Add path" prompt, messages) disappear.
func assertFits(t *testing.T, view string, w, h int) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	for i, l := range lines {
		if lw := lipgloss.Width(l); lw > w {
			t.Errorf("line %d is %d cells wide on a %d-cell terminal (it will wrap): %q", i+1, lw, w, l)
		}
	}
	if len(lines) > h {
		t.Errorf("view has %d lines on a %d-line terminal; the bottom %d line(s) are pushed off screen", len(lines), h, len(lines)-h)
	}
}

func TestLayoutFitsCommonTerminalSizes(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {120, 30}, {160, 40}} {
		m, _ := newTestModel(t)
		press(m, tea.WindowSizeMsg{Width: sz[0], Height: sz[1]}, runes("a"))
		v := m.View()
		if !strings.Contains(v, "Project path") {
			t.Errorf("%dx%d: after pressing A the path prompt is not rendered", sz[0], sz[1])
		}
		assertFits(t, v, sz[0], sz[1])
	}
}

func TestAddPathTypedThenEnter(t *testing.T) {
	m, dir := newTestModel(t)
	repo := filepath.Join(dir, "my repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	press(m, tea.WindowSizeMsg{Width: 120, Height: 30}, runes("a"))
	for _, r := range repo { // typed one key at a time, as a person would
		if r == ' ' {
			press(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		press(m, runes(string(r)))
	}
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.params.Projects) != 1 {
		t.Fatalf("projects = %d after typing a path and Enter, want 1 (message: %q)", len(m.params.Projects), m.msg)
	}
}

func TestAddPathPasted(t *testing.T) {
	m, dir := newTestModel(t)
	repo := filepath.Join(dir, "pasted")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	press(m, tea.WindowSizeMsg{Width: 120, Height: 30}, runes("a"),
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(`"` + repo + `"`), Paste: true},
		tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.params.Projects) != 1 {
		t.Fatalf("projects = %d after pasting a quoted path and Enter, want 1 (message: %q)", len(m.params.Projects), m.msg)
	}
}

// The operator's case: paste three project paths at once (one per line) and press Enter.
func TestAddThreePathsPastedAsLines(t *testing.T) {
	m, dir := newTestModel(t)
	var paths []string
	for _, n := range []string{"repo one", "repo-two", "repo_three"} {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	press(m, tea.WindowSizeMsg{Width: 100, Height: 30}, runes("A"),
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Join(paths, "\r\n")), Paste: true},
		tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.params.Projects) != 3 {
		t.Fatalf("projects = %d after pasting three lines, want 3 (message: %q)", len(m.params.Projects), m.msg)
	}
	assertFits(t, m.View(), 100, 30)
}

// The same three paths typed with ";" between them, plus one that does not exist.
func TestAddPathsSemicolonSeparatedReportsBadOnes(t *testing.T) {
	m, dir := newTestModel(t)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	press(m, runes("a"), runes(a+";"+b+";"+filepath.Join(dir, "missing")), tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.params.Projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(m.params.Projects))
	}
	if !strings.Contains(m.msg, "Not added") || !strings.Contains(m.msg, "missing") {
		t.Fatalf("message does not name the path that could not be added: %q", m.msg)
	}
	// and the project list is persisted to parameters.yaml
	reloaded, err := config.Load(m.params.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Projects) != 2 {
		t.Fatalf("parameters.yaml holds %d projects after adding, want 2", len(reloaded.Projects))
	}
}

// The prompt must stay visible while typing a long path on a small terminal.
func TestPromptVisibleWhileTypingLongPathOn80x24(t *testing.T) {
	m, _ := newTestModel(t)
	press(m, tea.WindowSizeMsg{Width: 80, Height: 24}, runes("a"), runes(`C:\very\long\path\to\some\enterprise\legacy\repository\with\many\levels\project-name`))
	v := m.View()
	assertFits(t, v, 80, 24)
	if !strings.Contains(v, "project-name") {
		t.Fatalf("the end of the typed path is not visible in the prompt:\n%s", v)
	}
}
