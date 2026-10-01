package javaver

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

type mvnItem struct {
	name string // source | target | release
	raw  string
	line int
	via  string
}

type pluginCtx struct {
	depth    int
	artifact string
	items    []mvnItem
}

type capture struct {
	kind  string // prop | artifact | cfg
	name  string
	depth int
	line  int
	buf   strings.Builder
}

// parsePom extracts project/properties and the maven-compiler-plugin configuration. On a syntax error
// it returns what was gathered before the error together with the error.
func parsePom(data []byte) (props map[string]string, propLine map[string]int, items []mvnItem, err error) {
	props, propLine = map[string]string{}, map[string]int{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var stack []string
	var cur *pluginCtx
	var cp *capture
	for {
		tok, e := dec.Token()
		if e == io.EOF {
			return
		}
		if e != nil {
			err = e
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			d, name := len(stack), t.Name.Local
			line, _ := dec.InputPos()
			if name == "plugin" && cur == nil {
				cur = &pluginCtx{depth: d}
			}
			if cp != nil {
				break
			}
			switch {
			case d == 3 && stack[0] == "project" && stack[1] == "properties":
				cp = &capture{kind: "prop", name: name, depth: d, line: line}
			case cur != nil && d == cur.depth+1 && name == "artifactId":
				cp = &capture{kind: "artifact", name: name, depth: d, line: line}
			case cur != nil && d == cur.depth+2 && stack[cur.depth] == "configuration" &&
				(name == "source" || name == "target" || name == "release"):
				cp = &capture{kind: "cfg", name: name, depth: d, line: line}
			}
		case xml.CharData:
			if cp != nil {
				cp.buf.Write(t)
			}
		case xml.EndElement:
			d := len(stack)
			if cp != nil && d == cp.depth {
				val := strings.TrimSpace(cp.buf.String())
				switch cp.kind {
				case "prop":
					props[cp.name], propLine[cp.name] = val, cp.line
				case "artifact":
					if cur != nil {
						cur.artifact = val
					}
				case "cfg":
					if cur != nil {
						cur.items = append(cur.items, mvnItem{name: cp.name, raw: val, line: cp.line,
							via: "maven-compiler-plugin configuration <" + cp.name + ">"})
					}
				}
				cp = nil
			}
			if cur != nil && d == cur.depth {
				if cur.artifact == "maven-compiler-plugin" {
					items = append(items, cur.items...)
				}
				cur = nil
			}
			if d > 0 {
				stack = stack[:d-1]
			}
		}
	}
}

func mvnKind(name string) string {
	switch name {
	case "source":
		return model.KindJavaSource
	case "target":
		return model.KindJavaTarget
	}
	return model.KindJavaRelease
}

func analyzeMaven(ix *scan.Index, f scan.File, sink engine.Sink) {
	data, err := readCapped(ix.Abs(f))
	if err != nil {
		warnFile(sink, model.KindJavaSource, f.Rel, "could not read pom.xml: "+err.Error())
		return
	}
	props, propLine, items, perr := parsePom(data)
	if perr != nil {
		warnFile(sink, model.KindJavaSource, f.Rel, "pom.xml is not well-formed XML ("+perr.Error()+"); values read before the error are still reported.")
	}
	for _, n := range []string{"source", "target", "release"} {
		key := "maven.compiler." + n
		if raw, ok := props[key]; ok {
			items = append(items, mvnItem{name: n, raw: raw, line: propLine[key], via: "property " + key})
		}
	}
	for _, it := range items {
		v := javaVal{kind: mvnKind(it.name), subject: f.Rel, line: it.line, raw: it.raw, origin: "maven", via: it.via, conf: model.Observed}
		out, ok, changed, missing := substitute(it.raw, props)
		switch {
		case !ok:
			v.conf = model.Unknown
			v.finding = "pom.xml declares " + it.name + " via " + it.via + " as " + it.raw + ", which could not be resolved."
			v.limitation = "property " + missing + " is not defined in this pom's <properties>; it may come from a parent pom, a profile or settings.xml, none of which were read."
		case strings.TrimSpace(out) == "":
			v.conf = model.Unknown
			v.finding = "pom.xml declares " + it.name + " via " + it.via + " with an empty value."
		default:
			v.resolved = out
			if changed {
				v.conf = model.Derived
			}
			v.finding = "pom.xml declares " + it.name + " " + out + " via " + it.via + "."
		}
		emitVal(sink, v)
	}
}
