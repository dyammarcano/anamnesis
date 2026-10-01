//go:build windows

// Package winpath makes globally installed tools visible to Anamnesis even when the shell that started
// it predates the installation: Windows keeps the authoritative user and machine PATH in the
// registry, and a running process only sees the copy it inherited.
package winpath

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Refresh appends to this process's PATH every registry PATH entry (machine, then user) that is
// missing from it. It never removes or reorders entries and changes nothing outside the process.
// It returns the directories it added.
func Refresh() []string {
	var added []string
	have := map[string]bool{}
	cur := os.Getenv("PATH")
	for _, d := range filepath.SplitList(cur) {
		have[norm(d)] = true
	}
	for _, src := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		k, err := registry.OpenKey(src.root, src.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := k.GetStringValue("Path")
		_ = k.Close()
		if err != nil {
			continue
		}
		for _, d := range filepath.SplitList(v) {
			d = strings.TrimSpace(os.ExpandEnv(expandPercent(d)))
			if d == "" || have[norm(d)] {
				continue
			}
			have[norm(d)] = true
			added = append(added, d)
		}
	}
	if len(added) > 0 {
		_ = os.Setenv("PATH", strings.TrimRight(cur, string(os.PathListSeparator))+string(os.PathListSeparator)+strings.Join(added, string(os.PathListSeparator)))
	}
	return added
}

func norm(d string) string { return strings.ToLower(filepath.Clean(strings.TrimSpace(d))) }

// expandPercent turns %VAR% references (REG_EXPAND_SZ style) into $VAR for os.ExpandEnv.
func expandPercent(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("${" + s[i+1:i+1+j] + "}")
		s = s[i+2+j:]
	}
}
