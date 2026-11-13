# Dialect format 4.0.0

Status: slice 1 of #251. It adds a reading of the proposed dialect format
`4.0.0` (`spec/dialect.md` §2.2, the Spec's v0.5.2) and an in-process package
that prepares through it. Nothing in it changes how a run executes, how a
dialect of an earlier format is prepared, or how the Runtime is invoked.

Runner reads the proposed format `4.0.0` and agrees with the Spec's staged
vectors at v0.5.2. The reading is by the same maintainer as the Spec's
reference reader, so it is not an independent implementation, and no
conformance claim is made of it.

## 1. Where the format is read

- **`internal/dialect4`** holds what the format adds: the parameter types
  `decimal`, `string-list` and `address-range-list` (§2.2.2) and address
  ranges (§2.2.3); rule version `2.0.0`, its form and its match (§2.2.4); the
  derived values `duration`, `distance` and `speed`, in the integer and
  fixed-point arithmetic §2.2.5 gives them; each target's `derivation`
  (§2.2.6); and the counts and bounds of §2.2.7.
- **`sdk/dialect`** is the public package. It reads a document of the format
  that names no source and prepares cases through it, one at a time or in a
  batch ([its page](../sdk-dialect.md)). It depends on no network package and
  starts no process; a test holds it to that from `go list -deps`.
- **Runner's own preparer** reads each construct the format adds through
  `internal/dialect4` and the rest of the document as format `3.0.0`'s: the
  case's copy and then its rule, merged as two reads of the case with one read
  digest; a source read's rule of version `2.0.0`; the added parameter types,
  each list measured before its type; derivations in lineage; the case
  outcome's `rule`; `formatVersion` in place of `version`; and the unsigned
  parameters' case entry. This is how `jpack-runner verify-preparation` reads a
  dialect of the format, sources included, over fixtures.

Format `3.0.0` with sources stays where it was, byte for byte: the 47
hand-written preparation vectors and the artifact identity fixtures are
unchanged. Moving that preparer into the package is slice 2.

## 2. What is not admitted

Every path but `verify-preparation` refuses a dialect of format `4.0.0` with
`dialect_format_not_admitted` (HTTP 422): a release, a job (a release saved
before this check included), a preview, a delivery and a workflow. The
message names what it waits for: the Spec's workflow and recording schemas do
not admit the format yet (spec issue #324), so no record could name a
preparation of it. The gate is in `releaseDialectIdentity` and
`format4Form` (`internal/runner/dialect_format4.go`), not in the store.

Also refused by name, in both readers:

| what | code | why |
|---|---|---|
| the `extended` profile | `profile-unsupported` | its literal bound waits for spec issue #324; the `base` profile is read |
| a binding document with the format (verifier) | `capability-unsupported` | this verifier binds none for format `4.0.0` |
| a retained-column leaf in a copy | `input-malformed` | that leaf is format `3.0.0`'s |
| a dialect that names a source (package) | `sources-unread` | the Runner prepares those |
| a dialect of an earlier format (package) | `version-unread` | the package reads format `4.0.0` alone |

The `base` bounds held are §2.2.7's: at most 16 derived values, a literal of at
most 8192 bytes, a list parameter of at most 1024 elements each of at most 1024
bytes, measured before its type, and §12's counts over the case's rule (65
clauses, or 65 conditions listed by one `all` or `any`, are over). A `decimal`
is at most 128 characters as part of its type (§2.2.2), `parameter-invalid`
over it; Core §4.1's 256-character row does not apply to it.

## 3. What is bound, and where

- A preparation's bytes are §9.5's canonical form; its digest is what a
  record's binding would name. `mappingDigest` is the dialect's digest, the
  SHA-256 of its canonical form.
- The case's copy and its rule share one read digest, the SHA-256 of
  `canon({"copy": {facts, evidence}, "rule": rule})` (§2.2.6).
- The case outcome's `parameters` holds the case's parameters and `runAt`,
  which is the preparation's `verifiedAt`.
- The **parameter digest** is the SHA-256 of the case's parameters by name, in
  the canonical form `spec/dialect.md` §1 names `canon`, written
  `sha256:<hex>`. It does not hold `runAt`. The **keyed parameter digest** is
  HMAC-SHA-256 of the same bytes under a key the caller holds, labelled
  `hmac-sha256` with the caller's key identifier, of the grammar of the
  Runtime's keyed decision record (1 to 128 characters of `A-Z a-z 0-9 . _ - :
  /`). The package returns both and writes neither anywhere: no run of the
  format exists to record them. A value is bound as the case gave it: a
  list's elements in their order, a decimal's digits as written (`"0.75"` and
  `"0.750"` differ).

## 4. Results against the Spec's vectors

The Spec's `conformance/dialect/format-4/` at
`561b2a7245cf6ee13cb5be78da9c7a89f9adcdef` is vendored byte for byte under
`vectors/dialect-format-4/spec`, its three top-level files held to the Spec
commit's digests and every end-to-end file to the manifest's.

| set | through the verifier | through the package |
|---|---|---|
| end-to-end `case-rule-no-sources` | report, digests, outcomes and derivations exact | preparation byte for byte, digest `sha256:a8688b12…` |
| end-to-end `source-rule` | report, digests, outcomes and derivations exact | `sources-unread`, as designed |
| 167 rows | 167 agree | 162 agree; 5 of format `3.0.0` (two naming a source) are not read, as designed |

The rows are 151 framed by the README's base dialect, 9 whole documents of
format `4.0.0`, 5 of format `3.0.0`, an integer `version` of 4 and a selector
of two parts. No row of format `4.0.0` names a source, so `source-rule` is the
only coverage of a source's rule of version `2.0.0`.

Written by hand besides, as the Spec's reader tests write them: one case at
and one over each maximum of §2.2.7 and §12, through both readers, among them
a literal and 17 derived values counted before any form; derived values
against independent reckoning (durations against the time package, distances
against a float64 haversine within a millimetre, speeds against both);
`aheadWithin` and `compareParam` at their boundaries; a research read's rule
of version `2.0.0` refused; a copy's basis as a set and its targets in code
point order; the parameter digest's sensitivity and insensitivity, and the
keyed digest against the Runtime's vector; a batch against `Prepare` case by
case while several batches share one dialect, run once under `-race`.

## 5. What the Spec leaves open, and where a reading differs from its text

- **The extended profile.** §2.2.7 names `extended` ceilings (a literal of
  16384 bytes, a list element of 2048) against Core §4.1's table; spec issue
  #324 holds the literal-bound question. Both readers refuse the profile for
  the format until it is decided.
- **Records.** The workflow and recording schemas do not admit the format
  (spec issue #324); hence §2.
- **A copy's basis in format `3.0.0`.** §6.2 gives a copy's `basis` as "every
  entry's `Q` … distinct, ordered by code point". Runner's copy of format
  `3.0.0` keeps a pointer once per entry, in code point order, and is left
  byte for byte as it was; format `4.0.0` writes the set. No vector decides
  the format `3.0.0` case.
- **Not covered by a vector.** A source's list parameter over its bounds, and
  a calculator that did not compute under a rule of version `2.0.0`: the
  Spec's reader tests take re-signed copies of `source-rule` for these, which
  this change does not write. They are read by the code; nothing here tests
  them.

## 6. Cost

`BenchmarkPrepareBatch` (`sdk/dialect`) prepares the end-to-end vector's
case, a duration, a distance, a speed and an address-range match, in batches
of 1000. On an 8-core, 16-thread x86-64 desktop processor at 3.6 GHz under
Linux, Go 1.26, with other work on the machine: about 0.27 to 0.33 ms of wall
time per candidate in a batch, and 0.69 to 0.78 ms for one candidate on one
goroutine. A CPU profile of the one-goroutine benchmark puts about four
fifths of a preparation in the distance, nearly all of it the inverse sine,
found by bisection over the fixed-point sine as §2.2.5 gives it.
