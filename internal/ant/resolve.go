package ant

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// containers are task elements whose children are themselves tasks.
var containers = map[string]bool{
	"sequential": true, "parallel": true, "try": true, "trycatch": true, "catch": true, "finally": true,
	"if": true, "then": true, "else": true, "elseif": true, "for": true, "foreach": true, "switch": true,
	"case": true, "default": true, "retry": true, "waitfor": true, "limit": true, "daemons": true,
	"forget": true, "parallel-daemons": true, "timestampselector": false,
}

// condContainers hold tasks that may not run at all.
var condContainers = map[string]bool{
	"if": true, "then": true, "else": true, "elseif": true, "switch": true, "case": true, "default": true,
	"trycatch": true, "try": true, "catch": true, "finally": true, "retry": true,
}

// dataElems are types and conditions: never tasks, even when they appear in task position.
var dataElems = map[string]bool{
	"path": true, "classpath": true, "fileset": true, "dirset": true, "filelist": true, "patternset": true,
	"filterset": true, "filterchain": true, "propertyset": true, "mapper": true, "xmlcatalog": true,
	"zipfileset": true, "tarfileset": true, "zipgroupfileset": true, "files": true, "resources": true,
	"pathelement": true, "include": true, "exclude": true, "includesfile": true, "excludesfile": true,
	"arg": true, "env": true, "sysproperty": true, "jvmarg": true, "syspropertyset": true, "assertions": true,
	"filter": true, "attribute": true, "element": true, "text": true, "manifest-attribute": true,
	"section": true, "service": true, "metainf": true, "fileset-ref": true, "substitution": true,
	"regexp": true, "selector": true, "scriptselector": true, "description": true, "redirector": true,
	"compilerarg": true, "bootclasspath": true, "sourcepath": true, "extdirs": true, "src": true,
	"patternset-ref": true, "param": true, "format": true, "formatter": true, "batchtest": true, "test": true,
	// conditions
	"equals": true, "isset": true, "istrue": true, "isfalse": true, "not": true, "and": true, "or": true, "xor": true,
	"os": true, "contains": true, "http": true, "socket": true, "filesmatch": true, "matches": true,
	"isreference": true, "isfileselected": true, "hasmethod": true, "resourcecontains": true, "resourceexists": true,
	"isfailure": true, "length-cond": true, "typefound": true, "issigned": true, "parsersupports": true,
	"isreachable": true, "antversion": true, "javaversion": true,
	// selectors
	"present": true, "date": true, "depend": true, "depth": true, "different": true, "filename": true,
	"majority": true, "modified": true, "none": true, "readable": true, "writable": true, "size": true,
	"type": true, "contains-sel": true, "containsregexp": true, "selectorcontainer": true, "any": true, "all": true,
	// mapper / misc children
	"globmapper": true, "regexpmapper": true, "flattenmapper": true, "identitymapper": true, "compositemapper": true,
	"chainedmapper": true, "filtermapper": true, "mergemapper": true, "unpackagemapper": true, "packagemapper": true,
	"tokenfilter": true, "linecontains": true, "linecontainsregexp": true, "striplinecomments": true,
	"striplinebreaks": true, "expandproperties": true, "replacetokens": true, "headfilter": true, "tailfilter": true,
	"classconstants": true, "escapeunicode": true, "prefixlines": true, "sortfilter": true, "token": true,
	"classfileset": true, "jar-entry": true, "fail-msg": true, "propertyref": true, "mapper-ref": true,
	"commandline": true, "path-ref": true, "target": false,
}

// propDefiners maps a task to the attributes naming a property it may define at run time.
var propDefiners = map[string][]string{
	"condition": {"property"}, "available": {"property"}, "uptodate": {"property"}, "dirname": {"property"},
	"basename": {"property"}, "pathconvert": {"property"}, "loadfile": {"property"}, "makeurl": {"property"},
	"whichresource": {"property"}, "checksum": {"property"}, "length": {"property"}, "input": {"addproperty"},
	"exec": {"outputproperty", "resultproperty", "errorproperty"}, "apply": {"outputproperty", "resultproperty", "errorproperty"},
	"propertyregex": {"property"}, "math": {"result"}, "outofdate": {"property"}, "propertycopy": {"name"},
	"var": {"name"}, "uptodate-cond": {"property"}, "bindtargets": {}, "rootcert": {},
	"scriptdef": {}, "javah": {}, "sql": {"outputproperty"}, "isreachable": {}, "buildnumber": {},
}

// unresolvedNames lists the ${name} / @{name} references still present in s.
func unresolvedNames(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if (s[i] == '$' || s[i] == '@') && i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				out = append(out, s[i:])
				break
			}
			out = append(out, s[i:i+3+end])
			i += 2 + end
		}
	}
	return out
}

// lookup finds a property. found=false: not defined (or conditional, which is the same to a static
// reader); ok=false: defined but its value is not fully known.
func (p *Project) lookup(name string) (val string, ok, found bool) {
	if pr, has := p.Props[name]; has {
		if pr.Conditional {
			return "", false, false
		}
		return pr.Value, !pr.Unresolved, true
	}
	if name == "file.separator" {
		return "/", true, true
	}
	return "", false, false
}

// Resolve substitutes ${property} references the way Ant would at this point of the file. ok is false
// when any reference is undefined, conditional, an ${env.X} reference or a system property; the
// unresolved references are kept verbatim in resolved.
func (p *Project) Resolve(s string) (resolved string, ok bool) {
	if !strings.ContainsAny(s, "$@") {
		return s, true
	}
	var b strings.Builder
	ok = true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) {
			if s[i+1] == '$' {
				b.WriteByte('$')
				i++
				continue
			}
			if s[i+1] == '{' {
				end := strings.IndexByte(s[i+2:], '}')
				if end < 0 {
					b.WriteString(s[i:])
					return b.String(), false
				}
				name := s[i+2 : i+2+end]
				v, vok, found := p.lookup(name)
				if found {
					b.WriteString(v)
					if !vok {
						ok = false
					}
				} else {
					b.WriteString(s[i : i+3+end])
					ok = false
				}
				i += 2 + end
				continue
			}
		}
		if c == '@' && i+1 < len(s) && s[i+1] == '{' {
			ok = false
		}
		b.WriteByte(c)
	}
	return b.String(), ok
}

func (p *Project) addProp(pr *Property) {
	if cur, exists := p.Props[pr.Name]; exists {
		// First definition wins. When the winner is conditional the later literal is a candidate.
		if cur.Conditional && !pr.Conditional {
			cur.Alternatives = append(cur.Alternatives, pr.Value)
		}
		return
	}
	p.Props[pr.Name] = pr
	p.PropOrder = append(p.PropOrder, pr.Name)
}

func (p *Project) setValue(name, raw, origin string, loc, via Location, scoped, cond bool) {
	if name == "" {
		return
	}
	p.rawStrs = append(p.rawStrs, rawStr{raw, loc})
	val, ok := p.Resolve(raw)
	p.addProp(&Property{Name: name, Value: val, Raw: raw, Origin: origin, Loc: loc, Via: via, Unresolved: !ok, Scoped: scoped, Conditional: cond})
}

func (p *Project) setConditional(name, origin string, loc Location, scoped bool) {
	if name == "" {
		return
	}
	if cur, exists := p.Props[name]; exists {
		if origin == "var" {
			cur.Conditional = true // <var> mutates an existing property at run time
		}
		return
	}
	p.addProp(&Property{Name: name, Origin: origin, Loc: loc, Conditional: true, Scoped: scoped})
}

// defineTask applies the property-defining semantics of one task. scoped: the task sits inside a
// target; cond: it sits inside a container that may not run.
func (p *Project) defineTask(t *Task, scoped, cond bool) {
	name := strings.ToLower(t.Name)
	hasCond := t.Raw["if"] != "" || t.Raw["unless"] != ""
	if name == "dirname" && p.defineDirname(t, scoped, cond || hasCond) {
		return
	}
	switch name {
	case "property":
		p.defineProperty(t, scoped, cond || hasCond)
	case "loadproperties":
		if f := t.Raw["srcfile"]; f != "" {
			p.loadPropFile(t, f, t.Raw["prefix"], scoped, cond || hasCond)
		} else {
			p.warn("%s: <loadproperties> without srcFile defines properties that are not visible statically", t.Loc)
		}
	case "tstamp":
		pre := t.Raw["prefix"]
		if pre != "" {
			pre += "."
		}
		for _, n := range []string{"DSTAMP", "TSTAMP", "TODAY"} {
			p.setConditional(pre+n, "tstamp", t.Loc, scoped)
		}
		for _, c := range t.Children {
			if strings.EqualFold(c.Name, "format") {
				if pn := c.Raw["property"]; pn != "" {
					p.setConditional(pre+pn, "tstamp", c.Loc, scoped)
				}
			}
		}
	default:
		attrs, ok := propDefiners[name]
		if !ok {
			return
		}
		for _, a := range attrs {
			if n := t.Raw[a]; n != "" {
				v, _ := p.Resolve(n)
				origin := "condition"
				if name == "var" {
					origin = "var"
				}
				p.setConditional(v, origin, t.Loc, scoped)
			}
		}
	}
}

func (p *Project) defineProperty(t *Task, scoped, cond bool) {
	if env, ok := t.Raw["environment"]; ok && env != "" {
		v, _ := p.Resolve(env)
		v = strings.TrimSuffix(v, ".")
		if _, seen := p.EnvPrefixes[v]; !seen {
			p.EnvPrefixes[v] = t.Loc
		}
		return
	}
	name := t.Raw["name"]
	if name != "" {
		if rn, ok := p.Resolve(name); ok {
			name = rn
		}
	}
	has := func(k string) bool { _, ok := t.Raw[k]; return ok }
	switch {
	case name != "" && has("value"):
		p.setValue(name, t.Raw["value"], "value", t.Loc, Location{}, scoped, cond)
	case name != "" && has("location"):
		raw := t.Raw["location"]
		p.rawStrs = append(p.rawStrs, rawStr{raw, t.Loc})
		val, ok := p.Resolve(raw)
		if ok {
			if isAbsPath(val) {
				val = normAbs(val)
			} else {
				val = path.Clean(path.Join(p.Basedir, filepath.ToSlash(val)))
			}
		}
		p.addProp(&Property{Name: name, Value: val, Raw: raw, Origin: "location", Loc: t.Loc, Unresolved: !ok, Scoped: scoped, Conditional: cond})
	case name != "" && has("refid"):
		p.addProp(&Property{Name: name, Raw: t.Raw["refid"], Origin: "refid", Loc: t.Loc, Conditional: true, Scoped: scoped})
	case name != "" && t.Text != "":
		p.setValue(name, t.Text, "text", t.Loc, Location{}, scoped, cond)
	case has("file"):
		p.loadPropFile(t, t.Raw["file"], t.Raw["prefix"], scoped, cond)
	case has("resource"), has("url"):
		p.warn("%s: <property resource/url> loads properties that are not visible statically", t.Loc)
	}
}

func (p *Project) loadPropFile(t *Task, raw, prefix string, scoped, cond bool) {
	p.rawStrs = append(p.rawStrs, rawStr{raw, t.Loc})
	miss := func(reason string) {
		p.MissingPropertyFiles = append(p.MissingPropertyFiles, UnresolvedImport{Raw: raw, Reason: reason, Loc: t.Loc, Kind: "property-file"})
	}
	res, ok := p.Resolve(raw)
	if !ok {
		miss("unresolved property reference: " + strings.Join(unresolvedNames(res), ", "))
		return
	}
	abs := res
	if !isAbsPath(res) {
		abs = path.Join(p.Basedir, filepath.ToSlash(res))
	}
	abs = filepath.Clean(filepath.FromSlash(abs))
	rel := p.ld.rel(abs)
	if rel == filepath.ToSlash(abs) && isAbsPath(rel) {
		miss("property file lies outside the analyzed project and was not read: " + rel)
		return
	}
	if strings.HasSuffix(strings.ToLower(abs), ".xml") {
		miss("XML property files are not read")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		miss("file not found: " + rel)
		return
	}
	defer func() { _ = f.Close() }()
	entries, err := parseJavaProperties(f)
	if err != nil {
		miss("could not be read: " + err.Error())
		return
	}
	if prefix != "" && !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}
	for _, e := range entries {
		p.setValue(prefix+e.Key, e.Value, "file", Location{File: rel, Line: e.Line}, t.Loc, scoped, cond)
	}
}

// defineScoped records properties defined inside targets; they only exist when that target runs, so
// they are applied after every top-level (parse-time) property.
func (p *Project) defineScoped() {
	var walk func(ts []*Task, cond bool)
	walk = func(ts []*Task, cond bool) {
		for _, t := range ts {
			p.defineTask(t, true, cond)
			walk(t.Children, cond || condContainers[strings.ToLower(t.Name)])
		}
	}
	for _, n := range p.TargetOrder {
		if t := p.Targets[n]; t != nil {
			walk(t.Tasks, false)
		}
	}
}

func (p *Project) resolveTree(t *Task) {
	t.Attrs = make(map[string]string, len(t.Raw))
	t.Unresolved = nil
	for k, v := range t.Raw {
		r, ok := p.Resolve(v)
		t.Attrs[k] = r
		if !ok {
			if t.Unresolved == nil {
				t.Unresolved = map[string]bool{}
			}
			t.Unresolved[k] = true
		}
	}
	for _, c := range t.Children {
		p.resolveTree(c)
	}
}

// defineDirname evaluates <dirname property=x file=...> when the file reference is known, which is
// the usual way builds derive a directory from ${ant.file}.
func (p *Project) defineDirname(t *Task, scoped, cond bool) bool {
	n := t.Raw["property"]
	f, ok := p.Resolve(t.Raw["file"])
	if n == "" || !ok || f == "" {
		return false
	}
	abs := filepath.ToSlash(f)
	if !isAbsPath(abs) {
		abs = path.Join(p.Basedir, abs)
	}
	p.addProp(&Property{Name: n, Value: path.Dir(path.Clean(abs)), Raw: t.Raw["file"], Origin: "dirname", Loc: t.Loc, Scoped: scoped, Conditional: cond})
	return true
}
