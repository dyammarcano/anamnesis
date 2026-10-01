# ADR-0001: MTA is an external component, invoked, not embedded

- Date: 2026-10-01
- Status: accepted (Wave 0)

## Context

The brief asks for one cohesive application and leaves open whether MTA code is embedded, adapted,
or invoked. The source analysis (`docs/kantra-internals.md`) shows:

- Java analysis is done by `java-external-provider`, a separate Go module, which drives Eclipse JDT LS,
  a Java program needing JDK 17+. Neither can be compiled into a Go binary.
- The analyzer-lsp engine is linkable (kantra links it), but brings 30 direct + 17 indirect module
  requirements (gRPC, protobuf, OpenTelemetry, Jaeger, logrus, cobra) and on its own runs only the
  builtin rules, about 29% of effort-bearing default Java rules (kantra-internals §8, measured).
- Any provider init failure aborts the whole MTA run (kantra-internals §5.4.6). Ant/Eclipse repos
  always fail Java provider init (§5.2).
- The distribution's rulesets and binaries carry no licence files locally (OQ-1).

## Decision

Anamnesis invokes the operator's installed MTA CLI as a subprocess under a strict invocation
contract (architecture §5), reads `output.yaml` with its own schema-compatible types, predicts MTA's
known failures in Preflight, and computes rule coverage itself. It embeds no analyzer-lsp or kantra
code and redistributes no MTA material.

## Consequences

- MTA is optional at runtime; its absence or failure becomes evidence, and the assessment continues.
- Anamnesis's binary stays small, with a short dependency list.
- The operator must have MTA installed to get MTA evidence. That's acceptable: it is installed
  separately anyway because of JDT LS.
- Builtin-only MTA evaluation is not available to Ant repositories through kantra 8.3.0. Mitigated by
  the binary and staged-copy routes (H-1) and by Anamnesis's native descriptor and technology analyzers.

## Reversal

Embedding analyzer-lsp later is additive (a new analyzer in `internal/migration`) and does not change
the evidence model. Revisit if H-1 fails **and** builtin-rule coverage for Ant repos proves essential.
