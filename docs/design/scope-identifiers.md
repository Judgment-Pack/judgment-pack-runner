# Scope identifiers

Status: built for #237. Nothing in it changes how a run executes, how its
input is prepared, or how the Runtime is invoked.

A store has one owner. Until this change it had no way to group its jobs,
runs and journal entries by the party they are run for, so a service running
jobs for several parties could list a party's records only by keeping its own
index of ids. A **scope identifier** is an optional, opaque label fixed on a
job when it is created. The job's triggers, occurrences, runs and journal
entries carry it; the store-wide listings can be filtered by it; and a request
that names the job, or one of its records, under another scope than the job's
is refused. A job created without one is the job it always was, and every
record about it is written byte for byte as before.

**A scope identifier is a label and a filter, not an authorization
boundary.** Every request the control API admits holds the owner bearer, or
in [service mode](service-mode.md) an identity token for a listed subject,
and each may do everything the API offers: name any scope, or list every
scope's records by naming none. Holding a caller to the scopes it may name is
not part of this change.

## 1. The form

A scope identifier is a JSON string, or a query parameter's value, of 1 to
128 bytes of UTF-8 that holds none of these code points, as Unicode 15.0.0
assigns them: `General_Category=Cc` (tab and line feed among them),
`Bidi_Control`, `Default_Ignorable_Code_Point`, `Co` (private use), `Cn`
(unassigned) and `Noncharacter_Code_Point` (`isScopeIdentifier`,
`internal/runner/scope.go`). It is opaque: Runner compares it as written, byte
for byte, and never folds its case, trims it or normalizes it, so two scope
identifiers a person would read alike (a letter and its confusable, a composed
and a decomposed character, `Scope-A` and `scope-a`) are two scope
identifiers.

**This is the gateway's rule for its own scope identifier** (its `SPEC.md`
§1.2a and `go/scope.go`: its display policy 1, with tab and line feed refused
as well), so that one identifier is taken by both. Runner writes it again
with the standard library rather than sharing code: the classes are the
standard library's tables, `Default_Ignorable_Code_Point` is derived from them
as `DerivedCoreProperties.txt` derives it, and `Cn` is a code point in no
other category. A test holds the standard library to Unicode 15.0.0, so that a
toolchain whose tables move fails it rather than move the rule. (In 15.0.0
every `Bidi_Control` code point is also `Default_Ignorable_Code_Point`, and
every noncharacter is also `Cn`; both are named, as the gateway names them, so
that the rule reads as its does.) **The two
rules are meant to stay the same:** a rule changed in one is changed in the
other. The table tests in `scope_test.go` mirror the gateway's corpus cases:
valid, empty, at the bound, past the bound, a control character, tab, line
feed, a zero-width space, a bidirectional override, private use, a
noncharacter, a code point unassigned in 15.0.0, null and a value that is not a
string.

A scope identifier out of form is refused with `400`
`invalid_scope`, whose message says what the form is and repeats nothing of
the value.

## 2. Where it is fixed

`POST /v1/jobs` takes an optional `scope` member beside `name`, `releaseId`,
`reviewed` and `trigger`. Absent, the job has none. Written `null`, empty, not
a string, or out of form, it is refused as `invalid_scope` (a body whose
`scope` is not a string is refused as `invalid_request`, as any member of
another type is), and the refusal is journaled as any refused creation is.
A body is refused as `invalid_scope`, too, when a member's name only folds to
`scope` (`Scope`, `SCOPE`, `ſcope`), alone or beside `scope`: the JSON decoder
would read it as `scope`, and a reader that looks for `scope` as written must
never read another value than Runner does. A `scope` written twice is refused
as `invalid_request`, as every duplicate member is.

The scope is fixed on the job there and nowhere else. A repeated creation from
the same release, which returns the job it made, must name the same scope as
well as the same name and trigger, or it is refused `409`
`release_already_used`, as one naming another name is. A Runner held to the
manual-admission gate refuses a scope, as it refuses a trigger
(`manual_unsupported`). `GET /v1/status` lists the capability
`scope-identifiers`, so that a client can tell a Runner that takes one from one
that would refuse the member as unknown.

**A job's scope cannot be changed or dropped.** No request member sets it
after creation: an upgrade's body holds no `scope` (one that names it is
refused `400` `invalid_request`, as every unknown member is), the upgrade
keeps the job's record and its scope, and an upgrade request must itself name
the job's scope (section 3). The job's creation and revision records,
`job_creations` and `job_revisions`, carry it as the job does.

## 3. How a request names it

A request names a scope identifier in one way on every route: the query
parameter `scope`, percent-encoded UTF-8, decoded as
`application/x-www-form-urlencoded` decodes it (so `+` is a space). A query
parameter rather than a body member, because it is the one carrier every route
has, a read as well as a write; the store-wide listings already take their
filters there; and a run's body is the input whose digest its
`Idempotency-Key` is held to, which a scope must not change. A `scope` named
twice, empty or out of form is `400` `invalid_scope`; so is a query that does
not parse when one of its pieces names `scope`, so that a malformed scope is
never read as none.

Each route reads it before it reads anything else of the request
(`Service.scoped`):

- **A route that names a job, a run, a trigger or an occurrence** in its path
  (the job's, else the run's, else the trigger's, else the occurrence's), or
  `GET /v1/events` with `job` named once, not empty: the record must carry the
  scope named, or none when none is named. (A `job` named more than once, even
  as the same id, names no record: the route refuses it, `400`
  `invalid_filter`, whatever the ids; one named empty is none.) So:
  - a scoped job is addressable only by a request that names its scope;
  - a request that names a scope for a job created under none is refused;
  - a request that names another scope is refused.

  The refusal is the route's own answer for a record the store does not
  hold: Runner hands the route the request with an id no record carries in
  place of the one it named, so that the route answers it in its own order,
  byte for byte, and changes and journals nothing, as it does for that
  request. That is `404` `not_found` ("This job, release or run was not
  found.") on most routes; `401` `invalid_trigger_token` on the two a trigger
  key opens; `404` `upgrade_not_completed` on the upgrade receipt's; and, on a
  route that refuses a request for some other fault before it looks the
  record up, that refusal. It names neither identifier. A record the store
  does not hold is answered by the route as before; a store that cannot say
  which scope a record carries is answered `500`, and the request is not
  served.
- **The store-wide listings**, `GET /v1/jobs`, `GET /v1/runs` and
  `GET /v1/events`: the scope is a filter (section 5).
- **Any other route** (`POST /v1/jobs`, previews, inputs, profiles, status,
  the chain of runs and the rest) refuses a `scope` with `400`
  `invalid_scope`, rather than answer as if none were named: a new job's
  scope is its body's member.

**What a caller can tell apart.** A request for a record under another scope
is answered, byte for byte, as the same request for a record the store does not
hold, so its answer does not say whether the record exists; the one lookup of
the record's scope before the route runs is all that differs, in time. That
choice is made for the answer's sake, not as a boundary: a store-wide listing
without a filter serves every job with its scope to any request it admits.

**Paths with no caller inherit the job's scope.** A schedule falling due, a
missed schedule's catch-up, a watched file changing, a cloud signal
arriving, the dispatcher starting, ending or expiring a run, a restart
interrupting one, a preparation advancing an occurrence: each writes the scope
of the job whose record it changes, and needs none named. A workflow's
evaluation names no job and so no scope. A request that starts such work (an
event or a record delivery, a cancel, a reconcile) names the scope as every
request does.

## 4. What carries it

The scope is written as the member `scope`, present only when the job has
one:

| Record | Where the member is set |
|---|---|
| a job, its creation and its revisions | `createJobBy`; an upgrade keeps the record |
| a trigger and its revisions | `createJobBy` (the first trigger), `configureTriggerBy` (a new one); every later write keeps the record |
| an occurrence | `newOccurrence`, which every kind of occurrence is made by: schedule, catch-up, file, cloud, event and record delivery |
| a run | `submitBy`, which every run is queued by, from the job it reads in the same transaction: run now, a trigger's run |
| a journal entry about the job | its writer (below) |

A run is served with `scope` by its routes and in run lists, and kept in its
record, as `interruptedAt` is; like that member it is **not part of a
verification export**, whose readers decode strictly, nor of the run's entry
in the chain of runs, which this change leaves as it was.

**Journal entries.** Every entry that names a job created under a scope
carries the member `scope`, whatever its kind and whoever it names in `by`:
`job.created`, `job.upgraded`, `trigger.configured`, `trigger.paused`,
`trigger.resumed`, `trigger.key-rotated`, every occurrence's and run's change
of state (`runChange`, `occurrenceChange`), and `admission.refused` for a run,
an event delivery or a cloud signal bound to the job. An entry that names no
job carries none: Runner's own, a preview, and a refused creation, whose job
does not exist. Following the journal's rule that a member added later raises
`entryVersion` ([activity journal](activity-journal.md), section 5), **an
entry that carries a scope has `entryVersion` `"3"`**, a caller's or not,
since an entry of `"1"` or `"2"` never holds the member. Every other entry
keeps the version it had: `"1"`, or `"2"` for a caller's.

## 5. Listings

`GET /v1/jobs?scope=`, `GET /v1/runs?scope=` and `GET /v1/events?scope=`
answer only the records that carry exactly the scope named, compared byte for
byte (SQLite's `BINARY` comparison of the member as stored: no prefix
matches, no case is folded). They combine with each route's other filters:
`q`, `state` and `review` on runs, `job` on the journal (whose answer then
holds the job's entries that carry the scope, and not Runner's own, which carry
none). Without `scope` each lists everything, as before.

A job's own routes (`/v1/jobs/{job}/runs`, `/events`, `/triggers`,
`/occurrences`) name the job under its scope (section 3) and answer the job's
records as they always have.

The filter reads the member inside each record, as `q` reads a name: a
listing walks the table from its newest row and stops at a page of matches, so
a scope with few records among many reads further. That is the cost of a
filter without a column of its own, the same as a search's.

## 6. The store, and going back

No table or column is added: the scope is a member of the JSON records the
store already keeps (`jobs`, `job_creations`, `job_revisions`, `triggers`,
`trigger_revisions`, `occurrences`, `runs`, and the journal's `detail`), and a
record written without one is byte for byte the record an earlier Runner
writes. A store in which no job was created under a scope stays at schema
`"3"`, unchanged.

**A store's first job under a scope raises its schema to `"5"`**, in the
transaction that creates the job (`raiseScopedSchema`); this Runner opens a
store at `"3"` or `"5"` alike and migrates nothing. (`"4"` is the schema of a
store prepared for the manual-admission gate, which a Runner without that gate
still refuses.) The raise is made because
an earlier Runner would not merely ignore the member: it would serve a scoped
job's records to any request, and drop the member from every record it
rewrites (a run it dispatches, an occurrence it advances, a trigger it pauses,
a job it upgrades), so that the job and its records would be read afterwards
as created under none, and addressable without naming a scope. Every earlier
Runner refuses a store at a schema not its own, so it refuses such a store
from then on. **Going back** from a store that holds a scoped job needs a copy
of the store from before that job was created; a store that holds none is
opened by an earlier Runner as before. A store at `"5"` cannot be prepared for
the manual-admission gate, whose preparation takes only a store at `"3"`.

## 7. Idempotency and the caller

An `Idempotency-Key` stays held within its requester's runs of a job
([service mode](service-mode.md), section 7). The scope identifier is not
mixed into it: keys are held per job, and a job has one scope, so no two scopes
share a key's place.

The scope is not a caller. A caller, or the installation, names a scope on a
request; the journal names the caller in `by` and the job's scope in `scope`,
apart.

## 8. Where Runner differs from the gateway

- The gateway's operator fixes a scope on a platform in its configuration;
  Runner's requester fixes one on a job when it creates it.
- The gateway reads the scope from its request body (`/acquire`); Runner
  reads it from the query parameter `scope` on every route.
- The gateway signs the scope into a receipt; Runner's records and entries are
  the operator's own, unsigned, and its verification exports and chain of runs
  do not carry the scope.
- Runner refuses a scope named on a route that takes none.

## 9. Not in this change

- Holding a caller to the scopes it may name, or any permission that differs
  between callers or scopes.
- A scope on a release, a preview, a profile or a workflow.
- A column or an index of its own for the scope.
- Desk naming a scope: Desk names none, and a store whose jobs have none is
  served to it as before.
