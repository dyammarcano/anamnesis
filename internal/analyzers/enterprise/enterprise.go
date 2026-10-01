// Package enterprise reports Java EE / J2EE deployment descriptors, app-server signals and
// view-file counts. It reads descriptor root elements only; it never executes anything.
package enterprise

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

const maxLocs = 50

type analyzer struct{}

// Analyzers returns the enterprise analyzer.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "enterprise" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	groups := classifyAll(p.Files)
	infos := sniffAll(ctx, p, groups)
	emitDescriptors(sink, groups, infos)
	emitViewFiles(sink, p.Files)
	hits := scanEnvRefs(ctx, p)
	servers := collectServers(p.Files, groups, infos, hits)
	emitServers(sink, servers)
	emitJavaEE(sink, p.Files, groups, servers)
	return ctx.Err()
}

var exactDescriptors = map[string]bool{
	"web.xml": true, "application.xml": true, "ejb-jar.xml": true, "persistence.xml": true,
	"orm.xml": true, "faces-config.xml": true, "beans.xml": true, "webservices.xml": true,
	"struts-config.xml": true, "jboss.xml": true, "jbosscmp-jdbc.xml": true,
}

var vendorPrefixes = []string{"jboss-", "weblogic", "ibm-", "sun-", "glassfish-", "geronimo-"}

// classify maps an indexed file to a descriptor type, or false when it is not a descriptor.
func classify(f scan.File) (string, bool) {
	b := f.Base
	if exactDescriptors[b] {
		return b, true
	}
	switch {
	case f.Ext == ".wsdl":
		return "*.wsdl", true
	case f.Ext == ".tld":
		return "*.tld", true
	case b == "context.xml":
		if strings.Contains(strings.ToLower(f.Rel), "meta-inf/") {
			return "context.xml (META-INF)", true
		}
		return "", false
	case strings.HasSuffix(b, "-ds.xml"):
		return "*-ds.xml", true
	case f.Ext == ".xml" && strings.HasPrefix(b, "applicationcontext"):
		return "applicationContext*.xml", true
	}
	if f.Ext == ".xml" || f.Ext == ".xmi" {
		for _, pre := range vendorPrefixes {
			if !strings.HasPrefix(b, pre) {
				continue
			}
			if pre == "ibm-" {
				return strings.TrimSuffix(b, f.Ext) + ".xml/xmi", true
			}
			if f.Ext == ".xml" {
				return b, true
			}
		}
	}
	return "", false
}

func classifyAll(ix *scan.Index) map[string][]scan.File {
	groups := map[string][]scan.File{}
	for _, f := range ix.Files {
		if typ, ok := classify(f); ok {
			groups[typ] = append(groups[typ], f)
		}
	}
	return groups
}

func sortedTypes(groups map[string][]scan.File) []string {
	out := make([]string, 0, len(groups))
	for k := range groups {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sniffAll(ctx context.Context, p *engine.Project, groups map[string][]scan.File) map[string]specInfo {
	var files []scan.File
	for _, typ := range sortedTypes(groups) {
		files = append(files, groups[typ]...)
	}
	var mu sync.Mutex
	infos := make(map[string]specInfo, len(files))
	scan.ForEach(ctx, files, func(f scan.File) {
		si := sniffXML(p.Files.Abs(f))
		mu.Lock()
		infos[f.Rel] = si
		mu.Unlock()
	})
	return infos
}

func capLocs(l []model.Location) ([]model.Location, int) {
	total := len(l)
	if total > maxLocs {
		l = l[:maxLocs]
	}
	return l, total
}

func emitDescriptors(sink engine.Sink, groups map[string][]scan.File, infos map[string]specInfo) {
	for _, typ := range sortedTypes(groups) {
		files := groups[typ]
		counts := map[string]int{}
		unreadable := 0
		locs := make([]model.Location, 0, len(files))
		for _, f := range files {
			locs = append(locs, model.Location{Path: f.Rel})
			si := infos[f.Rel]
			if !si.OK {
				unreadable++
				continue
			}
			counts[formatSpec(typ, si)]++
		}
		specs := make([]string, 0, len(counts))
		for s := range counts {
			specs = append(specs, s)
		}
		sort.Slice(specs, func(i, j int) bool {
			if counts[specs[i]] != counts[specs[j]] {
				return counts[specs[i]] > counts[specs[j]]
			}
			return specs[i] < specs[j]
		})
		specStr, countStr := "unknown", ""
		if len(specs) > 0 {
			specStr = strings.Join(specs, ", ")
			parts := make([]string, len(specs))
			for i, s := range specs {
				parts[i] = fmt.Sprintf("%s=%d", s, counts[s])
			}
			countStr = strings.Join(parts, "; ")
		}
		shown, total := capLocs(locs)
		vals := map[string]string{
			"count":           fmt.Sprint(len(files)),
			"spec_version":    specStr,
			"locations_total": fmt.Sprint(total),
		}
		if countStr != "" {
			vals["spec_version_counts"] = countStr
		}
		var lim []string
		if unreadable > 0 {
			lim = append(lim, fmt.Sprintf("%d file(s) could not be parsed as XML, so no version was read from them", unreadable))
		}
		lim = append(lim, "only the root element, its version attribute, namespace, xsi:schemaLocation file name and DOCTYPE public id are read; descriptor content is not interpreted")
		sink.Emit(model.Evidence{
			Category: model.CatTechnology, Kind: model.KindEnterpriseDescriptor, Subject: typ,
			Finding: fmt.Sprintf("%d %s descriptor file(s) found; declared version: %s", len(files), typ, specStr),
			Status:  model.Info, Confidence: model.Observed,
			Locations: shown, Limitations: lim, Values: vals,
		})
	}
}

func emitViewFiles(sink engine.Sink, ix *scan.Index) {
	exts := []string{".jsp", ".jspf", ".jspx", ".jsf", ".xhtml", ".tld"}
	vals := map[string]string{}
	var locs []model.Location
	total := 0
	for _, e := range exts {
		fs := ix.WithExt(e)
		vals[strings.TrimPrefix(e, ".")] = fmt.Sprint(len(fs))
		total += len(fs)
		for _, f := range fs {
			locs = append(locs, model.Location{Path: f.Rel})
		}
	}
	if total == 0 {
		return
	}
	sort.Slice(locs, func(i, j int) bool { return locs[i].Path < locs[j].Path })
	shown, n := capLocs(locs)
	vals["total"] = fmt.Sprint(total)
	vals["locations_total"] = fmt.Sprint(n)
	sink.Emit(model.Evidence{
		Category: model.CatTechnology, Kind: model.KindEnterpriseDescriptor, Subject: "web-view-files",
		Finding: fmt.Sprintf("%d web view file(s): jsp=%s jspf=%s jspx=%s jsf=%s xhtml=%s tld=%s", total, vals["jsp"], vals["jspf"], vals["jspx"], vals["jsf"], vals["xhtml"], vals["tld"]),
		Status:  model.Info, Confidence: model.Observed,
		Locations: shown, Values: vals,
		Limitations: []string{"counts are by file extension; .xhtml and .jsf files are not necessarily JSF pages"},
	})
}
