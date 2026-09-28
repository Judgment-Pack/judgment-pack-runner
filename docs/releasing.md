# Releasing Runner

Only maintainers release this repository. A release is a tag a person pushes on a commit
already on `main`. [`release.yml`](../.github/workflows/release.yml) builds everything
else from that tag and publishes nothing until a maintainer approves it.

"Release" on this page means a **repository release**. It is unrelated to a frozen pack
release, a pack's `version`, a JPS `specVersion`, or the `jobs/1` companion protocol.

## What a release is

- Four archives, for Linux and macOS on `amd64` and `arm64`. Each holds `jpack-runner`,
  `jpack-source-worker`, `jpack-google-relay`, `openapi.json`, `LICENSE`, `README.md` and
  `THIRD_PARTY_NOTICES`.
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
| Formatting, vet, race tests and real-Runtime integration (CI, run again at the tag) | `amd64` | `arm64` |
| The released archive: all three programs present; `jpack-runner` and `jpack-source-worker` name the release; `jpack-runner` refuses to start without a boot line | `amd64` and `arm64` | `arm64` |

The `darwin/amd64` archive is built and checksummed and is not run by anything. The
archive checks do not start a job: that needs Runtime and a boot line from Desk, which
CI covers at the tag from source.

## Once, before the first release

1. Create the `production` environment with a required reviewer. The workflow reads the
   rule before it builds anything and refuses a repository without it, because GitHub
   creates a missing environment on first use with no protection.

   ```sh
   gh api -X PUT repos/Judgment-Pack/judgment-pack-runner/environments/production \
     -F 'reviewers[][type]=User' -F "reviewers[][id]=$(gh api user --jq .id)"
   ```

2. Turn on release immutability for the repository (Settings → General → Releases), so
   publishing locks the tag and the assets.

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

   Use the GoReleaser version the workflow names. A snapshot build names itself a
   snapshot: `jpack-runner version` prints `jpack-runner v<last tag>-SNAPSHOT-<commit>`.
3. Merge the pull request.

## Tag

Tag the merge commit on `main`, signed, and push only that tag:

```sh
git fetch origin
git tag -s <tag> -m "runner <tag>" "$(git rev-parse origin/main)"
git push origin <tag>
```

If no signing key is configured, stop and settle the signing policy. Do not replace a
signed tag with an unsigned one. A tag is never moved or reused; a fix is a new version.

A tag with a hyphen (`v0.1.0-rc.1`) is a prerelease: it is published as one and is never
marked latest. Make the first release a release candidate, since no run of the workflow
precedes it.

## What the workflow does

1. **Admits the tag.** It is an exact SemVer version, its commit is on `main`, its notes
   exist, and the `production` environment requires a reviewer.
2. **Runs CI at the tag.** The release calls [`ci.yml`](../.github/workflows/ci.yml)
   itself, so the tagged state passes every check a commit passes, against the Runtime and
   Gateway revisions CI pins at that tag.
3. **Packages without publishing.** One toolchain, named exactly in the workflow; no
   cgo; no recorded paths. The tag is written into the programs.
4. **Runs each archive on its platform**, as the table above states.
5. **Attests and drafts.** After every smoke test passes, the archives are attested and a
   draft release is created with the notes from the tag.
6. **Waits at the `production` gate.** Review the draft on the Releases page, then approve
   the pending deployment on the run. The draft is then published.

No maintainer token and no repository secret is used.

If a run fails, fix the cause on `main` and release a new version. Re-run a failed job
only when the failure was the hosted runner's.

## Verifying a download

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify <archive> --repo Judgment-Pack/judgment-pack-runner
```

On macOS use `shasum -a 256 --check --ignore-missing checksums.txt`.
`gh attestation verify` prints nothing when its output is not a terminal; add
`--format json` in a script.

A consumer that builds from source pins the tag and the commit it names.
