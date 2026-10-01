package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/dyammarcano/anamnesis/internal/config"
	"github.com/dyammarcano/anamnesis/internal/correlate"
	"github.com/dyammarcano/anamnesis/internal/discovery"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/prepare"
	"github.com/dyammarcano/anamnesis/internal/report"
	"github.com/dyammarcano/anamnesis/internal/workspace"
)

// Version is the Anamnesis version.
var Version = report.AppVersion

var registerOnce sync.Once

// Options control one Assess call. The CLI and the TUI build the same struct.
type Options struct {
	Params   *config.Parameters         // required: parameters.yaml is the only configuration source
	Phases   []engine.Phase             // Preflight only, or Preflight+Deep
	Allow    map[engine.Capability]bool // nil: derived from Params (AllowFromParams)
	Only     map[string]bool            // run only these analyzers (rerun of failed ones)
	Progress func(engine.Event)         // optional progress callback
	Args     []string                   // recorded in run.json
	// Prior holds earlier outcomes keyed by discovery.Key(root). With Only set, evidence and timings
	// of analyzers that are not rerun are carried over so the report stays complete.
	Prior map[string]Outcome
}

// Outcome is the result for one project.
type Outcome struct {
	Name            string
	Root            string
	ID              string
	RunID           string
	RunDir          string
	Report          report.Report
	Evidence        []model.Evidence
	Conclusions     []model.Conclusion
	Timings         []engine.Timing
	Deep            bool   // the deep phase was part of this run
	Cancelled       bool   // the context was cancelled before the run finished
	ConsolidatedDir string // set when the call assessed more than one project
	Err             error  // project-level IO/scan failure; a failed analyzer is evidence, not an Err
}

// Failed returns the names of analyzers that failed.
func (o Outcome) Failed() []string { return o.Report.FailedAnalyzers() }

// Conclusion returns the conclusion for a topic, or nil.
func (o Outcome) Conclusion(topic string) *model.Conclusion {
	for i := range o.Conclusions {
		if o.Conclusions[i].Topic == topic {
			return &o.Conclusions[i]
		}
	}
	return nil
}

// AllowFromParams derives the capability map from parameters.yaml.
func AllowFromParams(p *config.Parameters) map[engine.Capability]bool {
	return map[engine.Capability]bool{
		engine.SideEffectBuild: p.Build.Allow,
		engine.SideEffectMTA:   p.MTA.Allow,
		engine.Network:         p.Network.Allow,
	}
}

// EnabledProjects lists the enabled projects' paths in parameters.yaml.
func EnabledProjects(p *config.Parameters) []string {
	var out []string
	for _, pr := range p.Projects {
		if pr.IsEnabled() {
			out = append(out, pr.Path)
		}
	}
	return out
}

// Preflight runs only the Preflight phase (no preparation, nothing repository-controlled runs).
func Preflight(ctx context.Context, paths []string, opts Options) ([]Outcome, error) {
	opts.Phases = []engine.Phase{engine.Preflight}
	return Assess(ctx, paths, opts)
}

// Assess is the single engine path used by the CLI and the TUI. With no paths it assesses the
// enabled projects of parameters.yaml. Reports are written even when analyzers failed or the
// context was cancelled.
func Assess(ctx context.Context, paths []string, opts Options) ([]Outcome, error) {
	if opts.Params == nil {
		return nil, errors.New("no parameters loaded (parameters.yaml is required)")
	}
	registerOnce.Do(registerAll)
	if len(paths) == 0 {
		paths = EnabledProjects(opts.Params)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no projects to assess: add an enabled entry under projects: in %s or pass paths", opts.Params.Path)
	}
	if len(opts.Phases) == 0 {
		opts.Phases = []engine.Phase{engine.Preflight, engine.Deep}
	}

	type target struct {
		root string
		err  error
		raw  string
	}
	var targets []target
	seen := map[string]bool{}
	var roots []string
	for _, p := range paths {
		c, err := discovery.Canonical(p)
		if err != nil {
			targets = append(targets, target{raw: p, err: err})
			continue
		}
		if k := discovery.Key(c); !seen[k] {
			seen[k] = true
			targets = append(targets, target{root: c})
			roots = append(roots, c)
		}
	}
	ws, err := workspace.New(opts.Params.OutputDir, roots)
	if err != nil {
		return nil, err
	}

	analyzers := engine.Registered()
	var outs []Outcome
	for _, t := range targets {
		if t.err != nil {
			outs = append(outs, Outcome{Name: filepath.Base(t.raw), Root: t.raw, Err: t.err})
			continue
		}
		outs = append(outs, assessOne(ctx, ws, t.root, opts, analyzers))
	}

	var withReport []report.Report
	for _, o := range outs {
		if o.RunDir != "" && o.Err == nil {
			withReport = append(withReport, o.Report)
		}
	}
	if len(withReport) > 1 {
		dir := ws.ConsolidatedDir()
		if err := report.WriteConsolidated(dir, withReport); err != nil {
			return outs, fmt.Errorf("write consolidated report: %w", err)
		}
		for i := range outs {
			if outs[i].RunDir != "" {
				outs[i].ConsolidatedDir = dir
			}
		}
	}
	return outs, nil
}

func hasPhase(ps []engine.Phase, want engine.Phase) bool {
	return slices.Contains(ps, want)
}

// tagSnapshot marks prereq-analyzer evidence as belonging to the environment "found" or "prepared".
// Prepared items get a distinct ID, because the analyzer derives IDs from kind and subject alone.
func tagSnapshot(ev []model.Evidence, snapshot string, rehash bool) {
	for i := range ev {
		if ev[i].Analyzer != "prereq" {
			continue
		}
		if ev[i].Values == nil {
			ev[i].Values = map[string]string{}
		}
		ev[i].Values["snapshot"] = snapshot
		if rehash {
			sum := sha256.Sum256([]byte(ev[i].ID + "|" + snapshot))
			ev[i].ID = hex.EncodeToString(sum[:])[:16]
		}
	}
}

func assessOne(ctx context.Context, ws *workspace.Workspace, root string, opts Options, analyzers []engine.Analyzer) Outcome {
	started := time.Now().UTC()
	name, id := filepath.Base(root), discovery.ID(root)
	runDir := ws.ProjectRunDir(name, id)
	out := Outcome{Name: name, Root: root, ID: id, RunID: ws.RunID, RunDir: runDir, Deep: hasPhase(opts.Phases, engine.Deep)}

	allow := opts.Allow
	if allow == nil {
		allow = AllowFromParams(opts.Params)
	}
	prm := *opts.Params // per-project copy: prepare mutates it in memory and must not leak into the saved file
	prm.JDKs = append([]config.JDK(nil), opts.Params.JDKs...)
	eopt := engine.Options{
		Phases:      opts.Phases,
		Allow:       allow,
		Concurrency: prm.Analysis.Concurrency,
		Timeout:     time.Duration(prm.Analysis.AnalyzerTimeoutMinutes) * time.Minute,
		Only:        opts.Only,
		Progress:    opts.Progress,
	}
	progress := func(an, state string) {
		if opts.Progress != nil {
			opts.Progress(engine.Event{Project: name, Analyzer: an, State: state})
		}
	}

	var evidence []model.Evidence
	var timings []engine.Timing
	p, err := engine.NewProject(ctx, root, runDir)
	if err != nil {
		ev := model.Evidence{Analyzer: "scan", Category: model.CatAnalyzer, Kind: model.KindAnalyzerFailed, Subject: "scan",
			Finding: fmt.Sprintf("the project could not be scanned: %v", err), Status: model.Failed, Confidence: model.Observed, Severity: model.SevHigh,
			Limitations: []string{"no analyzer ran; this report contains no project evidence"}}
		ev.Finalize()
		evidence = append(evidence, ev)
		timings = append(timings, engine.Timing{Analyzer: "scan", State: "failed", Err: err.Error()})
	} else {
		p.Params = &prm
		evidence, timings = runAnalyzers(ctx, p, analyzers, eopt, &out, progress)
		if prior, ok := opts.Prior[discovery.Key(root)]; ok && len(opts.Only) > 0 {
			evidence, timings = mergePrior(prior, opts.Only, evidence, timings)
		}
	}
	engine.SortEvidence(evidence)

	out.Cancelled = ctx.Err() != nil
	out.Evidence, out.Timings = evidence, timings
	out.Conclusions = correlate.Correlate(evidence)
	ended := time.Now().UTC()
	meta := report.Meta{Name: name, Root: root, ID: id, RunID: ws.RunID, RunDir: runDir, Args: opts.Args, Started: started, Ended: ended}
	out.Report = report.Build(meta, evidence, out.Conclusions, timings)

	var errs []error
	if err := workspace.WriteEvidence(runDir, evidence); err != nil {
		errs = append(errs, fmt.Errorf("write evidence: %w", err))
	}
	if err := workspace.WriteJSON(runDir, "run.json", newManifest(meta, opts, allow, out.Cancelled, timings)); err != nil {
		errs = append(errs, fmt.Errorf("write run.json: %w", err))
	}
	if err := out.Report.WriteAll(runDir); err != nil {
		errs = append(errs, fmt.Errorf("write report: %w", err))
	}
	out.Err = errors.Join(errs...)
	return out
}

// runAnalyzers runs the phases. A deep run first records the environment as found (Preflight),
// then prepares the machine, then re-probes prerequisites (as prepared) and runs the Deep phase.
func runAnalyzers(ctx context.Context, p *engine.Project, analyzers []engine.Analyzer, eopt engine.Options, out *Outcome, progress func(an, state string)) ([]model.Evidence, []engine.Timing) {
	if !hasPhase(eopt.Phases, engine.Deep) || len(eopt.Only) > 0 {
		r := engine.Run(ctx, p, analyzers, eopt)
		return r.Evidence, r.Timings
	}
	pre := eopt
	pre.Phases = []engine.Phase{engine.Preflight}
	r1 := engine.Run(ctx, p, analyzers, pre)
	tagSnapshot(r1.Evidence, "found", false)
	evidence := append([]model.Evidence(nil), r1.Evidence...)
	timings := append([]engine.Timing(nil), r1.Timings...)
	if ctx.Err() != nil {
		return evidence, timings
	}

	progress("prepare", "start")
	t0 := time.Now()
	prep := prepare.Run(ctx, p.Params, p.RunDir, r1.Evidence)
	evidence = append(evidence, prep...)
	timings = append(timings, engine.Timing{Analyzer: "prepare", State: "done", DurationMs: time.Since(t0).Milliseconds(), Evidence: len(prep)})
	progress("prepare", "done")
	if ctx.Err() != nil {
		return evidence, timings
	}

	re := eopt
	re.Phases = []engine.Phase{engine.Preflight, engine.Deep}
	re.Only = map[string]bool{"prereq": true}
	r2 := engine.Run(ctx, p, analyzers, re)
	tagSnapshot(r2.Evidence, "prepared", true)
	evidence = append(evidence, r2.Evidence...)
	for _, t := range r2.Timings {
		t.Analyzer += " (prepared)"
		timings = append(timings, t)
	}
	if ctx.Err() != nil {
		return evidence, timings
	}

	deep := eopt
	deep.Phases = []engine.Phase{engine.Deep}
	r3 := engine.Run(ctx, p, analyzers, deep)
	evidence = append(evidence, r3.Evidence...)
	timings = append(timings, r3.Timings...)
	return evidence, timings
}

func mergePrior(prior Outcome, only map[string]bool, ev []model.Evidence, tm []engine.Timing) ([]model.Evidence, []engine.Timing) {
	var mergedEv []model.Evidence
	for _, e := range prior.Evidence {
		if !only[e.Analyzer] {
			mergedEv = append(mergedEv, e)
		}
	}
	var mergedTm []engine.Timing
	for _, t := range prior.Timings {
		if !only[t.Analyzer] {
			mergedTm = append(mergedTm, t)
		}
	}
	sort.SliceStable(tm, func(i, j int) bool { return tm[i].Analyzer < tm[j].Analyzer })
	return append(mergedEv, ev...), append(mergedTm, tm...)
}

type manifestProject struct {
	Name string `json:"name"`
	Root string `json:"root"`
	ID   string `json:"id"`
}

type runManifest struct {
	AppVersion string          `json:"anamnesis_version"`
	RunID      string          `json:"run_id"`
	Args       []string        `json:"args"`
	Parameters string          `json:"parameters_file"`
	Started    time.Time       `json:"started"`
	Ended      time.Time       `json:"ended"`
	Project    manifestProject `json:"project"`
	Phases     []string        `json:"phases"`
	Allowed    []string        `json:"capabilities_allowed"`
	Only       []string        `json:"only,omitempty"`
	Cancelled  bool            `json:"cancelled"`
	Timings    []engine.Timing `json:"timings"`
}

func newManifest(meta report.Meta, opts Options, allow map[engine.Capability]bool, cancelled bool, timings []engine.Timing) runManifest {
	m := runManifest{AppVersion: Version, RunID: meta.RunID, Args: meta.Args, Started: meta.Started, Ended: meta.Ended,
		Project: manifestProject{meta.Name, meta.Root, meta.ID}, Cancelled: cancelled, Timings: timings}
	if opts.Params != nil {
		m.Parameters = opts.Params.Path
	}
	for _, ph := range opts.Phases {
		if ph == engine.Deep {
			m.Phases = append(m.Phases, "deep")
		} else {
			m.Phases = append(m.Phases, "preflight")
		}
	}
	for c, ok := range allow {
		if ok {
			m.Allowed = append(m.Allowed, string(c))
		}
	}
	sort.Strings(m.Allowed)
	for n := range opts.Only {
		m.Only = append(m.Only, n)
	}
	sort.Strings(m.Only)
	return m
}

// ExportConsolidated writes a consolidated report for already-assessed projects into a new
// consolidated run directory under parameters.yaml's output_dir and returns that directory.
func ExportConsolidated(params *config.Parameters, outs []Outcome) (string, error) {
	var reports []report.Report
	var roots []string
	for _, o := range outs {
		if o.RunDir != "" && o.Err == nil {
			reports = append(reports, o.Report)
			roots = append(roots, o.Root)
		}
	}
	if len(reports) == 0 {
		return "", errors.New("no assessed projects to consolidate")
	}
	ws, err := workspace.New(params.OutputDir, roots)
	if err != nil {
		return "", err
	}
	dir := ws.ConsolidatedDir()
	return dir, report.WriteConsolidated(dir, reports)
}
