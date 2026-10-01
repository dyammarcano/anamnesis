# Third-party provenance
<!-- rev:004 (RFC 3339) 2026-10-01T18:33:16Z -->

The register of everything Anamnesis takes from MTA/Kantra, analyzer-lsp or any other third
party. **A row is added in the same commit that brings the material in.** No row means the material
is not in Anamnesis.

## Licences found in the reference sources

| Source | Licence | Evidence |
|---|---|---|
| kantra (MTA CLI) 8.3.0 source | Apache License 2.0 | `vendor-ref/mta-cli/LICENSE` |
| analyzer-lsp @ `861e51620602` (engine, providers, output schema) | Apache License 2.0 | `vendor-ref/analyzer-lsp/LICENSE`; no `NOTICE` file present |
| analyzer-lsp test fixtures (gradle examples) | MIT | `…/dependency/testdata/gradle-example*/LICENSE` |
| MTA 8.3.0 Windows distribution (binaries, JDT LS, rulesets, static report, Maven indexes) | **not determinable locally**: the zip contains no LICENSE/NOTICE/COPYING file | listing of `mta-8.3.0-cli-windows-amd64.zip` |
| Default rulesets (`konveyor/rulesets`, `windup/windup-rulesets` submodules) | **not determinable locally**: the submodules are absent from the source zip | `vendor-ref/mta-cli/.gitmodules` |

What Apache-2.0 requires when code is copied or adapted: ship a copy of the licence, keep the original
copyright and attribution notices in copied files, mark modified files as changed, and carry any
`NOTICE` content (none exists here).

## Policy

1. **Prefer reimplementation from understanding over copying.** The reference trees are for reading.
2. **Never redistribute** the MTA distribution's binaries, JDT LS, rulesets, static report or Maven
   indexes. Anamnesis uses an MTA installation the operator already has, at runtime.
3. Anything copied or adapted gets a row here, an attribution header in the file, and the Apache-2.0
   text under `third_party/licenses/`.
4. Short factual strings, such as kantra error messages used as patterns to classify failures, are
   recorded here even though they are not copyrightable expression, so the coupling stays visible.

## Register

| # | Material | Original project | Original path | Licence | Treatment | Anamnesis destination | Modifications | Status |
|---|---|---|---|---|---|---|---|---|
| P-1 | `output.yaml` / `dependencies.yaml` schema (RuleSet, Violation, Incident, Category, Dep, DepsFlatItem field names and YAML keys) | konveyor/analyzer-lsp | `output/v1/konveyor/violations.go` | Apache-2.0 | conceptually reimplemented: own Go types, same YAML keys, so Anamnesis can read MTA output | `internal/migration/mta/output.go` | own types; no sorting/marshal methods; only the fields Anamnesis reads | implemented |
| P-2 | Failure message patterns (`unable to get build tool`, `unable to init`, `unable to initialize providers`, `failed to start providers`, `cannot find requirement maven`, `JAVA_HOME is not set`, `missing kantra dependency`, `kantra installation directory not found`, `timed out starting providers`) | kantra, analyzer-lsp | see `docs/kantra-internals.md` §4, §5.3 | Apache-2.0 | referenced as match patterns | `internal/migration/mta/failure.go` | none (exact substrings) | implemented |
| P-3 | KANTRA_DIR resolution order (env → executable dir with `rulesets`+`jdtls`+`static-report` → `%USERPROFILE%\.kantra`) | kantra | `pkg/util/util.go:220-273` | Apache-2.0 | conceptually reimplemented to locate an existing installation | `internal/migration/mta/locate.go` | reads only `mta.install_dir` / `mta.executable` from parameters.yaml (ADR-0004); no env, no PATH search | implemented |
| P-4 | Ruleset provider/effort classification | — (Anamnesis original) | `tools/rulesetscan` | — | original | `internal/migration/mta/coverage.go` | — | implemented |

Nothing from the MTA distribution or the rulesets is copied. Rulesets are read at runtime from the
operator's MTA installation to compute coverage (P-4). Their licence must be confirmed before any
ruleset content is ever embedded (open question OQ-1 in `docs/architecture.md`).

## Go module dependencies

Each third-party Go module Anamnesis depends on gets a row here with its licence and reason.
| Module | Version | Licence | Why it is needed | Added |
|---|---|---|---|---|
| `go.yaml.in/yaml/v3` | v3.0.5 | MIT and Apache-2.0 (dual, per file) | parameters.yaml, GitHub workflow parsing, MTA `output.yaml` | 2026-10-01 |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | MIT | interactive TUI (event loop, async progress) | 2026-10-01 |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | MIT | TUI layout and styling | 2026-10-01 |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | `internal/winpath`: reads the machine and user PATH from the Windows registry so globally installed tools are found even from a stale shell; Job Object process-tree termination is still a known gap | 2026-10-01 |

All other modules in `go.sum` are transitive dependencies of Bubble Tea and Lip Gloss (`go list -m all`: 27 modules).
