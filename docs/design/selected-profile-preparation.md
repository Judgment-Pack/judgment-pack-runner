# Selected-profile preparation

Status: Runner #191, for the Desk's selected retained-profile workflow
(Judgment-Pack/judgment-pack-desk#599; the question was #190). Contract
`selected-profile-preparation/1`.

## What it is

A host can hold the full ordinary registry of 16 installation profiles
([installed input profiles](installed-input-profiles.md)) and still prepare inputs for
an explicitly selected retained generated-input profile. The selected profile is
admitted to a separate namespace with its own bound. It never counts toward the 16
ordinary profiles, and the ordinary registry, store, executor and work in progress
are not changed by it.

## The same service and the same lease

The namespace is served by the same `Service` as every other route. It lives in the
same store, under the same executor lease and authority
([executor ownership](executor-ownership.md)). Nothing in it opens a store, takes or
releases a lease, or starts a worker. There is no second workspace and no second
executor owner. Nothing about the ordinary side is stopped, evicted or raised to make
room: the 16-profile ceiling is unchanged. Under manual admission, admission and
planning are refused as `manual_profile_unsupported`, as ordinary registration and
planning are.

## What a selected profile is

A selected profile is the retained-column producer profile (`columnProducerProfile`):
valid on its own under the installation's admission, and declaring the installed
`producer-carrier` capability. Any other profile is refused as
`selected_profile_unsupported`. Selection is not a way to hold more ordinary profiles.

It is immutable once admitted. An identical admission returns the same digest.
Changed contents under an admitted ID are refused as `selected_profile_conflict`.
The profile is kept for the store's life, and no request removes it. The two
namespaces never share an ID:

- admitting the ID of an ordinary profile (configured, registered or set aside) is
  refused as `selected_profile_ordinary`;
- registering an ordinary profile under a selected ID is refused as 409
  `input_profile_selected`;
- a configured profile with a selected ID refuses to open the store.

## The bound

At most **16** selected profiles, and their stored JSON array is at most **81920**
bytes. One past either bound is refused as `selected_profile_limit`, and no profile
is evicted. The reasons:

- A mapping names at most 16 sources, and a retained preparation is planned under at
  most 16 profiles. So 16 selected profiles cover the profiles of any one retained
  preparation, and the bound does not narrow that domain.
- A retained preparation is refused when the profiles it is planned under exceed
  `profilesBytes` (81920). A namespace larger than that could not be planned under at
  all.

## Preparation only

`POST /v1/retained-column/1/selected-inputs/next` runs next-input planning under the
selected profiles alone. It prepares and checks the supplied input and returns either
the next source to acquire or the complete prepared input. It acquires no source,
calls no network, evaluates nothing and persists nothing: every table of the store is
the same after it as before. The namespace has no
release, job or run route. A selected profile is never bound outside this route:
every ordinary binding resolves through `refuseSetAsideIDs`, which refuses a selected
profile as 422 `input_profile_selected`. That covers planning, preview, import,
release creation, job creation and upgrade, and a run's submission and start. A run's
submission whose mapping names a selected profile is refused by that name before the
job, its release or the mapping's comparison with the release is read. Only an
identical retry under an idempotency key already used is answered first, with the run
first accepted, as it is after a profile was set aside. In the other direction,
selected planning refuses an input that names an ordinary profile as
`selected_profile_ordinary`. The route keeps no state per request, so a cancelled
request leaves nothing to clean up.

## Routes and the capability report

The three routes follow the retained-column adapter's style. Each is served only under
`/v1/retained-column/1/`, after bearer authentication and the browser-Origin refusal.
Each needs one `JPack-Conversion-Contract` selector that names the exact installed
contract with the roles `document-validation`, `preview`, `preparation` and
`producer-carrier`. The selector is checked before the body is read. Without the
prefix, a route is refused as `selected_preparation_unselected`. The planning body's
`source.conversionContract` must equal the selector, so the selection is carried
through the preparation.

- `GET /v1/retained-column/1/selected-profiles` returns the capability report: the
  contract, the bound (`profiles`, `profilesBytes`) and each selected profile with its
  digest.
- `POST /v1/retained-column/1/selected-profiles` admits one profile (at most 32 KiB):
  201 when new, 200 when identical.
- `POST /v1/retained-column/1/selected-inputs/next` plans and prepares, as above.

`GET /v1/status` names the capability `selected-profile-preparation`. The pre-start
probe (`jpack-runner capabilities`, [runner capabilities](runner-capabilities.md)) is
unchanged. It is a closed, versioned boot contract, and this namespace adds no boot
requirement. `openapi.json` documents the routes, and
`vectors/selected-profile-preparation.json` is the handwritten public vector.

## The stored row

The namespace is stored under its own metadata key, `selected-input-profiles-v1`.
The ordinary row is not touched. Each admission writes the row synchronously before
the profile becomes visible. When the store opens, the row is checked, and a row
Runner did not write refuses to open the store. That covers a row over the bound, one
that cannot be read, one that is not an array of profiles in their exact form, an ID
stored twice, and an ordinary profile's ID. A stored profile that this Runner's
selection refuses on its own is set aside, as an ordinary registered profile is
([profile set aside](../../internal/runner/profile_set_aside.go)). It keeps its bytes
and its ID, counts toward the bound, is listed with its reason, and is refused as
`input_profile_set_aside` when planned. An older Runner does not read the key. It
refuses a binding to a selected profile as one the installation does not hold.
