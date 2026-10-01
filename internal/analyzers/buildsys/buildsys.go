// Package buildsys detects which build systems a project carries, with every location, and makes the
// absence of Ant, Maven and Gradle explicit instead of leaving it to be concluded from silence.
package buildsys

import (
	"context"
	"maps"
	"path"
	"sort"
	"strconv"
	"strings"

	"anamnesis/internal/engine"
	"anamnesis/internal/model"
	"anamnesis/internal/scan"
)

const maxLocations = 50

type analyzer struct{}

// Analyzers returns the analyzers of this package.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

func (analyzer) Name() string                  { return "buildsys" }
func (analyzer) Phase() engine.Phase           { return engine.Preflight }
func (analyzer) Requires() []engine.Capability { return nil }

func fileLocs(files []scan.File) ([]model.Location, int) {
	locs := make([]model.Location, 0, min(len(files), maxLocations))
	for i, f := range files {
		if i >= maxLocations {
			break
		}
		locs = append(locs, model.Location{Path: f.Rel})
	}
	return locs, len(files)
}

func atRoot(files []scan.File) bool {
	for _, f := range files {
		if !strings.Contains(f.Rel, "/") {
			return true
		}
	}
	return false
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	ix := p.Files
	collect := func(names ...string) []scan.File {
		var out []scan.File
		for _, n := range names {
			out = append(out, ix.Named(n)...)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
		return out
	}
	present := func(subject string, files []scan.File, finding string, extra map[string]string, assume ...string) {
		locs, total := fileLocs(files)
		vals := map[string]string{
			"at_root":         strconv.FormatBool(atRoot(files)),
			"count":           strconv.Itoa(total),
			"locations_total": strconv.Itoa(total),
		}
		maps.Copy(vals, extra)
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindBuildSystem, Subject: subject, Finding: finding,
			Status: model.Info, Confidence: model.Observed, Locations: locs, Values: vals, Assumptions: assume,
		})
	}
	absent := func(subject, marker string) {
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindBuildSystemAbsent, Subject: subject,
			Finding: "No " + marker + " was found anywhere in the indexed project tree.",
			Status:  model.Info, Confidence: model.Observed,
			Values:      map[string]string{"marker": marker},
			Limitations: []string{"Absence of " + subject + " says nothing about whether the project has another build system or can be built."},
		})
	}

	if f := collect("build.xml"); len(f) > 0 {
		present("ant", f, "Ant build file(s) found ("+strconv.Itoa(len(f))+" build.xml).", nil)
	} else {
		absent("ant", "build.xml")
	}

	if f := collect("pom.xml"); len(f) > 0 {
		w := collect("mvnw", "mvnw.cmd")
		present("maven", f, "Maven project file(s) found ("+strconv.Itoa(len(f))+" pom.xml).",
			map[string]string{"mvnw": strconv.FormatBool(len(w) > 0)})
	} else {
		absent("maven", "pom.xml")
	}

	if f := collect("build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "gradlew", "gradlew.bat"); len(f) > 0 {
		var groovyRoot, ktsRoot, wrapper bool
		for _, x := range f {
			root := !strings.Contains(x.Rel, "/")
			switch x.Base {
			case "build.gradle":
				groovyRoot = groovyRoot || root
			case "build.gradle.kts":
				ktsRoot = ktsRoot || root
			case "gradlew", "gradlew.bat":
				wrapper = true
			}
		}
		present("gradle", f, "Gradle file(s) found ("+strconv.Itoa(len(f))+" build/settings/wrapper files).",
			map[string]string{
				"wrapper":               strconv.FormatBool(wrapper),
				"root_build_gradle":     strconv.FormatBool(groovyRoot),
				"root_build_gradle_kts": strconv.FormatBool(ktsRoot),
			})
	} else {
		absent("gradle", "build.gradle, build.gradle.kts or settings.gradle*")
	}

	var eclipse []scan.File
	for _, f := range ix.Files {
		switch {
		case f.Base == ".project" || f.Base == ".classpath":
			eclipse = append(eclipse, f)
		case strings.HasPrefix(f.Rel, ".settings/") || strings.Contains(f.Rel, "/.settings/"):
			eclipse = append(eclipse, f)
		}
	}
	if len(eclipse) > 0 {
		present("eclipse", eclipse, "Eclipse metadata found ("+strconv.Itoa(len(eclipse))+" .project/.classpath/.settings files).", nil)
	}

	var nb []scan.File
	for _, f := range ix.Named("project.properties") {
		if path.Base(path.Dir(f.Rel)) == "nbproject" {
			nb = append(nb, f)
		}
	}
	if len(nb) > 0 {
		present("netbeans", nb, "NetBeans project metadata found ("+strconv.Itoa(len(nb))+" nbproject/project.properties).", nil)
	}

	jarDirs := map[string]int{}
	for _, j := range ix.WithExt(".jar") {
		jarDirs[path.Dir(j.Rel)]++
	}
	if len(jarDirs) > 0 {
		dirs := make([]string, 0, len(jarDirs))
		for d := range jarDirs {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		locs := make([]model.Location, 0, maxLocations)
		jars := 0
		rootDir := false
		for _, d := range dirs {
			jars += jarDirs[d]
			if d == "." {
				rootDir = true
			}
			if len(locs) < maxLocations {
				locs = append(locs, model.Location{Path: d})
			}
		}
		sink.Emit(model.Evidence{
			Category: model.CatBuild, Kind: model.KindBuildSystem, Subject: "manual-libs",
			Finding: strconv.Itoa(len(dirs)) + " director(ies) contain .jar files (" + strconv.Itoa(jars) + " jars), consistent with hand-managed libraries.",
			Status:  model.Info, Confidence: model.Observed, Locations: locs,
			Values: map[string]string{
				"at_root": strconv.FormatBool(rootDir), "count": strconv.Itoa(len(dirs)), "jars": strconv.Itoa(jars),
				"locations_total": strconv.Itoa(len(dirs)),
			},
			Limitations: []string{"Whether these jars are used by a build was not determined."},
		})
	}
	return ctx.Err()
}
