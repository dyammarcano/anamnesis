# Anamnesis — architecture
<!-- rev:005 (RFC 3339) 2026-10-01T18:33:16Z -->

Anamnesis is one Go executable (`anamnesis.exe`) that assesses local legacy Java
repositories and writes a defensible modernization report. It runs as a TUI with no arguments and
as a CLI with arguments; **both drive the same engine**. MTA is one evidence source among several,
never a single point of failure. Research basis: `docs/kantra-internals.md`.

## 1. Principles that shape the code

| Principle | Consequence in code |
|---|---|
| Evidence, not verdicts | Every analyzer returns `[]Evidence`; conclusions are computed later and carry the IDs of the evidence they rest on |
| A failed analyzer is evidence | `Analyze` errors become `Evidence{Status: FAILED}` with command, exit code and log reference, never a dropped result |
| Confidence is explicit | `OBSERVED` / `DERIVED` / `INFERRED` / `UNKNOWN` on every item; the report never upgrades it |
| The analyzed repo is read-only | No analyzer receives a writable handle to it; all output goes under the Anamnesis output root, which must not be inside any analyzed repo |
| Nothing repo-controlled runs without a decision | Running a build, `gradlew`, `mvn`, or MTA on a Maven/Gradle project is a **side-effect capability**: Preflight reports it, a human enables it per project |
| Windows first | Paths, tool resolution (`.exe/.bat/.cmd`, `PATHEXT`), process-tree termination (Job Objects) designed for Windows from the start |
| Bounded | One shared file walk per project; bounded worker pools; per-capability timeouts; logs streamed to disk, only tails kept in memory |

## 2. Package layout

Single module, `main` at the repo root so `go build -o anamnesis.exe` is the whole build.

```
main.go                       args → cli.Run, none → tui.Run
internal/
  model/        Project, Evidence, Confidence, Status, Severity, Location, Artifact, Conclusion
  engine/       Analyzer contract, registry, scheduler, run lifecycle, progress events
  workspace/    output root, run IDs, per-capability artifact dirs, bounded log capture
  proc/         subprocess runner: Job Object tree-kill, timeout, env allowlist, redacted logging
  redact/       secret detection and redaction for logs, env dumps, config excerpts
  scan/         single streaming walk per project → file index (paths, sizes, kinds), cached per run
  discovery/    canonical paths (Windows), parent-dir repo discovery, repository identity
  gitinfo/      read-only git facts via `git -C` (SHA, branch, dirty, remotes)
  buildsys/     build-system detection from evidence
    ant/        streaming Ant XML parser, imports, target graph, task classifier, safety model
    maven/      POM facts (modules, compiler source/target/release, packaging)
    gradle/     Groovy/Kotlin DSL text facts, wrapper detection
    eclipse/    .project, .classpath, .settings/org.eclipse.jdt.core.prefs
  javaver/      Java-version evidence from every source, conflicts kept side by side
  prereq/       prerequisites inferred from evidence; tool probing; install suggestions (never installs)
  buildrun/     opt-in local build execution, failure classification
  ci/           GitHub Actions parsing and classification; read-only `gh`; same-SHA correlation
  deps/         JAR/WAR/EAR inventory: manifests, class-file majors, duplicates (zip central dir only)
  tech/         enterprise technology inventory (imports, descriptors, server-specific files)
  migration/
    mta/        locate installation, predict failures, invoke, parse output, coverage, classify failures
    jdktools/   jdeps, jdeprscan as independent analyzers
  correlate/    conclusions with traceability (conclusion → evidence IDs → locations/commands)
  report/       two reports: report.json (canonical, machines) and report.html (self-contained, people); consolidated in both
  cli/          analyze | preflight | report subcommands
  tui/          interactive front end over engine
fixtures/       small synthetic repositories, one per scenario (Wave 1+)
```

The dependency direction is `cli`/`tui` → `engine` → analyzers → `model`. Analyzers do not import
each other; anything shared (file index, git facts) is passed in through `engine.Project`.

## 3. Analyzer contract

```go
type Analyzer interface {
    Name() string                       // stable ID, used in evidence and reruns
    Phase() Phase                       // Preflight | Deep
    Requires() []Capability             // e.g. tool:ant, side-effect:build, network:github
    Preflight(ctx context.Context, p *Project) Readiness   // cheap; may not execute repo code
    Analyze(ctx context.Context, p *Project, sink EvidenceSink) error
}
```

Differences from the prompt's sketch, and why:

- `Analyze` writes to a **sink** rather than returning a slice, so a 2M-LOC scan streams evidence and
  the TUI sees progress while it runs.
- `Readiness` (not `Capability`) is the Preflight result: `READY`, `NEEDS_TOOL`, `NEEDS_DECISION`
  (side effects), `NOT_APPLICABLE`, with the inferred prerequisites attached.
- `Requires()` lets the scheduler refuse a side-effect analyzer that the user has not enabled, before
  the analyzer runs. Analyzers cannot override this decision themselves.

## 4. Evidence model

```go
type Evidence struct {
    ID          string            // deterministic: hash(analyzer, kind, subject, location)
    Analyzer    string
    Category    Category          // BUILD, JAVA_VERSION, CI, DEPENDENCY, TECHNOLOGY, MIGRATION, ENVIRONMENT, GIT, SOURCE
    Kind        string            // e.g. "ant.javac.source", "mta.violation", "tool.missing"
    Subject     string            // what it is about: target name, rule ID, tool name, file
    Finding     string            // one human sentence
    Status      Status            // PASS, FAIL, WARN, INFO, FAILED (analyzer failure), SKIPPED
    Confidence  Confidence        // OBSERVED, DERIVED, INFERRED, UNKNOWN
    Severity    Severity          // INFO, LOW, MEDIUM, HIGH, CRITICAL
    Locations   []Location        // file, line, column; repo-relative + canonical repo root
    RuleID      string            // MTA rule, Anamnesis rule, jdeprscan API
    Target      string            // migration target label when applicable
    Effort      *Effort           // raw MTA effort only; see §6
    Command     *CommandRecord    // argv, dir, env names (no values), exit code, duration, log ref
    Artifacts   []ArtifactRef     // relative paths under the run dir
    Assumptions []string
    Limitations []string
    Values      map[string]string // typed-at-edges facts, e.g. "source":"1.6"
    ObservedAt  time.Time         // UTC
}
```

`Conclusion{ID, Statement, Kind: FACT|EVIDENCE|INFERENCE|ESTIMATE|UNKNOWN, EvidenceIDs, Confidence,
Assumptions, Limitations}` is what reports lead with. Every statement in the executive summary is a
`Conclusion`, so every sentence there traces back to evidence.

**Buildability** is modelled as two independent axes, never one flag:
`SourceBuildability` (proven locally / proven in CI for this SHA / not proven) and
`LocalReproducibility` (reproduced / failed:<class> / not attempted). The four states the prompt
names are derived from the pair.

## 5. MTA integration decision (ADR-0001)

From the source analysis:

| What | Decision | Reason |
|---|---|---|
| analyzer-lsp engine, providers, kantra code | **E – left external**; Anamnesis invokes the operator's installed MTA CLI as a subprocess | Java analysis needs JDT LS (a Java program) and `java-external-provider` regardless; embedding the engine adds ~47 module requirements (gRPC, OTel, Jaeger…) and still cannot run Java rules |
| `output.yaml` / `dependencies.yaml` schema | **C – reimplemented** as Anamnesis types with the same YAML keys (provenance P-1) | small, stable, needed to read MTA output |
| KANTRA_DIR discovery | **C – reimplemented** read-only (P-3) | to find an installation without asking the user |
| Kantra's prerequisite checks | **C – reimplemented as predictions** in Preflight | so Anamnesis reports *before* running MTA that it will fail, and why: no `mvn`, JDK < 17 / string-compare defect, no `JAVA_HOME`, no pom/gradle at root, `build.gradle.kts` only, incomplete kantra dir |
| Rule coverage | **C – Anamnesis original** (P-4) | MTA output hides rules dropped for missing providers (kantra-internals §6); Anamnesis computes coverage from the rulesets |
| Rulesets, JDT LS, static report, Maven indexes | **E – never redistributed** | licence not determinable locally (OQ-1); the operator already has them |

**Invocation contract** (Wave 5): fresh output dir owned by Anamnesis (never `--overwrite`); `cmd.Dir` = a
Anamnesis scratch dir (JDT LS writes `org.eclipse.*` there); explicit `--input`, `--output`, `--target`,
`--mode`, `--run-local`, `--disable-maven-search`, `--no-progress`; `KANTRA_DIR` set explicitly; the run
inside a Job Object with a timeout; stdout/stderr and `analysis.log` kept as artifacts; a repo-carried
`.konveyor/profiles` recorded as evidence because it can change the run.

**Routes for Ant/Eclipse repositories**, each tried only when its preconditions hold, and each
recorded as evidence whatever the outcome:
1. binary route on a **copy** of an existing WAR/EAR/JAR, never the in-repo file;
2. staged-copy route: copy sources to the run dir, generate a minimal `pom.xml`, analyze the copy, map
   incident URIs back to repo paths (hypothesis H-1, to validate on a fixture first);
3. otherwise MTA is reported `NOT_APPLICABLE` with the exact predicted error, and the native
   technology/descriptor analyzers carry the migration evidence.

## 6. Effort semantics

MTA stores one integer per rule (`Violation.Effort`); there is no total in the output. Anamnesis
records per rule: `ruleID, effort, incidents, category, target labels, affected files`. It derives
`effort × incidents` per rule and sums by rule, technology label, module and category. These are
labelled **DERIVED, MTA effort points**. Hours are never computed. The report schema reserves an
`estimates` block with a `calibration` field (`hoursPerPoint`, its source and sample size) that stays
empty until a measured calibration exists.

## 7. Output layout (ADR-0002)

```
<output-root>/assessments/<repo-name>-<8-hex of canonical path>/<run-id>/
    run.json                 run manifest: Anamnesis version, args, tools, timings
    report.json  report.html
    evidence.jsonl           every Evidence, one per line, deterministic order
    capabilities/<analyzer>/ stdout.log, stderr.log, artifacts (e.g. mta/output.yaml)
<output-root>/assessments/consolidated/<run-id>/consolidated-report.{json,html}
```

Default `<output-root>` is `%LOCALAPPDATA%\Anamnesis`, overridable with `--out`. The `run-id` is the
UTC start time `20261001T164228Z`. Anamnesis refuses an output root inside any project being analyzed.

## 8. Dependencies (ADR-0003, proposed)

| Need | Choice | Notes |
|---|---|---|
| TUI | Charm Bubble Tea (+ Lip Gloss for layout); exact module path and version pinned when added | evaluate in Wave 2 against the screen in the brief; Windows console support to be confirmed on this machine; the only large dependency |
| YAML (GitHub workflows, MTA output) | `go.yaml.in/yaml/v3` | stdlib has none |
| Windows Job Objects | `golang.org/x/sys/windows` | process-tree termination on timeout/cancel |
| XML, JSON, HTML templates, zip, logging, flags | stdlib (`encoding/xml` streaming, `html/template`, `archive/zip`, `log/slog`, `flag`) | no CLI framework: three subcommands do not need cobra |

## 9. Plan by wave, tied to the MTA research

| Wave | Delivers | Done-signal (observable) | MTA source it leans on |
|---|---|---|---|
| 1 Core | `model`, `engine` (scheduler, sink, progress), `workspace`, `proc` (Job Object), `discovery`, `scan`, `redact`; `anamnesis preflight <path>` prints identity + file inventory as JSON | `preflight` on a fixture and on this repo writes `evidence.jsonl` under the output root; repo `git status` unchanged | — |
| 2 Preflight + TUI | `gitinfo`, `buildsys` detection, `javaver`, `prereq`, CI presence, MTA-applicability prediction; TUI list/add/folder/preflight | operator launches `anamnesis.exe`, adds paths, sees build system, Java evidence, prerequisites, MTA prediction | kantra-internals §3–§5 (prediction rules), §4 (prerequisites), §10 (env vars) |
| 3 Build intelligence | Ant parser, target graph, safety classifier, Maven/Gradle facts, opt-in build runner, failure classes | dangerous-Ant fixture classified DANGEROUS with the offending task; build of a fixture produces a classified result | J:bldtool/* (what MTA itself executes, to set the same bar) |
| 4 CI intelligence | workflow parse + purpose classification, setup-java extraction, `gh` same-SHA lookup | lint-only workflow is not counted as build proof; same-SHA vs different-SHA fixtures | — |
| 5 Migration | MTA locate/predict/invoke/parse/coverage; binary and staged-copy routes; jdeps/jdeprscan | MTA fixture `output.yaml` parsed to evidence with effort intact; a failing provider classified with the exact chain from kantra-internals §5.3; Anamnesis completes the report anyway | A:output/v1/konveyor/violations.go, K:cmd/analyze/run.go, K:pkg/provider/env_local.go, J:provider.go, A:parser/rule_parser.go |
| 6 Reports | json → txt/html, consolidated | self-contained `report.html` opens from disk with no server | K:cmd/analyze/static_report.go (for comparison only) |
| 7 Hardening | large-repo performance, cancellation, redaction review, golden tests, Windows cmd/PowerShell runs | timed run on a large repository supplied by the operator | — |

## 10. Open questions

| ID | Question | Why it matters | How it gets answered |
|---|---|---|---|
| OQ-1 | Licence of the default rulesets and of the MTA distribution contents | decides whether any rule content may ever be embedded | operator, or the upstream repositories |
| OQ-2 | The static report's effort aggregation formula | whether Anamnesis's derived totals match MTA's UI | read the static-report bundle in the distribution |
| OQ-3 | Does JDT LS (as shipped in 8.3.0) parse Java 6/7 source at its declared level? | accuracy of the staged-copy route on Java 6-era code | experiment on a Java 6 fixture in Wave 5 |
| OQ-4 | Does kantra run `mvn dependency:tree` even with `--mode source-only` (it always calls `GetDependencies`, K:cmd/analyze/run.go:304)? | whether MTA on a Maven repo executes repo build config in every mode | read `A:core` GetDependencies path in Wave 5 |
| OQ-5 | Precedence between a repo-carried `.konveyor/profiles` profile and explicit CLI flags | whether an untrusted repo can override Anamnesis's flags | read `K:pkg/profile/profile.go` in Wave 5 |
| OQ-6 | Go module path / remote for this repository | `go.mod` | operator; Wave 1 uses a local path that can be renamed |
| OQ-7 | Where the operator's private target repositories are | final acceptance; public stand-in is EJBCA 4.0.16 (`docs/validation/ejbca-4.0.16.md`), which is the Wave 2 done-signal input | operator |
| H-1 | Hypothesis: a staged copy plus a synthetic `pom.xml` lets MTA's Java provider analyze an Ant repository | the main route to Java-rule evidence for Ant projects | fixture experiment, Wave 5 |
