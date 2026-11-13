# Dialect format 4.0.0 vectors (staged, proposed)

`spec/` holds the Spec's hand-written vectors for the proposed dialect format `4.0.0`
(`spec/dialect.md` §2.2), copied byte for byte from the Spec repository's
`conformance/dialect/format-4/` at commit `561b2a7245cf6ee13cb5be78da9c7a89f9adcdef` (the Spec's
v0.5.2): its `README.md`, `manifest.json`, the 167 rows of `vectors.json` and the two end-to-end
vectors `case-rule-no-sources/` and `source-rule/`. Nothing here was recorded from the Runner's
code: every expected answer is the Spec's, written by hand. Their relative links point into the Spec
repository, not this one.

The files are held to their digests (`internal/runner/dialect_format4_test.go`): the three top-level
files to the SHA-256 their Spec commit gives them, and every file of an end-to-end vector to the
SHA-256 the manifest names. Never convert their line endings (`.gitattributes`); never edit them. A
new Spec revision is taken by copying its files again, in one change, with the pin above.

What reads them:

- `internal/runner/dialect_format4_test.go` runs both end-to-end vectors through
  `jpack-runner verify-preparation`'s reader (`VerifyPreparation`), and every row through it and
  through the `sdk/dialect` package, each held to the row's expected answer;
- `sdk/dialect/dialect_test.go` prepares `case-rule-no-sources/` through the package, byte for byte,
  and holds `source-rule/`'s dialect to the package's refusal of a dialect that names a source
  (`sources-unread`).

Runner reads the proposed format `4.0.0` and agrees with these staged vectors at v0.5.2. The
package and the Runner's reading of the format are by the same maintainer as the Spec's reference
reader: they are not an independent implementation, and no conformance claim is made of them.
