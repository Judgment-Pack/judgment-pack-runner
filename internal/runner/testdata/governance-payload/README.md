# Private execution-payload vectors

These frozen synthetic vectors describe
`judgment-pack-runner/execution-payload/1`; they are private experimental host
fixtures, not an adopted portable Spec format or conformance claim. See
[`docs/governance-payload.md`](../../../../docs/governance-payload.md) for the
exact encoding, bounds and execution limits.

`manifest.json` names each vector's exact facts/evidence files, optional typed
preparation/source files, optional operational expected public key, exact
descriptor file and SHA-256 descriptor digest. Omitted manifest evidence means
no evidence file/argument. `null.json` is exactly four bytes; it represents
supplied evidence in `numbers-null`. The same file represents facts `null` in
`scalar-null`. Facts files deliberately distinguish a trailing newline, large
integer values and decimal spellings. `numbers-omitted`, `other-integer` and
`other-spelling` differ in their exact facts preimage.

`release.json` is the typed legacy Release in Runner's existing canonical
encoding. `preparation.json` and `source.json` are also their typed canonical
preimages. Those files have no trailing newline. They retain number lexemes
`9007199254740993`, `1.2300` and `1e+308` in metadata as well as facts. Synthetic
citations, imports and responses do not claim verification. The release's
Runtime pin hashes a synthetic text marker, not an executable. The signing
public key is the public RFC 8032 test-vector key; no private key is included.

The manifest itself is pinned by the focused Go test. Vectors were constructed
once independently of the Go implementation, using Python's standard library
with preserved JSON number tokens and explicit Go-compatible string escaping.
Tests compare with those fixed files; they never rewrite their expected bytes.
