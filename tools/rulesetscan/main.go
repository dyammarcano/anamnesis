// Command rulesetscan streams an MTA distribution zip (e.g. mta-8.3.0-cli-windows-amd64.zip)
// and classifies every default rule by the provider capabilities its
// `when` clause uses. Line-oriented heuristic (no YAML parser in stdlib):
// a rule starts at a `ruleID:` line; a condition is any `<provider>.<cap>:` key.
package main

import (
	"archive/zip"
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

type rule struct {
	id, file  string
	providers map[string]bool
	caps      map[string]bool
	effort    string
	category  string
	hasOr     bool
	hasAnd    bool
	targets   []string
}

var (
	reRuleID = regexp.MustCompile(`^\s*-?\s*ruleID:\s*(\S+)`)
	reCond   = regexp.MustCompile(`^\s*-?\s*(java|builtin|go|nodejs|python|dotnet|csharp|yaml)\.([A-Za-z]+):`)
	reEffort = regexp.MustCompile(`^\s*effort:\s*(\d+)`)
	reCat    = regexp.MustCompile(`^\s*category:\s*(\S+)`)
	reOr     = regexp.MustCompile(`^\s*-?\s*or:\s*$`)
	reAnd    = regexp.MustCompile(`^\s*-?\s*and:\s*$`)
	reTarget = regexp.MustCompile(`konveyor\.io/target=([^\s"']+)`)
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: rulesetscan <mta-distribution.zip>")
		os.Exit(2)
	}
	path := os.Args[1]
	zr, err := zip.OpenReader(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer func() { _ = zr.Close() }()

	var rules []*rule
	files, parseErrs := 0, 0
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "rulesets/java/") || (!strings.HasSuffix(f.Name, ".yaml") && !strings.HasSuffix(f.Name, ".yml")) {
			continue
		}
		if strings.HasSuffix(f.Name, "ruleset.yaml") || strings.Contains(f.Name, "/tests/") || strings.HasSuffix(f.Name, ".test.yaml") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			parseErrs++
			continue
		}
		files++
		var cur *rule
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if m := reRuleID.FindStringSubmatch(line); m != nil {
				cur = &rule{id: m[1], file: f.Name, providers: map[string]bool{}, caps: map[string]bool{}}
				rules = append(rules, cur)
				continue
			}
			if cur == nil {
				continue
			}
			if m := reCond.FindStringSubmatch(line); m != nil {
				cur.providers[m[1]] = true
				cur.caps[m[1]+"."+m[2]] = true
			}
			if m := reEffort.FindStringSubmatch(line); m != nil {
				cur.effort = m[1]
			}
			if m := reCat.FindStringSubmatch(line); m != nil {
				cur.category = m[1]
			}
			if reOr.MatchString(line) {
				cur.hasOr = true
			}
			if reAnd.MatchString(line) {
				cur.hasAnd = true
			}
			for _, m := range reTarget.FindAllStringSubmatch(line, -1) {
				cur.targets = append(cur.targets, m[1])
			}
		}
		if err := sc.Err(); err != nil {
			parseErrs++
		}
		_ = rc.Close()
	}

	class := map[string]int{}
	capCount := map[string]int{}
	effortByClass := map[string]map[string]int{}
	targets := map[string]int{}
	catCount := map[string]int{}
	for _, r := range rules {
		var c string
		switch {
		case len(r.providers) == 0:
			c = "no-condition-detected"
		case r.providers["builtin"] && len(r.providers) == 1:
			c = "builtin-only"
		case r.providers["java"] && len(r.providers) == 1:
			c = "java-only"
		case r.providers["java"] && r.providers["builtin"] && r.hasOr && !r.hasAnd:
			c = "mixed-or (builtin part survives)"
		case r.providers["java"] && r.providers["builtin"]:
			c = "mixed-and/nested (dropped without java)"
		default:
			c = "other:" + strings.Join(keys(r.providers), "+")
		}
		class[c]++
		if effortByClass[c] == nil {
			effortByClass[c] = map[string]int{}
		}
		e := r.effort
		if e == "" {
			e = "none"
		}
		effortByClass[c][e]++
		for k := range r.caps {
			capCount[k]++
		}
		for _, t := range r.targets {
			targets[t]++
		}
		cat := r.category
		if cat == "" {
			cat = "none"
		}
		catCount[cat]++
	}

	fmt.Printf("rule files scanned: %d   unreadable: %d   rules found: %d\n\n", files, parseErrs, len(rules))
	fmt.Println("rules by provider class:")
	printSorted(class)
	fmt.Println("\ncondition capability usage (rules using it):")
	printSorted(capCount)
	fmt.Println("\nrules by category:")
	printSorted(catCount)
	fmt.Println("\neffort values by class:")
	for _, c := range keysByCount(class) {
		fmt.Printf("  %-42s %v\n", c, effortByClass[c])
	}
	fmt.Println("\ntop 25 konveyor.io/target labels (label occurrences):")
	for i, k := range keysByCount(targets) {
		if i == 25 {
			break
		}
		fmt.Printf("  %6d  %s\n", targets[k], k)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysByCount(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func printSorted(m map[string]int) {
	for _, k := range keysByCount(m) {
		fmt.Printf("  %6d  %s\n", m[k], k)
	}
}
