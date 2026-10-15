# Reviewed job upgrades

Status: implemented; reviewed independently by a clean-room Codex agent. This is
same-vendor review, used at the maintainer's explicit request.

A release is an immutable execution snapshot; a job chooses one release at each
revision. `POST /v1/jobs/{job}/upgrade` takes `releaseId`, `baseReleaseId`,
`baseRevision` and `reviewed: true`. The operator previews and checks the new release
through the existing preview API, then reviews the complete candidate. The pack
identity stays the same. Changed pack bytes require a greater artifact version,
verified by Runtime's `artifact version` command. The number does not establish
compatibility or grant permission. Previewing does not publish a pack identity;
using a release in a job does, and that identity cannot later acquire other bytes.

The installation's tested-release requirement applies to upgrades. Every trigger
must be paused and every accepted, preparing, waiting or needs-attention occurrence
must settle first. Existing trigger configurations must be valid against the new
release; incompatible configurations require a new reviewed job. The upgrade
leaves triggers paused and advances their revisions so a resume reviewed against
the old release cannot enable the new one. Credentials and their issuance
revisions remain intact. Connected Desk Dialect subscriptions retain their original
release; an upgrade does not retarget those subscriptions.

One SQLite transaction changes the job, retains its revision receipt, advances
trigger revisions, and appends `job.upgraded` and `trigger.configured` entries.
Queued and running runs already retain their own release and Runtime pins and are
not rewritten. New run admission reads the selected revision in its transaction.

The store schema advances from 2 to 3. Migration retains each existing job as its
initial creation receipt and revision. Earlier Runner binaries refuse schema 3.
Creation retries use permanent `job_creations` receipts, so upgrading cannot free
an earlier preview for duplicate job creation. Exact upgrade retries return the
original revision receipt even after a subsequent upgrade or restart; they do not
repeat checks or write another event. A different request with a stale base is
refused. An uncertain acknowledgment requires reconciliation with that same request.

The job journal includes every release in its revision history. Refused upgrades
follow the existing policy for refused configuration changes: no journal event.
This capability is advertised as `job-upgrades`; older clients can continue to
create and run jobs. Graph releases remain outside the single-pack Jobs contract.

Tests cover real Runtime version checks including large components, immutable
identity admission, migration from schema 2, exact replay after further revisions
and restart, historical run pins, every pending occurrence state, stale trigger
reviews, and atomic rollback when the event write fails.
