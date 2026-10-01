// Package redact masks likely secrets in text before it is stored in evidence or reports.
package redact

import (
	"regexp"
	"strings"
)

// Mask replaces a redacted value.
const Mask = "[REDACTED]"

const keyPat = `(?:pass(?:word|wd)?|pwd|secret|token|api[_-]?key|credential[s]?|auth|private[_-]?key|access[_-]?key)`

var (
	pemRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*(?:PRIVATE KEY|CERTIFICATE|PGP[A-Z ]*BLOCK)-----.*?(?:-----END [A-Z0-9 ]*-----|$)`)
	// Authorization: Bearer xxx / Basic xxx / any scheme.
	authHdrRe = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|basic|token|digest)\s+)?[^\s"',;]+`)
	// scheme://user:pass@host
	urlUserRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s:@]+(?::[^/\s@]*)?@`)
	// key=value or key: value, key may be embedded in a longer identifier (db.password, X_API_KEY).
	kvRe = regexp.MustCompile(`(?i)(["']?[\w.\-]*` + keyPat + `[\w.\-]*["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s"',;&]+)`)
	// XML-style <password>value</password>
	xmlRe = regexp.MustCompile(`(?i)(<[\w.\-]*` + keyPat + `[\w.\-]*>)([^<]+)(</)`)
	// bare long blobs: 40+ hex or 32+ base64-ish chars with mixed content.
	blobRe = regexp.MustCompile(`\b(?:[A-Fa-f0-9]{40,}|[A-Za-z0-9+/_\-]{40,}={0,2})\b`)
)

// Redact masks likely secrets in s. It is conservative toward hiding: false positives are
// acceptable, leaking a value is not.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = pemRe.ReplaceAllString(s, Mask)
	s = authHdrRe.ReplaceAllString(s, "${1}"+Mask)
	s = urlUserRe.ReplaceAllString(s, "${1}"+Mask+"@")
	s = kvRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := kvRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		val := strings.Trim(sub[2], `"'`)
		// Leave unexpanded references and obvious placeholders intact: they are not secrets.
		if strings.HasPrefix(val, "${") || strings.HasPrefix(val, "%") || val == Mask || val == "" {
			return m
		}
		return sub[1] + Mask
	})
	s = xmlRe.ReplaceAllString(s, "${1}"+Mask+"${3}")
	s = blobRe.ReplaceAllStringFunc(s, func(m string) string {
		var digit, lower, upper bool
		for _, r := range m {
			switch {
			case r >= '0' && r <= '9':
				digit = true
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= 'A' && r <= 'Z':
				upper = true
			}
		}
		// Hex digests: digits + letters. Base64-like: all three classes and few slashes, so
		// lower-case file paths and identifiers are left readable.
		if hexOnly(m) && digit {
			return Mask
		}
		if digit && lower && upper && strings.Count(m, "/") <= 2 {
			return Mask
		}
		return m
	})
	return s
}

// RedactLines applies Redact to each line and truncates lines longer than max (0 = 400).
func RedactLines(lines []string, max int) []string {
	if max <= 0 {
		max = 400
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		l = Redact(strings.TrimRight(l, "\r\n"))
		if len(l) > max {
			l = l[:max] + "..."
		}
		out[i] = l
	}
	return out
}

var secretNameRe = regexp.MustCompile(`(?i)` + keyPat)

// RedactEnvNames returns env-var names only, from KEY=VALUE pairs or bare names, de-duplicated and
// without values. Names that look secret-bearing are kept (the name is not the secret) but any
// accidental "=value" is dropped.
func RedactEnvNames(env []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range env {
		name := kv
		if before, _, ok := strings.Cut(kv, "="); ok {
			name = before
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// LooksSecretName reports whether an identifier suggests a secret-bearing variable.
func LooksSecretName(name string) bool { return secretNameRe.MatchString(name) }

func hexOnly(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
