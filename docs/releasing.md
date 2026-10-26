# Releasing Runner

Only maintainers release this repository. A release is a tag a person pushes on a commit
already on `main`. [`release.yml`](../.github/workflows/release.yml) builds everything
else from that commit and publishes no release until a maintainer approves it.

"Release" on this page means a **repository release**. It is unrelated to a frozen pack
release, a pack's `version`, a JPS `specVersion`, or the `jobs/1` companion protocol.

## What a release is

- Four archives, for Linux and macOS on `amd64` and `arm64`. Each holds `jpack-runner`,
  `jpack-source-worker`, `jpack-google-relay`, `openapi.json`, `LICENSE`, `README.md` and
  `THIRD_PARTY_NOTICES`, and nothing else.
- `checksums.txt`, the SHA-256 of each archive.
- A build-provenance attestation for each archive, signed by GitHub for this workflow at
  the tagged commit.
- The notes a person wrote, from `docs/releases/<tag>.md`.

A release does not include Runtime, Gateway or Desk. It does not include a Windows
build: Windows compiles, and nothing tests it. It does not include the Cloud Run image
under `deploy/google-cloud/`.

## What is tested where

| | Linux | macOS |
| --- | --- | --- |
| Formatting, vet, race tests and real-Runtime integration (main CI at the tagged commit, its evidence taken at the tag: [CI at the tag](#ci-at-the-tag)) | `amd64` | `arm64` |
| The released archive, run: all three programs present; `jpack-runner` and `jpack-source-worker` name the release; `jpack-runner` refuses to start without a boot line | `amd64` and `arm64` | `arm64` |
| The released archive, read and not run: it holds files and nothing else, each under its plain name and once; its documents are the commit's, byte for byte; each program is one program, built from the package of its name, for the archive's platform, at the lowest level of its architecture, the bytes the packer built, readable and executable by everyone; no file is there that a release does not hold; the archive reads to its end | `amd64` and `arm64` | `amd64` and `arm64` |

What the reading of the archives is for: a mistake in the packaging, such as a program
built from the wrong package or for the wrong platform, or an archive cut short. It
accepts a member only in the form the packer writes one, and reads an archive as
Python's standard library does. It is not a defence against an archive made to be read
differently by different programs: whoever could put one where the check looks could
change the check.

A program is compared with the bytes the packer wrote when it built it, by the packer's
own account (`dist/artifacts.json`). Those bytes are checked against nothing but their
build record: the check does not say the packer built a program well.

The form is the packer's for what this repository gives it today: short names in plain
letters, files of ordinary size, modes as the checkout has them. A file added under a
long name or one not in plain letters, or an upgrade of the packer, may be written in a
form the check refuses. Look into such a refusal: the check may have to follow.

The `darwin/amd64` archive is built, read and checksummed and is not run by anything.
`jpack-google-relay` is not started by any release check. The archive checks do not
start a job: that needs Runtime and a boot line from Desk, which CI covers at the tagged
commit from source.

## Once, before the first release

1. Create the `production` environment with a required reviewer. The workflow reads the
   rule before it builds anything and refuses a repository without it, because GitHub
   creates a missing environment on first use with no protection.

   ```sh
   gh api -X PUT repos/Judgment-Pack/judgment-pack-runner/environments/production \
     -F 'reviewers[][type]=User' -F "reviewers[][id]=$(gh api user --jq .id)"
   ```

   The workflow reads that a reviewer rule exists. It does not read who the reviewers
   are, and it cannot stop an administrator from changing the rule later. With one
   maintainer, the person who pushed the tag is the person who approves: the gate is a
   pause to read the draft, not a second person.

2. Turn on release immutability for the repository (Settings → General → Releases). It
   locks the tag and the assets once a release is published, not before. Until then the
   workflow compares the tag with the commit the run was started for. The workflow does
   not read this setting.

## Prepare the release

1. Open a pull request that adds `docs/releases/<tag>.md`: what changed since the last
   release, which Runtime, Gateway and Desk revisions it was verified with, and what an
   operator must do. It is the text the release page will carry.
2. Validate locally, from a plain clone of the branch (a snapshot build fails inside a
   `git worktree`):

   ```sh
   go fmt ./... && git diff --exit-code
   go vet ./...
   JPACK_TEST_BIN=/absolute/path/to/jpack go test -race ./...
   python3 tools/third_party_notices.py --check
   goreleaser check
   goreleaser release --snapshot --clean --skip=publish
   ```

   Use the GoReleaser version and the Go toolchain the workflow names.
   `THIRD_PARTY_NOTICES` includes the toolchain's own licence, so it is written and
   checked with the toolchain that links the release. A snapshot build names itself a
   snapshot: `jpack-runner version` prints `jpack-runner v<last tag>-SNAPSHOT-<commit>`.
3. Merge the pull request, and let main CI at the merge commit complete: its run is the
   evidence the release takes, for 48 hours from its start ([CI at the tag](#ci-at-the-tag)).

## Tag

Tag the merge commit on `main`, signed, and push only that tag:

```sh
git fetch origin
git tag -s <tag> -m "runner <tag>" "$(git rev-parse origin/main)"
git push origin <tag>
```

If no signing key is configured, stop and settle the signing policy. Do not replace a
signed tag with an unsigned one. Signing is the maintainer's practice; the workflow does
not check it. A tag is never moved or reused; a fix is a new version.

`Admit the tag` takes `vX.Y.Z` or `vX.Y.Z-<prerelease>`, as SemVer 2.0.0 writes them, and
requires an annotated tag (`git tag -s` makes one). The evidence gate accepts the same grammar:
`.github/ci-evidence.json` sets `tag.prerelease: true` (kit v0.11.0), so a prerelease such as
`v0.1.0-rc.1` passes the gate like a release and is published as a prerelease, never marked
latest. A lightweight tag is refused (`tag-annotated`).

## What the workflow does

Every job works on the commit the run was started for, by its digest, not on the tag's
name.

1. **Admits the tag.** It is a version as above, it names the commit the run was started
   for, that commit is on `main`, its notes exist, and the `production` environment has a
   reviewer rule.
2. **Takes main's CI evidence at that commit, or refuses** (`CI at the tag`). Both `Runner`
   jobs and `Release contents` of the push-to-main run of
   [`ci.yml`](../.github/workflows/ci.yml) at the exact commit, every check passed, against
   the Runtime, Gateway and old-Runner commits the commit pins, with the release toolchain.
   `ci.yml` is not run again; a refusal stops the release
   ([CI at the tag](#ci-at-the-tag)).
3. **Packages without publishing.** One toolchain, named exactly in the workflow; no
   cgo; no recorded paths; `amd64` at `v1` and `arm64` at `v8.0`. The tag is written into
   the programs. All four archives are then read by `tools/release_archives.py`, as the
   table above states. CI runs that script against sound archives and against archives
   spoiled one way at a time.
4. **Runs the archives of three targets**, each on a runner of its own platform.
5. **Attests and drafts.** After every smoke test passes, and only if the tag still names
   the commit, the archives are attested and a draft release is created with the notes
   from the commit.
6. **Waits at the `production` gate.** Review the draft on the Releases page: the notes
   and the five assets. Approve the pending deployment on the run.
7. **Compares the draft with the run, and publishes.** A draft can be changed while it
   waits, by anyone who can write to the repository. After the gate, the tag is compared
   with the commit once more, and the run's own files are held to their checksums. The
   release the run drafted is then read through the API by its id, without downloading it:
   its state, the title the workflow gave it, the notes the commit holds (compared without
   the newlines either ends in), and the digest GitHub reports for each asset, which must
   be the files the run packaged, no more and no fewer. Then it is published by that id,
   never by tag, and the published assets' digests are read once more; if they differ the
   job fails at `Require the published release to hold what this run packaged`, printing
   both sets. An approval given more than thirty days after the run finds the run's
   archives gone, and the job fails: release a new version.

### The window that remains

The API cannot publish a release on condition that its assets are the ones just read, so one
window remains: between the last read of the draft's asset digests and the publish call, a few
moments of the same step. Whoever can write to the repository's releases can act in it: a
repository writer, which in this organisation is a maintainer, the same people the
`production` gate asks to approve. Nothing in the workflow closes that window. A file
replaced in it is published, and the read after publication then fails the run by name; with
release immutability on, the published release can no longer be changed, so the replacement
stays visible as a failed release and is not quietly corrected.

No maintainer token and no repository secret is used.

### What exists before the approval

The gate is on the release. Three things exist before it and are not secret:

- the archives, as an artifact of the workflow run, downloadable for thirty days by
  anyone who can read the repository's runs;
- the attestation of those archives, recorded in a public transparency log when it is
  made. It says which workflow built an archive, from which commit. It does not say a
  maintainer approved it;
- the draft, visible to those who can write to the repository.

### If a run fails

Fix the cause on `main` and release a new version. Re-running is for a failure that was
the hosted runner's: re-run the failed jobs, not all jobs.

What to do with a draft depends on which job failed:

| The job that failed | The draft | What to do |
| --- | --- | --- |
| `CI at the tag` | none was made | read the refusal (the step's last line, or the run's `release-evidence-<attempt>` artifact) and take the [fresh route](#the-fresh-route) once. A second refusal: fix the cause on `main` and release a new version |
| Any other job before `Attest and draft release` | none was made | re-run the failed jobs |
| `Attest and draft release` | may exist, and may lack assets | read it, delete it by hand, then re-run the failed jobs. The job refuses to run while a release under the tag exists, draft or published, and refuses when it cannot find out |
| `Publish release`, before publication | needs looking at: it may be as it was made, changed, incomplete or gone | read why the job failed, and look at the Releases page. **Keep** a draft that is as it was made, and re-run the failed jobs: that job only publishes the release id the run drafted. Do not publish a draft that was changed or is incomplete. Where the draft cannot be published, release a new version |
| `Publish release`, at `Require the published release to hold what this run packaged` | the message says the published release does not hold what this run packaged, and prints both digest sets (published, holding files this run did not package, or not published after the call) | someone who can write to the repository changed the release in the window above; the digest sets shown are the evidence. Treat it as an incident: tell the maintainers, point no one at the release, and release a new version from a new tag. Do not re-run |
| `Publish release`, at `Require the published release to hold what this run packaged`, with the message "could not be verified" | published, not yet checked: an API or network error, or an unusable read, stopped the check. No digest sets are printed and nothing is diagnosed | **Do not re-run** the publish step. Re-read the same release id and compare its asset digests with the run's: `gh api repos/Judgment-Pack/judgment-pack-runner/releases/<id>/assets --paginate --jq '.[] \| .name + " " + .digest'` (the id is in the run's `Attest and draft release` output and in the failed step), against the digests in the run's `checksums.txt` for the four archives and `sha256sum checksums.txt` for the list itself. If they match, the release is good. If they differ, treat it as the row above: an incident, and a new version from a new tag |

Delete a draft with `gh release delete <tag> --repo Judgment-Pack/judgment-pack-runner`,
which leaves the tag. A published release is never deleted to make room for another.

## CI at the tag

The release does not run CI again at a commit main has already passed. `CI at the tag` runs
the development kit's release evidence gate, vendored as
[`scripts/dev/release_evidence.py`](../scripts/dev/release_evidence.py) (the kit release on its
line 2), with this repository's configuration,
[`.github/ci-evidence.json`](../.github/ci-evidence.json). The gate's contract, its refusal
codes and its decision record are the kit's `docs/ci-evidence.md`; what follows is what it
holds this repository's release to.

Main CI records the evidence. On a push to `main`, each `ci.yml` job, after its checks, writes
the commit, the git object ids of its inputs, its runner's image and tools, and each check's
result as read from the `check.sh` summary, and uploads it as the artifact
`ci-evidence-ci-<job>-<attempt>`. Pull request runs record nothing.

The release reuses that evidence only when all of this holds, and refuses otherwise:

- **One run.** The `CI` workflow (`.github/workflows/ci.yml`, by its id), run by a push to
  `main` in this repository at the tagged commit itself, completed and successful, its newest
  attempt; no newer failed, cancelled or running attempt of a `main` run at that commit, and
  none unfinished. Pull request runs, other branches' runs and runs from forks neither count
  nor block. The attempt started at most 48 hours before the release reads it.
- **Every job.** `Runner (ubuntu-latest)`, `Runner (macos-latest)` and `Release contents`,
  each `success` on a GitHub-hosted runner with its one label, from the selected attempt; no job
  may be skipped and no other job may be there.
- **Every check, passed.** The Runner legs: `local-paths`, `agent-docs`, `diff-check`,
  `go-fmt`, `go-vet`, `build-pinned`, `manual-gate`, `unit-tests`, `chain-tests`, `openapi`,
  `build`, `ci-evidence`. `Release contents`: `release-toolchain`, `third-party-notices`,
  `release-archives`, `release-programs`. Each `PASS`; no skip is permitted.
- **The real companions ran.** `build-pinned` builds the Runtime, earlier Runtime, old Runner
  and Gateway at the commits `scripts/checks/build-pinned.sh` pins. `manual-gate`,
  `chain-tests` and `unit-tests` fail when one of the tests they name did not run and pass, so a
  `PASS` of each, on both Runner legs, is the proof that those tests ran against the pinned
  builds and did not skip. Two of them, with two subtests, are required on Linux only, because
  they observe the Runtime through Linux `/proc`, and may skip on macOS by design:
  `TestManualGovernedActualRuntimeHasNoAuthorityFD` (`manual-gate`) and
  `TestExecutorOwnedRealRuntimeSurvivesOwnerDeath` with `/close` and `/kill` (`chain-tests`).
  The evidence carries the object ids of those four scripts, of `go.mod` and `go.sum`, of
  `scripts/checks/release-toolchain.sh`, `scripts/dev/check.sh`, the `Release contents` scripts
  (`scripts/checks/release-programs.sh`, `tools/third_party_notices.py`,
  `tools/release_archives.py`, `tools/test_release_archives.py`) and `.goreleaser.yml`, both
  workflows, `dev.json`, the configuration and the gate, and the release derives the same ids
  from its own checkout.
- **The release toolchain.** All three jobs set up Go 1.26.5, the toolchain `package` links the
  archives with. The two Linux jobs' image, OS release, Go, Python, git and bash must equal what
  this job's runner resolves with the same Go set up; the macOS job must have run Go 1.26.5 on
  macOS ARM64 with the image family `macos26` (`ImageOS`). A newer image of that family is
  accepted for the 48 hours the evidence lasts: its version is recorded in the decision record
  and not compared.
- **The tag.** The tag is annotated, names the commit, and is the same tag object when the
  gate starts and after it has read everything.

The decision, `reuse` or `refuse` with its code, is the step's last line; the whole decision
record (runs, attempt, jobs, artifacts, inputs, toolchains, check results) is in the step's
log and kept as the run's `release-evidence-<attempt>` artifact. Everything after
`CI at the tag` is checked fresh, as before: the tag against the commit, the notes, the
`production` reviewer rule, the notices again with the toolchain that links the release,
packaging, checksums, the contents of every archive, the native archive runs, the attestation,
the draft and the published assets.

| | Main CI (push to `main`) | Release before (v0.24.0, v0.25.0) | Release now |
| --- | --- | --- | --- |
| The `Runner` checks, Linux and macOS, with the pinned companions | run | run again (`ci.yml` called), with Go 1.26.x | taken from main's evidence |
| `Release contents` | run | run again | taken from main's evidence |
| Go in the `Runner` jobs | 1.26.5 (it was 1.26.x, which resolved to 1.26.9 in run 40395171085) | 1.26.x | 1.26.5, compared |
| Tag, notes, reviewer rule, packaging, notices, archives, native runs, attestation, draft, publication | not run | run | run, unchanged |

Measured (job start to completion, from the API): v0.24.0 (run 39287501642) ran `CI at the
tag` as `Release contents` 92 s, `Runner (ubuntu-latest)` 581 s and `Runner (macos-latest)`
620 s, 651 s from the end of `Admit the tag` to the start of `Package`; v0.25.0 (run
40371047612) 74 s, 589 s and 488 s, 594 s. The gate's own time is measured at the next
release.

### The fresh route

A refusal stops that release run; nothing runs CI again on its own. Read the refusal, then,
once:

1. Re-run **all** jobs of the main CI run at the commit:
   `gh run rerun <run id> --repo Judgment-Pack/judgment-pack-runner` (the run id is in the
   decision record). A failed-jobs-only re-run leaves the other jobs' evidence in an earlier
   attempt and is refused. This also works after `main` has moved on, and is the route for
   `run-age` (evidence older than 48 hours) and `runtime-identity` (the hosted image changed
   since main's run).
2. When that run has completed, re-run the release run's failed jobs:
   `gh run rerun <release run id> --failed --repo Judgment-Pack/judgment-pack-runner`.
3. If the gate refuses again, or the evidence cannot be made at that commit (a failure that is
   the code's, inputs that stay incompatible), fix the cause on `main` and release a new
   version. One exception: `runtime-identity` naming the Linux image version while GitHub is
   rolling a new hosted image out (main's jobs and this job landed on different versions) is
   not the code's. Wait for the rollout to finish and take the route again; it needs no new
   version.

`run-unfinished` usually means the tag was pushed while main CI at the commit was still
running: wait for it to complete, then step 2. A lightweight tag cannot pass;
see [Tag](#tag).

The gate is first exercised live on the first push to `main` after it lands (the evidence
capture) and at the next release (the gate itself). The tests that ship with the kit run it
over handwritten API answers; `tools/test_ci_evidence.py` holds the configuration to `ci.yml`,
`release.yml` and `dev.json`, and the vendored gate to the kit's.

### Moving a pin

- **The Go toolchain.** Every `go-version` in `ci.yml` (`Runner` and `Release contents`) and
  `release.yml` (`Package` and `CI at the tag`), and the two `go` pins in
  `.github/ci-evidence.json`, move together in one pull request; `tools/test_ci_evidence.py`
  and `release-toolchain` fail until they name one version. CI stays on that exact patch, not
  on the newest patch the hosted runners hold.
- **The macOS image family.** When GitHub points `macos-latest` at another macOS, the first
  push to `main` after it fails at `Runner (macos-latest)`'s capture step with
  `runner-platform` ("ImageOS is <new>, not macos26"), and a release refuses with
  `runtime-identity`. Pull request CI stays green: it records no evidence. Move
  `profiles.macos.platform.ImageOS` to the value the refusal names, in one pull request; its
  run on `main` records the new evidence.

## Verifying a download

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify <archive> \
  --repo Judgment-Pack/judgment-pack-runner \
  --signer-workflow Judgment-Pack/judgment-pack-runner/.github/workflows/release.yml \
  --source-ref refs/tags/<tag> \
  --source-digest <commit>
```

The first command checks the archive against `checksums.txt`, which is only as good as
where that file came from. The second checks it against an attestation made in this
repository, by this workflow, in a run started for that tag, from that commit. With
`--repo` alone it would check the repository and nothing more. `<commit>` is the full
digest of the commit the tag names (`git rev-parse '<tag>^{commit}'`). Neither command
says a maintainer approved the release: its being published says that.

On macOS use `shasum -a 256 --check --ignore-missing checksums.txt`.
`gh attestation verify` prints nothing when its output is not a terminal; add
`--format json` in a script.

A consumer that builds from source pins the tag and the commit it names.
