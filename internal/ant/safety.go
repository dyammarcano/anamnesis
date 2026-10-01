package ant

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Class is the safety class of a task or target. A target's class is the worst class over its
// transitive closure. Target names are never evidence: only the tasks in the closure count.
type Class string

const (
	ClassSafe      Class = "SAFE"
	ClassReview    Class = "REVIEW_REQUIRED"
	ClassDangerous Class = "DANGEROUS"
	ClassUnknown   Class = "UNKNOWN"
)

func (c Class) rank() int {
	switch c {
	case ClassDangerous:
		return 3
	case ClassReview:
		return 2
	case ClassUnknown:
		return 1
	}
	return 0
}

// Reason explains why a target is not SAFE.
type Reason struct {
	Class   Class
	Task    string   // element name; "" for graph problems (missing target, unresolved call)
	Loc     Location // the task (or the referencing edge)
	Detail  string
	Path    []string // target chain from the classified target to the one holding the task
	Macro   string   // macrodef/presetdef the task was expanded from
	CallLoc Location // where that macro was called
	Scope   string   // "project-level" for tasks outside any target
}

func (r Reason) String() string {
	var s string
	if r.Task != "" {
		s = fmt.Sprintf("%s: <%s> at %s", r.Class, r.Task, r.Loc)
	} else {
		s = fmt.Sprintf("%s: %s", r.Class, r.Loc)
	}
	if r.Macro != "" {
		s += fmt.Sprintf(" (macro %s called at %s)", r.Macro, r.CallLoc)
	}
	s += ": " + r.Detail
	switch {
	case r.Scope == "project-level":
		s += " [project-level task, runs before any target"
		if len(r.Path) > 0 {
			s += "; entered via " + strings.Join(r.Path, " -> ")
		}
		s += "]"
	case len(r.Path) > 1:
		s += " [reached via " + strings.Join(r.Path, " -> ") + "]"
	}
	return s
}

// TargetSafety is the static classification of one target. Nothing is executed to produce it.
type TargetSafety struct {
	Target    string // plain name, or "<build file>#<name>" for a target in another build file
	Class     Class
	Reasons   []Reason // worst first
	Closure   []string // the target and everything it depends on or calls, traversal order
	Javac     int      // javac tasks in the closure
	Packaging int      // jar/war/ear/ejbjar tasks in the closure
	Default   bool
	Loc       Location
}

type finding struct {
	class  Class
	detail string
}

type foundTask struct {
	t *Task
	finding
}

type targetInfo struct {
	found []foundTask
	javac int
	pack  int
}

var serverTokens = []string{"home", "appserver", "jboss", "weblogic", "websphere", "glassfish", "tomcat", "catalina", "deploy", "server"}

func hasServerToken(s string) bool {
	s = strings.ToLower(s)
	for _, t := range serverTokens {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// strongServerToken is serverTokens without the very common "home" and "server".
func strongServerToken(s string) bool {
	s = strings.ToLower(s)
	for _, t := range serverTokens {
		if t == "home" || t == "server" {
			continue
		}
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func propRefs(raw string) []string {
	var out []string
	for _, r := range unresolvedNames(raw) {
		if after, ok := strings.CutPrefix(r, "${"); ok {
			out = append(out, strings.TrimSuffix(after, "}"))
		}
	}
	return out
}

func stripRefs(s string) string {
	for _, r := range unresolvedNames(s) {
		s = strings.Replace(s, r, " ", 1)
	}
	return s
}

func (p *Project) isEnvName(n string) bool {
	l := strings.ToLower(n)
	if strings.HasPrefix(l, "env.") {
		return true
	}
	for pre := range p.EnvPrefixes {
		if pre != "" && strings.HasPrefix(n, pre+".") {
			return true
		}
	}
	return false
}

type pathKind int

const (
	pkInside pathKind = iota
	pkBase            // the basedir or project root itself
	pkFSRoot          // "/" or a drive root
	pkOutside
	pkUnresolved
)

func isDriveRoot(s string) bool {
	return len(s) == 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z'))
}

func (p *Project) pathKind(val string, ok bool) pathKind {
	if !ok || strings.Contains(val, "${") || strings.Contains(val, "@{") {
		return pkUnresolved
	}
	s := strings.TrimSpace(strings.ReplaceAll(val, "\\", "/"))
	if s == "" || s == "." || s == "./" {
		return pkBase
	}
	var abs string
	if isAbsPath(s) {
		abs = path.Clean(s)
	} else {
		abs = path.Join(p.Basedir, s)
	}
	if abs == "/" || isDriveRoot(abs) {
		return pkFSRoot
	}
	if strings.EqualFold(abs, p.Basedir) || strings.EqualFold(abs, p.rootN) {
		return pkBase
	}
	root := strings.TrimSuffix(p.rootN, "/") + "/"
	if len(abs) > len(root) && strings.EqualFold(abs[:len(root)], root) {
		return pkInside
	}
	return pkOutside
}

type destOpts struct {
	del     bool
	lenient bool
}

// pathFinding classifies one write/delete destination. nil means proven inside the project.
func (p *Project) pathFinding(raw, val string, ok bool, o destOpts) *finding {
	refs := propRefs(raw)
	switch p.pathKind(val, ok) {
	case pkInside:
		return nil // a server-named property that resolves inside the project is still inside it
	case pkBase:
		if o.del {
			return &finding{ClassDangerous, fmt.Sprintf("deletes the project/base directory itself (%q)", val)}
		}
		return nil
	case pkFSRoot:
		return &finding{ClassDangerous, fmt.Sprintf("targets a filesystem root (%q)", val)}
	case pkOutside:
		return &finding{ClassDangerous, fmt.Sprintf("destination %q is outside the project tree", val)}
	}
	left := unresolvedNames(val)
	var leftNames []string
	for _, l := range left {
		if after, ok0 := strings.CutPrefix(l, "${"); ok0 {
			leftNames = append(leftNames, strings.TrimSuffix(after, "}"))
		}
	}
	for _, n := range refs {
		if hasServerToken(n) {
			return &finding{ClassDangerous, fmt.Sprintf("destination %q depends on app-server/home property ${%s}", raw, n)}
		}
	}
	if hasServerToken(stripRefs(val)) {
		return &finding{ClassDangerous, fmt.Sprintf("destination %q has an app-server/deploy path component", val)}
	}
	var envs []string
	for _, n := range leftNames {
		if p.isEnvName(n) {
			envs = append(envs, "${"+n+"}")
		}
	}
	if len(envs) > 0 {
		return &finding{ClassReview, fmt.Sprintf("destination %q depends on environment variable %s", raw, strings.Join(envs, ", "))}
	}
	// An unresolved destination is never SAFE, even when the property name looks like a build
	// directory: "${build.dir}/../../x" or a build.dir defined elsewhere could point anywhere.
	return &finding{ClassReview, fmt.Sprintf("destination %q could not be resolved (unresolved: %s)", raw, strings.Join(left, ", "))}
}

type spec struct {
	dests   []string
	nested  bool // nested fileset/dirset/filelist dir= are write destinations
	del     bool
	lenient bool
}

var safeTasks = map[string]spec{
	"javac": {dests: []string{"destdir"}, lenient: true}, "javadoc": {dests: []string{"destdir"}, lenient: true},
	"jar": {dests: []string{"destfile", "jarfile"}, lenient: true}, "war": {dests: []string{"destfile", "warfile"}, lenient: true},
	"ear": {dests: []string{"destfile", "earfile"}, lenient: true}, "zip": {dests: []string{"destfile", "zipfile"}, lenient: true},
	"tar": {dests: []string{"destfile", "tarfile"}, lenient: true}, "gzip": {dests: []string{"destfile", "zipfile"}, lenient: true},
	"bzip2": {dests: []string{"destfile", "zipfile"}, lenient: true}, "ejbjar": {dests: []string{"destdir"}, lenient: true},
	"mkdir": {dests: []string{"dir"}}, "copy": {dests: []string{"todir", "tofile"}}, "move": {dests: []string{"todir", "tofile"}},
	"copyfile": {dests: []string{"dest"}}, "copydir": {dests: []string{"dest"}},
	"delete": {dests: []string{"file", "dir"}, nested: true, del: true},
	"echo":   {dests: []string{"file"}}, "touch": {dests: []string{"file"}, nested: true}, "concat": {dests: []string{"destfile"}},
	"replace": {dests: []string{"file", "dir"}, nested: true}, "replaceregexp": {dests: []string{"file"}, nested: true},
	"propertyfile": {dests: []string{"file"}}, "manifest": {dests: []string{"file"}},
	"unjar": {dests: []string{"dest"}}, "unwar": {dests: []string{"dest"}}, "unzip": {dests: []string{"dest"}},
	"untar": {dests: []string{"dest"}}, "gunzip": {dests: []string{"dest"}}, "bunzip2": {dests: []string{"dest"}},
	"rmic": {dests: []string{"base"}}, "xslt": {dests: []string{"destdir", "out"}}, "style": {dests: []string{"destdir", "out"}},
	"native2ascii": {dests: []string{"dest"}}, "fixcrlf": {dests: []string{"destdir", "srcdir"}},
	"chmod": {dests: []string{"file", "dir"}, nested: true}, "attrib": {dests: []string{"file"}, nested: true},
	"sync": {dests: []string{"todir"}}, "echoproperties": {dests: []string{"destfile"}}, "buildnumber": {dests: []string{"file"}},
	"checksum": {dests: []string{"todir"}}, "record": {dests: []string{"name"}}, "jspc": {dests: []string{"destdir"}},
	"echoxml": {dests: []string{"file"}},
	// no destination
	"property": {}, "tstamp": {}, "condition": {}, "available": {}, "uptodate": {}, "fail": {}, "loadproperties": {},
	"loadfile": {}, "loadresource": {}, "dirname": {}, "basename": {}, "pathconvert": {}, "length": {}, "xmlproperty": {},
	"sleep": {}, "dependset": {}, "import": {}, "include": {}, "ant": {}, "antcall": {}, "antcallback": {}, "runtarget": {},
	"subant": {}, "macrodef": {}, "presetdef": {}, "var": {}, "propertycopy": {}, "propertyregex": {}, "math": {},
	"outofdate": {}, "assert": {}, "description": {}, "typefound": {}, "makeurl": {}, "whichresource": {}, "mimemail-none": {},
	"exit": {}, "unset": {}, "pathelement": {}, "setproxy": {}, "stopwatch": {}, "nice": {}, "bindtargets": {},
}

var trustedNS = map[string]bool{
	"antlib:org.apache.tools.ant": true, "antlib:net.sf.antcontrib": true, "antlib:org.apache.tools.ant.types": true,
	"antlib:org.apache.tools.ant.taskdefs.optional": true,
}

func appServerTaskName(name string) bool {
	if strings.Contains(name, "deploy") {
		return true
	}
	switch name {
	case "wsadmin", "asadmin", "wlconfig", "wlrun", "wlstop":
		return true
	}
	for _, pre := range []string{"jboss", "glassfish", "weblogic", "websphere", "wl", "jonas", "tomcat", "catalina"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

func exeBase(exe string) string {
	exe = strings.TrimSpace(exe)
	if i := strings.LastIndexAny(exe, "/\\"); i >= 0 {
		exe = exe[i+1:]
	}
	exe = strings.ToLower(exe)
	for _, ext := range []string{".exe", ".bat", ".cmd", ".sh", ".com", ".ps1"} {
		exe = strings.TrimSuffix(exe, ext)
	}
	return exe
}

var shellExe = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "csh": true, "tcsh": true, "ksh": true, "dash": true, "fish": true,
	"cmd": true, "powershell": true, "pwsh": true, "command": true, "busybox": true, "env": true, "sudo": true,
	"su": true, "runas": true, "start": true, "ssh": true, "scp": true, "sftp": true, "ftp": true, "telnet": true,
	"rsh": true, "rlogin": true, "nc": true, "ncat": true, "curl": true, "wget": true,
}

var deployExe = map[string]bool{
	"asadmin": true, "wsadmin": true, "jboss-cli": true, "jboss-admin": true, "wlst": true, "deployer": true,
	"appcmd": true, "msdeploy": true, "catalina": true, "startup": true, "shutdown": true,
}

var javaExe = map[string]bool{"java": true, "javaw": true, "javac": true, "javadoc": true, "jar": true, "keytool": true, "jarsigner": true}

func (p *Project) nestedSets(t *Task) []*Task {
	var out []*Task
	for _, c := range t.Children {
		switch strings.ToLower(c.Name) {
		case "fileset", "dirset", "filelist", "files", "zipfileset":
			if _, ok := c.Raw["dir"]; ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func wildAll(c *Task) bool {
	inc, _ := patterns(c)
	if len(inc) == 0 {
		return true
	}
	for _, p := range inc {
		switch strings.ReplaceAll(p, "\\", "/") {
		case "**", "**/*", "*", "**/*.*", "*.*", "**/**":
			return true
		}
	}
	return false
}

type pathItem struct {
	raw, val string
	ok       bool
}

// classpathItems gathers the path elements a taskdef/typedef loads classes from, following
// classpathref/refid to <path id=...> definitions.
func (p *Project) classpathItems(t *Task) (items []pathItem, missing bool) {
	add := func(raw, val string, ok bool) {
		if !ok {
			items = append(items, pathItem{raw, val, false})
			return
		}
		for _, el := range splitPathList(val) {
			items = append(items, pathItem{raw, el, true})
		}
	}
	var visit func(x *Task, depth int)
	visit = func(x *Task, depth int) {
		if depth > 6 {
			return
		}
		for _, a := range []string{"refid", "classpathref"} {
			if id := x.Attrs[a]; id != "" {
				if ref := p.paths[id]; ref != nil {
					visit(ref, depth+1)
				} else {
					missing = true
				}
			}
		}
		for _, a := range []string{"path", "location", "classpath"} {
			if val, raw, present, ok := x.Value(a); present {
				add(raw, val, ok)
			}
		}
		switch strings.ToLower(x.Name) {
		case "fileset", "dirset", "filelist":
			if val, raw, present, ok := x.Value("dir"); present {
				add(raw, val, ok)
			}
		}
		for _, c := range x.Children {
			switch strings.ToLower(c.Name) {
			case "pathelement", "classpath", "path", "fileset", "dirset", "filelist", "zipfileset":
				visit(c, depth+1)
			}
		}
	}
	visit(t, 0)
	return items, missing
}

func (p *Project) taskFindings(t *Task) []finding {
	if !t.TaskPos || len(t.Expansion) > 0 {
		return nil
	}
	name := strings.ToLower(t.Name)
	if containers[name] || dataElems[name] {
		return nil
	}
	var out []finding
	add := func(c Class, format string, args ...any) {
		out = append(out, finding{c, fmt.Sprintf(format, args...)})
	}
	ns := strings.ToLower(t.NS)
	if ns != "" && !trustedNS[ns] {
		switch {
		case hasServerToken(ns) && strongServerToken(ns), appServerTaskName(name):
			add(ClassDangerous, "task from application-server namespace %q", t.NS)
		default:
			add(ClassReview, "custom task <%s> from namespace %q", t.Name, t.NS)
		}
		return out
	}
	if appServerTaskName(name) {
		add(ClassDangerous, "application-server/deployment task")
		return out
	}
	switch name {
	case "exec", "apply":
		out = append(out, p.execFindings(t)...)
		return out
	case "java":
		cls, craw, _, _ := t.Value("classname")
		jar, jraw, _, _ := t.Value("jar")
		joined := strings.ToLower(cls + " " + jar + " " + craw + " " + jraw)
		if strongServerToken(joined) {
			add(ClassDangerous, "starts a JVM program that looks like an application-server/deployment tool (classname=%q jar=%q)", craw, jraw)
		} else {
			add(ClassReview, "runs a JVM program (classname=%q jar=%q)", craw, jraw)
		}
		return out
	case "sql":
		add(ClassDangerous, "sql task connects to a database")
		return out
	case "ftp", "scp", "sshexec", "sshsession", "telnet", "rexec", "mail", "mimemail", "rlogin", "sftp", "ssh", "rsh":
		add(ClassDangerous, "%s task talks to a remote machine", name)
		return out
	case "get":
		add(ClassReview, "get downloads from a remote URL")
		return out
	case "input":
		add(ClassReview, "input waits for an interactive prompt")
		return out
	case "junit", "junitlauncher", "testng", "junitreport-none":
		add(ClassReview, "%s executes project code (tests)", name)
		return out
	case "script", "groovy", "jython", "beanshell", "scriptdef", "javascript":
		add(ClassReview, "embedded %s code runs inside Ant", name)
		return out
	case "taskdef", "typedef", "componentdef":
		return p.defFindings(t)
	}
	if strings.HasPrefix(name, "cvs") || strings.HasPrefix(name, "p4") || strings.HasPrefix(name, "vss") ||
		strings.HasPrefix(name, "clearcase") || name == "svn" || name == "git" {
		add(ClassReview, "%s is a version-control task that may contact a remote repository", name)
		return out
	}
	sp, known := safeTasks[name]
	if !known {
		add(ClassReview, "unknown or custom task <%s>", t.Name)
		return out
	}
	for _, a := range sp.dests {
		val, raw, present, ok := t.Value(a)
		if !present {
			continue
		}
		if f := p.pathFinding(raw, val, ok, destOpts{del: sp.del, lenient: sp.lenient}); f != nil {
			out = append(out, finding{f.class, name + " " + a + ": " + f.detail})
		}
	}
	if sp.nested {
		for _, c := range p.nestedSets(t) {
			val, raw, _, ok := c.Value("dir")
			del := sp.del && wildAll(c)
			if f := p.pathFinding(raw, val, ok, destOpts{del: del, lenient: sp.lenient}); f != nil {
				out = append(out, finding{f.class, name + " <" + c.Name + " dir>: " + f.detail})
			}
		}
	}
	return out
}

func (p *Project) defFindings(t *Task) []finding {
	cls, craw, _, _ := t.Value("classname")
	res, rraw, _, _ := t.Value("resource")
	lc := strings.ToLower(cls)
	if strings.HasPrefix(lc, "org.apache.tools.ant.") || strings.HasPrefix(strings.ToLower(res), "org/apache/tools/ant/") {
		return nil
	}
	items, missing := p.classpathItems(t)
	if _, _, present, _ := t.Value("classpathref"); present && len(items) == 0 {
		missing = true
	}
	inside := len(items) > 0 && !missing
	var bad []string
	for _, it := range items {
		if k := p.pathKind(it.val, it.ok); k != pkInside && k != pkBase {
			inside = false
			bad = append(bad, it.raw)
		}
	}
	if inside {
		return nil
	}
	what := craw
	if what == "" {
		what = rraw
	}
	if what == "" {
		what = t.Raw["file"]
	}
	detail := fmt.Sprintf("%s defines custom classes (%q) not provably loaded from a jar inside the project", strings.ToLower(t.Name), what)
	if len(bad) > 0 {
		detail += "; classpath outside or unresolved: " + strings.Join(bad, ", ")
	} else if len(items) == 0 {
		detail += "; no classpath"
	}
	return []finding{{ClassReview, detail}}
}

var deployWords = map[string]bool{"deploy": true, "undeploy": true, "redeploy": true}

func (p *Project) execFindings(t *Task) []finding {
	var out []finding
	exe, raw, present, ok := t.Value("executable")
	if !present {
		if c, craw, pc, cok := t.Value("command"); pc {
			if f := strings.Fields(c); len(f) > 0 {
				exe, raw, present, ok = f[0], strings.Fields(craw)[0], true, cok
			}
		}
	}
	add := func(c Class, format string, args ...any) { out = append(out, finding{c, fmt.Sprintf(format, args...)}) }
	switch {
	case !present:
		add(ClassReview, "%s without a resolvable executable", strings.ToLower(t.Name))
	case !ok:
		var strong, env []string
		for _, n := range propRefs(raw) {
			if strongServerToken(n) {
				strong = append(strong, "${"+n+"}")
			}
			if p.isEnvName(n) {
				env = append(env, "${"+n+"}")
			}
		}
		switch {
		case len(strong) > 0:
			add(ClassDangerous, "executable %q depends on app-server property %s", raw, strings.Join(strong, ", "))
		case len(env) > 0:
			add(ClassReview, "executable %q depends on environment variable %s", raw, strings.Join(env, ", "))
		default:
			add(ClassReview, "executable %q could not be resolved", raw)
		}
	default:
		b := exeBase(exe)
		switch {
		case shellExe[b]:
			add(ClassDangerous, "runs shell/remote tool %q", exe)
		case deployExe[b] || strongServerToken(b):
			add(ClassDangerous, "runs deployment/app-server tool %q", exe)
		case javaExe[b]:
			add(ClassReview, "runs the JDK tool %q (executes a JVM program)", exe)
		default:
			add(ClassReview, "runs external program %q", exe)
		}
	}
	var argText strings.Builder
	argText.WriteString(t.Attrs["args"])
	for _, c := range t.Children {
		if strings.EqualFold(c.Name, "arg") {
			for _, a := range []string{"value", "line", "file", "path"} {
				argText.WriteString(" " + c.Attrs[a])
			}
		}
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(argText.String()), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	}) {
		if deployWords[w] {
			add(ClassDangerous, "arguments invoke a deployment (%q)", w)
			break
		}
	}
	for _, a := range []string{"output", "error"} {
		if val, praw, pr, pok := t.Value(a); pr {
			if f := p.pathFinding(praw, val, pok, destOpts{}); f != nil {
				out = append(out, finding{f.class, a + " redirect: " + f.detail})
			}
		}
	}
	return out
}

func (p *Project) buildInfo(tasks []*Task) *targetInfo {
	info := &targetInfo{}
	walkTasks(tasks, func(t *Task) {
		for _, f := range p.taskFindings(t) {
			info.found = append(info.found, foundTask{t: t, finding: f})
		}
		if t.TaskPos && len(t.Expansion) == 0 {
			switch strings.ToLower(t.Name) {
			case "javac":
				info.javac++
			case "jar", "war", "ear", "ejbjar":
				info.pack++
			}
		}
	})
	return info
}

func (p *Project) targetInfoFor(t *Target) *targetInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if in, ok := p.cache[t]; ok {
		return in
	}
	in := p.buildInfo(t.Tasks)
	p.cache[t] = in
	return in
}

func (p *Project) topInfo() *targetInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.top == nil {
		p.top = p.buildInfo(p.TopTasks)
	}
	return p.top
}

// ---- closure ------------------------------------------------------------------------------------

type issue struct {
	id, from string
	kind     string // missing | unresolved
	detail   string
	loc      Location
	owner    *Project
}

type walkResult struct {
	order  []string
	parent map[string]string
	issues []issue
}

func (p *Project) qualify(owner *Project, name string) string {
	if owner == p {
		return name
	}
	return owner.File + "#" + name
}

func (p *Project) resolveNode(id string) (*Project, *Target) {
	if i := strings.LastIndex(id, "#"); i >= 0 {
		sp := p.Subs[id[:i]]
		if sp == nil {
			return nil, nil
		}
		return sp, sp.Targets[id[i+1:]]
	}
	return p, p.Targets[id]
}

func (p *Project) lookupName(name, prefix string) string {
	if prefix != "" {
		if _, ok := p.Targets[prefix+"."+name]; ok {
			return prefix + "." + name
		}
	}
	if _, ok := p.Targets[name]; ok {
		return name
	}
	return ""
}

func (p *Project) closureWalk(start string) walkResult {
	r := walkResult{parent: map[string]string{}}
	seen := map[string]bool{}
	active := map[string]bool{} // targets on the current recursion path (cycle detection)
	entered := map[*Project]bool{}
	var visit func(id, from string)
	follow := func(owner *Project, prefix string, e Edge, from string) {
		if !e.Resolved {
			r.issues = append(r.issues, issue{id: e.Kind + " " + e.Raw, from: from, kind: "unresolved", detail: e.Reason, loc: e.Loc, owner: owner})
			return
		}
		var nid string
		switch {
		case e.Foreign != "" && e.Foreign != p.File:
			nid = e.Foreign + "#" + e.Name
		case e.Foreign != "":
			nid = e.Name
		default:
			k := owner.lookupName(e.Name, prefix)
			if k == "" {
				k = e.Name
			}
			nid = p.qualify(owner, k)
		}
		if active[nid] && e.Kind == "depends" {
			// Ant rejects cyclic depends graphs at runtime, so such a target can never be executed.
			r.issues = append(r.issues, issue{id: nid, from: from, kind: "cycle", loc: e.Loc, owner: owner})
			return
		}
		if seen[nid] {
			return
		}
		if _, t := p.resolveNode(nid); t == nil {
			seen[nid] = true
			r.issues = append(r.issues, issue{id: nid, from: from, kind: "missing", loc: e.Loc, owner: owner})
			return
		}
		visit(nid, from)
	}
	visit = func(id, from string) {
		if seen[id] {
			return
		}
		seen[id] = true
		owner, tgt := p.resolveNode(id)
		if tgt == nil {
			r.issues = append(r.issues, issue{id: id, from: from, kind: "missing", owner: p})
			return
		}
		r.order = append(r.order, id)
		r.parent[id] = from
		active[id] = true
		defer delete(active, id)
		if !entered[owner] {
			entered[owner] = true
			for _, e := range owner.TopEdges {
				follow(owner, "", e, id)
			}
		}
		for _, e := range tgt.Edges {
			follow(owner, tgt.Prefix, e, id)
		}
	}
	visit(start, "")
	return r
}

// Closure returns target and every target reachable through depends, antcall/antcallback/foreach
// and ant/subant edges, cycle-safe, in traversal order. Targets in another build file are named
// "<build file>#<target>". Names that are not defined are omitted (Classify reports them).
func Closure(proj *Project, target string) []string {
	return proj.closureWalk(target).order
}

func chainTo(id string, parent map[string]string) []string {
	var rev []string
	for cur := id; cur != "" && len(rev) < 500; cur = parent[cur] {
		rev = append(rev, cur)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Classify statically classifies target (plain name, or "<file>#<name>"). Project-level tasks of
// every build file entered join the closure, because they run before any target.
func (p *Project) Classify(target string) TargetSafety {
	ts := TargetSafety{Target: target, Class: ClassSafe, Default: target != "" && target == p.Default}
	w := p.closureWalk(target)
	ts.Closure = w.order
	if _, t := p.resolveNode(target); t != nil {
		ts.Loc = t.Loc
	}
	var reasons []Reason
	entered := map[*Project]bool{}
	for _, id := range w.order {
		owner, tgt := p.resolveNode(id)
		chain := chainTo(id, w.parent)
		if !entered[owner] {
			entered[owner] = true
			for _, f := range owner.topInfo().found {
				reasons = append(reasons, Reason{Class: f.class, Task: f.t.Name, Loc: f.t.Loc, Detail: f.detail, Path: chain, Macro: f.t.ViaMacro, CallLoc: f.t.CallLoc, Scope: "project-level"})
			}
		}
		info := owner.targetInfoFor(tgt)
		ts.Javac += info.javac
		ts.Packaging += info.pack
		for _, f := range info.found {
			reasons = append(reasons, Reason{Class: f.class, Task: f.t.Name, Loc: f.t.Loc, Detail: f.detail, Path: chain, Macro: f.t.ViaMacro, CallLoc: f.t.CallLoc})
		}
	}
	for _, is := range w.issues {
		var r Reason
		switch is.kind {
		case "cycle":
			r = Reason{Class: ClassUnknown, Detail: fmt.Sprintf("dependency cycle: %q depends back on %q; Ant rejects cyclic target graphs, so this target cannot run as declared", is.from, is.id), Loc: is.loc, Path: chainTo(is.from, w.parent)}
		case "missing":
			if is.from == "" {
				r = Reason{Class: ClassUnknown, Detail: fmt.Sprintf("target %q is not defined in the loaded build files", is.id), Loc: Location{File: p.File}}
			} else {
				d := fmt.Sprintf("target %q (needed by %q) is not defined in the loaded build files", is.id, is.from)
				if is.owner != nil && len(is.owner.Unresolved) > 0 {
					var raws []string
					for i, u := range is.owner.Unresolved {
						if i == 3 {
							raws = append(raws, "...")
							break
						}
						raws = append(raws, u.Raw)
					}
					d += fmt.Sprintf("; %d unresolved import(s) could define it (%s)", len(is.owner.Unresolved), strings.Join(raws, ", "))
				}
				r = Reason{Class: ClassUnknown, Detail: d, Loc: is.loc, Path: chainTo(is.from, w.parent)}
			}
		default:
			r = Reason{Class: ClassUnknown, Detail: fmt.Sprintf("%s cannot be followed: %s", is.id, is.detail), Loc: is.loc, Path: chainTo(is.from, w.parent)}
		}
		reasons = append(reasons, r)
	}
	sort.SliceStable(reasons, func(i, j int) bool { return reasons[i].Class.rank() > reasons[j].Class.rank() })
	for _, r := range reasons {
		if r.Class.rank() > ts.Class.rank() {
			ts.Class = r.Class
		}
	}
	ts.Reasons = reasons
	return ts
}

// ClassifyAll classifies every canonical target of this build file (imported targets included).
func (p *Project) ClassifyAll() []TargetSafety {
	out := make([]TargetSafety, 0, len(p.TargetOrder))
	for _, n := range p.TargetOrder {
		out = append(out, p.Classify(n))
	}
	return out
}

// Candidates returns the targets whose closure is SAFE and contains at least one javac or
// jar/war/ear/ejbjar task, best first: the default target, then most javac tasks, then most
// packaging tasks, then the smaller closure. The name only breaks exact ties deterministically.
func Candidates(all []TargetSafety) []TargetSafety {
	var out []TargetSafety
	for _, ts := range all {
		if ts.Class == ClassSafe && ts.Javac+ts.Packaging > 0 {
			out = append(out, ts)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Default != b.Default:
			return a.Default
		case a.Javac != b.Javac:
			return a.Javac > b.Javac
		case a.Packaging != b.Packaging:
			return a.Packaging > b.Packaging
		case len(a.Closure) != len(b.Closure):
			return len(a.Closure) < len(b.Closure)
		}
		return a.Target < b.Target
	})
	return out
}
