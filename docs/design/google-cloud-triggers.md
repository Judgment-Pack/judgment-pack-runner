# Google Cloud delivery and unattended Gateway operations

Implementation scope: one Google Cloud Scheduler adapter, with a provider-neutral
Runner trigger boundary. Cloud Scheduler calls an authenticated Cloud Run relay;
the relay publishes only the original scheduler job name and scheduled timestamp
to Pub/Sub. A local pull consumer records that identity transactionally before
acknowledging delivery. The original scheduled timestamp survives both Scheduler
and Pub/Sub retries. No inbound connection to Desk is required.

Runner owns trigger admission, overlap, expiry, acquisition and evaluation. Desk
owns the editor and installation configuration. Gateway owns provider credentials,
acquisition and signed receipts. Cloud infrastructure stores occurrence signals;
inputs, receipts, pack releases and audit files remain in local Runner storage.

Cloud bindings are installation-selected dedicated pull subscriptions. Credentials
are an explicit local ADC file (including workload identity configurations), never
browser input or a job export. A trigger selects a connection ID and an exact
scheduler resource name. It is saved paused and enabled after review. Pausing
records and discards newly delivered occurrences; accepted work continues. Queue
expiry is measured from the original scheduled time. Skip-missed ignores signals
over 30 seconds late; deliver-before-expiry admits every still-current signal.
There is no fabricated cloud next-run time or cloud resource provisioning claim.

Gateway operation mappings may use installation-authorized persistent endpoints.
A run reads fresh local parameters and source files, then follows the existing
verified sequential acquisition planner. Each operation runs at most once per
occurrence; a crash during acquisition is recorded as interrupted and never retried
automatically. All source profiles are preflighted before the first call. Receipts,
arguments, adapter pins and downstream generated-value permissions are rechecked.
Interactive picker grants are not converted into persistent file permissions.
This first path uses installation-owned operations, not delegated account grants:
the owner installs the endpoint/profile binding and explicitly enables the frozen
job request. Allowed tools and typed arguments constrain invocation, but a receipt
does not prove that an arbitrary MCP tool is read-only. The installation must limit
the adapter and upstream account accordingly. Delegated standing file grants still
require separate Gateway work; they are not advertised as supported.

Google relay deployment is prepared for review, not applied to an account. It
requires authenticated Cloud Run invocation, a dedicated publisher service account
and a dedicated topic/subscription. Scheduler identity headers alone are not
credentials. AWS and Azure can later implement the same occurrence-signal contract.

References:
- https://docs.cloud.google.com/scheduler/docs/overview
- https://docs.cloud.google.com/pubsub/docs/subscriber
- https://docs.cloud.google.com/pubsub/docs/authentication

Validation: duplicate delivery/restart, changed payload, stale/future signals,
paused trigger, missing connection, bounded queue, uncertain acquisition, forged
receipts, redirect refusal, UI review and local fake-cloud end-to-end checks.
