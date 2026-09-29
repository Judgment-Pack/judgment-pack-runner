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

An input in neither form is refused at the first source that leaves both: two
acquisitions that share a session beside one that does not, or an acquisition in
a session of its own that is not its first call. One acquisition alone is in both
forms. The form is read from the receipts and from nothing else, so whoever
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
It reports `verified-inputs`, not policy truth or evaluator re-execution. The
export includes private case/request values and consumed grant salts; distribute
it only to intended reviewers. Public keys alone do not authenticate an export's
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
