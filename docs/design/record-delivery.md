# Immutable record delivery

Desk owns collection eligibility, reviewed recipe conversion, source acquisition
and the immutable record outbox. Runner owns admission, occurrence queues,
execution and retained results. Neither Runtime semantics nor Gateway signing
custody changes.

The local owner control API offers `record-delivery` in capabilities:

- `POST /v1/triggers/{trigger}/deliveries` (`deliverRecordRevision`):
  `{releaseId,event:{id,occurredAt,input}}`.
- `GET /v1/triggers/{trigger}/deliveries/{event}` (`readRecordDelivery`):
  reconcile by stable identity.

A reviewed job starts with a paused event trigger and `overlap: queue`. Desk saves
the exact input envelope before first delivery. The identity hashes collection,
record, record digest, recipe version and release identity. Retries preserve the
entire envelope, including timestamp and receipts. An identical delivery returns
the original occurrence after restart or pause. A changed payload conflicts.
A new record revision uses a new ID. A changed release requires a reviewed job.

The owner route allows old occurrence timestamps because a durable outbox may
wait through downtime. It still rejects future timestamps and checks the fixed
release, mapping, source proofs/freshness and the queue limit. Public token
event delivery retains its five-minute freshness window. The admission journal
attributes owner delivery to the installation, not an unused event credential.

If an unacknowledged snapshot loses eligibility, Desk first reads its stable
identity. A retained occurrence is reconciled; a 404 means it was never accepted
and can be marked superseded without admitting stale facts. Accepted work may
finish after pause. Queue expiry or preparation failure is an observable terminal
result, never permission to repeat uncertain provider acquisition.

## The delivery contract (#70)

Inputs collections need incremental evaluation of eligible immutable record
revisions against an approved frozen release. #70 asked whether Runner's event
admission and occurrence processing already hold that contract, and for an API
extension only where they do not. Each clause below names what holds it. Every
clause but the queue's was held already; the queue's needed one change, the
refusal under clause 5.

1. **Record and revision identity.** `event.id`, 1 to 128 bytes, is the
   caller's identity of one record revision under one release. Runner keys an
   occurrence by its trigger and that ID (`occurrences` is unique on trigger and
   identity), so one ID names at most one occurrence of a trigger, whichever
   route delivered it. The answer's `occurrence.eventId` repeats it, and
   `readRecordDelivery` reads by it; an ID holding a reserved character is
   percent-encoded in the path.
2. **A fixed release.** `releaseId` is required (`422 release_required`) and must
   be the job's release when the delivery is admitted (`409 release_changed`).
   It is part of the delivery's payload: the same `event.id` with another
   `releaseId` is another payload (clause 4). The occurrence records
   `releaseId` and `jobRevision`, and its run is made under them or not at all:
   a job whose release changed before the run was made fails the occurrence
   (`failed`). A job upgrade waits for every accepted occurrence to settle
   ([job revisions](job-revisions.md)).
3. **The same delivery, the same occurrence.** The same envelope, compared in
   canonical form, answers `200` with `replayed: true` and the retained
   occurrence: before its run is made and after, once evaluated, after a
   restart, while the trigger is paused, after the job is upgraded. A replay
   writes nothing, no occurrence, run or journal entry. Deliveries of one
   envelope at the same moment are admitted one at a time: one answer is `202`
   with `replayed: false`, every other its replay.
4. **Another payload under the same identity.** The same `event.id` with any
   other payload, whether facts, evidence, source responses, `occurredAt` or
   `releaseId`, answers `409 event_conflict` with `retryable: false`, through
   this route or a credential's `deliverEvent`; the retained occurrence is
   unchanged. Refusals alike within a minute are journaled once, as
   `admission.refused`.
5. **Queue, overlap and recovery.** An admitted occurrence is a row of the
   store, committed with its `occurrence.received` entry before the answer. It
   is made a run once, keyed by the occurrence's ID, before or after a restart.
   A process that stopped after making the run and before recording it on the
   occurrence leaves the occurrence `accepted`; after a restart the run is found
   by that key, and the occurrence is neither expired nor run again. An
   evaluation in progress at a stop is `interrupted` and never repeated. With
   `overlap: queue` each revision has its own run, one after another. With
   `overlap: skip`, a revision delivered while the job has work is answered
   `skipped`, reason `overlap`, which is that identity's answer from then on: a
   record subscription's trigger queues. An occurrence not made a run within
   `queueSeconds` of its admission is `expired`, reason `queue-expired`, also
   for good, and a run not started by then fails with that reason.

   While the store holds 100 pending occurrences (`accepted`, `preparing` or
   `waiting`), a record delivery is refused, `429 queue_full` with
   `retryable: true`, and nothing is admitted; the refusal is journaled. This
   is the change #70 needed. Runner admitted such a delivery `skipped`, reason
   `queue-full`, as it still admits a credential's delivery to `deliverEvent`.
   An event ID that is a record revision's would then answer that skip for
   good, and the revision could never run: an include-existing backfill of more
   records than the queue holds would lose every revision past it.
6. **Reconciliation without a second run.** A caller keeps `occurrence.id` from
   a `202`. A delivery left without an answer, its connection lost or its
   process stopped, is read by `readRecordDelivery`: `200` with the occurrence
   means it was admitted; `404 not_found` means it never was. Either way,
   delivering the same bytes again is safe, and answers the retained occurrence
   if there is one. The occurrence's `state` and `runId` lead to
   `GET /v1/runs/{run}`, whose `trigger.eventId` names the delivery. Neither a
   read nor a replay makes a run. The answer never carries the input.
7. **Existing runs keep theirs.** A run holds its own `releaseId`, `revision`,
   `input`, `result` and `trigger` (`occurrenceId`, `eventId`, `inputDigest`). A
   job upgrade (`POST /v1/jobs/{job}/upgrade`) changes none of them, nor the
   retained occurrence, whose delivery still replays under its first release.
8. **Inputs frozen at admission.** The input bytes are taken once, from the
   delivery: `admitBoundEvent` (`internal/runner/triggers_store.go`) writes
   `event.input` into the occurrence, with its digest as `inputDigest`, in the
   admission's transaction. `dispatchOccurrence` (`triggers_dispatch.go`) reads
   that row and gives its input to `submitInternal` (`store.go`), which writes
   the run, its `input` and `trigger.inputDigest`, in the run's own
   transaction; the occurrence then drops its copy. `dispatchNext` (`store.go`)
   evaluates the run row's input and writes its facts to the attempt's
   `facts.json` for the Runtime. Runner holds no other copy of a record and
   reads no source on this path, so there is no later record to read: a record
   changed while its revision waits is a new delivery. For a mapped input, the
   run holds the preparation made from those bytes when the run is made.
9. **Receipts, sources and generated inputs.** `event.input` is held at
   admission to the release's reviewed mapping (`422 mapping_mismatch`), each
   acquisition to the one receipt rule of the dialect companion's §5.2, which
   checks its signature, its result and arguments against what it commits, and
   its whole-second instants within the source's `maxAge`, and each target to
   the mapping's admission of its source's class: a generated source reaches
   only a target that admits `generated`, and the run's lineage says so. A
   refused acquisition or a missing response is `422 input_preparation_failed`;
   facts or evidence beside a source must be the verified derivation
   (`422 invalid_source_input`); a caller's `preparation` is refused
   (`422 invalid_event`). An old `occurredAt` does not excuse a receipt from
   its `maxAge`. The same checks run again when the run is made and when its
   evaluation starts, so an acquisition that aged while queued fails the
   occurrence or the run. An event trigger takes no automatic input: nothing
   on this path asks a provider, so a missing, refused or aged response is a
   refusal or a failed run and never an acquisition, and a replay sends
   nothing anywhere.

## How a caller uses it

Runner sees deliveries, not collections. What decides a delivery is the caller's:

- **Eligibility is the caller's.** Runner does not choose which records are
  eligible or when a revision is new. The caller delivers each eligible
  revision once, under an ID that is a function of the collection, the record,
  the revision's digest, the recipe version and the release, so that polling,
  a reload and a lost answer all deliver the same ID with the same bytes. A
  revision that lost eligibility before an answer is read first: `404` lets the
  caller withdraw it without delivering it.
- **The reviewed mapping is the release's.** `event.input.source.mapping` has
  the digest of the mapping the release was previewed and reviewed with, and an
  input to a release without one has no source. A changed recipe, mapping or
  pack is a new release, previewed, checked and reviewed, then a job upgrade
  with the trigger paused, or a new job. The
  trigger is enabled again after review (`POST /v1/triggers/{trigger}/state`),
  and revisions are delivered under the new `releaseId` with new IDs.
- **Including existing records is the caller's choice.** Runner does not tell an
  existing record from a new one. With existing records included, the outbox
  holds a delivery for each eligible record when the subscription is enabled,
  and delivers them as it delivers any revision; an old `occurredAt` is
  accepted. The outbox paces itself by the queue: a `429 queue_full` is
  delivered again later with the same bytes.
- **The outbox persists, then delivers.** It saves each envelope's exact bytes
  before the first delivery and answers each outcome:
  - `202`, or `200` with `replayed: true`: admitted; keep `occurrence.id` and
    read `state` and `runId` until the run ends.
  - No answer: read `readRecordDelivery`; deliver the same bytes after a `404`.
  - `429 queue_full` or `503 runner_unavailable`: deliver the same bytes later.
  - `409 trigger_paused`: hold the entry until the trigger is enabled again.
  - `409 release_changed`: the job moved to another release; decide eligibility
    under it again, with new IDs.
  - `409 event_conflict`: the outbox changed or lost the bytes it persisted. It
    is a defect, not a retry; another payload is another ID.
  - Any other `422`: these bytes are refused for good, for the code named; a
    new acquisition is a new revision with its own ID.

## Validation

`TestRecordDeliveryFrozenRevisionAndRestart`,
`TestRecordDeliveryHTTPPreservesVersion3AndJournalsRefusals` and, for #70,
`record_delivery_contract_test.go`: a repeated delivery, concurrent
deliveries, a changed revision, frozen inputs, a changed release, restart
recovery, a run made before a stop, a changed payload, the full queue, an
acknowledged and an uncertain delivery over loopback, and a delivery carrying
an acquisition. The tests that run need `JPACK_TEST_BIN` set, as do the existing
event concurrency, token, expiry and initial-trigger tests.
