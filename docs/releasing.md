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
| Formatting, vet, race tests and real-Runtime integration (CI, run again at the tag) | `amd64` | `arm64` |
| The released archive, run: all three programs present; `jpack-runner` and `jpack-source-worker` name the release; `jpack-runner` refuses to start without a boot line | `amd64` and `arm64` | `arm64` |
| The released archive, read and not run: its files are the commit's, byte for byte; each program was built from the package of its name; it holds nothing else | `amd64` and `arm64` | `amd64` and `arm64` |

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
   workflow compares the tag with the commit the run was started for.

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

A tag is `vX.Y.Z` or `vX.Y.Z-<prerelease>`, as SemVer 2.0.0 writes them. Build metadata
(`+...`) is refused. A tag with a hyphen (`v0.1.0-rc.1`) is a prerelease: it is published
as one and is never marked latest. Make the first release a release candidate, since no
run of the workflow precedes it.

## What the workflow does

Every job works on the commit the run was started for, by its digest, not on the tag's
name.

1. **Admits the tag.** It is a version as above, it names the commit the run was started
   for, that commit is on `main`, its notes exist, and the `production` environment has a
   reviewer rule.
2. **Runs CI at that commit.** The release calls the commit's own
   [`ci.yml`](../.github/workflows/ci.yml), against the Runtime and Gateway revisions it
   pins. A check `main` has gained since that commit is not run.
3. **Packages without publishing.** One toolchain, named exactly in the workflow; no
   cgo; no recorded paths. The tag is written into the programs. All four archives are
   then opened and read, as the table above states.
4. **Runs the archives of three targets**, each on a runner of its own platform.
5. **Attests and drafts.** After every smoke test passes, and only if the tag still names
   the commit, the archives are attested and a draft release is created with the notes
   from the commit.
6. **Waits at the `production` gate.** Review the draft on the Releases page, then approve
   the pending deployment on the run. The tag is compared with the commit once more, and
   the draft is published.

No maintainer token and no repository secret is used.

### What exists before the approval

The gate is on the release. Three things exist before it and are not secret:

- the archives, as an artifact of the workflow run, downloadable for seven days by anyone
  who can read the repository's runs;
- the attestation of those archives, recorded in a public transparency log when it is
  made. It says which workflow built an archive, from which commit. It does not say a
  maintainer approved it;
- the draft, visible to those who can write to the repository.

### If a run fails

Fix the cause on `main` and release a new version. Re-running is for a failure that was
the hosted runner's: re-run the failed jobs, not all jobs.

The draft job refuses to run while a release under the tag exists, draft or published.
If a failed run left a draft, read it, delete it by hand (`gh release delete <tag> --repo
Judgment-Pack/judgment-pack-runner`, which leaves the tag), and re-run the failed job. A
published release is never deleted to make room for another.

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
