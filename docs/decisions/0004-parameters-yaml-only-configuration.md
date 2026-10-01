# ADR-0004: parameters.yaml is the only configuration source

- Date: 2026-10-01
- Status: accepted (operator decision)

## Decision

Anamnesis reads no environment variables for configuration. Everything it needs lives in
`parameters.yaml` (`internal/config`): projects, `output_dir`, discover roots, tool executables,
named JDKs, local-build and MTA settings, side-effect permissions (build / MTA / network), and
timeouts. The file is found via `--parameters`, else the working directory, else next to the
executable. Unknown keys are rejected, so a misspelled setting is never silently ignored. The TUI
persists project additions and removals to the same file.

- Child processes receive `JAVA_HOME`, `KANTRA_DIR` and the like **set explicitly** from parameters,
  never inherited as a decision input.
- The machine environment may still be **observed and reported** as evidence (e.g. "JAVA_HOME is set
  in the process environment"), labelled as not used by Anamnesis.
- Assumed exception, pending operator confirmation: a tool not configured under `tools:` may be
  resolved from `PATH`, and the evidence records `resolved_from: PATH (not configured)`.

## Consequences

Runs are reproducible from one reviewable file. `parameters.example.yaml` documents every key.
Side effects stay off unless the file enables them.
