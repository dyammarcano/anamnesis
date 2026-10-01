// Package config loads parameters.yaml, the ONLY source of Anamnesis configuration.
// Anamnesis reads no environment variables for configuration. The machine environment may still be
// observed and reported as evidence, but it never changes what Anamnesis does.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FileName is the parameters file name.
const FileName = "parameters.yaml"

type Project struct {
	Path    string `yaml:"path"`
	Name    string `yaml:"name,omitempty"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil → true
}

func (p Project) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Environment controls preparation of the machine. Tools (git, ant, mvn, gradle, gh, …) are global:
// resolved from PATH. When one is missing and InstallMissing is true, Anamnesis installs it with scoop
// (after preflight has recorded the environment as found).
type Environment struct {
	InstallMissing bool `yaml:"install_missing"`
	InstallJDKs    bool `yaml:"install_jdks"` // also install a JDK matching the project's declared target
	// RestoreUserEnv puts the user-level JAVA_HOME and PATH back to their values from before a JDK
	// install (scoop JDK packages change both globally). Anamnesis passes JAVA_HOME explicitly anyway.
	RestoreUserEnv bool     `yaml:"restore_user_env"`
	ScoopBuckets   []string `yaml:"scoop_buckets"` // added before installing, e.g. [main, java]
}

// JDK is a named JDK installation the operator makes available to Anamnesis.
type JDK struct {
	Name string `yaml:"name"`
	Home string `yaml:"home"`
}

type Build struct {
	Allow          bool   `yaml:"allow"`           // side effect: run the project's build in a staged copy
	JDK            string `yaml:"jdk"`             // jdks[].name used as JAVA_HOME for builds; "" → not set
	TimeoutMinutes int    `yaml:"timeout_minutes"` // 0 → 20
}

type MTA struct {
	Allow                    bool     `yaml:"allow"`       // side effect: run MTA (may run mvn/gradlew of the project)
	InstallDir               string   `yaml:"install_dir"` // MTA distribution dir: rulesets/, jdtls/, static-report/
	Executable               string   `yaml:"executable"`  // "" → <install_dir>/windows-mta-cli.exe | mta-cli(.exe) | kantra(.exe)
	JDK                      string   `yaml:"jdk"`         // jdks[].name passed as JAVA_HOME to MTA (needs 17+)
	Targets                  []string `yaml:"targets"`     // e.g. openjdk17, eap8, jakarta-ee
	Mode                     string   `yaml:"mode"`        // source-only | full; "" → source-only
	TimeoutMinutes           int      `yaml:"timeout_minutes"`
	ForceWhenPredictedToFail bool     `yaml:"force_when_predicted_to_fail"`
	JvmMaxMem                string   `yaml:"jvm_max_mem"` // passed to MTA explicitly, e.g. "4g"
}

type Network struct {
	Allow bool `yaml:"allow"` // read-only GitHub queries via gh
}

type Analysis struct {
	Concurrency            int `yaml:"concurrency"`              // 0 → 4
	AnalyzerTimeoutMinutes int `yaml:"analyzer_timeout_minutes"` // 0 → 30
}

// Parameters is the whole file.
type Parameters struct {
	OutputDir     string      `yaml:"output_dir"`
	Projects      []Project   `yaml:"projects"`
	DiscoverRoots []string    `yaml:"discover_roots"`
	Environment   Environment `yaml:"environment"`
	JDKs          []JDK       `yaml:"jdks"`
	Build         Build       `yaml:"build"`
	MTA           MTA         `yaml:"mta"`
	Network       Network     `yaml:"network"`
	Analysis      Analysis    `yaml:"analysis"`

	// Path is where the file was loaded from (absolute). Relative paths in the file resolve against
	// its directory.
	Path string `yaml:"-"`
	// Created is true when this run wrote the file as a first-run starter (FindOrCreate).
	Created bool `yaml:"-"`
}

// Find locates parameters.yaml: explicit path, else the working directory, else next to the
// executable.
func Find(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	candidates := candidatePaths()
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s not found (looked in: %s)", FileName, strings.Join(candidates, ", "))
}

// candidatePaths is the search order: working directory, next to the executable, then the per-user
// location (UserPath). Locating the file uses the OS's standard per-user directory; no setting is
// read from the environment.
func candidatePaths() []string {
	var c []string
	if wd, err := os.Getwd(); err == nil {
		c = append(c, filepath.Join(wd, FileName))
	}
	if exe, err := os.Executable(); err == nil {
		c = append(c, filepath.Join(filepath.Dir(exe), FileName))
	}
	if u, err := UserPath(); err == nil {
		c = append(c, u)
	}
	return c
}

// UserPath is the per-user parameters file: %APPDATA%\Anamnesis\parameters.yaml on Windows.
func UserPath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "Anamnesis", FileName), nil
}

// FindOrCreate finds parameters.yaml; when none exists anywhere in the search order (and none was
// named explicitly) it writes a commented starter file at UserPath, so a first run — e.g. after
// `go install`, where no example file sits next to the binary — works without manual setup.
// The starter enables no side effects. created reports whether the file was just written.
func FindOrCreate(explicit string) (path string, created bool, err error) {
	if explicit != "" {
		p, err := filepath.Abs(explicit)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Stat(p); err != nil {
			return "", false, fmt.Errorf("parameters file %s: %w", p, err)
		}
		return p, false, nil
	}
	if p, err := Find(""); err == nil {
		return p, false, nil
	}
	up, err := UserPath()
	if err != nil {
		return "", false, fmt.Errorf("%s not found and no per-user directory is available: %w", FileName, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(up), 0o755); err != nil {
		return "", false, err
	}
	body := strings.ReplaceAll(starterTemplate, "{{OUTPUT_DIR}}", filepath.Join(home, "Anamnesis", "assessments"))
	if err := os.WriteFile(up, []byte(body), 0o644); err != nil {
		return "", false, err
	}
	return up, true, nil
}

// starterTemplate is written on first run. It must stay valid for Load (KnownFields is strict).
const starterTemplate = `# Anamnesis parameters - the only configuration source (no environment variables).
# Created automatically on first run. Edit freely; relative paths resolve against this file's folder.

# Where assessments are written. Must be outside every analyzed project.
output_dir: {{OUTPUT_DIR}}

# Repositories to assess (never modified). Add them here or press A in the TUI.
projects: []

# Parent folders the TUI (key F) and "anamnesis discover" scan for repositories.
discover_roots: []

# Missing global tools (ant, mvn, gradle, gh) and a JDK matching the project's declared Java target
# can be installed with scoop after preflight has recorded the environment as found.
environment:
  install_missing: false
  install_jdks: false
  restore_user_env: true
  scoop_buckets: [main, java]

# JDKs available on this machine, used where a section below names one.
jdks: []

# Local build experiment: runs the project's build in a staged copy, never in the repository.
build:
  allow: false
  jdk: ""
  timeout_minutes: 20

# MTA (Migration Toolkit for Applications): set install_dir to an extracted MTA distribution.
mta:
  allow: false
  install_dir: ""
  executable: ""
  jdk: ""
  targets: [openjdk17, eap8, jakarta-ee]
  mode: source-only
  timeout_minutes: 60
  force_when_predicted_to_fail: false
  jvm_max_mem: ""

# Read-only GitHub queries through gh (same-commit CI evidence).
network:
  allow: false

analysis:
  concurrency: 4
  analyzer_timeout_minutes: 30
`

// Load reads and validates the file and applies defaults.
func Load(path string) (*Parameters, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var p Parameters
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // a misspelled key is an error, not a silently ignored setting
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	p.Path = abs
	p.resolve()
	return &p, p.Validate()
}

func (p *Parameters) dir() string { return filepath.Dir(p.Path) }

func (p *Parameters) abs(s string) string {
	if s == "" || filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(p.dir(), s)
}

func (p *Parameters) resolve() {
	p.OutputDir = p.abs(p.OutputDir)
	for i := range p.Projects {
		p.Projects[i].Path = p.abs(strings.Trim(strings.TrimSpace(p.Projects[i].Path), `"'`))
	}
	for i := range p.DiscoverRoots {
		p.DiscoverRoots[i] = p.abs(p.DiscoverRoots[i])
	}
	for i := range p.JDKs {
		p.JDKs[i].Home = p.abs(p.JDKs[i].Home)
	}
	p.MTA.InstallDir = p.abs(p.MTA.InstallDir)
	p.MTA.Executable = p.abs(p.MTA.Executable)
	if p.Build.TimeoutMinutes <= 0 {
		p.Build.TimeoutMinutes = 20
	}
	if p.MTA.TimeoutMinutes <= 0 {
		p.MTA.TimeoutMinutes = 60
	}
	if p.MTA.Mode == "" {
		p.MTA.Mode = "source-only"
	}
	if p.Analysis.Concurrency <= 0 {
		p.Analysis.Concurrency = 4
	}
	if p.Analysis.AnalyzerTimeoutMinutes <= 0 {
		p.Analysis.AnalyzerTimeoutMinutes = 30
	}
}

// Validate checks internal consistency. Missing tools are not errors; they become evidence.
func (p *Parameters) Validate() error {
	var errs []string
	if p.OutputDir == "" {
		errs = append(errs, "output_dir is required")
	}
	if p.MTA.Mode != "source-only" && p.MTA.Mode != "full" {
		errs = append(errs, "mta.mode must be source-only or full")
	}
	names := map[string]bool{}
	for _, j := range p.JDKs {
		if j.Name == "" || j.Home == "" {
			errs = append(errs, "every jdks entry needs name and home")
		}
		names[j.Name] = true
	}
	if p.Build.JDK != "" && !names[p.Build.JDK] {
		errs = append(errs, fmt.Sprintf("build.jdk %q is not defined in jdks", p.Build.JDK))
	}
	if p.MTA.JDK != "" && !names[p.MTA.JDK] {
		errs = append(errs, fmt.Sprintf("mta.jdk %q is not defined in jdks", p.MTA.JDK))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s: %s", p.Path, strings.Join(errs, "; "))
	}
	return nil
}

// JDKHome returns the home of a named JDK, or "".
func (p *Parameters) JDKHome(name string) string {
	for _, j := range p.JDKs {
		if j.Name == name {
			return j.Home
		}
	}
	return ""
}

// Tool always returns "": tools are global and resolved from PATH (operator decision, ADR-0005).
// Kept so analyzers have one call site if per-tool overrides are ever reintroduced.
func (p *Parameters) Tool(name string) string { return "" }

// AddJDK registers a JDK (e.g. one Anamnesis just installed) if no entry with that home exists.
func (p *Parameters) AddJDK(name, home string) {
	for _, j := range p.JDKs {
		if strings.EqualFold(filepath.Clean(j.Home), filepath.Clean(home)) {
			return
		}
	}
	p.JDKs = append(p.JDKs, JDK{Name: name, Home: home})
}

// Save writes the parameters back (used by the TUI when projects are added or removed).
func (p *Parameters) Save() error {
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	header := "# Anamnesis parameters — the only configuration source (no environment variables).\n"
	return os.WriteFile(p.Path, append([]byte(header), b...), 0o644)
}
