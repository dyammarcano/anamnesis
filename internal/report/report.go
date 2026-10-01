// Package report renders correlated conclusions and evidence into exactly two reports per project —
// report.json (canonical, for machines) and a single self-contained report.html (for people) — plus a
// consolidated cross-project view in the same two formats.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"anamnesis/internal/correlate"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
)

const (
	// AppVersion is the Anamnesis version stamped into every report.
	AppVersion = "0.1.0-dev"
	// SchemaVersion versions the report.json layout.
	SchemaVersion = "1"
)

// Meta is what the caller knows about one run of one project.
type Meta struct {
	Name    string
	Root    string
	ID      string
	RunID   string
	RunDir  string
	Args    []string
	Started time.Time
	Ended   time.Time
}

// ProjectInfo identifies the analyzed project.
type ProjectInfo struct {
	Name    string            `json:"name"`
	Root    string            `json:"root"`
	ID      string            `json:"id"`
	RunDir  string            `json:"run_dir"`
	Summary map[string]string `json:"summary,omitempty"` // identity.summary and git.repository values
}

// Estimates is reserved for calibrated engineering-hour estimates. Calibration stays null until a
// measured hours-per-effort-point exists.
type Estimates struct {
	Calibration any    `json:"calibration"`
	Note        string `json:"note"`
}

// Report is the canonical report document.
type Report struct {
	SchemaVersion string                      `json:"schema_version"`
	AppVersion    string                      `json:"anamnesis_version"`
	RunID         string                      `json:"run_id"`
	Project       ProjectInfo                 `json:"project"`
	Conclusions   []model.Conclusion          `json:"conclusions"`
	Evidence      map[string][]model.Evidence `json:"evidence"` // grouped by category
	Timings       []engine.Timing             `json:"timings"`
	MTAEffort     map[string]any              `json:"mta_effort"`
	Estimates     Estimates                   `json:"estimates"`
}

// Build assembles the report from evidence and its conclusions.
func Build(meta Meta, ev []model.Evidence, concl []model.Conclusion, timings []engine.Timing) Report {
	grouped := map[string][]model.Evidence{}
	summary := map[string]string{}
	for _, e := range ev {
		grouped[string(e.Category)] = append(grouped[string(e.Category)], e)
		switch e.Kind {
		case model.KindIdentity:
			maps.Copy(summary, e.Values)
		case model.KindGitRepository:
			for k, v := range e.Values {
				summary["git_"+k] = v
			}
		}
	}
	if len(summary) == 0 {
		summary = nil
	}
	if concl == nil {
		concl = []model.Conclusion{}
	}
	if timings == nil {
		timings = []engine.Timing{}
	}
	es := correlate.MTAEffort(ev)
	return Report{
		SchemaVersion: SchemaVersion,
		AppVersion:    AppVersion,
		RunID:         meta.RunID,
		Project:       ProjectInfo{Name: meta.Name, Root: meta.Root, ID: meta.ID, RunDir: meta.RunDir, Summary: summary},
		Conclusions:   concl,
		Evidence:      grouped,
		Timings:       timings,
		MTAEffort: map[string]any{
			"total_points": es.TotalPoints, "rules": es.Rules, "incidents": es.Incidents,
			"by_rule": es.ByRule, "by_target": es.ByTarget, "by_category": es.ByCategory, "by_label": es.ByLabel,
			"formula": es.Formula, "unit": es.Unit,
		},
		Estimates: Estimates{
			Calibration: nil,
			Note:        "Engineering hours are UNKNOWN. MTA effort points are not converted to hours until a measured calibration (hours per point, its source and sample size) exists.",
		},
	}
}

// Conclusion returns the conclusion for topic, or nil.
func (r *Report) Conclusion(topic string) *model.Conclusion {
	for i := range r.Conclusions {
		if r.Conclusions[i].Topic == topic {
			return &r.Conclusions[i]
		}
	}
	return nil
}

// AllEvidence returns every evidence item, flattened in category order.
func (r *Report) AllEvidence() []model.Evidence {
	cats := make([]string, 0, len(r.Evidence))
	for c := range r.Evidence {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var out []model.Evidence
	for _, c := range cats {
		out = append(out, r.Evidence[c]...)
	}
	return out
}

// FailedAnalyzers lists analyzer names whose evidence is analyzer.failed.
func (r *Report) FailedAnalyzers() []string {
	var out []string
	for _, e := range r.AllEvidence() {
		if e.Kind == model.KindAnalyzerFailed {
			out = append(out, e.Subject)
		}
	}
	sort.Strings(out)
	return out
}

// WriteJSON writes dir/report.json.
func (r *Report) WriteJSON(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.json"), buf.Bytes(), 0o644)
}

// WriteAll writes report.json and report.html into dir, returning the first error but attempting
// both.
func (r *Report) WriteAll(dir string) error {
	var first error
	for _, f := range []func(string) error{r.WriteJSON, r.WriteHTML} {
		if err := f(dir); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---- shared view model ----------------------------------------------------------------------------

type cmdRow struct {
	Argv       string
	Dir        string
	Exit       int
	DurationMs int64
	TimedOut   bool
	LogRef     string
	Err        string
}

type evRow struct {
	ID         string
	Kind       string
	Subject    string
	Finding    string
	Status     string
	Confidence string
	Severity   string
	Locs       []string
	Cmd        *cmdRow
	Artifacts  []string
	Limits     []string
	Failed     bool
}

type concView struct {
	model.Conclusion
	Because     []evRow
	BecauseMore int      // supporting evidence beyond becauseCap; all of it is in report.json / evidence.jsonl
	Missing     []string // evidence IDs not present in the report
}

type kv struct {
	K string
	V int
}

type secView struct {
	No          int
	Anchor      string
	Title       string
	Brief       bool
	Conclusions []concView
	Rows        []evRow
	More        int // rows omitted from the listing
	Lines       []string
	Effort      []kv
	Timings     []engine.Timing
	Open        bool
	Empty       string
}

type sectionSpec struct {
	Title   string
	Topics  []string
	Kinds   []string
	Special string
	Brief   bool
}

var layout = []sectionSpec{
	{Title: "Executive technical summary", Brief: true, Topics: []string{"project.identity", "build.systems", "java.generation", "buildability.state", "environment.missing", "migration.mta", "migration.effort", "analyzers.failed"}},
	{Title: "Project identity and source inventory", Topics: []string{"project.identity"}, Kinds: []string{model.KindIdentity, model.KindGitRepository}},
	{Title: "Build systems", Topics: []string{"build.systems"}, Kinds: []string{model.KindBuildSystem, model.KindBuildSystemAbsent, model.KindAntProject, model.KindAntImportUnresolved, model.KindAntPackaging, model.KindAntEnvRef}},
	{Title: "Java version evidence (declared, binary, inferred)", Topics: []string{"java.declared", "java.binaries", "java.generation"}, Kinds: []string{model.KindJavaSource, model.KindJavaTarget, model.KindJavaRelease, model.KindJavaImplicit, model.KindJavaClassMajor, model.KindJavaManifestJDK, model.KindJavaSummary}},
	{Title: "Local environment (this machine, not the project)", Topics: []string{"java.local-runtime"}, Kinds: []string{model.KindJavaLocalRuntime}},
	{Title: "Buildability (source axis and local-reproducibility axis)", Topics: []string{"buildability.source", "buildability.local-reproducibility", "buildability.state"}, Kinds: []string{model.KindBuildLocal, model.KindAntBuildCandidate}},
	{Title: "Prerequisites and environment gaps", Topics: []string{"environment.missing"}, Kinds: []string{model.KindPrereqTool}},
	{Title: "Environment: as found vs as prepared", Topics: []string{"environment.prepared"}, Kinds: []string{model.KindEnvInstall, model.KindEnvJavaHome, model.KindEnvBuildJDK}, Special: "environment"},
	{Title: "Build safety", Topics: []string{"safety"}, Kinds: []string{model.KindAntTargetSafety}},
	{Title: "Enterprise technologies", Topics: []string{"enterprise", "technologies"}, Kinds: []string{model.KindEnterpriseDescriptor, model.KindEnterpriseAppServer, model.KindEnterpriseJavaEE, model.KindTechUsage}},
	{Title: "Dependencies and bundled binaries", Topics: []string{"dependencies"}, Kinds: []string{model.KindDepsInventory, model.KindDepsDuplicate, model.KindDepsOld}},
	{Title: "CI evidence", Topics: []string{"ci"}, Kinds: []string{model.KindCIWorkflow, model.KindCISameSHA}},
	{Title: "Migration assessment (MTA)", Topics: []string{"migration.mta"}, Kinds: []string{model.KindMTAPrediction, model.KindMTAPrereq, model.KindMTARun, model.KindMTACoverage, model.KindMTARuleError, model.KindMTAInsight}},
	{Title: "Migration effort (MTA effort points)", Topics: []string{"migration.effort", "migration.hours"}, Kinds: []string{model.KindMTAViolation}, Special: "effort"},
	{Title: "JDK tooling (jdeps, jdeprscan)", Kinds: []string{model.KindJdeps, model.KindJdeprscan}},
	{Title: "Estimates and calibration", Special: "estimates"},
	{Title: "Unknowns", Topics: []string{"unknowns"}},
	{Title: "Analyzers that failed or were skipped", Topics: []string{"analyzers.failed"}, Kinds: []string{model.KindAnalyzerFailed, model.KindAnalyzerSkipped}, Special: "failed"},
	{Title: "Timings", Special: "timings"},
	{Title: "Assumptions and limitations", Special: "assumptions"},
	{Title: "Raw artifacts and log references", Special: "artifacts"},
}

// failedSection returns the number of the "analyzers that failed" section.
func failedSection() int {
	for i, sp := range layout {
		if sp.Special == "failed" {
			return i + 1
		}
	}
	return 0
}

const listCap = 60 // evidence rows listed per section before pointing at report.json

// becauseCap bounds the supporting evidence printed under one conclusion (most severe first); a
// conclusion can rest on thousands of items (e.g. every Ant target) and the reader needs the worst.
const becauseCap = 25

var severityRank = map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3, "INFO": 4, "": 5}

func locStr(l model.Location) string {
	if l.Line > 0 {
		return l.Path + ":" + strconv.Itoa(l.Line)
	}
	return l.Path
}

func toRow(e model.Evidence) evRow {
	row := evRow{ID: e.ID, Kind: e.Kind, Subject: e.Subject, Finding: e.Finding, Status: string(e.Status),
		Confidence: string(e.Confidence), Severity: string(e.Severity), Artifacts: e.Artifacts, Limits: e.Limitations,
		Failed: e.Status == model.Failed || e.Status == model.Fail}
	for _, l := range e.Locations {
		row.Locs = append(row.Locs, locStr(l))
	}
	if c := e.Command; c != nil {
		row.Cmd = &cmdRow{Argv: strings.Join(c.Argv, " "), Dir: c.Dir, Exit: c.ExitCode, DurationMs: c.DurationMs, TimedOut: c.TimedOut, LogRef: c.LogRef, Err: c.Err}
	}
	return row
}

func (r *Report) view() (secs []secView, failed int, byTopic map[string]concView) {
	all := r.AllEvidence()
	byID := map[string]model.Evidence{}
	byKind := map[string][]model.Evidence{}
	for _, e := range all {
		byID[e.ID] = e
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	byTopic = map[string]concView{}
	conc := func(c model.Conclusion) concView {
		cv := concView{Conclusion: c}
		for _, id := range c.EvidenceIDs {
			if e, ok := byID[id]; ok {
				cv.Because = append(cv.Because, toRow(e))
			} else {
				cv.Missing = append(cv.Missing, id)
			}
		}
		if len(cv.Because) > becauseCap {
			sort.SliceStable(cv.Because, func(i, j int) bool {
				a, b := cv.Because[i], cv.Because[j]
				if a.Failed != b.Failed {
					return a.Failed
				}
				return severityRank[a.Severity] < severityRank[b.Severity]
			})
			cv.BecauseMore = len(cv.Because) - becauseCap
			cv.Because = cv.Because[:becauseCap]
		}
		return cv
	}
	for _, c := range r.Conclusions {
		byTopic[c.Topic] = conc(c)
	}
	failed = len(byKind[model.KindAnalyzerFailed])

	for i, sp := range layout {
		s := secView{No: i + 1, Anchor: "s" + strconv.Itoa(i+1), Title: sp.Title, Brief: sp.Brief}
		for _, t := range sp.Topics {
			if cv, ok := byTopic[t]; ok {
				s.Conclusions = append(s.Conclusions, cv)
			}
		}
		if !sp.Brief {
			for _, k := range sp.Kinds {
				for _, e := range byKind[k] {
					if sp.Special == "effort" && k == model.KindMTAViolation {
						continue // the effort section shows the aggregation, not every violation
					}
					if len(s.Rows) >= listCap {
						s.More++
						continue
					}
					s.Rows = append(s.Rows, toRow(e))
				}
			}
		}
		switch sp.Special {
		case "effort":
			es := correlate.MTAEffort(all)
			s.Lines = append(s.Lines, "Formula: "+es.Formula, "Unit: "+es.Unit,
				fmt.Sprintf("Total: %d points over %d rules (%d incidents)", es.TotalPoints, es.Rules, es.Incidents))
			var rules []kv
			for k, v := range es.ByRule {
				rules = append(rules, kv{k, v})
			}
			sort.Slice(rules, func(a, b int) bool {
				if rules[a].V != rules[b].V {
					return rules[a].V > rules[b].V
				}
				return rules[a].K < rules[b].K
			})
			if len(rules) > 40 {
				s.Lines = append(s.Lines, fmt.Sprintf("Showing the top 40 of %d rules by points; all violations are in report.json.", len(rules)))
				rules = rules[:40]
			}
			s.Effort = rules
		case "environment":
			found, prep := map[string]string{}, map[string]string{}
			for _, e := range byKind[model.KindPrereqTool] {
				switch e.Values["snapshot"] {
				case "found":
					found[e.Subject] = e.Values["state"]
				case "prepared":
					prep[e.Subject] = e.Values["state"]
				}
			}
			names := make([]string, 0, len(found))
			for t := range found {
				names = append(names, t)
			}
			sort.Strings(names)
			for _, t := range names {
				p := prep[t]
				if p == "" {
					p = "(not re-probed)"
				}
				s.Lines = append(s.Lines, fmt.Sprintf("tool %s: as found %s, as prepared %s", t, found[t], p))
			}
			if len(names) == 0 {
				s.Lines = append(s.Lines, "No found/prepared prerequisite snapshots were recorded: the environment is reported as found (preflight only, or preparation did not run).")
			}
			for _, e := range byKind[model.KindEnvJavaHome] {
				s.Lines = append(s.Lines, fmt.Sprintf("JAVA_HOME before: %q, after: %q", e.Values["user_before"], e.Values["user_after"]))
			}
		case "estimates":
			s.Lines = []string{"calibration: null (no measured hours-per-point exists)", r.Estimates.Note}
		case "failed":
			s.Open = true
			if len(s.Rows) == 0 {
				s.Empty = "No analyzer failed or was skipped in this run."
			}
		case "timings":
			s.Timings = r.Timings
			if len(r.Timings) == 0 {
				s.Empty = "No analyzer timings were recorded."
			}
		case "assumptions":
			seen := map[string]bool{}
			for _, c := range r.Conclusions {
				for _, a := range c.Assumptions {
					if l := "Assumption (" + c.Topic + "): " + a; !seen[l] {
						seen[l] = true
						s.Lines = append(s.Lines, l)
					}
				}
				for _, a := range c.Limitations {
					if l := "Limitation (" + c.Topic + "): " + a; !seen[l] {
						seen[l] = true
						s.Lines = append(s.Lines, l)
					}
				}
			}
			if len(s.Lines) == 0 {
				s.Empty = "No assumptions or limitations were recorded."
			}
		case "artifacts":
			s.Lines = append(s.Lines, "evidence.jsonl: every evidence item, one per line", "run.json: run manifest (arguments, timings)",
				"capabilities/<analyzer>/: logs and artifacts written by analyzers")
			for _, e := range all {
				if e.Command != nil && e.Command.LogRef != "" {
					s.Lines = append(s.Lines, fmt.Sprintf("log: %s (%s %s, exit %d)", e.Command.LogRef, e.Kind, e.Subject, e.Command.ExitCode))
				}
				for _, a := range e.Artifacts {
					s.Lines = append(s.Lines, fmt.Sprintf("artifact: %s (%s %s)", a, e.Kind, e.Subject))
				}
			}
		}
		secs = append(secs, s)
	}
	return secs, failed, byTopic
}
