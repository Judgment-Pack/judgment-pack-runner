# Judgment Pack Runner

An open-source, local Jobs runner for [Judgment Packs](https://github.com/Judgment-Pack/judgment-pack-spec),
published at [Judgment-Pack/judgment-pack-runner](https://github.com/Judgment-Pack/judgment-pack-runner)
under the [Apache License 2.0](LICENSE).

Runner turns a reviewed pack snapshot into a durable job: check a release, submit
facts and evidence availability, run the pinned evaluator, and retain the result
and audit history. It also stores on-demand job and run briefs. All job artifacts
remain on the local filesystem, with SQLite holding the queue and records.

**Research preview — single-owner local Jobs pilot.** External actions, recurring
schedules, source polling, graph execution, shared-user permissions and AI agent
loops are not implemented in Runner. Closing a browser does not stop accepted work;
Desk and its runner companion must stay running on the host.

## Repository responsibilities

| Repository | Responsibility |
| --- | --- |
| [Specification](https://github.com/Judgment-Pack/judgment-pack-spec) | Pack format and JPS semantics. |
| [Runtime](https://github.com/Judgment-Pack/judgment-pack-runtime) | Pack validation, test evaluation, decisions and audit records. |
| **Runner** | Frozen releases, admission, durable queue, execution, input snapshots, history and saved job/run briefs. |
| [Desk](https://github.com/Judgment-Pack/judgment-pack-desk) | Jobs UI, authenticated proxy, source selection and explicit AI brief generation. |
| [Gateway](https://github.com/Judgment-Pack/judgment-pack-gateway) | Optional connected-source acquisition, such as a selected Google Drive JSON file, through Desk. |

Runner does not contact Gateway or an identity provider itself. Manual inputs and
local JSON uploads need no Gateway. Google Drive supplies input bytes through Desk;
it is not an artifact store. Runner uses Runtime's public CLI rather than embedding
a second evaluator.

## Compatibility and versions

The initial local pilot was verified with these source revisions:

| Component | Verified revision |
| --- | --- |
| Desk | [`e36244e`](https://github.com/Judgment-Pack/judgment-pack-desk/commit/e36244eb172a69b84175ca85c85150eaaa5aec82) |
| Runtime | [`6842494`](https://github.com/Judgment-Pack/judgment-pack-runtime/commit/6842494ff0492c4f1a3bd185451d7d9c21a8e97e) |
| Gateway, for connected inputs | [`v0.3.1`](https://github.com/Judgment-Pack/judgment-pack-gateway/releases/tag/v0.3.1), as pinned by Desk |

These are a reproducible baseline, not a promise that every earlier release supports
the Jobs contract. Build matching Desk and Runtime revisions when using this pilot.
Runner's companion protocol is `jobs/1`; its HTTP contract is in [openapi.json](openapi.json).
Repository release versions, a pack's `version`, and its JPS `specVersion` are separate.
GitHub topic tags describe the repository; they do not indicate release compatibility.

## Build and run

Requires Go 1.25+ and Linux, macOS or FreeBSD (the single-dispatcher lock is currently
implemented on these hosts). SQLite is embedded via `modernc.org/sqlite`; no database
server or CGO is required.

```sh
git clone https://github.com/Judgment-Pack/judgment-pack-runner.git
cd judgment-pack-runner
go build -trimpath -o bin/jpack-runner ./cmd/jpack-runner
/path/to/jpack-desk --runner "$PWD/bin/jpack-runner" --jpack /absolute/path/to/jpack /path/to/project
```

Alternatively install `jpack-runner` beside `jpack-desk`. Desk discovers that sibling
binary. The runner is a long-lived Desk companion, not a browser task. Closing every
browser tab does not stop accepted work. Stopping Desk stops the companion; restarting
Desk resumes queued work and marks previously running work interrupted. A shut down
computer does not execute jobs. This is not a hosted scheduler or a Vercel function.

The executable receives one bounded JSON boot line on stdin with `dir`, `runtime`,
`workspace`, `owner`, and a random `token`, with optional trusted `inputProfiles` for v2. It binds an ephemeral IPv4 loopback port,
prints `{"protocol":"jobs/1","url":"http://127.0.0.1:..."}`, and stays alive until
stdin closes. The bearer is never printed, passed in argv, read from a project file,
or sent to the browser. Desk selects the workspace and stable installation owner.
Direct browser origins are refused by the control API. Desk's API uses its existing
bearer and origin guards. The pilot has one local owner, not delegated machine roles.

## First job

1. Open **Jobs → Create job**, or **Packs → More → Create job**.
2. Choose a saved pack and enter sample facts, with optional evidence availability.
3. Select **Check release**. Runtime validates the exact pack, creates its reviewed-set
   lock, evaluates the sample as rehearsal, and runs its saved test expectations.
4. Review the fixed release, test report and advisory coverage, then create the job.
   Failed/incomplete tests block creation; releases without tests require explicit
   review as untested. A sample preview is not a passing test suite.
5. Use **Run job** for a new operational input, or submit through the API.

A release retains the exact pack text, pack digest, generated config and lock,
validation output, sample inputs/output, and a digest-pinned copy of the Runtime
executable. A job's only revision in this pilot is revision 1. Later edits to a pack
cannot change an existing job. Create and review a new job for changed policy.
The evaluator is Runtime's **experimental JPS 0.2.0-draft** contract, outputVersion 2;
this package does not expand Runtime's conformance claim or approve external action.

For manual inputs and version 1 file mappings, evidence is a caller-declared map of requirement IDs to `present`, `absent`, or
`unknown`. It is not verified source acquisition. Omitted evidence is preserved as
omitted; an empty supplied object remains a supplied object. Omitted facts and false
values remain distinct. Use the explicit file-input path below to map selected JSON into these inputs;
Runner does not fetch or interpret evidence documents itself.

## Release readiness

Desk's **Check release** sends the current saved expectations alongside the exact
pack and sample inputs. Runner uses the digest-pinned Runtime's `packs test`
command in a private directory containing only the frozen pack and matrix. It
retains the matrix text/digest, pack and Runtime digests, timestamp, optional Desk
suite metadata, full report, and failed-attempt diagnostics with the release.
Prior Tests-page results and unsaved AI proposals are never reused as a pass.

`tests` is `passed`, `failed`, `error`, or `not-run`. All saved expected cases must
run and match before `passed` is accepted. An empty/malformed supplied matrix,
incomplete response, changed identity, or abnormal process exit cannot pass.
Failed and incomplete checks remain reviewable but cannot create jobs, including
through the direct API. A caller may omit a matrix and explicitly review an
untested release; this is labeled `not-run`, never passed. Existing untested jobs
keep their original records. No coverage probe blocks admission: gaps are advisory.

Desk rereads the pack and saved cases before checking and again before creation;
changed inputs require a new review. Once admitted, the job stays bound to its
frozen release regardless of later edits. API callers supply their own matrix;
Runner does not claim that a caller-supplied suite exhausts a project's tests.
Job and run briefs include retained release test evidence when generated on demand.

## File inputs and connected sources

Choose manual facts, a local JSON file, or a user-selected Google Drive JSON file.
A file represents **one input record**, not a batch. Every artifact remains in local
storage. Drive is read-only input acquisition through the existing Gateway and
single-use picker grants; it is not Jobs storage and no file is uploaded to Drive.
Local uploads do not require Gateway or AI. File mappings do not invoke a model.

`POST /v1/inputs/preview` accepts `{"source":{"mapping":...,"snapshot":...}}`.
The mapping has version `1`, provider `local-file` or `google-drive`, a `facts`
array of `{target,source}` JSON Pointers and an `evidence` array of
`{requirement,source}`. Missing source paths remain omitted; false, zero and null
are retained. Evidence values must be literal `present`, `absent` or `unknown`.
Availability is a caller declaration, never inferred from having a connection.

A snapshot contains `version:1` and `original:{name,mediaType,bytes,sha256}` with
canonical base64 bytes and a `sha256:` digest. Local files include an RFC3339
`selectedAt` timestamp and no `proof`. Drive files retain the existing Desk
DocumentObject acquisition response, seal registry and selection binding.
Files must be UTF-8 JSON objects, at most 200,000 bytes and 32 levels deep, without
duplicate keys or unpaired Unicode surrogates. Fractional and large integer
literals are preserved; the mapping preview returns `factsText`/`evidenceText`
for exact display. Send the source-only envelope to avoid browser number rounding.

Release previews freeze the mapping. Each later run must supply a fresh selected
snapshot using that exact provider and mapping. A different mapping requires a
new job/release. The runner retains original bytes, mapping/digest, source metadata
and proof with each run. Retries use the same snapshot and idempotency key. Manual
jobs remain compatible with their existing input contract. HTTP file submission
is also available; the runner never reads arbitrary filesystem paths or fetches
remote URLs on a caller's behalf.

Desk independently verifies Drive receipts against the **current configured pin**
on acquisition and before release checks, creation and run submission. A retained
run's receipt is rechecked when viewed. Runner validates original hashes and
receipt-to-document bindings for **version 1**, but does **not** cryptographically authenticate an
API caller's receipt. API consumers must perform that verification themselves;
there is no runner-issued authenticity badge. Even a valid receipt attests to
acquisition, not the truth of supplied evidence. Brief inputs include compact
provenance and mapped facts, excluding raw file bytes and consumed picker grants.

This is explicit file selection per run. Background acquisition, recurring
schedules, batch records, CSV mapping, and standing source grants are not included.

## Verified mapping v2 (Runner and Desk)

Runner now supports ordered case/file/MCP input mappings with cryptographic v3
receipt verification, exact request commitments, trusted source classifications,
validated dependency chaining and per-target lineage. Releases freeze the mapping
and operator profiles; operational evaluations retain matching Runtime citations.
An offline `verify-run` command checks retained inputs against independently trusted
profiles and a release digest. V1 behavior and records remain unchanged.

Desk's **Mapped sources** workflow now supports named-source review, advanced JSON
mapping edits, explicit acquisition and fixed-release runs through the
`/v1/inputs/next` planner. Supply trusted installation profiles with Desk's
`--runner-input-profiles /absolute/file.json` flag. The original v1 file picker
remains available. This integration requires the matching local working-tree
builds; the revision table above identifies the previously published v1 baseline.
HTTP SQL/model adapters, sealed-session verification and scheduling remain later work.
The [v2 contract and API flow](docs/MAPPING-V2.md) states the supported scope,
trust assumptions, numeric limits and receipt-only verification guarantees.

## Saved one-page briefs

In Desk, open a job or a finished run, select **Brief** on the right rail, and choose
**Generate brief**. Desk sends the frozen source snapshot to the configured Assistant;
Runner persists the resulting brief, source snapshot and revision history. Reopening
the brief reads that saved version without another model call. **Regenerate brief**
creates a new revision and retains the previous one.

The brief combines context, findings, uncertainty and next action with required
evidence and recorded results. Evidence availability and evaluation results come
from the retained record, separately from AI prose. A sample result remains labeled
as a sample; a brief is not an approval, new evaluation or verification of evidence.
Saved briefs are shared by views of the same local workspace and installation;
this pilot does not add multi-user sharing or access control. Test-case briefs are
stored by Desk, outside Runner's job/run records.

## API

See [the HTTP contract](openapi.json). Desk exposes each `/v1/<suffix>` operation at
`/api/operations/<suffix>` with its existing authenticated bearer. There is no secret
in the URL. Example, with environment variables populated privately by the caller:

```sh
curl --fail-with-body "$DESK_URL/api/operations/jobs/$JOB_ID/runs" \
  -H "Authorization: Bearer $DESK_BEARER" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $SUBMISSION_ID" \
  --data-binary @input.json
```

`input.json` contains `{"facts": {...}}` and optionally `"evidence": {...}`.
A new acceptance returns 202 only after its queue row and idempotency binding commit.
A repeated key with the same input returns 200 and the original run; a different
input returns 409. Keys are scoped to job and authenticated installation owner and
retained with history. Repeating job creation with the same reviewed preview and name
returns the existing job. Changing inputs invalidates the UI's preview approval.

Lists return at most 50 records, newest first, with `next` as the next `after` cursor
(or 0 at the end). Run lists omit raw inputs and audits; open a run for those records.
Input bodies are limited to 2 MiB, packs and matrices to 1 MiB each, pending work to 100 runs, and each
Runtime invocation to 30 seconds. There is one dispatcher per private workspace store.

## Durability and recovery

SQLite uses WAL and synchronous FULL. The queued record includes the immutable
release ID and input snapshot. Before invoking Runtime, the dispatcher persists
`running`. Each run has a unique private attempt directory and one attempt in this
pilot. The non-rehearsal call records to that directory's Runtime audit trail.
Completed means the response and single audit record agree on pack, inputs,
disposition, reviewed set and the supported contract. Reject, unresolved and
not-applicable decisions can all have execution state **Completed**.

An evaluator refusal is **Failed**, with no published decision. Process interruption
is **Interrupted**, not an invented failure decision. If a process dies between audit
append and result commit, the next process retains the attempt files and marks the
run interrupted. It does not automatically replay or claim the partial result was
completed. Inspect retained stdout/stderr and audit before choosing a new submission.
There is no exactly-once claim, automatic recovery reconciliation, or automatic retry
of a possibly evaluated invocation. External actions are not enabled.

## State and backups

Desk uses `<Desk config directory>/jobs/<project-path digest>/`. The runner refuses
symlinked/private-state directories or unsafe database permissions. Files are private;
the database and attempt directories can contain sensitive supplied facts. State is
scoped to the configured project path and installation; project relocation and moving
Jobs storage need a later migration feature. Do not rename the store to impersonate
another workspace or owner.

Jobs are **not included in Desk's chat/workspace backup**. For a complete pilot backup,
stop Desk and the runner, then copy the entire Jobs workspace directory, including the
SQLite database, any WAL files, pinned runtimes, releases and attempts. Restore the
entire directory with its private permissions in the same installation/project scope.
Do not copy a live SQLite file by itself. Retention/deletion and hosted backup are
not implemented yet; plan disk capacity before sustained use.

## Verification

```sh
JPACK_TEST_BIN=/absolute/path/to/jpack go test -race ./...
go vet ./...
```

Tests exercise the real executable: release preview, audit/input binding, normal and
missing-evidence decisions, duplicate submissions across restart, queue recovery,
exclusive dispatcher ownership, Runtime drift, HTTP authority, and forced exit after
a real audit append but before the runner records completion. Integration tests require
`JPACK_TEST_BIN`; without it they report skipped rather than claim a real evaluation.
The fixture is a synthetic Apache-2.0 Runtime specification example, not business policy.

## License and contributions

This repository is public and licensed under [Apache-2.0](LICENSE). Report bugs or
propose changes through [GitHub issues](https://github.com/Judgment-Pack/judgment-pack-runner/issues)
and pull requests. Include a reproduction and relevant verification results. Use
`git commit -s` to add a DCO sign-off, consistent with the Judgment-Pack repositories.
Do not include real credentials, customer evidence or private operational records
in public issues, examples or test fixtures.

CI runs formatting, vet, race tests and real-Runtime integration on Linux and
macOS. The workflow pins Runtime revision `6842494` so release, audit, mapping
and offline verification tests run against a reproducible evaluator contract.
