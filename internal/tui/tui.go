// Package tui is the interactive front end. It drives exactly the same cli.Assess as the command
// line; projects, their enabled flag and the side-effect permissions live in parameters.yaml.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"anamnesis/internal/cli"
	"anamnesis/internal/config"
	"anamnesis/internal/discovery"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
)

const (
	stNew     = "NEW"
	stReady   = "READY"
	stAttn    = "NEEDS ATTENTION"
	stRunning = "RUNNING"
	stDone    = "DONE"
	stFailed  = "FAILED"
)

var (
	styTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("24")).Padding(0, 1)
	styDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styHead   = lipgloss.NewStyle().Bold(true)
	stySel    = lipgloss.NewStyle().Reverse(true)
	styPane   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	styErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styRun    = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styStatus = map[string]lipgloss.Style{
		stNew: styDim, stReady: styOK, stAttn: styWarn, stRunning: styRun, stDone: styOK.Bold(true), stFailed: styErr.Bold(true),
	}
)

type proj struct {
	Status   string
	Out      *cli.Outcome
	Progress map[string]string
	Order    []string
	Err      string
	Queued   bool
}

type mode int

const (
	modeList mode = iota
	modeInput
	modeInspect
)

type inputKind int

const (
	inputPath inputKind = iota
	inputFolder
)

type job struct {
	path   string
	key    string
	phases []engine.Phase
	only   map[string]bool
	prior  *cli.Outcome
}

type progressMsg struct {
	key string
	ev  engine.Event
}
type doneMsg struct {
	key string
	out cli.Outcome
	err error
}
type batchDoneMsg struct{}

type model_ struct {
	params *config.Parameters
	state  map[string]*proj
	cursor int
	mode   mode
	w, h   int
	msg    string

	input     string
	inputKind inputKind

	inspectCat int // 0 = all, n = nth category
	inspectOff int

	events   chan tea.Msg
	running  bool
	cancel   context.CancelFunc
	quitting bool
}

func keyOf(path string) string { return discovery.Key(filepath.Clean(path)) }

func newModel(p *config.Parameters) *model_ {
	m := &model_{params: p, state: map[string]*proj{}, events: make(chan tea.Msg, 1024), w: 110, h: 32}
	m.msg = "Space enables/disables a project (saved to parameters.yaml). P preflight, D deep analysis."
	return m
}

func (m *model_) st(path string) *proj {
	k := keyOf(path)
	if p, ok := m.state[k]; ok {
		return p
	}
	p := &proj{Status: stNew, Progress: map[string]string{}}
	m.state[k] = p
	return p
}

func (m *model_) current() (config.Project, *proj, bool) {
	if m.cursor < 0 || m.cursor >= len(m.params.Projects) {
		return config.Project{}, nil, false
	}
	pr := m.params.Projects[m.cursor]
	return pr, m.st(pr.Path), true
}

func projName(pr config.Project) string {
	if pr.Name != "" {
		return pr.Name
	}
	return filepath.Base(pr.Path)
}

func (m *model_) save() {
	if err := m.params.Save(); err != nil {
		m.msg = styErr.Render("could not save " + m.params.Path + ": " + err.Error())
	}
}

func (m *model_) Init() tea.Cmd { return waitEvent(m.events) }

func waitEvent(ch <-chan tea.Msg) tea.Cmd { return func() tea.Msg { return <-ch } }

// ---- update ---------------------------------------------------------------------------------

func (m *model_) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = v.Width, v.Height
	case progressMsg:
		if p, ok := m.state[v.key]; ok {
			p.Status, p.Queued = stRunning, false
			if _, seen := p.Progress[v.ev.Analyzer]; !seen {
				p.Order = append(p.Order, v.ev.Analyzer)
			}
			p.Progress[v.ev.Analyzer] = v.ev.State
		}
		return m, waitEvent(m.events)
	case doneMsg:
		m.finish(v)
		return m, waitEvent(m.events)
	case batchDoneMsg:
		m.running = false
		m.cancel = nil
		if m.quitting {
			return m, tea.Quit
		}
		m.msg = "Run finished."
		return m, waitEvent(m.events)
	case tea.KeyMsg:
		switch m.mode {
		case modeInput:
			return m.updateInput(v)
		case modeInspect:
			return m.updateInspect(v)
		}
		return m.updateList(v)
	}
	return m, nil
}

func (m *model_) finish(v doneMsg) {
	p, ok := m.state[v.key]
	if !ok {
		return
	}
	p.Queued = false
	o := v.out
	switch {
	case v.err != nil && o.RunDir == "":
		p.Status, p.Err = stFailed, v.err.Error()
	case o.RunDir == "":
		p.Status, p.Err = stFailed, fmt.Sprint(o.Err)
	default:
		p.Out = &o
		p.Err = ""
		if o.Err != nil {
			p.Err = o.Err.Error()
		}
		p.Status = deriveStatus(o)
	}
}

func deriveStatus(o cli.Outcome) string {
	if len(o.Failed()) > 0 || o.Cancelled || o.Err != nil {
		return stAttn
	}
	if c := o.Conclusion("environment.missing"); c != nil && c.Value != "" && c.Value != "0" {
		return stAttn
	}
	if o.Deep {
		return stDone
	}
	return stReady
}

func (m *model_) updateList(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if k.Type == tea.KeySpace {
		s = " "
	}
	if len(s) == 1 {
		s = strings.ToLower(s)
	}
	switch s {
	case "ctrl+c", "q":
		if m.running && !m.quitting {
			m.quitting = true
			m.cancel()
			m.msg = styWarn.Render("Stopping: partial evidence and reports are being written. Press Q again to quit immediately.")
			return m, nil
		}
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.params.Projects)-1 {
			m.cursor++
		}
	case " ":
		if pr, _, ok := m.current(); ok {
			e := !pr.IsEnabled()
			m.params.Projects[m.cursor].Enabled = &e
			m.save()
		}
	case "a":
		m.mode, m.inputKind, m.input = modeInput, inputPath, ""
	case "f":
		m.mode, m.inputKind, m.input = modeInput, inputFolder, ""
	case "x":
		if pr, _, ok := m.current(); ok {
			m.params.Projects = append(m.params.Projects[:m.cursor:m.cursor], m.params.Projects[m.cursor+1:]...)
			delete(m.state, keyOf(pr.Path))
			if m.cursor >= len(m.params.Projects) && m.cursor > 0 {
				m.cursor--
			}
			m.save()
			m.msg = "Removed " + projName(pr) + " from parameters.yaml (the repository itself is untouched)."
		}
	case "p":
		return m.launch(false)
	case "d":
		return m.launch(true)
	case "t":
		return m.rerunFailed()
	case "r":
		m.openReport()
	case "e":
		m.export()
	case "i":
		if _, p, ok := m.current(); ok && p.Out != nil {
			m.mode, m.inspectCat, m.inspectOff = modeInspect, 0, 0
		} else {
			m.msg = "Nothing to inspect yet: run P or D on this project first."
		}
	}
	return m, nil
}

// targets are the enabled projects, or the project under the cursor when none is enabled.
func (m *model_) targets() []config.Project {
	var out []config.Project
	for _, pr := range m.params.Projects {
		if pr.IsEnabled() {
			out = append(out, pr)
		}
	}
	if len(out) == 0 {
		if pr, _, ok := m.current(); ok {
			out = append(out, pr)
		}
	}
	return out
}

func (m *model_) launch(deep bool) (tea.Model, tea.Cmd) {
	if m.running {
		m.msg = "A run is already in progress."
		return m, nil
	}
	phases := []engine.Phase{engine.Preflight}
	what := "Preflight"
	if deep {
		phases = []engine.Phase{engine.Preflight, engine.Deep}
		what = fmt.Sprintf("Deep analysis (parameters.yaml allows: build=%s mta=%s network=%s; edit the file to change)",
			onOff(m.params.Build.Allow), onOff(m.params.MTA.Allow), onOff(m.params.Network.Allow))
	}
	var jobs []job
	for _, pr := range m.targets() {
		jobs = append(jobs, job{path: pr.Path, key: keyOf(pr.Path), phases: phases})
	}
	if len(jobs) == 0 {
		m.msg = "No projects: press A to add a path or F to add a folder."
		return m, nil
	}
	m.msg = fmt.Sprintf("%s on %d project(s)...", what, len(jobs))
	return m, m.start(jobs)
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "off"
}

func (m *model_) rerunFailed() (tea.Model, tea.Cmd) {
	if m.running {
		m.msg = "A run is already in progress."
		return m, nil
	}
	var jobs []job
	for _, pr := range m.targets() {
		p := m.st(pr.Path)
		if p.Out == nil {
			continue
		}
		failed := p.Out.Failed()
		if len(failed) == 0 {
			continue
		}
		only := map[string]bool{}
		for _, n := range failed {
			only[n] = true
		}
		phases := []engine.Phase{engine.Preflight}
		if p.Out.Deep {
			phases = []engine.Phase{engine.Preflight, engine.Deep}
		}
		jobs = append(jobs, job{path: pr.Path, key: keyOf(pr.Path), phases: phases, only: only, prior: p.Out})
	}
	if len(jobs) == 0 {
		m.msg = "No failed analyzers to rerun."
		return m, nil
	}
	m.msg = fmt.Sprintf("Rerunning failed analyzers on %d project(s)...", len(jobs))
	return m, m.start(jobs)
}

// start runs the jobs one after another in a goroutine; progress and results return as messages.
func (m *model_) start(jobs []job) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.running = cancel, true
	snap := *m.params // the run reads a snapshot; the UI may edit the live parameters meanwhile
	snap.Projects = append([]config.Project(nil), m.params.Projects...)
	snap.JDKs = append([]config.JDK(nil), m.params.JDKs...)
	for _, j := range jobs {
		p := m.st(j.path)
		p.Status, p.Queued, p.Err = stRunning, true, ""
		p.Progress, p.Order = map[string]string{}, nil
	}
	ch := m.events
	go func() {
		for _, j := range jobs {
			if ctx.Err() != nil {
				ch <- doneMsg{key: j.key, err: ctx.Err()}
				continue
			}
			key := j.key
			opts := cli.Options{
				Params: &snap, Phases: j.phases, Only: j.only, Args: []string{"tui"},
				Progress: func(ev engine.Event) { ch <- progressMsg{key: key, ev: ev} },
			}
			if j.prior != nil {
				opts.Prior = map[string]cli.Outcome{discovery.Key(j.prior.Root): *j.prior}
			}
			outs, err := cli.Assess(ctx, []string{j.path}, opts)
			d := doneMsg{key: key, err: err}
			if len(outs) > 0 {
				d.out = outs[0]
			}
			ch <- d
		}
		ch <- batchDoneMsg{}
	}()
	return nil
}

func (m *model_) updateInput(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.mode = modeList
	case tea.KeyEnter:
		m.mode = modeList
		m.submitInput(strings.TrimSpace(m.input))
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		m.input = ""
	case tea.KeyCtrlC:
		m.mode = modeList
	case tea.KeySpace:
		m.input += " "
	case tea.KeyRunes:
		m.input += strings.NewReplacer("\r", "", "\n", "").Replace(string(k.Runes))
	}
	return m, nil
}

func (m *model_) has(path string) bool {
	for _, pr := range m.params.Projects {
		if keyOf(pr.Path) == keyOf(path) {
			return true
		}
	}
	return false
}

func (m *model_) submitInput(text string) {
	if text == "" {
		return
	}
	switch m.inputKind {
	case inputPath:
		c, err := discovery.Canonical(text) // strips pasted quotes
		if err != nil {
			m.msg = styErr.Render("Cannot add: " + err.Error())
			return
		}
		if m.has(c) {
			m.msg = "Already in the list: " + c
			return
		}
		m.params.Projects = append(m.params.Projects, config.Project{Path: c})
		m.cursor = len(m.params.Projects) - 1
		m.save()
		m.msg = "Added " + c
	case inputFolder:
		cands, err := discovery.Candidates(text)
		if err != nil {
			m.msg = styErr.Render("Cannot scan: " + err.Error())
			return
		}
		added := 0
		for _, c := range cands {
			if canon, err := discovery.Canonical(c); err == nil && !m.has(canon) {
				m.params.Projects = append(m.params.Projects, config.Project{Path: canon})
				added++
			}
		}
		m.save()
		m.msg = fmt.Sprintf("Found %d candidate repositories, added %d new.", len(cands), added)
	}
}

func (m *model_) openReport() {
	_, p, ok := m.current()
	if !ok || p.Out == nil || p.Out.RunDir == "" {
		m.msg = "No report yet for this project."
		return
	}
	if runtime.GOOS != "windows" {
		m.msg = "Report folder: " + p.Out.RunDir
		return
	}
	if err := exec.Command("explorer.exe", p.Out.RunDir).Start(); err != nil {
		m.msg = styErr.Render("Could not open Explorer: " + err.Error())
		return
	}
	m.msg = "Opened " + p.Out.RunDir
}

func (m *model_) export() {
	var outs []cli.Outcome
	for _, pr := range m.params.Projects {
		if p := m.st(pr.Path); p.Out != nil {
			outs = append(outs, *p.Out)
		}
	}
	dir, err := cli.ExportConsolidated(m.params, outs)
	if err != nil {
		m.msg = styErr.Render("Export failed: " + err.Error())
		return
	}
	m.msg = "Consolidated report: " + filepath.Join(dir, "consolidated-report.html")
}

// ---- inspect --------------------------------------------------------------------------------

func (m *model_) categories(o *cli.Outcome) []string {
	set := map[string]bool{}
	for _, e := range o.Evidence {
		set[string(e.Category)] = true
	}
	var cats []string
	for c := range set {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	return cats
}

func (m *model_) inspectRows(o *cli.Outcome) []model.Evidence {
	if m.inspectCat == 0 {
		return o.Evidence
	}
	cats := m.categories(o)
	if m.inspectCat > len(cats) {
		return nil
	}
	var out []model.Evidence
	for _, e := range o.Evidence {
		if string(e.Category) == cats[m.inspectCat-1] {
			out = append(out, e)
		}
	}
	return out
}

func (m *model_) updateInspect(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	_, p, ok := m.current()
	if !ok || p.Out == nil {
		m.mode = modeList
		return m, nil
	}
	rows := m.inspectRows(p.Out)
	page := max(m.h-8, 3)
	s := k.String()
	switch {
	case s == "esc" || s == "q" || s == "i":
		m.mode = modeList
	case s == "ctrl+c":
		m.mode = modeList
	case s == "up" || s == "k":
		m.inspectOff--
	case s == "down" || s == "j":
		m.inspectOff++
	case s == "pgup":
		m.inspectOff -= page
	case s == "pgdown" || s == " ":
		m.inspectOff += page
	case s == "home":
		m.inspectOff = 0
	case s == "end":
		m.inspectOff = len(rows)
	case len(s) == 1 && s[0] >= '0' && s[0] <= '9':
		m.inspectCat, m.inspectOff = int(s[0]-'0'), 0
	}
	if mx := len(m.inspectRows(p.Out)) - page; m.inspectOff > mx {
		m.inspectOff = mx
	}
	if m.inspectOff < 0 {
		m.inspectOff = 0
	}
	return m, nil
}

// ---- view -----------------------------------------------------------------------------------

func fit(s string, w int) string {
	r := []rune(s)
	if w <= 0 {
		return ""
	}
	if len(r) > w {
		if w > 1 {
			r = append(r[:w-1], '…')
		} else {
			r = r[:w]
		}
	}
	return string(r) + strings.Repeat(" ", w-len(r))
}

func wrapText(s string, w int) []string {
	if w < 10 {
		w = 10
	}
	var out []string
	line := ""
	for word := range strings.FieldsSeq(s) {
		if line != "" && len([]rune(line))+1+len([]rune(word)) > w {
			out = append(out, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

func javaShort(o *cli.Outcome) string {
	if c := o.Conclusion("java.generation"); c != nil && c.Value != "" {
		parts := strings.Split(c.Value, "_JAVA_")
		if len(parts) == 2 {
			return "Java " + parts[1] + " (" + strings.ToLower(parts[0]) + ")"
		}
	}
	return "Java ?"
}

func summary(p *proj) string {
	if p.Out == nil {
		return "-"
	}
	bs := "?"
	if c := p.Out.Conclusion("build.systems"); c != nil && c.Value != "" {
		bs = c.Value
	}
	return bs + " | " + javaShort(p.Out)
}

func (m *model_) View() string {
	if m.mode == modeInspect {
		return m.viewInspect()
	}
	var b strings.Builder
	b.WriteString(styTitle.Render("ANAMNESIS "+cli.Version) + "  " + styDim.Render("parameters: "+m.params.Path) + "\n")
	leftW := max(m.w*46/100, 44)
	rightW := max(m.w-leftW-4, 30)
	bodyH := max(m.h-8, 8)
	left := styPane.Width(leftW).Height(bodyH).Render(m.viewList(leftW, bodyH))
	right := styPane.Width(rightW).Height(bodyH).Render(m.viewDetail(rightW, bodyH))
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n")
	if m.mode == modeInput {
		label := "Add path"
		if m.inputKind == inputFolder {
			label = "Add folder (scans for repositories)"
		}
		b.WriteString(styHead.Render(label+": ") + m.input + "▏  " + styDim.Render("Enter accept, Esc cancel, paste allowed, quotes stripped") + "\n")
	} else {
		b.WriteString(m.msg + "\n")
	}
	b.WriteString(styDim.Render("A add path  F add folder  X remove  Space enable  P preflight  D deep  R open report  E export consolidated  T rerun failed  I inspect  Q quit"))
	return b.String()
}

func (m *model_) viewList(w, h int) string {
	var lines []string
	lines = append(lines, styHead.Render(fit("  PROJECT", w-2)))
	if len(m.params.Projects) == 0 {
		lines = append(lines, styDim.Render("No projects yet. Press A to add a path or F to add a folder."))
	}
	statusW := 16
	nameW := 18
	sumW := max(w-6-nameW-statusW-2, 8)
	start := 0
	if m.cursor >= h-3 {
		start = m.cursor - (h - 4)
	}
	for i := start; i < len(m.params.Projects) && len(lines) < h; i++ {
		pr := m.params.Projects[i]
		p := m.st(pr.Path)
		box := "[ ]"
		if pr.IsEnabled() {
			box = "[x]"
		}
		status := p.Status
		if p.Status == stRunning && p.Queued {
			status = "RUNNING (queued)"
		}
		row := box + " " + fit(projName(pr), nameW) + " " + fit(summary(p), sumW) + " "
		stText := fit(status, statusW)
		if i == m.cursor {
			lines = append(lines, stySel.Render(row+stText))
		} else {
			lines = append(lines, row+styStatus[p.Status].Render(stText))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *model_) viewDetail(w, h int) string {
	pr, p, ok := m.current()
	if !ok {
		return styDim.Render("Select or add a project.")
	}
	var l []string
	add := func(s string) { l = append(l, wrapText(s, w-1)...) }
	l = append(l, styHead.Render(fit(projName(pr), w-1)))
	add(pr.Path)
	l = append(l, "Status: "+styStatus[p.Status].Render(p.Status))
	if p.Err != "" {
		add(styErr.Render("Error: " + p.Err))
	}
	if p.Status == stRunning {
		if len(p.Order) == 0 {
			l = append(l, styDim.Render("waiting for its turn..."))
		}
		for _, a := range p.Order {
			mark := "..."
			switch p.Progress[a] {
			case "done":
				mark = "ok "
			case "failed":
				mark = "FAILED"
			case "skipped":
				mark = "skipped"
			}
			l = append(l, fmt.Sprintf("  %-8s %s", mark, a))
		}
	}
	if o := p.Out; o != nil {
		l = append(l, "")
		topic := func(label, t string, withStatement bool) {
			c := o.Conclusion(t)
			if c == nil {
				return
			}
			v := c.Value
			if v == "" {
				v = "(" + string(c.Kind) + ")"
			}
			l = append(l, styHead.Render(label)+" ["+string(c.Kind)+"/"+string(c.Confidence)+"] "+v)
			if withStatement {
				for _, s := range wrapText(c.Statement, w-3) {
					l = append(l, "  "+s)
				}
			}
		}
		topic("Build systems", "build.systems", false)
		topic("Java declared", "java.declared", false)
		topic("Java local runtime (this machine)", "java.local-runtime", false)
		topic("Java generation", "java.generation", false)
		topic("Buildability, source", "buildability.source", false)
		topic("Buildability, local", "buildability.local-reproducibility", true)
		topic("Buildability, state", "buildability.state", false)
		topic("Prerequisites missing", "environment.missing", true)
		topic("MTA", "migration.mta", true)
		topic("MTA effort points", "migration.effort", false)
		if f := o.Failed(); len(f) > 0 {
			l = append(l, styErr.Render("Failed analyzers: "+strings.Join(f, ", ")+"  (T reruns them)"))
		}
		if o.Cancelled {
			l = append(l, styWarn.Render("This run was interrupted; evidence is partial."))
		}
		add("Report: " + filepath.Join(o.RunDir, "report.html"))
	}
	if len(l) > h {
		l = append(l[:h-1], styDim.Render("... (press I to inspect all evidence)"))
	}
	return strings.Join(l, "\n")
}

func (m *model_) viewInspect() string {
	pr, p, ok := m.current()
	if !ok || p.Out == nil {
		return "nothing to inspect"
	}
	cats := m.categories(p.Out)
	var b strings.Builder
	b.WriteString(styTitle.Render("EVIDENCE: "+projName(pr)) + "\n")
	tabs := []string{"0 ALL"}
	for i, c := range cats {
		if i < 9 {
			tabs = append(tabs, fmt.Sprintf("%d %s", i+1, c))
		}
	}
	for i, t := range tabs {
		if i == m.inspectCat {
			tabs[i] = stySel.Render(" " + t + " ")
		} else {
			tabs[i] = " " + t + " "
		}
	}
	b.WriteString(strings.Join(tabs, "") + "\n")
	rows := m.inspectRows(p.Out)
	page := max(m.h-8, 3)
	end := min(m.inspectOff+page, len(rows))
	for i := m.inspectOff; i < end; i++ {
		e := rows[i]
		loc := ""
		if len(e.Locations) > 0 {
			loc = fmt.Sprintf("  @ %s:%d", e.Locations[0].Path, e.Locations[0].Line)
		}
		line := fmt.Sprintf("[%s/%s] %s %s: %s%s", e.Status, e.Confidence, e.Kind, e.Subject, e.Finding, loc)
		line = fit(strings.ReplaceAll(line, "\n", " "), m.w-2)
		if e.Status == model.Failed || e.Status == model.Fail {
			line = styErr.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(styDim.Render(fmt.Sprintf("\n%d-%d of %d   Up/Down/PgUp/PgDn scroll, 0-9 filter by category, Esc back", m.inspectOff+1, end, len(rows))))
	return b.String()
}

// Run starts the TUI and returns the process exit code.
func Run() int {
	params, err := cli.LoadParams("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	m := newModel(params)
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
