# Anamnesis
<!-- rev:004 (RFC 3339) 2026-10-01T18:33:16Z -->

One Windows executable that assesses local legacy Java repositories (Ant, Maven, Gradle, Eclipse/NetBeans,
hand-managed JARs) and writes an evidence-backed modernization report. Every conclusion carries the
evidence it rests on, its confidence (OBSERVED / DERIVED / INFERRED / UNKNOWN) and its limitations.
MTA (Migration Toolkit for Applications) is one evidence source. Its failure is recorded as evidence and
never stops the assessment.

It works on any local legacy Java repository. Nothing in it is specific to one project or
organisation. It never modifies the repositories it analyses: builds and MTA run on staged copies, and
all output goes to a directory outside them.

## Requirements

- Windows 10/11. Other platforms are not supported yet.
- To build: Go 1.26 or newer.
- Optional, used when present or installed on demand:
  - [scoop](https://scoop.sh), which installs missing Ant, Maven, Gradle, gh and a matching JDK;
  - git;
  - the GitHub CLI `gh`, for same-commit CI evidence (read-only);
  - a JDK 11+ for `jdeps`/`jdeprscan`;
  - an extracted MTA (Migration Toolkit for Applications) distribution.

## Build

```
go build -o anamnesis.exe .
```

The executable needs no Go toolchain to run.

## Configure

Everything comes from `parameters.yaml`; Anamnesis reads no environment variables for configuration.
Copy `parameters.example.yaml` to `parameters.yaml` (next to the exe or in the working directory)
and set:

- `output_dir`: where assessments go. It must be outside every analyzed project.
- `projects`: repositories to assess. They are never modified.
- `environment`: install missing global tools with scoop, plus a JDK matching the project's
  declared target. `restore_user_env` puts the user-level `JAVA_HOME`/`Path` back afterwards.
- `build.allow`, `mta.allow`, `network.allow`: side effects. All are off unless enabled.
  Builds and MTA always run on a staged copy, never in the repository.
- `mta.install_dir`: an extracted MTA distribution (contains `rulesets/`, `jdtls/`, `static-report/`).

## Run

```
anamnesis.exe                       # interactive TUI
anamnesis.exe preflight             # fast, read-only; never installs, never runs project code
anamnesis.exe analyze               # preflight → prepare (installs) → deep analysis
anamnesis.exe analyze --only ant-safety,javaver
anamnesis.exe report <run-dir>      # re-render reports from evidence.jsonl
anamnesis.exe discover <parent-dir> # list candidate repositories
```

Each run writes `<output_dir>/<name>-<id>/<UTC run id>/` with `evidence.jsonl`, `run.json`,
two reports, `report.json` (machine-readable) and `report.html` (self-contained, for people), and per-capability logs. A run over
several projects also writes `<output_dir>/consolidated/<run id>/consolidated-report.{json,html}`.

## Validation tools

- `go run ./tools/treedigest <dir> <manifest>` prints a digest of a directory tree. Run it before
  and after an assessment to prove the analyzed repository was not modified.
- `go run ./tools/rulesetscan <mta-distribution.zip>` classifies MTA's default rules by the
  providers they need, which shows how much coverage remains when MTA's Java provider cannot start.

## Documentation

- Architecture: `docs/architecture.md`
- MTA/Kantra 8.3.0 internals: `docs/kantra-internals.md`
- Decisions: `docs/decisions/`
- Third-party provenance and licences: `docs/third-party-provenance.md`
- Validation against real legacy code: `docs/validation/`

## License

BSD 3-Clause; see `LICENSE`. Third-party material and its licences are listed in
`docs/third-party-provenance.md`. No MTA/Kantra code, rules or binaries are included.
