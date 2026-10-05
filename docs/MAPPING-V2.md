# Input mapping v2: verified projection and Desk integration

Status: implemented in Runner and Desk's advanced Jobs mapping workflow. This
contract supersedes the 2026-09-25 handoff for this bounded slice. The existing
version 1 file mapping and its historical records remain supported unchanged.

## Decisions and ownership

Runner verifies v3 acquisition receipts itself, projects facts/evidence, records
lineage and supplies Runtime citations. The caller obtains responses; Runner
performs no network acquisition. Gateway retains credentials and signing keys.
Runtime remains the evaluator. Desk provides the editor, explicit acquisition
orchestration and retained lineage view. No new repository or shared module is introduced.

Source classes are `asserted`, `record`, and `generated`. Case/local-file data is
asserted. For authenticated sources the installation, not a mapping/API caller,
supplies a trusted profile. A profile pins source, authority, gateway public key,
adapter name/version/digest, shape, endpoint, class and permitted read tools. Its
canonical digest is frozen into the mapping; the release retains the profile.
The receipt signature authenticates the corresponding acquisition identity; the
operator profile supplies classification. This is not a new signed Gateway
class field. A profile cannot give one gateway source conflicting classifications.

`record` identifies the acquisition channel, not the authorship or truth of every
byte stored in that system. Endpoint and adapter metadata remain Gateway/adapter
testimony. The profile does not pin hidden credentials, tenant configuration or
model weights. An installation must replace its profile when those deployment
changes invalidate its reviewed classification or read-only guarantees.

Generated outputs need explicit admission per fact/evidence target. A generated
parameter influencing a subsequent source requires that target to admit both
its direct class and `generated`. This prevents a database lookup from erasing a
model's influence. Caller identity parameters remain asserted inputs; their use
does not turn the directly retrieved record class into asserted. Dependencies
are retained separately. Pack-level class restrictions are a later spec proposal.

The Go derivation implementation is vendored, isolated and pinned at experiments
commit `96fa9587`, with its license and 21-case corpus. It reuses the existing
second implementation; it is not a third independent implementation.

## Supported input and trust configuration

The trusted boot line optionally supplies `inputProfiles: [...]`. Profiles contain
public verification configuration only. No HTTP endpoint can create or modify
profiles. `GET /v1/input-profiles` returns profiles with their canonical digests for
review; this does not authorize arbitrary acquisition. A profile for an operation
must have `shape: "mcp"` and a nonempty `tools` allowlist. Read-only backend
credentials and operator-reviewed tools enforce the external read boundary.

`POST /v1/inputs/preview`, `/v1/previews`, and `/v1/jobs/{job}/runs` accept:

```json
{
  "source": {
    "mapping": {
      "version": 2,
      "case": {
        "parameters": {"vendorId": {"pointer": "/id", "type": "integer"}},
        "facts": [],
        "evidence": []
      },
      "sources": [{
        "name": "vendor",
        "kind": "operation",
        "profile": "vendors",
        "profileDigest": "sha256:<digest returned by input-profiles>",
        "maxAge": 300,
        "arguments": {
          "tool": "execute_sql",
          "arguments": {"sql": {"$text": "SELECT to_jsonb(v)::text AS record FROM vendors v WHERE v.id = {{vendorId}}"}}
        },
        "read": {
          "unwrap": ["/content/0/text", "/data/statements/0/rows/0/record"],
          "rule": {
            "ruleVersion": "1",
            "parameters": {"vendorId": "integer"},
            "clauses": [
              {
                "when": {"op": "all", "of": [
                  {"op": "equalsParam", "field": "/id", "param": "vendorId"},
                  {"op": "exists", "field": "/description"}
                ]},
                "claim": {
                  "facts": [{"pointer": "/vendor/description", "from": "/description"}],
                  "evidence": {"vendor-record": "present"},
                  "acquisitionStatus": "resolved"
                },
                "reason": "matching-vendor"
              },
              {
                "when": {"op": "always"},
                "claim": {"facts": [], "evidence": {"vendor-record": "unknown"}, "acquisitionStatus": "unknown"},
                "reason": "wrong-subject"
              }
            ]
          }
        }
      }],
      "unmapped": [],
      "unmappedEvidence": []
    },
    "case": {"id": 7},
    "sources": {"vendor": {"response": {"result": {}, "receipt": {}, "salts": {"args": "<64 lowercase hex characters>"}}}}
  }
}
```

The response/profile digest above are placeholders, not working credentials or
proof. The unwrap layout must match the chosen MCP server's actual response.
This illustrative query expects a unique vendor row. Review that invariant and
response cardinality; an unwrap is not a row-count assertion. A failed unwrap is
an input-preparation error, not a fabricated missing record. To derive absence,
return an explicit bounded status envelope and guard it in a rule.

Source kinds:

- `operation`: one retained v3 MCP response `{result, receipt, salts}`. The request
  template names a fixed, operator-allowed tool. Each source is one acquisition.
- `selected-file`, `provider: "local-file"`: `sources[name].snapshot` is the v1
  selected JSON snapshot. No profile or request arguments. Its class is asserted.
- `selected-file`, `provider: "google-drive"`: the existing selected snapshot
  retains the response and `salts.args` in `proof.response`. A pinned record
  profile for `drive`, `maxAge` and an arguments template are required. Resolve
  `fileId` and the consumed selection `grant` from typed case parameters. Runner
  verifies their exact commitment, receipt identity and original-byte binding.
  The snapshot's public key never establishes trust. Its retained registry is
  not verified in this receipt-only slice.

`read` selects exactly one of `copy: {facts: [{target,source}], evidence:
[{requirement,source}]}` and a portable `rule`. Optional `unwrap` applies up to
four RFC6901 pointers in order; each resolves to a string containing one JSON
value. Root destination writes are refused. Fact targets cannot overlap, and
each evidence requirement has one producing source. Alternative rule clauses
may produce the same destination; the union is used for mapping review.

Case parameters are typed `string`, `integer` or `timestamp`. A source-local
parameter is `{from: "earlier-source", pointer: "/derived/fact", type: ...}`.
It reads only that source's derived facts after `acquisitionStatus: "resolved"`.
It never reads a raw rejected artifact. Missing/unknown/absent upstream outputs
skip the dependent source and record `dependency-unavailable`. Supplying a response
for that skipped source is refused. A present but mistyped export is an error.
An export needed only by another source is still a declared derived fact and is
subject to admission, lineage, and the unused-target warning.

Whole-value `{"$param":"name"}` substitution preserves the parameter type.
`{"$text":"...{{name}}..."}` permits integer parameters only, with canonical
rendering. Prefer provider bind parameters for all values. Keep SQL structure
and identifiers fixed. For DBHub `execute_sql`, the `sql` field must be a fixed
string or `$text`; a whole-value parameter cannot replace it. The template forms are reserved and cannot be literal
one-member objects. Unknown forms, undeclared parameters, shadowing, forward
references, cycles and unknown fields are refused.

By default targets admit asserted and record. Use `admits.facts` and
`admits.evidence` maps to replace a target's admitted class list, for example:
`{"facts":{"/vendor/risk":["generated"]}}`. Admission applies to all possible
outputs, including branches the preview did not reach.

## Calculated values

An installation declares a calculator on a trusted profile, as
`calculator: {name, version}`. The profile must be `record` and `mcp`. Declaring
it asserts that each allowed tool computes a deterministic function of the inputs
it echoes and the tables it reports, and calls no model; Runner cannot check that.
A calculator that calls a model is a `generated` profile and cannot be declared a
calculator. The reasons are in [the design record](design/calculated-values.md).

The value a calculated source's read applies to, after `unwrap`, is an object with
a `calculation` member of exactly `calculator` (the pinned name and version),
`status` (`computed`, `input-missing` or `cannot-compute`), `inputs` (each input
used, by name) and `asOf` (for each table read, a timestamp in the rule's form).
The source binds that member:

```json
"calculation": {
  "inputs": {"amount": "invoiceAmount", "currency": "invoiceCurrency"},
  "tables": {"ecb-rates": 86400}
}
```

Each input names a parameter of the source, a case parameter or one taken from an
earlier source, and never `runAt`. Each table names the oldest its contents may
be, in seconds, from 1 through 315,360,000. A source with a calculator profile
and no `calculation`, or the reverse, fails preview before any acquisition.

Preparation fails unless the answer names the pinned calculator and version;
every echoed input is bound and equals its parameter as canonical JSON; and every
reported table is named and has an instant. A `computed` answer must also echo
every bound input and report every named table, as of no more than its limit
before the verification instant and no more than 30 seconds after it.

For `input-missing` or `cannot-compute`, Runner does not apply the source's read.
The claim has no facts, every mapped evidence requirement `unknown`, acquisition
status `unknown`, reason `calculation-input-missing` or
`calculation-cannot-compute`, and basis `/calculation/status`. Dependent sources
record `dependency-unavailable`. Each lineage entry of a calculated source that
was acquired carries `calculation`: the calculator, the status, each echoed input
with the parameter it is bound to and that parameter's source and pointer, and
the reported `asOf`. A calculated source skipped as `dependency-unavailable`
acquired nothing, and its entries carry no `calculation`.

Neither the echo nor the instants are Runner's to establish: they are the
calculator's statements in a signed response, held to the case and to the
mapping's limits. A profile, mapping or lineage entry without these members
encodes as before.

## Numeric and resource profile

V1 still preserves fractional and large integer values. V2 claims, rules,
request arguments and signed Gateway envelopes use Gateway canonical JSON:
integers in ±(2^53−1), no floating literals, code-point key order, exact Unicode.
There is no implicit numeric conversion. Explicit decimal strings can represent
money or measurements. Migrating an arbitrary v1 mapping to v2 is not automatic.

Unwrapped artifacts may contain fractional numbers for rule comparisons; copying
one into a claim fails. Runner additionally limits numeric tokens to 128
characters and exponent magnitude to 308 before reference-rule arithmetic.
The vendored contract itself is unchanged.

Limits: 2 MiB aggregate input, 128 KiB mapping, 16 sources, 64 parameters per
scope, 64 clauses per rule, 64 children per compound condition, four unwraps,
200,000 bytes per selected document/unwrap string, value nesting at most 32
(with 64 levels for outer API/export envelopes), 256 fact
targets and 128 evidence targets. Duplicate keys and malformed Unicode are refused.
No acquisition or network wait holds a SQLite transaction or dispatcher slot.

## Time, sessions, errors and retries

This first slice intentionally uses **receipt-only verification**. It does not
claim a sealed/complete session, fresh session ownership, absence of omitted
attempts or proof that a caller did not choose among multiple valid responses.
The acquisitions of one input are in one of two forms, and one receipt cannot
stand in for two sources:

- **they share a session**, at any call indexes that differ. Desk's preparation
  leaves them so;
- **each is alone in a session of its own and is that session's first call**
  (call index zero). Runner leaves them so when it acquires unattended, on
  either path: it names a session for each acquisition, takes the answer only
  as that session's first call, and asks the gateway to seal the session after.

An input in neither form is refused at the first source, in the mapping's order,
that leaves both: two acquisitions that share a session beside one that does not,
or, among acquisitions of sessions apart, one that is not its session's first
call. One acquisition alone shares a session with itself, and is taken at any
call index, as it was. The form is read from the receipts and from nothing else, so whoever
verifies an export later reads the same form. Neither form says a session is
sealed or complete, as stated above: a caller that asks twice chooses which
answer to submit, in one session or in two.

Citations are recorded by call index, and those of equal index in the mapping's
order of sources. Cached responses can be reused within their source's `maxAge`.
`maxAge` is required, from 1 through 86,400 seconds. Observation and Gateway
serving time permit at most 30 seconds of future clock skew.

Runner supplies whole-second UTC `runAt` only to derivation rules. It is the
input verification instant before queue admission, retained as `verifiedAt`.
It is prohibited in acquisition templates, resolving the caller-before-acceptance
circularity without introducing a session-intent API. Freshness is assessed then,
not when the queued evaluator later starts. Rule `freshWithin` retains its own
strict no-future semantics. A long queued wait does not silently reacquire data.

On dispatcher restart, retained preparation is recomputed against the frozen
release profiles at its original verification time. New submissions require the
current configured profiles still to match the release. Historical verification
uses independently supplied historical trust configuration.

Malformed rules fail preview. Bad proof, mismatched request/identity, stale data,
failed unwrap, mistyped dependencies and derivation rejection fail preparation;
they do not become policy unknowns and do not queue an evaluation. Rejected API
submissions return a bounded problem and are not operational run records. A
valid rule may deliberately produce `unknown` or `absent`.

V2 idempotency fingerprints the submitted input before projection, excluding
server-computed `preparation`. Resubmit the same source envelope and key, even
when its proof has since expired. The original accepted run is returned.
Reusing a key with different inputs returns 409. Reacquiring a source is a new
submission with a new key. A retry does not consume a selection grant again.

## Release checks, runtime and offline verification

The verified sample fixes the mapping and trusted profiles. Runtime's inventory
is obtained from the exact frozen pack. Every candidate fact pointer must be
covered by a target/ancestor or acknowledged in `unmapped`; evidence requirements
must be mapped or listed in `unmappedEvidence`. Undeclared evidence is refused.
Root and partially mapped aggregate pointers generate explicit review warnings.
Inventory paths over-approximate actual reads, and an omission is not a guarantee
of an unknown evaluation. Subject binding and row cardinality remain explicit
review obligations, not a search for one `equalsParam` anywhere in the rule.
Mapping readiness is separate from saved behavioral tests and advisory test coverage.

Runner ignores supplied preparation metadata and recomputes it. Supplied facts
or evidence, if present, must equal that computation. It retains all source
responses/salts, instantiated requests, parameters, source outcomes and lineage.
Each lineage entry names class, generated influence, presence/omission, reason,
read digest, actual condition `basis`, dependency pointers and verified citation.
`candidateFrom` lists the static possible copy fields; it is deliberately not
labeled actual reads. Exact rule bytes and outcomes allow replay. Unmapped fields
remain identified by the frozen mapping. Briefs exclude raw responses and grants.

Only operational evaluation supplies `--cites`; rehearsal supplies none. Runner
checks that Runtime's retained audit contains exactly the supplied citations,
facts and evidence. No Runtime internals or semantic changes are introduced.

`GET /v1/runs/{run}/verification` exports `{version:2,releaseDigest,release,run}`.
It accepts `?version=` 2, 3, 4 or 5, asked at most once, and serves version 2
when none is asked for. Version 3 adds the audit record's original bytes
([below](#exact-bytes-of-the-audit-record)), version 4 also the run's entry in
the installation's chain of runs ([below](#the-installations-chain-of-runs)),
and version 5 also the record's signature sidecar
([below](#record-signatures)). Any other version, a version asked for more than
once, or a query that is not well formed, is answered 400 `invalid_version`:
"Ask once for verification export version 2, 3, 4 or 5, in a well-formed
query."

The version asked for is the most an export can be, not what it is. A run that
lacks what a version adds is exported at the highest version whose material it
holds, answered 200 like any other. The steps are taken in turn: version 5
needs the record's signature sidecar, or the run is served as version 4;
version 4 needs the run's entry in the chain of runs, or version 3; version 3
needs the audit record's bytes, or version 2. So, asked for 5, a run with its
entry and no sidecar answers 4, a run with no entry answers 3 or 2, and a run
without the bytes, such as one that failed or has not finished, answers 2.
Nothing but the export's own `version` says which was served: read `version`
from the body, never from the request. What each version needs is
[below](#exact-bytes-of-the-audit-record).

A run of a job with no input mapping, or with a v1 mapping, is answered 422
`not_verified_mapping`, "This run does not use mapping v2.", although a
completed one has the audit record's bytes, an entry in the chain of runs and,
under a signing key, a signature sidecar; whether it may have an export without
the lineage is an open question (#44).

Retain a trusted release digest independently, alongside the installation's public
profiles. An untrusted export cannot establish its own public-key or release trust.

```sh
jpack-runner verify-run \
  --file retained-run.json \
  --profiles trusted-historical-profiles.json \
  --release-digest sha256:YOUR_INDEPENDENTLY_RETAINED_RELEASE_DIGEST
```

Verification recomputes the frozen release digest, receipt signatures, exact
arguments/result digests, historical freshness, projection, admission, lineage
and audit input/citation binding. It performs no network request or model call.
It verifies what a reader of the export reads: the export must be exactly what
the runner encodes, so a member named like one of its fields but for case is
refused, and the retained result and audit record are read by exact member names.
Without `--runtime` it reports `verified-inputs`, not policy truth or evaluator
re-execution.

The retained disposition is then not read. A record whose inputs are intact and
whose disposition was changed after the run still verifies. The report says so, on
standard output for a program and on standard error for a person:

```text
{"exactBytes":"not-in-export","exportVersion":2,"retainedDisposition":"not-checked","scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"verified-inputs","targetsByClass":{"asserted":3,"record":0,"generated":0}}
verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source. Exact-byte checks were not possible: a version-2 export carries no original bytes of the audit record, so nothing here gives the digest a gateway receipt names it by.
```

Verification recomputes every input the same way, but what that establishes
depends on the input's class. `targetsByClass` counts the run's fact and evidence
targets by the `class` its lineage records for each: the class of the source the
target is mapped from. Every target is counted, including one the run has no
value for, and one whose source was skipped because a dependency was
unavailable, which has no response and no citation.

An `asserted` target was typed into the case or read from a local file, and
nothing but the export vouches for it. An export whose asserted inputs were
rewritten after the run, with its preparation, result and audit record
recomputed to match, verifies as the original did, with or without `--runtime`.
A run of a case-only mapping has only asserted targets.

A `record` or `generated` target of a source that was acquired was derived from
a signed response. Verification checks the response's signature, under the key a
trusted profile pins, and derives the target again from that response and from
the dependencies the export retains. Changing the response, or a target's
retained value alone, fails verification. Changing a parameter need not.
Parameters, including the case's, are dependencies, not targets, and are not
counted. One that the source's own request carries unambiguously is committed
to by its receipt, so changing it fails verification; the record it chose
remains the caller's assertion, as above. A request carries a parameter
unambiguously as a whole value (`$param`), or in `$text` that names no other
parameter. Text that names two or more commits none of them: `{{a}}{{b}}`
renders 1 and 23 as it renders 12 and 3. A calculator's signed answer also
commits each parameter its `calculation` binds, since the answer's echo of that
input must equal it. A parameter that a rule reads, and that neither commits,
is not signed: changing it in the export can change a derived target, for
example evidence from `present` to `unknown`, and the export still verifies.
Another source's receipt does not sign it for this one, and a value derived
from such a parameter, through a dependency, is not signed either.

`unsignedParameters` names them. For each source that was acquired and has
targets, and whose rule reads a parameter that the source's own receipt does
not commit, it gives the source, each such parameter's `name` and `kind`, and
how many targets the source maps, with a value or without. A source's receipt
commits a parameter as above: its signed request carries it unambiguously, or
its calculator's signed answer echoes it. Another source's receipt commits
nothing for this one: it signs that source's request, not this source's rule.
A rule reads the parameters its conditions name (`equalsParam`'s `param`,
`freshWithin`'s `asOf` and `maxAge`, at any depth), not those it only declares.
The kinds:

- `case`: a case parameter, as the operator supplied it.
- `local-file`: a fact of an earlier local-file source, which is asserted.
- `upstream`: a fact of an earlier acquired source whose own rule read a
  parameter of one of these kinds. Its value can change with that parameter,
  so it is not signed for this source unless this source's own request
  carries it unambiguously.
- `ambiguous-text`: one of those, which the request carries only in text that
  names another parameter.
- `runAt`: the run's time, which no request can carry. Verification takes it
  from the export's own `preparation.verifiedAt`, so a verdict that reads it,
  such as `freshWithin`, rests on the time the export says it was prepared.
- `upstream-runAt`: a fact of an earlier acquired source whose own rule read
  `runAt`, and no parameter of the operator's.

A fact of an earlier acquired source that its rule derived from signed values
alone, or copied from its response, is not listed. A skipped source, and a
calculator that did not compute, whose rule was not applied, read nothing, and
are not listed. The member is worked out from the frozen mapping and the run's
outcomes, so the stored lineage is as it was, and it is present only when a
source is listed:

```text
"unsignedParameters":[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"},{"name":"vendorId","kind":"ambiguous-text"}],"targets":1}]
```

When any target is asserted, the line on standard error says so: as above when
all are, or how many, for example `1 of the run's 3 inputs is the operator's own:
nothing here checks it against a source.` For the member above it adds `1 of the
run's 3 inputs was derived by a rule that reads a parameter resting on the
operator's say, which its source's receipt does not commit: nothing here checks
that parameter against a source.` and `2 of the run's 3 inputs were derived by a
rule that reads runAt, or a value derived from it, which rests on the export's
own verification time.` When there is neither, the line is as it was.

Add `--runtime /absolute/path/to/jpack` to check the disposition too, for a
completed run. The executable's bytes are hashed and it is refused unless the
digest is the release's `runtimeDigest`; the copy that was hashed is the one run.
It evaluates the verified facts and evidence under the release's frozen pack,
configuration and lock as a rehearsal (`jpack experimental evaluate --rehearsal`),
in a private directory under the system's temporary directory (`TMPDIR`) that is
removed after. A rehearsal appends no audit record; an answer not labelled as a
rehearsal, or an audit record left anyway, is refused. Nothing is written to a
Runner store. The canonical disposition is compared byte for byte with the one the
run's result retains and the one its audit record retains:

```text
{"exactBytes":"not-in-export","exportVersion":2,"retainedDisposition":"matches-re-execution","scope":"retained input derivation, audit binding, and the retained disposition against a re-execution by the release's Runtime; not sealed-session completeness or policy truth","status":"verified-disposition","targetsByClass":{"asserted":0,"record":3,"generated":0}}
verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true. Exact-byte checks were not possible: a version-2 export carries no original bytes of the audit record, so nothing here gives the digest a gateway receipt names it by.
```

When either differs, the command exits 1, reports `"status":"disposition-differs"`
with `"retainedDisposition":"differs-from-re-execution"`, and names the failure
on standard error. `verified-disposition` adds one statement to `verified-inputs`:
the release's Runtime, given the verified inputs, decides what the record says.
It is not policy truth. Nor does it say more of asserted inputs than
`verified-inputs` does: the report counts them and the line names them the same way.

Add `--require-sourced` to refuse a run with an asserted fact or evidence target,
or with a target of an acquired source whose rule reads a parameter of the kind
`case`, `local-file`, `upstream` or `ambiguous-text`. Verification is otherwise unchanged;
the refusal exits 1, reports its own status, and names on standard error how many
targets it refuses. It comes before any re-execution, so with `--runtime` the
executable is not run:

```text
{"exactBytes":"not-in-export","exportVersion":2,"retainedDisposition":"not-checked","scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"inputs-asserted","targetsByClass":{"asserted":1,"record":2,"generated":0}}
runner: inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source
```

A target derived from an unsigned parameter is refused as `parameters-unsigned`.
When a run has both, the status is `inputs-asserted`, and the report's members
show both:

```text
runner: parameters-unsigned: 1 of the run's 3 inputs was derived by a rule that reads a parameter resting on the operator's say, which its source's receipt does not commit, and --require-sourced refuses it: nothing here checks that parameter against a source
```

An asserted target is refused whether or not the run has a value for it: an
omitted value is the operator's say as much as a present one. Without the flag
nothing is refused that was accepted before. What it lets through:

- A `generated` target: its receipt says what the generating source answered,
  not that the answer is true.
- A `record` or `generated` target whose source was skipped: it has no value,
  rather than an asserted one, and no receipt.
- A target whose rule reads `runAt`, or `upstream-runAt`, and no other unsigned
  parameter. `runAt` is exempt so that freshness checks that use it are not
  refused. The report and the line say that such a verdict rests on the
  export's own verification time.

The export includes private case/request values and consumed grant salts;
distribute it only to intended reviewers. Public keys alone do not authenticate an export's
choice of policy or its historical verification time.

### Exact bytes of the audit record

A digest of an audit record, such as the `decision.recordDigest` by which a
gateway action receipt names the record it relied on, is the SHA-256 of the
bytes the Runtime wrote: the record's line in the attempt's audit trail,
without the newline that ends it. The Runtime writes `&`, `<` and `>` as they
are. Runner keeps the record parsed, as `run.audit`, and encodes it again with
them escaped: the same JSON value, with other bytes and another digest. With
Runtime 0.24.0 this happens, for example, when the pack's identifier holds an
`&`. A run's facts reach the Runtime as Runner encoded them, already escaped,
and are recorded so.

So a run also keeps the record's line exactly, as `run.auditBytes`, in standard
base64. Base64 rather than a JSON string: every JSON encoder passes it through
unchanged; it carries any bytes, including bytes that are not UTF-8, which no
JSON string can; and it decodes in one step to the bytes a digest is taken
over. `GET /v1/runs/{run}` shows it; run lists and briefs leave it out. A run
recorded before Runner kept the bytes has none, nor does a run that did not
complete. An attempt's trail that is not the record's one line, ended by a
newline, still parses, but no line of it is the record: Runner refuses such an
evaluation, and the run fails rather than completing without the bytes. Runner
0.4.0 recorded such a run completed, without them.

The verification export has four versions:

- **Version 2**, served when no version is asked for and for `?version=2`, is
  the export Runner has always made, byte for byte. It carries no original
  bytes: its readers decode it strictly and accept only version 2, so a new
  member would break them.
- **Version 3**, served for `?version=3`, carries `run.auditBytes` beside the
  parsed `run.audit`. A run that holds no bytes is exported as version 2 even
  then: nothing is made up in their place.
- **Version 4**, served for `?version=4`, is version 3 with the run's entry in
  the installation's chain of runs ([below](#the-installations-chain-of-runs)).
  A run with no entry is exported as version 3, or version 2, even then.
- **Version 5**, served for `?version=5`, is version 4 with the signature
  sidecar of the run's audit record, `run.auditSignatures`
  ([below](#record-signatures)). A run with no sidecar is exported as version
  4, or 3, or 2, even then.

Any other `version`, `version` asked for more than once, or a query that is not
well formed, is refused with `invalid_version`. How a caller tells which version
it received, and which runs have no export, is
[above](#release-checks-runtime-and-offline-verification).

Version 3 is larger than version 2 by the bytes, in base64. It is held to the
8 MiB that version 2 is held to, beside the member carrying the bytes, which
are held to the 8 MiB of an audit trail that Runner reads. So `verify-run`
reads a version-3 export of up to about 18.7 MiB, and accepts as version 3 any
export that it accepts as version 2. Version 5's sidecar is held to 16 KiB
beside that, and the export is held to 8 MiB beside it.

`verify-run` accepts all four, and checks each as above. Of a version-3, 4 or 5
export it also checks that the bytes are one line, with neither a line
feed nor a carriage return, that they parse as strictly as the export does, and
that they are the same JSON value as `run.audit`. Otherwise it refuses the
export. Its report gives their SHA-256 as `recordDigest`, the digest a gateway
receipt would name for those bytes. Of a version-3 export:

```text
{"exactBytes":"matches-record","exportVersion":3,"recordDigest":"sha256:58ceb36b8c45956df5d6efb6d5b0fcce47e7f5c4ced7f39fa4c11c3286e12a6d","retainedDisposition":"not-checked","scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"verified-inputs","targetsByClass":{"asserted":3,"record":0,"generated":0}}
verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source. The export's bytes of the audit record parse to the record. Their SHA-256 is sha256:58ceb36b8c45956df5d6efb6d5b0fcce47e7f5c4ced7f39fa4c11c3286e12a6d: a digest held independently, such as a gateway receipt's, shows whether they are the bytes the Runtime wrote for this run.
```

Of a version-2 export it reports `"exactBytes":"not-in-export"` and no
`recordDigest`, and the line on standard error says that exact-byte checks were
not possible, as in the examples above. What the line says of the bytes comes
after what it says of asserted inputs and unsigned parameters, and before what
it says of the run's entry in the chain of runs and of its record signature.

What version 3 establishes is narrow. The bytes it carries are consistent with
the record it exports, and their digest is the one a gateway receipt would name
for those bytes. Bytes that were changed have another digest, and fail a
comparison with an original digest held independently of the export, such as a
gateway receipt's. `verify-run` reads no receipt: that comparison is the
reader's step. Without it, version 3 does not establish that these are the
bytes the Runtime wrote for this run, nor that the record belongs to this run:
nothing binds the run to the record beyond the checks above. Another run of the
same release with the same inputs could supply its own record and bytes. Bytes
re-encoded without changing their value, with other whitespace or an escaped
`&`, also verify, with another digest, without `run.audit` being rewritten.

### The installation's chain of runs

Each attempt has its own audit directory, never reused, so the Runtime's trail
inside it holds one record: as a chain it says nothing about the installation's
history (Runtime ADR-0047). Runner keeps a chain of its own over the runs it
retains. Each completed run is given one entry, a line of compact JSON:

```text
{"entryVersion":"1","trail":"68e77cefed3b0f20750d9b61ffceec38","sequence":3,"previous":"sha256:…","kind":"run","run":"run_…","auditDigest":"sha256:…"}
```

- `trail` is the chain's identity, 128 random bits in hex, minted with the
  first entry and carried by every entry after it.
- `sequence` is the entry's place in the chain, from 1.
- `previous` is the SHA-256 of the exact bytes of the entry before it, without
  a newline, and of nothing for the first entry.
- `run` is the run's id, and `auditDigest` the SHA-256 of its audit record's
  exact bytes, `run.auditBytes`: the digest a gateway receipt's
  `decision.recordDigest` names the record by.

These are the Runtime's rules for its own trail, and the Runtime's checkpoint,
`{"checkpointVersion":"1","recordDigest","sequence","trail"}` in its RFC 8785
canonical form, names an entry the same way: the SHA-256 of the entry's line,
its sequence and its trail. So one verifier's understanding carries over. A
Runtime whose `jpack audit verify` takes `--trail` (after 0.25.0) reads Runner's
chain as a chained trail, and holds it to the same checkpoints. A test holds
Runner to such a Runtime, and skips under one without that command, as under
Runtime 0.25.0 and earlier; CI pins 0.26.0, which has it (#35). An entry is read
by its members, as JSON reads them, and every digest is over the bytes as
stored, never over a re-encoding.

**Where the chain lives, and how it holds.**

- It is the `run_chain` table of the store's SQLite database: one row per
  entry, holding its line's exact bytes. It is backed up and restored with the
  rest of the store. A transaction covers the run and its entry, which a
  separate file could not share.
- An entry is appended in the transaction that records its run as completed,
  after the evaluation, once the record's bytes are kept. Every run recorded
  completed has its entry. A crash leaves both or neither. If the append fails,
  the completion is not recorded either. The dispatcher stops, as on any
  storage error, and after a restart the run reads interrupted, like any run
  whose completion was not recorded. A completion without the record's exact
  bytes fails the same way, though the evaluation refuses such a trail first.
- Nothing is held in memory. Each append reads the last entry inside its
  transaction, continues its trail and sequence, and links to its line, so a
  restart changes nothing.
- SQLite serializes write transactions. Runner holds the store's installation
  lock, and its database handle has one connection, so two appends never read
  the same last entry. The sequence is also the table's key, and a run's id is
  unique in it. No sequence is repeated or skipped.
- Runner does not chain after a last entry it did not write, or one that is not
  at its own sequence. The dispatcher stops instead.
- Failed, interrupted and expired runs are given no entry: Runner retains no
  audit record for them, though an attempt's directory may hold one.
- Runner writes no entry it would not read back. A run's id that would make one
  is refused like a failed append.

**Runs recorded before this release** have no entry, and are never given one:
they are unchained. Asked for version 4, such a run's export is version 3, or
version 2 without the record's bytes. Nothing is made up in the entry's place.
`verify-run` reports such an export as before. It refuses `--chain`, `--expect`
and `--require-witnessed` for it, and says the run is unchained.

**A deleted run.** Runner deletes no run and no entry, and any future pruning
of runs must keep their entries. A run removed from the store keeps its entry,
which still names it, and asking for its export answers `410 run_not_held` with
the entry's sequence. An entry removed leaves a gap, which fails the sequence
and `previous` of the line after it. Some removals leave a consistent chain,
which only a checkpoint held from before the change detects:

- an entry removed with every later entry rewritten to close the gap
  (`checkpoint-record-mismatch`, or `checkpoint-beyond-chain`);
- the last entries removed, or an older copy of the store restored
  (`checkpoint-beyond-chain`).

**The export.** Version 4 is version 3 with one more member:

```text
"chain":{"entry":"<the entry's line, in base64>","checkpoint":{"checkpointVersion":"1","recordDigest":"sha256:…","sequence":3,"trail":"…"}}
```

The checkpoint is the entry's own. It is a function of the entry's bytes, so
every export of the run carries the same one. Version 3's readers decode
strictly and accept only versions 2 and 3, so the entry needed a new version,
and versions 2 and 3 are byte for byte what they were. The entry is held to
1024 bytes, beside the 8 MiB the rest of the export is held to.

`GET /v1/run-chain` serves the whole chain as `application/jsonl`: every entry's
line, exactly as stored, each ended by a newline. It is read in pages. Rows are
only appended, so the answer is the chain as it stood at some moment during the
reading. A failure after the answer began aborts the transfer rather than
shortening the chain. Desk does not pass this route through yet.

**Verification.** For a version-4 export `verify-run` checks the following:

- The entry is one Runner writes, names this run and the SHA-256 of the
  export's `run.auditBytes`, and the checkpoint is the entry's. Otherwise it
  refuses the export.
- With `--chain <file>`, the chain as `GET /v1/run-chain` served it. Every line
  must be an entry at its own sequence, of the trail of the entry before it,
  linked to that line's exact bytes (the first to the SHA-256 of nothing). No
  run may be named twice. The line at the run's sequence must be the export's
  entry, byte for byte.
- With `--expect <file>`, which may be given more than once, the checkpoints a
  holder kept, one per line, as the Runtime's `audit verify --expect` reads
  them. Each must name an entry of its trail that has its digest. Without
  `--chain`, each must name the run's own entry: one at another sequence is
  checked only along the chain, and is refused without one. Along the chain, a
  checkpoint beyond its end fails, as a chain cut short.
- `--require-witnessed` refuses a run whose entry no held checkpoint covers.

A check that fails refuses the run with `"status":"chain-invalid"` and its named
findings, before any re-execution. `--require-witnessed` refuses with
`"status":"unwitnessed"`. The report's `runChain` member says what was checked,
and the line on standard error says what that establishes:

```text
{"exactBytes":"matches-record","exportVersion":4,"recordDigest":"sha256:…","retainedDisposition":"not-checked","runChain":{"checkpoint":{"checkpointVersion":"1","recordDigest":"sha256:…","sequence":1,"trail":"…"},"findings":[],"findingsTotal":0,"scope":"one-supplied-entry","status":"valid","witnessed":false},"scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"verified-inputs","targetsByClass":{"asserted":3,"record":0,"generated":0}}
verified-inputs: … The export's entry in the installation's chain of runs, at sequence 1, binds this run to those bytes. Only that one supplied entry was checked, which establishes nothing against the operator, who keeps the chain and could have written the entry with the export: a checkpoint held independently, supplied with --expect, would. The report gives the entry's checkpoint, for a holder to keep from now on.
```

The scope is `one-supplied-entry` with neither flag, `one-supplied-chain` with
a chain and no held checkpoint, and `checkpoint` with held checkpoints.
`witnessed` is true only when every check passed and a held checkpoint at or
after the entry's sequence covers it. With `--chain` the member also gives the
chain's line count and the checkpoint of its last line, and with `--expect` how
many checkpoints were supplied and matched, and the highest sequence they reach.

**What the chain establishes, and what it does not.**

- **Without a held checkpoint, nothing against the operator.** The operator
  keeps the store and the chain. They can write an entry with the export, or
  rewrite the chain from any point with every link recomputed, so that one
  supplied entry, or one supplied chain, is consistent. `one-supplied-entry` and
  `one-supplied-chain` show only that.
- **With a checkpoint held independently of the operator that covers the run's
  entry**, the entry is the one that existed when the checkpoint was made, and
  so is the digest of the record's bytes it names. Rewriting the record, moving
  the entry to another run, and removing or rewriting an entry the checkpoint
  covers all fail.
- **What no check here shows:**
  - that a checkpoint was held independently: `verify-run` cannot tell who
    supplied it;
  - when a checkpoint was made;
  - anything after the last held checkpoint: those entries are unwitnessed, and
    entries removed from the end since are missed unless a held checkpoint
    names them;
  - that the record's bytes are the ones the Runtime wrote, beyond what Runner
    kept;
  - that the inputs or the policy are true, or anything about the run's result
    that its record does not hold;
  - anything about a run that never reached the chain: an operator who controls
    Runner when a decision is made controls what it records.

Getting a checkpoint to a holder is the operator's step, as for the Runtime's
trail (ADR-0047 §2a, C4). Hand the export's checkpoint, or that of any line of
`GET /v1/run-chain` (its trail, sequence and SHA-256), to the counterparty, an
auditor, or a store the operator does not control. A checkpoint the operator
keeps proves nothing to anyone who does not trust the operator.

### Record signatures

With a signing key, the Runtime signs each chained record it writes (Runtime
ADR-0047 §2b): it appends a line to `signatures.jsonl`, beside its trail, that
signs the record's trail, its sequence and the SHA-256 of its exact line bytes
with Ed25519. Runner gives each run's Runtime the installation's key, keeps the
line, and `verify-run` checks it.

**The key.** The boot line's `signingKey` names an Ed25519 seed, 64
hexadecimal characters, the form `jpack audit key generate` and the gateway's
`keygen` write. Runner holds its path to the placement the Runtime's guide
states, and refuses to start otherwise:

- the path is absolute and clean, with no symbolic link anywhere in it, so it
  is the key's real path;
- no directory on the way to it is Runner's state directory, compared by device
  and inode, so it is outside every attempt's directory;
- it is one regular file with one name, owned by the user Runner runs as, and
  neither readable nor writable by its group or others.

A refusal names neither the path nor anything of the key. Runner never opens
the key, and never logs or stores its path. It passes the path to each
operational evaluation's Runtime as `JPACK_SIGNING_KEY`, and to nothing else: no
validation, lock, rehearsal or saved test is given it. The Runtime reads the
variable whatever configVersion a project declares, so the release's
configuration stays at configVersion `"3"`, unchanged. Naming the key in that
configuration, `audit.signingKey`, would need configVersion `"6"`, which no
released Runtime reads, and would put the path in every release and export.
The Runtime checks the key again as it opens it, and signs nothing with a key it
refuses. Runtime 0.25.0 and earlier do not sign, ignore the variable, and run
as before. Signing never refuses a run: a record left unsigned is still
recorded.

**What Runner keeps.** After an operational evaluation, Runner reads the
attempt's `audit/signatures.jsonl` as it reads the trail: a private regular
file, here of at most 16 KiB. It keeps the file's bytes exactly, newlines
included, as `run.auditSignatures`, in standard base64, in the transaction that
records the run completed. With no file, or an empty one, it keeps none, and
that is not an error. A file that cannot be read that way fails the run. What
the file holds is read only when a run is verified: a line that does not sign
the record leaves it unsigned, as a signature the Runtime could not write
does. `GET /v1/runs/{run}` shows the sidecar; run lists and briefs leave it
out. It is in the SQLite database, and is backed up and restored with it.

**Version 5** of the export carries `run.auditSignatures`. Version 4's readers
decode strictly and accept only versions 2 to 4, so the sidecar is a new
version's, and versions 2, 3 and 4 are byte for byte what they were. A run with
no sidecar is unsigned, and is exported as version 4 (or 3, or 2) whatever is
asked.

**Checking a signature.** `verify-run --public-key <hex>` checks the record's
signature under the key it is given: 64 lowercase hexadecimal characters, as
`jpack audit key public` prints them. The key is held to the guide's key check
before anything signed with it is read. It must be the canonical encoding of a
point of the curve whose order does not divide 8. Otherwise anyone could sign
under it, or a lenient reader would take it for another key, so a key of small
order, an encoding that is not canonical, and a value that is no point are
refused. The check follows the guide's "Record signatures, exactly", with the
trail known: an attempt's trail holds one record, the line `run.auditBytes`
holds. One key is in force, the one given, and no key is revoked. Each sidecar
line is read by the guide's rule, and each check it fails is a finding, with
the Runtime's names: `signature-invalid`, `signature-record-mismatch`,
`signature-no-record`, `rotation-invalid` and `sidecar-out-of-order`. A record
line that is chained but not as a trail's first line is `sequence-mismatch` or
`previous-mismatch`. A line of no shape the rule reads, a torn last line among
them, is unreadable, and is not a failure. The signature is checked by RFC
8032's equation without the cofactor and with a canonical scalar, as Go's
`crypto/ed25519` checks it once the key has passed the key check.

The report's `signature` member gives the status, the key's `keyId` and public
key, the findings, and how many sidecar lines were readable and unreadable:

```text
"signature":{"findings":[],"findingsTotal":0,"keyId":"21fe31dfa154a261626bf854046fd227","publicKey":"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a","readableLines":1,"status":"signed","unreadableLines":0}
```

- **`signed`:** a valid signature under the key covers the record, and no check
  failed.
- **`unsigned`:** no valid signature covers it, and no check failed. The
  sidecar holds no line that signs it, or the export carries none: an export
  asked for as version 4 or below, or a run recorded unsigned. That is not a
  failure, as a signature the Runtime could not write leaves its decision
  recorded. `--require-signed` refuses it, with status `unsigned`.
- **`invalid`:** a check failed. `verify-run` refuses the run, with status
  `signature-invalid`, before a re-execution, which then does not run.
- **`not-checked`:** a version-5 export checked without `--public-key`.
  `--require-signed` needs `--public-key`.

The member is in the report of a version-5 export, and of any export checked
with `--public-key`. The line on standard error ends with what the check
established.

**What a signature establishes, and what it does not.** A valid signature shows
that whoever held the key signed these exact bytes as the attempt's record.
Bytes changed or re-encoded, and a record rewritten with its chain entry, fail
it, unless signed again with the key. A signature binds the record, not the
run: a record taken from another run with that run's sidecar verifies, and the
run's entry in the chain of runs is what binds a record to its run. So:

- **Nothing against the operator,** who holds the key. A record the operator
  rewrote and signed again verifies like the one first written. Only a
  checkpoint someone else holds shows the difference
  ([above](#the-installations-chain-of-runs)).
- **Nothing once the key is copied or stolen.** Whoever holds a copy signs what
  they like. Each attempt starts a sidecar of its own, so a key is changed by
  configuring a new one, not by a rotation line. A verifier then checks each run
  under the key that was in force for it.
- Not when the record was signed, nor that a run was ever recorded.

**Reading the guide's rule for one record.** The rule depends on whether the
trail's lines are chained records, "the chain's own rule", which the guide
describes as being recognised by its members without stating their forms.
Runner reads a line as a chained record when it is one JSON object naming
`trail` (32 lowercase hexadecimal characters), `sequence` (an integer from 1 to
2^53−2) and `previous` (`sha256:` and 64 lowercase hexadecimal characters), each
once. Those are the forms the guide gives a sidecar line's trail and record,
and the forms Runner holds its own chain's entries to. It reads an attempt's
trail as chained from its first line, as a new trail is, so the line must be
sequence 1, linked to the SHA-256 of nothing. A legacy prefix, and a repair's
discontinuity, cannot occur in an attempt's directory, and are the Runtime's
`audit verify`'s to read.

## Deferred scope

Preparation sessions that Runner issues to a caller, seal verification, HTTP catalog support,
SQL async statement lifecycles, model HTTP calls, multi-record/page acquisition,
and pack-level class declarations remain separate work. Model classification
admission is implemented and tested for authenticated MCP sources; this does not
claim a model HTTP integration exists.


## Desk acquisition planning

`POST /v1/inputs/next` accepts source-only partial v2 input. It validates the
whole mapping, trusted profiles, fixed tool permissions and generated-value
admission before returning any request. It verifies and derives the supplied
prefix and returns either `{next:{name,kind,provider?,profile?,source?,arguments?}}`
or `{input,factsText,evidenceText?}`. A rejected or unavailable upstream claim
cannot feed raw values into a dependent request. Skipped sources supply no response.
This endpoint does not acquire, persist or evaluate, and does not reserve a
preparation session. Complete submissions are checked again at admission.

Desk provides named-source review and an advanced JSON mapping editor, with
explicit file selection and sequential acquisition using this endpoint. Trust
profiles enter through the installation's `--runner-input-profiles` file, passed
over Runner's private boot channel (maximum boot line 64 KiB). They cannot be
written through Desk's browser or project configuration. Each mapped run starts
with new case input and file selections. Acquisitions share one gateway session.

V2 Drive snapshots may have an empty legacy `proof.registry`: every receipt is
verified independently and session sealing is not asserted. V1 still requires
its existing registry container. Desk closes acquisition sessions as cleanup;
that does not add seal verification to Runner. Signed response bytes are retained
without JavaScript number conversion. Historical lineage and verification exports
are read from the stored run and never cause another source acquisition.
