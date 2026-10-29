# Dialect document checks: where the Runner and the Runtime's tools meet

Runner #86. The Runtime ships offline dialect document tools (`jpack dialect contract`,
`canonicalize`, `digest`, `validate`, `check`, and the MCP equivalents). The Runner reads,
binds, prepares and verifies dialect documents. This note records which side answers which
question, and how the two are held to each other.

## Two questions, two owners

- **Is the document well formed?** Canonical bytes, digest, version, form and internal
  references. The Runtime's document tools answer this, for the source kinds their contract
  names (`validationSourceKinds`: operation, selected-file, research). Their contract claims no
  binding, profile or preparation verification (`bindingVerification`, `profileCompatibility`
  and `preparationVerification` are false).
- **Can this installation run it?** Installed profiles and their digests, the binding of an
  unbound dialect at import, acquisition, retained responses, freshness, the preparation and
  its binding into the Runtime's record. The Runner answers this, and only the Runner.

A document the Runtime finds `document-valid` is not thereby executable. The Runtime encodes a
version-3 document whose source is of kind `http`, `sql` or `web` (encoding does not look at
sources) and its validator refuses the kind as unsupported; the Runner refuses each such kind
by name. A valid operation or research document is still refused by the Runner without its
installed profiles, its binding at import (when published unbound) and its source records.

## The boundary chosen

The Runner keeps its own in-process reader and calls no Runtime document tool when it previews,
prepares or runs anything. The alternatives were weighed:

- **A shared Go package.** The Runtime's dialect code is internal to its module, and importing
  the Runtime into the Runner, or the Runner into the Runtime, would make one release train
  wait on the other. Not taken: it creates the dependency the issue rules out.
- **A Runtime invocation per document or per stage.** A subprocess per preview or per
  acquisition stage adds latency and a failure mode to every run and still leaves the Runner
  needing its own reader to prepare. Not taken.
- **The Runner's reader, held to the Runtime's tools by shared fixtures.** Taken. The Runner
  does not grow a second validator: what decides document validity for an author is the
  Runtime's answer, and the Runner's reader exists because preparation has to read the
  document anyway. CI holds the two together.

## How they are held together

`internal/runner/dialect_tools_test.go` runs, in the `chain-tests` step (it must run and pass),
against the pinned retained-column companion Runtime (`scripts/checks/build-pinned.sh`, v0.39.0):

- `TestDialectDocumentsAgreeWithTheRuntimesDocumentTools`: every dialect document of the
  preparation vectors (`vectors/preparation`, which the specification publishes byte for byte
  as its dialect vectors), version 2, version 3 bound and version 3 published unbound, is
  `document-valid` to the Runtime and read by the Runner, with the same canonical bytes and the
  same digest. That digest is the identity each fixture records: the bound dialect's is its
  preparation's `mappingDigest`, the published dialect's is the one its binding document
  names, and a named (`formatVersion`) dialect's is the published identity a release records
  (`release.dialect`).
- `TestDialectDocumentRefusalsAgreeWithTheRuntimesDocumentTools`: a duplicated member, an
  unknown version, a fractional number, an unknown source member and an unknown source kind are
  refused by both, each with the Runtime's diagnostic code named.
- `TestADocumentTheRuntimeAcceptsIsNotExecutableOnThatAlone`: the separation above.

The comparison is made at the Spec revision the Runtime's tools are pinned to, which the tests
name (`dialectToolsSpecRevision`, `b7cc8906c23d92c63f4e42c4706cc1e21b9d324f`). When the Runtime
moves that pin the tests fail until the constant, and the review it stands for, move with it.
The retained-column leaf has its own pin (Spec `v0.3.0`, `docs/design/retained-columns-draft.md`).

## Not here

Profiles, credentials, acquisition planning, retained-response checks and run recovery stay in
the Runner and the Gateway. Core evaluation and portable workflow semantics do not change.
