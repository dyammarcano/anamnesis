package tech

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

var (
	reTypeDecl   = regexp.MustCompile(`^(?:(?:public|protected|private|abstract|final|static|strictfp|sealed|non-sealed)\s+)*(?:class|interface|enum|record|@interface)\s+[A-Za-z_$]`)
	reAnnotDecl  = regexp.MustCompile(`^@[\w.]+(?:\([^)]*\))?\s+(?:(?:public|protected|private|abstract|final|static|strictfp|sealed)\s+)*(?:class|interface|enum|record)\s+\w`)
	reTaglibURI  = regexp.MustCompile(`uri\s*=\s*["']([^"']+)["']`)
	reImportAttr = regexp.MustCompile(`import\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	reXmlns      = regexp.MustCompile(`xmlns:\w+\s*=\s*["']([^"']+)["']`)
	reDirective  = regexp.MustCompile(`<%@(.*?)%>`)
)

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	return sc
}

// scanJava streams a .java file and reports import names and fully qualified annotation names. It
// stops at the first type declaration, so large sources cost only their header.
func scanJava(r io.Reader, visit func(name string, line int)) {
	sc := newScanner(r)
	inBlock := false
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if inBlock {
			i := strings.Index(s, "*/")
			if i < 0 {
				continue
			}
			s = strings.TrimSpace(s[i+2:])
			inBlock = false
		}
		for strings.HasPrefix(s, "/*") {
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				inBlock = true
				s = ""
				break
			}
			s = strings.TrimSpace(s[2+i+2:])
		}
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		switch {
		case strings.HasPrefix(s, "import "):
			for part := range strings.SplitSeq(s, ";") {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, "import ") {
					if n := importName(part); n != "" {
						visit(n, line)
					}
				}
			}
		case strings.HasPrefix(s, "package "):
		case strings.HasPrefix(s, "@interface"):
			return
		case strings.HasPrefix(s, "@"):
			if n := qualifiedAnnotation(s); n != "" {
				visit(n, line)
			}
			if reAnnotDecl.MatchString(s) {
				return
			}
		default:
			if reTypeDecl.MatchString(s) {
				return
			}
		}
	}
}

func importName(part string) string {
	s := strings.TrimSpace(strings.TrimPrefix(part, "import "))
	s = strings.TrimSpace(strings.TrimPrefix(s, "static "))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.TrimSuffix(s, ".*")
	if !strings.Contains(s, ".") {
		return ""
	}
	return s
}

// qualifiedAnnotation returns a fully qualified annotation name such as javax.ejb.Stateless.
func qualifiedAnnotation(s string) string {
	rest := s[1:]
	end := strings.IndexAny(rest, "( \t")
	if end >= 0 {
		rest = rest[:end]
	}
	if rest == "" || !strings.Contains(rest, ".") || rest[0] < 'a' || rest[0] > 'z' {
		return ""
	}
	return rest
}

// scanJSP streams a JSP and reports page-directive imports and taglib URIs.
func scanJSP(r io.Reader, imp func(string, int), tag func(string, int)) {
	sc := newScanner(r)
	line := 0
	var acc strings.Builder
	accLine, accN := 0, 0
	for sc.Scan() {
		line++
		s := sc.Text()
		if accN > 0 {
			acc.WriteByte(' ')
			acc.WriteString(s)
			accN++
			if strings.Contains(s, "%>") || accN > 20 {
				directive(acc.String(), accLine, imp, tag)
				acc.Reset()
				accN = 0
			}
			continue
		}
		if strings.Contains(s, "<%@") {
			for _, m := range reDirective.FindAllStringSubmatch(s, -1) {
				directive(m[1], line, imp, tag)
			}
			if last := strings.LastIndex(s, "<%@"); last >= 0 && !strings.Contains(s[last:], "%>") {
				acc.Reset()
				acc.WriteString(s[last+3:])
				accLine, accN = line, 1
			}
			continue
		}
		if strings.Contains(s, "jsp:directive.page") {
			directive("page "+s, line, imp, tag)
		}
		if strings.Contains(s, "xmlns:") {
			for _, m := range reXmlns.FindAllStringSubmatch(s, -1) {
				if taglibTech(m[1]) != tagOther {
					tag(m[1], line)
				}
			}
		}
	}
}

func directive(d string, line int, imp, tag func(string, int)) {
	t := strings.TrimSpace(d)
	switch {
	case strings.HasPrefix(t, "taglib"):
		if m := reTaglibURI.FindStringSubmatch(t); m != nil {
			tag(m[1], line)
		}
	case strings.HasPrefix(t, "page"):
		for _, m := range reImportAttr.FindAllStringSubmatch(t, -1) {
			v := m[1]
			if v == "" {
				v = m[2]
			}
			for n := range strings.SplitSeq(v, ",") {
				n = strings.TrimSuffix(strings.TrimSpace(n), ".*")
				if strings.Contains(n, ".") {
					imp(n, line)
				}
			}
		}
	}
}
