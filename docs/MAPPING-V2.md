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
{"retainedDisposition":"not-checked","scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"verified-inputs","targetsByClass":{"asserted":3,"record":0,"generated":0}}
verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source.
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
targets, and whose rule reads a parameter that no signed request commits, it
gives the source, each such parameter's `name` and `kind`, and how many targets
the source maps, with a value or without. A case parameter is committed when
the request of any acquired source refers to it, since changing it fails that
source's receipt. A source's own parameter, read from an earlier source's fact,
is committed only by its own request. The kinds:

- `case`: a case parameter, as the operator supplied it.
- `local-file`: a fact of an earlier local-file source, which is asserted.
- `runAt`: the run's time, which no request can carry. Verification takes it
  from the export's own `preparation.verifiedAt`, so a verdict that reads it,
  such as `freshWithin`, rests on the time the export says it was prepared.

A fact of an earlier acquired source is derived from that source's signed
response, and is not listed; what that source's own rule reads is listed for
that source. A skipped source's rule read nothing, and is not listed. The
member is worked out from the frozen mapping, so the stored lineage is as it
was, and it is present only when a source is listed:

```text
"unsignedParameters":[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"}],"targets":1}]
```

When any target is asserted, the line on standard error says so: as above when
all are, or how many, for example `1 of the run's 3 inputs is the operator's own:
nothing here checks it against a source.` For the member above it adds `1 of the
run's 3 inputs was derived by a rule that reads a parameter from the case or a
local file, which no signed request commits: nothing here checks that
parameter against a source.` and `2 of the run's 3 inputs were derived by a rule
that reads runAt, which rests on the export's own verification time.` When there
is neither, the line is as it was.

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
{"retainedDisposition":"matches-re-execution","scope":"retained input derivation, audit binding, and the retained disposition against a re-execution by the release's Runtime; not sealed-session completeness or policy truth","status":"verified-disposition","targetsByClass":{"asserted":0,"record":3,"generated":0}}
verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true.
```

When either differs, the command exits 1, reports `"status":"disposition-differs"`
with `"retainedDisposition":"differs-from-re-execution"`, and names the failure
on standard error. `verified-disposition` adds one statement to `verified-inputs`:
the release's Runtime, given the verified inputs, decides what the record says.
It is not policy truth. Nor does it say more of asserted inputs than
`verified-inputs` does: the report counts them and the line names them the same way.

Add `--require-sourced` to refuse a run with an asserted fact or evidence target,
or with a target of an acquired source whose rule reads a `case` or `local-file`
parameter that no signed request commits. Verification is otherwise unchanged;
the refusal exits 1, reports its own status, and names on standard error how many
targets it refuses. It comes before any re-execution, so with `--runtime` the
executable is not run:

```text
{"retainedDisposition":"not-checked","scope":"retained input derivation and audit binding; not sealed-session completeness or policy truth","status":"inputs-asserted","targetsByClass":{"asserted":1,"record":2,"generated":0}}
runner: inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source
```

A target derived from an unsigned parameter is refused as `parameters-unsigned`.
When a run has both, the status is `inputs-asserted`, and the report's members
show both:

```text
runner: parameters-unsigned: 1 of the run's 3 inputs was derived by a rule that reads a parameter from the case or a local file, which no signed request commits, and --require-sourced refuses it: nothing here checks that parameter against a source
```

An asserted target is refused whether or not the run has a value for it: an
omitted value is the operator's say as much as a present one. Without the flag
nothing is refused that was accepted before. What it lets through:

- A `generated` target: its receipt says what the generating source answered,
  not that the answer is true.
- A `record` or `generated` target whose source was skipped: it has no value,
  rather than an asserted one, and no receipt.
- A target whose rule reads `runAt` and no other unsigned parameter. Every
  freshness rule reads it, so refusing it would refuse every freshness check.
  The report and the line say that such a verdict rests on the export's own
  verification time.

The export includes private case/request values and consumed grant salts;
distribute it only to intended reviewers. Public keys alone do not authenticate an export's
choice of policy or its historical verification time.

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
