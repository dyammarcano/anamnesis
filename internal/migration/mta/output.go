// Package mta runs the operator's installed MTA (Konveyor kantra) CLI as an external component and
// turns its output into Anamnesis evidence. Nothing of MTA is embedded or redistributed.
package mta

// The types in this file are a schema-compatible reimplementation of konveyor/analyzer-lsp
// output/v1 (Apache-2.0), provenance P-1. Same YAML keys, own Go types, only the fields Anamnesis
// reads, no sorting or marshalling methods.

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// RuleSet is one element of the top-level list in output.yaml.
type RuleSet struct {
	Name        string               `yaml:"name,omitempty"`
	Description string               `yaml:"description,omitempty"`
	Tags        []string             `yaml:"tags,omitempty"`
	Violations  map[string]Violation `yaml:"violations,omitempty"`
	Insights    map[string]Violation `yaml:"insights,omitempty"`
	Errors      map[string]string    `yaml:"errors,omitempty"`
	Unmatched   []string             `yaml:"unmatched,omitempty"`
	Skipped     []string             `yaml:"skipped,omitempty"`
}

// Violation is one rule's result. Effort is the rule's story-point value, per incident.
type Violation struct {
	Description string     `yaml:"description"`
	Category    string     `yaml:"category,omitempty"`
	Labels      []string   `yaml:"labels,omitempty"`
	Incidents   []Incident `yaml:"incidents"`
	Links       []Link     `yaml:"links,omitempty"`
	Effort      *int       `yaml:"effort,omitempty"`
}

// Link is a reference attached to a violation.
type Link struct {
	URL   string `yaml:"url"`
	Title string `yaml:"title,omitempty"`
}

// Incident is one place a rule matched.
type Incident struct {
	URI        string         `yaml:"uri"`
	Message    string         `yaml:"message"`
	CodeSnip   string         `yaml:"codeSnip,omitempty"`
	LineNumber *int           `yaml:"lineNumber,omitempty"`
	Variables  map[string]any `yaml:"variables,omitempty"`
}

// DepsFlatItem is one element of dependencies.yaml.
type DepsFlatItem struct {
	FileURI      string `yaml:"fileURI"`
	Provider     string `yaml:"provider"`
	Dependencies []Dep  `yaml:"dependencies"`
}

// Dep is a resolved dependency. Type is the Maven scope.
type Dep struct {
	Name     string   `yaml:"name,omitempty"`
	Version  string   `yaml:"version,omitempty"`
	Type     string   `yaml:"type,omitempty"`
	Indirect bool     `yaml:"indirect,omitempty"`
	Labels   []string `yaml:"labels,omitempty"`
}

// ParseOutput reads output.yaml from r, streaming document by document.
func ParseOutput(r io.Reader) ([]RuleSet, error) {
	var out []RuleSet
	dec := yaml.NewDecoder(r)
	for {
		var batch []RuleSet
		err := dec.Decode(&batch)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("decode output.yaml: %w", err)
		}
		out = append(out, batch...)
	}
}

// ParseOutputFile reads output.yaml at path.
func ParseOutputFile(path string) ([]RuleSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseOutput(f)
}

// ParseDependencies reads dependencies.yaml from r.
func ParseDependencies(r io.Reader) ([]DepsFlatItem, error) {
	var out []DepsFlatItem
	dec := yaml.NewDecoder(r)
	for {
		var batch []DepsFlatItem
		err := dec.Decode(&batch)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("decode dependencies.yaml: %w", err)
		}
		out = append(out, batch...)
	}
}

// ParseDependenciesFile reads dependencies.yaml at path.
func ParseDependenciesFile(path string) ([]DepsFlatItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ParseDependencies(f)
}

var winDrivePath = regexp.MustCompile(`^/[A-Za-z]:`)

// URIToPath converts an incident URI (file:///C:/x/y or a plain path) to a slash-separated path.
// ok is false when the URI is not a file reference.
func URIToPath(uri string) (path string, ok bool) {
	if uri == "" {
		return "", false
	}
	if !strings.Contains(uri, "://") {
		return strings.ReplaceAll(uri, `\`, "/"), true
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if u.Host != "" && u.Host != "localhost" { // UNC: file://host/share/x
		p = "//" + u.Host + p
	}
	if winDrivePath.MatchString(p) {
		p = p[1:]
	}
	return p, true
}
