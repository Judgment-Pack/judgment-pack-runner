# A journal of job activity

Status: accepted on 2026-10-05, with the maintainer's answers recorded in
section 10. Part of issue #41: this record first, then the build it plans
(section 8). Nothing here is built yet. Line references are to `main` at
`db067e5`.

Runner keeps each job's records as their latest state. This record decides a
journal beside them: one entry for each change of state Runner makes, written in
the transaction that makes the change, and served in order with a cursor, so
that Desk can show what happened to a job without inventing it. Everything
Runner records today stays as it is. The journal is additive, and it is the
operator's own log: not chained, not signed, binding nothing against the
operator (section 6).

## 1. Why: what Runner keeps today

Runner's records say where a job is, not how it got there. In the review that
opened #41 (Runner v0.5.0, `0ba6bcd`, whose code `main` holds unchanged):

- **Runs, occurrences and triggers are updated in place.** A run's state and
  record are rewritten by `saveRun` (`internal/runner/store.go:209-212`) and
  `finishRun` (`run_chain.go:184`), an occurrence's by `saveOccurrence`
  (`triggers_store.go:390-393`), and a trigger's by `saveTrigger` (`:34-43`).
- **Occurrence transitions keep no time.** An occurrence holds when it was
  received, when it was scheduled and when its queue time ends, and a
  preparation when it and each source task started and completed
  (`triggers_types.go:54-66`, `preparation.go:18-31`). Submitted, failed,
  expired, cancelled, needs attention and reconciled have no time
  (`triggers_dispatch.go:239-298`, `preparation.go:209-445`). A reconcile clears
  the reason the occurrence needed attention (`preparation.go:442-445`), and a
  cancellation replaces the state and reason it had (`:403-413`).
- **Trigger revisions are kept and never served.** Each configuration, pause or
  resume, and key rotation writes a full snapshot to `trigger_revisions`
  (`triggers_store.go:39-41`, called at `:123`, `:202` and `:226`, and at job
  creation at `store.go:302`). No route reads them: the trigger listing serves
  each trigger's current row (`triggers_http.go:28-67`). Measured: a trigger
  with four revisions served only the fourth. A snapshot names no one, and the
  trigger's `updatedAt` is overwritten by the next change.
- **A refused admission writes nothing.** A request to start work that is
  refused with 409, 422 or 429 leaves no trace in the store.
- **An interrupted run gets the restart's time.** A run that Runner finds
  running when it starts again is marked `interrupted` with `finishedAt` set to
  the time of the restart (`store.go:177-184`), not the time it stopped.
- **The requester is the installation or a trigger, never a person.** A run's
  `requestedBy` is the boot line's owner or `trigger:<id>`
  (`store.go:315-318`). Nothing records a review: `reviewed` is a flag a request
  must carry to enable a trigger (`triggers_store.go:138-140`), and it is not
  stored.

Three stores are append-only: `run_chain`, one entry per completed run
(`run_chain.go:78`); `trigger_revisions`, which is not served; and brief
revisions (`brief_state.go:156`). None tells a job's story. A view that derived
events from the differences it observed between two reads would not be a
record: it would miss whatever changed twice between reads, and date each change
by the read.

## 2. The entries

### Members every entry has

```json
{
  "sequence": 118,
  "entryVersion": "1",
  "kind": "occurrence.needs-attention",
  "at": "2026-10-06T09:14:03.218734Z",
  "by": {"kind": "runner"},
  "concerns": {
    "job": "job_…",
    "release": "release_…",
    "trigger": "trg_…",
    "occurrence": "occ_…"
  },
  "revision": 3,
  "from": "waiting",
  "to": "needs-attention",
  "reason": "A required source is unavailable."
}
```

- `sequence`: the entry's place in the journal, from 1, and the cursor
  (section 4).
- `entryVersion`: `"1"`, a string, as the chain of runs writes its own.
- `kind`: one of the kinds below.
- `at`: when Runner wrote the entry, by its own clock, in the form of every
  time Runner records (`model.go:109`). Order is `sequence`, never `at`.
- `by`: who Runner can say initiated the change (below).
- `concerns`: the ids the change is about, each present only where it applies:
  `job`, `release`, `trigger`, `occurrence`, `run`. An entry that names a job
  also names its release; a release has at most one job (`store.go:124`).
- `revision`: the trigger revision, where an entry concerns a trigger: after
  the change, for a change of the trigger; the one an occurrence was admitted
  under (`triggers_types.go:60`), for the occurrence and its run.
- `from` and `to`: the record's state before and after, for triggers (`paused`
  or `enabled`), occurrences and runs. `from` is null when the change created
  the record.
- `reason`: the reason or problem text Runner recorded with the change, where
  the record serves one, cut at 1,024 bytes.

Members particular to a kind are listed with it.

**What no entry holds.** Nothing a list does not already serve. Run lists leave
out a run's input, its audit record, the record's bytes and its signatures
(`list.go:77-89`); occurrences are served without their input, pending input or
the private bindings of their source tasks (`preparation.go:38-54`). No entry
holds any of these, nor a fact or evidence value, a case, a request body, an
idempotency key, an event payload, a Gateway or worker address, a profile
binding, a trigger key, a key's digest, the bearer or the boot line. A reason
is copied only from a record that serves it.

### Who an entry names

`by` names what Runner can know initiated a change, and nothing more.

- `{"kind": "installation", "owner": "<owner>"}`: a request on the control
  listener. Every such request carries the owner bearer (`http.go:224`), which
  Runner receives once, on its boot line, and which Desk holds
  (`cmd/jpack-runner/main.go:47`, `:55`, `:69`). `owner` is the boot line's
  owner, which the store keeps and refuses to change (`store.go:136-147`). It
  names the installation, as `requestedBy` does (`store.go:315`). Runner cannot
  tell Desk acting for a person from a script holding the same bearer.
- `{"kind": "trigger", "trigger": "trg_…", "revision": 3}`: Runner acting for
  a trigger, at the revision the trigger had: a schedule falling due, a watched
  file changing, a cloud signal arriving.
- `{"kind": "trigger-credential", "trigger": "trg_…", "keyRevision": 2}`: an
  event delivery whose `X-Trigger-Token` matched the trigger's current key
  (`triggers_store.go:236`). `keyRevision` is the trigger revision at which
  that key was issued: the token's identity, never the token or its digest.
  Runner does not keep it today. The migration adds it (section 8), and a key
  issued before the migration has `keyRevision: null`.
- `{"kind": "cloud-connection", "connection": "<id>"}`: a cloud signal that
  Runner discarded (`cloud.go:99-134`).
- `{"kind": "runner"}`: Runner itself: its dispatcher, its automation, its
  start and its stop.

**A person, never, today.** No request names one, and one bearer authorises
every request. A person can be recorded only once a request carries an identity
that Runner has a reason to believe. Until then `by` has no kind for a person,
and Desk must not present `installation` as one (question 2).

A request without the owner bearer, or from a browser origin, is refused
before any route (`http.go:222-235`) and is not journaled: it can come from any
local process.

### The kinds

In the `By` column, `credential` is `trigger-credential` and `cloud` is
`cloud-connection`. Pending is accepted, preparing or waiting. A dash: the kind
concerns no record with a state.

| Kind | From | To | By |
|---|---|---|---|
| `trigger.configured` | none, paused, enabled | paused | installation |
| `trigger.paused` | enabled | paused | installation |
| `trigger.resumed` | paused | enabled | installation |
| `trigger.key-rotated` | its state | its state | installation |
| `occurrence.received` | none | accepted | trigger, credential |
| `occurrence.skipped` | none | skipped | trigger, credential |
| `occurrence.preparing` | accepted | waiting, preparing | runner |
| `occurrence.ready` | waiting, preparing | accepted | runner |
| `occurrence.submitted` | accepted | submitted | runner |
| `occurrence.failed` | none, accepted, preparing | failed | trigger, runner |
| `occurrence.expired` | none, pending | expired | trigger, runner |
| `occurrence.needs-attention` | waiting | needs-attention | runner |
| `occurrence.cancelled` | waiting, needs-attention | cancelled | installation |
| `occurrence.reconciled` | needs-attention | waiting | installation |
| `run.queued` | none | queued | installation, trigger |
| `run.started` | queued | running | runner |
| `run.expired` | queued | failed | runner |
| `run.completed` | running | completed | runner |
| `run.failed` | running | failed | runner |
| `run.interrupted` | running | interrupted | runner |
| `release.previewed` | — | — | installation |
| `job.created` | — | — | installation |
| `admission.refused` | — | — | installation, credential, cloud |
| `journal.began` | — | — | runner |
| `runner.started` | — | — | runner |
| `runner.stopped` | — | — | runner |

The issue's list is here, with five more: `occurrence.preparing` and
`occurrence.ready` are changes of state Runner makes today; `journal.began` is
where the journal starts; `runner.started` is the restart's own entry the issue
asks for; and `runner.stopped` tells an orderly stop from a crash.

### Where each is written

Each kind is written by the function that makes the change, in its transaction.
Where that function is one bare statement today, it becomes a transaction that
holds the change and its entry (section 4).

**Triggers.** Every trigger entry is a new revision, and carries
`previousRevision`.

- `trigger.configured`: `configureTrigger` (`triggers_store.go:113-126`), and
  `createJobConfigured` for a trigger made with its job (`store.go:286-305`).
  Both are one transaction today. `previousRevision` is null for a new trigger.
  Member: `triggerKind` (`schedule`, `event`, `file` or `cloud`).
- `trigger.paused`, `trigger.resumed`: `setTriggerState`
  (`triggers_store.go:128-204`), through `writeTrigger`'s transaction
  (`:44-54`). A request that changes nothing writes nothing (`:141-143`). A
  resume that issues an event trigger its first key (`:184-193`) carries
  `keyIssued: true`, and its revision becomes the key's `keyRevision`. A resume
  of a schedule carries `nextAt`.
- `trigger.key-rotated`: `rotateTriggerKey` (`:205-227`), through
  `writeTrigger`. Its revision becomes the new key's `keyRevision`.

**Occurrences.**

- At admission, one entry for the occurrence's first state:
  `occurrence.received`; `occurrence.skipped` for overlap or a full queue
  (`triggers_store.go:375-381`), a missed schedule
  (`triggers_dispatch.go:123-126`), or a paused or missed cloud trigger
  (`cloud.go:149-157`); `occurrence.failed` when automatic input could not be
  read (`triggers_dispatch.go:136-139`, `:214-216`); `occurrence.expired` for a
  cloud signal already past its queue time (`cloud.go:152-154`). Written by
  `admitOccurrence` (`triggers_store.go:366-389`), in its caller's transaction:
  event delivery (`:293-306`), a schedule falling due
  (`triggers_dispatch.go:151-162`), a file change (`:226-237`), a cloud signal
  (`cloud.go:161-170`). Members: `occurrenceKind`, and the `scheduledAt`,
  `missedFrom`, `missedThrough` and `expiresAt` the occurrence holds.
- `occurrence.preparing`: `prepareOccurrence` (`preparation.go:178-186`) for a
  durable preparation, with its `deadline`; `prepareLegacyOccurrence`
  (`background.go:245-246`) otherwise.
- `occurrence.ready`: `advancePreparation` (`preparation.go:235-247`), with
  `readyUntil`; `prepareLegacyOccurrence` (`background.go:263-266`).
- `occurrence.submitted`: `dispatchOccurrence`
  (`triggers_dispatch.go:295-298`), and its finding of a run admitted before a
  crash (`:243-249`). `concerns.run` names the run.
- `occurrence.failed` after admission: `dispatchOccurrence` (`:274-278`,
  `:288-291`), `prepareOccurrence` (`preparation.go:170-176`),
  `prepareLegacyOccurrence` (`background.go:257-259`), and the restart
  (`store.go:185`; section 4).
- `occurrence.expired` after admission: `dispatchOccurrence`
  (`triggers_dispatch.go:261-265`), `prepareOccurrence`
  (`preparation.go:161-167`), `advancePreparation` (`:222`, `:348`),
  `prepareLegacyOccurrence` (`background.go:232-238`, `:260-262`).
- `occurrence.needs-attention`: `stopPreparation` (`preparation.go:209-214`),
  for each refusal in `advancePreparation` (`:233` to `:345`). The entry keeps
  the reason that a later reconcile clears.
- `occurrence.cancelled`: `cancelOccurrence` (`:396-421`).
- `occurrence.reconciled`: `reconcileOccurrence` (`:425-446`).

`saveOccurrence` is one bare statement today (`triggers_store.go:390-393`), and
`savePreparation` reads the state and then writes (`preparation.go:197-208`).
Each becomes one transaction that holds the state check, the change and the
entry.

**Runs.**

- `run.queued`: `submitInternal` (`store.go:373-422`), one transaction today.
  `by` is the installation for a request, and the trigger for an automatic run,
  as `requestedBy` is (`:315-318`). `concerns.occurrence` names the occurrence
  an automatic run came from.
- `run.started`: the dispatcher (`store.go:474-479`).
- `run.expired`: the dispatcher, for an automatic run whose queue time ran out
  (`:462-471`). Its state is `failed`, as today; the kind tells an expiry from a
  failure without reading the problem text.
- `run.completed`, `run.failed`, and `run.interrupted` at an orderly stop:
  `finishRun` (`run_chain.go:178-193`), one transaction today, which for a
  completed run also appends its entry to the chain of runs. `run.completed`
  carries `chainSequence`, that entry's sequence.
- `run.interrupted` at a restart: `Open` (section 4). Every `run.interrupted`
  carries `interruption` (section 4).

`saveRun` is one bare statement today (`store.go:209-212`).

**Releases and jobs.**

- `release.previewed`: `preview` (`runtime.go:254`), one bare insert today. A
  refused preview stores no release, and writes no entry. Members: `packId`,
  `packVersion`, `packDigest`, `runtimeDigest` and `tests`.
- `job.created`: `createJobConfigured` (`store.go:260-309`). A repeated creation
  returns the stored job (`:266-276`) and writes nothing. A job created with a
  trigger writes `job.created`, then `trigger.configured`.

**Refused admissions.** `admission.refused` records a request to start work that
Runner refused, in a transaction of its own: there is no change to share one.
Members: `request`, `status` and `code` (or `reason`, for a cloud signal), and
`coalescedSeconds`. The requests are:

- `submit-run`: `POST /v1/jobs/{job}/runs` (`http.go:192-213`,
  `store.go:311-422`);
- `deliver-event`: `POST /v1/triggers/{trigger}/events`
  (`triggers_http.go:171-187`, `triggers_store.go:229-309`), including a token
  refused as `invalid_trigger_token`, which is `by` the installation;
- `create-job`: `POST /v1/jobs` (`http.go:147-169`), naming a release the store
  holds;
- `cloud-signal`: a signal Runner discards (`cloud.go:99-134`), whose reason it
  reports today only as the connection's latest problem, held in memory
  (`cloud.go:49-56`).

A refusal is journaled when it is answered 400, 401, 409, 422 or 429 to a
request that names a job, trigger or release the store holds, and for every
discarded cloud signal. A 404 names nothing the store holds, and a 503 says the
store itself failed (`store.go:319-321`, `triggers_store.go:262-264`): neither
is journaled. The dispatcher's own retry after a full queue
(`triggers_dispatch.go:285-287`) refuses no caller and changes nothing, and is
not an entry. Nor is a refused change of configuration: it is answered to the
person who asked, while a refused admission is often a sender's that no one at
Desk sees.

A sender refused again and again, such as one still using a rotated token
through Desk's event endpoint, must not grow the journal without limit. Runner
writes a refusal unless the journal holds one for the same request, job,
trigger or release, and code (for a cloud signal, the same connection and
reason), written in the last 60 seconds. The check reads
the journal in the refusal's own transaction, so it holds across a restart and
keeps nothing in memory. `coalescedSeconds: 60` tells a reader that identical
refusals in the 60 seconds after the entry are not entries.

**Runner and the journal.**

- `journal.began`: the first entry of every store's journal, written by the
  migration that creates the journal (section 4). Member: `held`, the number of
  jobs, triggers, occurrences and runs the store held then.
- `runner.started`: `Open`, once the store's lock is held and its Runtime
  pinned (`store.go:96-151`), in the transaction that marks what the last
  process left running. Members: `version`, the Runner's build, and
  `runtimeDigest`.
- `runner.stopped`: `Close` (`store.go:199-207`), after the dispatcher,
  automation and background work stop and before the store closes. A process
  that is killed writes none: the next `runner.started` follows an entry that is
  not `runner.stopped`.

### What is not an entry

- Progress inside a preparation that leaves the occurrence's state as it was:
  a source task's intent, committed before the network call
  (`preparation.go:271-276`); a worker that could not be reached (`:303-309`);
  a task queued, running or completed (`:318-321`, `:350-353`); the next check
  time (`:185`). Each task keeps its own `startedAt` and `completedAt`, which
  nothing overwrites (`:29-30`).
- The dispatch side's trigger updates, written with `history=false`: the next
  schedule time, written with the occurrence it admits
  (`triggers_dispatch.go:146-162`); the file observer's private cursor
  (`:173-188`, `:207-212`, `:221-237`; `triggers_types.go:37-38`); a watched
  file's problem text (`triggers_dispatch.go:166-179`). A later kind could
  record a trigger's problem appearing and clearing, if Desk needs it.
- Reads, including a token reading the result of its occurrence
  (`triggers_store.go:317-353`), and previews of a trigger or an input.
- Briefs, which keep revisions of their own (section 7).
- The source worker, another process with its own store (`source_worker.go`).

## 3. Configuration changes, by revision

A trigger entry carries `revision`, the trigger's revision after the change,
and `previousRevision`, before it. The values are the snapshots that
`trigger_revisions` already holds for both (`triggers_store.go:15`, `:39-41`).
The entry never copies a value. A configuration can hold a constant input or a
case (`triggers_types.go:5-11`); such values belong to the snapshot, not to the
journal. A snapshot is the trigger's record, which says `hasKey` and does not
hold the key's digest: the digest is a column of `triggers`, outside the
snapshot (`triggers_store.go:14`, `:35`).

A key rotation's entry names its revision only. The new token is shown once,
in the answer to the request (`triggers_http.go:164-169`), and stored only as a
digest (`triggers_store.go:226`); neither reaches the journal. The same holds
for the first key, issued when an event trigger is first enabled.

A route serving a revision's snapshot, so that a reader can see what a
`trigger.configured` entry changed, is a step of its own and not part of this
record. The journal names the revisions such a route would serve.

## 4. Order, cursor, retries, restarts and older stores

**Order.** SQLite assigns `sequence` as the entry is inserted, in the
transaction that makes the change. The column is `INTEGER PRIMARY KEY
AUTOINCREMENT`, so no sequence is used twice, even after a row is removed.
SQLite runs one write transaction at a time, and Runner's handle has one
connection (`store.go:119`), so sequences follow the order in which changes
commit: a reader that has read through sequence n never later finds an entry
at or below n. The entries of one transaction are adjacent, in the order this
record names: a job before its trigger, a restart before what it finds. `at` is
Runner's clock, which can be set back. Order is `sequence`.

**Cursor and pages.** The cursor is a sequence. A page holds at most 50 entries
after it, oldest first; run lists also hold 50 (`list.go:55`, `:69`). Those
lists page newest first, with `after` meaning older than, and answer `next: 0`
at the end (`list.go:29`, `:55`; `triggers_http.go:201-210`). The journal
pages forward, as the chain of runs is read (`run_chain.go:215-234`): `after`
means later than, and `next` is the sequence of the page's last entry, or the
cursor given when the page is empty. A reader that has caught up asks again
with the same cursor, and never starts over.

**One entry per change, in the change's transaction.** A change and its entry
commit together or not at all. Each writer named in section 2 begins a
transaction, makes its change, inserts its entry and commits. Where a writer is
one bare statement today (`saveRun`, `saveOccurrence`, the release insert, and
the restart's update of occurrences at `store.go:185`), it becomes such a
transaction. A check that guards a change, such as `savePreparation`'s refusal
to overwrite a final state (`preparation.go:197-208`), runs inside it, so the
entry describes the change that was made.

**Idempotent retries write nothing.** A request that changes nothing records
nothing: a submission repeated with the same key and input (`store.go:332-341`,
`:379-387`), an event repeated with the same payload
(`triggers_store.go:251-258`), a cloud signal delivered again
(`cloud.go:135-137`), a job creation repeated (`store.go:266-276`), a pause of
a paused trigger or a resume of an enabled one (`triggers_store.go:141-143`). A
refused request retried is refused again, and is an entry again, within the
bound of section 2.

**Restarts.** A restart neither loses nor duplicates an entry: each was
committed with its change, and nothing is held in memory between them.

- A crash between two transactions that belong together. An automatic run is
  admitted in one transaction and its occurrence marked submitted in the next
  (`triggers_dispatch.go:281-298`). A crash between them leaves `run.queued`,
  with the occurrence still accepted. The next pass finds the run and marks the
  occurrence submitted (`:243-249`), writing `occurrence.submitted` then. Each
  change has its entry once.
- Work the stopped process left running. `Open` writes `runner.started` and, in
  the same transaction, marks each run it finds running as interrupted
  (`store.go:177-184`) and each legacy preparation it finds preparing as failed
  (`:185`), with an entry for each. Today these are separate statements.

**An interruption's own time.** When Runner saw the run stop, at an orderly
stop (`store.go:494-504`), `run.interrupted` carries:

```json
"interruption": {"seen": true, "at": "2026-10-06T09:58:51.120Z"}
```

When it found the run at a restart, it carries:

```json
"interruption": {
  "seen": false,
  "lastKnownRunning": "2026-10-06T09:58:40.771Z",
  "restart": 56
}
```

Runner cannot know when a process it was not running in stopped. It can say
that the run was running at `lastKnownRunning`, its `startedAt`, the last time
Runner recorded it alive, and had stopped by the restart, the `runner.started`
entry at sequence `restart`, which has its own time. The evaluation the run was
in is held to 30 seconds (`runtime.go:141`), so the stop came at most about
that long after `lastKnownRunning`, unless the dispatcher stalled outside the
evaluation; the entry claims only the window. An occurrence failed at a restart
carries the same member, with the time of its `occurrence.preparing` entry as
`lastKnownRunning` where the journal holds one. What the run record itself
says is question 5.

**Stores from before the journal.** The migration creates the journal and, in
the same transaction, writes `journal.began`. Nothing is back-filled: no entry
is made from a record's current state, even where the record holds a time,
since an entry says what Runner recorded when it happened. Every page answers
`journalBegan`, the time of that entry, so that a reader of a job created
before it can say there is no journal before that time. A new store's journal
begins at its first start.

**A Runner without the journal.** An earlier Runner that opened the store after
the journal began would change records and write no entries, and nothing would
show the gap. Runner refuses a store whose `schema` metadata is not its own
(`store.go:136-147`). The migration raises it from `"1"` to `"2"`, so that
every earlier Runner refuses the store from then on (question 6). Going back
then needs a copy of the store from before the migration. The chain of runs
was added without this (`store.go:133-135`): a run an earlier Runner completes
is only unchained. A journal that missed changes would claim more than it held.

## 5. The routes

```http
GET /v1/jobs/{job}/events?after=<sequence>
GET /v1/events?after=<sequence>&job=<job>
```

- `GET /v1/jobs/{job}/events` serves the entries that concern the job: those
  naming it, and those naming its release alone (its preview, a refused
  creation). A job the store does not hold is answered 404
  `not_found`, as the job's other routes answer (`http.go:185-191`).
- `GET /v1/events` serves every entry, including those that concern no job:
  Runner's own, a release never made into a job, a refusal that names nothing
  else. With `job`, it serves that job's entries and Runner's own
  (`journal.began`, `runner.started`, `runner.stopped`), which concern no job
  but explain a gap in every job's (question 3). A `job` the store does not
  hold is answered 404 `not_found`.
- `after` is optional, and 0 when absent: from the start. A value that is not
  a decimal integer from 0 is refused with 400 `invalid_cursor`, as lists
  refuse one (`list.go:240-245`).
- Both answer `application/json`, are not cached, need the owner bearer and
  refuse browser origins, as every route does (`http.go:17-22`, `:222-235`).

```json
{
  "journalBegan": "2026-10-06T08:00:00.000000Z",
  "items": [
    {"sequence": 117, "entryVersion": "1", "kind": "occurrence.received"},
    {"sequence": 118, "entryVersion": "1", "kind": "occurrence.needs-attention"}
  ],
  "next": 118,
  "more": false
}
```

Each item is a whole entry, shortened here. `more` is true when another page is
already available.

**The OpenAPI document** (`openapi.json`) gains:

- the two paths, with `after` (an integer, at least 0) and, on `/events`, `job`;
- `JournalEntry`: the common members; `kind` as an enum of the kinds of
  section 2; `by` as a `oneOf` of the identity forms; the members particular to
  a kind, each optional, with its kind named in its description. A member added
  later raises `entryVersion`, as a verification export's version is raised for
  a member its strict readers would refuse;
- `JournalPage`: `journalBegan`, `items` (at most 50 entries), `next` and
  `more`, all required;
- `activity-journal` among the capabilities `GET /v1/status` lists
  (`http.go:106`), so that Desk can tell a Runner that serves the journal from
  one that does not.

`docs/MAPPING-V2.md` and the README describe the routes, the README where it
lists what Desk passes through.

## 6. What the journal establishes, and what it does not

It is the operator's own log, kept beside the records it describes.

- **It establishes**, of a store that nothing but Runner has changed: which
  changes Runner made, in the order it made them, when its clock said, at whose
  request as far as Runner can tell, and which requests to start work it
  refused. After `journalBegan`, each change has its entry and each entry its
  change, because they commit together.
- **Nothing against the operator.** The journal is a table in the store the
  operator holds. Rows can be rewritten, removed or inserted, the sequence
  renumbered (SQLite keeps its counter in a table of its own), and the store
  restored from an older copy, with nothing in the journal to show it. It is
  not chained and not signed, and no one else holds a checkpoint of it.
- **Not who a person was.** `installation` is whoever holds the bearer.
- **Not when.** `at` is the operator's clock, as a record's `at` is in Desk
  ADR-0010, section 7.
- **Not what happened before `journalBegan`.** An earlier Runner cannot change
  the store after it, since it refuses the store (question 6), but the operator
  can, by other means (above).
- **Not how often a sender was refused.** Identical refusals within 60 seconds
  of an entry are not entries.
- **Not what a decision was, or that it was right.** A run's record, its exact
  bytes, its entry in the chain of runs and its signature establish what
  `docs/MAPPING-V2.md` says they do; the journal adds nothing to them.

**Chaining it later.** The journal could be chained as the runs are (trail,
sequence, and previous over the exact bytes of the line before) and its
checkpoints handed to a holder, so that rewriting it fails against a
checkpoint held independently. That would be a second chain beside the chain
of runs, and would touch:

- Runner's entry form, which reads only `kind: "run"` (`run_chain.go:63`,
  `:281`), and what a Runtime's `audit verify --trail` reads;
- Desk ADR-0010's Context, whose account of Runner names one chain, the chain
  of runs;
- its section 5, which passes `GET /v1/run-chain` through and hands the Jobs
  chain over with a cursor: a second chain needs its own route, cursor and
  hand-over;
- its section 7, whose table would need a row for the journal, saying what its
  chain establishes and that, without a held checkpoint, it establishes nothing
  against the operator;
- its section 8, whose `audit checkpoint --trail <file> --since N` would run
  once more, on the journal.

This record does not propose it (question 1).

## 7. How it relates to what exists

- **The chain of runs** (`run_chain.go`; `docs/MAPPING-V2.md`, "The
  installation's chain of runs") is the only append-only store a verifier
  reads, and it covers completed runs only. The journal does not replace it, and
  `verify-run` does not read the journal. `run.completed` is written in the
  transaction that appends the run's chain entry, and names its sequence.
  Failed, interrupted and expired runs have journal entries and, as now, no
  chain entry.
- **Trigger revisions** keep the values, and the journal names the revisions
  (section 3).
- **Brief revisions** are the precedent for keeping history with a time: a
  subject's revisions are numbered from 1 and appended, each with its time and
  model, up to 100 (`brief_state.go:124-128`, `:156`). They are kept as one
  document per subject, rewritten on each save (`briefs.go:106`), and name no
  actor. The journal is a row per entry, never rewritten, with no limit
  (question 4). Briefs stay out of it: a brief is a derived document, never
  evaluation or review authority (`brief_state.go:3`).
- **Desk's Activity tab** (desk #213) is built from what Runner serves today.
  It labels an interrupted run's `finishedAt` as the time Runner recorded the
  interruption, which stays true under question 5's recommendation.
- **Desk's journal consumer** (desk #218) reads these routes once a Runner
  release serves them and Desk pins it. Desk's proxy forwards only the paths it
  lists (desk `internal/desk/jobs.go:234`, at `c15769d`) and the query keys
  `after`, `q`, `state`, `review` and `preparations` (`:349`): the two routes
  and `job` need adding there. Desk refreshes no faster than its existing 5
  seconds, shows the journal as Runner's record, and derives no entry itself.
- **Runner #42.** `run.expired` tells an expired automatic run without its
  problem text, and `interruption` gives an interrupted run its own time;
  question 5 settles the run record's member. Serving `evaluation.stderr`
  stays with #42.

## 8. Delivery

Each step is its own pull request, and meets the issue's acceptance criteria
named with it.

1. **This record**, accepted on 2026-10-05 with the maintainer's answers to
   section 9 recorded in section 10.
2. **The schema migration**: the journal's table, indexed by job, release and
   kind; `journal.began` in the migration's transaction; the `schema` metadata
   raised to `"2"` (question 6); a `key_revision` column beside
   `key_hash` in `triggers`. A test opens a store written by Runner v0.5.0 and
   finds `journal.began`, its counts, and no other entry.
3. **The writers**, one per change in section 2, each in its change's
   transaction, the bare writes made transactions. Tests, each checked by
   breaking the fix and seeing the test fail:
   - for each writer, a state write made to fail leaves no entry, and an entry
     write made to fail leaves no change;
   - a restart during an evaluation yields `runner.started` and
     `run.interrupted` with `seen: false` and a `lastKnownRunning` that is not
     the restart's time, and a second restart adds nothing more;
   - an orderly stop yields `seen: true` and `runner.stopped`;
   - each idempotent retry of section 4 writes nothing, and repeated refusals
     stay within the bound;
   - no entry holds a token, key, key digest, bearer, input or address: the test
     searches every entry's bytes for the secrets it made.
4. **The routes and the OpenAPI document**, with `docs/MAPPING-V2.md` and the
   README.
5. **The contract in CI with a stand-in Desk**: a test that reads the routes as
   Desk will, from cursor 0, following `next` across a restart, and holds each
   answer to the OpenAPI schemas and to a fixture of every kind that Desk's own
   test reads too, as the briefs' wire contract is held on both sides
   (`brief_state.go:4`).

Steps 3 to 5 change a public surface. Each takes one cross-vendor review round,
recorded on its pull request, as the issue asks.

## 9. Questions for the maintainer

1. **Tamper evidence now or later?** Recommended: later. The journal ships
   labelled as the operator's own log. Chaining it makes a second chain, which
   touches Runner's entry form and Desk ADR-0010's Context and sections 5, 7
   and 8 (section 6). It is worth doing when a holder wants the Jobs activity
   witnessed, not before.
2. **Human review as a recorded act?** Recommended: not until an identity
   exists. Today `reviewed` is a flag set by whoever holds the bearer
   (`triggers_store.go:138-140`), and one bearer authorises every request: an
   entry would record a checkbox, not a person's review. `trigger.resumed`
   carries no review member, since every resume carries the flag.
3. **A store-wide route?** Recommended: yes, `GET /v1/events`, with a `job`
   filter. It is the only place for Runner's own entries and for refusals and
   releases that name no job, and with `job` it gives a job's entries with the
   restarts between them.
4. **Retention?** Recommended: never pruned, like the chain of runs. Runner
   deletes no run (README, "State and backups"), and a journal pruned ahead of
   its runs would leave runs without a history. An entry is a few hundred bytes,
   small beside the input and audit record of the run it describes. When
   retention of runs is designed, it prunes both, and the journal records the
   pruning.
5. **Does an interruption's own time replace `finishedAt` or sit beside it?**
   Recommended: beside, as `interruptedAt` on the run. `finishedAt` keeps its
   meaning, the time Runner recorded the run's end, which desk #213 labels so.
   `interruptedAt` is present when Runner saw the run stop, at an orderly stop.
   A run found at a restart has none: Runner cannot know when it stopped, and
   its journal entry gives the window, from `lastKnownRunning` to the restart
   (section 4). This answers part of #42, though not as #42 words it: #42 would
   put the restart's time in `interruptedAt` and the last time the run was known
   alive in `finishedAt`, which changes the meaning of a member Desk reads.
6. **Should a store the journal began in refuse an earlier Runner?**
   Recommended: yes, by raising the `schema` metadata to `"2"` (section 4).
   Otherwise an earlier Runner can change records without entries and leave no
   sign of the gap. The cost: going back to an earlier Runner needs a copy of
   the store from before the migration.

Out of scope, as the issue says: a tamper-evident journal, human review as a
recorded act, and serving `evaluation.stderr` (#42).

## 10. The maintainer's answers

The maintainer answered on 2026-10-05 with "accept the recommendation", read
as accepting each recommendation of section 9 as written. Each answer is
recorded below; a later record may overrule any of them.

1. **Tamper evidence: later.** The journal ships now, labelled as the
   operator's own log. Chaining it waits for a holder who wants the Jobs
   activity witnessed, and for a record of its own, which would revisit Desk
   ADR-0010's Context and sections 5, 7 and 8.
2. **Human review: not recorded** until a request carries an identity that
   Runner has a reason to believe. `trigger.resumed` carries no review member.
3. **A store-wide route: yes.** `GET /v1/events`, with a `job` filter, beside
   `GET /v1/jobs/{job}/events` (section 5).
4. **Retention: never pruned**, like the chain of runs. When retention of runs
   is designed, it prunes both, and the journal records the pruning.
5. **The interruption's time: beside `finishedAt`.** A run gains
   `interruptedAt`, present when Runner saw it stop. `finishedAt` keeps its
   meaning. A run found at a restart has no `interruptedAt`, and its
   `run.interrupted` entry gives the window, from `lastKnownRunning` to the
   restart (section 4).
6. **Earlier Runners: refused.** The migration raises the store's `schema`
   metadata to `"2"` (section 4), so every earlier Runner refuses a store the
   journal began in. Going back to one needs a copy of the store from before
   the migration.
