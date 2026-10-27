# Selected RC2 owner coverage

These bytes come from the exact published Spec archive identified in README.md.
The corresponding focused Go tests execute these owners:

| Rows | Owner and scope |
| --- | --- |
| 72 conversions, 7 bounds, 7 shortest, 17 applicable reference fragments | ParsePlan, ParseFrame, Preview and Derive |
| 11 raw-value recipes | Exact literal suffix/list expansion, origin-specific UTF-8 byte and frame admission |
| 128 Base capability/selector rows | Closed capability decode, installed tuple, role minima and transport selector |
| 51 Base producer-consistency rows | Trusted tuple projection, carrier, complete original inventory, revisions and transformation |
| 27 preparations | Actual preparation consistency and exact output comparison |
| 28 preparation reader boundaries | Actual preparation reader, direct and sole-text custody, depth/numeric/malformed admission |

The capability corpus has 74 Extended rows and producer consistency has one
Extended row outside Runner's Base support. Frame-only Extended tests establish
only the internal frame reader's independent bounds.

The fixed synthetic installation exists only in `_test.go` code. Preparation
fixtures use a private reader checking the original signature, result commitment,
profile/request and freshness conditions of Dialect §5.2. Its last paragraph
assigns chain/caller/witness checks to Gateway; these preparation-only fixtures
are not full Gateway evidence. Public VerifyPreparation retains the full
acquisition reader, and a paired test proves it rejects the partial fixture.
Production HTTP rejects the synthetic installation before reading a body.

Nine synthetic preparation exports and eleven export reader boundary rows lack
Runner's complete release/run/chain/sidecar context. They are not claimed as
successful VerifyInputs or replay witnesses. Actual signed lifecycle tests use
an independently identified Runtime and signed producer fixture, both selected
Core 0.2 and Core 0.3 packs, SQLite restart and exports 2/3/4/5 with replay.
That producer fixture does not implement the future Desk adapter/history owner.

Eighteen source-input boundary rows have a separate enclosing owner. In
particular, the six `source-input-document-{depth,number,exponent}-{exact,one-over}`
rows place probes in a v2 root `snapshot`; Runner's existing v2 grammar refuses
that member and uses named sources. Those generic synthetic rows do not expand
Runner's grammar and are not represented as passing source-input conformance.
Valid named-source selected inputs remain covered by the actual lifecycle.
The other source-input rows are not counted in the preparation reader coverage.

Historical Draft resources remain unchanged. This owner mapping neither replaces
complete artifact accounting nor establishes a published Runtime release gate.
