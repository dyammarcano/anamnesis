// Package tech reports which Java technologies the sources import. It reads only import, package
// and leading annotation lines of .java files and the page directives of JSP files.
package tech

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

const maxLocs = 50

type analyzer struct{}

// Analyzers returns the tech analyzer.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "tech" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

type techAgg struct {
	files int
	occ   int
	pkgs  map[string]int
	locs  []model.Location
}

type fileHit struct {
	occ  int
	pkgs map[string]int
	line int
}

func sortLocs(l []model.Location) {
	sort.Slice(l, func(i, j int) bool {
		if l[i].Path != l[j].Path {
			return l[i].Path < l[j].Path
		}
		return l[i].Line < l[j].Line
	})
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	ix := p.Files
	files := append([]scan.File(nil), ix.WithExt(".java")...)
	nJava := len(files)
	for _, e := range []string{".jsp", ".jspf", ".jspx"} {
		files = append(files, ix.WithExt(e)...)
	}
	nJSP := len(files) - nJava

	var (
		mu         sync.Mutex
		agg        = map[string]*techAgg{}
		unreadable atomic.Int64
	)
	scan.ForEach(ctx, files, func(f scan.File) {
		fh, err := os.Open(ix.Abs(f))
		if err != nil {
			unreadable.Add(1)
			return
		}
		defer func() { _ = fh.Close() }()
		local := map[string]*fileHit{}
		add := func(tech, pkg string, line int) {
			h := local[tech]
			if h == nil {
				h = &fileHit{pkgs: map[string]int{}, line: line}
				local[tech] = h
			}
			h.occ++
			if pkg != "" {
				h.pkgs[pkg]++
			}
		}
		onImport := func(name string, line int) {
			if e, ok := lookup(name); ok {
				add(e.tech, pkgOf(name), line)
			}
		}
		if f.Ext == ".java" {
			scanJava(fh, onImport)
		} else {
			scanJSP(fh, onImport, func(uri string, line int) { add(taglibTech(uri), uri, line) })
		}
		if len(local) == 0 {
			return
		}
		mu.Lock()
		for t, h := range local {
			a := agg[t]
			if a == nil {
				a = &techAgg{pkgs: map[string]int{}}
				agg[t] = a
			}
			a.files++
			a.occ += h.occ
			for k, v := range h.pkgs {
				a.pkgs[k] += v
			}
			a.locs = append(a.locs, model.Location{Path: f.Rel, Line: h.line})
			if len(a.locs) > 8*maxLocs {
				sortLocs(a.locs)
				a.locs = a.locs[:maxLocs]
			}
		}
		mu.Unlock()
	})

	names := make([]string, 0, len(agg))
	for n := range agg {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		emitTech(sink, n, agg[n])
	}
	sink.Emit(model.Evidence{
		Category: model.CatTechnology, Kind: model.KindTechUsage, Subject: "(import scan coverage)",
		Finding: fmt.Sprintf("scanned %d .java and %d JSP file(s) for imports; %d could not be opened; %d technology group(s) found",
			nJava, nJSP, unreadable.Load(), len(names)),
		Status: model.Info, Confidence: model.Observed,
		Values: map[string]string{
			"java_files": fmt.Sprint(nJava), "jsp_files": fmt.Sprint(nJSP),
			"unreadable": fmt.Sprint(unreadable.Load()), "technologies": fmt.Sprint(len(names)),
		},
		Limitations: []string{"Java files are read only up to the first type declaration; JSP files are read in full for page directives"},
	})
	return ctx.Err()
}

func emitTech(sink engine.Sink, name string, a *techAgg) {
	sortLocs(a.locs)
	locs := a.locs
	if len(locs) > maxLocs {
		locs = locs[:maxLocs]
	}
	pk := make([]string, 0, len(a.pkgs))
	for k := range a.pkgs {
		pk = append(pk, k)
	}
	sort.Slice(pk, func(i, j int) bool {
		if a.pkgs[pk[i]] != a.pkgs[pk[j]] {
			return a.pkgs[pk[i]] > a.pkgs[pk[j]]
		}
		return pk[i] < pk[j]
	})
	if len(pk) > 10 {
		pk = pk[:10]
	}
	vals := map[string]string{
		"files":           fmt.Sprint(a.files),
		"occurrences":     fmt.Sprint(a.occ),
		"packages":        strings.Join(pk, ","),
		"locations_total": fmt.Sprint(a.files),
	}
	status, sev := model.Info, model.SevInfo
	lim := []string{
		"import statements show compile-time references only; they do not prove runtime use or deployment",
		"wildcard imports are counted as the package; locations show the first matching line per file, the alphabetically first files",
	}
	info := techInfo[name]
	if info.removed != "" {
		vals["removed_in_jdk"] = info.removed
		vals["jdk_note"] = info.note
		status, sev = model.Warn, model.SevLow
		lim = append(lim, "removal flag is a property of the JDK API; whether this project is affected depends on the JDK it targets")
	} else if info.note != "" {
		vals["jdk_note"] = info.note
	}
	sink.Emit(model.Evidence{
		Category: model.CatTechnology, Kind: model.KindTechUsage, Subject: name,
		Finding: fmt.Sprintf("%d file(s) reference %s (%d import/taglib occurrence(s))", a.files, name, a.occ),
		Status:  status, Severity: sev, Confidence: model.Observed,
		Locations: locs, Limitations: lim, Values: vals,
	})
}
