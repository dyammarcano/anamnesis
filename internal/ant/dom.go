package ant

import (
	"bufio"
	"encoding/xml"
	"io"
	"strings"
	"unicode/utf8"
)

// element is a minimal DOM node built from the streaming xml.Decoder. Build files are small, so a
// DOM is the simplest way to keep document order, line numbers and nesting together.
type element struct {
	Space    string // namespace URI (e.g. antlib:org.apache.tools.ant), "" for plain Ant
	Name     string // local name
	Attrs    []attr
	Line     int
	Text     string
	Children []*element
}

type attr struct{ Name, Value string }

func (e *element) get(name string) (string, bool) {
	for _, a := range e.Attrs {
		if a.Name == name {
			return a.Value, true
		}
	}
	return "", false
}

func (e *element) attr(name string) string {
	v, _ := e.get(name)
	return v
}

const maxTextPerElement = 64 * 1024

// parseXML tokenizes r with a lenient decoder. It returns whatever tree was built before a syntax
// error, so a damaged file still yields partial facts; err reports the damage.
func parseXML(r io.Reader) (root *element, directives []string, err error) {
	br := bufio.NewReaderSize(r, 64*1024)
	if b, _ := br.Peek(3); len(b) == 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	dec := xml.NewDecoder(br)
	dec.Strict = false
	dec.CharsetReader = charsetReader
	var stack []*element
	for {
		// The position before reading a token is where that token starts: character data is its own
		// token, so this is the line of the '<'.
		line, _ := dec.InputPos()
		tok, e := dec.Token()
		if e != nil {
			if e != io.EOF {
				err = e
			}
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			el := &element{Space: t.Name.Space, Name: t.Name.Local, Line: line}
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				el.Attrs = append(el.Attrs, attr{Name: strings.ToLower(a.Name.Local), Value: a.Value})
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, el)
			} else if root == nil {
				root = el
			}
			stack = append(stack, el)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if len(top.Text) < maxTextPerElement {
					top.Text += string(t)
				}
			}
		case xml.Directive:
			directives = append(directives, string(t))
		}
	}
	return root, directives, err
}

// charsetReader accepts any declared encoding: UTF-8/ASCII pass through, every other single-byte
// encoding is approximated as Latin-1 (build files only need ASCII-range markup to be understood).
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	}
	return &latin1Reader{r: in}, nil
}

type latin1Reader struct {
	r    io.Reader
	buf  []byte
	pend []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	if len(l.pend) == 0 {
		if l.buf == nil {
			l.buf = make([]byte, 4096)
		}
		k, err := l.r.Read(l.buf)
		for _, b := range l.buf[:k] {
			l.pend = utf8.AppendRune(l.pend, rune(b))
		}
		if k == 0 {
			return 0, err
		}
	}
	n := copy(p, l.pend)
	l.pend = l.pend[n:]
	return n, nil
}
