// Package antsafety statically classifies the targets of Ant build files as SAFE,
// REVIEW_REQUIRED, DANGEROUS or UNKNOWN and ranks the build candidates. It only reads files; Ant
// targets are never executed by this analyzer.
package antsafety

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/ant"
	"github.com/dyammarcano/anamnesis/internal/engine"
	"github.com/dyammarcano/anamnesis/internal/model"
)

const (
	maxLocations   = 50
	maxReasons     = 6
	maxReasonChars = 1500
	maxClosure     = 60
	maxCandidates  = 10
)

// Analyzers returns this package's analyzers.
func Analyzers() []engine.Analyzer { return []engine.Analyzer{analyzer{}} }

type analyzer struct{}

func (analyzer) Name() string                  { return "ant-safety" }
func (analyzer) Phase() engine.Phase           { return engine.Deep }
func (analyzer) Requires() []engine.Capability { return nil }

func subject(ap *ant.Project, target string) string {
	if ap.File == "build.xml" {
		return target
	}
	return ap.File + "#" + target
}

func severity(c ant.Class) model.Severity {
	switch c {
	case ant.ClassDangerous:
		return model.SevHigh
	case ant.ClassReview:
		return model.SevMedium
	case ant.ClassUnknown:
		return model.SevLow
	}
	return model.SevInfo
}

func joinCapped(items []string, sep string, maxChars int) string {
	var b strings.Builder
	for i, s := range items {
		if b.Len()+len(s)+len(sep) > maxChars && i > 0 {
			fmt.Fprintf(&b, "%s... (+%d more)", sep, len(items)-i)
			break
		}
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}

func (analyzer) Analyze(ctx context.Context, p *engine.Project, sink engine.Sink) error {
	projects, _ := ant.LoadAll(p) // parse failures are reported by ant-facts
	var modulesWithoutCandidate []string
	for _, ap := range projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		all := ap.ClassifyAll()
		for _, ts := range all {
			emitTarget(sink, ap, ts)
		}
		cands := ant.Candidates(all)
		switch {
		case len(cands) > 0:
			for i, c := range cands {
				if i == maxCandidates {
					break
				}
				emitCandidate(sink, ap, c, i+1, len(cands))
			}
		case ap.IsRoot():
			sink.Emit(model.Evidence{
				Category: model.CatSafety, Kind: model.KindAntBuildCandidate, Subject: "none:" + ap.File,
				Finding: "No target of " + ap.File + " can be proven SAFE to build; targets are never executed by this analyzer: " + ap.NoCandidateReason(all),
				Status:  model.Warn, Confidence: model.Derived, Severity: model.SevMedium,
				Locations: []model.Location{{Path: ap.File}},
				Values:    map[string]string{"class": "NONE", "reason": ap.NoCandidateReason(all), "targets_classified": strconv.Itoa(len(all))},
				Assumptions: []string{
					"target names carry no weight; only the tasks in each target's transitive closure were classified",
				},
				Limitations: []string{"static analysis only: property values from -D options, the environment or conditions are unknown"},
			})
		default:
			modulesWithoutCandidate = append(modulesWithoutCandidate, ap.File)
		}
	}
	if len(modulesWithoutCandidate) > 0 {
		sort.Strings(modulesWithoutCandidate)
		locs := make([]model.Location, 0, len(modulesWithoutCandidate))
		for i, f := range modulesWithoutCandidate {
			if i == maxLocations {
				break
			}
			locs = append(locs, model.Location{Path: f})
		}
		sink.Emit(model.Evidence{
			Category: model.CatSafety, Kind: model.KindAntBuildCandidate, Subject: "none:modules",
			Finding: fmt.Sprintf("%d module build file(s) have no target that can be proven SAFE to build; targets are never executed by this analyzer", len(modulesWithoutCandidate)),
			Status:  model.Warn, Confidence: model.Derived, Severity: model.SevLow, Locations: locs,
			Values: map[string]string{"class": "NONE", "count": strconv.Itoa(len(modulesWithoutCandidate)), "locations_total": strconv.Itoa(len(modulesWithoutCandidate))},
		})
	}
	return nil
}

func emitTarget(sink engine.Sink, ap *ant.Project, ts ant.TargetSafety) {
	reasons := make([]string, 0, len(ts.Reasons))
	for _, r := range ts.Reasons {
		reasons = append(reasons, r.String())
	}
	shown := reasons
	if len(shown) > maxReasons {
		shown = shown[:maxReasons]
	}
	closure := ts.Closure
	closureStr := strings.Join(closure, ",")
	if len(closure) > maxClosure {
		closureStr = strings.Join(closure[:maxClosure], ",") + fmt.Sprintf(",...(+%d)", len(closure)-maxClosure)
	}
	var locs []model.Location
	seen := map[model.Location]bool{}
	addLoc := func(l ant.Location) {
		if l.IsZero() || len(locs) >= maxLocations {
			return
		}
		ml := model.Location{Path: l.File, Line: l.Line}
		if !seen[ml] {
			seen[ml] = true
			locs = append(locs, ml)
		}
	}
	addLoc(ts.Loc)
	for _, r := range ts.Reasons {
		addLoc(r.Loc)
	}
	conf := model.Derived
	if ts.Class == ant.ClassUnknown {
		conf = model.Unknown
	}
	status := model.Warn
	if ts.Class == ant.ClassSafe {
		status = model.Pass
	}
	finding := fmt.Sprintf("Target %q is %s by static analysis of its %d-target closure; targets are never executed by this analyzer", ts.Target, ts.Class, len(closure))
	if len(ts.Reasons) > 0 {
		finding += ": " + ts.Reasons[0].String()
	}
	limits := []string{"static analysis only: property values from -D options, the environment, conditions or commands are unknown"}
	if n := len(ap.Unresolved); n > 0 {
		limits = append(limits, fmt.Sprintf("%d import(s) of %s could not be followed, so targets, properties and tasks they define are not classified", n, ap.File))
	}
	sink.Emit(model.Evidence{
		Category: model.CatSafety, Kind: model.KindAntTargetSafety, Subject: subject(ap, ts.Target), Target: ts.Target,
		Finding: finding, Status: status, Confidence: conf, Severity: severity(ts.Class), Locations: locs,
		Values: map[string]string{
			"class": string(ts.Class), "reasons": joinCapped(shown, "; ", maxReasonChars), "reasons_total": strconv.Itoa(len(reasons)),
			"closure": closureStr, "default": strconv.FormatBool(ts.Default),
			"javac": strconv.Itoa(ts.Javac), "packaging": strconv.Itoa(ts.Packaging), "build_file": ap.File,
		},
		Assumptions: []string{
			"the class is the worst class over every task in the transitive closure (depends, antcall, ant, subant, macro bodies) and project-level tasks",
			"target names carry no weight: a target called build, compile or deploy is classified only by what its tasks do",
		},
		Limitations: limits,
	})
}

func emitCandidate(sink engine.Sink, ap *ant.Project, c ant.TargetSafety, rank, total int) {
	why := fmt.Sprintf("closure of %d target(s) is SAFE and contains %d javac and %d packaging task(s)", len(c.Closure), c.Javac, c.Packaging)
	if c.Default {
		why = "default target; " + why
	}
	sink.Emit(model.Evidence{
		Category: model.CatSafety, Kind: model.KindAntBuildCandidate, Subject: subject(ap, c.Target), Target: c.Target,
		Finding: fmt.Sprintf("Target %q is build candidate #%d of %d in %s: %s; it was classified statically and has not been executed", c.Target, rank, total, ap.File, why),
		Status:  model.Info, Confidence: model.Derived,
		Locations: []model.Location{{Path: c.Loc.File, Line: c.Loc.Line}},
		Values: map[string]string{
			"class": string(c.Class), "reason": why, "rank": strconv.Itoa(rank), "default": strconv.FormatBool(c.Default),
			"javac": strconv.Itoa(c.Javac), "packaging": strconv.Itoa(c.Packaging), "build_file": ap.File,
		},
		Assumptions: []string{"ranking: default target first, then most javac tasks, then most packaging tasks, then smaller closure; names are not evidence"},
		Limitations: []string{"static analysis only: SAFE means no task in the closure was found to write outside the project, deploy, prompt, download or run unknown code"},
	})
}
