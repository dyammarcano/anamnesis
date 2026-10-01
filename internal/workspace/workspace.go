// Package workspace owns Anamnesis's output layout. All output lives outside analyzed
// repositories (ADR-0002).
package workspace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"anamnesis/internal/discovery"
	"anamnesis/internal/model"
)

// Workspace is one Anamnesis invocation's output root and run id.
type Workspace struct {
	Root  string // output root (absolute)
	RunID string // UTC start time, 20060102T150405Z
	Start time.Time
}

// New validates root (parameters.yaml output_dir) against the analyzed project roots and returns a
// workspace. It refuses any output root equal to or inside an analyzed project.
func New(root string, projectRoots []string) (*Workspace, error) {
	if root == "" {
		return nil, fmt.Errorf("output_dir is not set in parameters.yaml")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Resolve symlinks/junctions on the deepest existing ancestor, so an output_dir that is a link
	// into an analyzed repository is recognised as inside it.
	if abs, err = resolveExisting(abs); err != nil {
		return nil, fmt.Errorf("cannot resolve output_dir %s: %w", root, err)
	}
	for _, pr := range projectRoots {
		if discovery.Inside(abs, pr) {
			return nil, fmt.Errorf("output root %s is inside analyzed project %s; refusing to write into an analyzed repository", abs, pr)
		}
	}
	now := time.Now().UTC()
	return &Workspace{Root: abs, RunID: now.Format("20060102T150405Z"), Start: now}, nil
}

// ProjectRunDir is <output_dir>/<name>-<id>/<run-id>.
func (w *Workspace) ProjectRunDir(name, id string) string {
	return filepath.Join(w.Root, name+"-"+id, w.RunID)
}

// ConsolidatedDir is <output_dir>/consolidated/<run-id>.
func (w *Workspace) ConsolidatedDir() string {
	return filepath.Join(w.Root, "consolidated", w.RunID)
}

// WriteEvidence writes evidence.jsonl (one JSON object per line, caller-provided order).
func WriteEvidence(dir string, ev []model.Evidence) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "evidence.jsonl"))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, e := range ev {
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close() // a failed close can mean a truncated evidence file; report it
}

// ReadEvidence loads evidence.jsonl.
func ReadEvidence(dir string) ([]model.Evidence, error) {
	f, err := os.Open(filepath.Join(dir, "evidence.jsonl"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []model.Evidence
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var e model.Evidence
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// WriteJSON writes v as indented JSON to dir/name.
func WriteJSON(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
}

// resolveExisting evaluates symlinks on the deepest existing ancestor of p and re-appends the
// part that does not exist yet.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil // nothing exists, not even the volume root; nothing to resolve
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
