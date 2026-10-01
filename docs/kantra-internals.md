# MTA / Kantra 8.3.0 internals
<!-- rev:004 (RFC 3339) 2026-10-01T18:33:16Z -->

Source-level analysis of the MTA CLI (kantra) 8.3.0 and the analyzer-lsp commit it pins, done to
decide what Anamnesis reuses, wraps or reimplements. Every claim cites `file:line` in the
reference trees:

- `K:` = `vendor-ref/mta-cli/` (kantra, module `github.com/konveyor-ecosystem/kantra`)
- `A:` = `vendor-ref/analyzer-lsp/` at `861e51620602`
- `J:` = `A:external-providers/java-external-provider/pkg/java_external_provider/`

`vendor-ref/` is not part of this repository; it is excluded on purpose. To reproduce the citations,
use the MTA 8.3.0 CLI source archive (`mta-8.3.0-cli-src.zip`, kantra) and
`https://github.com/konveyor/analyzer-lsp` checked out at `861e51620602a9d4547cb87888338318d68b06e9`.

Statements marked **(measured)** come from `tools/rulesetscan` run against the
Windows distribution zip. Ten load-bearing claims were checked independently by Codex, read-only
(an independent read-only check by a second model, Codex): 6 confirmed, 3 partly, 1 refuted. The text below
carries the corrections (§4 Maven check, §5.2 mode ordering, §6 missing-provider logging). Statements marked **(unverified)** could not be confirmed from local
sources. Kantra's own `.claude/architecture.md` and `.claude/testing.md` are partly **stale**: they
cite `cmd/analyze-bin.go`, `cmd/analyze-hybrid.go` and a `RUN_LOCAL` env var, none of which exist
in this tree. The code below is the authority.

---

## 1. Shape of the system

```
mta-cli.exe (kantra, cobra)                         K:main.go → K:cmd/root.go
  └─ analyze                                        K:cmd/analyze/analyze.go:70
       ├─ Alizer language detection                 K:cmd/analyze/analyze.go:169
       ├─ choose mode: Local (containerless) | Network (hybrid)   :234-245
       ├─ provider.Environment (local | container)  K:pkg/provider/environment.go, env_local.go, env_container.go
       └─ runAnalysis — one pipeline for both modes K:cmd/analyze/run.go:69
            └─ analyzer-lsp core (in-process library)          A:core/analyzer.go
                 ├─ builtin provider (in-process)              A:provider/internal/builtin/
                 └─ java provider = separate process, gRPC     A:provider/grpc/provider.go:421
                      └─ java-external-provider.exe            J:provider.go
                           ├─ build tool: maven|gradle|binary  J:bldtool/tool.go:145
                           └─ Eclipse JDT LS (java.exe, JDK17+) J:provider.go:446-493
```

Kantra is a thin orchestrator. Analysis is done by the analyzer-lsp **engine**, linked in as a Go
library (`konveyorAnalyzer "github.com/konveyor/analyzer-lsp/core"`, K:cmd/analyze/run.go:17).
Java understanding comes from a **separate binary** (`java-external-provider`) that drives
**Eclipse JDT LS**, a Java program.

## 2. Entry point and CLI construction

| Step | Location |
|---|---|
| `main` calls `cmd.Execute()` | K:main.go |
| `analyze` command, flags | K:cmd/analyze/analyze.go:70-325 |
| `PreRunE`: requires `--input`/`--output`; resolves kantra dir; loads a profile; validates | :79-124 |
| `RunE`: list modes, override settings, Alizer, provider selection, mode, `runAnalysis` | :125-286 |

Defaults that matter: `--run-local=true` (containerless, :318); `--mode=full` (:304);
`--enable-default-rulesets=true` (:310); `--context-lines=100` (:314); `--analyze-known-libraries=false`
adds the dependency label selector `!konveyor.io/dep-source=open-source` (K:cmd/analyze/run.go:212-215).

## 3. Mode selection: containerless vs hybrid

1. Alizer runs on the input (K:cmd/analyze/analyze.go:169).
2. `setProviders` maps Alizer languages with `CanBeComponent` to provider names, or takes `--provider`
   verbatim (K:cmd/analyze/providers.go:146-171).
3. **If any detected provider is not `java`, mode switches to hybrid** (`runLocal = false`) — containers
   required (K:cmd/analyze/analyze.go:236-239).
4. Containerless configures **both `java` and `builtin`** regardless of what Alizer found
   (`AllLocalProviders`, K:pkg/provider/defaults.go:77, used at K:pkg/provider/env_local.go:85), except
   in `ExternalOnly` mode, where only `builtin` plus user-managed external providers are configured
   (:86-88).
5. In hybrid mode only, when Alizer found nothing, `detectJavaProviderFallback` looks for
   `pom.xml` then `build.gradle` at the input root — and nothing else (K:cmd/analyze/providers.go:18-46).
   `build.xml`, `.classpath`, `build.gradle.kts` are not considered.
6. `--provider builtin` is **rejected**: `validateProviders` allows only java/python/go/nodejs/csharp
   (K:cmd/analyze/validate.go:260-279). There is no builtin-only containerless run in 8.3.0.

Hybrid mode runs the same `java-external-provider` code inside a container, so it does **not**
change the build-tool behaviour described in §5. It additionally needs podman/docker
(`CONTAINER_TOOL`, default `/usr/bin/podman`, K:cmd/internal/settings/settings.go:44).

## 4. Containerless prerequisites (`localEnvironment.Start`)

K:pkg/provider/env_local.go:62-104, in this order — each is fatal:

| Check | Line | Error text |
|---|---|---|
| input path ≠ current working directory (JDT LS writes `.metadata` in cwd) | :69-71 | `input path %s cannot be the current directory` |
| **`mvn` on PATH** for every run that uses the Java provider, including Ant and Gradle projects; skipped only in `ExternalOnly` mode (all providers user-managed via `--override-provider-settings`, :73-77) | :110-112 | `… cannot find requirement maven; ensure maven is installed` |
| `java -version` runs; if output contains `openjdk`, major ≥ 17 (Oracle-branded JDKs skip the check) | :115-134 | `cannot find requirement openjdk17+…` |
| `JAVA_HOME` set | :135-137 | `JAVA_HOME is not set; ensure JAVA_HOME is set` |
| kantra dir, `rulesets/`, `jdtls/java-analyzer-bundle/…core-1.0.0-SNAPSHOT.jar`, `jdtls/bin/jdtls`, `fernflower.jar` | :144-173 | `kantra installation directory not found` / `missing kantra dependency` (K:pkg/util/kantra_dir.go:15-41) |

**KANTRA_DIR resolution** (K:pkg/util/util.go:220-273): (1) `KANTRA_DIR` env, used even if the path
does not exist; (2) the directory of the running executable, if it contains `rulesets`, `jdtls` and
`static-report`; (3) `%USERPROFILE%\.kantra` (`os.UserHomeDir`). The MTA Windows distribution zip has
exactly the layout (2) expects, so an extracted distribution is self-locating.

## 5. Java provider initialisation and build-tool discovery

### 5.1 Call chain

```
K:cmd/analyze/run.go:285          anlzr.ProviderStart()
A:core/analyzer.go:129            ProviderStart: every non-builtin provider inits in its own goroutine (:159-185)
A:provider/grpc/provider.go:421   start(): exec java-external-provider --port <free> --name java (:447-463)
A:provider/grpc/provider.go:283   ProviderInit → Init → gRPC Init on the provider process (:362)
J:provider.go:293                 javaProvider.Init
J:provider.go:392                 bldtool.GetBuildTool(...)
J:bldtool/tool.go:145-163         gradle? → binary archive? → maven? → nil
```

### 5.2 `GetBuildTool` — what counts as a buildable Java project

J:bldtool/tool.go:145-163 tries, in order:

| Order | Detector | Condition | Location |
|---|---|---|---|
| 1 | Gradle | `<location>/build.gradle` exists — **Groovy DSL only; `build.gradle.kts` is not detected** | J:bldtool/gradle.go:45-51 |
| 2 | Maven binary | location ends in `.jar`, `.war`, `.ear` | J:bldtool/tool.go:146-150, J:bldtool/maven_binary.go:45 |
| 3 | Maven | `<location>/pom.xml` (or `<location>/<DependencyPath>`) exists | J:bldtool/maven.go:41-55 |
| — | none matched | returns `nil` | J:bldtool/tool.go:162 |

Only the **root** of the input is examined. A repository whose `pom.xml` lives in a subdirectory, or
whose build is Ant, Eclipse-only, or hand-assembled, gets `nil`.

`nil` is fatal immediately: `return nil, …, errors.New("unable to get build tool")` (J:provider.go:401-403).
The mode is validated first (:300-304), but the build-tool lookup does not depend on it. It runs, and
fails, in `source-only` mode too. Only the later resolve step (:405) is mode-dependent.

Gradle dependency work further requires the wrapper: `a gradle wrapper must be present in the project`
(J:bldtool/gradle.go:167-173, :319-329; `gradlew.bat` on Windows).

### 5.3 The four errors

| Error text | Emitted by | Meaning |
|---|---|---|
| `unable to get build tool` | J:provider.go:402 (`javaProvider.Init`) | no Gradle/Maven/archive at the input root |
| `unable to init: <remote error>` | A:provider/grpc/provider.go:368 (`grpcProvider.Init`) | gRPC client wrapping the provider process's `Successful=false` reply |
| `unable to init provider` (log only) | A:core/analyzer.go:171 | per-provider failure, logged, collected |
| `unable to initialize providers: <joined>` | A:core/analyzer.go:245 | `errors.Join` of every provider init error |
| `failed to start providers: <wrapped>` | K:cmd/analyze/run.go:287-288 | kantra aborts the run |

For an Ant/Eclipse repository with `mvn` present, the composite message is therefore:

```
failed to start providers: unable to initialize providers: unable to init: unable to get build tool
```

Without `mvn` on PATH the run never gets that far; it fails in §4 with `cannot find requirement maven`.

### 5.4 Answers to the Wave 0 questions

1. **Which component emits each error** — table §5.3.
2. **Assumptions behind it** — the input root holds `build.gradle`, `pom.xml`, or is a `.jar/.war/.ear`;
   `mvn` is on PATH; a JDK ≥ 17 is on `JAVA_HOME`; the kantra dir is complete.
3. **Build tools expected** — Maven, Gradle (Groovy DSL, with wrapper), or a binary archive.
4. **Is Maven implicitly required?** — **Yes**, twice: kantra checks `mvn` before any provider starts
   whenever the Java provider is in play (K:pkg/provider/env_local.go:73-77, :110), and the Maven/binary build tools shell out to
   `mvn` (`dependency:tree`, J:bldtool/maven.go:134-157; `dependency:copy`, J:bldtool/maven_downloader.go:47-58).
5. **Legacy Ant/Eclipse applications** — always fail at J:provider.go:402. There is dormant code that
   would write a synthetic `.project`/`.classpath` *into the analysed directory* so JDT LS can import a
   build-less tree (J:service_client.go:459-467, :489-521), but it is **unreachable** at this commit
   because `Init` returns first. If it were ever reached it would modify the analysed repository.
6. **Is the failure fatal by design?** — **Yes, at engine level.** `ProviderStart` collects every init
   error and returns, discarding the builtin provider that initialised fine (A:core/analyzer.go:218-246).
   No partial run is produced and no `output.yaml` is written.
7. **What analysis is still possible without the Java provider** — see §8.

## 6. Engine, rules, targets

| Topic | Behaviour | Location |
|---|---|---|
| Rule loading | `env.Rules` = user `--rules` + `<kantraDir>/rulesets` when defaults enabled | K:pkg/provider/env_local.go:224-232 |
| Target/source | `--target`/`--source` validated against `konveyor.io/target=` / `konveyor.io/source=` labels found by walking rulesets | K:cmd/analyze/validate.go:55-95, K:pkg/labels/lister.go:117-155 |
| Selector miss | rule recorded in `RuleSet.Skipped` | A:engine/engine.go:411-415 |
| **Missing provider** | condition dropped at parse time and logged (V(2)/V(5), so only in `analysis.log` at high verbosity); `or` keeps the surviving conditions; `and` drops the rule when any condition was removed. `Skipped` is filled only by selector mismatch (A:engine/engine.go:411-415); no other path that records parser-dropped rules in `output.yaml` was found | A:parser/rule_parser.go:32-39, :450-453, :486-492, :524-530 |
| Rule evaluation error | `RuleSet.Errors[ruleID] = err` — per-rule partial failure, run continues | A:engine/engine.go:306-312 |
| Effort 0 or absent | result goes to `Insights`, not `Violations` | A:engine/engine.go:321-326 |
| Tag + message rule | split; message part becomes a separate violation if effort ≠ 0 | A:engine/engine.go:429-439 |
| Dependency resolution | `GetDependencies` failure is logged, not fatal | K:cmd/analyze/run.go:303-307 |
| Static report failure | **fatal** for the run, after `output.yaml` is already written | K:cmd/analyze/run.go:360-364 |

The unrecorded drop of rules whose provider is absent matters most for Anamnesis: an `output.yaml`
from a reduced provider set *looks* complete. Coverage has to be computed independently from the
rulesets, not inferred from the output.

## 7. Output schema and effort

`output.yaml` is a YAML list of `RuleSet` (A:output/v1/konveyor/violations.go:16-46):

```
RuleSet    { name, description, tags[], violations{ruleID→Violation}, insights{ruleID→Violation},
             errors{ruleID→string}, unmatched[], skipped[] }
Violation  { description, category(potential|optional|mandatory), labels[], incidents[], links[],
             extras, effort *int }                                    :79-101
Incident   { uri, message, codeSnip, lineNumber *int, variables{} }  :122-134
Dep        { name, version, classifier, type, indirect, resolvedIdentifier, extras, labels[], prefix } :187-198
```

`dependencies.yaml` is a list of `DepsFlatItem{fileURI, provider, dependencies[]}` (:270-274).
`--json-output` re-marshals both to `output.json`/`dependencies.json` (K:cmd/analyze/output.go:16-75).

**Effort.** One integer per **rule** (`Violation.Effort`), documented as "expected story points for this
incident" (violations.go:99). It is copied from the rule's `effort:` (A:engine/engine.go:753,
A:parser/rule_parser.go:629-631). Output stores **no total**. The static report is a prebuilt web app
fed by `window["apps"] = <json>` (K:cmd/analyze/static_report.go:107-129); its aggregation formula is in
that app, not in the local Go source **(unverified)**. Anamnesis therefore stores rule effort and
incident count separately and labels any product `effort × incidents` as a derived figure with its
formula shown.

## 8. What survives without the Java provider (measured)

Line-heuristic classification of the 2,814 rules in the 333 Java rule files of the 8.3.0 distribution
**(measured)**:

| Rule class | Rules | Of which effort > 0 |
|---|---|---|
| `java.*` conditions only | 1,395 | 1,155 |
| `builtin.*` conditions only | 1,367 | 471 |
| mixed, `or` (builtin branch survives) | 44 | 25 |
| mixed, `and`/nested (dropped) | 5 | 3 |

Capabilities used: `java.referenced` 1,087 · `builtin.file` 387 · `builtin.hasTags` 381 ·
`builtin.xml` 371 · `java.dependency` 369 · `builtin.filecontent` 343 · `builtin.xmlPublicID` 7.

So a builtin-only run would evaluate roughly half the rules but only about **29% of the
effort-bearing ones** (rule count, not incident weight). Builtin capabilities are file globs, regex
over file content, XPath over XML, XML public IDs, JSON paths and tag checks
(A:provider/internal/builtin/provider.go:19-24, service_client.go:129-374). That covers deployment
descriptors (`web.xml`, `ejb-jar.xml`, `weblogic*.xml`, `ibm-*.xml`, `jboss*.xml`) and config files.
It cannot see Java API usage. `builtin.hasTags` rules depend on tags that Java rules may have
produced, so some builtin rules lose recall too.

**Routes to MTA evidence for an Ant/Eclipse repository**, none verified yet:

| Route | Mechanism | Status |
|---|---|---|
| Binary input | pass an existing `.war/.ear/.jar` built from the repo; `mavenBinaryBuildTool` decompiles it with fernflower | plausible; writes a `java-project/` dir **next to the input file** (J:dependency/binary_resolver.go:50), so the archive must be copied out of the repo first |
| Staged copy + synthetic `pom.xml` | Anamnesis copies sources into its own run dir and generates a minimal POM (source dirs, local JARs as `system` deps) so `GetBuildTool` finds Maven | hypothesis — JDT LS behaviour on Java 6-era source untested |
| Builtin-only | not offered by kantra 8.3.0 (§3.6); would need analyzer-lsp embedded or a different binary | rejected for now (dependency weight, §10) |
| Hybrid/container | same Java provider code → same failure | does not help |

## 9. Side effects, subprocesses, Windows

| Concern | Behaviour | Location |
|---|---|---|
| Output dir | `--overwrite` does `os.RemoveAll(output)` | K:cmd/analyze/validate.go:213-218 |
| JDT LS working files | `-configuration ./` and Eclipse dirs `org.eclipse.*` created in the **current working directory**, removed at Stop | J:provider.go:486, K:pkg/provider/env_local.go:30-35, :177-187 |
| Repo-carried config | a profile under `<input>/.konveyor/profiles` is auto-loaded and can change input, mode, rules, label selector, default-ruleset use | K:cmd/analyze/validate.go:222-258, K:cmd/analyze/profile.go:20-35 |
| Repo code execution | Gradle projects: provider runs the repository's `gradlew`/`gradlew.bat` (J:bldtool/gradle.go:167-182, :319-350). Maven: runs `mvn dependency:tree` in the project dir (J:bldtool/maven.go:132-157), which resolves plugins and downloads artefacts | — |
| Network | `mvn`/Gradle downloads; no direct Maven Central search URL in this commit (grep for `search.maven.org` / `central.sonatype` returned nothing); open-source labelling uses the local `maven.default.index` / `maven-index.txt` | J:provider.go:364-369 |
| Provider process | started with `exec.CommandContext` on a free TCP port; command line printed to stdout | A:provider/grpc/provider.go:447-471 |
| Timeouts | provider init 8 min default (A:core/analyzer.go:193-215); `mvn dependency:tree` 5 min (J:bldtool/maven.go:153) | — |
| JDT LS JVM | `-Xms1g -XX:MaxRAMPercentage=70.0`, optional `-Xmx` from `JVM_MAX_MEM` | J:provider.go:470-492 |
| JDK check defect | `getJavaExecutable` compares the major version **as a string** (`"8" < "17"` is false), so JDK 8 on `JAVA_HOME` passes and fails later | J:provider.go:892-898 |
| Windows | stderr filter for an fsnotify warning (K:pkg/util/stderr_filter_windows.go:19-77); hidden-file check via `GetFileAttributes` with `\\?\` long-path prefix (K:cmd/internal/hiddenfile/hidden_windows.go:14-41); JDT LS `config_win` (J:provider.go:921-934); `java.exe` under `JAVA_HOME` (J:provider.go:871-881) | — |
| Process tree | kantra and the provider use `exec.CommandContext`; on Windows that terminates only the direct child, not `java.exe` grandchildren **(Go runtime behaviour, not observed here)** | — |

## 10. Environment variables

| Variable | Effect | Location |
|---|---|---|
| `KANTRA_DIR` | kantra installation directory | K:pkg/util/util.go:195, :230-236 |
| `JAVA_HOME` | required; JDT LS JVM | K:pkg/provider/env_local.go:135, J:provider.go:873 |
| `JAVA8_HOME` | passed to JDT LS as Gradle import JDK | J:service_client.go:430-454 |
| `JVM_MAX_MEM` | JDT LS `-Xmx` | K:cmd/internal/settings/settings.go:46 |
| `CONTAINER_TOOL` / `PODMAN_BIN` | hybrid container runtime | settings.go:44, :91-94 |
| `RUNNER_IMG`, `JAVA_PROVIDER_IMG`, `GO_/PYTHON_/NODEJS_/CSHARP_PROVIDER_IMG` | hybrid images | settings.go:45-51 |
| `CMD_NAME` | root command name | settings.go:43 |
| `http_proxy` / `https_proxy` / `no_proxy` (either case) | flag defaults; exported to children | K:cmd/analyze/analyze.go:311-313, run.go:53-67 |
| `KANTRA_SKIP_MAVEN_CACHE` | hybrid Maven cache volume | K:pkg/provider/env_container.go:371 |
| `XDG_CONFIG_HOME` | Linux only, kantra dir fallback | K:pkg/util/util.go:262-264 |

## 11. Dependency weight of embedding analyzer-lsp

`A:go.mod` declares 30 direct and 17 indirect requirements, including gRPC, protobuf, OpenTelemetry
with the Jaeger exporter, logrus, cobra and gval. The Java provider is a separate module with its own
`go.mod` and needs JDT LS (Java) at runtime regardless. Embedding the engine would buy builtin-only
evaluation in-process at the cost of that whole graph. See ADR-0001.
