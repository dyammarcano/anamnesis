package mta

import (
	"sort"
	"strings"
)

// EffortLabel is the mandatory caption for any figure produced by Aggregate.
const EffortLabel = "MTA effort points, DERIVED: effort × incidents"

// IncidentRef is one incident reduced to what aggregation needs. Path is repo-relative (or an
// artifact path when the incident lies outside the repository); empty when unknown.
type IncidentRef struct {
	Path string
	Line int
}

// RuleResult is one (ruleset, rule) outcome with ALL of its incidents.
type RuleResult struct {
	RuleSet   string
	RuleID    string
	Category  string
	Labels    []string
	Targets   []string // values of konveyor.io/target labels
	Effort    int      // points per incident, as MTA states them
	Incidents []IncidentRef
}

// Bucket is one row of a breakdown.
type Bucket struct {
	Key       string
	Points    int // sum of effort × incidents
	Incidents int
	Rules     int // distinct rules contributing
}

// Calibration reserves the place where a measured points-to-hours ratio would live. It is always
// empty: Anamnesis never converts effort to hours.
type Calibration struct {
	HoursPerPoint *float64
	Source        string
}

// EffortReport is the aggregate the report renders. Label must be shown with every figure.
type EffortReport struct {
	Label       string
	TotalPoints int
	Incidents   int
	Rules       int
	ByRule      []Bucket
	// ByTarget attributes a rule's full points to each of its target labels, so the buckets can
	// sum to more than TotalPoints.
	ByTarget    []Bucket
	ByCategory  []Bucket
	ByModule    []Bucket // first path segment of the incident file
	ByPackage   []Bucket // Java package directory of the incident file
	Calibration Calibration
}

type bucketAcc struct {
	points, incidents int
	rules             map[string]bool
}

type accMap map[string]*bucketAcc

func (m accMap) add(key, rule string, points, incidents int) {
	b := m[key]
	if b == nil {
		b = &bucketAcc{rules: map[string]bool{}}
		m[key] = b
	}
	b.points += points
	b.incidents += incidents
	b.rules[rule] = true
}

func (m accMap) buckets() []Bucket {
	out := make([]Bucket, 0, len(m))
	for k, b := range m {
		out = append(out, Bucket{Key: k, Points: b.points, Incidents: b.incidents, Rules: len(b.rules)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Points != out[j].Points {
			return out[i].Points > out[j].Points
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Aggregate derives effort totals and breakdowns. It never produces hours.
func Aggregate(results []RuleResult) EffortReport {
	rep := EffortReport{Label: EffortLabel}
	byRule, byTarget, byCat, byMod, byPkg := accMap{}, accMap{}, accMap{}, accMap{}, accMap{}
	for _, r := range results {
		n := len(r.Incidents)
		pts := r.Effort * n
		id := r.RuleSet + "/" + r.RuleID
		rep.Rules++
		rep.Incidents += n
		rep.TotalPoints += pts
		byRule.add(id, id, pts, n)
		cat := r.Category
		if cat == "" {
			cat = "(none)"
		}
		byCat.add(cat, id, pts, n)
		if len(r.Targets) == 0 {
			byTarget.add("(no target label)", id, pts, n)
		}
		for _, t := range r.Targets {
			byTarget.add(t, id, pts, n)
		}
		for _, inc := range r.Incidents {
			if inc.Path == "" {
				byMod.add("(unknown)", id, r.Effort, 1)
				byPkg.add("(unknown)", id, r.Effort, 1)
				continue
			}
			byMod.add(ModuleOf(inc.Path), id, r.Effort, 1)
			byPkg.add(JavaPackageOf(inc.Path), id, r.Effort, 1)
		}
	}
	rep.ByRule, rep.ByTarget, rep.ByCategory = byRule.buckets(), byTarget.buckets(), byCat.buckets()
	rep.ByModule, rep.ByPackage = byMod.buckets(), byPkg.buckets()
	return rep
}

// ModuleOf is the first path segment of a slash-separated path; files at the top level are "(root)".
func ModuleOf(path string) string {
	path = strings.TrimPrefix(path, "./")
	i := strings.IndexByte(path, '/')
	if i < 0 {
		return "(root)"
	}
	return path[:i]
}

var javaRoots = []string{"/src/main/java/", "/src/test/java/", "/src/", "/java/", "/source/", "/sources/"}

// JavaPackageOf derives the dotted Java package of a file from its directory, relying on common
// source-root names. When no root is recognised the directory itself is returned.
func JavaPackageOf(path string) string {
	path = strings.TrimPrefix(path, "./")
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "(default package)"
	}
	dir := "/" + path[:i] + "/"
	for _, root := range javaRoots {
		if j := strings.LastIndex(dir, root); j >= 0 {
			rest := strings.Trim(dir[j+len(root):], "/")
			if rest == "" {
				return "(default package)"
			}
			return strings.ReplaceAll(rest, "/", ".")
		}
	}
	return strings.Trim(dir, "/")
}
