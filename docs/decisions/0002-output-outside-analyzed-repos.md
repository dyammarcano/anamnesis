# ADR-0002: All Anamnesis output lives outside the analyzed repositories

- Date: 2026-10-01
- Status: accepted (Wave 0); amended the same day by ADR-0004. The output root now comes only from
  `output_dir` in `parameters.yaml`. The `%LOCALAPPDATA%` default and the `--out` flag below are
  withdrawn, and the layout drops the inner `assessments/` level.

## Context

Writing inside an analyzed repository makes `git status` dirty, lets later scans and MTA read
Anamnesis's own output, and changes the thing being assessed. MTA has its own hazards: `--overwrite`
deletes the output directory (`os.RemoveAll`), JDT LS writes `org.eclipse.*` into the current working
directory, and binary analysis writes `java-project/` next to the input archive (kantra-internals §9).

## Decision

- Output root defaults to `%LOCALAPPDATA%\Anamnesis` (`--out` to override). Layout per
  architecture §7: `assessments/<repo-name>-<8-hex canonical-path hash>/<UTC run-id>/`.
- Anamnesis refuses an output root that is inside, or equal to, any project in the run.
- Every subprocess Anamnesis starts gets a working directory inside the run dir, never a repo.
- Inputs that tools write next to, such as archives for MTA's binary route, are copied into the run dir
  first.
- Anamnesis never passes `--overwrite` to MTA; it always creates a fresh directory.

## Consequences

Reports are found through the TUI ("open report location") or the path printed by the CLI, not by
looking in the repo. Two repos with the same folder name never collide, thanks to the path hash.

## Reversal

A future `--out` pointing anywhere outside the analyzed repos is already supported. Writing inside a
repo stays forbidden.
