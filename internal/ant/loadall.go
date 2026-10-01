package ant

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/scan"
)

// LoadError records a build file that could not be parsed at all.
type LoadError struct {
	File string
	Err  error
}

type allEntry struct {
	once     sync.Once
	projects []*Project
	errs     []LoadError
}

var allCache sync.Map // *engine.Project -> *allEntry

// LoadAll parses every build.xml of the project (and, as roots of their own, any other build*.xml
// that no loaded build file imports or calls). The result is memoized per engine.Project so the
// analyzers share one parse; callers must treat the returned projects as read-only. Projects are
// sorted by File.
func LoadAll(p *engine.Project) ([]*Project, []LoadError) {
	v, _ := allCache.LoadOrStore(p, &allEntry{})
	e := v.(*allEntry)
	e.once.Do(func() { e.projects, e.errs = loadAll(p) })
	return e.projects, e.errs
}

func loadAll(p *engine.Project) ([]*Project, []LoadError) {
	var (
		mu       sync.Mutex
		projects []*Project
		errs     []LoadError
	)
	main := p.Files.Named("build.xml")
	scan.ForEach(context.Background(), main, func(f scan.File) {
		ap, err := Load(p.Root, f.Rel)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, LoadError{File: f.Rel, Err: err})
			return
		}
		projects = append(projects, ap)
	})
	covered := map[string]bool{}
	for _, ap := range projects {
		for _, f := range ap.Files {
			covered[strings.ToLower(f)] = true
		}
		for f := range ap.Subs {
			covered[strings.ToLower(f)] = true
		}
	}
	for _, f := range p.Files.Match("build*.xml") {
		if f.Base == "build.xml" || covered[strings.ToLower(f.Rel)] {
			continue
		}
		ap, err := Load(p.Root, f.Rel)
		if err != nil || len(ap.TargetOrder) == 0 {
			continue // not a build file (or nothing runnable)
		}
		projects = append(projects, ap)
		for _, cf := range ap.Files {
			covered[strings.ToLower(cf)] = true
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].File < projects[j].File })
	sort.Slice(errs, func(i, j int) bool { return errs[i].File < errs[j].File })
	return projects, errs
}

// ChooseTarget picks the build target to run for the project from the ROOT build.xml: the top-ranked
// candidate whose transitive closure is SAFE and contains javac/jar/war/ear (default target first
// when SAFE). It only reads files; nothing is executed. ok=false carries the reason no target
// qualifies.
func ChooseTarget(p *engine.Project) (buildFile, target, reason string, ok bool) {
	projects, errs := LoadAll(p)
	var root *Project
	for _, ap := range projects {
		if ap.File == "build.xml" {
			root = ap
			break
		}
	}
	if root == nil {
		for _, e := range errs {
			if e.File == "build.xml" {
				return "", "", fmt.Sprintf("root build.xml could not be parsed: %v", e.Err), false
			}
		}
		return "", "", "no root build.xml", false
	}
	all := root.ClassifyAll()
	cands := Candidates(all)
	if len(cands) > 0 {
		c := cands[0]
		why := fmt.Sprintf("closure is SAFE (%d javac, %d packaging task(s))", c.Javac, c.Packaging)
		if c.Default {
			why = "default target; " + why
		}
		return root.File, c.Target, why, true
	}
	return "", "", root.NoCandidateReason(all), false
}

// NoCandidateReason explains why none of the classified targets is a build candidate.
func (p *Project) NoCandidateReason(all []TargetSafety) string {
	if p.Default != "" {
		for _, ts := range all {
			if ts.Target != p.Default {
				continue
			}
			if ts.Class == ClassSafe {
				return fmt.Sprintf("no target can be proven SAFE with a javac/jar/war/ear task: default %q is SAFE but builds nothing (no javac or packaging task in its closure)", p.Default)
			}
			why := "an unclassified cause"
			if len(ts.Reasons) > 0 {
				why = ts.Reasons[0].String()
			}
			return fmt.Sprintf("no target can be proven SAFE: default %q is %s via %s", p.Default, ts.Class, why)
		}
		return fmt.Sprintf("no target can be proven SAFE: default target %q is not defined", p.Default)
	}
	return fmt.Sprintf("no target can be proven SAFE: no default target and none of %d targets is SAFE with a javac/jar/war/ear task", len(all))
}
