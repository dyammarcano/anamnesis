// Package identity emits the basic size and shape facts of a project: file counts, Java volume,
// archives, generated-source directory candidates and library directories.
package identity

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
)

const maxLocations = 50

var generatedNames = map[string]bool{
	"generated": true, "gen-src": true, "generated-sources": true, ".apt_generated": true, "src-gen": true,
}

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "identity" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	ix := p.Files
	jars, wars, ears := ix.WithExt(".jar"), ix.WithExt(".war"), ix.WithExt(".ear")

	var generated []string
	for _, d := range ix.Dirs {
		if generatedNames[strings.ToLower(path.Base(d))] {
			generated = append(generated, d)
		}
	}

	libCount := map[string]int{}
	for _, j := range jars {
		libCount[path.Dir(j.Rel)]++
	}
	libDirs := make([]string, 0, len(libCount))
	for d := range libCount {
		libDirs = append(libDirs, d)
	}
	sort.Strings(libDirs)
	first := libDirs
	if len(first) > 20 {
		first = first[:20]
	}
	shown := make([]string, len(first))
	for i, d := range first {
		name := d
		if d == "." {
			name = "(root)"
		}
		shown[i] = name + " (" + strconv.Itoa(libCount[d]) + ")"
	}

	locs := make([]model.Location, 0, maxLocations)
	for _, d := range generated {
		if len(locs) >= maxLocations {
			break
		}
		locs = append(locs, model.Location{Path: d})
	}

	sink.Emit(model.Evidence{
		Category: model.CatSource,
		Kind:     model.KindIdentity,
		Subject:  p.Name,
		Finding: "Project contains " + strconv.Itoa(len(ix.Files)) + " files (" + strconv.FormatInt(ix.TotalBytes, 10) +
			" bytes), " + strconv.Itoa(ix.JavaFiles) + " Java source files and " +
			strconv.Itoa(len(jars)) + " jar, " + strconv.Itoa(len(wars)) + " war, " + strconv.Itoa(len(ears)) + " ear archives.",
		Status:     model.Info,
		Confidence: model.Observed,
		Locations:  locs,
		Values: map[string]string{
			"files":            strconv.Itoa(len(ix.Files)),
			"bytes":            strconv.FormatInt(ix.TotalBytes, 10),
			"java_files":       strconv.Itoa(ix.JavaFiles),
			"java_lines":       strconv.FormatInt(ix.JavaLines, 10),
			"jars":             strconv.Itoa(len(jars)),
			"wars":             strconv.Itoa(len(wars)),
			"ears":             strconv.Itoa(len(ears)),
			"generated_dirs":   strconv.Itoa(len(generated)),
			"lib_dirs":         strconv.Itoa(len(libDirs)),
			"lib_dirs_first20": strings.Join(shown, "; "),
			"locations_total":  strconv.Itoa(len(generated)),
		},
		Assumptions: []string{"Generated-source directories are candidates by directory name only (generated, gen-src, generated-sources, .apt_generated, src-gen); their contents were not inspected."},
		Limitations: []string{
			"java_lines is an approximate newline count over .java files, including blank lines and comments.",
			"VCS metadata directories (.git, .svn, .hg) are not indexed.",
		},
	})

	if n := len(ix.WalkErrors); n > 0 {
		errs := ix.WalkErrors
		if len(errs) > 10 {
			errs = errs[:10]
		}
		sink.Emit(model.Evidence{
			Category: model.CatSource, Kind: model.KindIdentity, Subject: "walk-errors",
			Finding:     strconv.Itoa(n) + " paths could not be read while indexing the project; counts may be incomplete.",
			Status:      model.Warn,
			Confidence:  model.Observed,
			Values:      map[string]string{"walk_errors": strconv.Itoa(n), "first_errors": strings.Join(errs, " | ")},
			Limitations: []string{"Facts derived from the file index exclude the unreadable paths."},
		})
	}
	return ctx.Err()
}
