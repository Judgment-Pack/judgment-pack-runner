# Run results: counts, an error class, and following runs by outcome

Status: built for #239 and #217. A run that has not finished, a run finished
before this change, and every journal entry other than a run's end or an
occurrence's failure are what they always were, byte for byte.

Four things are added:

- **counts** on a finished run (section 2);
- an **error class**, from a closed list, on a run or an occurrence that ended
  without a decision (section 3);
- the **disposition**, the error class and the counts on the journal entry of
  a run's end (section 4);
- **filters by outcome and by error class** on the runs listings and the
  journal, and by kind on the journal (section 5).

Section 6 records the decision #217 asks for, on pushing finished runs.

## 1. What Runner knows about a run's result

A run is **one evaluation** of an input frozen when the run was queued. Its
record holds the Runtime's output (`result`), whose `disposition` is the one
decision the evaluation made: `{"kind": "outcome", "outcomeId": "…"}`,
`{"kind": "unresolved"}` or `{"kind": "not-applicable"}`, with the pack's
reasons and the handoff beside it. The audit record holds the same
disposition, and Runner refuses an evaluation whose two differ
(`runtime.go`). A run that failed, expired or was interrupted holds no
disposition.

Sources are reached **before** a run exists: an occurrence of a trigger whose
input is mapped from sources acquires them, or a request that names sources
prepares them, and the run is queued with the preparation in its
input (`input.preparation`). The preparation records one outcome per source it
prepared (`preparation.outcomes`, the case among them when the mapping reads
one). Runner does not keep a count of the calls a preparation made, and a
preparation's citations are not one per call (a research source adds one per
retained response), so no count of calls is given.

A run's end has, until this change, a state, its times, a sentence
(`problem`), and for four ways of ending without an evaluation a code
(`reason`: `queue-expired`, `profile-set-aside`, `binding-refused`,
`prepared-by-earlier-runner`). An occurrence's end has a state and a `reason`
that is a code for some ends (`queue-expired`, `attempt-limit-reached`,
`retry-wait-too-long`, and the skips `missed`, `latest-after-missed`,
`trigger-paused`, `queue-full`, `overlap`) and a sentence for the others. An
attempt a retry kept has a reason code too (`runner-stopped`,
`runner-restarted`).

## 2. Counts

A run that reaches `completed`, `failed` or `interrupted` from now on is
written with `counts`:

```json
"counts": {"sources": 2, "decisions": 1, "byKind": {"outcome": 1}, "byOutcome": {"proceed": 1}}
```

| Member | What it counts |
|---|---|
| `sources` | the sources the run's preparation records an outcome for (`input.preparation.outcomes`); absent for a run whose input holds no preparation (facts given directly, or a file or constant input) |
| `decisions` | the decisions the run's record holds: 1 for a completed run, 0 for any other |
| `byKind` | those decisions by disposition kind, `outcome`, `unresolved` or `not-applicable`, as the Runtime wrote it; `{}` when there are none |
| `byOutcome` | those of kind `outcome`, by `outcomeId`; `{}` when there are none |

**Today a run is always exactly one decision**, so `decisions` is 1 or 0 and
each map holds at most one member. The shape is kept for runs that hold
several, so that a reader written against it stays right; nothing here makes
a run hold several. A disposition whose kind is not a string is counted in
`decisions` and in neither map; one of kind `outcome` without a string
`outcomeId` is counted in `byKind` and not in `byOutcome`. An interrupted run
holds no decision even when its attempt may have written an audit record
(`retries.md`, section 6): the count is of what the record holds.

`counts` is absent on a run not yet finished, on a run finished before this
change (nothing is computed after the fact), and on a run queued again by an
explicit retry, which loses it with its `problem` until its next end. Counts
are per run: no route adds runs' counts together, so no count can mix two
scopes.

## 3. The error class

Beside `reason`, a run that ends `failed` or `interrupted`, and an occurrence
that ends `failed` or `expired`, from now on carries `errorClass`, one of:

| Class | Meaning | Produced by |
|---|---|---|
| `source` | a source acquisition through a Gateway connection failed: no answer, a refusal, an answer over its limit, a receipt for another session | an occurrence whose acquisition failed with `gateway_unavailable`, `gateway_refused`, `gateway_response_too_large` or `gateway_receipt_mismatch` |
| `preparation` | the input could not be read or prepared, or no longer passes its checks | an occurrence whose input could not be read or prepared for another reason; an occurrence whose run Runner refused to queue for its input (an answer below 500); a run whose input failed its check again at dispatch; `prepared-by-earlier-runner` |
| `evaluation` | the Runtime's evaluation failed, or what it wrote did not pass Runner's checks | a run whose evaluation failed |
| `configuration` | what the work was set up with changed or is not installed | `profile-set-aside`; a run whose release's profiles failed their check at dispatch; an occurrence whose job's release changed before admission; an occurrence whose acquisition named a connection no longer installed (`gateway_connection_required`) |
| `limit` | a time or count bound ended the work | `queue-expired` (a run or an occurrence), `attempt-limit-reached`, `retry-wait-too-long`, a durable preparation's source wait expiring |
| `interrupted` | Runner stopped or restarted during the work | a run interrupted at a stop or found at a restart; an acquisition found at a restart; `runner-stopped` and `runner-restarted`, the reasons of an attempt a retry kept (`retries.md`) |
| `internal` | Runner's own fault | `binding-refused` (a binding the Runtime refused is Runner's defect); a run whose release could not be read at dispatch; an occurrence whose submission Runner could not make (an answer of 500 or more) |

The list is closed: a class is added only with a change of this record, and a
reader may treat an unknown class as a newer Runner's. **Every reason code
maps to exactly one class**, by the table `reasonClasses`
(`run_results.go`). The skips (`missed`, `latest-after-missed`,
`trigger-paused`, `queue-full`, `overlap`) are a trigger's own rule, not a
failure, and are listed apart (`notFailureReasons`); a skipped occurrence has
no class. A test reads every code the package assigns to a `reason` and holds
it to one of the two lists. A failure that has a sentence and no code takes
its class from the place it is met, as the table's last column says.

**Not a failure.** A completed run has no error class, whatever its
disposition: an `unresolved` decision is a decision, counted in `byKind`, and
its reasons are the pack's. Runner does not read a source's status in the
preparation to call a decision a source failure. An occurrence waiting for a
person (`needs-attention`) or cancelled by one has none either: the first has
not ended, the second was not a failure.

`errorClass` is absent on success, on a record that ended before this change,
and on a run queued again by a retry, until its next end.

## 4. The journal

The entry of a run's end, `run.completed`, `run.failed`, `run.expired` or
`run.interrupted`, carries the run's `counts`; `run.completed` carries
`disposition`, the decision's `kind` and, for kind `outcome`, its
`outcomeId` (not its reasons or handoff, which the run serves); the others
carry `errorClass`. `occurrence.failed` and `occurrence.expired` carry the
occurrence's `errorClass`.

Following the journal's rule that a member added later raises `entryVersion`,
**an entry that carries `disposition`, `errorClass` or `counts` has
`entryVersion` `"5"`**, whatever its `by`, scope and attempt (an entry of
`"5"` may hold a caller, a scope and an attempt as well). Every other entry
keeps its version and its bytes.

## 5. Filters

`GET /v1/runs` and `GET /v1/jobs/{job}/runs` take:

- `outcome=<outcomeId>`: the completed runs whose disposition is of kind
  `outcome` with exactly that `outcomeId`, compared byte for byte. Repeated, a
  run matches any of them. A run with no disposition, or one of another kind,
  matches none: so does a failed run, and a run finished before this change
  is matched by the disposition its result has always held.
- `errorClass=<class>`: the runs whose `errorClass` is that class. Repeated, any
  of them. A run that ended before this change has no class and matches none.

`GET /v1/events` and `GET /v1/jobs/{job}/events` take the same two filters,
over the entries that carry the member (`disposition.outcomeId`, for an entry
whose `disposition.kind` is `outcome`; `errorClass`), and `kind=<kind>`, the
entries of that kind, repeated for any of them. The store-wide route with
`job` serves, as before, the job's entries and Runner's own
(`journal.began`, `runner.started`, `runner.stopped`), of those the ones the
filters keep: Runner's own carry no disposition and no class, so an `outcome`
or `errorClass` filter leaves them out, and a `kind` filter keeps them when it
names their kind.

A filter combines with every other filter of its route (`q`, `state`,
`review`, `scope`, the job of the route, `job`, the cursors) by AND, and is
applied before the page is cut, so paging is as stable as it was: a run's or
an entry's place in the order does not depend on the filter. Values:

- each parameter may be given at most 10 times;
- an `outcome` is 1 to 256 bytes of UTF-8 with no control character (the
  bound a workflow's outcome rule has); one that is well formed and matches
  no run answers an empty page;
- an `errorClass` outside the closed list, a `kind` outside the journal's
  kinds, an `outcome` out of form, or any of them given more than 10 times, is
  `400` `invalid_filter`, as a malformed `state` is; on `GET /v1/jobs` either
  run filter is `400` `invalid_filter`, as `state` is;
- a piece of the query that does not decode (`outcome=%zz`, a `;`) and names
  one of the filters is `400` `invalid_filter`, not dropped, so that a filter
  is never read as none (the runs listings read the rest of their query
  leniently, as before; the journal refuses any query that does not decode).

**Scope.** The filters read the same rows as the scope filter, and AND with
it: a listing under a scope never shows another scope's runs or entries, with
or without them. Nothing is counted across rows.

**The store.** No column, index or table is added and the schema is not
raised: the filters read members of records the store already keeps, as `q`
and `scope` do, at the same cost: a listing walks from its newest row and
stops at a page of matches. An earlier Runner that opens a store this Runner
wrote ignores the new members of a run (it decodes runs leniently, and
rewrites a finished run only to queue an interrupted one again, which drops
them as this Runner does), and serves an entry of `"5"` without the members it
does not know, since it reads an entry's detail into its own form; the store
keeps them, and this Runner serves them again. The listings are statements
outside the store contract (`store-interface.md`, section 6), so the filters
are the sqlite kind's; what the contract carries, the run's record, keeps its
counts and class in both kinds (`TestStoreKindsConform`).

**What is not changed.** A verification export's run, whose readers decode
strictly, and the run's entry in the chain of runs carry neither member, as
they carry no `reason`. Relocation copies the store and rewrites no run.

`GET /v1/status` lists the capability `run-results`, so that a client can
tell a Runner that filters from one that would ignore the parameters and
answer every run.

## 6. A push of finished runs: decided against, for now

#217 asks for a decision on a push or a stream of finished runs. **The stream
is the journal.** A consumer follows
`GET /v1/events?kind=run.completed&kind=run.failed&kind=run.expired&kind=run.interrupted`
(or `outcome=` for the decisions it acts on, `errorClass=` for the failures)
from its cursor, `next`, asking again with the same cursor when it has caught
up (`activity-journal.md`, section 4). Each entry is written in the
transaction of the change it records, its sequence never goes back, and
`scope` holds a consumer to one scope's runs. Nothing is lost while the
consumer is away, and Runner holds nothing per consumer.

**No push is built in this release.** A push would need, each its own design:

- **delivery records**: a durable outbox of what is owed to which receiver,
  written in the change's transaction, with each delivery's state, so that a
  restart neither drops nor repeats one silently (the record delivery outbox,
  `record-delivery.md`, is the inbound precedent);
- **receivers**: their configuration, who may set one, and a scope rule, since
  a receiver of one scope must never be sent another's;
- **signing**: a key and a signature over each body, so that a receiver can
  tell Runner's push from anyone's;
- **retries**: a back-off, a bound, and what a receiver that never answers
  leaves behind;
- **what is sent**: the entry, or the run, and whether the receiver must still
  read the journal to fill a gap.

## 7. Not in this change

- A filter by disposition kind (`unresolved`, `not-applicable`): `review=true`
  and `state=completed` find unresolved decisions with requested handoffs, as
  before.
- An error class on a run or an occurrence that ended before this change, or
  derived from a sentence.
- Counts across runs, or of the calls a preparation made.
- A push of finished runs (section 6).
