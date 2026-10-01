package ant

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const maxMacroDepth = 8

// finalize runs once a file (and everything it imports) is parsed: scoped properties, attribute
// substitution, macro expansion, the dependency graph, cross-file calls and environment references.
func (ld *loader) finalize(p *Project) {
	p.defineScoped()
	p.collectIDs()
	for _, t := range p.TopTasks {
		p.resolveTree(t)
	}
	for _, n := range p.TargetOrder {
		if t := p.Targets[n]; t != nil {
			for _, k := range t.Tasks {
				p.resolveTree(k)
			}
		}
	}
	for _, m := range p.Macros {
		for _, b := range m.Body {
			p.resolveTree(b)
		}
	}
	p.expandAll()
	p.buildEdges()
	p.scanEnv()
}

func (p *Project) collectIDs() {
	var walk func(t *Task)
	walk = func(t *Task) {
		if id := t.Raw["id"]; id != "" {
			if _, ok := p.paths[id]; !ok && dataElems[strings.ToLower(t.Name)] {
				p.paths[id] = t
			}
		}
		for _, c := range t.Children {
			walk(c)
		}
	}
	for _, n := range p.TargetOrder {
		if t := p.Targets[n]; t != nil {
			for _, k := range t.Tasks {
				walk(k)
			}
		}
	}
	for _, t := range p.TopTasks {
		walk(t)
	}
}

// ---- macro expansion ---------------------------------------------------------------------------

func (p *Project) expandAll() {
	for _, t := range p.TopTasks {
		p.expandTree(t, nil)
	}
	for _, n := range p.TargetOrder {
		if t := p.Targets[n]; t != nil {
			for _, k := range t.Tasks {
				p.expandTree(k, nil)
			}
		}
	}
}

func containsStr(ss []string, s string) bool {
	return slices.Contains(ss, s)
}

func (p *Project) expandTree(t *Task, stack []string) {
	p.expandCall(t, stack)
	if len(t.Expansion) > 0 {
		return // the call's element contents are expanded through the cloned body
	}
	for _, c := range t.Children {
		p.expandTree(c, stack)
	}
}

func cloneTask(b *Task) *Task {
	c := *b
	c.Raw = make(map[string]string, len(b.Raw))
	maps.Copy(c.Raw, b.Raw)
	c.Attrs, c.Unresolved, c.Expansion = nil, nil, nil
	c.Children = nil
	for _, ch := range b.Children {
		c.Children = append(c.Children, cloneTask(ch))
	}
	return &c
}

func substRaw(s string, vals map[string]string) string {
	if !strings.Contains(s, "@{") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '@' && i+1 < len(s) && s[i+1] == '{' {
			if end := strings.IndexByte(s[i+2:], '}'); end >= 0 {
				name := strings.ToLower(s[i+2 : i+2+end])
				if v, ok := vals[name]; ok {
					b.WriteString(v)
					i += 2 + end
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func substTask(b *Task, vals map[string]string, elems map[string][]*Task) *Task {
	c := cloneTask(b)
	c.Children = nil
	for k, v := range c.Raw {
		c.Raw[k] = substRaw(v, vals)
	}
	c.Text = substRaw(c.Text, vals)
	for _, ch := range b.Children {
		if repl, ok := elems[ch.Name]; ok && len(ch.Children) == 0 && len(ch.Raw) == 0 {
			for _, r := range repl {
				rc := cloneTask(r)
				rc.TaskPos = ch.TaskPos
				c.Children = append(c.Children, rc)
			}
			continue
		}
		c.Children = append(c.Children, substTask(ch, vals, elems))
	}
	return c
}

func (p *Project) expandCall(t *Task, stack []string) {
	m := p.Macros[t.Name]
	if m == nil || m.Kind == "scriptdef" || containsStr(stack, t.Name) || len(stack) >= maxMacroDepth || len(t.Expansion) > 0 {
		return
	}
	m.Calls++
	callLoc := t.Loc
	if !t.CallLoc.IsZero() {
		callLoc = t.CallLoc
	}
	next := append(append([]string(nil), stack...), m.Name)
	finish := func(c *Task) {
		c.ViaMacro = m.Name
		c.CallLoc = callLoc
		c.Target = t.Target
		c.TaskPos = true
		p.resolveTree(c)
		t.Expansion = append(t.Expansion, c)
		p.expandTree(c, next)
	}
	switch m.Kind {
	case "presetdef":
		for _, b := range m.Body {
			c := cloneTask(b)
			maps.Copy(c.Raw, t.Raw)
			for _, ch := range t.Children {
				c.Children = append(c.Children, cloneTask(ch))
			}
			finish(c)
		}
	case "macrodef":
		vals := map[string]string{}
		for _, a := range m.Attributes {
			if v, ok := t.Raw[a.Name]; ok {
				r, _ := p.Resolve(v)
				vals[a.Name] = r
			} else {
				vals[a.Name] = a.Default
			}
		}
		elems := map[string][]*Task{}
		for _, el := range m.Elements {
			for _, ch := range t.Children {
				if ch.Name == el {
					elems[el] = append(elems[el], ch.Children...)
				}
			}
			if _, ok := elems[el]; !ok {
				elems[el] = nil
			}
		}
		for _, b := range m.Body {
			finish(substTask(b, vals, elems))
		}
	}
}

// ---- walking -----------------------------------------------------------------------------------

func walkTask(t *Task, fn func(*Task)) {
	fn(t)
	for _, c := range t.Children {
		walkTask(c, fn)
	}
	for _, e := range t.Expansion {
		walkTask(e, fn)
	}
}

func walkTasks(ts []*Task, fn func(*Task)) {
	for _, t := range ts {
		walkTask(t, fn)
	}
}

// EachTask visits every task occurrence: project-level tasks, every target's tasks (macro and
// presetdef calls expanded in place) and the bodies of macros that are never called, so that for
// example a <javac> hidden in an unused macrodef is still seen (with its @{attr} unresolved).
// Nested data elements are visited too; test Task.TaskPos to select tasks.
func (p *Project) EachTask(fn func(*Task)) {
	walkTasks(p.TopTasks, fn)
	for _, n := range p.TargetOrder {
		if t := p.Targets[n]; t != nil {
			walkTasks(t.Tasks, fn)
		}
	}
	names := make([]string, 0, len(p.Macros))
	for n := range p.Macros {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if m := p.Macros[n]; m.Calls == 0 {
			walkTasks(m.Body, fn)
		}
	}
}

// ---- edges and cross-file calls ----------------------------------------------------------------

func (p *Project) buildEdges() {
	for _, n := range p.TargetOrder {
		t := p.Targets[n]
		if t == nil {
			continue
		}
		for _, d := range t.Depends {
			name, ok := p.Resolve(d)
			e := Edge{Kind: "depends", Raw: d, Name: name, Resolved: ok, Loc: t.Loc}
			if !ok {
				e.Reason = "unresolved property reference: " + strings.Join(unresolvedNames(name), ", ")
			}
			t.Edges = append(t.Edges, e)
		}
		walkTasks(t.Tasks, func(x *Task) { p.taskEdges(t, x) })
	}
	walkTasks(p.TopTasks, func(x *Task) { p.taskEdges(nil, x) })
}

func (p *Project) addEdges(t *Target, es ...Edge) {
	if t == nil {
		p.TopEdges = append(p.TopEdges, es...)
		return
	}
	t.Edges = append(t.Edges, es...)
}

func (p *Project) taskEdges(t *Target, x *Task) {
	if !x.TaskPos || len(x.Expansion) > 0 {
		return
	}
	switch strings.ToLower(x.Name) {
	case "antcall", "antcallback", "runtarget", "foreach":
		raw, ok := x.Raw["target"]
		if !ok || raw == "" {
			return
		}
		val, _, _, rok := x.Value("target")
		e := Edge{Kind: "antcall", Raw: raw, Name: val, Resolved: rok, Loc: x.Loc}
		if !rok {
			e.Reason = "unresolved property reference: " + strings.Join(unresolvedNames(val), ", ")
		}
		p.addEdges(t, e)
	case "ant":
		p.antEdges(t, x)
	case "subant":
		p.subantEdges(t, x)
	}
}

func targetName(t *Target) string {
	if t == nil {
		return ""
	}
	return t.Name
}

func (p *Project) pathAttr(x *Task, attr string) (string, bool, bool) {
	val, _, present, ok := x.Value(attr)
	return val, present, ok
}

func (p *Project) absFrom(dir, v string) string {
	if isAbsPath(v) {
		return filepath.Clean(filepath.FromSlash(v))
	}
	return filepath.Clean(filepath.FromSlash(path.Join(dir, filepath.ToSlash(v))))
}

func (p *Project) antEdges(t *Target, x *Task) {
	from := targetName(t)
	cc := CrossCall{Kind: "ant", FromTarget: from, Raw: x.Raw["antfile"], Loc: x.Loc}
	unresolved := func(reason string) {
		cc.Reason = reason
		p.CrossCalls = append(p.CrossCalls, cc)
		p.addEdges(t, Edge{Kind: "ant", Raw: x.Raw["antfile"], Reason: reason, Loc: x.Loc})
	}
	dir := p.Basedir
	if d, present, ok := p.pathAttr(x, "dir"); present {
		if !ok {
			unresolved("dir is unresolved: " + strings.Join(unresolvedNames(d), ", "))
			return
		}
		dir = normAbs(p.absFrom(p.Basedir, d))
	}
	file := "build.xml"
	if f, present, ok := p.pathAttr(x, "antfile"); present && f != "" {
		if !ok {
			unresolved("antfile is unresolved: " + strings.Join(unresolvedNames(f), ", "))
			return
		}
		file = f
	}
	abs := p.absFrom(dir, file)
	sub, reason := p.openSub(abs)
	if sub == nil {
		unresolved(reason)
		return
	}
	var targets []string
	if v, present, ok := p.pathAttr(x, "target"); present && v != "" {
		if !ok {
			unresolved("target is unresolved: " + strings.Join(unresolvedNames(v), ", "))
			return
		}
		targets = append(targets, v)
	}
	for _, c := range x.Children {
		if strings.EqualFold(c.Name, "target") {
			if n, _, _, ok := c.Value("name"); ok && n != "" {
				targets = append(targets, n)
			}
		}
	}
	if len(targets) == 0 {
		if sub.Default == "" {
			unresolved("called build file declares no default target")
			return
		}
		targets = []string{sub.Default}
	}
	foreign := sub.File
	if sub == p {
		foreign = ""
	}
	for _, tn := range targets {
		p.addEdges(t, Edge{Kind: "ant", Raw: x.Raw["antfile"], Name: tn, Foreign: foreign, Resolved: true, Loc: x.Loc})
		c := cc
		c.File, c.Target, c.Resolved = sub.File, tn, true
		p.CrossCalls = append(p.CrossCalls, c)
	}
}

func (p *Project) subantEdges(t *Target, x *Task) {
	from := targetName(t)
	antfile := "build.xml"
	if f, present, ok := p.pathAttr(x, "antfile"); present && f != "" && ok {
		antfile = f
	}
	if g, present, ok := p.pathAttr(x, "genericantfile"); present && g != "" && ok {
		antfile = g
	}
	var targets []string
	if v, present, ok := p.pathAttr(x, "target"); present && v != "" && ok {
		targets = append(targets, strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })...)
	}
	var buildFiles []string
	bad := func(reason string) {
		p.CrossCalls = append(p.CrossCalls, CrossCall{Kind: "subant", FromTarget: from, Raw: antfile, Loc: x.Loc, Reason: reason})
		p.addEdges(t, Edge{Kind: "subant", Raw: antfile, Reason: reason, Loc: x.Loc})
	}
	addPath := func(v string) {
		abs := p.absFrom(p.Basedir, v)
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			abs = filepath.Join(abs, antfile)
		}
		buildFiles = append(buildFiles, abs)
	}
	if bp, present, ok := p.pathAttr(x, "buildpath"); present {
		if !ok {
			bad("buildpath is unresolved: " + strings.Join(unresolvedNames(bp), ", "))
		} else {
			for _, el := range splitPathList(bp) {
				addPath(el)
			}
		}
	}
	for _, c := range x.Children {
		switch strings.ToLower(c.Name) {
		case "fileset", "dirset":
			dv, _, present, ok := c.Value("dir")
			if !present {
				continue
			}
			if !ok {
				bad("fileset dir is unresolved: " + strings.Join(unresolvedNames(dv), ", "))
				continue
			}
			dirAbs := p.absFrom(p.Basedir, dv)
			wantDirs := strings.EqualFold(c.Name, "dirset")
			inc, exc := patterns(c)
			if len(inc) == 0 {
				inc = []string{"**/*.xml"}
				if wantDirs {
					inc = []string{"*"}
				}
			}
			for _, m := range globWalk(dirAbs, inc, exc, wantDirs, 200) {
				if wantDirs {
					m = filepath.Join(m, antfile)
				}
				buildFiles = append(buildFiles, m)
			}
		case "filelist":
			dv, _, present, ok := c.Value("dir")
			if !present || !ok {
				continue
			}
			files, _, _, fok := c.Value("files")
			if !fok {
				bad("filelist files are unresolved")
				continue
			}
			for _, f := range strings.FieldsFunc(files, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
				buildFiles = append(buildFiles, p.absFrom(p.absDirString(dv), f))
			}
		case "buildpath", "path":
			for _, pe := range c.Children {
				if v, _, present, ok := pe.Value("location"); present && ok {
					addPath(v)
				}
			}
		}
	}
	if len(buildFiles) == 0 {
		if len(x.Children) == 0 && x.Raw["buildpath"] == "" {
			bad("no buildpath or nested path/fileset")
		} else if len(p.CrossCalls) == 0 || p.CrossCalls[len(p.CrossCalls)-1].Loc != x.Loc {
			bad("no build files could be enumerated")
		}
		return
	}
	if len(buildFiles) > 100 {
		p.warn("%s: <subant> expands to %d build files; only the first 100 are followed", x.Loc, len(buildFiles))
		buildFiles = buildFiles[:100]
	}
	for _, abs := range buildFiles {
		sub, reason := p.openSub(abs)
		if sub == nil {
			p.CrossCalls = append(p.CrossCalls, CrossCall{Kind: "subant", FromTarget: from, Raw: p.ld.rel(abs), Loc: x.Loc, Reason: reason})
			p.addEdges(t, Edge{Kind: "subant", Raw: p.ld.rel(abs), Reason: reason, Loc: x.Loc})
			continue
		}
		tns := targets
		if len(tns) == 0 {
			if sub.Default == "" {
				p.addEdges(t, Edge{Kind: "subant", Raw: sub.File, Reason: "called build file declares no default target", Loc: x.Loc})
				continue
			}
			tns = []string{sub.Default}
		}
		foreign := sub.File
		if sub == p {
			foreign = ""
		}
		for _, tn := range tns {
			p.addEdges(t, Edge{Kind: "subant", Raw: sub.File, Name: tn, Foreign: foreign, Resolved: true, Loc: x.Loc})
			p.CrossCalls = append(p.CrossCalls, CrossCall{Kind: "subant", FromTarget: from, Raw: sub.File, File: sub.File, Target: tn, Loc: x.Loc, Resolved: true})
		}
	}
}

func (p *Project) absDirString(dv string) string { return normAbs(p.absFrom(p.Basedir, dv)) }

// splitPathList splits an Ant path string on ';' and ':' without breaking drive letters.
func splitPathList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ";") {
		cur := ""
		for seg := range strings.SplitSeq(part, ":") {
			switch {
			case cur == "":
				cur = seg
			case len(cur) == 1 && ((cur[0] >= 'a' && cur[0] <= 'z') || (cur[0] >= 'A' && cur[0] <= 'Z')):
				cur += ":" + seg
			default:
				out = append(out, cur)
				cur = seg
			}
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	return out
}

// openSub loads (once) the build file at abs for an <ant>/<subant> call.
func (p *Project) openSub(abs string) (*Project, string) {
	ld := p.ld
	k := key(abs)
	if sp, ok := ld.byKey[k]; ok {
		return sp, ""
	}
	if err, ok := ld.fail[k]; ok {
		return nil, "called build file could not be parsed: " + err.Error()
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		return nil, "called build file not found: " + ld.rel(abs)
	}
	if len(ld.byKey) >= maxProjects {
		return nil, "called-build-file limit reached; " + ld.rel(abs) + " not followed"
	}
	sp, perr := ld.parseProject(abs, p)
	if perr != nil {
		ld.fail[k] = perr
		return nil, "called build file could not be parsed: " + perr.Error()
	}
	ld.finalize(sp)
	return sp, ""
}

func patterns(c *Task) (inc, exc []string) {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	}
	inc = split(c.Attrs["includes"])
	exc = split(c.Attrs["excludes"])
	for _, ch := range c.Children {
		switch strings.ToLower(ch.Name) {
		case "include":
			if n := ch.Attrs["name"]; n != "" {
				inc = append(inc, n)
			}
		case "exclude":
			if n := ch.Attrs["name"]; n != "" {
				exc = append(exc, n)
			}
		}
	}
	return inc, exc
}

func matchSegs(ps, ss []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			for len(ps) > 0 && ps[0] == "**" {
				ps = ps[1:]
			}
			if len(ps) == 0 {
				return true
			}
			for i := 0; i <= len(ss); i++ {
				if matchSegs(ps, ss[i:]) {
					return true
				}
			}
			return false
		}
		if len(ss) == 0 {
			return false
		}
		if ok, _ := path.Match(ps[0], ss[0]); !ok {
			return false
		}
		ps, ss = ps[1:], ss[1:]
	}
	return len(ss) == 0
}

func antMatch(pattern, rel string) bool {
	pattern = strings.ToLower(strings.ReplaceAll(pattern, "\\", "/"))
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(strings.ToLower(rel), "/"))
}

// globWalk lists files (or directories) under dir matching Ant include/exclude patterns.
func globWalk(dir string, inc, exc []string, wantDirs bool, limit int) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".svn") {
			return filepath.SkipDir
		}
		if p == dir || d.IsDir() != wantDirs {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		matched := false
		for _, pat := range inc {
			if antMatch(pat, rel) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		for _, pat := range exc {
			if antMatch(pat, rel) {
				return nil
			}
		}
		out = append(out, p)
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ---- environment references --------------------------------------------------------------------

func (p *Project) scanEnv() {
	prefixes := []string{"env"}
	var declared []string
	for k := range p.EnvPrefixes {
		if k != "env" && k != "" {
			declared = append(declared, k)
		}
	}
	sort.Strings(declared)
	prefixes = append(prefixes, declared...)
	seen := map[string]bool{}
	add := func(s string, loc Location) {
		// Scan raw text: substituted values never show the reference.
		for i := 0; i+1 < len(s); i++ {
			if s[i] == '$' && s[i+1] == '{' {
				end := strings.IndexByte(s[i+2:], '}')
				if end < 0 {
					break
				}
				name := s[i+2 : i+2+end]
				for _, pre := range prefixes {
					if strings.HasPrefix(name, pre+".") && len(name) > len(pre)+1 {
						p.noteEnv(seen, name[len(pre)+1:], pre, loc)
					}
				}
				i += 2 + end
			}
		}
	}
	for _, r := range p.rawStrs {
		add(r.s, r.loc)
	}
	scan := func(t *Task) {
		for k, v := range t.Raw {
			add(v, t.Loc)
			if k == "property" && strings.EqualFold(t.Name, "isset") {
				for _, pre := range prefixes {
					if strings.HasPrefix(v, pre+".") && len(v) > len(pre)+1 {
						p.noteEnv(seen, v[len(pre)+1:], pre, t.Loc)
					}
				}
			}
		}
	}
	p.EachTask(scan)
	sort.SliceStable(p.EnvRefs, func(i, j int) bool {
		a, b := p.EnvRefs[i], p.EnvRefs[j]
		if a.Var != b.Var {
			return a.Var < b.Var
		}
		if a.Loc.File != b.Loc.File {
			return a.Loc.File < b.Loc.File
		}
		return a.Loc.Line < b.Loc.Line
	})
}

func (p *Project) noteEnv(seen map[string]bool, v, prefix string, loc Location) {
	k := v + "|" + loc.String()
	if seen[k] {
		return
	}
	seen[k] = true
	p.EnvRefs = append(p.EnvRefs, EnvRef{Var: v, Prefix: prefix, Loc: loc})
}
