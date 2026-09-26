# Portable derivation rule — external contract

This is the specification a second implementation is built from, clean-room, without reading the
reference implementation in this directory. It exists because studies 006 and 007 derived a claim
from acquired bytes with one hand-written function (`derive_payload`) that was simultaneously the
ground-truth oracle, the control builder, the grader, and the source of the stage labels — so the
derivation was only ever checked against itself ([ADR-0002](../docs/adr/0002-trustworthy-input-acquisition-research-line.md),
the circularity this line exists to break). The correction is to make the derivation **data**: a
portable rule two independent implementations apply to the same artifact and MUST agree on. Their
agreement is the evidence that "layer 4 — derivation fidelity" is real and not a private convention
of one program. This is the agreement track's method (`python/` vs the reference runtime) applied to
the bytes→claim step.

## What a derivation rule is

A derivation rule maps one **acquired artifact** (a JSON value — the bytes a source returned, as
retained by the attestation core) plus named **parameters** to a **derived claim**: the facts,
evidence availability, and acquisition status a Judgment Pack would then evaluate, together with the
`reason` the derivation reached and the `basis` (which artifact fields it read). The rule carries no
evaluation semantics of its own and authorizes nothing; it says only how bytes become a claim.

It guarantees nothing about whether the artifact is true — only that, given the artifact and
parameters, the claim is a deterministic function of the rule. Two conforming implementations produce
the **byte-identical** canonical claim for every (rule, artifact, parameters).

## Canonicalization

Digest-level agreement is over a canonical serialization identical to the attestation core's `canon`
([../acquisition-proxy/SPEC.md](../acquisition-proxy/SPEC.md)): object members ordered by ascending
Unicode code point, compact separators, raw UTF-8, restricted to objects, arrays, strings of Unicode
scalar values, booleans, null, and integers in −(2^53−1)…2^53−1. A value outside that domain is not
part of any conforming claim.

## Pointers

A **pointer** is an RFC 6901 JSON Pointer. `get(artifact, pointer)` resolves it against the artifact
and yields either a JSON value or the distinguished result **absent** (any token names a member or
index that does not exist, or descends into a non-container). `absent` is not `null`: a member
explicitly set to `null` is present with value `null`.

## Parameters

A rule declares `parameters`, a map from name to type: `string`, `integer`, or `timestamp`. A caller
supplies a value per declared parameter. A **timestamp** is the exact string form
`YYYY-MM-DDThh:mm:ssZ` — four-digit year, zero-padded month/day/hour/minute/second, literal `T` and
`Z`, UTC, whole-second precision. Its instant is its count of seconds since `1970-01-01T00:00:00Z` in
the proleptic Gregorian calendar (the days-from-civil computation is standard and unambiguous). A
string not of this exact form has no instant.

## Rule document

```
{
  "ruleVersion": "1",
  "parameters": { "<name>": "string" | "integer" | "timestamp", ... },
  "clauses": [ <clause>, ... ]
}
```

`clauses` is an ordered, non-empty array. The **last** clause MUST have `when` of `{"op":"always"}`,
so every artifact matches some clause and the derivation is total.

A **clause** is `{ "when": <condition>, "claim": <claim>, "reason": "<label>" }`.

### Conditions

A condition is one of:

| condition | true iff |
| --- | --- |
| `{"op":"always"}` | always |
| `{"op":"exists","field":P}` | `get(artifact,P)` is not absent |
| `{"op":"equals","field":P,"to":LIT}` | `get(artifact,P)` is present and JSON-equals the literal `LIT` |
| `{"op":"equalsParam","field":P,"param":N}` | `get(artifact,P)` is present and JSON-equals the value of parameter `N` |
| `{"op":"isTrue","field":P}` | `get(artifact,P)` is the boolean `true` (strict: not `1`, not `"true"`) |
| `{"op":"isDecimalString","field":P}` | `get(artifact,P)` is a string matching `^(0\|[1-9][0-9]*)$` |
| `{"op":"freshWithin","field":P,"asOf":N,"maxAge":M}` | `get(artifact,P)` and parameter `N` both have an instant, and `0 ≤ (instant(N) − instant(P)) ≤ value(M)` seconds |
| `{"op":"all","of":[C,...]}` | every listed condition is true (empty list: true); evaluated left to right and **short-circuits** at the first false |
| `{"op":"any","of":[C,...]}` | some listed condition is true (empty list: false); evaluated left to right and **short-circuits** at the first true |
| `{"op":"not","of":C}` | `C` is false |

**JSON-equals** compares by JSON type and value: `1` (number), `1.0` (number, equal to `1`), `"1"`
(string), `true` (boolean), `null`, and structural equality for arrays/objects (objects compared as
unordered member sets). A comparison where either side is `absent` is false.

`freshWithin`'s `asOf` and `maxAge` name parameters (of type `timestamp` and `integer`); its `field`
is an artifact pointer. If either instant is missing, the condition is false — which is how a
malformed or missing time fails the freshness gate rather than passing it.

### Claim

```
{
  "facts":    [ {"pointer": FACTPTR, "from": ARTPTR}, ... ],
  "evidence": { "<requirement-id>": "present" | "absent" | "unknown", ... },
  "acquisitionStatus": "resolved" | "absent" | "unknown"
}
```

Each `facts` entry copies the artifact value at `from` to `pointer` in the (initially empty) facts
document, creating intermediate objects along the RFC 6901 path. A `from` that resolves to `absent`
is a rule error for that artifact (a well-formed rule only reads fields a preceding guard required);
implementations MUST treat it identically — see *Errors*. `evidence` and `acquisitionStatus` are
taken literally.

## Deriving a claim

1. Evaluate clauses in order against the artifact and parameters. The **match** is the first clause
   whose `when` is true. (The final `always` clause guarantees a match.)
2. `basis` is the sorted (by ascending Unicode code point) list of the distinct artifact pointers a
   leaf op **actually resolved** while evaluating the `when` conditions of clauses `0 … matchIndex`
   inclusive. "Actually resolved" is path-sensitive: because `all`/`any` short-circuit, a leaf behind
   a short-circuited branch is not read, so a clause that fails on an early term does not contribute
   the pointers of its later terms. A leaf op reads its `field`; `always` reads none; parameter names
   are not pointers. This makes the basis the fields the derivation genuinely consulted on the path
   it took — e.g. a `not_found` artifact that fails every `found`-branch clause on `/status` alone
   contributes only `/status` from those clauses, not the deeper fields they would have read.
3. Build `facts` from the match's `claim.facts` as above.
4. The **derived claim** is:

   ```
   {
     "facts": <the built facts document>,
     "evidenceAvailability": <the match's claim.evidence>,
     "acquisitionStatus": <the match's claim.acquisitionStatus>,
     "reason": <the match's reason>,
     "basis": <the basis list>
   }
   ```

   canonicalized. Two conforming implementations MUST produce identical canonical bytes.

## Errors

A rule that does not satisfy the *Rule document* shape (missing `ruleVersion` "1", empty `clauses`,
a final clause whose `when` is not `always`, an unknown `op`, a parameter of unknown type, a value
outside the canon domain, or a matched `claim` whose `from` resolves to `absent` for the given
artifact) is **rejected**: `derive` raises rather than returning a claim, and a conforming
implementation MUST reject the same rules and artifacts. Rejection is deterministic and is not a
claim.

## Agreement interface

A conforming implementation provides a command that reads exactly one JSON object on standard input:

```
{"rule": <rule document>, "artifact": <artifact value>, "params": {<name>: <value>, ...}}
```

`rule` and `artifact` are required; `params` is optional and defaults to `{}`. The whole request
must be well-formed JSON containing no lone surrogate in any string — a lone surrogate is invalid
Unicode and **rejects the request**, distinct from the number domain (an artifact may hold a float,
which rejects only if it is copied into the claim; no string anywhere in the request may hold a lone
surrogate). A request missing `rule` or `artifact`, not a JSON object, or otherwise malformed is
rejected.

If the rule and artifact derive a claim, the command writes the derived claim's **canonical bytes**
(`canon` of the derived claim) to standard output with no trailing newline and exits `0`. On any
rejection — a malformed request, or a rule/artifact rejected by *Errors* — it writes nothing to
standard output and exits with a nonzero status. Nothing else is written to standard output. This is
the interface the agreement harness diffs across implementations.

## Conformance

Two implementations conform iff, over the same rule and the same set of (artifact, parameters)
cases, they produce byte-identical canonical claims for every case a valid rule derives, and both
reject (nonzero, empty stdout) every case the *Errors* section rejects. The agreement harness in
this directory drives the reference implementation and any second implementation over `corpus/` and
diffs their stdout and exit status; a diff is a defect in one implementation or an ambiguity in this
document.
