# Calculated values

Status: accepted and implemented. Decides issue #14.

A value a formula produced already reaches a pack as a fact: a mapping fetches it
from a calculator exposed as an MCP tool, through an `operation` source, and Runner
verifies the receipt. What the mapping and the lineage did not say was that the
value was calculated, by which calculator, from what, from tables how old, and why
there is no value when the calculator could not compute one. Each mapping author
invented a status for the last, and nothing held the rest.

This record decides the five questions of the issue. The contract is in
[MAPPING-V2.md](../MAPPING-V2.md#calculated-values).

## 1. A marker on the profile, not a source class

A calculator is declared by the installation, on the trusted profile that allows
its tools, as `calculator: {name, version}`. The profile's class stays `record`;
a calculator profile must be a `record` profile of shape `mcp`.

Declaring it, the installation asserts that each tool the profile allows computes
a deterministic function of the inputs it echoes and the tables it reports, and
calls no model. Runner cannot check that; it is the installation's statement, as
`record` is. A calculator that calls a model is not a calculator in this sense: its
profile is `generated`, and admission treats it so.

Why not a fourth class. The class names the acquisition channel, and the channel
is a signed read, as for any record. What is new is a statement about how the
bytes were made. Admission defaults and the propagation of generated influence
are keyed by class: a fourth class would make every target opt in to a value
that is, as a channel, a record. Restricting classes per pack is later spec work.

Why the profile and not the mapping. The profile is the installation's authority,
and a mapping is a caller's. A mapping cannot make a tool a calculator. The
calculator's version is pinned: a new version is a new profile, so a new release,
as a new adapter version is.

## 2. What a calculator's answer carries

The value a read applies to (after its `unwrap` steps) is an object with a
`calculation` member of exactly four members:

```json
{
  "calculation": {
    "calculator": {"name": "fx-convert", "version": "2.1.0"},
    "status": "computed",
    "inputs": {"amount": "1200.00", "currency": "EUR"},
    "asOf": {"ecb-rates": "2026-10-01T14:00:00Z"}
  },
  "converted": "1302.36"
}
```

- `calculator` is the name and version the profile pins.
- `status` is `computed`, `input-missing` or `cannot-compute`.
- `inputs` echoes each input the calculator used, by name.
- `asOf` gives, for each table the calculator read, the instant its contents are
  as of, in the rule's timestamp form.

The value itself is read by the source's rule or copy, as before.

## 3. The mapping binds what is echoed, and preview refuses it otherwise

A source whose profile is a calculator declares how its answer is bound:

```json
"calculation": {
  "inputs": {"amount": "invoiceAmount", "currency": "invoiceCurrency"},
  "tables": {"ecb-rates": 86400}
}
```

Each echoed input names a parameter of the source: a case parameter, or one the
source takes from an earlier source's derived fact. Each table names the oldest
its contents may be, in seconds, from 1 through 315,360,000. A source with a
calculator profile and no `calculation`, or with `calculation` and another
profile, fails preview and refuses the run before any acquisition. `runAt` cannot
be bound: it never reaches a request, so a calculator cannot echo it.

Runner holds the answer to the binding when it prepares the input:

- the calculator and version are the profile's;
- every echoed input is bound, and equals its parameter's value as the same
  canonical JSON (`"7"` is not `7`);
- every reported table is named in the mapping, with an instant;
- for `computed`, every bound input is echoed, and every named table is reported,
  as of no more than its limit before the verification instant and no more than
  30 seconds after it, the skew allowed for an acquisition's own times.

A failure of any of these fails preparation, as stale data and a mismatched
request do: the answer is about other inputs, or rests on tables too old, and is
not an answer about this case.

## 4. A calculation that failed is `unknown`, under its own reason

When `status` is not `computed`, Runner does not apply the source's rule or copy.
It derives the claim itself: no facts; each evidence requirement the source maps
is `unknown`; the acquisition status is `unknown`; the reason is
`calculation-input-missing` or `calculation-cannot-compute`; and the basis is
`/calculation/status`. Sources that depend on its facts are skipped as
`dependency-unavailable`, as for any unresolved source.

Why `unknown` and not a preparation failure. That the calculator could not
compute is an answer about this case, and usually one a person should see; the
pack routes it. A stale table or an answer for other inputs is a fault of the
source, and faults stop the run. Why Runner and not a guard in the rule: a guard
is optional, can be wrong, and names its reason freely. Runner applies one rule to
every mapping, and a reviewer reads one vocabulary.

## 5. What the lineage records

Each lineage entry of a calculated source carries `calculation`:

```json
"calculation": {
  "calculator": {"name": "fx-convert", "version": "2.1.0"},
  "status": "computed",
  "inputs": [
    {"name": "amount", "parameter": "invoiceAmount", "source": "case", "pointer": "/amount"},
    {"name": "currency", "parameter": "invoiceCurrency", "source": "case", "pointer": "/currency"}
  ],
  "asOf": {"ecb-rates": "2026-10-01T14:00:00Z"}
}
```

`inputs` lists the inputs the calculator echoed, by name, each with where its
value came from: the case and the case pointer, or the earlier source and the
pointer of its derived fact. The echoed values are not repeated; the retained
response holds them. Offline verification recomputes this member with the rest
of the lineage.

## What this does not establish

- That the calculator is deterministic, or calls no model. The installation says so.
- That its arithmetic is right, or its tables correct.
- That it used only the inputs it echoes. The echo is the calculator's testimony,
  carried in the signed response; Runner holds the echo to the case, and cannot see
  past it.
- That an `asOf` instant is true. It too is the calculator's statement; Runner
  holds it to the limit the mapping states.

## Compatibility

A profile without `calculator`, a source without `calculation` and a lineage entry
of any other source encode as before, so their digests, releases and exports are
unchanged. Desk passes the profile file and the mapping through unchanged; its
lineage view does not show the new member yet.

Out of scope, as the issue says: arithmetic in a derivation rule or a pack, and
operations over HTTP.
