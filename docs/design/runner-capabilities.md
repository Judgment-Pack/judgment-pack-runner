# Runner pre-start capabilities

Status: private experimental host/companion contract, version 3, 2026-10-19.
The reviewed candidate supports optional `local-admin-manual-pack/1` host admission
on supported platforms, with explicit retained enrollment, exact registration and
per-attempt authority commitment at the actual operational starter. This is not
adopted portable Policy conformance. Ownership itself grants no permission.
Unconfigured legacy workspaces retain their behavior. See
[manual admission gate](manual-admission-gate.md) for scope and recovery limits.

## Probe before boot

The only accepted probe invocation is:

```sh
jpack-runner capabilities --format json
```

It exits successfully after writing this single UTF-8 JSON object and a newline
to stdout, with no stderr output:

```json
{"version":3,"capabilities":["jobs/1","required-boot-capabilities/1","executor-ownership/1","policy-manual-pack-run/1"],"governanceAdmission":true,"governanceProfiles":["local-admin-manual-pack/1"]}
```

Version 3 is a closed shape: exactly these four case-sensitive members, integer
`version: 3`, this ordered capability array, `governanceAdmission: true`, and the
single named governance profile. No duplicate members are permitted. There are no
installation paths, identifiers, configuration values, credentials or secrets in
the response. The response is less than 512 bytes. A shape or semantic change
requires an explicitly reviewed contract version; consumers must not interpret
unknown versions as a known shape. Earlier version 2 remains the closed
three-member ownership-only response below; hosts without supported admission
retain it. Earlier version 1 was also closed. Adding admission deliberately
changes the response version rather than widening version 2.

```json
{"version":2,"capabilities":["jobs/1","required-boot-capabilities/1","executor-ownership/1"],"governanceAdmission":false}
```

`jobs/1` names the existing companion protocol. `required-boot-capabilities/1`
means Runner strictly checks the optional boot requirement below before opening
its service. `executor-ownership/1` means a supplied trusted ownership binding is
required before the service opens and is inherited by every Service Runtime
invocation ([binding and lifetime](executor-ownership.md)).
`policy-manual-pack-run/1` names only the closed manual profile: a payload
descriptor, lease helper, version string or successful probe does not establish
permission to execute. Requiring an unknown capability, including the distinct
unsupported `governance-admission/1`, refuses startup.

The command does not read stdin or installation/project configuration, inspect a
Runtime or workspace, open a store, bind a listener, start workers or make provider
calls. It works without an installed Runtime or accessible workspace. Missing,
additional, reordered or misspelled probe arguments, including `--format=json`,
are refused with nonzero exit status; they cannot start the companion. Unknown
commands are likewise refused. `version` accepts no extra arguments. Other
existing subcommands retain their own parsers. Companion startup accepts no argv.

A host must probe **before sending any boot bytes**. It should give the probe
closed stdin (EOF), a short timeout and bounded stdout/stderr capture, terminate
an overlong or noisy probe, and accept only successful exit with the exact
supported closed response. Do not pass a bearer, boot line or other authority to
the probe. A legacy Runner may ignore unknown argv and try companion startup;
with EOF it refuses its missing boot line before opening anything. A legacy
failure is not permission to fall back to starting governed work.

Pin the exact executable inspected by identity/digest through actual companion
launch. Checking one path and later executing different bytes at that path is
not a trustworthy capability check. The host must also repeat its required
capabilities in the actual boot line: inspecting capabilities alone does not
protect against changed binaries, a different launch path or a boot mismatch.
Those host-side timeout, capture and executable-identity controls are host
requirements, not effects of this Runner command.

## Actual boot requirement

The existing bounded stdin boot object may contain this optional member:

```json
{
  "requiredCapabilities": {
    "version": 1,
    "capabilities": ["jobs/1", "required-boot-capabilities/1"]
  }
}
```

This example shows the additional member only; the existing `dir`, `runtime`,
`workspace`, `owner`, `token` and optional connection/profile/signing settings
still carry their established authority and validation. Boot input remains one
newline-terminated JSON object of at most 128 KiB.

`requiredCapabilities` is a closed object with exactly `version` and
`capabilities`, with their exact case-sensitive spellings. `version` must be the
integer literal `1`. `capabilities` must be a non-null array of at most 16 unique,
nonempty strings, each at most 64 bytes and exactly a supported capability name.
An empty array is valid and requires no features. Missing members, nulls, alternate
types, unsupported versions/names, unknown members and duplicate names refuse.
No version coercion, optional requirement, unknown-name omission or fallback is
allowed.

Duplicate JSON object members anywhere in the boot envelope are refused,
including escaped spellings that decode to the same member. Case variants of
`requiredCapabilities` are refused even when the canonical member is absent.
A second member or differently spelled alias cannot overwrite an unsupported
requirement with an empty list. Requirement member aliases such as `Version` or
`Capabilities` are refused. Validation also bounds JSON nesting to 128 levels.
A legacy valid boot with no `requiredCapabilities` continues to work; unrelated
unknown boot fields retain their existing ignored-field behavior. Malformed or
ambiguous boot envelopes are not legacy compatibility cases.

Requirement and envelope validation completes before `runner.Open`, therefore
before state-directory creation, locking, store migration, Runtime pinning,
queue recovery, worker startup, provider acquisition or listener creation. A
refusal writes no handshake on stdout and no boot values or paths in its generic
stderr diagnostic. It exits nonzero. Passing the requirement only establishes
support for the named features; the normal boot/service validation still runs
and can refuse independently.


## Ownership and requirement agreement

The additional boot member `executorOwnership` uses the closed version-1 binding
in [executor-ownership-boot.json](executor-ownership-boot.json). Its root must be a
clean absolute path, its machine workspace a bounded identifier, and its Runner
workspace must exactly equal the boot's `workspace`. Filesystem device/inode pins
are canonical unsigned decimal **strings**, never JSON numbers; inodes are positive.
The binding names the independently retained external lease, not portable store
metadata or request-selected paths.

The boot must explicitly require `executor-ownership/1` if and only if it supplies
`executorOwnership`. Requiring ownership without its binding, supplying a binding
without its explicit requirement, null/malformed bindings, unknown/case-aliased
members and duplicate keys all refuse before `Open`. No fallback removes this
requirement after a failed start. A boot without ownership or its requirement
retains existing local Jobs behavior.

Ownership binding and requirement syntax are checked before `Open`; `Open` then
acquires and verifies the existing external lease before any portable-directory
creation, store access, Runtime pinning, recovery or worker/provider startup.

## Manual admission and requirement agreement

The closed `executorAdmission` binding in the
[manual wire contract](manual-admission-wire-shape.md) requires
`policy-manual-pack-run/1` if and only if the binding is supplied, and also
requires matching executor ownership. Installation, both workspace identities,
executor, challenge, profile and inherited Unix stream descriptor are trusted
host custody, never browser or restored-store assertions. All fixed external,
prepared schema4 and boot demands must agree. Unsupported platforms refuse.

After actual external ownership and fixed operational signing, Runner activates
the dedicated channel before recovery/workers. Every operational start requires
current reserve/start commitment and the original session's locally journaled
one-use launch. Enabled workspaces reject automatic triggers, acquisitions,
workflow operations and upgrades; there is no mixed legacy-resource escape.
Legacy workspaces do not acquire governance from a probe response alone.

## Verification

Targeted command tests use stdin that panics on any read and missing configuration
locations. Boot refusal tests use an absent Runtime and nonexistent state/workspace
paths: no state directory may be created, which proves refusal precedes `Open`
and hence all worker startup. They cover unsupported governance, malformed and
closed-shape requirements, duplicate/escaped/case-alias downgrade attempts,
limits, and preservation of valid legacy boot values. Repository CI supplies
the broader platform, integration and race checks.

Ownership tests also cover canonical uint64 encoding beyond JavaScript's exact
integer range, exact machine/Runner identities and the shared boot fixture.
Service tests exercise real previews, saved checks, dispatch, upgrades and the
private workflow adapter, and refuse launches after close. The Linux real-Runtime
process-lifetime test described in [executor ownership](executor-ownership.md)
observes the exact pinned executable and its inherited lease before stopping its
owner. It requires `JPACK_TEST_BIN`; a skipped integration is not verification.
