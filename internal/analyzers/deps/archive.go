package deps

import (
	"archive/zip"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/scan"
)

// archive is what the central directory (plus three small entries) of one jar/war/ear says.
type archive struct {
	Path       string            `json:"path"`
	Kind       string            `json:"kind"`
	Size       int64             `json:"size"`
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Source     string            `json:"versionSource"` // pom.properties | manifest | filename | none
	Group      string            `json:"group,omitempty"`
	Entries    int               `json:"entries"`
	ClassMajor int               `json:"firstClassMajor,omitempty"`
	NestedJars []string          `json:"nestedJars,omitempty"`
	Modules    []string          `json:"modules,omitempty"`
	Manifest   map[string]string `json:"manifest,omitempty"`
	PomCount   int               `json:"pomPropertiesCount,omitempty"`
	Err        string            `json:"error,omitempty"`
	keys       []string
}

var reNameVersion = regexp.MustCompile(`^(.+?)[-_]v?(\d[\w.\-+]*)$`)

func stemOf(base string) string {
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		return base[:i]
	}
	return base
}

// splitNameVersion splits name-1.2.3 style stems. Version is "" when the stem has no version part.
func splitNameVersion(stem string) (string, string) {
	if m := reNameVersion.FindStringSubmatch(stem); m != nil {
		return m[1], m[2]
	}
	return stem, ""
}

func readLimited(zf *zip.File, max int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, max))
}

func classMajor(zf *zip.File) int {
	rc, err := zf.Open()
	if err != nil {
		return 0
	}
	defer func() { _ = rc.Close() }()
	var b [8]byte
	if _, err := io.ReadFull(rc, b[:]); err != nil {
		return 0
	}
	if b[0] != 0xCA || b[1] != 0xFE || b[2] != 0xBA || b[3] != 0xBE {
		return 0
	}
	return int(b[6])<<8 | int(b[7])
}

var manifestKeys = map[string]string{
	"implementation-title": "Implementation-Title", "implementation-version": "Implementation-Version",
	"implementation-vendor": "Implementation-Vendor", "specification-title": "Specification-Title",
	"specification-version": "Specification-Version", "bundle-symbolicname": "Bundle-SymbolicName",
	"bundle-version": "Bundle-Version", "bundle-name": "Bundle-Name", "main-class": "Main-Class",
	"build-jdk": "Build-Jdk", "created-by": "Created-By",
}

// parseManifest reads the main section only, unfolding continuation lines.
func parseManifest(b []byte) map[string]string {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var lines []string
	for l := range strings.SplitSeq(text, "\n") {
		if l == "" {
			break
		}
		if l[0] == ' ' && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	out := map[string]string{}
	for _, l := range lines {
		i := strings.IndexByte(l, ':')
		if i <= 0 {
			continue
		}
		if canon, ok := manifestKeys[strings.ToLower(l[:i])]; ok {
			out[canon] = strings.TrimSpace(l[i+1:])
		}
	}
	return out
}

func parseProps(b []byte) map[string]string {
	out := map[string]string{}
	for l := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == '!' {
			continue
		}
		i := strings.IndexAny(l, "=:")
		if i <= 0 {
			continue
		}
		out[strings.TrimSpace(l[:i])] = strings.TrimSpace(l[i+1:])
	}
	return out
}

func pickPom(zr *zip.ReadCloser, poms []*zip.File, stem string) map[string]string {
	var parsed []map[string]string
	for _, zf := range poms {
		b, err := readLimited(zf, 16<<10)
		if err != nil {
			continue
		}
		parsed = append(parsed, parseProps(b))
	}
	if len(parsed) == 1 {
		return parsed[0]
	}
	low := strings.ToLower(stem)
	for _, m := range parsed {
		if id := strings.ToLower(m["artifactId"]); id != "" && strings.Contains(low, id) {
			return m
		}
	}
	return nil
}

func manifestNames(man map[string]string) []string {
	var out []string
	if v := man["Bundle-SymbolicName"]; v != "" {
		if i := strings.IndexByte(v, ';'); i >= 0 {
			v = v[:i]
		}
		out = append(out, strings.TrimSpace(v))
	}
	for _, k := range []string{"Implementation-Title", "Specification-Title", "Bundle-Name"} {
		if v := man[k]; v != "" {
			out = append(out, v)
		}
	}
	return out
}

func manifestVersion(man map[string]string) string {
	for _, k := range []string{"Implementation-Version", "Bundle-Version", "Specification-Version"} {
		if v := man[k]; v != "" {
			return v
		}
	}
	return ""
}

func normKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), "-"))
}

// derive fills Name/Version/Source with the precedence pom.properties > manifest > filename.
func derive(a *archive, pom, man map[string]string) {
	stem := stemOf(path.Base(a.Path))
	fn, fv := splitNameVersion(stem)
	mnames := manifestNames(man)
	var keys []string
	add := func(k string) {
		k = normKey(k)
		if k == "" {
			return
		}
		if slices.Contains(keys, k) {
			return
		}
		keys = append(keys, k)
	}
	switch {
	case pom != nil && pom["version"] != "" && pom["artifactId"] != "":
		a.Group, a.Name, a.Version, a.Source = pom["groupId"], pom["artifactId"], pom["version"], "pom.properties"
		add(pom["artifactId"])
	case manifestVersion(man) != "":
		a.Version, a.Source = manifestVersion(man), "manifest"
		a.Name = fn
		if len(mnames) > 0 {
			a.Name = mnames[0]
		}
	case fv != "":
		a.Name, a.Version, a.Source = fn, fv, "filename"
	default:
		a.Name, a.Source = stem, "none"
		if len(mnames) > 0 {
			a.Name = mnames[0]
		}
	}
	add(fn)
	add(stem)
	for _, n := range mnames {
		add(n)
	}
	a.keys = keys
}

func readArchive(abs string, f scan.File) archive {
	a := archive{Path: f.Rel, Kind: strings.TrimPrefix(f.Ext, "."), Size: f.Size}
	zr, err := zip.OpenReader(abs)
	if zr == nil {
		if err != nil {
			a.Err = err.Error()
		} else {
			a.Err = "cannot open archive"
		}
		derive(&a, nil, nil)
		return a
	}
	defer func() { _ = zr.Close() }()
	a.Entries = len(zr.File)
	var manifest *zip.File
	var poms []*zip.File
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, "/") {
			continue
		}
		low := strings.ToLower(zf.Name)
		switch {
		case low == "meta-inf/manifest.mf":
			manifest = zf
		case strings.HasPrefix(low, "meta-inf/maven/") && strings.HasSuffix(low, "/pom.properties"):
			poms = append(poms, zf)
		case strings.HasSuffix(low, ".jar") && (strings.Contains(low, "web-inf/lib/") || strings.HasPrefix(low, "lib/") || strings.Contains(low, "app-inf/lib/")):
			a.NestedJars = append(a.NestedJars, zf.Name)
		case a.Kind == "ear" && !strings.Contains(low, "/") &&
			(strings.HasSuffix(low, ".war") || strings.HasSuffix(low, ".jar") || strings.HasSuffix(low, ".rar") || strings.HasSuffix(low, ".sar")):
			a.Modules = append(a.Modules, zf.Name)
		case a.ClassMajor == 0 && strings.HasSuffix(low, ".class") && !strings.HasPrefix(low, "meta-inf/") && low != "module-info.class":
			a.ClassMajor = classMajor(zf)
		}
	}
	var man map[string]string
	if manifest != nil {
		if b, err := readLimited(manifest, 256<<10); err == nil {
			man = parseManifest(b)
		}
	}
	if len(man) > 0 {
		a.Manifest = man
	}
	a.PomCount = len(poms)
	var pom map[string]string
	if len(poms) > 0 {
		pom = pickPom(zr, poms, stemOf(path.Base(a.Path)))
	}
	derive(&a, pom, man)
	return a
}
