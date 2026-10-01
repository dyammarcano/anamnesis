// Package cli implements the command line: preflight, analyze, report and discover. It also owns
// Assess, the single engine path shared with the TUI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/config"
	"github.com/dyammarcano/anamnesis/internal/correlate"
	"github.com/dyammarcano/anamnesis/internal/discovery"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/report"
	"github.com/dyammarcano/anamnesis/internal/workspace"
)

var usage = `Anamnesis ` + Version + ` - assess legacy Java repositories

Usage:
  anamnesis                         interactive TUI
  anamnesis preflight [path...]     Preflight phase only (never runs project code, never installs)
  anamnesis analyze   [path...]     Preflight, environment preparation, then Deep phase
  anamnesis report <run-dir>        re-render report.json and report.html from evidence.jsonl
  anamnesis discover [parent...]    list candidate repositories under a parent folder

Options (preflight, analyze):
  --only a,b            run only the named analyzers (rerun failed ones)

Global:
  --parameters <file>   parameters.yaml (default: working directory, then next to the executable)

With no paths, preflight and analyze use the enabled projects in parameters.yaml. Output location,
side effects (build, MTA, network), tools and timeouts all come from parameters.yaml; there are no
other configuration flags and no environment variables. See parameters.example.yaml.
`

// LoadParams finds and loads parameters.yaml, with an error that names the file.
func LoadParams(file string) (*config.Parameters, error) {
	path, created, err := config.FindOrCreate(file)
	if err != nil {
		return nil, err
	}
	p, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("cannot use parameters file %s: %w\nsee parameters.example.yaml in the repository for the expected layout", path, err)
	}
	p.Created = created
	if created {
		fmt.Fprintf(os.Stderr, "created a starter %s at %s\n(builds, MTA, network and installs are off; edit that file to enable them)\n", config.FileName, path)
	}
	return p, nil
}

// extractParameters removes the global --parameters flag from args.
func extractParameters(args []string) (string, []string, error) {
	var file string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--parameters" || a == "-parameters":
			if i+1 >= len(args) {
				return "", nil, errors.New("--parameters needs a file")
			}
			file = args[i+1]
			i++
		case strings.HasPrefix(a, "--parameters="):
			file = strings.TrimPrefix(a, "--parameters=")
		case strings.HasPrefix(a, "-parameters="):
			file = strings.TrimPrefix(a, "-parameters=")
		default:
			rest = append(rest, a)
		}
	}
	return file, rest, nil
}

// parseArgs parses flags that may appear before, between or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos, nil
}

// Run executes a subcommand and returns the process exit code: 0 when the run completed (even with
// failed analyzers), 1 on a usage or IO error.
func Run(args []string) int {
	file, args, err := extractParameters(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
	switch args[0] {
	case "preflight":
		return cmdAssess("preflight", file, args[1:], false)
	case "analyze":
		return cmdAssess("analyze", file, args[1:], true)
	case "report":
		return cmdReport(args[1:])
	case "discover":
		return cmdDiscover(file, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0
	case "version", "--version":
		fmt.Println("anamnesis", Version)
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n%s", args[0], usage)
	return 1
}

func cmdAssess(name, file string, args []string, deep bool) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	only := fs.String("only", "", "comma-separated analyzer names")
	pos, err := parseArgs(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	params, err := LoadParams(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	opts := Options{Params: params, Args: append([]string{name}, args...), Phases: []engine.Phase{engine.Preflight}}
	if deep {
		opts.Phases = []engine.Phase{engine.Preflight, engine.Deep}
	}
	if *only != "" {
		registerOnce.Do(registerAll)
		known := map[string]bool{}
		var names []string
		for _, a := range engine.Registered() {
			known[a.Name()] = true
			names = append(names, a.Name())
		}
		opts.Only = map[string]bool{}
		for n := range strings.SplitSeq(*only, ",") {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			if !known[n] {
				fmt.Fprintf(os.Stderr, "error: unknown analyzer %q in --only; known analyzers: %s\n", n, strings.Join(names, ", "))
				return 1
			}
			opts.Only[n] = true
		}
	}
	opts.Progress = func(ev engine.Event) {
		if ev.State == "failed" || ev.State == "skipped" {
			fmt.Fprintf(os.Stderr, "  [%s] %s %s %s\n", ev.Project, ev.Analyzer, ev.State, ev.Err)
		}
	}
	if deep {
		fmt.Printf("parameters: %s\nside effects allowed there: build=%v mta=%v network=%v\n", params.Path, params.Build.Allow, params.MTA.Allow, params.Network.Allow)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	outs, err := Assess(ctx, pos, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if len(outs) == 0 {
			return 1
		}
	}
	code := 0
	if err != nil {
		code = 1
	}
	for _, o := range outs {
		Summarize(os.Stdout, o)
		if o.Err != nil {
			code = 1
		}
	}
	if len(outs) > 0 && outs[0].ConsolidatedDir != "" {
		fmt.Printf("\nconsolidated report: %s\n", filepath.Join(outs[0].ConsolidatedDir, "consolidated-report.html"))
	}
	if ctx.Err() != nil {
		fmt.Println("interrupted: partial evidence and reports were written")
	}
	return code
}

// Summarize prints the key conclusions of one outcome and where its report is.
func Summarize(w io.Writer, o Outcome) {
	_, _ = fmt.Fprintf(w, "\n== %s\n", o.Name)
	if o.RunDir == "" {
		_, _ = fmt.Fprintf(w, "   not assessed: %v\n", o.Err)
		return
	}
	for _, t := range []string{"build.systems", "java.generation", "java.local-runtime", "buildability.state", "environment.missing", "migration.mta", "migration.effort"} {
		if c := o.Conclusion(t); c != nil {
			_, _ = fmt.Fprintf(w, "   [%s/%s] %s: %s\n", c.Kind, c.Confidence, t, trunc(c.Statement, 220))
		}
	}
	if f := o.Failed(); len(f) > 0 {
		_, _ = fmt.Fprintf(w, "   FAILED analyzers: %s\n", strings.Join(f, ", "))
	}
	if o.Err != nil {
		_, _ = fmt.Fprintf(w, "   write error: %v\n", o.Err)
	}
	_, _ = fmt.Fprintf(w, "   report: %s\n", filepath.Join(o.RunDir, "report.html"))
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func cmdReport(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: anamnesis report <run-dir>")
		return 1
	}
	dir, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ev, err := workspace.ReadEvidence(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read evidence from %s: %v\n", dir, err)
		return 1
	}
	meta := report.Meta{Name: filepath.Base(filepath.Dir(dir)), RunID: filepath.Base(dir), RunDir: dir}
	var timings []engine.Timing
	if b, err := os.ReadFile(filepath.Join(dir, "run.json")); err == nil {
		var m runManifest
		if json.Unmarshal(b, &m) == nil {
			meta.Name, meta.Root, meta.ID, meta.RunID = m.Project.Name, m.Project.Root, m.Project.ID, m.RunID
			meta.Args, meta.Started, meta.Ended = m.Args, m.Started, m.Ended
			timings = m.Timings
		}
	}
	engine.SortEvidence(ev)
	rep := report.Build(meta, ev, correlate.Correlate(ev), timings)
	if err := rep.WriteAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("re-rendered:", filepath.Join(dir, "report.html"))
	return 0
}

func cmdDiscover(file string, args []string) int {
	parents := args
	if len(parents) == 0 {
		params, err := LoadParams(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		parents = params.DiscoverRoots
		if len(parents) == 0 {
			fmt.Fprintln(os.Stderr, "usage: anamnesis discover <parent>  (or set discover_roots in parameters.yaml)")
			return 1
		}
	}
	code := 0
	for _, parent := range parents {
		c, err := discovery.Candidates(parent)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", parent, err)
			code = 1
			continue
		}
		fmt.Printf("%s: %d candidate(s)\n", parent, len(c))
		for _, p := range c {
			fmt.Println("  " + p)
		}
	}
	return code
}
