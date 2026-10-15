# Immutable record delivery

Desk owns collection eligibility, reviewed recipe conversion, source acquisition
and the immutable record outbox. Runner owns admission, occurrence queues,
execution and retained results. Neither Runtime semantics nor Gateway signing
custody changes.

The local owner control API offers `record-delivery` in capabilities:

- `POST /v1/triggers/{trigger}/deliveries`: `{releaseId,event:{id,occurredAt,input}}`.
- `GET /v1/triggers/{trigger}/deliveries/{event}`: reconcile by stable identity.

A reviewed job starts with a paused event trigger and `overlap: queue`. Desk saves
the exact input envelope before first delivery. The identity hashes collection,
record, record digest, recipe version and release identity. Retries preserve the
entire envelope, including timestamp and receipts. An identical delivery returns
the original occurrence after restart or pause. A changed payload conflicts.
A new record revision uses a new ID. A changed release requires a reviewed job.

The owner route allows old occurrence timestamps because a durable outbox may
wait through downtime. It still rejects future timestamps and checks the fixed
release, mapping, source proofs/freshness and normal queue limits. Public token
event delivery retains its five-minute freshness window. The admission journal
attributes owner delivery to the installation, not an unused event credential.

If an unacknowledged snapshot loses eligibility, Desk first reads its stable
identity. A retained occurrence is reconciled; a 404 means it was never accepted
and can be marked superseded without admitting stale facts. Accepted work may
finish after pause. Queue expiry or preparation failure is an observable terminal
result, never permission to repeat uncertain provider acquisition.

Validation: `TestRecordDeliveryFrozenRevisionAndRestart` plus existing event
concurrency, token, expiry and initial-trigger tests with `JPACK_TEST_BIN` set.
