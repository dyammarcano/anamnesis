// Package gitinfo reports the git repository state of a project, taking care never to attribute an
// enclosing repository's facts to the project and never to leak credentials embedded in remote URLs.
package gitinfo

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/proc"
)

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "gitinfo" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

var userinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/@\s]*@`)

// redactURL replaces scheme://user:pass@ with scheme://***@.
func redactURL(s string) string { return userinfoRe.ReplaceAllString(s, "${1}***@") }

type runner struct {
	ctx  context.Context
	p    *engine.Project
	tool string
	last *model.CommandRecord
}

// resolveTool returns the configured executable for git, or "git" resolved from PATH, plus where
// that choice came from.
func resolveTool(p *engine.Project) (tool, from string) {
	if p.Params != nil {
		if t := p.Params.Tool("git"); t != "" {
			return t, "parameters.yaml"
		}
	}
	return "git", "PATH (global tool)"
}

func (r *runner) git(args ...string) (proc.Result, bool) {
	full := append([]string{"--no-optional-locks", "-C", r.p.Root}, args...)
	res, err := proc.Run(r.ctx, proc.Spec{Name: r.tool, Args: full, Dir: r.p.RunDir, Timeout: 30 * time.Second, RunDir: r.p.RunDir})
	if err != nil {
		return res, false
	}
	rec := res.Record
	r.last = &rec
	return res, true
}

func unavailable(res proc.Result) bool { return res.Record.ExitCode == -1 && res.Record.Err != "" }

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	if err := os.MkdirAll(p.RunDir, 0o755); err != nil {
		return err
	}
	tool, from := resolveTool(p)
	r := &runner{ctx: ctx, p: p, tool: tool}
	emit := func(e model.Evidence) {
		e.Category, e.Kind, e.Subject = model.CatGit, model.KindGitRepository, "git"
		e.Command = r.last
		if e.Values == nil {
			e.Values = map[string]string{}
		}
		e.Values["resolved_from"] = from
		sink.Emit(e)
	}

	res, ok := r.git("rev-parse", "--show-toplevel")
	if !ok {
		return nil
	}
	if unavailable(res) {
		emit(model.Evidence{
			Finding: "git not available", Status: model.Warn, Confidence: model.Observed,
			Values:      map[string]string{"state": "unknown"},
			Limitations: []string{"No repository facts could be collected: " + res.Record.Err},
		})
		return nil
	}
	if res.Record.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		switch {
		case strings.Contains(stderr, "not a git repository"):
			emit(model.Evidence{
				Finding: "not a git repository", Status: model.Info, Confidence: model.Observed,
				Values: map[string]string{"state": "none"},
			})
		default:
			emit(model.Evidence{
				Finding: "git could not determine the repository state: " + firstLine(stderr), Status: model.Warn, Confidence: model.Unknown,
				Values:      map[string]string{"state": "unknown"},
				Limitations: []string{"git exited with code " + strconv.Itoa(res.Record.ExitCode) + "; for 'dubious ownership' errors the repository is left untouched and no safe.directory override is applied."},
			})
		}
		return nil
	}

	top := strings.TrimSpace(firstLine(res.Stdout))
	if !sameDir(top, p.Root) {
		emit(model.Evidence{
			Finding: "inside another repository at " + top + "; no repository facts are attributed to this project",
			Status:  model.Info, Confidence: model.Observed,
			Values:      map[string]string{"state": "enclosed", "toplevel": top},
			Limitations: []string{"Facts about the enclosing repository (commit, branch, remotes, dirty state) are deliberately not reported."},
		})
		return nil
	}

	vals := map[string]string{"state": "own", "toplevel": top}
	var limits []string

	if res, ok := r.git("rev-parse", "HEAD"); ok && res.Record.ExitCode == 0 {
		vals["sha"] = strings.TrimSpace(firstLine(res.Stdout))
	} else {
		limits = append(limits, "HEAD has no resolvable commit (empty repository or unreadable).")
	}
	if res, ok := r.git("symbolic-ref", "--short", "HEAD"); ok && res.Record.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
		vals["branch"] = strings.TrimSpace(firstLine(res.Stdout))
	} else {
		vals["branch"] = "detached"
	}
	if res, ok := r.git("status", "--porcelain"); ok && res.Record.ExitCode == 0 {
		n := 0
		for l := range strings.SplitSeq(res.Stdout, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		vals["dirty_count"] = strconv.Itoa(n)
	} else {
		limits = append(limits, "git status failed; dirty_count unknown.")
	}
	if res, ok := r.git("remote", "-v"); ok && res.Record.ExitCode == 0 {
		var lines []string
		for l := range strings.SplitSeq(res.Stdout, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, redactURL(l))
			}
		}
		vals["remotes"] = strings.Join(lines, "; ")
	}

	finding := "project is the root of its own git repository"
	if s := vals["sha"]; s != "" {
		finding += " at " + short(s) + " on " + vals["branch"]
	}
	if d := vals["dirty_count"]; d != "" && d != "0" {
		finding += " with " + d + " uncommitted path(s)"
	}
	emit(model.Evidence{
		Finding: finding + ".", Status: model.Info, Confidence: model.Observed,
		Values: vals, Limitations: append(limits, "Remote URLs have userinfo redacted."),
	})
	return ctx.Err()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// sameDir compares two directory paths: cleaned, case-insensitive on Windows, with a final
// os.SameFile check so symlinks and 8.3 names do not produce a false "enclosed".
func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(filepath.FromSlash(a)), filepath.Clean(filepath.FromSlash(b))
	if ca == cb || (runtime.GOOS == "windows" && strings.EqualFold(ca, cb)) {
		return true
	}
	sa, err1 := os.Stat(ca)
	sb, err2 := os.Stat(cb)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}
