# Adding installation input profiles

The private Runner control API accepts `POST /v1/input-profiles` with one complete
`InputProfile`. This configures installation authority; a catalog entry, imported
fixture, model response, or browser request cannot establish that authority.
The host chooses and validates its connection, adapter, key, class, capabilities
and model before registering a profile. Registration neither starts acquisition
nor authorizes it. The existing bearer and browser-Origin guards apply.

Registration adds an immutable definition. A new profile returns 201 and its
canonical digest; an identical retry returns 200 and that same digest. An existing
ID with different contents returns 409. A host changing a model, adapter or key
uses a new profile ID and reviews a new binding. No request can remove an older
profile, rewrite a release, change an existing binding, or restart a worker.

The SQLite metadata store retains registered definitions before the API exposes
them. Readers take deep snapshots of atomically published definitions without taking a
registration lock, including when an upgrade already holds a database transaction. Registration is serialized, additive and
idempotent, so a lost response can be checked by repeating the exact registration.
A failed write exposes no new profile. Restart validates retained definitions and
combines them with bootstrap profiles; an ID collision with different contents
refuses startup. Bootstrap configuration retains its existing semantics, but it
cannot override a profile explicitly registered and retained by this API.

The combined installation still admits at most 16 profiles, using the existing
profile validator. Reaching the limit refuses additions without evicting previous
profiles. This bounded first implementation deliberately has no automatic garbage
collection or replacement. Those need a separate reviewed lifecycle, including
references from saved releases and jobs. It also does not add background Gateway
connections: research remains caller-acquired, and automatic operation acquisition
still requires configured connections. On restart those connections can reference
registered profiles without duplicating their definitions in bootstrap configuration.

Saved releases retain precisely the profiles they used. New profiles are available
to input planning, preparation and release creation immediately; existing release
checks and offline verification use their retained definitions. The store's new
metadata key is additive: an older Runner does not load these profiles and refuses
a new binding it cannot satisfy rather than substituting another profile.

Acceptance covers authenticated API refusal, exact forms, bounds, independent
caller/snapshot mutation, concurrent idempotency, restart, bootstrap conflict, real
Runtime preparation and release-pin preservation. This is a prerequisite for Desk
fresh portable research (#423), not an AI executor or a workflow implementation.
