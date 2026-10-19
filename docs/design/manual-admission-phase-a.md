# Private manual admission: historical Phase A seams

Historical snapshot, 2026-10-19. The statements below describe the inactive
Phase A implementation. The completed candidate is described in
[manual admission gate](manual-admission-gate.md) and
[current capabilities](runner-capabilities.md); the Phase A hold below is no
longer the current supported-host capability/Open behavior.

This is private host machinery, with no adopted portable Policy conformance claim.
Production activation remains disabled. The capability probe is still the exact
closed v2 response with `governanceAdmission:false`; `Open` refuses every supplied
`executorAdmission` binding before state creation, ownership acquisition, recovery
or workers. Validating a staged binding is not capability support or permission.
No registration mutation route or usable execution-enrollment command is added.
Unconfigured Jobs retain their behavior.

The agreed [wire schemas and synthetic vectors](manual-admission-wire.json) and
[shape rules](manual-admission-wire-shape.md) bind a dedicated inherited Unix stream
socket at fd3. The inactive client snapshots trusted binding fields, requires one
fresh activation first, serializes exact sequence strings, enforces 64KiB frames
and five-second exchanges, and permanently closes on cancellation or protocol
fault. It never retries a mutation. Full request digests and result semantics are
checked; a recovered reservation cannot progress to commitStart. History carries
its original decision digest and grants nothing. Domain refusals are bounded codes
and do not imply that no durable mutation happened.

The host must create both raw socketpair ends atomically close-on-exec or retain syscall.ForkLock.RLock across creation and setting both flags and pass
only the designated child end. Runner marks the explicitly inherited fd, duplicates
it through `net.FileConn`, and closes that original. A process fixture verifies
that an actual worker inherits the ownership file and no authority socket. This
fixture proves descriptor plumbing; Phase C still requires real Desk authority
and real Runtime admission/order/recovery fixtures.

`GET /v1/jobs/{job}/execution-binding` is protected by the existing owner bearer
and origin guards. Its closed v1 export contains both the exact canonical typed
Release preimage (base64) and its digest, plus workspace/job/revision/release tuple.
A single SQLite snapshot reads the selected job/release and all triggers. Any
trigger, mapping, dialect, source/preparation or mismatched retained Pack/Runtime
material refuses. Exporting material creates no registration or operation.

Private manual ingress captures only facts and optional evidence from the original
bounded request body. `X-Jpack-Manual-Intent` is a host-prepared selector, never
proof of authority; an unconfigured Runner rejects it. No caller/operation identity
is accepted in the body. Dormant exact-byte storage retains request/facts/evidence
as base64, with original Idempotency-Key and evidence presence. Restore verifies
those bindings before restoring the exact bytes used by the payload codec and
Runtime files. This metadata is excluded from public Run/verification exports;
legacy serialized runs keep their existing encoding. A restored manual entry
cannot dispatch through an unconfigured Service.

Private one-use launch types accept only a matching fresh start-committed receipt,
require successful local persistence, and consume before starting the exact
prepared executable/arguments/environment through the existing executor lease.
They remain unwired. Phase C must first verify all actual input/config/lock files
against the frozen payload and use this object at the common operational starter.
The current defensive manual mode blocks direct operational evaluation, private
workflow acquisition/evaluation, automatic submit and upgrades.

Remaining integration: explicit anchored execution enrollment and older-binary
schema refusal; Desk local-admin intent preparation; actual host socket lifecycle;
post-ownership epoch activation before workers; authoritative registration lookup;
durable one-attempt preparation and start journal; complete automatic-trigger and
provider-route refusal; the non-bypassable operational starter; real ordering and
restore/recovery fixtures. No existing field, successful export or retained receipt
can substitute for those steps. Keep the current capability false until they pass.
