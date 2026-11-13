---
status: proposed
date: 2026-11-13
deciders: the Runner's maintainers
---

# A second storage kind on PostgreSQL, behind the whole store contract, with row security per scope identifier

## Context and problem statement

A Runner keeps its queue and records in one SQLite database under an exclusive process lock, and
its attempt directories and pinned Runtime on local disk, so it cannot restart on another host
with no durable local disk (issue #244). The store contract covers part of that state; jobs,
triggers, occurrences and most of the journal were kept beside it. Service mode needs a second
storage kind whose rows are held to the scope identifier each transaction is for, without
changing a byte of what the SQLite kind writes.

## Decision drivers

- SQLite stays the default kind, its schema and bytes unchanged.
- One contract every kind implements and one conformance suite every kind runs.
- No process-wide lock and no record cache in the contract or the new kind (several Runners on one
  store, issue #245, must not be made harder).
- The database conditions of #244: a database of its own, three operator-made roles, idempotent
  digested migrations that make no role and store no setting, no definer function, row security
  forced on every scoped table, no truncation or drop at run time, one call to remove a scope's
  rows.
- No refusal turned into a pass by state a restart on another host loses.

## Considered options

- Widen the store contract to everything a Runner keeps, then add a PostgreSQL kind behind it.
- Add a PostgreSQL kind for the contract as it stood, and keep the rest of the state on SQLite.
- Replace SQLite by PostgreSQL for every Runner.

## Decision outcome

Chosen option: "Widen the store contract to everything a Runner keeps, then add a PostgreSQL kind
behind it", because it is the only one under which one kind holds all a Runner needs to restart
elsewhere while the SQLite kind stays as it is; the second option still needs local disk, and the
third changes every installation.

The contract is widened first, in slices, each keeping the sqlite kind's statements and bytes
(`docs/design/store-interface.md`, section 9). The Runner's transaction functions are then made
safe to run twice, under a gate that runs every one twice in the in-memory kind, and the listings
put behind the contract. The kind is then built as `docs/design/store-postgresql.md` fixes it: one
schema owned by a role that cannot log in, reached by a migration login through `SET ROLE`, with
policies of its own for migrations and a migration that fails when a row statement changes no row;
a request login and a worker login with table privileges only, each route's transactions on the
login and scope the note names; row security enabled and forced on every table, keyed on a
per-transaction setting with a prefix that keeps an unset setting from reading as the none scope;
the Runner's rules across scopes held by keys and rows no policy narrows; writers committing one
at a time, so that the journal's sequence follows the order of commits; a lease per claim in place
of the process lock, its token fencing every write a claimant makes, by the database's clock; a
missing attempt directory taken as unknown, so that a retry across hosts fails closed; the pinned
Runtime rebuilt at start from shipped executables by digest; and the pgx driver through
`database/sql`. The note lists fifteen decisions (D1 to D15) with the options and a recommendation
for each; this record accepts the direction, and each decision is taken when the slice that needs
it is built.

### Consequences

- Good, because the sqlite kind and every record it writes are unchanged, and a byte test holds
  them.
- Good, because every new operation is tested on every kind by one suite, so the second kind is
  held to the same semantics.
- Bad, because a transaction function retried on serialization failure must start from copies of
  its inputs and read nothing but the store, its arguments and the clock, a rule every future
  transaction keeps; three do not yet, and are changed before the kind is written.
- Bad, because the second kind's writes serialize, one transaction at a time, as SQLite's do.
- Bad, because an interrupted run whose attempt directories the host does not hold cannot be
  retried; its caller submits a new run.
- Bad, because an image must carry every Runtime a held release names.
- Revisit when several Runners share a store (#245), or when a caller is held to the scopes it
  may name.

## More information

- Issue #244; `docs/design/store-interface.md` (the contract and section 9, the inventory);
  `docs/design/store-postgresql.md` (the second kind); `docs/design/scope-identifiers.md`;
  `docs/design/service-mode.md`.
- The form is the one the Runtime repository's decision records use.
