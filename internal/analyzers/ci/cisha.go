package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dyammarcano/anamnesis/internal/discovery"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/proc"
	"github.com/dyammarcano/anamnesis/internal/redact"
)

// States of KindCISameSHA.
const (
	StateSuccessSameSHA   = "SUCCESS_SAME_SHA"
	StateFailedSameSHA    = "FAILED_SAME_SHA"
	StateNoRunSameSHA     = "NO_RUN_SAME_SHA"
	StateSuccessOtherOnly = "SUCCESS_OTHER_SHA_ONLY"
	StateUnavailable      = "UNAVAILABLE"
)

type shaAnalyzer struct{}

func (shaAnalyzer) Name() string                  { return "cisha" }
func (shaAnalyzer) Phase() engine.Phase           { return engine.Deep }
func (shaAnalyzer) Requires() []engine.Capability { return []engine.Capability{engine.Network} }

type ghRun struct {
	DatabaseID   int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	Conclusion   string `json:"conclusion"`
	Status       string `json:"status"`
	HeadSHA      string `json:"headSha"`
	URL          string `json:"url"`
}

const ghFields = "databaseId,workflowName,conclusion,status,headSha,url"

func (shaAnalyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	if len(p.Files.Under(".github/workflows")) == 0 {
		return nil // nothing to correlate; ciworkflows reports the absence
	}
	if err := os.MkdirAll(p.RunDir, 0o755); err != nil {
		return err
	}
	s := &sha{p: p, sink: sink, logDir: p.CapabilityDir("cisha")}
	s.run(ctx)
	return nil
}

type sha struct {
	p      *engine.Project
	sink   engine.Sink
	logDir string
}

func (s *sha) unavailable(subject, reason string, cmd *model.CommandRecord, extra map[string]string) {
	vals := map[string]string{"state": StateUnavailable, "reason": reason}
	maps.Copy(vals, extra)
	s.sink.Emit(model.Evidence{
		Category: model.CatCI, Kind: model.KindCISameSHA, Subject: subject,
		Finding: "CI result for this SHA is UNAVAILABLE: " + reason + ". Nothing is concluded about source buildability in CI; local reproducibility is a separate question.",
		Status:  model.Info, Confidence: model.Unknown, Values: vals, Command: cmd,
		Limitations: []string{"No CI evidence was obtained, so the absence of a result is not a failed build."},
	})
}

func (s *sha) tool(name string) (path, from string, ok bool) {
	if s.p.Params != nil {
		if cfg := s.p.Params.Tool(name); cfg != "" {
			if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
				return cfg, "parameters.yaml tools." + name, true
			}
			return "", "parameters.yaml tools." + name + " is set but " + cfg + " does not exist", false
		}
	}
	if lp, err := exec.LookPath(name); err == nil {
		return lp, "PATH (global tool)", true
	}
	return "", name + " is not configured and not on PATH", false
}

func (s *sha) exec(ctx context.Context, tool string, args []string, stem string) proc.Result {
	r, _ := proc.Run(ctx, proc.Spec{
		Name: tool, Args: args, Dir: s.p.RunDir, Timeout: 90 * time.Second,
		LogDir: s.logDir, LogName: stem, RunDir: s.p.RunDir,
	})
	return r
}

func (s *sha) run(ctx context.Context) {
	subject := "HEAD"

	gitPath, gitFrom, ok := s.tool("git")
	if !ok {
		s.unavailable(subject, "git unavailable ("+gitFrom+")", nil, nil)
		return
	}
	top := s.exec(ctx, gitPath, []string{"--no-optional-locks", "-C", s.p.Root, "rev-parse", "--show-toplevel"}, "git-toplevel")
	if top.Record.ExitCode != 0 {
		s.unavailable(subject, "the project is not a readable git repository ("+redact.Redact(oneLine(firstNonEmpty(top.Stderr, top.Record.Err)))+")", &top.Record, nil)
		return
	}
	if !sameDir(strings.TrimSpace(top.Stdout), s.p.Root) {
		s.unavailable(subject, "the project is enclosed by another git repository at "+strings.TrimSpace(top.Stdout)+"; its own CI history is not identifiable", &top.Record, nil)
		return
	}
	head := s.exec(ctx, gitPath, []string{"--no-optional-locks", "-C", s.p.Root, "rev-parse", "HEAD"}, "git-head")
	sha := strings.TrimSpace(head.Stdout)
	if head.Record.ExitCode != 0 || !regexp.MustCompile(`^[0-9a-f]{40,64}$`).MatchString(sha) {
		s.unavailable(subject, "HEAD could not be resolved (no commits?)", &head.Record, nil)
		return
	}
	short := sha[:7]
	subject = "HEAD@" + short

	rem := s.exec(ctx, gitPath, []string{"--no-optional-locks", "-C", s.p.Root, "remote", "get-url", "origin"}, "git-origin")
	if rem.Record.ExitCode != 0 {
		s.unavailable(subject, "no remote named origin", &rem.Record, map[string]string{"sha": sha})
		return
	}
	owner, repo, ok := parseGitHubRemote(strings.TrimSpace(rem.Stdout))
	if !ok {
		s.unavailable(subject, "origin is not a github.com remote", &rem.Record, map[string]string{"sha": sha})
		return
	}
	slug := owner + "/" + repo
	subject = slug + "@" + short

	ghPath, ghFrom, ok := s.tool("gh")
	if !ok {
		s.unavailable(subject, "gh unavailable ("+ghFrom+")", nil, map[string]string{"sha": sha, "repo": slug})
		return
	}
	auth := s.exec(ctx, ghPath, []string{"auth", "status"}, "gh-auth")
	if auth.Record.ExitCode != 0 {
		s.unavailable(subject, "gh is not authenticated (gh auth status exited "+strconv.Itoa(auth.Record.ExitCode)+")", &auth.Record, map[string]string{"sha": sha, "repo": slug, "gh_resolved_from": ghFrom})
		return
	}

	wfs := LoadWorkflows(s.p)
	byRun := map[string]*Workflow{}
	for _, w := range wfs {
		if w.ParseErr == "" {
			byRun[w.RunName()] = w
			byRun[w.File] = w
		}
	}
	unparsed := 0
	for _, w := range wfs {
		if w.ParseErr != "" {
			unparsed++
		}
	}

	same, rec, err := s.list(ctx, ghPath, slug, sha, 50, "gh-run-list-sha")
	if err != nil {
		s.unavailable(subject, "gh run list failed: "+redact.Redact(oneLine(err.Error())), rec, map[string]string{"sha": sha, "repo": slug})
		return
	}

	st, pick, notes := decide(same, byRun)
	var other []ghRun
	if st == StateNoRunSameSHA {
		var rec2 *model.CommandRecord
		other, rec2, err = s.list(ctx, ghPath, slug, "", 20, "gh-run-list-other")
		if err == nil {
			if o, found := successOnBuild(other, byRun, sha); found {
				st, pick = StateSuccessOtherOnly, o
			}
			rec = rec2
		} else {
			notes = append(notes, "Other-SHA context could not be read: "+redact.Redact(oneLine(err.Error())))
		}
	}
	s.emit(subject, sha, slug, st, pick, notes, unparsed, rec, ghFrom)
}

func (s *sha) list(ctx context.Context, gh, slug, sha string, limit int, stem string) ([]ghRun, *model.CommandRecord, error) {
	args := []string{"run", "list", "--repo", slug}
	if sha != "" {
		args = append(args, "--commit", sha)
	}
	args = append(args, "--json", ghFields, "--limit", strconv.Itoa(limit))
	r := s.exec(ctx, gh, args, stem)
	rec := r.Record
	if rec.ExitCode != 0 {
		return nil, &rec, fmt.Errorf("exit %d: %s", rec.ExitCode, firstNonEmpty(r.Stderr, rec.Err))
	}
	var runs []ghRun
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.Stdout)), &runs); err != nil {
		return nil, &rec, fmt.Errorf("unreadable JSON: %v", err)
	}
	return runs, &rec, nil
}

// decide applies the same-SHA rules using only runs of workflows classified BUILD. Lint-only,
// test-only or unclassified workflows never count.
func decide(runs []ghRun, byRun map[string]*Workflow) (state string, pick *ghRun, notes []string) {
	var buildRuns []ghRun
	ignored := map[string]bool{}
	for _, r := range runs {
		if w := byRun[r.WorkflowName]; w != nil && w.Purpose == PurposeBuild {
			buildRuns = append(buildRuns, r)
		} else {
			ignored[r.WorkflowName] = true
		}
	}
	if len(ignored) > 0 {
		var n []string
		for k := range ignored {
			n = append(n, k)
		}
		notes = append(notes, "Runs not counted (workflow not classified BUILD, or unknown): "+strings.Join(n, ", "))
	}
	var failed *ghRun
	pending := 0
	for i := range buildRuns {
		r := buildRuns[i]
		switch r.Conclusion {
		case "success":
			rr := r
			return StateSuccessSameSHA, &rr, notes
		case "failure", "timed_out", "startup_failure":
			if failed == nil {
				rr := r
				failed = &rr
			}
		default:
			pending++
		}
	}
	if failed != nil {
		return StateFailedSameSHA, failed, notes
	}
	if pending > 0 {
		notes = append(notes, fmt.Sprintf("%d same-SHA BUILD run(s) exist but none concluded success or failure (in progress, cancelled or skipped).", pending))
	}
	return StateNoRunSameSHA, nil, notes
}

func successOnBuild(runs []ghRun, byRun map[string]*Workflow, notSHA string) (*ghRun, bool) {
	for i := range runs {
		r := runs[i]
		if r.HeadSHA == notSHA {
			continue
		}
		if w := byRun[r.WorkflowName]; w != nil && w.Purpose == PurposeBuild && r.Conclusion == "success" {
			return &r, true
		}
	}
	return nil, false
}

func (s *sha) emit(subject, sha, slug, state string, pick *ghRun, notes []string, unparsed int, rec *model.CommandRecord, ghFrom string) {
	vals := map[string]string{"state": state, "sha": sha, "repo": slug, "gh_resolved_from": ghFrom}
	ev := model.Evidence{
		Category: model.CatCI, Kind: model.KindCISameSHA, Subject: subject,
		Status: model.Info, Confidence: model.Inferred, Command: rec, Values: vals,
		Assumptions: []string{"BUILD workflows are identified by inferring purpose from workflow steps in the working tree, which may differ from the files at that SHA."},
	}
	const sep = " This concerns source buildability in CI for this SHA only; it says nothing about whether the build reproduces locally."
	switch state {
	case StateSuccessSameSHA:
		vals["run_url"], vals["workflow"] = pick.URL, pick.WorkflowName
		ev.Status = model.Pass
		ev.Finding = fmt.Sprintf("A CI run of build workflow %q concluded success for HEAD %s (%s).", pick.WorkflowName, sha[:7], pick.URL) + sep
	case StateFailedSameSHA:
		vals["run_url"], vals["workflow"] = pick.URL, pick.WorkflowName
		ev.Status, ev.Severity = model.Fail, model.SevMedium
		ev.Finding = fmt.Sprintf("Build workflow %q concluded %s for HEAD %s, with no successful build run for that SHA (%s). A failed CI run is not by itself a source defect; check the log.", pick.WorkflowName, pick.Conclusion, sha[:7], pick.URL) + sep
	case StateSuccessOtherOnly:
		vals["run_url"], vals["workflow"], vals["other_sha"] = pick.URL, pick.WorkflowName, pick.HeadSHA
		ev.Status = model.Warn
		ev.Finding = fmt.Sprintf("No conclusive build run exists for HEAD %s; build workflow %q succeeded only for another SHA (%s, %s). That does not cover the current commit.", sha[:7], pick.WorkflowName, shortSHA(pick.HeadSHA), pick.URL) + sep
	default:
		ev.Status = model.Warn
		ev.Finding = fmt.Sprintf("No conclusive CI build run was found for HEAD %s (no run, or only lint/test/other workflows).", sha[:7]) + sep
	}
	if unparsed > 0 {
		notes = append(notes, fmt.Sprintf("%d workflow file(s) did not parse and could not be classified.", unparsed))
	}
	ev.Limitations = notes
	if len(notes) > 0 {
		vals["notes"] = strings.Join(notes, " | ")
	}
	s.sink.Emit(ev)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func sameDir(a, b string) bool {
	ca, err := discovery.Canonical(filepath.FromSlash(a))
	if err != nil {
		return false
	}
	cb, err := discovery.Canonical(b)
	if err != nil {
		return false
	}
	return discovery.Key(ca) == discovery.Key(cb)
}

var remoteRe = regexp.MustCompile(`^(?:https?://(?:[^/@]+@)?github\.com/|ssh://(?:[^/@]+@)?github\.com(?::\d+)?/|(?:[^/@]+@)?github\.com:)([A-Za-z0-9_.\-]+)/([A-Za-z0-9_.\-]+?)(?:\.git)?/?$`)

// parseGitHubRemote extracts owner/repo from a github.com remote URL. Credentials in the URL are
// ignored and never stored.
func parseGitHubRemote(u string) (owner, repo string, ok bool) {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}
