// Package proc runs external commands for analyzers: bounded output, timeout, full logs on disk,
// and a CommandRecord as evidence. It never runs anything with the analyzed repo as working dir
// unless the caller passes it explicitly (build execution only).
package proc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dyammarcano/anamnesis/internal/model"
)

const tailBytes = 64 * 1024

// Spec describes one command.
type Spec struct {
	Name    string // executable; resolved with exec.LookPath (PATHEXT aware on Windows)
	Args    []string
	Dir     string        // working directory; must be set
	Timeout time.Duration // 0 → 2 minutes
	Env     []string      // extra KEY=VALUE pairs appended to the inherited environment
	LogDir  string        // absolute dir for stdout.log/stderr.log; "" → no files
	LogName string        // file stem inside LogDir, e.g. "ant-version"
	RunDir  string        // run root, used to make LogRef relative
}

// Result carries the tails of stdout/stderr (last 64 KiB each) and the record.
type Result struct {
	Record model.CommandRecord
	Stdout string
	Stderr string
}

// Run executes spec. A missing executable or non-zero exit is returned in the Record, not as an
// error; err is non-nil only for misuse (empty Dir).
func Run(ctx context.Context, s Spec) (Result, error) {
	if s.Dir == "" {
		return Result{}, errors.New("proc: Dir must be set")
	}
	if s.Timeout == 0 {
		s.Timeout = 2 * time.Minute
	}
	rec := model.CommandRecord{Argv: append([]string{s.Name}, s.Args...), Dir: s.Dir, ExitCode: -1}
	for _, kv := range s.Env {
		if i := bytes.IndexByte([]byte(kv), '='); i > 0 {
			rec.EnvNames = append(rec.EnvNames, kv[:i])
		}
	}
	path, err := exec.LookPath(s.Name)
	if err != nil {
		rec.Err = err.Error()
		return Result{Record: rec}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	// TODO(windows): kill the whole process tree via a Job Object; CommandContext kills only the
	// direct child, so grandchildren (e.g. java.exe under ant.bat) can outlive a timeout.
	cmd := exec.CommandContext(cctx, path, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.Env...)

	outTail, errTail := &tail{max: tailBytes}, &tail{max: tailBytes}
	var outW, errW io.Writer = outTail, errTail
	var files []*os.File
	if s.LogDir != "" {
		if err := os.MkdirAll(s.LogDir, 0o755); err == nil {
			stem := s.LogName
			if stem == "" {
				stem = filepath.Base(s.Name)
			}
			if f, err := os.Create(filepath.Join(s.LogDir, stem+".stdout.log")); err == nil {
				outW = io.MultiWriter(outTail, f)
				files = append(files, f)
				rec.LogRef = rel(s.RunDir, filepath.Join(s.LogDir, stem+".stdout.log"))
			}
			if f, err := os.Create(filepath.Join(s.LogDir, stem+".stderr.log")); err == nil {
				errW = io.MultiWriter(errTail, f)
				files = append(files, f)
			}
		}
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	start := time.Now()
	runErr := cmd.Run()
	rec.DurationMs = time.Since(start).Milliseconds()
	for _, f := range files {
		_ = f.Close() // log files are best-effort; the command record is the evidence
	}
	if cctx.Err() == context.DeadlineExceeded {
		rec.TimedOut = true
	}
	if cmd.ProcessState != nil {
		rec.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		if _, ok := errors.AsType[*exec.ExitError](runErr); !ok {
			rec.Err = runErr.Error()
		}
	}
	return Result{Record: rec, Stdout: outTail.String(), Stderr: errTail.String()}, nil
}

func rel(base, p string) string {
	if base == "" {
		return filepath.ToSlash(p)
	}
	if r, err := filepath.Rel(base, p); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

// tail keeps the last max bytes written.
type tail struct {
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.buf) }
