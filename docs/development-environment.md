# Development environment
<!-- rev:004 (RFC 3339) 2026-10-01T18:33:16Z -->

Where the sources and tools used to build Anamnesis were found on the development
machine. **Anamnesis itself must never depend on any absolute path listed here** — these are
the inputs used for research and validation only.

## Machine

| Item | Value | How observed |
|---|---|---|
| OS | Windows 11 Pro 10.0.26300 | session environment |
| Shell | PowerShell 7 (primary), Git Bash available | session environment |
| Go | go1.27.0 windows/amd64, `C:\Program Files\Go\bin\go.exe` | `go version` |
| GOPATH / GOMODCACHE | `%USERPROFILE%\go` / `%USERPROFILE%\go\pkg\mod` | `go env` |
| JDK | Temurin 21.0.12.1 at `%USERPROFILE%\scoop\apps\temurin21-jdk\current` | `java -version`, `JAVA_HOME` |
| `jdeps`, `jdeprscan` | present (same JDK) | `Get-Command` |
| `git` | scoop, `%USERPROFILE%\scoop\apps\git\current\cmd\git.exe` | `Get-Command` |
| `gh` | `C:\Program Files\GitHub CLI\gh.exe` | `Get-Command` (auth state not checked) |
| `ant`, `mvn`, `gradle` | **not on PATH at session start**; ant and maven later installed by Anamnesis's prepare step (below); gradle still absent | `Get-Command` returned nothing |
| `kantra`, `mta-cli` | **not on PATH** | `Get-Command` returned nothing |
| `KANTRA_DIR` | not set | `$env:KANTRA_DIR` |
| `scoop` | `%USERPROFILE%\scoop\shims\scoop.ps1` | `Get-Command` |
| `codex` | `%USERPROFILE%\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe` | `Get-Command` |

## Workspace

| Item | Path |
|---|---|
| Anamnesis workspace (git repo, branch `master`, no commits yet) | `<repo>` |
| Reference sources (gitignored, read-only) | `<repo>\vendor-ref\` |

## MTA / Kantra sources

| Copy | Path | Version evidence |
|---|---|---|
| MTA CLI (kantra) source, from `mta-8.3.0-cli-src.zip` | `vendor-ref\mta-cli\` | archive name `mta-8.3.0-cli-src.zip`; module `github.com/konveyor-ecosystem/kantra`; the tree carries no version string of its own (set by ldflags at build time, see `konflux.Dockerfile`) |
| analyzer-lsp at the commit pinned by that kantra | `vendor-ref\analyzer-lsp\` | `go.mod`: `github.com/konveyor/analyzer-lsp v0.9.0-beta.1.0.20260326145242-861e51620602`; checked out `861e51620602a9d4547cb87888338318d68b06e9` (2026-03-26, "Generate Maven settings.xml with proxy config (#1129)") (shallow clone) |
| MTA 8.3.0 Windows distribution (not extracted) | `%USERPROFILE%\Downloads\mta-8.3.0-cli-windows-amd64.zip` (752 MB) | contains `windows-mta-cli.exe`, `java-external-provider.exe`, `jdtls/`, `rulesets/`, `static-report/`, `fernflower.jar`, `maven-index.txt`, `maven.default.index`, `task.gradle`, `task-v9.gradle` |
| MTA 8.3.0 Linux distribution (not inspected) | `%USERPROFILE%\Downloads\mta-8.3.0-cli-linux-amd64.zip` | archive name only |

Only one copy of each was found (search of `B:\`, `%USERPROFILE%`, `D:\`, `E:\` to depth 3).
The source zip does **not** include the `hack/build/rulesets` and `hack/build/windup-rulesets` git
submodules (`.gitmodules`); the default rulesets are only available from the distribution zip.

## Candidate validation repositories

**None found.** No directory matching `*eneficio*` exists within depth 3 of `B:\`,
`%USERPROFILE%`, `D:\` or `E:\`. Validation against the real legacy repositories is blocked on the
operator supplying their paths; until then, fixtures under `fixtures/` are the only inputs.

## Consequences for development

- Local builds of Ant/Maven/Gradle projects cannot be attempted on this machine until those tools are
  installed by the operator. This is itself the "missing prerequisite" path Anamnesis must report
  correctly, so it is useful rather than blocking.
- MTA cannot be run until the Windows distribution is extracted somewhere outside the repo and
  `mvn` is on `PATH` (kantra's containerless mode refuses to start without it — see
  `docs/kantra-internals.md` §4).

## Changes made to this machine during validation (2026-10-01)

Anamnesis's `prepare` step (ADR-0005) ran `scoop install` for the tools EJBCA and MTA need:

| Package | Version | Why | User-environment effect |
|---|---|---|---|
| `ant` | 1.10.18 | EJBCA declares an Ant build | `ant\current\bin` added to the user PATH (kept) |
| `maven` | 3.10.0 | MTA (kantra) requires `mvn` even for non-Maven projects | `maven\current\bin` added to the user PATH (kept) |
| `temurin8-jdk` | 8.0.504-1 | lowest declared target 1.5/1.6 needs a JDK ≤ 11 to compile | scoop set user `JAVA_HOME` to JDK 8 and prepended its `bin` to PATH. Anamnesis **restored both**, and the user `JAVA_HOME` is still `temurin21-jdk` (checked before and after each run) |

The MTA 8.3.0 Windows distribution was extracted to `<mta-install-dir>` (outside the repo):
`windows-mta-cli.exe version` reports `8.3.0`, SHA `ee82eac978278b7ba6136ba96a8f21316baaf744`.
`parameters.yaml` points `mta.install_dir` there.
