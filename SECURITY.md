# Security policy

## Support status

This is a **research preview**: a single-owner, local jobs pilot. It is pre-1.0 and provides no
security, compatibility, or support service-level guarantee. It must not be used as the sole control
for consequential production decisions. A retained run records what the pinned runtime answered for
the inputs it was given. It is not a statement that those inputs were true, and it approves no
external action.

## Reporting a vulnerability

Do not open a public issue for a vulnerability that could:

- let a caller use the control API without the owner bearer, or let a trigger token do more than
  start a run of its own trigger;
- disclose the owner bearer, a trigger token, a worker token, or a credential;
- make the runner accept a receipt, a release, or a mapped input it should refuse;
- start a program other than the release's digest-pinned runtime; or
- read or write outside the runner's own storage.

Use this repository's [private vulnerability reporting](https://github.com/Judgment-Pack/judgment-pack-runner/security/advisories/new)
(Security → Report a vulnerability). If that is unavailable, open a minimal non-sensitive issue
asking a maintainer to establish a private channel. Include:

- a minimal synthetic reproduction;
- the affected command, endpoint or module and the commit;
- expected and actual behaviour;
- likely impact; and
- any suggested mitigation.

Never include real tokens, receipts, credentials, customer evidence, or private operational records
in a report. Synthetic fixtures only.

A limit this file or the [README](README.md) already states is not a vulnerability. An open issue
that asks for a stronger guarantee is the place to discuss it.

## Security boundary

Everything below describes what the runner holds to and, more importantly, what it does not.

**There is one local owner.** The pilot has no delegated roles, no shared-user permissions and no
access control beyond the owner bearer. Whoever holds that bearer can do everything the control API
offers.

**The control API is local.** The runner binds an ephemeral IPv4 loopback port and refuses direct
browser origins. Its bearer arrives on stdin at start. It is not printed, not passed as an argument,
not read from a project file, and not sent to the browser. The runner provides no public ingress,
no tunnel, and no transport security of its own.

**A trigger token is narrow by design.** It is random, shown once, and stored only as a SHA-256
digest. Rotation revokes the previous token immediately. A token cannot list jobs, inspect records,
edit triggers, or choose another release. A direct caller of the runner needs the owner bearer as
well as the trigger token. A `202` acknowledges an occurrence, not a completed decision.

**The evaluator is a separate program, pinned by digest.** A release retains a digest-pinned copy
of the runtime executable, and the runner runs it through its public command line. The runner
embeds no evaluator. It does not extend the runtime's conformance claim.

**Inputs are assertions unless a mapping verifies them.** Manual inputs and version 1 file mappings
are caller declarations. For version 1 the runner checks original hashes and the binding of a
receipt to its document, and does **not** cryptographically authenticate an API caller's receipt.
Version 2 mappings verify version 3 gateway receipts against operator-supplied profiles. Even a
valid receipt attests to acquisition, not to the truth of what was acquired.

**`verify-run` checks retained inputs, not the result.** It reports `verified-inputs`, not policy
truth or evaluator re-execution ([docs/MAPPING-V2.md](docs/MAPPING-V2.md)).

**A release whose tests never ran can become a job.** Such a release is labelled `not-run`, never
passed, and requires explicit review as untested. A release whose tests ran and did not pass cannot.

**Storage is local files.** Job artifacts, input snapshots and records are kept on the local
filesystem, with SQLite holding the queue and records. The source worker's database contains
requests and proof secrets. Anyone who can read those files can read their contents, so protect the
directory with the host's own permissions and keep it out of anything you distribute.

**Not in scope for this pilot:** availability, multi-user operation, hosted or public deployment,
external actions, graph execution, and agent loops.
