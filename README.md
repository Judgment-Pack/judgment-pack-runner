# Judgment Pack Runner

An open-source, local Jobs runner for [Judgment Packs](https://github.com/Judgment-Pack/judgment-pack-spec),
published at [Judgment-Pack/judgment-pack-runner](https://github.com/Judgment-Pack/judgment-pack-runner)
under the [Apache License 2.0](LICENSE).

Runner turns a reviewed pack snapshot into a durable job: check a release, submit
facts and evidence availability, run the pinned evaluator, and retain the result
and audit history. It also stores on-demand job and run briefs. All job artifacts
remain on the local filesystem, with SQLite holding the queue and records.

**Research preview — single-owner local Jobs pilot.** Runner supports durable local
schedules, stable JSON file changes, authenticated events, Google Cloud Scheduler
delivery and unattended Gateway operation mappings. AWS/Azure adapters, external
actions, graph execution, shared-user permissions and AI agent loops are not implemented. Closing a browser does not stop work;
Desk and its runner companion must stay running on the host.

## Repository responsibilities

| Repository | Responsibility |
| --- | --- |
| [Specification](https://github.com/Judgment-Pack/judgment-pack-spec) | Pack format and JPS semantics. |
| [Runtime](https://github.com/Judgment-Pack/judgment-pack-runtime) | Pack validation, test evaluation, decisions and audit records. |
| **Runner** | Frozen releases, schedules, file observation, authenticated occurrences, durable queue, execution, input snapshots, history and saved job/run briefs. |
| [Desk](https://github.com/Judgment-Pack/judgment-pack-desk) | Jobs UI, authenticated proxy, source selection and explicit AI brief generation. |
| [Gateway](https://github.com/Judgment-Pack/judgment-pack-gateway) | Optional connected-source acquisition, such as a selected Google Drive JSON file, through Desk. |

Runner contacts installation-authorized Gateway operation endpoints and Google
Pub/Sub using explicit local ADC credentials. Manual inputs and local JSON uploads
need no Gateway. Google Drive supplies input bytes through Desk;
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
`workspace`, `owner`, and a random `token`, with optional trusted `inputProfiles` for v2 and an absolute `inputRoot` for file automation. It binds an ephemeral IPv4 loopback port,
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
5. Use **Run job** for a new operational input, or configure **Triggers**. Automatic triggers are saved paused and require separate review before enabling.

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


### Job and run lists

`GET /v1/jobs?q=...` searches job names. Jobs include `packTitle`, `packVersion`
and up to five `recentRuns` (ID, execution state, creation time).
`GET /v1/runs` lists all jobs' runs; `GET /v1/jobs/{job}/runs` remains scoped to
one job. Run lists accept `q` (run ID or job name), `state` and `review=true`.
Search uses literal substrings with SQLite ASCII case folding. Filters apply
before the existing 50-record cursor pagination; combine them with `after`.

Attention includes failed or interrupted execution, unresolved decisions and
requested handoffs. Rejection alone is not an execution failure. Summaries include
`jobName`, disposition and handoff but omit input snapshots, audit bodies and
other result details. Use the individual run endpoint to inspect retained inputs.
These are additive list APIs; stored releases, jobs and runs do not change.

## Local schedules and events

Choose a trigger during job creation, or open **Job → Triggers → Add trigger**.
Creating a job with its first trigger is atomic. Saving or editing a trigger always
pauses it; **Preview trigger** reads configured inputs explicitly, and **Enable**
requires review. The UI shows the next three schedule occurrences with UTC offsets.
The installation's Runner remains the sole scheduling authority.

| Trigger | Behavior |
| --- | --- |
| Interval | Elapsed time, from 1 minute to 30 days, anchored to `startAt`. |
| Daily / weekly | Local wall-clock time in an IANA zone. Missing DST times are skipped; repeated times run once, at the first occurrence. |
| One time | One future RFC3339 timestamp. |
| File changes | A project-relative JSON file; changed content must remain stable for 1–3600 seconds. Enabling uses existing content as the baseline. A missing file may arrive later. |
| Authenticated event | A caller supplies a unique event ID, timestamp, and complete fresh input. |

Schedules have an optional exclusive end timestamp. Enabling begins with future
occurrences; paused time is not backfilled. If an active schedule is over 30 seconds
late (including restart or host sleep), **Skip missed runs** records one skipped
range, while **Run latest once** admits only its latest occurrence. Neither option
creates an unbounded catch-up burst. This is a best-effort local scheduler, not a
real-time timer. The computer must be awake and Desk running.

Overlap defaults to **Skip this occurrence**; **Queue this occurrence** uses a
bounded queue. Up to 100 occurrences can await admission, alongside the existing
bounded run queue. Queue expiry is explicit, from 1 minute to 24 hours, and is
checked again before evaluation. Pausing does not cancel previously accepted work.
Execution still uses the single durable worker. No automatic evaluation retry is
introduced. A previously running invocation becomes interrupted after a crash;
its audit may already exist and it is never replayed automatically.

An occurrence keeps the trigger/revision, frozen job/release, event or scheduled
identity, received time, input digest, admission outcome, and linked run. Cursor
advancement and occurrence storage are transactional. Run admission uses the
occurrence ID as its idempotency key. After a crash between admission and linkage,
Runner reconciles the existing run rather than evaluating twice. This is durable
local deduplication, not an end-to-end exactly-once claim about external systems.

### Inputs and file authority

Automatic runs never reuse a release sample. Choose explicit constant inputs, an
input-envelope file (`{"facts":{...},"evidence":{...}}`), or fresh project files for
each local source in the fixed v1/v2 mapping. V2 case parameters can be constants
or a separate case file. Files must be regular JSON files within the installation's
`inputRoot`, up to 200 KB each and 2 MiB total. `os.Root` prevents symlink/path escape.
The watched file must participate in the input. Its retained bytes match the
observed change digest. Files are read independently; multiple files are not an
atomic filesystem snapshot. An atomic rename is recommended when publishing inputs.

Desk supplies its authorized project root at boot. Runner binds this root to its
store identity so a restart cannot silently redirect existing triggers to a different
project. Missing files, invalid JSON, and unsupported numeric values cause visible
input failures; they never fall back to samples or previous values. Local values
remain asserted inputs, not authenticated external records. Manual and v1 exact
JSON values remain exact; mapping v2 retains its stricter numeric domain.

Interactive Google Drive grants and retained operation responses cannot power
unattended schedules. Such mappings may use manual runs or authenticated event
inputs acquired by an authorized sender. Persistent picker file grants remain unsupported. Installed Gateway operation
connections support fresh scheduled acquisition, as described below. Receipt freshness is checked at event
acceptance and run admission under the existing mapping contract; retained evaluation
recomputes verification at that recorded time. Queue expiry bounds the delay.

### Event delivery

Enable an event trigger to receive a scoped random token, shown once. Runner stores
only its SHA-256 digest. **Rotate token** invalidates the previous credential
immediately. Tokens cannot list jobs, inspect records, edit triggers, or choose
another release. Desk exposes a dedicated non-browser endpoint:

```http
POST /api/job-events/trg_<id>
Authorization: Bearer <trigger-token>
Content-Type: application/json

{"id":"source-delivery-123","occurredAt":"2026-09-26T13:00:00Z","input":{"facts":{}}}
```

Use the event's current RFC3339 timestamp: new deliveries may be at most five
minutes old or 30 seconds in the future. The same ID and canonical payload return
the original occurrence, even if paused or aged since acceptance. A different
payload with the same ID returns 409. Rotation revokes old credentials even for
retries. New deliveries to a paused trigger return 409 without acceptance.
A 202 acknowledges the occurrence, not a completed decision: inspect its state,
including `skipped` for overlap or queue limits. No input is exposed in the receipt.

Direct Runner callers need the private owner bearer plus `X-Trigger-Token`.
Desk's scoped route refuses browser Origin headers and does not forward arbitrary
paths. It does not provide public ingress, cloud credentials, or a tunnel. Google Cloud uses an authenticated relay and an outbound pull subscription.
Future AWS/Azure adapters can reuse the same occurrence admission boundary.

There are at most 128 configured triggers per local store. Trigger revisions and
occurrences are retained in `runs.sqlite`; backups must include the entire stopped
Runner state directory, including releases, runtimes and attempts. Desk chat backups
still exclude Jobs. Automatic retention, archive/delete controls, managed services
and AWS/Azure adapters remain future work.


### Google Cloud Scheduler

[Setup and deployment](deploy/google-cloud/README.md) provides an authenticated
relay, a paused Terraform deployment and the local ADC configuration. Runner pulls
only occurrence signals over outbound HTTPS. Timing stays in Google Cloud;
execution, input facts, source responses, results and audit files remain local.
The relay preserves Scheduler's original job name and scheduled time across retries.

Trusted stdin boot accepts `cloudConnections` (up to 8): `id`, full `subscription`,
and absolute `credentialsFile`. The boot envelope is bounded to 128 KiB. These
are installation settings, not browser or release fields. The public
`GET /v1/background-connections` endpoint exposes IDs, subscription names and
bounded diagnostics, never credential paths or contents.

A `cloud` trigger pins `{connection, subscription, job}` and automatic inputs.
It is created paused and enabled after review. A committed occurrence precedes
acknowledgement. Expiry is measured from the scheduled instant. Cloud `missed: skip`
ignores signals over 30 seconds late; `missed: latest` delivers **each** unexpired
signal, unlike local schedule catch-up. A paused trigger records new signals as
skipped. Unknown or malformed signals are discarded with a connection diagnostic.
One dedicated subscription must belong to only one local installation.

### Persistent Gateway operation inputs

Trusted boot `gatewayConnections` (up to 32) binds an installed input `profile`
to a Gateway origin `url`: HTTPS or explicit loopback HTTP. Addresses are installation
authority; the browser receives profile IDs only. Gateway keeps provider credentials.

Use `input.kind: mapped-sources` for v2 operation mappings on schedule or cloud
triggers. `files` names local-file sources; `case` or a relative `path` supplies
fresh typed parameters. The sequential planner preflights all source profiles and
permissions, calls each operation once, verifies its receipt and exact arguments,
and derives facts/evidence through the existing mapping contract. Never reuse a
release sample or retained response as a scheduled input.

For legacy connections (without `durable: true`), an occurrence is durably marked
`preparing` before network acquisition. A crash
in that state becomes a visible failure on restart and is **not** retried.
Acquisition has a 120-second overall budget, bounded by occurrence expiry, plus
existing source and combined input limits. Preview makes real provider calls.
Enabling a trigger authorizes future operation calls, including provider charges.
Interactive Drive grants, background picker file access, HTTP-shaped v2 profiles
and connected file-change triggers remain unsupported.

Example additional trusted boot fields:

```json
{
  "gatewayConnections": [{"profile":"vendor-registry","url":"https://gateway.example.com"}],
  "cloudConnections": [{"id":"google-production","subscription":"projects/example-project/subscriptions/desk-schedule-desk","credentialsFile":"/absolute/path/to/adc.json"}]
}
```

These additions retain protocol `jobs/1`. See [OpenAPI](openapi.json) and the
[design note](docs/design/google-cloud-triggers.md). The Google template is prepared
and locally validated; live cloud IAM and account delivery need deployment testing.

### Durable source waiting

Durable acquisition is owned by **Runner's caller-side source worker**, not by
Gateway's signer. Build `./cmd/jpack-source-worker` and run it independently of
Desk/Runner, using a persistent private directory outside the Gateway store:

```sh
jpack-source-worker --store /absolute/private/source-worker --gateway http://127.0.0.1:8787 --port 8792
```

The worker creates `source-worker.token` with owner-only permissions. Configure
an installed Gateway connection with its original Gateway URL, the separate
worker URL and token file:

```json
{"gatewayConnections":[{"profile":"vendor-registry","url":"http://127.0.0.1:8787","durable":true,"operationsUrl":"http://127.0.0.1:8792","operationsTokenFile":"/absolute/private/source-worker/source-worker.token"}]}
```

Desk uses the same entries under `gateway` in its installation-owned
`--runner-connections` file. Matching input trust profiles are still required.
Never put token bytes in browser settings, project files or URLs. An authenticated
Gateway can be configured on the worker with `--gateway-token-file`.

The worker calls the normal Gateway `/acquire` API once, and retains the returned
proof in **caller custody**. Gateway never retains commitment salts or replayable
requests. The worker requires its private bearer credential and refuses browser
Origins. Its database is bound to one Gateway origin and one OS process owner.
The worker supports Linux/macOS/FreeBSD process locks, as Runner does.

Schedule and Google Cloud triggers checkpoint case/local-file inputs and their
immutable operations. Runner can restart while the worker continues acquiring;
it resumes the same operation IDs. Completed worker responses survive its own
restart. If the worker or Gateway stops before a response is retained, the call
needs attention and is **never replayed automatically**. Four acquisitions per
worker and bounded three-second Runner polling keep independent jobs moving.

`preparationSeconds` is the overall source wait (60–604800 seconds; default 3600).
`queueSeconds` independently bounds admission and the subsequent evaluation queue.
Gateway and adapter timeouts still apply; configure both for long calls. The
Gateway timeout defaults to 30 seconds. The updated Gateway build permits up to
seven days, while earlier builds retain their smaller configurable ceiling.
Mapping freshness and signed argument commitments are checked before evaluation.

Desk shows **Waiting for sources** in Runs and keeps preparation history in
Triggers. Cancellation fences late results locally; it does not promise to stop
a remote provider. **Check status** retains the operation ID and original deadline.
Configuration preview does not claim that live inputs have already been verified.

Keep the worker running as a separate local service. Its private SQLite store
contains requests and proof secrets and needs a separate backup; never distribute
it with the Gateway receipt corpus. Capacity is 10,000 retained operations; no
automatic pruning is provided. Do not delete records whose IDs can still be
retried. Native provider handles/callbacks, automatic refresh and browser-initiated
acquisition recovery remain outside this implementation. Runtime behavior is
unchanged. See [the recovery contract](docs/design/durable-sources.md).
