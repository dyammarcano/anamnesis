package enterprise

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

type family struct {
	name       string
	pathTokens []string
	jarTokens  []string
}

var families = []family{
	{"jboss", []string{"jboss", "wildfly"}, []string{"jboss", "wildfly"}},
	{"weblogic", []string{"weblogic"}, []string{"weblogic", "wlfullclient", "wlclient"}},
	{"websphere", []string{"websphere"}, []string{"websphere", "com.ibm.ws", "was_runtime"}},
	{"glassfish", []string{"glassfish", "sunas"}, []string{"glassfish", "appserv-"}},
	{"geronimo", []string{"geronimo"}, []string{"geronimo"}},
	{"tomcat", []string{"tomcat", "catalina"}, []string{"tomcat", "catalina"}},
}

// envTokenFamily maps lower-cased variable / property names to a family. "" means the name does not
// identify a family.
var envTokenFamily = map[string]string{
	"jboss_home": "jboss", "jboss.home": "jboss", "wildfly_home": "jboss", "wildfly.home": "jboss",
	"wl_home": "weblogic", "wl.home": "weblogic", "weblogic_home": "weblogic", "weblogic.home": "weblogic", "bea_home": "weblogic",
	"was_home": "websphere", "was.home": "websphere", "websphere_home": "websphere",
	"as_home": "glassfish", "as.home": "glassfish", "glassfish_home": "glassfish", "glassfish.home": "glassfish",
	"catalina_home": "tomcat", "catalina.home": "tomcat", "catalina_base": "tomcat", "catalina.base": "tomcat",
	"tomcat_home": "tomcat", "tomcat.home": "tomcat",
	"geronimo_home": "geronimo", "geronimo.home": "geronimo",
	"appsrv_home": "", "appsrv.home": "",
}

var envRe = buildEnvRe()

func buildEnvRe() *regexp.Regexp {
	toks := make([]string, 0, len(envTokenFamily))
	for k := range envTokenFamily {
		toks = append(toks, regexp.QuoteMeta(k))
	}
	sort.Strings(toks)
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(toks, "|") + `)\b`)
}

var envExt = map[string]bool{
	".properties": true, ".xml": true, ".sh": true, ".bat": true, ".cmd": true, ".ps1": true,
	".conf": true, ".cfg": true, ".ini": true, ".env": true, ".gradle": true,
}

type envHit struct {
	tok string
	fam string
	loc model.Location
}

func displayEnv(tok string) string {
	if strings.Contains(tok, "_") {
		return strings.ToUpper(tok)
	}
	return tok
}

func homeHint(b []byte) bool {
	return bytes.Contains(b, []byte("ome")) || bytes.Contains(b, []byte("OME")) ||
		bytes.Contains(b, []byte("ase")) || bytes.Contains(b, []byte("ASE"))
}

func scanEnvRefs(ctx context.Context, p *engine.Project) []envHit {
	var files []scan.File
	for _, f := range p.Files.Files {
		if envExt[f.Ext] && f.Size > 0 && f.Size <= 4<<20 {
			files = append(files, f)
		}
	}
	var mu sync.Mutex
	var hits []envHit
	scan.ForEach(ctx, files, func(f scan.File) {
		fh, err := os.Open(p.Files.Abs(f))
		if err != nil {
			return
		}
		defer func() { _ = fh.Close() }()
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		seen := map[string]bool{}
		var local []envHit
		line := 0
		for sc.Scan() {
			line++
			b := sc.Bytes()
			if !homeHint(b) {
				continue
			}
			for _, m := range envRe.FindAll(b, -1) {
				tok := strings.ToLower(string(m))
				if seen[tok] {
					continue
				}
				seen[tok] = true
				local = append(local, envHit{tok: tok, fam: envTokenFamily[tok], loc: model.Location{Path: f.Rel, Line: line}})
			}
		}
		if len(local) > 0 {
			mu.Lock()
			hits = append(hits, local...)
			mu.Unlock()
		}
	})
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.loc.Path != b.loc.Path {
			return a.loc.Path < b.loc.Path
		}
		if a.loc.Line != b.loc.Line {
			return a.loc.Line < b.loc.Line
		}
		return a.tok < b.tok
	})
	return hits
}

type serverState struct {
	family    string
	descCount int
	descTypes map[string]int
	pathFiles int
	jars      int
	envRefs   map[string]int
	locsDesc  []model.Location
	locsEnv   []model.Location
	locsJar   []model.Location
	locsPath  []model.Location
}

func addLoc(dst *[]model.Location, l model.Location) {
	if len(*dst) < 200 {
		*dst = append(*dst, l)
	}
}

func containsAny(s string, toks []string) bool {
	for _, t := range toks {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func descFamily(typ, root string) string {
	switch {
	case strings.HasPrefix(typ, "jboss"), typ == "*-ds.xml":
		return "jboss"
	case strings.HasPrefix(typ, "weblogic"):
		return "weblogic"
	case strings.HasPrefix(typ, "ibm-"):
		return "websphere"
	case strings.HasPrefix(typ, "sun-"), strings.HasPrefix(typ, "glassfish-"):
		return "glassfish"
	case strings.HasPrefix(typ, "geronimo-"):
		return "geronimo"
	case typ == "context.xml (META-INF)" && root == "Context":
		return "tomcat"
	}
	return ""
}

func collectServers(ix *scan.Index, groups map[string][]scan.File, infos map[string]specInfo, hits []envHit) map[string]*serverState {
	states := map[string]*serverState{}
	get := func(name string) *serverState {
		st := states[name]
		if st == nil {
			st = &serverState{family: name, descTypes: map[string]int{}, envRefs: map[string]int{}}
			states[name] = st
		}
		return st
	}
	for _, fam := range families {
		get(fam.name)
	}
	for _, f := range ix.Files {
		if f.Ext == ".java" || f.Ext == ".class" {
			continue
		}
		low := strings.ToLower(f.Rel)
		for _, fam := range families {
			st := states[fam.name]
			if f.Ext == ".jar" && containsAny(f.Base, fam.jarTokens) {
				st.jars++
				addLoc(&st.locsJar, model.Location{Path: f.Rel})
				continue
			}
			if containsAny(low, fam.pathTokens) {
				st.pathFiles++
				addLoc(&st.locsPath, model.Location{Path: f.Rel})
			}
		}
	}
	for _, typ := range sortedTypes(groups) {
		for _, f := range groups[typ] {
			fam := descFamily(typ, infos[f.Rel].Root)
			if fam == "" {
				continue
			}
			st := get(fam)
			st.descCount++
			st.descTypes[typ]++
			addLoc(&st.locsDesc, model.Location{Path: f.Rel})
		}
	}
	for _, h := range hits {
		name := h.fam
		if name == "" {
			name = "unspecified"
		}
		st := get(name)
		st.envRefs[displayEnv(h.tok)]++
		addLoc(&st.locsEnv, h.loc)
	}
	return states
}

func (s *serverState) envTotal() int {
	n := 0
	for _, c := range s.envRefs {
		n += c
	}
	return n
}

func (s *serverState) total() int { return s.descCount + s.pathFiles + s.jars + s.envTotal() }

func joinCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s:%d", k, m[k])
	}
	return strings.Join(parts, ",")
}

func (s *serverState) signals() string {
	var parts []string
	if s.descCount > 0 {
		parts = append(parts, "descriptors="+joinCounts(s.descTypes))
	}
	if s.pathFiles > 0 {
		parts = append(parts, fmt.Sprintf("path_files=%d", s.pathFiles))
	}
	if len(s.envRefs) > 0 {
		parts = append(parts, "env_refs="+joinCounts(s.envRefs))
	}
	if s.jars > 0 {
		parts = append(parts, fmt.Sprintf("jars=%d", s.jars))
	}
	return strings.Join(parts, "; ")
}

func (s *serverState) locations() []model.Location {
	var all []model.Location
	all = append(all, s.locsDesc...)
	all = append(all, s.locsEnv...)
	all = append(all, s.locsJar...)
	all = append(all, s.locsPath...)
	seen := map[model.Location]bool{}
	var out []model.Location
	for _, l := range all {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
		if len(out) == maxLocs {
			break
		}
	}
	return out
}

func emitServers(sink engine.Sink, states map[string]*serverState) {
	names := make([]string, 0, len(states))
	for n := range states {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := states[n]
		if st.total() == 0 {
			continue
		}
		vals := map[string]string{
			"family":          n,
			"signals":         st.signals(),
			"signal_count":    fmt.Sprint(st.total()),
			"locations_total": fmt.Sprint(st.total()),
		}
		finding := fmt.Sprintf("%s application server signals: %s", n, st.signals())
		lim := []string{"attribution is by descriptor, file and path names, jar names and environment-variable names; it does not prove the server is installed or is the deployment target"}
		if n == "jboss" {
			vals["includes"] = "wildfly"
		}
		if n == "unspecified" {
			finding = fmt.Sprintf("app-server home variable referenced (%s); the name does not identify a server family", st.signals())
		}
		sink.Emit(model.Evidence{
			Category: model.CatTechnology, Kind: model.KindEnterpriseAppServer, Subject: n,
			Finding: finding, Status: model.Info, Confidence: model.Derived,
			Locations: st.locations(), Limitations: lim, Values: vals,
		})
	}
}

var coreDescriptors = map[string]bool{
	"web.xml": true, "application.xml": true, "ejb-jar.xml": true, "faces-config.xml": true, "webservices.xml": true,
}

func emitJavaEE(sink engine.Sink, ix *scan.Index, groups map[string][]scan.File, servers map[string]*serverState) {
	var signals []string
	core, vendor := 0, 0
	for _, typ := range sortedTypes(groups) {
		n := len(groups[typ])
		if coreDescriptors[typ] {
			core += n
			signals = append(signals, fmt.Sprintf("descriptor:%s(%d)", typ, n))
		} else if typ == "persistence.xml" || typ == "orm.xml" || typ == "beans.xml" || typ == "context.xml (META-INF)" {
			signals = append(signals, fmt.Sprintf("descriptor:%s(%d)", typ, n))
		}
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		st := servers[n]
		if st.descCount > 0 && n != "tomcat" {
			vendor += st.descCount
			signals = append(signals, fmt.Sprintf("vendor-descriptors:%s(%d)", n, st.descCount))
		}
	}
	jsp := len(ix.WithExt(".jsp")) + len(ix.WithExt(".jspf")) + len(ix.WithExt(".jspx"))
	if jsp > 0 {
		signals = append(signals, fmt.Sprintf("jsp-files(%d)", jsp))
	}
	for _, n := range names {
		if servers[n].total() > 0 {
			signals = append(signals, "appserver-signals:"+n)
		}
	}
	var servlets, ejbs, beans int
	for _, f := range ix.WithExt(".java") {
		switch {
		case strings.HasSuffix(f.Base, "servlet.java"):
			servlets++
		case strings.HasSuffix(f.Base, "ejb.java"):
			ejbs++
		case strings.HasSuffix(f.Base, "bean.java"):
			beans++
		}
	}
	if servlets > 0 {
		signals = append(signals, fmt.Sprintf("filename:*Servlet.java(%d,weak)", servlets))
	}
	if ejbs > 0 {
		signals = append(signals, fmt.Sprintf("filename:*EJB.java(%d,weak)", ejbs))
	}
	if beans > 0 {
		signals = append(signals, fmt.Sprintf("filename:*Bean.java(%d,weak)", beans))
	}
	detected := core > 0 || vendor > 0 || jsp > 0
	sigStr := strings.Join(signals, "; ")
	finding := "no Java EE descriptors, vendor descriptors or JSP files found; Java EE usage is not indicated by file layout"
	if detected {
		finding = "Java EE / J2EE usage is indicated by file layout: " + sigStr
	} else if sigStr != "" {
		finding += " (weaker signals: " + sigStr + ")"
	}
	sink.Emit(model.Evidence{
		Category: model.CatTechnology, Kind: model.KindEnterpriseJavaEE, Subject: "javaee",
		Finding: finding, Status: model.Info, Confidence: model.Inferred,
		Values: map[string]string{"detected": fmt.Sprint(detected), "signals": sigStr},
		Assumptions: []string{
			"web.xml, ejb-jar.xml, application.xml, faces-config.xml, webservices.xml, vendor descriptors or JSP files indicate Java EE; persistence.xml, beans.xml and file-name patterns are listed as signals but alone do not set detected",
		},
		Limitations: []string{
			"inferred from file names and descriptors only; imports and annotations are reported by the tech analyzer",
			"*Servlet.java / *EJB.java / *Bean.java are weak name patterns and are never used to set detected",
		},
	})
}
