package ant

import (
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type kv struct {
	Key, Value string
	Line       int
}

// parseJavaProperties reads java.util.Properties format: key/value separated by '=', ':' or
// whitespace, '#'/'!' comment lines, trailing-backslash continuation and \uXXXX escapes. Input that
// is not valid UTF-8 is read as Latin-1, as java.util.Properties does.
func parseJavaProperties(r io.Reader) ([]kv, error) {
	data, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		return nil, err
	}
	var text string
	if utf8.Valid(data) {
		text = string(data)
	} else {
		rs := make([]rune, len(data))
		for i, b := range data {
			rs[i] = rune(b)
		}
		text = string(rs)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimPrefix(text, string(rune(0xFEFF)))
	lines := strings.Split(text, "\n")
	var out []kv
	for i := 0; i < len(lines); i++ {
		start := i + 1
		line := strings.TrimLeft(lines[i], " \t\f")
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		for oddBackslashes(line) && i+1 < len(lines) {
			line = line[:len(line)-1]
			i++
			line += strings.TrimLeft(lines[i], " \t\f")
		}
		if oddBackslashes(line) {
			line = line[:len(line)-1]
		}
		k, v := splitKV(line)
		out = append(out, kv{Key: unescapeProp(k), Value: unescapeProp(v), Line: start})
	}
	return out, nil
}

func oddBackslashes(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func splitKV(line string) (string, string) {
	i := 0
	for i < len(line) {
		c := line[i]
		if c == '\\' {
			i += 2
			continue
		}
		if c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f' {
			break
		}
		i++
	}
	if i > len(line) {
		i = len(line)
	}
	key := line[:i]
	rest := strings.TrimLeft(line[i:], " \t\f")
	if rest != "" && (rest[0] == '=' || rest[0] == ':') {
		rest = strings.TrimLeft(rest[1:], " \t\f")
	}
	return key, rest
}

func unescapeProp(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+5 <= len(s) {
				if n, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(n))
					i += 4
					continue
				}
			}
			b.WriteByte('u')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
