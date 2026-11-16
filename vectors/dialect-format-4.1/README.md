# Dialect format 4.1.0 vectors (staged, proposed)

`spec/` holds the Spec's hand-written vectors for the proposed dialect format `4.1.0`, a paged
`operation` source (the Spec's RFC 0033, in Review), copied byte for byte from the Spec
repository's `conformance/dialect/format-4.1/` at commit `7510ea12d7079b60073aac498ca9c21ec1362cd4`:
its `README.md`, `manifest.json` and the 88 vector directories, one for each row of the RFC's
Conformance table. Nothing here was recorded from the Runner's code: every expected answer is the
Spec's, written by hand. Their relative links point into the Spec repository, not this one.

The files are held to their digests (`internal/runner/dialect_format41_test.go`): the two
top-level files to the SHA-256 their Spec commit gives them, and every file of a vector to the
SHA-256 the manifest names. Never convert their line endings (`.gitattributes`); never edit them. A
new Spec revision is taken by copying its files again, in one change, with the pin above.

`internal/runner/dialect_format41_test.go` runs every vector through
`jpack-runner verify-preparation`'s reader (`VerifyPreparation`), each held to the manifest's
expected answer. One vector, `paged-research-in-paged-session`, is not read, and why is in
[the design note](../../docs/design/dialect-format-4.md), section 7. The reading is by the same
maintainer as the Spec's reference reader: it is not an independent implementation, and no
conformance claim is made of it.
