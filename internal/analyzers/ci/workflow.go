// Package ci reads GitHub Actions workflows and, optionally, the CI result for the project's HEAD.
// "Source buildability proven in CI for this SHA" and "reproducible locally" are separate claims;
// nothing here speaks to the second.
package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dyammarcano/anamnesis/internal/engine"
)

// Purpose of a workflow, inferred from its steps.
const (
	PurposeBuild   = "BUILD"
	PurposeTest    = "TEST"
	PurposeLint    = "LINT"
	PurposeDeploy  = "DEPLOY"
	PurposeRelease = "RELEASE"
	PurposeOther   = "OTHER"
)

const maxWorkflowBytes = 2 << 20

// Workflow is the parsed, secret-free view of one workflow file.
type Workflow struct {
	File         string // repo-relative, slash
	Name         string // the workflow's `name:`; empty when absent
	Triggers     []string
	Jobs         []string
	Purpose      string
	Publishes    bool // build workflow that also deploys/releases
	JavaVersions []string
	JavaDists    []string
	SetupActions []string
	Tools        []string
	Uploads      bool
	Caches       bool
	Services     []string
	EnvNames     []string
	SecretNames  []string
	Lines        map[string]int // fact label -> first line
	ParseErr     string
}

// RunName is how GitHub names this workflow's runs: the `name:` key, else the file path.
func (w *Workflow) RunName() string {
	if w.Name != "" {
		return w.Name
	}
	return w.File
}

// LoadWorkflows reads .github/workflows/*.yml|yaml from the project (not nested directories).
func LoadWorkflows(p *engine.Project) []*Workflow {
	var out []*Workflow
	for _, f := range p.Files.Under(".github/workflows") {
		rest := strings.TrimPrefix(f.Rel, ".github/workflows/")
		if strings.Contains(rest, "/") || (f.Ext != ".yml" && f.Ext != ".yaml") {
			continue
		}
		w := &Workflow{File: f.Rel, Lines: map[string]int{}}
		if f.Size > maxWorkflowBytes {
			w.ParseErr = fmt.Sprintf("file is %d bytes, over the %d byte parse limit", f.Size, maxWorkflowBytes)
			w.Purpose = "UNKNOWN"
			out = append(out, w)
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.Root, filepath.FromSlash(f.Rel)))
		if err != nil {
			w.ParseErr = err.Error()
			w.Purpose = "UNKNOWN"
			out = append(out, w)
			continue
		}
		ParseWorkflow(w, data)
		out = append(out, w)
	}
	return out
}

var (
	secretRe  = regexp.MustCompile(`secrets\.([A-Za-z0-9_\-]+)`)
	matrixRef = regexp.MustCompile(`^\s*\$\{\{\s*matrix\.([A-Za-z0-9_\-]+)\s*\}\}\s*$`)

	reMaven       = regexp.MustCompile(`(^|[\s;&|(])(\./|\.\\)?(mvn|mvnw)(\.cmd)?(\s|$)`)
	reGradle      = regexp.MustCompile(`(^|[\s;&|(])(\./|\.\\)?(gradle|gradlew)(\.bat)?(\s|$)`)
	reAnt         = regexp.MustCompile(`(^|[\s;&|(])ant(\s|$)`)
	reJavac       = regexp.MustCompile(`(^|[\s;&|(])javac(\s|$)`)
	reDocker      = regexp.MustCompile(`(^|[\s;&|(])docker(\s|$)`)
	reMvnBuild    = regexp.MustCompile(`\b(package|install|verify|compile|assemble)\b`)
	reMvnTest     = regexp.MustCompile(`\b(test|surefire:test|failsafe:integration-test|integration-test)\b`)
	reGradleBuild = regexp.MustCompile(`\b(build|assemble|jar|war|bootJar|shadowJar|installDist|distZip|compileJava|classes)\b`)
	reGradleTest  = regexp.MustCompile(`\b(test|check|integrationTest)\b`)
	reLint        = regexp.MustCompile(`(?i)(checkstyle|spotless|spotbugs|\bpmd\b|ktlint|detekt|codeql|super-linter|\blint\b|dependency-review|sonar)`)
	reDeploy      = regexp.MustCompile(`(?i)(\bdeploy\b|docker push|kubectl|\bhelm\b|publish|aws s3|rsync|scp |gh-pages|pages-deploy|deploy-pages)`)
	reRelease     = regexp.MustCompile(`(?i)(gh release|action-gh-release|create-release|upload-release-asset|release-drafter|semantic-release)`)
	reTestWord    = regexp.MustCompile(`(?i)(\btest(s)?\b|pytest|jest|junit)`)
)

// ParseWorkflow fills w from the YAML text. A malformed file sets ParseErr and leaves Purpose UNKNOWN.
func ParseWorkflow(w *Workflow, data []byte) {
	if w.Lines == nil { // callers may pass a zero Workflow; never panic on it
		w.Lines = map[string]int{}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		w.ParseErr = err.Error()
		w.Purpose = "UNKNOWN"
		return
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		w.ParseErr = "top level is not a mapping"
		w.Purpose = "UNKNOWN"
		return
	}
	root := doc.Content[0]

	if n := mapGet(root, "name"); n != nil && n.Kind == yaml.ScalarNode {
		w.Name = n.Value
	}
	triggerNode := mapGet(root, "on")
	if triggerNode == nil {
		triggerNode = mapGet(root, "true") // YAML 1.1 readers turn `on` into a boolean key
	}
	w.Triggers = triggerNames(triggerNode)
	if triggerNode != nil {
		w.Lines["triggers"] = triggerNode.Line
	}

	sets := newSets()
	envNames(mapGet(root, "env"), sets.env)
	collectSecrets(string(data), sets.secrets)

	var cmds []string // every run command line, for purpose inference
	var uses []string // every action reference
	if jobs := mapGet(root, "jobs"); jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			id, job := jobs.Content[i].Value, jobs.Content[i+1]
			w.Jobs = append(w.Jobs, id)
			if job.Kind != yaml.MappingNode {
				continue
			}
			matrix := matrixValues(job)
			envNames(mapGet(job, "env"), sets.env)
			if u := mapGet(job, "uses"); u != nil { // reusable workflow call
				uses = append(uses, u.Value)
				sets.setup[u.Value] = true
			}
			if sv := mapGet(job, "services"); sv != nil && sv.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(sv.Content); j += 2 {
					label := sv.Content[j].Value
					if img := mapGet(sv.Content[j+1], "image"); img != nil {
						label += " (" + img.Value + ")"
					}
					sets.services[label] = true
					w.setLine("services", sv.Content[j].Line)
				}
			}
			if c := mapGet(job, "container"); c != nil {
				label := "container"
				if c.Kind == yaml.ScalarNode {
					label += " (" + c.Value + ")"
				} else if img := mapGet(c, "image"); img != nil {
					label += " (" + img.Value + ")"
				}
				sets.services[label] = true
				w.setLine("services", c.Line)
			}
			steps := mapGet(job, "steps")
			if steps == nil || steps.Kind != yaml.SequenceNode {
				continue
			}
			for _, st := range steps.Content {
				if st.Kind != yaml.MappingNode {
					continue
				}
				envNames(mapGet(st, "env"), sets.env)
				if u := mapGet(st, "uses"); u != nil {
					uses = append(uses, u.Value)
					w.stepUses(u, mapGet(st, "with"), matrix, sets)
				}
				if r := mapGet(st, "run"); r != nil {
					for k, line := range strings.Split(r.Value, "\n") {
						line = strings.TrimSpace(line)
						if line == "" || strings.HasPrefix(line, "#") {
							continue
						}
						cmds = append(cmds, line)
						w.noteTools(line, r.Line+k+boolInt(r.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0), sets)
					}
				}
			}
		}
	}

	w.JavaVersions = sorted(sets.javaVer)
	w.JavaDists = sorted(sets.javaDist)
	w.SetupActions = sorted(sets.setup)
	w.Tools = sorted(sets.tools)
	w.Services = sorted(sets.services)
	w.EnvNames = sorted(sets.env)
	w.SecretNames = sorted(sets.secrets)
	w.Uploads = sets.upload
	w.Caches = sets.cache
	w.Purpose, w.Publishes = inferPurpose(w, cmds, uses)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (w *Workflow) setLine(label string, line int) {
	if _, ok := w.Lines[label]; !ok && line > 0 {
		w.Lines[label] = line
	}
}

type sets struct {
	javaVer, javaDist, setup, tools, services, env, secrets map[string]bool
	upload, cache                                           bool
}

func newSets() *sets {
	return &sets{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, false, false}
}

func (w *Workflow) stepUses(u, with *yaml.Node, matrix map[string][]string, s *sets) {
	action := strings.ToLower(u.Value)
	base := action
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	switch {
	case strings.HasSuffix(base, "/setup-java"):
		w.setLine("setup-java", u.Line)
		s.setup[base] = true
		if with != nil {
			for _, v := range resolveValues(mapGet(with, "java-version"), matrix) {
				s.javaVer[v] = true
			}
			if f := mapGet(with, "java-version-file"); f != nil {
				s.javaVer["file:"+f.Value] = true
			}
			if d := mapGet(with, "distribution"); d != nil {
				for _, v := range resolveValues(d, matrix) {
					s.javaDist[v] = true
				}
			}
		}
	case strings.Contains(base, "/setup-") || strings.HasPrefix(base, "setup-"):
		s.setup[base] = true
		w.setLine("setup", u.Line)
	case strings.Contains(base, "upload-artifact"):
		s.upload = true
		w.setLine("upload-artifact", u.Line)
	case strings.HasSuffix(base, "/cache") || strings.Contains(base, "/cache/"):
		s.cache = true
		w.setLine("cache", u.Line)
	case strings.HasPrefix(base, "docker/"):
		s.tools["docker"] = true
		w.setLine("docker", u.Line)
	}
	if with != nil {
		if c := mapGet(with, "cache"); c != nil && strings.Contains(base, "setup-") {
			s.cache = true
			w.setLine("cache", c.Line)
		}
	}
}

func (w *Workflow) noteTools(line string, lineNo int, s *sets) {
	for name, re := range map[string]*regexp.Regexp{"mvn": reMaven, "gradle": reGradle, "ant": reAnt, "javac": reJavac, "docker": reDocker} {
		if re.MatchString(line) {
			s.tools[name] = true
			w.setLine("cmd:"+name, lineNo)
		}
	}
}

// inferPurpose classifies from commands and actions; the result is an inference, not a fact.
func inferPurpose(w *Workflow, cmds, uses []string) (purpose string, publishes bool) {
	var build, test, lint, deploy, release bool
	for _, c := range cmds {
		switch {
		case reMaven.MatchString(c):
			if reMvnBuild.MatchString(c) {
				build = true
			}
			if strings.Contains(c, "deploy") && !strings.Contains(c, "-DskipDeploy") {
				deploy = true
				build = true // mvn deploy runs the package phase first
			}
			if reMvnTest.MatchString(c) {
				test = true
			}
		case reGradle.MatchString(c):
			if reGradleBuild.MatchString(c) {
				build = true
			}
			if reGradleTest.MatchString(c) {
				test = true
			}
		case reAnt.MatchString(c):
			if !regexp.MustCompile(`(^|\s)-(version|projecthelp|p)(\s|$)`).MatchString(c) {
				build = true
			}
		case reJavac.MatchString(c):
			build = true
		}
		if reLint.MatchString(c) {
			lint = true
		}
		if reDeploy.MatchString(c) {
			deploy = true
		}
		if reRelease.MatchString(c) {
			release = true
		}
		if !build && reTestWord.MatchString(c) {
			test = true
		}
	}
	for _, u := range uses {
		switch {
		case reLint.MatchString(u):
			lint = true
		case reRelease.MatchString(u):
			release = true
		case reDeploy.MatchString(u):
			deploy = true
		}
	}
	for _, t := range w.Triggers {
		if t == "release" {
			release = true
		}
	}
	switch {
	case build:
		return PurposeBuild, deploy || release
	case release:
		return PurposeRelease, false
	case deploy:
		return PurposeDeploy, false
	case test:
		return PurposeTest, false
	case lint:
		return PurposeLint, false
	}
	return PurposeOther, false
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func triggerNames(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	switch n.Kind {
	case yaml.ScalarNode:
		out = append(out, n.Value)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
	}
	return out
}

// envNames records variable NAMES only; values are never read.
func envNames(n *yaml.Node, into map[string]bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		into[n.Content[i].Value] = true
	}
}

func collectSecrets(text string, into map[string]bool) {
	for _, m := range secretRe.FindAllStringSubmatch(text, -1) {
		into[m[1]] = true
	}
}

// matrixValues returns strategy.matrix keys with their scalar list values (include/exclude ignored).
func matrixValues(job *yaml.Node) map[string][]string {
	out := map[string][]string{}
	m := mapGet(mapGet(job, "strategy"), "matrix")
	if m == nil || m.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i].Value, m.Content[i+1]
		if v.Kind == yaml.SequenceNode {
			for _, c := range v.Content {
				if c.Kind == yaml.ScalarNode {
					out[k] = append(out[k], c.Value)
				}
			}
		}
	}
	return out
}

// resolveValues returns the scalar, or the matrix list when it is a ${{ matrix.X }} reference. An
// unresolvable expression is kept verbatim so the report shows it was not resolved.
func resolveValues(n *yaml.Node, matrix map[string][]string) []string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil
	}
	if m := matrixRef.FindStringSubmatch(n.Value); m != nil {
		if vals, ok := matrix[m[1]]; ok && len(vals) > 0 {
			return vals
		}
	}
	return []string{n.Value}
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
