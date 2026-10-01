package report

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/model"
)

// ConsolidatedRow is one project in the cross-project table.
type ConsolidatedRow struct {
	Project         string   `json:"project"`
	Root            string   `json:"root"`
	RunID           string   `json:"run_id"`
	BuildSystems    string   `json:"build_systems"`
	JavaDeclared    string   `json:"java_declared"`
	JavaBinaryMajor string   `json:"java_binary_major"`
	JavaGeneration  string   `json:"java_generation"`
	Buildability    string   `json:"buildability_state"`
	MTAState        string   `json:"mta_state"`
	EffortPoints    string   `json:"mta_effort_points"`
	FailedAnalyzers []string `json:"failed_analyzers"`
	ReportHTML      string   `json:"report_html"` // relative to the consolidated directory when possible
	ReportJSON      string   `json:"report_json"`
}

// Consolidated is the cross-project document.
type Consolidated struct {
	SchemaVersion string            `json:"schema_version"`
	AppVersion    string            `json:"anamnesis_version"`
	Projects      []ConsolidatedRow `json:"projects"`
}

func conclValue(r *Report, topic, none string) string {
	c := r.Conclusion(topic)
	if c == nil || c.Kind == model.KindUnknown || c.Value == "" {
		return none
	}
	return c.Value
}

func relLink(base, target string) string {
	if rel, err := filepath.Rel(base, target); err == nil {
		return (&url.URL{Path: filepath.ToSlash(rel)}).String()
	}
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(target), "/")
}

func buildRows(dir string, reports []Report) []ConsolidatedRow {
	rows := make([]ConsolidatedRow, 0, len(reports))
	for i := range reports {
		r := &reports[i]
		row := ConsolidatedRow{
			Project: r.Project.Name, Root: r.Project.Root, RunID: r.RunID,
			BuildSystems:    conclValue(r, "build.systems", "unknown"),
			JavaDeclared:    conclValue(r, "java.declared", "none declared"),
			JavaBinaryMajor: conclValue(r, "java.binaries", "unknown"),
			JavaGeneration:  conclValue(r, "java.generation", "unknown"),
			Buildability:    conclValue(r, "buildability.state", "unknown"),
			MTAState:        conclValue(r, "migration.mta", "unknown"),
			EffortPoints:    conclValue(r, "migration.effort", "n/a"),
			FailedAnalyzers: r.FailedAnalyzers(),
		}
		if row.FailedAnalyzers == nil {
			row.FailedAnalyzers = []string{}
		}
		if r.Project.RunDir != "" {
			row.ReportHTML = relLink(dir, filepath.Join(r.Project.RunDir, "report.html"))
			row.ReportJSON = relLink(dir, filepath.Join(r.Project.RunDir, "report.json"))
		}
		rows = append(rows, row)
	}
	return rows
}

// WriteConsolidated writes dir/consolidated-report.json and dir/consolidated-report.html.
func WriteConsolidated(dir string, reports []Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	doc := Consolidated{SchemaVersion: SchemaVersion, AppVersion: AppVersion, Projects: buildRows(dir, reports)}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "consolidated-report.json"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "consolidated-report.html"))
	if err != nil {
		return err
	}
	if err := consTmpl.Execute(f, struct {
		D       Consolidated
		Version string
		Rows    []consRowView
	}{doc, AppVersion, viewRows(doc.Projects)}); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type consRowView struct {
	ConsolidatedRow
	Href template.URL
	JSON template.URL
}

// viewRows marks the relative links as URLs; they are built by relLink (escaped path or file URL),
// never from analyzed content.
func viewRows(rows []ConsolidatedRow) []consRowView {
	out := make([]consRowView, len(rows))
	for i, r := range rows {
		out[i] = consRowView{ConsolidatedRow: r, Href: template.URL(r.ReportHTML), JSON: template.URL(r.ReportJSON)}
	}
	return out
}

var consTmpl = template.Must(template.New("cons").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Anamnesis - consolidated</title>
<style>
:root{--bg:#f6f7f9;--fg:#1c2128;--muted:#5b6670;--card:#fff;--line:#d8dde3;--accent:#1f6feb;--bad:#cf222e;--badbg:#ffebe9;--code:#eef1f4}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#8b949e;--card:#161b22;--line:#30363d;--accent:#58a6ff;--bad:#ff7b72;--badbg:#3c1618;--code:#21262d}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
header{padding:20px 24px;border-bottom:1px solid var(--line);background:var(--card)}
header h1{margin:0 0 4px;font-size:20px}.meta{color:var(--muted);font-size:13px}
main{padding:16px 24px 60px;overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:13px;background:var(--card)}
th,td{border:1px solid var(--line);padding:6px 8px;text-align:left;vertical-align:top}
th{background:var(--code)}
code{font-family:ui-monospace,Consolas,monospace;font-size:12px;background:var(--code);padding:0 4px;border-radius:3px;word-break:break-all}
a{color:var(--accent)}
.fail{color:var(--bad);background:var(--badbg);font-weight:600}
.num{text-align:right;font-variant-numeric:tabular-nums}
</style></head><body>
<header><h1>ANAMNESIS {{.Version}} &mdash; consolidated</h1>
<div class="meta">{{len .Rows}} project(s). Values are the machine values of each project's conclusions; open a report for the evidence behind them. Effort is in MTA effort points, never hours.</div></header>
<main><table><thead><tr><th>Project</th><th>Build systems</th><th>Java declared</th><th>Binary class major</th><th>Buildability state</th><th>MTA state</th><th class="num">MTA effort points</th><th>Failed analyzers</th><th>Report</th></tr></thead><tbody>
{{range .Rows}}<tr><td>{{.Project}}<br><span class="meta">{{.Root}}</span></td><td>{{.BuildSystems}}</td><td>{{.JavaDeclared}}</td><td>{{.JavaBinaryMajor}}</td><td><code>{{.Buildability}}</code></td><td><code>{{.MTAState}}</code></td><td class="num">{{.EffortPoints}}</td>
<td{{if .FailedAnalyzers}} class="fail"{{end}}>{{if .FailedAnalyzers}}{{range .FailedAnalyzers}}{{.}}<br>{{end}}{{else}}none{{end}}</td>
<td>{{if .Href}}<a href="{{.Href}}">report.html</a> &middot; <a href="{{.JSON}}">json</a>{{end}}</td></tr>
{{end}}</tbody></table></main></body></html>
`))
