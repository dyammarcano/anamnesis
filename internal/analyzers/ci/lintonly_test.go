package ci

import "testing"

// The brief's explicit case: a successful lint-only workflow on the same SHA is not build evidence.

const lintOnly = `name: Lint
on: [push]
jobs:
  checkstyle:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-java@v4
        with: {java-version: '17', distribution: temurin}
      - run: mvn -B checkstyle:check spotless:check
`

const buildWf = `name: Build
on: [push]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: mvn -B package
`

func parsed(t *testing.T, name, body string) *Workflow {
	t.Helper()
	w := &Workflow{}
	ParseWorkflow(w, []byte(body))
	if w.Name == "" {
		w.Name = name
	}
	return w
}

func TestLintOnlyWorkflowIsNotBuild(t *testing.T) {
	if w := parsed(t, "Lint", lintOnly); w.Purpose == PurposeBuild {
		t.Fatalf("lint-only workflow classified %s; it must never count as a build", w.Purpose)
	}
	if w := parsed(t, "Build", buildWf); w.Purpose != PurposeBuild {
		t.Fatalf("mvn package workflow classified %s, want %s", w.Purpose, PurposeBuild)
	}
}

func TestLintOnlySuccessIsNotSameSHABuildProof(t *testing.T) {
	lint := parsed(t, "Lint", lintOnly)
	byRun := map[string]*Workflow{"Lint": lint}
	runs := []ghRun{{WorkflowName: "Lint", Conclusion: "success", Status: "completed", HeadSHA: "abc123"}}
	state, pick, _ := decide(runs, byRun)
	if state == StateSuccessSameSHA {
		t.Fatalf("state = %s from a lint-only success (picked %+v); a lint run is not a build", state, pick)
	}
	// and the same SHA with a real build success does count
	byRun["Build"] = parsed(t, "Build", buildWf)
	runs = append(runs, ghRun{WorkflowName: "Build", Conclusion: "success", Status: "completed", HeadSHA: "abc123"})
	if state, _, _ := decide(runs, byRun); state != StateSuccessSameSHA {
		t.Fatalf("state = %s, want %s when a BUILD workflow succeeded on the same SHA", state, StateSuccessSameSHA)
	}
}
