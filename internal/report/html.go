package report

import (
	"html/template"
	"os"
	"path/filepath"
)

type htmlData struct {
	R        *Report
	Failed   int
	FailedNo int
	Secs     []secView
	Panel    []concView
	State    string
}

// WriteHTML writes dir/report.html: one self-contained file (inline CSS, no external assets, no
// scripts), usable from file://.
func (r *Report) WriteHTML(dir string) error {
	secs, failed, byTopic := r.view()
	d := htmlData{R: r, Failed: failed, FailedNo: failedSection(), Secs: secs}
	for _, t := range []string{"buildability.source", "buildability.local-reproducibility", "buildability.state", "environment.missing", "java.local-runtime"} {
		if cv, ok := byTopic[t]; ok {
			d.Panel = append(d.Panel, cv)
		}
	}
	if cv, ok := byTopic["buildability.state"]; ok {
		d.State = cv.Value
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "report.html"))
	if err != nil {
		return err
	}
	if err := reportTmpl.Execute(f, d); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

var reportTmpl = template.Must(template.New("report").Parse(`{{define "ev"}}{{if .}}<table class="ev"><thead><tr><th>Evidence</th><th>Finding</th><th>Where</th></tr></thead><tbody>
{{range .}}<tr{{if .Failed}} class="rowfail"{{end}}>
<td><span class="badge st-{{.Status}}">{{.Status}}</span> <span class="badge c-{{.Confidence}}">{{.Confidence}}</span><br><code>{{.Kind}}</code><br>{{.Subject}}</td>
<td>{{.Finding}}{{range .Limits}}<div class="lim">Limitation: {{.}}</div>{{end}}</td>
<td>{{range .Locs}}<code>{{.}}</code><br>{{end}}{{with .Cmd}}<div class="cmd"><b>command</b> <code>{{.Argv}}</code><br>dir <code>{{.Dir}}</code> &middot; exit <b>{{.Exit}}</b> &middot; {{.DurationMs}} ms{{if .TimedOut}} &middot; <span class="badge st-FAILED">TIMED OUT</span>{{end}}{{if .LogRef}}<br>log <code>{{.LogRef}}</code>{{end}}{{if .Err}}<br>error <code>{{.Err}}</code>{{end}}</div>{{end}}{{range .Artifacts}}<div>artifact <code>{{.}}</code></div>{{end}}</td>
</tr>{{end}}</tbody></table>{{end}}{{end}}
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Anamnesis - {{.R.Project.Name}}</title>
<style>
:root{--bg:#f6f7f9;--fg:#1c2128;--muted:#5b6670;--card:#fff;--line:#d8dde3;--accent:#1f6feb;--fact:#1a7f37;--evid:#0969da;--inf:#9a6700;--est:#8250df;--unk:#57606a;--bad:#cf222e;--badbg:#ffebe9;--warnbg:#fff8c5;--okbg:#dafbe1;--code:#eef1f4}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#8b949e;--card:#161b22;--line:#30363d;--accent:#58a6ff;--fact:#3fb950;--evid:#58a6ff;--inf:#d29922;--est:#bc8cff;--unk:#8b949e;--bad:#ff7b72;--badbg:#3c1618;--warnbg:#3a2f0b;--okbg:#10301a;--code:#21262d}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
header{padding:20px 24px;border-bottom:1px solid var(--line);background:var(--card)}
header h1{margin:0 0 4px;font-size:20px}
header .meta{color:var(--muted);font-size:13px}
main{max-width:1200px;margin:0 auto;padding:16px 24px 60px}
nav{margin:12px 0;font-size:13px;display:flex;flex-wrap:wrap;gap:6px 14px}
nav a{color:var(--accent);text-decoration:none}
section{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px 18px;margin:16px 0}
section h2{font-size:17px;margin:4px 0 10px}
.concl{border-top:1px solid var(--line);padding:8px 0}
.concl:first-of-type{border-top:0}
.chead{display:flex;flex-wrap:wrap;gap:6px;align-items:center}
.concl p{margin:6px 0}
.topic{background:var(--code);padding:1px 6px;border-radius:4px;font-size:12px}
.value{font-family:ui-monospace,Consolas,monospace;font-size:12px;border:1px solid var(--line);border-radius:4px;padding:1px 6px}
code{font-family:ui-monospace,Consolas,monospace;font-size:12px;background:var(--code);padding:0 4px;border-radius:3px;word-break:break-all}
.badge{display:inline-block;font-size:11px;font-weight:600;letter-spacing:.03em;padding:1px 7px;border-radius:10px;border:1px solid currentColor;white-space:nowrap}
.k-FACT{color:var(--fact)}.k-EVIDENCE{color:var(--evid)}.k-INFERENCE{color:var(--inf)}.k-ESTIMATE{color:var(--est)}.k-UNKNOWN{color:var(--unk);border-style:dashed}
.c-OBSERVED,.c-DERIVED,.c-INFERRED,.c-UNKNOWN{color:var(--muted);font-weight:500}
.st-FAILED,.st-FAIL{color:var(--bad);background:var(--badbg)}.st-WARN,.st-SKIPPED{color:var(--inf)}.st-PASS{color:var(--fact)}.st-INFO{color:var(--muted)}
details{margin:6px 0}
summary{cursor:pointer;color:var(--accent);font-size:13px}
table{border-collapse:collapse;width:100%;font-size:13px;margin:6px 0}
th,td{border:1px solid var(--line);padding:5px 8px;text-align:left;vertical-align:top}
th{background:var(--code)}
.rowfail td{background:var(--badbg)}
.cmd{margin-top:4px;font-size:12px;color:var(--muted)}
.lim{color:var(--muted);font-size:12px}
ul.plain{margin:6px 0;padding-left:20px}
.banner{border:2px solid var(--bad);background:var(--badbg);border-radius:8px;padding:10px 16px;margin:16px 0;font-weight:600}
.banner a{color:var(--bad)}
.panel{border:2px solid var(--accent);border-radius:8px;padding:12px 18px;margin:16px 0;background:var(--card)}
.panel h2{margin:2px 0 6px;font-size:18px}
.panel .state{font-family:ui-monospace,Consolas,monospace;font-size:15px;font-weight:700;padding:6px 10px;border-radius:6px;background:var(--warnbg);display:inline-block;margin:4px 0 8px}
.panel .note{color:var(--muted);font-size:13px;margin:4px 0 8px}
.empty{color:var(--muted);font-style:italic}
.num{text-align:right;font-variant-numeric:tabular-nums}
@media (max-width:700px){main{padding:12px}table{display:block;overflow-x:auto}}
</style></head><body>
<header><h1>ANAMNESIS {{.R.AppVersion}} &mdash; {{.R.Project.Name}}</h1>
<div class="meta">{{.R.Project.Root}} &middot; run {{.R.RunID}} &middot; schema {{.R.SchemaVersion}}</div>
<div class="meta">Every statement carries a kind (<span class="badge k-FACT">FACT</span> <span class="badge k-EVIDENCE">EVIDENCE</span> <span class="badge k-INFERENCE">INFERENCE</span> <span class="badge k-ESTIMATE">ESTIMATE</span> <span class="badge k-UNKNOWN">UNKNOWN</span>) and a confidence. A machine result is a fact about a check, not a verdict.</div></header>
<main>
{{if .Failed}}<div class="banner">{{.Failed}} analyzer(s) FAILED. Evidence from them may be partial or missing. <a href="#s{{.FailedNo}}">See section {{.FailedNo}}</a>.</div>{{end}}
<div class="panel" id="repro"><h2>Why the build could / could not be reproduced locally</h2>
<div class="state">{{if .State}}{{.State}}{{else}}NO BUILDABILITY EVIDENCE{{end}}</div>
<div class="note">Two independent axes: whether the <em>source</em> builds, and whether <em>this machine</em> reproduced it. A missing tool or a build that was not attempted never shows the source is unbuildable.</div>
{{range .Panel}}<div class="concl"><div class="chead"><span class="badge k-{{.Kind}}">{{.Kind}}</span> <span class="badge c-{{.Confidence}}">{{.Confidence}}</span> <code class="topic">{{.Topic}}</code>{{if .Value}} <span class="value">{{.Value}}</span>{{end}}</div>
<p>{{.Statement}}</p>
{{if .Because}}<details><summary>Evidence ({{len .Because}})</summary>{{template "ev" .Because}}</details>{{end}}
{{range .Limitations}}<div class="lim">Limitation: {{.}}</div>{{end}}</div>{{end}}
</div>
<nav>{{range .Secs}}<a href="#{{.Anchor}}">{{.No}}. {{.Title}}</a>{{end}}</nav>
{{range .Secs}}{{$brief := .Brief}}<section id="{{.Anchor}}"><h2>{{.No}}. {{.Title}}</h2>
{{range .Conclusions}}<div class="concl">
<div class="chead"><span class="badge k-{{.Kind}}">{{.Kind}}</span> <span class="badge c-{{.Confidence}}">{{.Confidence}}</span> <code class="topic">{{.Topic}}</code>{{if .Value}} <span class="value">{{.Value}}</span>{{end}}</div>
<p>{{.Statement}}</p>
{{if and .Because (not $brief)}}<details><summary>because: evidence ({{len .Because}}{{if .BecauseMore}} of {{len .EvidenceIDs}}{{end}})</summary>{{template "ev" .Because}}{{if .BecauseMore}}<div class="lim">... and {{.BecauseMore}} more supporting evidence item(s), most severe shown first; all are in evidence.jsonl and report.json</div>{{end}}{{range .Missing}}<div class="lim">evidence {{.}} is not present in this report</div>{{end}}</details>{{end}}
{{range .Assumptions}}<div class="lim">Assumption: {{.}}</div>{{end}}{{range .Limitations}}<div class="lim">Limitation: {{.}}</div>{{end}}
</div>{{end}}
{{if .Rows}}{{if .Open}}<div>{{template "ev" .Rows}}</div>{{else}}<details><summary>Observed evidence ({{len .Rows}}{{if .More}} shown, {{.More}} more in report.json{{end}})</summary>{{template "ev" .Rows}}</details>{{end}}{{end}}
{{if .Lines}}<ul class="plain">{{range .Lines}}<li>{{.}}</li>{{end}}</ul>{{end}}
{{if .Effort}}<table><thead><tr><th>Rule</th><th class="num">Effort points (effort x incidents)</th></tr></thead><tbody>{{range .Effort}}<tr><td><code>{{.K}}</code></td><td class="num">{{.V}}</td></tr>{{end}}</tbody></table>{{end}}
{{if .Timings}}<table><thead><tr><th>Analyzer</th><th>State</th><th class="num">Duration (ms)</th><th class="num">Evidence</th><th>Error</th></tr></thead><tbody>{{range .Timings}}<tr{{if eq .State "failed"}} class="rowfail"{{end}}><td>{{.Analyzer}}</td><td>{{if eq .State "failed"}}<span class="badge st-FAILED">FAILED</span>{{else}}{{.State}}{{end}}</td><td class="num">{{.DurationMs}}</td><td class="num">{{.Evidence}}</td><td>{{.Err}}</td></tr>{{end}}</tbody></table>{{end}}
{{if .Empty}}<p class="empty">{{.Empty}}</p>{{end}}
</section>{{end}}
</main></body></html>
`))
