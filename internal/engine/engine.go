// Package engine runs analyzers over projects and collects normalized evidence. The CLI and the
// TUI both drive this package; there is no second code path.
package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"anamnesis/internal/config"
	"anamnesis/internal/discovery"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

// Phase orders analyzers: Preflight is fast and never executes repository-controlled code; Deep may
// run tools, and side-effect analyzers run only when the user enabled them (Options.Allow).
type Phase int

const (
	Preflight Phase = iota
	Deep
)

// Capability names a requirement an analyzer has before it may run.
type Capability string

const (
	// SideEffectBuild: executes the project's build (Ant/Maven/Gradle), i.e. repository-controlled code.
	SideEffectBuild Capability = "side-effect:build"
	// SideEffectMTA: runs MTA, which may invoke mvn/gradlew of the project.
	SideEffectMTA Capability = "side-effect:mta"
	// Network: contacts a remote service (e.g. GitHub through gh). Read-only.
	Network Capability = "network"
)

// Project is the shared, read-only view analyzers receive.
type Project struct {
	Root   string      // canonical absolute path
	Name   string      // base name
	ID     string      // 8 hex of canonical path
	Files  *scan.Index // the single walk
	RunDir string      // this project's run directory (absolute); analyzers write only under it

	// Params is parameters.yaml — the ONLY configuration source. Analyzers must not read
	// environment variables to decide behaviour (tool paths, JDKs, MTA install, targets, timeouts
	// all come from here). Observing the environment as evidence is allowed.
	Params *config.Parameters
}

// CapabilityDir returns (and documents) the directory an analyzer may write artifacts to.
func (p *Project) CapabilityDir(analyzer string) string {
	return filepath.Join(p.RunDir, "capabilities", analyzer)
}

// Sink receives evidence as it is produced, so large scans stream and the TUI shows progress.
type Sink interface {
	Emit(model.Evidence)
}

// Analyzer is one independent source of evidence.
type Analyzer interface {
	Name() string
	Phase() Phase
	// Requires lists capabilities that must be allowed before Analyze is called. A side-effect
	// capability that is not allowed makes the engine emit SKIPPED evidence instead of running.
	Requires() []Capability
	Analyze(ctx context.Context, p *Project, sink Sink) error
}

// Timeouter is optionally implemented by analyzers whose own budget (e.g. mta.timeout_minutes,
// build.timeout_minutes) differs from the default per-analyzer timeout. A value > 0 wins.
type Timeouter interface {
	Timeout(p *Project) time.Duration
}

// Options control one run.
type Options struct {
	Phases      []Phase             // which phases to run
	Allow       map[Capability]bool // side effects / network the user enabled
	Concurrency int                 // analyzers in parallel per project; 0 → 4
	Timeout     time.Duration       // per analyzer; 0 → 30 minutes
	Only        map[string]bool     // if non-empty, run only these analyzer names (rerun failed)
	Progress    func(Event)         // optional
}

// Event reports progress to a UI.
type Event struct {
	Project  string
	Analyzer string
	State    string // "start", "done", "failed", "skipped"
	Err      string
	Elapsed  time.Duration
}

// Timing records one analyzer run.
type Timing struct {
	Analyzer   string `json:"analyzer"`
	State      string `json:"state"`
	DurationMs int64  `json:"durationMs"`
	Evidence   int    `json:"evidence"`
	Err        string `json:"err,omitempty"`
}

// Result is the outcome for one project.
type Result struct {
	Project  *Project
	Evidence []model.Evidence // deterministic order
	Timings  []Timing
}

// NewProject canonicalizes root and performs the single walk. The caller sets Params.
func NewProject(ctx context.Context, root, runDir string) (*Project, error) {
	canon, err := discovery.Canonical(root)
	if err != nil {
		return nil, err
	}
	ix, err := scan.Walk(ctx, canon)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", canon, err)
	}
	return &Project{Root: canon, Name: filepath.Base(canon), ID: discovery.ID(canon), Files: ix, RunDir: runDir}, nil
}

type collector struct {
	mu       sync.Mutex
	analyzer string
	items    []model.Evidence
	count    int
}

func (c *collector) Emit(e model.Evidence) {
	if e.Analyzer == "" {
		e.Analyzer = c.analyzer
	}
	e.Finalize()
	c.mu.Lock()
	c.items = append(c.items, e)
	c.count++
	c.mu.Unlock()
}

// Run executes the selected analyzers on one project. Analyzer failures and panics become FAILED
// evidence; Run itself only returns an error when ctx is cancelled before anything ran.
func Run(ctx context.Context, p *Project, analyzers []Analyzer, opt Options) Result {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 4
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Minute
	}
	phases := map[Phase]bool{}
	for _, ph := range opt.Phases {
		phases[ph] = true
	}

	var (
		mu      sync.Mutex
		all     []model.Evidence
		timings []Timing
		wg      sync.WaitGroup
		sem     = make(chan struct{}, opt.Concurrency)
	)
	progress := func(ev Event) {
		if opt.Progress != nil {
			ev.Project = p.Name
			opt.Progress(ev)
		}
	}

	for _, a := range analyzers {
		if len(phases) > 0 && !phases[a.Phase()] {
			continue
		}
		if len(opt.Only) > 0 && !opt.Only[a.Name()] {
			continue
		}
		if missing := missingCaps(a, opt.Allow); len(missing) > 0 {
			ev := model.Evidence{
				Analyzer: a.Name(), Category: model.CatAnalyzer, Kind: "analyzer.skipped", Subject: a.Name(),
				Finding: fmt.Sprintf("%s was not run: requires %v, which was not enabled for this run", a.Name(), missing),
				Status:  model.Skipped, Confidence: model.Observed,
				Values: map[string]string{"requires": fmt.Sprint(missing)},
			}
			ev.Finalize()
			mu.Lock()
			all = append(all, ev)
			timings = append(timings, Timing{Analyzer: a.Name(), State: "skipped"})
			mu.Unlock()
			progress(Event{Analyzer: a.Name(), State: "skipped"})
			continue
		}
		wg.Add(1)
		go func(a Analyzer) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			col := &collector{analyzer: a.Name()}
			progress(Event{Analyzer: a.Name(), State: "start"})
			start := time.Now()
			timeout := opt.Timeout
			if t, ok := a.(Timeouter); ok {
				if d := t.Timeout(p); d > 0 {
					timeout = d + time.Minute // slack so the analyzer's own timeout fires first and is reported precisely
				}
			}
			err := safeAnalyze(ctx, a, p, col, timeout)
			t := Timing{Analyzer: a.Name(), State: "done", DurationMs: time.Since(start).Milliseconds()}
			if err != nil {
				t.State, t.Err = "failed", err.Error()
				col.Emit(model.Evidence{
					Category: model.CatAnalyzer, Kind: "analyzer.failed", Subject: a.Name(),
					Finding: fmt.Sprintf("analyzer %s failed: %v", a.Name(), err),
					Status:  model.Failed, Confidence: model.Observed, Severity: model.SevMedium,
					Limitations: []string{"evidence from this analyzer may be partial or missing"},
				})
			}
			t.Evidence = col.count
			mu.Lock()
			all = append(all, col.items...)
			timings = append(timings, t)
			mu.Unlock()
			progress(Event{Analyzer: a.Name(), State: t.State, Err: t.Err, Elapsed: time.Since(start)})
		}(a)
	}
	wg.Wait()
	SortEvidence(all)
	sort.Slice(timings, func(i, j int) bool { return timings[i].Analyzer < timings[j].Analyzer })
	return Result{Project: p, Evidence: all, Timings: timings}
}

func missingCaps(a Analyzer, allow map[Capability]bool) []Capability {
	var out []Capability
	for _, c := range a.Requires() {
		if !allow[c] {
			out = append(out, c)
		}
	}
	return out
}

func safeAnalyze(ctx context.Context, a Analyzer, p *Project, sink Sink, timeout time.Duration) (err error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	err = a.Analyze(cctx, p, sink)
	if err == nil && cctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

// SortEvidence orders evidence deterministically (ObservedAt is not part of the order).
func SortEvidence(ev []model.Evidence) {
	sort.SliceStable(ev, func(i, j int) bool {
		a, b := ev[i], ev[j]
		if a.Analyzer != b.Analyzer {
			return a.Analyzer < b.Analyzer
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		la, lb := firstLoc(a), firstLoc(b)
		if la != lb {
			return la < lb
		}
		return a.ID < b.ID
	})
}

func firstLoc(e model.Evidence) string {
	if len(e.Locations) == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%09d", e.Locations[0].Path, e.Locations[0].Line)
}

// Registry is the ordered list of all analyzers; packages register in init via cli wiring.
var registry []Analyzer

// Register adds analyzers to the global registry (called from internal/cli wiring).
func Register(a ...Analyzer) { registry = append(registry, a...) }

// Registered returns all registered analyzers sorted by name.
func Registered() []Analyzer {
	out := append([]Analyzer(nil), registry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
