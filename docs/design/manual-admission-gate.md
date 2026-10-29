# Private manual admission gate

This is an experimental host contract, not adopted Policy Spec conformance or
portable authority. The sole profile is `local-admin-manual-pack/1`. Runner's
coordinator has enabled the reviewed uniform candidate for isolated
Desk/Runner/Runtime validation. Supported hosts report closed v3 with
governanceAdmission true scoped solely to this profile. Explicit trusted
enrollment, matching demands and fresh authority admission remain required;
the capability response grants no permission by itself. There is no environment
switch or alternate test capability protocol. Release/live activation remains
outside this candidate validation.

Desk retains execution demand independently of portable Runner data. Its fixed
`executors/policy-execution-v1/<machineWorkspace>` leaf is a sibling of the
workspace's pinned ownership directory. Runner validates the bounded private
directory and closed demand, including when admission configuration is omitted.
An empty/malformed directory holds; valid other-workspace demand preserves the
current ownership-only workspace. Current demand requires matching boot identity.

`prepare-manual-store` is a nonexecuting offline helper with closed stdin/stdout
and an intentional duplicate of Desk's continuously held external lease at fd3.
It never starts Open, Runtime, providers, recovery or a worker. See the adjacent
[wire fixture](manual-admission-store-prepare-wire.json) and
[shape](manual-admission-store-prepare-shape.md). It refuses all triggers, old
queued/running work and pending acquisition before publishing Runner schema4.
Legacy workspaces retain schema3. Its durable marker precedes SQLite publication;
recovered or partial preparation cannot automatically advance Desk enable.

Helper lock/marker/sync use the held state root. The dedicated helper process
retains that directory as cwd until exit and selects a private, nondefault clone
of SQLite's native VFS. Only `xFullPathname` changes: it accepts and preserves the
exact relative main filename `runs.sqlite`. Native open/delete/access, locking,
sync, journal and WAL/SHM callbacks retain relative paths under that cwd. Temp
storage stays in memory; unrelated database filenames refuse. The normal default
VFS is unchanged. Open and library callers never change cwd; the library adapter
uses a held descriptor path on Linux and refuses hosts without that path.

Configured placement and database inode are rechecked around connection opening
and durable boundaries. Subprocess regressions replace the named state after
adapter setup, observe actual journal/WAL/SHM in the retained original, and inspect
both stores after preparation refuses. These checks do not promise atomicity
against a hostile same-OS-user actor replacing database inodes between checks or
rearranging the entire trusted plane. Detected replacement holds; no normal start
repairs a marker/schema mismatch. Desk accepts only an original fresh committed
helper response, never recovered observation.

Governed startup validates the immutable boot and store demands, acquires actual
external ownership, owns/closes the inherited authority descriptor, fixes actual
signing, then activates the authenticated challenge before Runner recovery or
workers. No stopped configured service falls back to unowned execution. Ordinary
enabled Open refuses old schemas rather than upgrading restored queues.

The owner-only execution-binding export describes exact canonical Release bytes.
Desk's anchored authority owns registration, current eligibility, trusted intent,
logical-operation replay barriers and attempts. A private HTTP selector alone
grants nothing. Runner retains exact original request/facts/evidence bytes and
the selected resource/registration. Reserve binds those bytes and the existing
execution-payload descriptor; SQLite retains its exact attempt before commitStart.

Before commitStart, Runner verifies actual Pack/config/lock/facts/evidence,
Runtime, signing, current job/release tuple and external custody. Only the real
channel's original successful fresh reply can supply its sealed session/epoch
proof. Successful local start journaling precedes minting one in-memory launch.
The common operational starter consumes it, repeats actual-material/current-tuple
verification and delegates to the ownership-aware lease starter. Fabricated or
copied receipt DTOs, recovered history/reserve, lost replies, journal failures,
stale sessions/epochs, changed files and post-Close calls cannot launch or replay.

The current common starter explicitly allows only fixed utility command shapes
for validation, lock, saved checks, version/audit verification and rehearsal;
utilities retain ownership and do not become operational permits. Operational
evaluate, including alternate/private workflow callers, requires the original
one-use launch. Enabled workspaces globally refuse triggers, acquisitions,
SourceInput/Preparation/mapping, profile installation and upgrades before activity.
There is no mixed legacy-resource escape within an enabled workspace.

Fresh committed logical start is irrevocable: later withdrawal may precede its
physical spawn. Withdrawal before the anchored commitment blocks. This is not
an atomic ledger/OS transaction; failure or uncertainty after commitment remains
history without automatic replay. Exact ordering and host lifecycle behavior
require the combined real Desk/Runner/Runtime fixture before usable activation.

## Fences and the acceptance cases (runner #147)

The admission epoch is a fencing epoch in the lease vocabulary the agent's ADR-0025 fixes and
the Desk's store locks share: an owner, an authoritative expiry kept by the store clock, an
epoch, and the fence `{resource, owner, epoch}`. Here the resource is the registered
job/release tuple, the owner is the executor the authority activated (`executorId`), and the
epoch is the one its activation returned. The Runner reads an epoch only as a canonical
positive decimal string (the wire's counter domain, which contains ADR-0025's
1..999999999999999999); a JSON number is refused. A restart activates a new epoch, and a
reservation retained under the old one is never started.

`internal/runner/manual_admission_acceptance_test.go` holds the acceptance cases against the
real Runtime, each a test named for its case, all required by the `manual-gate` step:
omitted-dependency substitution, withdrawal/enqueue/dispatch races, lost admission response,
worker restart and fencing, identity revocation, and pre-withdrawal restore. No case needed a
new mechanic. The transport peer there stands in for the authority; it is not Desk Policy
authority.
