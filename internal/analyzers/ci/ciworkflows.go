package ci

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/redact"
)

// Analyzers returns ciworkflows and cisha.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{workflowsAnalyzer{}, shaAnalyzer{}} }

type workflowsAnalyzer struct{}

func (workflowsAnalyzer) Name() string                  { return "ciworkflows" }
func (workflowsAnalyzer) Phase() engine.Phase           { return engine.Preflight }
func (workflowsAnalyzer) Requires() []engine.Capability { return nil }

const maxLocations = 50

func (workflowsAnalyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	wfs := LoadWorkflows(p)
	if len(wfs) == 0 {
		sink.Emit(model.Evidence{
			Category: model.CatCI, Kind: model.KindCIWorkflow, Subject: ".github/workflows",
			Finding: "No GitHub Actions workflow files (.github/workflows/*.yml|yaml) were found.",
			Status:  model.Info, Confidence: model.Observed,
			Values:      map[string]string{"workflows": "0"},
			Limitations: []string{"Other CI systems (Jenkins, GitLab CI, Azure Pipelines, ...) are not examined."},
		})
		return nil
	}
	for _, w := range wfs {
		if err := ctx.Err(); err != nil {
			return err
		}
		sink.Emit(workflowEvidence(w))
	}
	return nil
}

func workflowEvidence(w *Workflow) model.Evidence {
	ev := model.Evidence{
		Category: model.CatCI, Kind: model.KindCIWorkflow, Subject: w.File,
		Locations: []model.Location{{Path: w.File, Line: 1}},
	}
	if w.ParseErr != "" {
		ev.Status, ev.Confidence, ev.Severity = model.Warn, model.Observed, model.SevLow
		ev.Finding = fmt.Sprintf("Workflow %s could not be parsed: %s. It is excluded from CI conclusions.", w.File, redact.Redact(oneLine(w.ParseErr)))
		ev.Values = map[string]string{"purpose": "UNKNOWN", "parse_error": "true"}
		ev.Limitations = []string{"A workflow that does not parse cannot be classified; a CI run of it, if any, will not be counted as a build."}
		return ev
	}

	ev.Status = model.Info
	ev.Confidence = model.Inferred // the purpose is an inference; the listed facts are read from the file
	ev.Finding = fmt.Sprintf("Workflow %s (%d job(s); triggers: %s) is classified %s by its steps.", w.File, len(w.Jobs), joinOr(w.Triggers, "none found"), w.Purpose)
	if w.Publishes {
		ev.Finding += " It also publishes or deploys."
	}
	if len(w.JavaVersions) > 0 {
		ev.Finding += " setup-java: " + strings.Join(w.JavaVersions, ", ") + "."
	}
	ev.Assumptions = []string{"Purpose is inferred from the commands and actions in the steps, not from what a run did."}
	ev.Values = map[string]string{
		"purpose":           w.Purpose,
		"purpose_basis":     "INFERRED from step commands",
		"workflow_name":     w.RunName(),
		"triggers":          strings.Join(w.Triggers, ","),
		"jobs":              strings.Join(w.Jobs, ","),
		"java_versions":     strings.Join(w.JavaVersions, ","),
		"java_distribution": strings.Join(w.JavaDists, ","),
		"setup_actions":     strings.Join(w.SetupActions, ","),
		"tools":             strings.Join(w.Tools, ","),
		"upload_artifact":   strconv.FormatBool(w.Uploads),
		"cache":             strconv.FormatBool(w.Caches),
		"services":          strings.Join(w.Services, ","),
		"env_names":         strings.Join(w.EnvNames, ","),
		"secret_names":      strings.Join(w.SecretNames, ","),
		"publishes":         strconv.FormatBool(w.Publishes),
	}
	for k, v := range ev.Values {
		if v == "" {
			delete(ev.Values, k)
		}
	}
	ev.Locations = locations(w)
	ev.Limitations = []string{"Environment variable and secret NAMES only; values are never read or stored."}
	if len(w.JavaVersions) > 0 && anyPrefix(w.JavaVersions, "file:") {
		ev.Limitations = append(ev.Limitations, "A java-version-file reference was not resolved.")
	}
	return ev
}

func locations(w *Workflow) []model.Location {
	labels := make([]string, 0, len(w.Lines))
	for k := range w.Lines {
		labels = append(labels, k)
	}
	sort.Slice(labels, func(i, j int) bool {
		if w.Lines[labels[i]] != w.Lines[labels[j]] {
			return w.Lines[labels[i]] < w.Lines[labels[j]]
		}
		return labels[i] < labels[j]
	})
	locs := []model.Location{{Path: w.File, Line: 1}}
	seen := map[int]bool{1: true}
	for _, l := range labels {
		line := w.Lines[l]
		if seen[line] || len(locs) >= maxLocations {
			continue
		}
		seen[line] = true
		locs = append(locs, model.Location{Path: w.File, Line: line})
	}
	return locs
}

func joinOr(s []string, empty string) string {
	if len(s) == 0 {
		return empty
	}
	return strings.Join(s, ", ")
}

func anyPrefix(s []string, prefix string) bool {
	for _, x := range s {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
