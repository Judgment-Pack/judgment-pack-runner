# Runner pre-start capabilities

Status: private experimental host/companion contract, version 1, 2026-10-18.
This implements the capability-negotiation foundation of Desk's Policy execution
admission design. It does not implement governed activation, resource registration,
per-attempt admission, withdrawal enforcement or Runtime dispatch authorization.
Execution-payload and executor-lease helpers remain inactive foundations. Existing
jobs remain ungoverned.

## Probe before boot

The only accepted probe invocation is:

```sh
jpack-runner capabilities --format json
```

It exits successfully after writing this single UTF-8 JSON object and a newline
to stdout, with no stderr output:

```json
{"version":1,"capabilities":["jobs/1","required-boot-capabilities/1"],"governanceAdmission":false}
```

Version 1 is a closed shape: exactly these three case-sensitive members, integer
`version: 1`, this ordered array of supported capability strings, and boolean
`governanceAdmission: false`. No duplicate members are permitted. There are no
installation paths, identifiers, configuration values, credentials or secrets in
the response. The response is less than 512 bytes. A shape or semantic change
requires an explicitly reviewed contract version; consumers must not interpret
unknown versions as version 1.

`jobs/1` names the existing companion protocol. `required-boot-capabilities/1`
means Runner strictly checks the optional boot requirement below before opening
its service. Governance admission is explicitly unsupported: a payload descriptor,
lease helper, version string or successful probe does not establish permission to
execute. No governed execution capability is advertised. Requiring an unknown
capability, including `governance-admission/1`, refuses startup.

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
Those host-side timeout, capture and executable-identity controls are requirements
for future host integration, not features implemented by this Runner command.

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

## Verification

Targeted command tests use stdin that panics on any read and missing configuration
locations. Boot refusal tests use an absent Runtime and nonexistent state/workspace
paths: no state directory may be created, which proves refusal precedes `Open`
and hence all worker startup. They cover unsupported governance, malformed and
closed-shape requirements, duplicate/escaped/case-alias downgrade attempts,
limits, and preservation of valid legacy boot values. Repository CI supplies
the broader platform, integration and race checks.
