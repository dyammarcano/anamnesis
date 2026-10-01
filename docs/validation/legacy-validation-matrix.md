# Legacy validation matrix
<!-- rev:005 (RFC 3339) 2026-10-01T18:33:16Z -->

Independent real-world legacy codebases Anamnesis is validated against. The goal is coverage of
**architectures**, so Anamnesis does not overfit to one application. Each row has a validation record
in `docs/validation/` and semantic invariants in `fixtures/golden/<id>/expected.yaml`.

| # | Fixture | Generation | Build | Enterprise | Purpose | Status |
|---|---|---|---|---|---|---|
| 1 | EJBCA CE 4.0.16 | Legacy Java (target 1.6) | Ant (+ NetBeans metadata) | Java EE / EJB / EAR, multi-app-server | Golden Legacy #1 — Ant + app-server-coupled build in an environment that cannot reproduce it | validated 2026-10-01: 16/16 automated invariants PASS; 3 deep-run invariants checked against the 2026-10-01 runs (see validation record) |
| 2 | *to choose* | Java 5–7 | Maven 2/3 | Java EE | Golden Legacy #2 — old Maven enterprise application (MTA Java provider applicable) | open |
| 3 | *to choose* | Java 6–7 | Ant + Eclipse `.classpath` | — | Golden Legacy #3 — Eclipse/Ant application, hand-managed `lib/` | open |
| 4 | *to choose* | Java 7–8 | Gradle (old wrapper) | — | Golden Legacy #4 — old Gradle application, Groovy DSL + wrapper | open |
| 5 | derived from #1–#4 | — | any | — | Golden Legacy #5 — intentionally broken environment (wrong `JAVA_HOME`, missing env vars, unreachable repository) | open |

## Coverage columns tracked per fixture

| Capability | #1 | #2 | #3 | #4 | #5 |
|---|---|---|---|---|---|
| Build-system detection | Ant | | | | |
| Java version from build config | Ant property via import | | | | |
| Java version from binaries | JAR majors 45–50 | | | | |
| Ant safety graph | deploy/install + dynamic import | | | | |
| Local build classification | as found: TOOL_NOT_AVAILABLE (ant); as prepared: PARTIAL (only an auxiliary SAFE target) | | | | |
| CI evidence | none (archive) | | | | |
| MTA path | predicted and observed `unable to get build tool`; failure preserved, report generated | | | | |
| Git identity | none (archive) | | | | |

## Selection rules for new fixtures

- Upstream source of the exact version, with a recorded checksum or commit.
- Stored outside the Anamnesis repo; recorded by tree digest; never modified.
- Must differ from existing rows in at least one architecture column.

## Supplementary MTA check

EJBCA (Ant) cannot exercise MTA's success path. Kantra's own Maven sample
(`vendor-ref/mta-cli/cmd/rules/test/examples/java/test-data/java`, copied to
`<fixtures-root>\kantra-java-sample`) is used for that. On 2026-10-01 MTA analysed a staged copy and
produced 26 violated rules and 48 incidents, normalised to 92 MTA effort points with the formula shown;
the incident paths mapped back to repository paths, and the sample tree digest was unchanged. It is a
tool sample, not a golden legacy fixture.

## Supplementary CI and consolidated-report check

`spring-projects/spring-petclinic`, a public repo with GitHub Actions, was shallow-cloned to
`<fixtures-root>\spring-petclinic`, and a three-project run (EJBCA, kantra sample, petclinic) was made
with `network.allow: true` on 2026-10-01 (run `20261001T181855Z`).
- Anamnesis matched HEAD `500158f7…` to a successful run of the BUILD-classified workflow
  "Java CI with Gradle" on the same SHA and concluded `KNOWN_BUILDABLE_IN_CI_LOCAL_NOT_REPRODUCED`. The
  DEPLOY workflow was classified separately and not counted.
- `consolidated-report.{json,html}` was written for all three projects.
- All three tree digests were unchanged and petclinic's `git status` stayed clean.
- The lint-only case (a lint workflow succeeding on the same SHA is not build proof) is covered by
  `TestLintOnlySuccessIsNotSameSHABuildProof`, which fails when the BUILD filter is removed.
