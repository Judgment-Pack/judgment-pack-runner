---
status: accepted
date: 2026-11-16
deciders: the Runner's maintainers
---

# Hold a caller to the scope identifiers its verified identity allows

Issue #304; the gateway's matching issue is its #479, which reads the same claim.

## Recommendation

**A caller's signed token names the scope identifiers it is for in one claim, `scope_ids`; the
service configuration names, per subject, the scope identifiers that subject may be granted; a
request is held to what both allow, and a token that names more than its subject's entry is
refused, not narrowed.** The marker `"unscoped"`, in a claim or an entry's list, names the
records under no scope identifier: a token reaches them only when its subject's entry grants them
and its claim names the marker, or it has no claim. A token with no claim reaches the unscoped
records only, is refused for an entry that does not grant them, and reaches the installation for
an `"installation"` entry. The installation's routes (`POST /v1/ticks`) and members
(`triggersOutsideSettings`) span every scope and are refused to every caller but the
installation's. The check is the default; `callerScopes: "unchecked"` keeps the behavior before
this change, and the start and `GET /v1/status` say so. The scope a store transaction is held to
is the one the checked identity allows, on every kind: on PostgreSQL the request login's
`jpack.scope`, on SQLite and the in-memory kind the engine's filter and the route's not-found.

Every decision below is marked C1 to C9 with its options; the recommendation is the first.

## Context and problem statement

A scope identifier ([scope identifiers](../design/scope-identifiers.md)) is fixed on a job when it
is created; the job's triggers, occurrences, runs and journal entries carry it, a request that
names one of those records must name its scope, and the store-wide listings filter by it. It is a
label and a filter, not an authorization boundary: every request the control API admits (the
owner bearer, or in [service mode](../design/service-mode.md) an identity token for a listed
subject) may name any scope identifier, or list every scope's records by naming none. On the
PostgreSQL kind row security holds each request transaction to the scope the request names
([store-postgresql.md](../design/store-postgresql.md), section 6, D7), but which scope a caller may
name is not decided there. So a caller that holds a valid token for one party's work can read and
change every other party's.

The premise was checked on main before this record: `Service.scoped` (`internal/runner/scope.go`)
reads the scope from the query alone, and `(*Identity).check` (`caller_identity.go`) returns the
issuer and subject and nothing else; no route consults the caller.

## Decision drivers

- One claim name and form for the Runner and the gateway, so that one issuer serves both.
- Two keys: an issuer cannot widen a caller's access on its own, and an operator's configuration
  does not grant a scope to a token the issuer did not make for it.
- A mis-issued token is visible: refused, never silently narrowed.
- Fail closed: a configuration that says nothing holds callers to the least, and keeping the old
  behavior is a word the operator writes.
- A refusal discloses nothing: a request about another scope's record is answered as one about a
  record the store does not hold, byte for byte, as today.
- The scope a PostgreSQL transaction is held to comes from the checked identity, never from the
  request alone.
- Nothing changes for the owner bearer (a companion, or service mode without `identity`), nor a
  byte of what either kind stores.

## C1. The claim

(a) **`scope_ids`, a JSON array of 1 to 256 strings, each a scope identifier in the existing form
(scope-identifiers.md, section 1), each once; or absent.** (b) A space-separated string, as OAuth's
`scope`. (c) A claim per scope (`scope_ids.a: true`).

(a) holds exactly what a scope identifier may be: (b) cannot carry a scope identifier with a space
in it, and the OAuth `scope` claim is already used for other things by most issuers. (c) puts a
caller's data in claim names. Read in (a), a claim that is present and not of the form (`null`, not
an array, empty, more than 256, a member that is not a string or not in form, a scope identifier
twice) refuses the token, `401`, with a reason that repeats nothing of the token. The claim is read
only after the signature, issuer, audience, expiry and subject checks pass, as every claim is.
One value is reserved: `"unscoped"` in the claim names the none scope (C4), not a scope
identifier; every other string of the form is a scope identifier.

## C2. The configuration

(a) **`identity.callerScopes` in the service configuration: an object whose names are subjects
the identity lists and whose values are, for each, an array of 1 to 256 scope identifiers (each
once) it may be granted, which may name the marker `"unscoped"` for the records under none;
`"unscoped"`, the none scope alone; or `"installation"`; or the string `"unchecked"`; or
absent.** (b) A member per issuer with a subject pattern. (c) A file of its own.

The identity has one issuer and lists its subjects exactly (service-mode.md, section 4); it has no
subject pattern to extend, so the entry is per subject. A name that is not a listed subject is
refused at start, as is an entry of any other form, with a sentence that names the member and
repeats no value. A listed subject with no entry is `"unscoped"`. **Absent, the configuration is
checked**, every subject `"unscoped"`. Without `identity` the member does not exist: the owner
bearer is the installation's.

## C3. What a token reaches

With `S` the claim's members and `E` the subject's entry (`"unscoped"` granting the none scope
alone, a list granting its scope identifiers and, when it names `"unscoped"`, the none scope,
`"installation"` granting every one), `"unscoped"` in either standing for the none scope:

| Claim | Entry | The request is held to |
|---|---|---|
| absent | `"installation"` | The installation: every scope, the store-wide listings unfiltered, and the installation's routes, as the owner bearer. |
| absent | `"unscoped"`, none, or a list naming `"unscoped"` | The none scope: the records created under no scope identifier. |
| absent | a list not naming `"unscoped"` | Refused, `401`, as a claim wider than the entry: the token would reach the none scope, which the entry does not grant. |
| `S`, every one granted by `E` | any | Exactly `S ∩ E`, which is `S`: its scope identifiers, and the none scope only when `S` names `"unscoped"`. Not the installation's routes. |
| `S`, one not granted by `E` | any | Refused, `401`, before any route: "the token's scope_ids names a scope identifier the configuration does not grant its subject". |

The intersection is computed, and a claim wider than it refused, rather than the claim taken:
the issuer cannot grant what the entry does not, and the entry grants nothing the token was not
made for. The entry, not the issuer, decides whether a subject reaches the none scope: a claimless
token, the default of most issuers, reaches it only for a subject the operator granted it. "Refused, not narrowed" keeps a mis-issued token from working in part, which an operator
would read as a Runner that lost records. The refusal is not journaled, as no identity refusal is
(service-mode.md, section 4): it comes before any route.

**C4. Whether a token reaches the none scope.** (a) **Only when its subject's entry grants it
(`"unscoped"`, or a list naming `"unscoped"`) and its claim names `"unscoped"` or is absent; a
claimless token of an entry that does not grant it is refused.** (b) Every token reaches the none
scope beside its claim's. (c) A token with a claim does not, and a claimless one does, whatever
the entry (as first built).

On the PostgreSQL kind the none scope is a scope like any other (`s:`, section 6 of the store
note), and on every kind it holds records callers make: a job created without a scope, its runs,
triggers and entries. Under (b) the none scope is a commons every caller of every party reads and
changes, so a caller of party A that forgets the scope on a creation puts A's job, its inputs and
its runs where party B's caller reads them, and every party reaches the installation's own
unscoped jobs. Under (c) the issuer alone decides it: a claimless token of a subject granted only
A reaches every unscoped record, jobs made before scopes existed among them. Under (a) a caller
of A never reaches a record outside A unless the operator's entry and the issuer's token both say
so; a subject that needs both is issued one token naming both. The gateway's unscoped sources differ: they are its operator's, configured,
not made by callers (see "The gateway", below).

**C5. The installation's entry.** (a) **`"installation"`, reached by a token with no claim; a
token of that subject that carries a claim is held to its claim.** (b) A reserved claim value.
(c) None: no identity caller reaches the installation's routes.

(b) would reserve a second value (C1 reserves `"unscoped"` alone) and let the issuer grant the
installation. (c) leaves service mode with identity no way to call `POST /v1/ticks`, since the
identity replaces the owner bearer (service-mode.md, section 5). Under (a) a token with a claim is narrowed by its issuer as every
other token is; a claimless token of an `"installation"` subject is the one credential with the
owner bearer's reach, and its entry is the operator's word for it.

## C6. The routes

Every route is served through `Service.scoped`, which reads the scope a request names before the
route does (scope-identifiers.md, section 3). It now reads the caller's reach (C3) first. For a
caller held to scopes (every row of C3 but the first):

- **A route that names a job, a run, a trigger or an occurrence** (in its path, or `GET
  /v1/events` with `job`): the scope named, or the none scope when none is named, must be one the
  caller reaches. If it is not, the request is handed to its route naming an id no record carries,
  as a request naming another scope than the record's is today, and the record's scope is not
  read: the answer is the route's own for a record the store does not hold, byte for byte (`404
  not_found` on most routes, `401 invalid_trigger_token` on the two a trigger key opens, `404
  upgrade_not_completed` on the upgrade receipt's), and nothing is changed or journaled. Reads and
  writes alike: that is how every write on these routes already answers another scope's record.
  The families: a job and its cursors, runs, run now, triggers and the trigger preview, an
  upgrade and its receipt, briefs, occurrences, events and deliveries; a run, its verification,
  diagnostics, retry and briefs; a trigger's state, key, events, deliveries and occurrence; an
  occurrence's cancel and reconcile; the journal naming a job; the manual execution binding; and
  the retained-column paths of these.
- **The store-wide listings**, `GET /v1/jobs`, `GET /v1/runs`, `GET /v1/events` without `job`:
  held to the scope named, if the caller reaches it, or, naming none, to the none scope, if it
  reaches that; otherwise answered as a listing of a scope that holds no record (an empty page).
  A held caller never reads the store for every scope. **C7.** (a) **An empty page.** (b) `403`.
  (a) is the listing's not-found, and depends on nothing another scope holds; so does (b), but a
  client then tells "this scope is not mine" apart from "this scope holds nothing" on a listing and
  not on a record.
- **`POST /v1/jobs`**: the scope its body names, or the none scope, must be one the caller
  reaches; otherwise `403 scope_not_allowed`, "This caller's identity does not allow a job under
  the scope identifier named, or under none when none is named.", after the body is read and its
  scope held to the form, before anything is read from the store, and not journaled (a `403` never
  is: `refused`). The creation names no record, so the record routes' not-found does not apply; a
  refusal that depends on no stored record discloses nothing, and `404` would say the release is
  not found when it is.
- **`POST /v1/ticks`** ticks every scope's shared-interval triggers: `403
  installation_caller_required`, "This route acts for every scope identifier: it is served only to
  the installation's caller.", before the scope parameter or the body is read, not journaled.
- **`GET /v1/status`** is served to every caller (a client reads its capabilities there), but
  `triggersOutsideSettings`, which lists the trigger ids of every scope, is left out for a held
  caller. **C8.** (a) **Refused: the ticks `403`, the member left out.** (b) Filtered to the
  caller's scopes. The issue offers either; (b) makes a held caller's tick tick only its scopes'
  triggers, which changes what a shared tick means for every Runner of a store and what an
  external scheduler's one call does, and filtering the member would need the triggers' scopes
  kept beside the ids. (a) changes neither.
- **Every other route** takes no scope identifier and reads no scoped record: previews, inputs,
  the input and selected profiles, the background connections and the chain of runs. Each is
  served as before. They are the installation's shared records (releases, profiles, the input
  root, the chain: store-postgresql.md, D9 and D10, and #279), and a caller of one scope reads and
  adds to them as it does today. A deployment whose parties must not share them gives them
  separate installations, as the store note says for the input root.

Paths with no caller (a schedule, a file trigger, a cloud signal, the dispatcher, a restart)
inherit the job's scope as before; a caller's reach decides only what its own requests name.

## C9. The default

(a) **Checked: a configuration without `callerScopes` holds every subject to the none scope, and
`"unchecked"` is written to keep the behavior before this change.** (b) Unchecked unless
configured. (b) would let a configuration written before this change, and every one that forgets
the member, keep every caller at every scope. Under (a) an upgrade with `identity` and no
`callerScopes` holds each caller to unscoped records, and `POST /v1/ticks` answers `403
installation_caller_required` for every subject until a subject has an `"installation"` entry, so
an external scheduler's shared ticks stop: the release notes say to write the entries (an
`"installation"` entry for the scheduler's subject), or `"unchecked"`, before upgrading. With `"unchecked"` the claim is not read at all, every caller
reaches what the installation reaches, the start writes on standard error that caller scopes are
unchecked, and `GET /v1/status` says `"callerScopes":"unchecked"` (`"checked"` otherwise; the member
is absent without `identity`). Without the member the start writes on standard error that caller scopes are
checked and every subject, having no entry, is unscoped: a token carrying `scope_ids` is then
refused `401`, records under a scope identifier read as absent (an event delivery to a trigger
under one gets the unknown-trigger answer), and `POST /v1/ticks` is refused `403` (#308).

## The store

**PostgreSQL.** `Service.scoped` holds each request's view of the store (`heldTo`) to the scope
the caller's reach allows: the scope named when the caller reaches it, the none scope when it
names none and reaches that, and otherwise `unheldScope`, a value no record carries (a control
character, outside the scope form), whose `jpack.scope` (`s:` and that value) reaches no scoped
row and fails every write's policy check. A held caller's request is never given the store for
every scope (the worker login), so a listing naming no scope no longer reads every scope for it.
Two transactions were held to a scope taken from the request: the creation's, to the body's
scope, which is now checked against the reach before the transaction opens; and the upgrade's,
to the query's scope, which now reads the scope `Service.scoped` held the request to. Row security
is unchanged and still holds each transaction to the scope it was set to; what changes is that the
Runner sets it from the identity.

**SQLite and the in-memory kind** apply the same rule in the engine: `Service.scoped` hands the
route the substituted id, and a listing gets the filter the reach allows. A listing held to the
none scope keeps exactly the records that carry no scope (`recordFilter.NoScope`,
`journalFilter.noScope`, held by every kind and by the conformance suite), where naming no scope
kept every record before. No table, column, migration or stored byte changes; the store's schema
stays where it is.

## The Runner as a caller of the gateway

Today the Runner's gateway calls (`/acquire` from `background.go`, `paging_loop.go` and the source
worker) carry no identity token and no scope identifier. When the gateway holds its callers to
scopes (#479), the Runner is one caller acting for several scopes, and should be admitted so:

- The Runner presents a token of its own, for its own subject, never a caller's token: a caller's
  token is checked by the Runner and kept nowhere (service-mode.md, section 4).
- For a run of a scoped job it names the job's scope identifier on `/acquire`, and none for an
  unscoped job. The job's scope was fixed when a caller held to that scope created it (C6), so
  the Runner names only a scope some caller was allowed.
- The gateway's configuration has an entry for the Runner's subject listing the scope identifiers
  that Runner serves, 1 to 256, and the Runner's token carries `scope_ids` naming them; a claim
  wider than the entry is refused there as here. A Runner that serves more than 256 scope
  identifiers needs a form the gateway does not have yet, as `"installation"` is here, or
  separate subjects per Runner.
- The gateway adopts C4's rule (a) (#479): its unscoped sources are reached only by a subject
  whose entry names `"unscoped"`, with a claim that names it or none. The Runner's token then
  names `"unscoped"` beside its scope identifiers to serve its unscoped jobs, and the gateway's
  entry for the Runner's subject names it too.

The Runner's side of this (a token file on a gateway connection, the scope on each call) is a
change of its own, after #479 fixes the gateway's rule; nothing here builds it.

## Where the Runner differs from the gateway

- The Runner's none scope holds records callers make; the gateway's unscoped sources are
  configured by its operator. Both reach them by C4's rule (a).
- The Runner has installation routes and an `"installation"` entry (C5); the gateway has neither.
- A request about another scope's record is answered as one about a record the store does not
  hold; the gateway answers a request naming a scope its caller may not use with its fixed wrong-scope
  refusal.

## Tests

- Each route family of C6, table-driven over every route that names a record, for a caller of
  scope A asking about scope B's records and about the none scope's: the answer is, byte for
  byte, the one for a record the store does not hold, and nothing is journaled; and about A's
  records, served.
- A claim wider than the entry, and a claim out of form, refused `401`; a token with no claim
  reaches the unscoped records only, and is refused `401` for an entry of scope identifiers that
  does not name `"unscoped"`; a claim naming `"unscoped"` beside an entry's scope identifiers is
  refused unless the entry names it, and reaches both when it does; `"installation"` with no claim reaches every scope and the
  ticks; with a claim, it does not.
- The listings, the creation, the ticks and the status member, as C6 says.
- `"unchecked"` serves every caller as before, and is reported at start and in the status.
- The PostgreSQL kind: a held caller's requests reach rows only through a view held to its reach,
  never the store for every scope, against a server (`TestPostgreSQL…`); the in-memory and SQLite
  kinds through the conformance suite and the engine tests.
- Mutants: the intersection dropped (the claim alone trusted); the view held to the scope the
  request names instead of the one the identity allows; a held caller let through to
  `POST /v1/ticks`. As built, four more: the creation's reach check dropped;
  `triggersOutsideSettings` shown to a held caller; the sqlite kind's none-scope filter dropped;
  an absent `callerScopes` read as unchecked. Each of the seven fails the tests. After review, one
  more: a claimless token admitted to the none scope for an entry that does not grant it. No store
  transaction was added, so the run-twice suite is not part of it.

## Consequences

- Good: a caller is held to the scope identifiers both its issuer and the operator allow, on every
  route that names or lists a scoped record, with the answers it already gets for records the
  store does not hold.
- Good: the PostgreSQL kind's row security now holds a boundary the Runner chose from the
  identity, not one the request chose.
- Bad: an installation with `identity` and no `callerScopes` holds every caller to unscoped records
  after the upgrade, and `POST /v1/ticks` is `403 installation_caller_required` for every subject
  until one has an `"installation"` entry, until the operator writes the entries or
  `"unchecked"`. The release notes say so. An earlier Runner refuses the member as one it does not read: going back means removing
  it.
- Bad: a subject that works in a scope and in the none scope needs `"unscoped"` in both its entry
  and its token's claim (C4), and a claimless token of an entry of scope identifiers alone is
  refused, so an issuer that mints claimless tokens by default needs the entry or the claim
  changed.
- Neutral: `"unscoped"` is reserved in a claim and an entry's list (C1), so a scope identifier
  spelled `unscoped` cannot be granted to a caller held to scopes; the installation reaches it.
- Neutral: a held caller still learns what the store note's D8 to D10 accept (the chain of runs,
  journal sequence gaps, the installation-wide counts and keys), and shares the installation's
  releases, profiles and input root; parties that must not, use separate installations. One more
  channel, accepted for now: a release's preview (`release.previewed`) takes no scope and is
  journaled under the none scope, naming its caller and the pack, whichever scopes the caller
  reaches, so a caller that reaches the none scope reads another scope's caller's previews. A
  refused creation is not such a channel (see "As built").

## As built

The build follows C1 to C9 as recommended (option a each time). Where each lives:

- The claim, the entry and the reach: `internal/runner/caller_scopes.go` (`readCallerScopes`,
  `reachOf`, `callerReach`); read in `(*Identity).admit` (`caller_identity.go`) after every other
  check of the token; the owner bearer's reach is set in `Service.handler` (`http.go`).
- The routes: `Service.scoped` (`scope.go`) refuses an installation route to every caller but the
  installation's before the scope parameter is read, holds the request to `unheldScope` in place
  of a scope its caller does not reach, substitutes the record id without reading the record's
  scope, and hands the listings `requestScope`; `POST /v1/jobs` and `GET /v1/status` in
  `http.go`; the journal listing in `eventsHandler`; the upgrade's transaction in
  `job_revisions.go`.
- The engine filters: `recordFilter.NoScope` and `journalFilter.noScope`, in the sqlite and
  postgresql kinds' queries and the in-memory kind, held by the conformance suite.
- The start's note: `cmd/jpack-runner/service.go`, from `(*Identity).CallerScopesNote`.

Two details the sections above leave open, as built:

- **The routes that name no record**, on the postgresql kind, for a caller that does not reach
  the none scope, run on a view held to `unheldScope`, as every other route of that caller does.
  They read no scoped row, and the installation's shared writes they make (a release's save, a
  profile's registration, a refusal's entry) are held to the none scope by their own
  transactions, as before; a test serves the chain of runs, the input profiles and the status so.
- **A refused creation** (`refusedJobBy`). The installation's, the owner bearer's included, is
  journaled under the none scope naming the release and the caller, as before. A held caller's,
  answered after the reach is checked, for a scope it reaches (`review_required`,
  `release_already_used`, …), is journaled under that scope, naming the release and the caller,
  so only that scope's callers read it. One answered before the body's scope is read (a body or
  scope out of form) is journaled under the none scope, naming the release and the caller only
  for a caller that reaches the none scope; for one that does not, the entry names neither: a
  caller kind, the request, its status and code, and the time. A claimless caller of the none
  scope thus reads no other scope's subject or release through `GET /v1/events`. Journaling
  fewer refusals would weaken the record of admissions, so each is still journaled.

## Not in this change

- Permissions that differ by route or by read and write within a scope.
- A claim or entry for a scope's subset of routes, or revocation before a token expires.
- The Runner's own token and scope on its gateway calls (above).
- Journaling an identity refusal.
