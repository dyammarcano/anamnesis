// Package ant statically models Apache Ant build files: properties (first definition wins, in
// document order, imports at their position), imports, targets, tasks (macro/presetdef calls
// expanded), the target dependency graph and a safety classifier. Nothing here executes Ant or
// writes anywhere; build files are only read.
//
// Known limits (also surfaced as Warnings where they occur): values supplied at run time
// (-D, ${env.X}, system properties, condition/available/exec results) are unknown; external XML
// entities are not expanded; macro "implicit" elements are not expanded; a file reached through
// <ant>/<subant> is parsed once, with the property snapshot of its first caller.
package ant

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

const (
	maxImportDepth = 25
	maxProjects    = 500
	maxXMLBytes    = 32 << 20
)

// Location is a position in a build file. File is repo-relative with forward slashes (absolute when
// the file lies outside the analyzed root).
type Location struct {
	File string
	Line int
}

func (l Location) String() string {
	if l.Line > 0 {
		return fmt.Sprintf("%s:%d", l.File, l.Line)
	}
	return l.File
}

// IsZero reports whether the location is unset.
func (l Location) IsZero() bool { return l.File == "" }

// Property is one Ant property definition.
type Property struct {
	Name  string
	Value string // substituted at definition time; may still hold ${...} when Unresolved
	Raw   string
	// Origin is value, location, file, text, refid, condition (condition/available/uptodate/exec/...),
	// tstamp, var or builtin.
	Origin string
	// Loc is where the definition lives: the properties-file line for Origin=file, otherwise the element.
	Loc Location
	// Via is the <property file=...> element that loaded a file property.
	Via         Location
	Conditional bool // value unknown at parse time (set only if some condition holds, or by a command)
	Unresolved  bool // value references properties that are not defined
	Scoped      bool // defined inside a target (only when that target runs)
	// Alternatives holds later definitions that would win if the first (conditional) one did not set it.
	Alternatives []string
}

// Edge is one dependency-graph edge out of a target.
type Edge struct {
	Kind     string // depends | antcall | ant | subant
	Raw      string
	Name     string // target name (unqualified)
	Foreign  string // repo-relative build file when the edge crosses into another build file, else ""
	Resolved bool
	Reason   string // why the edge could not be resolved
	Loc      Location
}

// Task is an element in a target (or at project level) with attributes before (Raw) and after (Attrs)
// property substitution. Attribute names are lower-cased.
type Task struct {
	Name       string
	NS         string
	Raw        map[string]string
	Attrs      map[string]string
	Unresolved map[string]bool
	Text       string
	Loc        Location
	Children   []*Task
	// Expansion holds the tasks a macrodef/presetdef call stands for.
	Expansion []*Task
	// TaskPos is true when the element is in task position (a target child, or a child of a container
	// such as <sequential>); false for data elements nested inside another task (fileset, arg...).
	TaskPos  bool
	ViaMacro string   // macro or presetdef this task was expanded from
	CallLoc  Location // call site of the outermost macro call
	Target   string   // owning target, "" for project level
}

// Value returns the substituted and raw value of an attribute, whether it is present and whether
// substitution fully succeeded.
func (t *Task) Value(name string) (val, raw string, present, ok bool) {
	raw, present = t.Raw[name]
	if !present {
		return "", "", false, true
	}
	val, has := t.Attrs[name]
	if !has {
		val = raw
	}
	return val, raw, true, !t.Unresolved[name]
}

// Target is an Ant target.
type Target struct {
	Name        string
	Depends     []string
	If, Unless  string
	Description string
	Loc         Location
	File        string // build file that defines it
	Tasks       []*Task
	Edges       []Edge
	Depth       int    // import depth of the defining file
	Prefix      string // prefix applied by <include>/<import as=>
}

// MacroAttr is a <macrodef> attribute.
type MacroAttr struct{ Name, Default string }

// Macro is a macrodef, presetdef or scriptdef.
type Macro struct {
	Name       string
	Kind       string // macrodef | presetdef | scriptdef
	Attributes []MacroAttr
	Elements   []string
	Body       []*Task
	Loc        Location
	Calls      int
}

// UnresolvedImport is an <import>/<include> (or property file) that could not be followed.
type UnresolvedImport struct {
	Raw      string
	Reason   string
	Loc      Location
	Kind     string // import | include | property-file
	Optional bool
}

// CrossCall is an <ant> or <subant> reference to another build file.
type CrossCall struct {
	Kind       string // ant | subant
	FromTarget string // "" for project level
	Raw        string // antfile (or buildpath) as written
	File       string // resolved repo-relative build file, "" when unresolved
	Target     string
	Loc        Location
	Resolved   bool
	Reason     string
}

// EnvRef is a reference to an environment variable through an <property environment=...> prefix.
type EnvRef struct {
	Var    string
	Prefix string
	Loc    Location
}

// Project is one parsed build file with its imports merged in.
type Project struct {
	Root    string // absolute project root (OS path)
	File    string // repo-relative build file
	Name    string
	Default string
	Basedir string // absolute, forward slashes

	Props     map[string]*Property
	PropOrder []string

	Targets     map[string]*Target // includes prefixed aliases
	TargetOrder []string           // canonical target names, definition order
	Macros      map[string]*Macro
	TopTasks    []*Task // tasks outside any target; they run at parse time, before every target
	TopEdges    []Edge

	Imports              []string // repo-relative files followed through import/include
	Unresolved           []UnresolvedImport
	MissingPropertyFiles []UnresolvedImport
	CrossCalls           []CrossCall
	Files                []string // every file parsed for this project, main file first
	EnvPrefixes          map[string]Location
	EnvRefs              []EnvRef
	Warnings             []string
	Subs                 map[string]*Project // every build file loaded in this load (including this one), by File

	absFile  string
	ld       *loader
	imported map[string]bool
	stack    map[string]bool
	paths    map[string]*Task
	rawStrs  []rawStr
	rootN    string

	mu    sync.Mutex
	cache map[*Target]*targetInfo
	top   *targetInfo
}

type rawStr struct {
	s   string
	loc Location
}

type loader struct {
	root  string
	byKey map[string]*Project
	subs  map[string]*Project
	fail  map[string]error
}

type pctx struct {
	rel, abs, dir string
	depth         int
	prefix        string
	include       bool
}

func (p *Project) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if slices.Contains(p.Warnings, msg) {
		return
	}
	if len(p.Warnings) < 200 {
		p.Warnings = append(p.Warnings, msg)
	}
}

// IsRoot reports whether the build file sits at the project root.
func (p *Project) IsRoot() bool { return !strings.Contains(p.File, "/") }

func key(abs string) string {
	k := filepath.ToSlash(filepath.Clean(abs))
	if runtime.GOOS == "windows" {
		k = strings.ToLower(k)
	}
	return k
}

func normAbs(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func (ld *loader) rel(abs string) string {
	r, err := filepath.Rel(ld.root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(r)
}

// Load parses buildFile (repo-relative or absolute) under root, following imports. An error is
// returned only when the main file cannot be read at all; damage elsewhere becomes Warnings or
// UnresolvedImports.
func Load(root, buildFile string) (*Project, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	ld := &loader{root: filepath.Clean(rootAbs), byKey: map[string]*Project{}, subs: map[string]*Project{}, fail: map[string]error{}}
	abs := buildFile
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(ld.root, filepath.FromSlash(buildFile))
	}
	abs = filepath.Clean(abs)
	p, err := ld.parseProject(abs, nil)
	if err != nil {
		return nil, err
	}
	ld.finalize(p)
	return p, nil
}

func builtinProp(n string) bool {
	return n == "basedir" || n == "ant.file" || n == "ant.project.name" || strings.HasPrefix(n, "ant.file.")
}

func (ld *loader) parseProject(abs string, inherit *Project) (*Project, error) {
	p := &Project{
		Root: ld.root, File: ld.rel(abs), absFile: abs, ld: ld,
		Props: map[string]*Property{}, Targets: map[string]*Target{}, Macros: map[string]*Macro{},
		EnvPrefixes: map[string]Location{}, Subs: ld.subs,
		imported: map[string]bool{}, stack: map[string]bool{}, paths: map[string]*Task{},
		cache: map[*Target]*targetInfo{}, rootN: normAbs(ld.root),
	}
	p.Basedir = normAbs(filepath.Dir(abs))
	if inherit != nil {
		for _, n := range inherit.PropOrder {
			ip := inherit.Props[n]
			if ip == nil || builtinProp(n) {
				continue
			}
			cp := *ip
			p.Props[n] = &cp
			p.PropOrder = append(p.PropOrder, n)
		}
		maps.Copy(p.EnvPrefixes, inherit.EnvPrefixes)
	}
	ld.byKey[key(abs)] = p
	ld.subs[p.File] = p
	if err := p.parseFile(abs, 0, "", false); err != nil {
		delete(ld.byKey, key(abs))
		delete(ld.subs, p.File)
		return nil, err
	}
	return p, nil
}

func (p *Project) parseFile(abs string, depth int, prefix string, include bool) error {
	rel := p.ld.rel(abs)
	f, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	root, directives, perr := parseXML(io.LimitReader(f, maxXMLBytes))
	if root == nil {
		if perr != nil {
			return fmt.Errorf("%s: %w", rel, perr)
		}
		return fmt.Errorf("%s: no XML root element", rel)
	}
	if perr != nil {
		p.warn("%s: XML syntax error, file read only partially: %v", rel, perr)
	}
	for _, d := range directives {
		if strings.Contains(d, "<!ENTITY") && strings.Contains(d, "SYSTEM") {
			p.warn("%s: declares external XML entities, which are not expanded (content they include is not seen)", rel)
			break
		}
	}
	if root.Name != "project" {
		return fmt.Errorf("%s: root element is <%s>, not <project>", rel, root.Name)
	}
	p.Files = append(p.Files, rel)
	p.stack[key(abs)] = true
	defer delete(p.stack, key(abs))
	if include && prefix == "" {
		prefix = root.attr("name")
	}
	ctx := &pctx{rel: rel, abs: abs, dir: filepath.Dir(abs), depth: depth, prefix: prefix, include: include}
	p.processProject(root, ctx)
	return nil
}

func (p *Project) processProject(el *element, ctx *pctx) {
	name := el.attr("name")
	if ctx.depth == 0 {
		p.Name = name
		p.Default = el.attr("default")
		if bd, ok := el.get("basedir"); ok && bd != "" {
			v, _ := p.Resolve(bd)
			if isAbsPath(v) {
				p.Basedir = normAbs(v)
			} else {
				p.Basedir = normAbs(filepath.Join(ctx.dir, filepath.FromSlash(v)))
			}
		}
		p.setBuiltin("basedir", p.Basedir)
		p.setBuiltin("ant.file", normAbs(ctx.abs))
		p.setBuiltin("ant.project.name", name)
	}
	if name != "" {
		p.setBuiltin("ant.file."+name, normAbs(ctx.abs))
	}
	for _, c := range el.Children {
		p.processTop(c, ctx)
	}
}

func (p *Project) setBuiltin(name, value string) {
	if _, ok := p.Props[name]; ok {
		return
	}
	p.Props[name] = &Property{Name: name, Value: value, Raw: value, Origin: "builtin", Loc: Location{File: p.File}}
	p.PropOrder = append(p.PropOrder, name)
}

func (p *Project) newTask(el *element, ctx *pctx, target string, pos bool) *Task {
	t := &Task{
		Name: el.Name, NS: el.Space, Raw: map[string]string{}, Text: strings.TrimSpace(el.Text),
		Loc: Location{File: ctx.rel, Line: el.Line}, TaskPos: pos, Target: target,
	}
	for _, a := range el.Attrs {
		t.Raw[a.Name] = a.Value
	}
	childPos := pos && containers[strings.ToLower(el.Name)]
	for _, c := range el.Children {
		t.Children = append(t.Children, p.newTask(c, ctx, target, childPos))
	}
	return t
}

func (p *Project) processTop(el *element, ctx *pctx) {
	name := strings.ToLower(el.Name)
	switch name {
	case "description":
		return
	case "import", "include":
		t := p.newTask(el, ctx, "", true)
		p.doImport(t, ctx, name == "include")
		return
	case "target", "extension-point":
		p.parseTarget(el, ctx)
		return
	case "macrodef", "presetdef", "scriptdef":
		p.parseMacro(el, ctx)
		return
	}
	t := p.newTask(el, ctx, "", true)
	if dataElems[name] {
		if id := t.Raw["id"]; id != "" {
			if _, ok := p.paths[id]; !ok {
				p.paths[id] = t
			}
		}
		return
	}
	p.defineTask(t, false, false)
	if name == "property" || name == "loadproperties" {
		return // SAFE bookkeeping; not worth carrying in TopTasks
	}
	p.TopTasks = append(p.TopTasks, t)
}

func (p *Project) parseTarget(el *element, ctx *pctx) {
	name := el.attr("name")
	if name == "" {
		p.warn("%s:%d: <%s> without a name", ctx.rel, el.Line, el.Name)
		return
	}
	t := &Target{
		Name: name, File: ctx.rel, Loc: Location{File: ctx.rel, Line: el.Line},
		If: el.attr("if"), Unless: el.attr("unless"), Description: el.attr("description"), Prefix: ctx.prefix,
	}
	for d := range strings.SplitSeq(el.attr("depends"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			t.Depends = append(t.Depends, d)
		}
	}
	for _, c := range el.Children {
		if strings.EqualFold(c.Name, "description") {
			if t.Description == "" {
				t.Description = strings.TrimSpace(c.Text)
			}
			continue
		}
		task := p.newTask(c, ctx, name, true)
		if dataElems[strings.ToLower(c.Name)] {
			if id := task.Raw["id"]; id != "" {
				if _, ok := p.paths[id]; !ok {
					p.paths[id] = task
				}
			}
			continue
		}
		t.Tasks = append(t.Tasks, task)
	}
	p.register(t, ctx)
}

// register applies Ant's override rules: a definition in the importing file replaces one from an
// imported file; at equal depth the later definition wins. <include> exposes only prefixed names.
func (p *Project) register(t *Target, ctx *pctx) {
	t.Depth = ctx.depth
	put := func(name string, canonical bool) {
		cur, exists := p.Targets[name]
		if exists && cur.Depth < t.Depth {
			return
		}
		p.Targets[name] = t
		if canonical && !exists {
			p.TargetOrder = append(p.TargetOrder, name)
		}
	}
	if ctx.include && ctx.prefix != "" {
		put(ctx.prefix+"."+t.Name, true)
		return
	}
	put(t.Name, true)
	if ctx.prefix != "" {
		put(ctx.prefix+"."+t.Name, false)
	}
}

func (p *Project) parseMacro(el *element, ctx *pctx) {
	name := el.attr("name")
	if name == "" {
		return
	}
	m := &Macro{Name: name, Kind: strings.ToLower(el.Name), Loc: Location{File: ctx.rel, Line: el.Line}}
	switch m.Kind {
	case "macrodef":
		for _, c := range el.Children {
			switch strings.ToLower(c.Name) {
			case "attribute":
				m.Attributes = append(m.Attributes, MacroAttr{Name: strings.ToLower(c.attr("name")), Default: c.attr("default")})
			case "element":
				m.Elements = append(m.Elements, c.attr("name"))
			case "sequential", "parallel":
				seq := p.newTask(c, ctx, "", true)
				m.Body = append(m.Body, seq.Children...)
			}
		}
	case "presetdef":
		for _, c := range el.Children {
			m.Body = append(m.Body, p.newTask(c, ctx, "", true))
		}
	}
	p.Macros[name] = m
}

func isAbsPath(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "\\") {
		return true
	}
	return len(s) >= 3 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' && (s[2] == '/' || s[2] == '\\')
}

// doImport follows <import>/<include> at its position in document order.
func (p *Project) doImport(t *Task, ctx *pctx, include bool) {
	kind := "import"
	if include {
		kind = "include"
	}
	raw := t.Raw["file"]
	optional := strings.EqualFold(t.Raw["optional"], "true") || strings.EqualFold(t.Raw["optional"], "yes") || t.Raw["optional"] == "on"
	p.rawStrs = append(p.rawStrs, rawStr{raw, t.Loc})
	fail := func(reason string) {
		p.Unresolved = append(p.Unresolved, UnresolvedImport{Raw: raw, Reason: reason, Loc: t.Loc, Kind: kind, Optional: optional})
	}
	if raw == "" {
		fail("no file attribute (resource-collection imports are not followed)")
		return
	}
	res, ok := p.Resolve(raw)
	if !ok {
		fail("unresolved property reference: " + strings.Join(unresolvedNames(res), ", "))
		return
	}
	target := filepath.FromSlash(strings.ReplaceAll(res, "\\", "/"))
	if !filepath.IsAbs(target) && !isAbsPath(res) {
		target = filepath.Join(ctx.dir, target)
	}
	target = filepath.Clean(target)
	k := key(target)
	if p.stack[k] {
		p.warn("%s: import cycle through %s ignored", t.Loc, p.ld.rel(target))
		return
	}
	if p.imported[k] {
		return // Ant skips a file imported twice
	}
	if ctx.depth+1 > maxImportDepth {
		fail("import depth limit reached")
		return
	}
	st, err := os.Stat(target)
	if err != nil || st.IsDir() {
		fail("file not found: " + p.ld.rel(target))
		return
	}
	p.imported[k] = true
	prefix := t.Raw["as"]
	if err := p.parseFile(target, ctx.depth+1, prefix, include); err != nil {
		fail("could not be parsed: " + err.Error())
		return
	}
	p.Imports = append(p.Imports, p.ld.rel(target))
}
