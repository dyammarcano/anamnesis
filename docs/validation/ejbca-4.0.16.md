# Golden Legacy #1 — EJBCA Community Edition 4.0.16
<!-- rev:003 (RFC 3339) 2026-10-01T18:33:16Z -->

EJBCA is **test input**. Anamnesis is the system under test. EJBCA is never modified, fixed or
modernized, and production code contains no EJBCA-specific behaviour.

## Provenance

| Item | Value |
|---|---|
| Project | EJBCA Community Edition (PrimeKey, now Keyfactor), JEE PKI Certificate Authority |
| Version | 4.0.16, the final EJBCA Community 4 release, dated 2013-06-27 on SourceForge |
| Source | upstream SourceForge project `ejbca`: `https://sourceforge.net/projects/ejbca/files/ejbca4/ejbca_4_0_16/` |
| Download URL used | `https://downloads.sourceforge.net/project/ejbca/ejbca4/ejbca_4_0_16/ejbca_4_0_16.zip` |
| Commit SHA | not applicable: source archive, no VCS metadata inside |
| Archive size | 42,841,605 bytes |
| Archive SHA-1 | `4b33957e61d330fe1bcf1d17508fe3243d373e72`, **matches** the upstream `ejbca_4_0_16.zip.SHA1` |
| Archive SHA-256 | `b6ac40f3dd92c8914b05d378f8a74fc0c21834eae573eaacba7a87d7da43e1a8` |
| Acquired | 2026-10-01T16:53:55Z |
| Local fixture path | `<fixtures-root>\ejbca_4_0_16`, deliberately **outside** the Anamnesis repo so git discovery cannot attribute EJBCA to Anamnesis's own repository |
| Pristine tree | 2,423 files, 656 dirs, 60,725,660 bytes; tree digest `035e26fdd9add60389c8bb9f5ca80e4dd67f3e6b9049d6bdf75f7a20587e1c7d` (`tools/treedigest`); full manifest `<fixtures-root>\ejbca_4_0_16.pristine.manifest` |

**Untouched-ness check.** After every Anamnesis run against EJBCA, re-run
`go run ./tools/treedigest <fixtures-root>\ejbca_4_0_16 <out>`. The digest must equal the
pristine one. Any difference means Anamnesis (or a tool it launched) wrote into the analyzed repository,
and that is a Anamnesis defect.

## Ground truth from independent inspection

Found by direct inspection with grep, file listing and class-header reads, **before** Anamnesis has
any detection code. These facts are what the invariants in `fixtures/golden/ejbca-4.0.16/expected.yaml`
are derived from. Anamnesis's job is to find them on its own.

| # | Fact | Evidence |
|---|---|---|
| G-1 | No VCS metadata (`.git` absent) | top-level listing |
| G-2 | Ant is the build system: root `build.xml` (`default="build"`), 28 more `build.xml` under `modules/` and `src/samples/`, `.xmli` include files | `build.xml:1`, file listing |
| G-3 | No `pom.xml`, `build.gradle*` anywhere. MTA's Java provider will therefore fail with `unable to get build tool` (kantra-internals §5.2) | file listing |
| G-4 | NetBeans project metadata in one module (`nbproject/`), with `javac.source=1.5` and a default of `1.4` in the generated build | `modules/batchenrollment-gui/nbproject/project.properties:59`, `…/nbproject/build-impl.xml:74` |
| G-5 | Target compatibility **Java 1.6**: `<property name="java.target.version" value="1.6"/>`, used as `target="${java.target.version}"` on module `<javac>` tasks | `modules/build-properties.xml:97`; e.g. `modules/admin-gui/build.xml:55` |
| G-6 | 81 of 83 `<javac>` tasks declare no `source` attribute, so source level is mostly implicit (defaults to the compiling JDK) | grep over `*.xml`/`*.xmli` |
| G-7 | The version property is reached only through imports: root `build.xml:30` imports `${ejbca.home}/modules/build-properties.xml`. Resolving it needs import following **with property substitution** | `build.xml:23-30` |
| G-8 | A property-dependent import: `bin/${appserver.type}.xml` (`optional="true"`), with candidates `jboss.xml`, `glassfish.xml`, `weblogic.xml`, `websphere.xml`, `cli.xml` | `build.xml:25`, `bin/` listing |
| G-9 | App-server detection from `APPSRV_HOME`/`JBOSS_HOME` env and marker files; supported servers listed as "Glassfish 2.1.1, JBoss 5.1.0.GA, JBoss 6.0.0, WebLogic 10.3.3, WebSphere 7.0.0.13" | `propertyDefaults.xml:71-91` |
| G-10 | The default target `build` depends on `fail-unless-appserver-detected` → `ejbca.ear`, so a default build in this environment must fail for lack of an application server, not because of the source | `build.xml:47` |
| G-11 | Deployment-oriented targets: `deploy` (depends on `build`, interactive password input) and `install` | `build.xml:609`, `build.xml:63` |
| G-12 | `bin/jboss.xml` performs 14 `copy`, 15 `chmod`, 1 `java`, 4 `antcall`, i.e. writes into the app-server installation | `bin/jboss.xml` task counts |
| G-13 | Java EE surface present: 1,548 `.java`, 80 `.jsp`, 50 `.jspf`, 12 `.xhtml`, module names `*-war`, `ejbca-ejb`, `ejbca-entity`, `ejbca-ws`, `ejbca.ear` target | file counts, `modules/` listing |
| G-14 | 73 bundled JARs. First-class-file majors: 45×27, 46×9, 47×11, 48×12, 49×10, 50×4 (Java 1.1–6; **none above Java 6**) | class header reads |
| G-15 | Database coupling: 25 `.sql` files; JNDI datasource prefix varies per app server | file counts, `propertyDefaults.xml:125-128` |

Technology-level claims (EJB annotations, JPA, JAX-WS, JMS usage in `.java`) are **not** asserted
here. They are left to Anamnesis's inventory, and are added to the invariants only once seen in evidence.

## Expected semantic outcome in the current environment

The current machine has JDK 21, no Ant, no Maven, no application server
(`docs/development-environment.md`). The defensible assessment is therefore:

```
Source generation:           legacy Java, target 1.6 (G-5, G-14)
Build system:                Ant (+ NetBeans metadata in one module)
Historical build env:        JDK ≤ 6-era compiler + JBoss 5.1/6.0 | GlassFish 2.1.1 | WebLogic 10.3.3 | WebSphere 7
Current local environment:   incompatible/incomplete: Ant missing, app server missing, JDK 21
Local build:                 NOT ATTEMPTED (Ant missing) → would fail at fail-unless-appserver-detected
Source buildability:         NOT DISPROVEN
Environment reproducibility: FAILED / INCOMPLETE
Static assessment:           AVAILABLE
Migration assessment (MTA):  Java provider NOT APPLICABLE (no pom/gradle; `unable to get build tool` predicted)
```

## Stage activation

| Stage | Needs Anamnesis capability | Wave | Status |
|---|---|---|---|
| 1 Discovery / preflight | discovery, scan, buildsys, javaver, prereq | 1–2 | validated |
| 2 Java version evidence | javaver (imports + property substitution, class majors, IDE metadata) | 2 | validated |
| 3 Ant safety | ant parser, graph, classifier | 3 | validated |
| 4 Local build experiment | buildrun + classifier | 3 | validated: as found `TOOL_NOT_AVAILABLE`; as prepared `PARTIAL:doc-build-tool` |
| 5 MTA | migration/mta | 5 | validated: forced run on a staged copy failed exactly as predicted, failure preserved |
| 6 Reports | report | 6 | validated: report.json / .txt / .html generated in every run |
| 7 Performance baseline | timings in `run.json` | 2 onward | recorded below |

## Results (2026-10-01)

Runs live under the output root (`<output_dir>\ejbca_4_0_16-756af9a8\` before the
Anamnesis rename). The EJBCA tree digest equalled the pristine `035e26fd…` after **every** run.

**Automated** (`go test -race ./...`, `TestGoldenEJBCA`): 16 invariants PASS, 0 FAIL, 3 PENDING. The
three pending ones need a deep run with installs, so they were checked against the final run
`20261001T180142Z`:

| Invariant | Observed | Verdict |
|---|---|---|
| buildability.semantics | `buildability.source = NOT_DISPROVEN`, `local-reproducibility = PARTIAL:doc-build-tool`, `state = NOT_PROVEN`; statement says the application as a whole was not built | holds |
| buildability.missing-tools | run `20261001T172755Z` (environment as found): ant `MISSING` with a scoop suggestion, nothing installed by preflight; installs appear only as `env.install` evidence of the prepare step | holds |
| migration.survives-failure | `mta.run FAILED / JAVA_PROVIDER_NO_BUILD_TOOL` with MTA's own message `failed to start providers: unable to initialize providers: unable to init: unable to get build tool`; 1,919 evidence items, 0 failed analyzers, all three reports written | holds |

**What the generic engine found:**
- target 1.6 at `modules/build-properties.xml:97`, resolved through an import
- the unresolved import `bin/${appserver.type}.xml`
- JAR class majors 45–50
- JBoss, WebLogic, WebSphere, GlassFish and Tomcat coupling
- JAXB in 288 files and JAX-WS in 47, flagged as removed in JDK 11
- jdeps: 9 JDK-internal APIs, 8 of them removed (e.g. `com.sun.image.codec.jpeg.*` in `batik-codec.jar`, `sun.misc.Perf`)
- jdeprscan: 30 APIs deprecated for removal in JDK 21
- 1,745 Ant targets classified: default `build` and `deploy` DANGEROUS via `<exec ${appserver.home}/bin/createEJBStubs…>` at `modules/ejbca-ejb/build.xml:366`; `install` REVIEW_REQUIRED

### Defects in Anamnesis exposed by this fixture (all fixed, regression-tested where noted)

| Defect | Fix | Regression test |
|---|---|---|
| `--only` with an unknown analyzer name ran nothing and exited 0 | usage error listing known analyzers | `TestOnlyRejectsUnknownAnalyzer` |
| A SAFE auxiliary target succeeding would have been reported as "locally buildable" | `full_build` = built target is the project default; otherwise `PARTIAL` | `TestBuildabilitySemantics` (falsified) |
| Unresolved `${build.dir}`-style destinations treated as SAFE | always at least REVIEW_REQUIRED | `TestUnresolvedBuildishDestinationIsNotSafe` |
| Depends cycles silently dropped from closures | UNKNOWN with the cycle path | `TestDependsCycleIsUnknownNotSafe` (falsified) |
| MTA source route pointed MTA at the repository itself | MTA analyses a staged copy; incident paths mapped back | Codex review finding |
| Tools installed by scoop invisible to the running process | prepend `<prefix>\bin`; refresh PATH from the registry at startup | observed in run `20261001T175620Z` |
| jdeps invoked with `-summary` + `--jdk-internals` (rejected); reported as "73 archives could not be analysed" | correct arguments; a rejected command line is an analyzer failure, never a property of the project | observed in run `20261001T174728Z` |
| jdeprscan run without a classpath; resolution misses reported as incompatible class files | the project's jars as `--class-path`; resolution misses worded as such | observed in run `20261001T180142Z` |
| Safety conclusion inlined 1,745 targets (3.2 MB report.txt) | summary + evidence cap per conclusion | report size 307 KB |

## Performance baseline

Repository: 2,423 files, 60.7 MB, 1,548 `.java` files, 267,333 Java lines (newline count, includes
blanks and comments), 73 `.jar`, 0 `.war`, 0 `.ear`.

| Measure | Value | Run |
|---|---|---|
| preflight duration | 12.4 s | `20261001T172755Z` |
| deep-analysis duration (preflight + prepare + deep) | 362.9 s | `20261001T180142Z` |
| ↳ jdeprscan (one JVM per jar, sequential) | 350.8 s | dominant cost; candidate for bounded parallelism |
| ↳ MTA (forced, fails at provider init) | 52.6 s | |
| ↳ local build (staging + Ant) | 42.3 s | |
| ↳ jdeps | 7.9 s | |
| report generation | under 1 s (not separately timed) | |
| peak memory | not measured | |
