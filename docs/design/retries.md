# Retries

Status: built for #238 and #219. A job created without a retry policy, and a
run nobody retries, are what they always were: every record about them, and
every journal entry, is written byte for byte as before.

Two things are added:

- a **retry policy** fixed on a job, which repeats a source acquisition that
  failed for a reason on a closed list (section 3), after a back-off and no
  sooner than the source's `Retry-After`, up to a number of attempts; and
- an **explicit retry** of an interrupted run, `POST /v1/runs/{run}/retry`,
  which queues the same run again and keeps the interrupted attempt's record
  (section 6).

## 1. Where Runner reaches a source

The issue reads as if a run calls a source. On main it does not: a run's input
is frozen when the run is queued, and the dispatcher only invokes the Runtime
on it (`dispatchNext`, `store.go`). Runner reaches a source in one place,
before a run exists: while it **prepares an occurrence** of a trigger whose
input is `mapped-sources`.

- With a **Gateway connection that is not durable**, `prepareLegacyOccurrence`
  (`background.go`) makes one call to the gateway's `/acquire` per operation
  source, bounded by the occurrence's `expiresAt`. A failure ends the
  occurrence `failed` with the error's sentence as its reason, and no run is
  queued.
- With a **durable connection**, the caller-owned source worker makes the
  call; a failure becomes `needs-attention` and is never repeated, since an
  operation whose outcome is uncertain may have run (`durable-sources.md`).

So the retry policy governs the **acquisition attempts of an occurrence** over
a non-durable connection, and the attempt records, the wait and the final
state are the occurrence's. A run is queued only once an attempt succeeds, as
before. Durable acquisition is not retried (section 9).

## 2. What reaches Runner from the gateway today

`automaticCall` sees one of:

- a transport failure, or no answer within its 60 seconds or the
  occurrence's deadline: `gateway_unavailable`;
- an answer whose status is not 200: `gateway_refused`, whatever the status;
- an answer over its size limit: `gateway_response_too_large`;
- a 200 whose receipt names another session: `gateway_receipt_mismatch`.

The gateway's `/acquire` answers a source that failed, **including a source
that was throttled**, with `400` and `{"error": "source failed: <text>"}`; the
provider's 429 and the wait it asked for appear only inside that text. It
answers `503`, with no `Retry-After`, only when its own admission is closed
or a delegation limit is reached. No gateway answer carries `Retry-After`
today.

Runner now reads the status and `Retry-After` of a gateway answer that is not
200 (section 4), and keeps the error and its sentence as they were, so that an
occurrence of a job without a policy fails exactly as before. Against today's
gateway that means a throttled source is a `400` and is **not** retried; a
closed gateway admission or a reached delegation limit (`503`) is. For a source's
throttling to be retried, the gateway would answer it as `429` (or `503`)
with `Retry-After`, rather than `400` with the wait in text. That is a gateway
change, not made here.

## 3. The policy

`POST /v1/jobs` takes an optional `retry` member:

```json
{"retry": {"maxAttempts": 3, "backoffSeconds": 5, "maxBackoffSeconds": 60}}
```

| Member | Bounds |
|---|---|
| `maxAttempts` | 1 to 10: the attempts an occurrence's acquisition may make, the first among them |
| `backoffSeconds` | 1 to 3600: the wait before the second attempt |
| `maxBackoffSeconds` | `backoffSeconds` to 3600: the longest back-off |

All three members are required, as written (a member whose name only folds to
one of them is refused, as `scope`'s are), and no other member is taken. A
policy out of form or bounds, or `retry` written `null`, is refused `422`
`invalid_retry_policy`, and the refusal is journaled as any refused creation
is. Without the member the job has no policy: **one attempt, as today**.

The back-off is deterministic: the wait before attempt *n* + 1 is
`min(backoffSeconds × 2^(n−1), maxBackoffSeconds)`. There is no jitter: one
Runner makes one call per occurrence, and nothing here spreads callers.

The policy is fixed at creation, kept by every upgrade (an upgrade keeps the
job's record; its body takes no `retry`), and is part of the creation's
replay: a repeated creation from the same release must name the same policy,
or none, or it is refused `409` `release_already_used`. A Runner held to the
manual-admission gate refuses a policy (`manual_unsupported`), as it refuses a
trigger. `GET /v1/status` lists the capability `retries`.

**Retried failures, a closed list.** An attempt is retried only when its
failing call was answered by the gateway with:

| Status | Attempt `reason` | Why it is safe to repeat |
|---|---|---|
| `429` | `gateway_refused`, `status` 429 | the call was refused for its rate, not served |
| `503` | `gateway_refused`, `status` 503 | the call was refused as unavailable, not served |

and no earlier call of the same attempt had answered (a mapping with several
operation sources acquires them in turn; repeating the attempt after one
answered would call that source again).

Nothing else is retried: no answer at all (a transport failure or a timeout,
`gateway_unavailable`: the call may have reached its source, and the existing
rule that an uncertain call is never repeated holds), any other gateway
status (a source failure is a `400`; a `502` or `504` from a proxy may follow
a call that ran), an answer over its limit, a receipt mismatch, a mapping or
input problem, a connection no longer installed, the occurrence's deadline or
a stop cutting the call short, and every evaluation failure, refusal and
configuration problem. Such an attempt is recorded `failed` and ends the
occurrence `failed` with its sentence, as today.

## 4. `Retry-After`

A gateway answer of a retried status may carry `Retry-After`, as
delta-seconds (`120`) or as an HTTP-date (`Wed, 21 Oct 2026 07:28:00 GMT`,
read by `http.ParseTime`). The next attempt waits for the longer of the
back-off and that wait. A value that is neither form, negative, or that names
a time already past, is ignored: the back-off alone applies. An HTTP-date is
measured from the moment Runner read the answer.

**The cap is 3600 seconds, and Runner fails rather than clamps.** A wait
longer than that, or a next attempt that would fall at or after the
occurrence's `expiresAt`, ends the occurrence `failed` with the reason
`retry-wait-too-long`: clamping would attempt sooner than the source asked,
and the occurrence's deadline is not extended for a retry.

## 5. Attempts, the wait, and durability

For a job with a policy, each acquisition attempt has a record in the
occurrence's `attempts`:

```json
{"number": 2, "delaySeconds": 5, "startedAt": "…", "endedAt": "…",
 "outcome": "transient", "reason": "gateway_refused", "status": 503,
 "retryAfterSeconds": 30}
```

`outcome` is `succeeded`, `failed` (not retried), `transient` (a failure on
the list in section 3) or `interrupted` (in flight at a crash).
`delaySeconds` is the wait that preceded the attempt, 0 for the first.
`status` and `retryAfterSeconds` are present when the gateway answered them.
An occurrence of a job without a policy has no `attempts`.

**The wait is stored.** After a transient attempt with attempts left, the
occurrence returns to `accepted`, keeps its pending input, and holds
`nextAttemptAt`, the time of the next attempt. The preparation workers pass
it by until that time, as they pass a durable preparation's `nextCheck`; no
timer or goroutine waits for it, and a restart reads it from the record. The
journal writes the return as `occurrence.retry-scheduled`, with the closed
attempt in `detail.attempt`.

**Exactly once, no earlier.** An attempt starts under the preparation lock by
writing the occurrence `preparing`, with its record, before the call; the
workers take only an `accepted` occurrence whose `nextAttemptAt` has passed.
So an attempt is made at most once for its number, and never before its time.

**At the limit.** A transient attempt that is the last the policy allows ends
the occurrence `failed`, reason `attempt-limit-reached`, and it is never tried
again.

**A crash during an attempt** is recovered as today: the restart ends the
occurrence `failed` ("Source acquisition was interrupted. It was not repeated
automatically."), and its open attempt record is closed `interrupted`. It is
not retried automatically, and it is not counted as transient: the call may
have reached the source, which a retry would then call again. This is the
narrowest answer that keeps the existing rule that an interrupted acquisition
is never repeated.

All times come from the Service's clock (`Service.clock`), so that tests move
time rather than sleep.

## 6. Explicit retry of an interrupted run (#219)

`POST /v1/runs/{run}/retry`, body `{"attempt": n}`, where *n* is the run's
`attempt` as the caller last read it. It goes through the scoped router like
every route that names a run: a request whose `scope` is not the run's is
answered as a run the store does not hold, `404` `not_found`.

It **queues the same run again**; it does not make a linked run. The run's id,
job, release, revision, input, `requestedBy` and trigger origin are kept. The
interrupted attempt's record is appended to the run's `attempts`:

```json
{"number": 1, "delaySeconds": 0, "startedAt": "…", "endedAt": "…",
 "outcome": "interrupted", "reason": "runner-stopped"}
```

where `reason` is `runner-stopped` for a stop Runner saw (the run's
`interruptedAt`, which becomes `endedAt`) and `runner-restarted` for a run
found running at a restart (no `endedAt`: Runner did not see it stop). The run
is then `queued`, with `startedAt`, `finishedAt`, `problem` and
`interruptedAt` cleared and `attempt` unchanged; the dispatcher makes the next
attempt *n* + 1, which evaluates into its own directory,
`attempts/<run>.<n+1>` (the first attempt's directory, `attempts/<run>`, is
kept as it was and never reused). `GET /v1/runs/{run}/diagnostics` reads the
latest attempt's directory.

Answers:

- `202` and the run, queued, for a retry made.
- `200` and the run as it now is, for a request whose *n* names an attempt
  already retried (its record is in `attempts`): a repeated request changes
  nothing.
- `409` `run_not_retryable` for a run in any state but `interrupted`; for an
  interrupted run whose record holds an audit record (its evaluation may have
  completed); for a run of a trigger whose `expiresAt` has passed; and for a
  run at 10 attempts.
- `409` `attempt_mismatch` for an *n* that is neither the run's attempt nor
  one already retried.
- `429` `queue_full`, `503` `dispatcher_stopped`, and `manual_unsupported`
  under the manual-admission gate, as for any run queued.

A refused retry of a run the store holds is journaled `admission.refused`
with `request` `retry-run`. A retry made is journaled `run.queued`, from
`interrupted`, by the caller, with the kept attempt in `detail.attempt`.

**Event ids.** Sending the original event id again replays the same
occurrence, whose `runId` is this run: it returns the run as it now is, queued
or in its new attempt, and queues nothing. The same holds for repeating the
original `Idempotency-Key` of a run queued by `POST /v1/jobs/{job}/runs`.

**What does not change.** A retried trigger run is still held to its queue
deadline and to source freshness at dispatch, as every attempt is. It keeps
its place in the store's order of insertion, so it is dispatched ahead of
runs queued after it was first queued. The job's retry policy does not apply
to runs: an interrupted run is retried only when asked, and the job's
`maxAttempts` (which counts acquisition attempts) does not bound it; the
bound of 10 attempts does.

## 7. The journal

Following the journal's rule that a member added later raises `entryVersion`,
**an entry that carries `detail.attempt` has `entryVersion` `"4"`**, whatever
its `by` and scope (an entry of `"4"` may hold a caller and a scope as well).
Every other entry keeps its version. The kind `occurrence.retry-scheduled` is
new; `run.queued` from `interrupted` is the existing kind with a `from`.

A refused retry names its run in `concerns`, and the refusal's coalescing key
names the run too, so that one run's refusal within the minute does not hide
another's. Every other refusal names no run and keeps its key. A malformed
retry request (empty, without `attempt`, or over the body limit) naming a run
the store holds is journaled as `400` `invalid_request`.

The journal's test that walks every change of state through one store at
schema `"3"` and entry version `"1"` does not walk `occurrence.retry-scheduled`,
which needs a job with a policy and so a store at `"6"`; the kind is covered by
the retry tests instead.

## 8. The store, and going back

No table or column is added. A job's `retry`, an occurrence's `attempts` and
`nextAttemptAt`, and a run's `attempts` are members of the records the store
keeps. The run's `attempts` is kept and served with the run, as
`interruptedAt` is, and is not part of a verification export, whose readers
decode strictly, nor of the run's entry in the chain of runs.

**A store's first job with a policy, or its first explicit retry, raises its
schema to `"6"`** in that transaction (`raiseRetrySchema`), from `"3"` or
`"5"`. An earlier Runner would drop the members from every record it rewrites
(a job it upgrades, an occurrence it advances, a run it dispatches) and would
evaluate a retried run into the first attempt's directory, which exists; every
earlier Runner refuses a store at a schema not its own. This Runner opens a
store at `"3"`, `"5"` or `"6"`, and `relocate` and the store's upgrade carry
it. A store at `"6"` that then creates a job under a scope stays at `"6"`.
Going back needs a copy of the store from before the raise.

The store contract's transaction gains `raiseSchema`, so that a kind without
schemas records the raise; and a run may now return from `interrupted` to
`queued`, once per retry, under the same state fence as every change.

## 9. Not in this change

- Retrying durable acquisition, whose operations may have run.
- Retrying an evaluation's failure, or a run that failed.
- An automatic retry of an interrupted run or acquisition.
- Jitter, or a policy that differs per source.
- **Two Runner processes over one store** are out of scope: the store is held
  by one process (its lock), and its single dispatcher and preparation lock
  are what make an attempt happen once.
- A gateway change: section 2 says what the gateway would answer for a
  source's throttling to be retried.
