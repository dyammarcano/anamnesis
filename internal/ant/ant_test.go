package ant_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"anamnesis/internal/analyzers/antfacts"
	"anamnesis/internal/ant"
	"anamnesis/internal/engine"
	"anamnesis/internal/model"
)

// writeTree creates the files (slash-separated relative paths) under a fresh temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadBuild(t *testing.T, files map[string]string) *ant.Project {
	t.Helper()
	root := writeTree(t, files)
	p, err := ant.Load(root, "build.xml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

func project(body string) string {
	return `<?xml version="1.0"?>` + "\n" + `<project name="t" default="main" basedir=".">` + "\n" + body + "\n</project>\n"
}

func newEngineProject(t *testing.T, root string) *engine.Project {
	t.Helper()
	p, err := engine.NewProject(context.Background(), root, filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	return p
}

type collectSink struct {
	mu  sync.Mutex
	evs []model.Evidence
}

func (s *collectSink) Emit(e model.Evidence) {
	e.Finalize()
	s.mu.Lock()
	s.evs = append(s.evs, e)
	s.mu.Unlock()
}

func (s *collectSink) kind(k string) []model.Evidence {
	var out []model.Evidence
	for _, e := range s.evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func TestPropertyFirstDefinitionWins(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		prop  string
		want  string
	}{
		{
			name:  "two value definitions",
			files: map[string]string{"build.xml": project(`<property name="a" value="first"/><property name="a" value="second"/>`)},
			prop:  "a", want: "first",
		},
		{
			name: "property file defined before inline value",
			files: map[string]string{
				"build.xml":    project(`<property file="p.properties"/><property name="a" value="inline"/>`),
				"p.properties": "a=fromfile\n",
			},
			prop: "a", want: "fromfile",
		},
		{
			name: "inline value before property file",
			files: map[string]string{
				"build.xml":    project(`<property name="a" value="inline"/><property file="p.properties"/>`),
				"p.properties": "a=fromfile\n",
			},
			prop: "a", want: "inline",
		},
		{
			name: "first of two property files",
			files: map[string]string{
				"build.xml":      project(`<property file="one.properties"/><property file="two.properties"/>`),
				"one.properties": "a=one\n",
				"two.properties": "a=two\n",
			},
			prop: "a", want: "one",
		},
		{
			name: "imported file defines first when imported first",
			files: map[string]string{
				"build.xml": project(`<import file="p.xml"/><property name="a" value="importer"/>`),
				"p.xml":     `<project name="p"><property name="a" value="imported"/></project>`,
			},
			prop: "a", want: "imported",
		},
		{
			name: "substitution happens at definition time using earlier properties",
			files: map[string]string{
				"build.xml": project(`<property name="x" value="1"/><property name="b" value="v${x}"/><property name="x" value="2"/>`),
			},
			prop: "b", want: "v1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := loadBuild(t, tc.files)
			pr := p.Props[tc.prop]
			if pr == nil {
				t.Fatalf("property %q not defined", tc.prop)
			}
			if pr.Value != tc.want {
				t.Errorf("property %q = %q, want %q", tc.prop, pr.Value, tc.want)
			}
		})
	}
}

func TestPropertyFileContinuationLines(t *testing.T) {
	props := "# a comment line\n" + // 1
		"! another comment\n" + // 2
		"long.value = one,\\\n" + // 3
		"    two,\\\n" + // 4
		"    three\n" + // 5
		"colon.key: colon value\n" + // 6
		"spaced.key   spaced value\n" + // 7
		"escaped\\ key=v\n" + // 8
		"windows.path=C:\\\\dir\\\\sub\n" + // 9
		"unicode=caf\\u00e9\n" + // 10
		"trailing=ends here\\\\\n" + // 11 (escaped backslash is not a continuation)
		"after=ok\n" // 12
	p := loadBuild(t, map[string]string{
		"build.xml":    project(`<property file="p.properties"/>`),
		"p.properties": props,
	})
	tests := []struct {
		key, want string
		line      int
	}{
		{"long.value", "one,two,three", 3},
		{"colon.key", "colon value", 6},
		{"spaced.key", "spaced value", 7},
		{"escaped key", "v", 8},
		{"windows.path", `C:\dir\sub`, 9},
		{"unicode", "caf\u00e9", 10},
		{"trailing", `ends here\`, 11},
		{"after", "ok", 12},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			pr := p.Props[tc.key]
			if pr == nil {
				t.Fatalf("key %q not read; have %v", tc.key, p.PropOrder)
			}
			if pr.Value != tc.want {
				t.Errorf("value = %q, want %q", pr.Value, tc.want)
			}
			if pr.Origin != "file" {
				t.Errorf("origin = %q, want file", pr.Origin)
			}
			if pr.Loc.File != "p.properties" || pr.Loc.Line != tc.line {
				t.Errorf("defined at %s, want p.properties:%d", pr.Loc, tc.line)
			}
		})
	}
	if _, ok := p.Props["# a comment line"]; ok {
		t.Error("comment line was read as a property")
	}
}

func TestImportResolvedThroughPropertyFromImportedFile(t *testing.T) {
	p := loadBuild(t, map[string]string{
		"build.xml":     project(`<import file="props.xml"/>` + "\n" + `<import file="${mods.dir}/more.xml"/>`),
		"props.xml":     `<project name="props"><property name="mods.dir" value="mods"/></project>`,
		"mods/more.xml": `<project name="more"><target name="extra"/></project>`,
	})
	if len(p.Unresolved) != 0 {
		t.Fatalf("unexpected unresolved imports: %+v", p.Unresolved)
	}
	if _, ok := p.Targets["extra"]; !ok {
		t.Errorf("target from the property-resolved import is missing; targets=%v imports=%v", p.TargetOrder, p.Imports)
	}
	want := map[string]bool{"props.xml": false, "mods/more.xml": false}
	for _, i := range p.Imports {
		if _, ok := want[i]; ok {
			want[i] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("import %s not followed; imports=%v", f, p.Imports)
		}
	}
}

func TestJavacTargetResolvedThroughImportedPropertyFile(t *testing.T) {
	// the property lives on line 3 of an imported file
	root := writeTree(t, map[string]string{
		"build.xml": project(`<import file="mods/build-properties.xml"/>
<target name="main"><javac srcdir="src" destdir="build" target="${java.target.version}"/></target>`),
		"mods/build-properties.xml": "<project name=\"props\">\n\n  <property name=\"java.target.version\" value=\"1.6\"/>\n</project>\n",
	})
	p := newEngineProject(t, root)
	sink := &collectSink{}
	if err := antfacts.Analyzers()[0].Analyze(context.Background(), p, sink); err != nil {
		t.Fatal(err)
	}
	targets := sink.kind(model.KindJavaTarget)
	if len(targets) != 1 {
		t.Fatalf("want exactly one java.target evidence, got %d: %+v", len(targets), targets)
	}
	e := targets[0]
	if got := e.Values["resolved"]; got != "1.6" {
		t.Errorf("resolved = %q, want 1.6", got)
	}
	if e.Confidence != model.Derived {
		t.Errorf("confidence = %s, want DERIVED (value came through property substitution)", e.Confidence)
	}
	if got := e.Values["raw"]; got != "${java.target.version}" {
		t.Errorf("raw = %q, want the unsubstituted reference", got)
	}
	if got := e.Values["defined_at"]; got != "mods/build-properties.xml:3" {
		t.Errorf("defined_at = %q, want mods/build-properties.xml:3", got)
	}
	// source level is absent on that javac: the analyzer must say so rather than invent one
	if len(sink.kind(model.KindJavaSource)) != 0 {
		t.Error("a java.source evidence was invented for a javac without a source attribute")
	}
	impl := sink.kind(model.KindJavaImplicit)
	if len(impl) != 1 || impl[0].Values["no_source"] != "1" || impl[0].Values["no_target"] != "0" {
		t.Errorf("java.javac.implicit evidence wrong: %+v", impl)
	}
}

func TestLiteralJavacTargetIsObservedNotDerived(t *testing.T) {
	root := writeTree(t, map[string]string{
		"build.xml": project(`<target name="main"><javac srcdir="src" destdir="build" source="1.5" target="1.5"/></target>`),
	})
	sink := &collectSink{}
	if err := antfacts.Analyzers()[0].Analyze(context.Background(), newEngineProject(t, root), sink); err != nil {
		t.Fatal(err)
	}
	tg := sink.kind(model.KindJavaTarget)
	if len(tg) != 1 || tg[0].Confidence != model.Observed || tg[0].Values["resolved"] != "1.5" {
		t.Fatalf("literal target should be OBSERVED 1.5, got %+v", tg)
	}
}

func TestUnresolvedDynamicImportIsReportedNotDropped(t *testing.T) {
	root := writeTree(t, map[string]string{
		"build.xml": project(`<import file="bin/${x}.xml" optional="true"/>
<target name="main"/>`),
	})
	p, err := ant.Load(root, "build.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Unresolved) != 1 {
		t.Fatalf("want 1 unresolved import, got %+v", p.Unresolved)
	}
	u := p.Unresolved[0]
	if u.Raw != "bin/${x}.xml" || !u.Optional || u.Kind != "import" || u.Loc.File != "build.xml" || u.Loc.Line == 0 {
		t.Errorf("unresolved import record wrong: %+v", u)
	}
	if !strings.Contains(u.Reason, "${x}") {
		t.Errorf("reason should name the missing property, got %q", u.Reason)
	}

	sink := &collectSink{}
	if err := antfacts.Analyzers()[0].Analyze(context.Background(), newEngineProject(t, root), sink); err != nil {
		t.Fatal(err)
	}
	evs := sink.kind(model.KindAntImportUnresolved)
	if len(evs) != 1 {
		t.Fatalf("want 1 ant.import.unresolved evidence, got %d", len(evs))
	}
	if evs[0].Values["raw"] != "bin/${x}.xml" {
		t.Errorf("evidence raw = %q", evs[0].Values["raw"])
	}
	if len(evs[0].Limitations) == 0 {
		t.Error("an unresolved import must carry a limitation explaining what is not visible")
	}
}

func TestUnresolvedImportVariants(t *testing.T) {
	tests := []struct {
		name       string
		importElem string
		wantRaw    string
		wantReason string
	}{
		{"missing file", `<import file="nope.xml"/>`, "nope.xml", "file not found"},
		{"no file attribute", `<import/>`, "", "no file attribute"},
		{"property never defined", `<import file="${nowhere}/a.xml"/>`, "${nowhere}/a.xml", "unresolved property"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := loadBuild(t, map[string]string{"build.xml": project(tc.importElem + `<target name="main"/>`)})
			if len(p.Unresolved) != 1 {
				t.Fatalf("want 1 unresolved, got %+v", p.Unresolved)
			}
			if p.Unresolved[0].Raw != tc.wantRaw || !strings.Contains(p.Unresolved[0].Reason, tc.wantReason) {
				t.Errorf("got %+v, want raw %q reason containing %q", p.Unresolved[0], tc.wantRaw, tc.wantReason)
			}
		})
	}
}

func TestImportCycleDoesNotHang(t *testing.T) {
	done := make(chan *ant.Project, 1)
	root := writeTree(t, map[string]string{
		"build.xml": project(`<import file="a.xml"/><target name="main"/>`),
		"a.xml":     `<project name="a"><import file="b.xml"/><target name="ta"/></project>`,
		"b.xml":     `<project name="b"><import file="a.xml"/><target name="tb"/></project>`,
	})
	go func() {
		p, err := ant.Load(root, "build.xml")
		if err != nil {
			t.Error(err)
		}
		done <- p
	}()
	select {
	case p := <-done:
		if p != nil {
			for _, want := range []string{"main", "ta", "tb"} {
				if _, ok := p.Targets[want]; !ok {
					t.Errorf("target %s missing after import cycle", want)
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load hung on an import cycle")
	}
}

const (
	pureBuild = `<javac srcdir="src" destdir="${build.dir}/classes"/><jar destfile="${build.dir}/out.jar" basedir="${build.dir}/classes"/>`
	dbTask    = `<sql driver="x.Driver" url="jdbc:x:y" userid="u" password="p">select 1</sql>`
)

func classOf(t *testing.T, body, target string) ant.TargetSafety {
	t.Helper()
	p := loadBuild(t, map[string]string{"build.xml": project(body)})
	return p.Classify(target)
}

func TestSafetyClassification(t *testing.T) {
	tests := []struct {
		name string
		body string
		// target to classify
		target string
		want   []ant.Class // any of
		javac  int
		pack   int
	}{
		{
			name: "dependency on a target that execs an app-server binary is DANGEROUS",
			body: `<target name="main" depends="helper"><echo message="hi"/></target>
<target name="helper"><exec executable="${appserver.home}/bin/x"/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name:   "exec of app-server property directly is DANGEROUS",
			body:   `<target name="main"><exec executable="${appserver.home}/bin/x"/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name:   "sql task is DANGEROUS",
			body:   `<target name="main">` + dbTask + `</target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name:   "sql reached through antcall is DANGEROUS",
			body:   `<target name="main"><antcall target="db"/></target><target name="db">` + dbTask + `</target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name: "delete of an environment-variable directory is not SAFE",
			body: `<property environment="env"/>
<target name="main"><delete dir="${env.FOO}"/></target>`,
			target: "main", want: []ant.Class{ant.ClassReview, ant.ClassDangerous, ant.ClassUnknown},
		},
		{
			name:   "delete of a filesystem root is DANGEROUS",
			body:   `<target name="main"><delete dir="/"/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name:   "delete of a directory outside the project is DANGEROUS",
			body:   `<target name="main"><delete dir="../../elsewhere"/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name: "javac and jar into build.dir under basedir is SAFE",
			body: `<property name="build.dir" location="build"/>
<target name="main">` + pureBuild + `</target>`,
			target: "main", want: []ant.Class{ant.ClassSafe}, javac: 1, pack: 1,
		},
		{
			name: "mkdir delete copy inside build.dir are SAFE",
			body: `<property name="build.dir" location="build"/>
<target name="main"><mkdir dir="${build.dir}"/><copy todir="${build.dir}/c"><fileset dir="src"/></copy><delete dir="${build.dir}"/></target>`,
			target: "main", want: []ant.Class{ant.ClassSafe},
		},
		{
			name:   "undefined target reference is UNKNOWN, not SAFE",
			body:   `<target name="main" depends="ghost"/>`,
			target: "main", want: []ant.Class{ant.ClassUnknown},
		},
		{
			name:   "unknown custom task is REVIEW_REQUIRED",
			body:   `<target name="main"><frobnicate x="1"/></target>`,
			target: "main", want: []ant.Class{ant.ClassReview},
		},
		{
			name:   "java task is REVIEW_REQUIRED",
			body:   `<target name="main"><java classname="a.B"/></target>`,
			target: "main", want: []ant.Class{ant.ClassReview},
		},
		{
			name:   "deployment-named task is DANGEROUS",
			body:   `<target name="main"><jboss-deploy/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
		{
			name:   "project-level exec runs before every target and taints a harmless target",
			body:   `<exec executable="${jboss.home}/bin/run.sh"/><target name="main"><echo message="x"/></target>`,
			target: "main", want: []ant.Class{ant.ClassDangerous},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := classOf(t, tc.body, tc.target)
			ok := false
			for _, w := range tc.want {
				if ts.Class == w {
					ok = true
				}
			}
			if !ok {
				t.Errorf("class = %s, want one of %v; reasons=%v", ts.Class, tc.want, ts.Reasons)
			}
			if tc.want[0] == ant.ClassSafe {
				if ts.Javac != tc.javac || ts.Packaging != tc.pack {
					t.Errorf("javac=%d packaging=%d, want %d/%d", ts.Javac, ts.Packaging, tc.javac, tc.pack)
				}
			} else if len(ts.Reasons) == 0 {
				t.Error("a non-SAFE class must carry at least one reason")
			}
		})
	}
}

func TestSafetyDoesNotUseTargetNames(t *testing.T) {
	names := []string{"build", "deploy", "install", "compile", "all", "zzz-neutral", "clean"}
	contents := []struct {
		name string
		body string
		want ant.Class
	}{
		{"pure build", pureBuild, ant.ClassSafe},
		{"database", dbTask, ant.ClassDangerous},
		{"app-server exec", `<exec executable="${appserver.home}/bin/x"/>`, ant.ClassDangerous},
	}
	for _, c := range contents {
		t.Run(c.name, func(t *testing.T) {
			classes := map[string]ant.Class{}
			for _, n := range names {
				body := `<property name="build.dir" location="build"/><target name="` + n + `">` + c.body + `</target>`
				ts := classOf(t, body, n)
				classes[n] = ts.Class
				if ts.Class != c.want {
					t.Errorf("target named %q with identical content is %s, want %s (names must not matter)", n, ts.Class, c.want)
				}
			}
			first := classes[names[0]]
			for n, cl := range classes {
				if cl != first {
					t.Errorf("class depends on name: %q=%s but %q=%s", names[0], first, n, cl)
				}
			}
		})
	}
}

func TestDependsCycleDoesNotHang(t *testing.T) {
	bodies := map[string]string{
		"depends cycle":   `<target name="a" depends="b"/><target name="b" depends="a"><javac srcdir="s" destdir="build"/></target>`,
		"self depends":    `<target name="a" depends="a"/>`,
		"antcall cycle":   `<target name="a"><antcall target="b"/></target><target name="b"><antcall target="a"/></target>`,
		"three-way cycle": `<target name="a" depends="b"/><target name="b" depends="c"/><target name="c" depends="a"/>`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			p := loadBuild(t, map[string]string{"build.xml": project(body)})
			done := make(chan struct{})
			go func() {
				defer close(done)
				all := p.ClassifyAll()
				if len(all) == 0 {
					t.Error("no targets classified")
				}
				if cl := ant.Closure(p, "a"); len(cl) == 0 {
					t.Error("empty closure for target a")
				}
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("classification hung on a cyclic target graph")
			}
		})
	}
}

func TestMalformedXMLYieldsErrorOrWarningNotPanic(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"not xml at all", "this is not xml\n"},
		{"empty file", ""},
		{"truncated mid-tag", `<project name="x" default="a"><target name="a"><javac srcdir="s" `},
		{"mismatched tags", `<project name="x"><target name="a"></project></target>`},
		{"wrong root element", `<html><body/></html>`},
		{"binary garbage", "\x00\x01\x02\xff\xfe<<<>>>"},
		{"unterminated property ref", `<project name="x"><target name="a"><echo message="${oops"/></target></project>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"build.xml": tc.content})
			p := newEngineProject(t, root)

			// the low-level loader
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Load panicked: %v", r)
					}
				}()
				ap, err := ant.Load(root, "build.xml")
				if err == nil && ap != nil {
					_ = ap.ClassifyAll() // partial trees must remain classifiable
				}
			}()

			// the shared loader: damage must surface as a LoadError or a warning, never vanish
			var projects []*ant.Project
			var errs []ant.LoadError
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("LoadAll panicked: %v", r)
					}
				}()
				projects, errs = ant.LoadAll(p)
			}()
			damaged := len(errs) > 0
			for _, ap := range projects {
				if len(ap.Warnings) > 0 {
					damaged = true
				}
			}
			if !damaged && tc.name != "unterminated property ref" {
				t.Errorf("damaged build file produced neither a LoadError nor a warning (projects=%d)", len(projects))
			}

			// and the analyzer turns it into evidence rather than failing
			sink := &collectSink{}
			if err := antfacts.Analyzers()[0].Analyze(context.Background(), p, sink); err != nil {
				t.Errorf("ant-facts returned an error instead of evidence: %v", err)
			}
			if len(sink.evs) == 0 {
				t.Error("no evidence at all for a damaged build file")
			}
		})
	}
}

func TestChooseTarget(t *testing.T) {
	const buildProps = `<property name="build.dir" location="build"/>`
	tests := []struct {
		name       string
		files      map[string]string
		wantOK     bool
		wantTarget string
		reasonHas  []string
	}{
		{
			name: "SAFE default target with javac is chosen",
			files: map[string]string{"build.xml": `<project name="p" default="main" basedir=".">` + buildProps +
				`<target name="other"><javac srcdir="s" destdir="${build.dir}"/><javac srcdir="t" destdir="${build.dir}"/></target>` +
				`<target name="main"><javac srcdir="s" destdir="${build.dir}"/></target></project>`},
			wantOK: true, wantTarget: "main", reasonHas: []string{"default target"},
		},
		{
			name: "DANGEROUS default, a different SAFE javac target is chosen",
			files: map[string]string{"build.xml": `<project name="p" default="main" basedir=".">` + buildProps +
				`<target name="main"><exec executable="${appserver.home}/bin/x"/></target>` +
				`<target name="compile"><javac srcdir="s" destdir="${build.dir}"/></target></project>`},
			wantOK: true, wantTarget: "compile",
		},
		{
			name: "DANGEROUS default and the only SAFE target has no javac",
			files: map[string]string{"build.xml": `<project name="p" default="main" basedir=".">` + buildProps +
				`<target name="main"><exec executable="${appserver.home}/bin/x"/></target>` +
				`<target name="clean"><delete dir="${build.dir}"/></target></project>`},
			wantOK: false, reasonHas: []string{"main", "DANGEROUS"},
		},
		{
			name: "DANGEROUS default via dependency chain and nothing else",
			files: map[string]string{"build.xml": `<project name="p" default="build" basedir=".">` + buildProps +
				`<target name="build" depends="detect"><javac srcdir="s" destdir="${build.dir}"/></target>` +
				`<target name="detect"><fail message="no server"/><exec executable="${jboss.home}/bin/probe"/></target></project>`},
			wantOK: false, reasonHas: []string{"build"},
		},
		{
			name:      "no build.xml at root",
			files:     map[string]string{"sub/build.xml": project(`<target name="main"/>`)},
			wantOK:    false,
			reasonHas: []string{"no root build.xml"},
		},
		{
			name:      "root build.xml unparsable",
			files:     map[string]string{"build.xml": "not xml"},
			wantOK:    false,
			reasonHas: []string{"could not be parsed"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, tc.files)
			file, target, reason, ok := ant.ChooseTarget(newEngineProject(t, root))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (file=%q target=%q reason=%q), want %v", ok, file, target, reason, tc.wantOK)
			}
			if reason == "" {
				t.Error("reason must always be set, also when ok=false")
			}
			if ok {
				if file != "build.xml" || target != tc.wantTarget {
					t.Errorf("chose %s#%s, want build.xml#%s", file, target, tc.wantTarget)
				}
			} else if file != "" || target != "" {
				t.Errorf("ok=false but file=%q target=%q were returned", file, target)
			}
			for _, s := range tc.reasonHas {
				if !strings.Contains(reason, s) {
					t.Errorf("reason %q does not contain %q", reason, s)
				}
			}
		})
	}
}

func TestChooseTargetNeverPicksNonSafeEvenIfItBuilds(t *testing.T) {
	root := writeTree(t, map[string]string{"build.xml": `<project name="p" default="main" basedir=".">` +
		`<target name="main"><javac srcdir="s" destdir="build"/><sql driver="d" url="u">x</sql></target>` +
		`<target name="deploy"><javac srcdir="s" destdir="build"/><exec executable="${appserver.home}/bin/deploy"/></target></project>`})
	_, target, reason, ok := ant.ChooseTarget(newEngineProject(t, root))
	if ok {
		t.Fatalf("picked %q although every target is DANGEROUS", target)
	}
	if reason == "" {
		t.Error("missing reason")
	}
}
