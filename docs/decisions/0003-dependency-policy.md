# ADR-0003: Go dependency policy and initial set

- Date: 2026-10-01
- Status: proposed. The TUI choice is confirmed in Wave 2 after a Windows console check.

## Decision

Stdlib by default: `encoding/xml` (streaming Ant/POM/Eclipse parsing), `archive/zip` (central-directory
reads of JARs without extraction), `html/template` (self-contained HTML), `log/slog`, `flag`
(three subcommands do not justify cobra), `os/exec`.

Admitted third-party modules, each needing a row in `docs/third-party-provenance.md` when added:

| Module | For | Admitted because |
|---|---|---|
| `go.yaml.in/yaml/v3` | GitHub workflows, MTA `output.yaml` | no stdlib YAML; both inputs are core evidence |
| `golang.org/x/sys/windows` | Job Objects for process-tree kill | `exec.CommandContext` kills only the direct child on Windows; MTA spawns `java.exe` grandchildren |
| Charm Bubble Tea + Lip Gloss | TUI | an event-loop TUI with resize, focus and async progress is a framework-sized job; reimplementing it would cost more than the dependency |

Builds are reproducible from `go.mod`/`go.sum` with `go build -o anamnesis.exe`. The produced
executable needs no Go toolchain to run.

## Rejected

- analyzer-lsp as a library: see ADR-0001.
- cobra/viper: not needed for the command surface.
- A regex-only YAML reader: workflow files are too varied to parse without a real parser.
