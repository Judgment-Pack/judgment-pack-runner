# Durable source preparation

Status: implemented. Independent review and dispositions: [Runner PR #4](https://github.com/Judgment-Pack/judgment-pack-runner/pull/4).

Runner owns the occurrence and its preparation. Gateway owns the source process,
credentials and ordinary signed acquisition response. Runtime sees one frozen,
verified facts/evidence snapshot. No browser, scheduler or model loop owns waiting.

## State and recovery

- `accepted`: waits within the original admission queue expiry.
- `waiting`: persisted case/file snapshot, absolute preparation deadline and an
  ordered list of immutable operation requests. Requests are committed before
  any network call. Each verified completion checkpoints its original response.
- Ready inputs return to `accepted`, with `readyUntil` for the evaluation queue.
- `needs-attention`: uncertain completion, changed authority, stale evidence or
  invalid proof. Owner reconciliation retains every operation ID and deadline.
- `expired` / `cancelled`: terminal for automatic processing. Late completions
  cannot overwrite the occurrence or create a decision.
- `submitted`: links the existing idempotent evaluation run. Source history stays
  in the occurrence, even when its final inputs move into the evaluation record.

The single-owner database process lock excludes another Runner. A mutex and
in-flight occurrence set exclude competing workers; each save checks the stored
state so terminal changes fence a response already in transit. We deliberately
have no expiring lease that could reassign an uncertain provider invocation.
Scaling to multiple Runner owners would require database leases and fencing
before this ownership assumption could change.

Runner's independent `jpack-source-worker` is the Gateway's caller. A SQLite
FULL/WAL transaction commits each operation claim before the worker calls the
ordinary `/acquire` endpoint. It retains the original response, including salts,
in caller-owned storage outside the signer store. Only its installation-owned
bearer can submit, inspect or cancel operations; browser Origins are refused.
Its store is bound to one Gateway origin and held by one OS process lock. Changed
requests cannot reuse an ID. Previously running work becomes uncertain after a
worker restart and is never replayed. Completed responses remain available.

Operation IDs do not prove input correctness. The final response must verify its
signature, result bytes, original argument commitment, source/adapter/endpoint
pins, output-class admission and freshness. The dedicated session must be
`async.<operation-id>`, call index zero. Pending status contains no evidence.
Local files and case parameters are captured once; completed sources do not
change silently while a later source waits. If their freshness window expires,
the occurrence needs review. A future refresh feature must invalidate dependent
claims explicitly instead of mixing observations without recording it. A job with
an uncertain preparation remains active for the default skip-overlap policy until
the owner reconciles or cancels it.

## Limits and operator configuration

Install the source worker separately from an individual Runner, with a stable
loopback port and private directory. It owns no signing key. Runner connects using
`operationsUrl` and `operationsTokenFile` alongside the original Gateway `url`.
Only mappings whose operation profiles all have durable worker connections use
this path. Legacy synchronous configurations remain bounded and non-retrying.

Four preparations per Runner and four acquisitions per worker bound concurrency.
Control requests last at most three seconds and reuse identical operation intent.
The worker's HTTP connection to Gateway remains open during the read; browser and
Runner requests do not. Source and adapter timeouts remain independent policy.
Cancellation/expiry persists before cancellation of the worker HTTP wait, and a
conditional completion update cannot overwrite terminal state. A remote provider
may continue after cancellation. Uncertainty never becomes negative evidence.

Gateway retains only its ordinary artifacts, receipts and seals, without caller
salts. The worker's SQLite database retains private requests and proof responses;
back it up separately with owner-only access. Public Runner progress omits private
inputs, worker/Gateway URLs and profile bindings. The worker admits at most 10,000
retained operations. No automated deletion is provided; retain claims while an ID
could be retried. Moving custody into this caller process resolves the clean-room
finding against the initial signer's `/operations` implementation without changing
SPEC.md or the receipt format.

## Verification

Run the normal Runner tests with `JPACK_TEST_BIN`. For the actual cross-process
path also set `JPACK_GATEWAY_TEST_BIN` and `JPACK_MCP_ADAPTER_TEST_BIN`, then run:

```
go test -race -run TestDurablePreparation ./internal/runner
```

The integration test builds a controllable MCP service, uses a separate caller worker, real Gateway and
adapter binaries, pins their actual identity, reviews a real Runtime release,
restarts Runner while the MCP call waits and checks that one source invocation
produces one auditable evaluation. Other tests cover checkpoint recovery,
original-ID reconciliation, cancellation, unrelated-job progress, expiry,
redaction, altered request commitments and stale receipts.

Provider-native asynchronous operation IDs, authenticated callbacks, explicit
refresh plans and browser-initiated acquisition are outside this first slice.
No new receipt format, Runtime behavior, workflow framework or hosted service is
introduced.
