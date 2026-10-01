package mta

// Rule coverage (P-4, Anamnesis original). MTA drops, without recording it in output.yaml,
// every rule whose provider did not start (docs/kantra-internals.md section 6). Coverage is
// therefore computed from the installed rulesets and the set of providers that actually ran.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dyammarcano/anamnesis/internal/model"
)

// Rule classes by the provider capabilities their `when` clause uses.
const (
	ClassJavaOnly      = "java-only"
	ClassBuiltinOnly   = "builtin-only"
	ClassMixedOr       = "mixed-or"  // builtin branch survives without java
	ClassMixedAnd      = "mixed-and" // and/nested: dropped without java
	ClassOtherProvider = "other-provider"
	ClassNoCondition   = "no-condition-detected"
)

var (
	reRuleID = regexp.MustCompile(`^\s*-?\s*ruleID:\s*(\S+)`)
	reCond   = regexp.MustCompile(`^\s*-?\s*(java|builtin|go|nodejs|python|dotnet|csharp|yaml)\.([A-Za-z]+):`)
	reEffort = regexp.MustCompile(`^\s*effort:\s*(\d+)`)
	reOr     = regexp.MustCompile(`^\s*-?\s*or:\s*$`)
	reAnd    = regexp.MustCompile(`^\s*-?\s*and:\s*$`)
)

// ClassCount is a rule tally for one class.
type ClassCount struct {
	Rules       int
	EffortRules int // rules with effort > 0
}

// RulesetStats is the heuristic census of <install>/rulesets/java.
type RulesetStats struct {
	Files            int
	Unreadable       int
	RulesTotal       int
	EffortRulesTotal int
	ByClass          map[string]ClassCount
}

// Providers states which providers actually ran in an MTA invocation.
type Providers struct {
	Java    bool
	Builtin bool
}

// String is the value stored in evidence.
func (p Providers) String() string {
	switch {
	case p.Java && p.Builtin:
		return "java+builtin"
	case p.Java:
		return "java"
	case p.Builtin:
		return "builtin"
	}
	return "none"
}

type ruleAcc struct {
	providers map[string]bool
	hasOr     bool
	hasAnd    bool
	effort    int
}

func (r *ruleAcc) class() string {
	switch {
	case len(r.providers) == 0:
		return ClassNoCondition
	case r.providers["builtin"] && len(r.providers) == 1:
		return ClassBuiltinOnly
	case r.providers["java"] && len(r.providers) == 1:
		return ClassJavaOnly
	case r.providers["java"] && r.providers["builtin"] && len(r.providers) == 2 && r.hasOr && !r.hasAnd:
		return ClassMixedOr
	case r.providers["java"] && r.providers["builtin"] && len(r.providers) == 2:
		return ClassMixedAnd
	}
	return ClassOtherProvider
}

// ScanRulesets reads <installDir>/rulesets/java/**/*.yaml line by line. It is a heuristic: a rule
// starts at a ruleID line and a condition is any "<provider>.<capability>:" key.
func ScanRulesets(ctx context.Context, installDir string) (RulesetStats, error) {
	st := RulesetStats{ByClass: map[string]ClassCount{}}
	root := filepath.Join(installDir, "rulesets", "java")
	if !isDir(root) {
		return st, os.ErrNotExist
	}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			st.Unreadable++
			return nil
		}
		if d.IsDir() {
			if d.Name() == "tests" {
				return filepath.SkipDir
			}
			return nil
		}
		low := strings.ToLower(d.Name())
		if (!strings.HasSuffix(low, ".yaml") && !strings.HasSuffix(low, ".yml")) ||
			low == "ruleset.yaml" || low == "ruleset.yml" || strings.HasSuffix(low, ".test.yaml") {
			return nil
		}
		if scanRuleFile(p, &st) {
			st.Files++
		} else {
			st.Unreadable++
		}
		return nil
	})
	return st, err
}

func scanRuleFile(path string, st *RulesetStats) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var cur *ruleAcc
	flush := func() {
		if cur == nil {
			return
		}
		c := cur.class()
		cc := st.ByClass[c]
		cc.Rules++
		st.RulesTotal++
		if cur.effort > 0 {
			cc.EffortRules++
			st.EffortRulesTotal++
		}
		st.ByClass[c] = cc
		cur = nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if reRuleID.MatchString(line) {
			flush()
			cur = &ruleAcc{providers: map[string]bool{}}
			continue
		}
		if cur == nil {
			continue
		}
		if m := reCond.FindStringSubmatch(line); m != nil {
			cur.providers[m[1]] = true
		}
		if m := reEffort.FindStringSubmatch(line); m != nil {
			cur.effort, _ = strconv.Atoi(m[1])
		}
		if reOr.MatchString(line) {
			cur.hasOr = true
		}
		if reAnd.MatchString(line) {
			cur.hasAnd = true
		}
	}
	flush()
	return sc.Err() == nil
}

// Evaluable returns how many rules (and effort-bearing rules) can be evaluated when only the given
// providers run. Rules with no detected condition or with other providers are not counted.
func (s RulesetStats) Evaluable(ran Providers) (rules, effortRules int) {
	add := func(class string) {
		c := s.ByClass[class]
		rules += c.Rules
		effortRules += c.EffortRules
	}
	switch {
	case ran.Java && ran.Builtin:
		add(ClassJavaOnly)
		add(ClassBuiltinOnly)
		add(ClassMixedOr)
		add(ClassMixedAnd)
	case ran.Java:
		add(ClassJavaOnly)
		add(ClassMixedOr)
	case ran.Builtin:
		add(ClassBuiltinOnly)
		add(ClassMixedOr)
	}
	return
}

// CoverageEvidence builds the KindMTACoverage item. scanErr non-nil yields an UNKNOWN item.
func CoverageEvidence(st RulesetStats, scanErr error, ran Providers) model.Evidence {
	ev := model.Evidence{
		Category: model.CatMigration, Kind: model.KindMTACoverage, Subject: "java rulesets",
		Confidence: model.Derived, Status: model.Info,
		Limitations: []string{
			"heuristic line-based reading of the installed ruleset YAML, not a parse",
			"counts rules, not incident weight: a rule that fires 500 times counts once",
			"builtin.hasTags rules depend on tags that java rules may produce, so some builtin rules lose recall without java",
			"MTA does not record rules dropped for a missing provider in output.yaml; this figure is computed independently",
		},
		Assumptions: []string{
			"a provider that failed to initialise aborts the whole MTA run (kantra 8.3.0), so providers that ran = java+builtin on success and none otherwise",
		},
	}
	if scanErr != nil {
		ev.Confidence = model.Unknown
		ev.Status = model.Warn
		ev.Finding = "ruleset coverage could not be computed: " + scanErr.Error()
		return ev
	}
	evalRules, evalEffort := st.Evaluable(ran)
	fullRules, fullEffort := st.Evaluable(Providers{Java: true, Builtin: true})
	ev.Values = map[string]string{
		"providers_ran":                 ran.String(),
		"rules_total":                   strconv.Itoa(st.RulesTotal),
		"rules_evaluable":               strconv.Itoa(evalRules),
		"effort_rules_total":            strconv.Itoa(st.EffortRulesTotal),
		"effort_rules_evaluable":        strconv.Itoa(evalEffort),
		"rules_evaluable_if_java_ran":   strconv.Itoa(fullRules),
		"effort_rules_if_java_ran":      strconv.Itoa(fullEffort),
		"rule_files":                    strconv.Itoa(st.Files),
		"rule_files_unreadable":         strconv.Itoa(st.Unreadable),
		"rules_java_only":               strconv.Itoa(st.ByClass[ClassJavaOnly].Rules),
		"rules_builtin_only":            strconv.Itoa(st.ByClass[ClassBuiltinOnly].Rules),
		"rules_mixed_or":                strconv.Itoa(st.ByClass[ClassMixedOr].Rules),
		"rules_mixed_and":               strconv.Itoa(st.ByClass[ClassMixedAnd].Rules),
		"effort_rules_builtin_only":     strconv.Itoa(st.ByClass[ClassBuiltinOnly].EffortRules),
		"effort_rules_java_only":        strconv.Itoa(st.ByClass[ClassJavaOnly].EffortRules),
		"rules_no_condition_or_foreign": strconv.Itoa(st.ByClass[ClassNoCondition].Rules + st.ByClass[ClassOtherProvider].Rules),
	}
	ev.Finding = "MTA could evaluate " + strconv.Itoa(evalRules) + " of " + strconv.Itoa(st.RulesTotal) +
		" default java rules (" + strconv.Itoa(evalEffort) + " of " + strconv.Itoa(st.EffortRulesTotal) +
		" effort-bearing) with providers " + ran.String() + " (heuristic, counts rules not incidents)"
	if evalRules < fullRules {
		ev.Status = model.Warn
	}
	return ev
}
