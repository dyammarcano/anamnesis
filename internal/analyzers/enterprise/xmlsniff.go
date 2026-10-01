package enterprise

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"regexp"
	"strings"
)

// specInfo is what the first XML element (and DOCTYPE) of a descriptor says about its version.
type specInfo struct {
	Root     string // local name of the root element
	Version  string // value of the root's version attribute
	NS       string // root namespace
	PublicID string // DOCTYPE public identifier
	Schema   string // version parsed from an xsi:schemaLocation xsd file name
	OK       bool   // a root element was read
}

var (
	rePublic  = regexp.MustCompile(`PUBLIC\s+"([^"]*)"`)
	reVersion = regexp.MustCompile(`(\d+(?:\.\d+)+)`)
	reXSD     = regexp.MustCompile(`[\w.\-]*?[-_](\d+(?:[._]\d+)*)\.xsd`)
)

// sniffXML streams at most 128 KiB and stops at the first start element.
func sniffXML(path string) specInfo {
	f, err := os.Open(path)
	if err != nil {
		return specInfo{}
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(io.LimitReader(f, 128<<10))
	if b, _ := br.Peek(3); bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	dec := xml.NewDecoder(br)
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var si specInfo
	for {
		tok, err := dec.Token()
		if err != nil {
			return si
		}
		switch t := tok.(type) {
		case xml.Directive:
			if m := rePublic.FindSubmatch([]byte(t)); m != nil {
				si.PublicID = string(m[1])
			}
		case xml.StartElement:
			si.Root = t.Name.Local
			si.NS = t.Name.Space
			si.OK = true
			for _, a := range t.Attr {
				switch {
				case a.Name.Local == "version" && a.Name.Space == "":
					si.Version = a.Value
				case a.Name.Local == "schemaLocation":
					if m := reXSD.FindStringSubmatch(a.Value); m != nil {
						si.Schema = strings.ReplaceAll(m[1], "_", ".")
					}
				}
			}
			return si
		}
	}
}

var specLabels = map[string]string{
	"web.xml":                 "Servlet",
	"ejb-jar.xml":             "EJB",
	"application.xml":         "J2EE/Java EE application",
	"persistence.xml":         "JPA",
	"orm.xml":                 "JPA",
	"faces-config.xml":        "JSF",
	"beans.xml":               "CDI",
	"webservices.xml":         "Web Services",
	"*.tld":                   "JSP taglib",
	"struts-config.xml":       "Struts",
	"applicationContext*.xml": "Spring beans",
}

// formatSpec renders the declared version, always saying where it came from.
func formatSpec(typ string, si specInfo) string {
	if !si.OK {
		return "unreadable"
	}
	label := specLabels[typ]
	switch {
	case si.NS == "http://schemas.xmlsoap.org/wsdl/":
		return "WSDL 1.1"
	case si.NS == "http://www.w3.org/ns/wsdl":
		return "WSDL 2.0"
	case strings.Contains(strings.ToLower(si.NS), "springframework") || strings.Contains(strings.ToUpper(si.PublicID), "SPRING"):
		label = "Spring beans"
	}
	join := func(v, src string) string {
		s := strings.TrimSpace(label + " " + v)
		if src != "" {
			s += " (" + src + ")"
		}
		return s
	}
	if si.Version != "" {
		return join(si.Version, "")
	}
	if m := reVersion.FindString(si.PublicID); m != "" {
		return join(m, "DTD")
	}
	if si.Schema != "" {
		return join(si.Schema, "xsd")
	}
	if si.NS != "" {
		return strings.TrimSpace(label + " namespace " + si.NS)
	}
	return strings.TrimSpace(label + " root=" + si.Root)
}
