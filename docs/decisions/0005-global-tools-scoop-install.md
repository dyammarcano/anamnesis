# ADR-0005: Global tools; missing tools are installed with scoop

- Date: 2026-10-01
- Status: accepted (operator decision). Supersedes the brief's "never install automatically" and
  the `tools:` section of ADR-0004.

## Decision

- git, ant, mvn, gradle, gh and similar tools are **global**, resolved from `PATH`. They are not
  configured per path in `parameters.yaml`.
- When a tool is missing, Anamnesis installs it with `scoop install <package>` (`internal/prepare`),
  governed by `environment.install_missing` in `parameters.yaml`.
- With `environment.install_jdks`, Anamnesis also installs a JDK able to compile the project's
  **declared** Java target: ≤ 8 → `temurin8-jdk`, ≤ 11 → `temurin11-jdk`, ≤ 17 → `temurin17-jdk`, …
  from the scoop `java` bucket. It uses that JDK for the local build by passing `JAVA_HOME` explicitly.
- Requirements scoop cannot satisfy (historical application servers) are reported as
  `NOT_INSTALLABLE`, never attempted.

## Ordering

`preflight` → `prepare` → re-check prerequisites → deep analysis. Preflight never installs, so
the report always holds the environment **as found** (the EJBCA first-run requirement) alongside the
environment **as prepared**. Every install is evidence: package, command, exit code, resulting state.

## Consequences

- scoop JDK packages set the **user-level `JAVA_HOME`** globally. Anamnesis observes it before and after
  and reports any change as a WARN. Anamnesis itself never depends on that variable.
- Installing changes the machine. That is the operator's explicit choice, and it can be turned off
  with `environment.install_missing: false`.
