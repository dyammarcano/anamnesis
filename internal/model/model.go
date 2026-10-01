// Package model holds Anamnesis's normalized evidence model. Every analyzer speaks only in
// these types, so correlation and reporting never depend on which tool produced a fact.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Confidence says how a fact is known. It is never upgraded downstream.
type Confidence string

const (
	Observed Confidence = "OBSERVED" // read directly from a file, command or API
	Derived  Confidence = "DERIVED"  // computed deterministically from observed facts (e.g. property substitution)
	Inferred Confidence = "INFERRED" // a judgement over several facts that could be wrong
	Unknown  Confidence = "UNKNOWN"  // could not be established; the reason belongs in Limitations
)

// Status is the outcome of one piece of evidence.
type Status string

const (
	Pass    Status = "PASS"
	Fail    Status = "FAIL" // the subject failed a check (e.g. tool missing)
	Warn    Status = "WARN"
	Info    Status = "INFO"
	Failed  Status = "FAILED" // the analyzer itself failed; Finding carries the error
	Skipped Status = "SKIPPED"
)

type Severity string

const (
	SevInfo     Severity = "INFO"
	SevLow      Severity = "LOW"
	SevMedium   Severity = "MEDIUM"
	SevHigh     Severity = "HIGH"
	SevCritical Severity = "CRITICAL"
)

type Category string

const (
	CatSource      Category = "SOURCE"
	CatBuild       Category = "BUILD"
	CatJavaVersion Category = "JAVA_VERSION"
	CatCI          Category = "CI"
	CatDependency  Category = "DEPENDENCY"
	CatTechnology  Category = "TECHNOLOGY"
	CatMigration   Category = "MIGRATION"
	CatEnvironment Category = "ENVIRONMENT"
	CatGit         Category = "GIT"
	CatSafety      Category = "SAFETY"
	CatAnalyzer    Category = "ANALYZER" // analyzer lifecycle: failures, timings
)

// Location points into the analyzed project. Path is repo-relative with forward slashes.
type Location struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// CommandRecord describes an executed subprocess. Env carries variable NAMES only, never values.
type CommandRecord struct {
	Argv       []string `json:"argv"`
	Dir        string   `json:"dir"`
	EnvNames   []string `json:"envNames,omitempty"`
	ExitCode   int      `json:"exitCode"`
	DurationMs int64    `json:"durationMs"`
	TimedOut   bool     `json:"timedOut,omitempty"`
	LogRef     string   `json:"logRef,omitempty"` // path relative to the run dir
	Err        string   `json:"err,omitempty"`    // start/wait error, e.g. executable not found
}

// Effort carries raw MTA effort semantics; it is never converted to hours here.
type Effort struct {
	Points    int `json:"points"`    // the rule's effort value, as MTA states it
	Incidents int `json:"incidents"` // number of incidents of that rule
}

type Evidence struct {
	ID          string            `json:"id"`
	Analyzer    string            `json:"analyzer"`
	Category    Category          `json:"category"`
	Kind        string            `json:"kind"`
	Subject     string            `json:"subject"`
	Finding     string            `json:"finding"`
	Status      Status            `json:"status"`
	Confidence  Confidence        `json:"confidence"`
	Severity    Severity          `json:"severity,omitempty"`
	Locations   []Location        `json:"locations,omitempty"`
	RuleID      string            `json:"ruleId,omitempty"`
	Target      string            `json:"target,omitempty"`
	Effort      *Effort           `json:"effort,omitempty"`
	Command     *CommandRecord    `json:"command,omitempty"`
	Artifacts   []string          `json:"artifacts,omitempty"` // paths relative to the run dir
	Assumptions []string          `json:"assumptions,omitempty"`
	Limitations []string          `json:"limitations,omitempty"`
	Values      map[string]string `json:"values,omitempty"`
	ObservedAt  time.Time         `json:"-"` // timing lives in run.json so evidence.jsonl is deterministic
}

// Finalize fills the deterministic ID and defaults. The engine calls it on every emitted item.
func (e *Evidence) Finalize() {
	if e.Severity == "" {
		e.Severity = SevInfo
	}
	if e.Confidence == "" {
		e.Confidence = Unknown
	}
	if e.ID == "" {
		loc := ""
		if len(e.Locations) > 0 {
			loc = e.Locations[0].Path + ":" + itoa(e.Locations[0].Line)
		}
		sum := sha256.Sum256([]byte(e.Analyzer + "|" + e.Kind + "|" + e.Subject + "|" + loc))
		e.ID = hex.EncodeToString(sum[:])[:16]
	}
	if e.ObservedAt.IsZero() {
		e.ObservedAt = time.Now().UTC()
	}
}

// ConclusionKind separates what is known from what is concluded (report semantics).
type ConclusionKind string

const (
	KindFact      ConclusionKind = "FACT"
	KindEvidence  ConclusionKind = "EVIDENCE"
	KindInference ConclusionKind = "INFERENCE"
	KindEstimate  ConclusionKind = "ESTIMATE"
	KindUnknown   ConclusionKind = "UNKNOWN"
)

// Conclusion is a reportable statement that must trace to evidence IDs.
type Conclusion struct {
	ID          string         `json:"id"`
	Topic       string         `json:"topic"` // e.g. "buildability.source", "java.generation"
	Statement   string         `json:"statement"`
	Value       string         `json:"value,omitempty"` // machine value, e.g. NOT_DISPROVEN
	Kind        ConclusionKind `json:"kind"`
	Confidence  Confidence     `json:"confidence"`
	EvidenceIDs []string       `json:"evidenceIds"`
	Assumptions []string       `json:"assumptions,omitempty"`
	Limitations []string       `json:"limitations,omitempty"`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
