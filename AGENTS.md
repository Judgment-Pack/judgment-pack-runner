# Working in this repository

This file is read by Codex as `AGENTS.md` and by Claude as `CLAUDE.md`; the two are the same text.
Edit `AGENTS.md` and run `scripts/sync-agent-docs.sh` to regenerate `CLAUDE.md`.

## Before you open a pull request

- Run `scripts/dev/check.sh` (or `jpdev check` where the kit is installed) once. It runs the
  checks listed in `dev.json` in order and prints one summary block.
- Paste that summary block in the pull request body, under a "Checks" heading.
- If a check fails: read its first lines in the block, fix the cause, re-run that one check with
  `--only NAME`, then run the whole list once more. Do not loop on the whole list.
- Start no CI watch. Push, report once, and stop; CI owns everything deterministic.

## Rules

- Write test vectors and fixtures by hand. Never record them "from a run" of the code under test.
- Keep machine-local paths and personal names out of tracked files, commit messages and pull
  request bodies. Refer to people by role or handle where a name is needed.
- Sign off every commit (`git commit -s`). Never force-push; never rewrite published history.
- End a process you started by its process id. Never use a pattern kill (`pkill`, `killall`).
- One pull request per issue or batch. State what changed and why in plain, factual wording.
- A change to a public surface (a schema, a contract, a command's output) says so in the pull
  request and changes its vectors and documentation in the same pull request.
- If a safeguard stops you (a permission prompt, a refused command, a failed gate), stop and say
  so in your report. Do not work around it.

## Reports

Your final report is short: the pull request and its head commit, what changed, the check
summary block, and anything you stopped on or chose not to do.

## This repository

- The checks are `dev.json`, run by `scripts/dev/check.sh`; CI runs the same list in the
  `Runner (ubuntu-latest)` and `Runner (macos-latest)` jobs, and the four release steps again in
  `Release contents` with the exact Go toolchain `release.yml` names. Branch protection requires
  those job names. The longer steps are scripts under `scripts/checks/`.
- The real-Runtime tests need the binaries `scripts/checks/build-pinned.sh` builds into `.ci/bin`
  from pinned Runtime, Gateway and old-Runner commits (it needs network access to fetch them). A
  pin and the release name beside it are moved together. The two test steps fail if a named test
  does not run and pass, so a skip is a failure there.
- Runner is a single-owner local Jobs pilot; do not add shared-user permissions, external actions
  or agent loops. Do not put real credentials, customer evidence or private operational records in
  examples or fixtures.
