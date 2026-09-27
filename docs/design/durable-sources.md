# Durable source preparation

Status: implemented locally; cross-repository release/review pending.

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

Gateway's immutable file claim is the admission fence across processes. A claim
is never reclaimed. It returns the retained acquisition response on an identical
request and rejects a changed request or principal. A Gateway owner disappearance
is uncertain, not evidence of failure at the provider. Fresh processes may claim
unstarted intents; a previously claimed operation cannot execute again.

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

The Gateway operations API currently requires Linux/macOS (Unix directory sync).
Other Gateway platforms refuse durable admission rather than weaken its claim.

Only mappings whose operation profiles all have `durable: true` use this path.
Existing synchronous configurations retain the bounded, non-retry behavior.
Four Runner preparation workers and four active async reads per Gateway process
bound local concurrency. Control requests last at most three seconds, with
bounded backoff for unavailable/throttled connections; identical operation POSTs
never repeat a claimed source. Provider call attempts are not retried by Runner.

One Gateway instance should own the async store. Other instances sharing it can
read completed results but report another instance's active claim as uncertain.
Source and adapter timeouts remain operator policy; the seven-day preparation
ceiling does not override them. No HTTP request stays open for that duration.

The operations directory is private: request arguments, result bytes and
commitment salts are sensitive. Back it up with the Gateway private store, not
just its public receipt corpus. Runner's original input checkpoints live in its
private SQLite store. Public progress responses omit them and Gateway addresses.
There is no automatic operation/occurrence deletion or retention policy yet.
Do not delete operation claims while clients could still retry those IDs.

## Verification

Run the normal Runner tests with `JPACK_TEST_BIN`. For the actual cross-process
path also set `JPACK_GATEWAY_TEST_BIN` and `JPACK_MCP_ADAPTER_TEST_BIN`, then run:

```
go test -race -run TestDurablePreparation ./internal/runner
```

The integration test builds a controllable MCP service, uses real Gateway and
adapter binaries, pins their actual identity, reviews a real Runtime release,
restarts Runner while the MCP call waits and checks that one source invocation
produces one auditable evaluation. Other tests cover checkpoint recovery,
original-ID reconciliation, cancellation, unrelated-job progress, expiry,
redaction, altered request commitments and stale receipts.

Provider-native asynchronous operation IDs, authenticated callbacks, explicit
refresh plans and browser-initiated acquisition are outside this first slice.
No new receipt format, Runtime behavior, workflow framework or hosted service is
introduced.
