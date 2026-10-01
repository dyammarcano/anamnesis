// Package deps inventories jar/war/ear archives from their central directories only and reports
// duplicate and clearly old libraries. Archives are never extracted.
package deps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

const (
	maxLocs       = 50
	maxDuplicates = 300
)

type analyzer struct{}

// Analyzers returns the deps analyzer.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "deps" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

// art is one library occurrence: a top-level archive or a jar nested in a war/ear.
type art struct {
	Path    string // repo-relative archive that contains it
	Detail  string // entry name inside Path for nested jars
	Keys    []string
	Name    string
	Version string
	Source  string
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	var files []scan.File
	for _, e := range []string{".jar", ".war", ".ear"} {
		files = append(files, p.Files.WithExt(e)...)
	}
	var (
		mu       sync.Mutex
		archives []archive
	)
	scan.ForEach(ctx, files, func(f scan.File) {
		a := readArchive(p.Files.Abs(f), f)
		mu.Lock()
		archives = append(archives, a)
		mu.Unlock()
	})
	sort.Slice(archives, func(i, j int) bool { return archives[i].Path < archives[j].Path })

	arts := buildArts(archives)
	dupTotal, dupEmitted := emitDuplicates(sink, arts)
	emitOld(sink, arts)
	emitInventory(sink, p, archives, dupTotal, dupEmitted)
	emitDeployables(sink, archives)
	return ctx.Err()
}

func buildArts(archives []archive) []art {
	var out []art
	for _, a := range archives {
		out = append(out, art{Path: a.Path, Keys: a.keys, Name: a.Name, Version: a.Version, Source: a.Source})
		for _, n := range a.NestedJars {
			stem := stemOf(path.Base(n))
			fn, fv := splitNameVersion(stem)
			src := "filename"
			if fv == "" {
				src = "none"
			}
			keys := []string{normKey(fn)}
			if s := normKey(stem); s != keys[0] {
				keys = append(keys, s)
			}
			out = append(out, art{Path: a.Path, Detail: n, Keys: keys, Name: fn, Version: fv, Source: src})
		}
	}
	return out
}

func (a art) key() string {
	if len(a.Keys) > 0 {
		return a.Keys[0]
	}
	return normKey(a.Name)
}

func sortedPaths(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func locsOf(paths []string) ([]model.Location, int) {
	total := len(paths)
	if total > maxLocs {
		paths = paths[:maxLocs]
	}
	out := make([]model.Location, len(paths))
	for i, p := range paths {
		out[i] = model.Location{Path: p}
	}
	return out, total
}

type dupGroup struct {
	key      string
	versions map[string]int
	paths    map[string]bool
}

func emitDuplicates(sink engine.Sink, arts []art) (total, emitted int) {
	groups := map[string]*dupGroup{}
	for _, a := range arts {
		k := a.key()
		if k == "" {
			continue
		}
		g := groups[k]
		if g == nil {
			g = &dupGroup{key: k, versions: map[string]int{}, paths: map[string]bool{}}
			groups[k] = g
		}
		v := a.Version
		if v == "" {
			v = "unknown"
		}
		g.versions[v]++
		g.paths[a.Path] = true
	}
	var dups []*dupGroup
	for _, g := range groups {
		if len(g.versions) > 1 || len(g.paths) > 1 {
			dups = append(dups, g)
		}
	}
	sort.Slice(dups, func(i, j int) bool {
		a, b := dups[i], dups[j]
		if len(a.versions) != len(b.versions) {
			return len(a.versions) > len(b.versions)
		}
		if len(a.paths) != len(b.paths) {
			return len(a.paths) > len(b.paths)
		}
		return a.key < b.key
	})
	total = len(dups)
	if len(dups) > maxDuplicates {
		dups = dups[:maxDuplicates]
	}
	for _, g := range dups {
		vs := make([]string, 0, len(g.versions))
		for v := range g.versions {
			vs = append(vs, v)
		}
		sort.Strings(vs)
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = fmt.Sprintf("%s (%d)", v, g.versions[v])
		}
		locs, n := locsOf(sortedPaths(g.paths))
		status, sev := model.Info, model.SevInfo
		why := fmt.Sprintf("present in %d location(s)", n)
		if len(g.versions) > 1 {
			status, sev = model.Warn, model.SevLow
			why = fmt.Sprintf("%d versions in %d location(s)", len(g.versions), n)
		}
		sink.Emit(model.Evidence{
			Category: model.CatDependency, Kind: model.KindDepsDuplicate, Subject: g.key,
			Finding: fmt.Sprintf("%s: %s: %s", g.key, why, strings.Join(parts, ", ")),
			Status:  status, Severity: sev, Confidence: model.Derived,
			Locations: locs,
			Values: map[string]string{
				"versions": strings.Join(parts, ", "), "version_count": fmt.Sprint(len(g.versions)),
				"locations_total": fmt.Sprint(n),
			},
			Limitations: []string{"artifacts are grouped by derived name; unrelated libraries that share a file name are reported together, and a nested jar's version comes from its file name only"},
		})
	}
	return total, len(dups)
}

type oldGroup struct {
	subject, version, reason string
	paths                    map[string]bool
	copies                   int
	sources                  map[string]bool
}

func emitOld(sink engine.Sink, arts []art) {
	groups := map[string]*oldGroup{}
	for _, a := range arts {
		subject, reason, ok := checkOld(a.Keys, a.Version)
		if !ok {
			continue
		}
		k := subject + "\x00" + a.Version
		g := groups[k]
		if g == nil {
			g = &oldGroup{subject: subject, version: a.Version, reason: reason, paths: map[string]bool{}, sources: map[string]bool{}}
			groups[k] = g
		}
		g.paths[a.Path] = true
		g.copies++
		g.sources[a.Source] = true
	}
	list := make([]*oldGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].subject != list[j].subject {
			return list[i].subject < list[j].subject
		}
		return list[i].version < list[j].version
	})
	for _, g := range list {
		locs, n := locsOf(sortedPaths(g.paths))
		ver := g.version
		if ver == "" {
			ver = "unknown"
		}
		sum := sha256.Sum256([]byte("deps|old|" + g.subject + "|" + ver + "|" + locs[0].Path))
		sink.Emit(model.Evidence{
			ID:       hex.EncodeToString(sum[:])[:16],
			Category: model.CatDependency, Kind: model.KindDepsOld, Subject: g.subject,
			Finding: fmt.Sprintf("%s %s: %s", g.subject, ver, g.reason),
			Status:  model.Warn, Severity: model.SevLow, Confidence: model.Derived,
			Locations: locs,
			Values: map[string]string{
				"version": ver, "reason": g.reason, "copies": fmt.Sprint(g.copies),
				"version_source": strings.Join(sortedPaths(g.sources), ","), "locations_total": fmt.Sprint(n),
			},
			Limitations: []string{"derived from artifact name and version only by a fixed table; the archive content was not inspected for the vulnerable code, and no advisory id is asserted"},
		})
	}
}

type invEntry struct {
	archive
	NestedCount int `json:"nestedJarCount"`
}

func emitInventory(sink engine.Sink, p *engine.Project, archives []archive, dupTotal, dupEmitted int) {
	var jars, wars, ears, withVer, unreadable, nested int
	var bytes int64
	var bad []string
	maxMajor := 0
	for _, a := range archives {
		switch a.Kind {
		case "jar":
			jars++
		case "war":
			wars++
		case "ear":
			ears++
		}
		bytes += a.Size
		if a.Version != "" {
			withVer++
		}
		if a.Err != "" {
			unreadable++
			if len(bad) < 5 {
				bad = append(bad, a.Path)
			}
		}
		nested += len(a.NestedJars)
		if a.ClassMajor > maxMajor {
			maxMajor = a.ClassMajor
		}
	}
	vals := map[string]string{
		"jars": fmt.Sprint(jars), "wars": fmt.Sprint(wars), "ears": fmt.Sprint(ears),
		"total_bytes": fmt.Sprint(bytes), "with_version_info": fmt.Sprint(withVer),
		"unknown_version": fmt.Sprint(len(archives) - withVer), "unreadable": fmt.Sprint(unreadable),
		"nested_jars": fmt.Sprint(nested), "duplicates_total": fmt.Sprint(dupTotal), "duplicates_emitted": fmt.Sprint(dupEmitted),
	}
	if maxMajor > 0 {
		vals["max_first_class_major"] = fmt.Sprint(maxMajor)
	}
	lim := []string{
		"only the zip central directory, MANIFEST.MF, pom.properties and the first class header of each archive are read; nothing is extracted",
		"with_version_info counts top-level archives whose version came from pom.properties, manifest or file name; nested jars are used for duplicate and old-library checks by file name only",
	}
	if unreadable > 0 {
		lim = append(lim, fmt.Sprintf("%d archive(s) could not be read as zip (for example %s); their names come from the file name", unreadable, strings.Join(bad, ", ")))
	}
	ev := model.Evidence{
		Category: model.CatDependency, Kind: model.KindDepsInventory, Subject: "archives",
		Finding: fmt.Sprintf("%d jar(s), %d war(s), %d ear(s), %d bytes; %d with a derived version, %d without; %d nested jar(s) listed",
			jars, wars, ears, bytes, withVer, len(archives)-withVer, nested),
		Status: model.Info, Confidence: model.Observed, Values: vals, Limitations: lim,
	}
	if len(archives) > 0 && p.RunDir != "" {
		if rel, err := writeInventory(p, archives); err == nil {
			ev.Artifacts = []string{rel}
		} else {
			ev.Limitations = append(ev.Limitations, "per-archive inventory file was not written: "+err.Error())
		}
	}
	sink.Emit(ev)
}

func writeInventory(p *engine.Project, archives []archive) (string, error) {
	dir := p.CapabilityDir("deps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	entries := make([]invEntry, len(archives))
	for i, a := range archives {
		entries[i] = invEntry{archive: a, NestedCount: len(a.NestedJars)}
	}
	b, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(full, b, 0o644); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(p.RunDir, full)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

var buildDirs = map[string]bool{
	"dist": true, "build": true, "target": true, "out": true, "output": true, "release": true,
	"deploy": true, "deployment": true, "bin": true, "publish": true, "artifacts": true, "staging": true,
}

func isDeployCandidate(rel string) bool {
	segs := strings.Split(strings.ToLower(rel), "/")
	if len(segs) == 1 {
		return true
	}
	for _, s := range segs[:len(segs)-1] {
		if buildDirs[s] {
			return true
		}
	}
	return false
}

func emitDeployables(sink engine.Sink, archives []archive) {
	var cands []string
	wars, ears, others := 0, 0, 0
	for _, a := range archives {
		if a.Kind != "war" && a.Kind != "ear" {
			continue
		}
		if a.Kind == "war" {
			wars++
		} else {
			ears++
		}
		if isDeployCandidate(a.Path) {
			cands = append(cands, a.Path)
		} else {
			others++
		}
	}
	if wars+ears == 0 {
		return
	}
	locs, n := locsOf(cands)
	sink.Emit(model.Evidence{
		Category: model.CatDependency, Kind: model.KindDepsInventory, Subject: "deployable-archives",
		Finding: fmt.Sprintf("%d WAR/EAR archive(s) at the project top level or under build-output style directories are candidates for binary (MTA) analysis; %d other WAR/EAR archive(s) elsewhere", n, others),
		Status:  model.Info, Confidence: model.Derived, Locations: locs,
		Values: map[string]string{
			"candidates": fmt.Sprint(n), "other_archives": fmt.Sprint(others), "wars": fmt.Sprint(wars), "ears": fmt.Sprint(ears),
			"locations_total": fmt.Sprint(n),
		},
		Limitations: []string{"classified by path only (top level, or a directory named dist/build/target/out/output/release/deploy/bin/publish/artifacts/staging); an archive there may be stale or third-party"},
	})
}
