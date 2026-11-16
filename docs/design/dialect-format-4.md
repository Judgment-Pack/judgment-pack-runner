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

## 2. The run path, and what is not admitted

A release, a job and a run are on a dialect of format `4.0.0` or `4.1.0` as on
one of format `3.0.0` (#247, `internal/runner/run_format4.go`), with what the
Spec's staged 0.6.0 records give them (spec PR #332): the run's preparation is
written in the form of the dialect-recording schema of version 2
(`formatVersion` in place of `version`, the case's `rule` outcome, a target's
`derivation`, a paged source's `paging` outcome) and read back from the store
in the same form; the release digest of a release on either format is RFC
0020's, the SHA-256 of the canon bytes of the whole retained release, and every
other release keeps the pinned encoding, byte for byte. A release on either
format outside the canon domain has no digest and is refused `input-malformed`
when it is saved; the reader that would digest one anyway (an export, a
workflow's freeze) fails, and a comparison of digests never matches it. The
canon domain admits integers alone, so a release whose Runner-added members
(`validation`, `preview`, `testEvidence.report`) hold a fraction cannot be
saved on either format: conformant to the Spec, and a limit to know when a
pack's preview reports one. A paged source's loop and the job's cursor are
[paging.md](paging.md)'s. `vectors/dialect-recording-2` holds the Spec's schema,
pinned, and a run's recorded preparation is checked against it.

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
  /`). Following that record's rule, an identifier that holds the SHA-256 of
  the parameters' text or of the preparation's bytes, as hexadecimal digits
  of either case or unpadded base64 of either alphabet, is refused; any other
  derivation is not detected. The package returns both and writes neither
  anywhere: no run of the format exists to record them. A list is bound with
  its elements in their order and a decimal with its digits as written
  (`"0.75"` and `"0.750"` differ); an integer is bound in `canon`'s form, by
  its value (`-0` is bound as `0`).

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
  the format until it is decided. Both scan for the declaration only in a
  dialect of at most 262144 bytes, as the Spec's reader does
  (`admit_envelope`); a larger one is `input-over-bound` whatever it
  declares. §11's text does not give that size; the Spec's reader does.
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

## 7. Format 4.1.0: a paged source

Runner reads the proposed format `4.1.0` (the Spec's RFC 0033, in Review),
format `4.0.0` with a paged `operation` source, where it reads format `4.0.0`:
in `verify-preparation` alone (`internal/runner/dialect_format41.go`). The
reading is by the same maintainer as the Spec's reference reader: it is not an
independent implementation, and no conformance claim is made of it.

- **The form** (RFC 0033 section 2): `paging` on an `operation` without a
  calculation, exactly `next`, `noNext`, `fills`, `items` and `maxPages`, and
  `fills`'s parent an object written in the source's `arguments`, within no
  template form, `input-malformed` otherwise; `fills` naming a member a
  profile does not let a reference fill, `profile-mismatch` when the source is
  prepared. `paging`, and an acquisitions member that has `pages`, are refused
  `input-malformed` in every other format, on every path.
- **The walk** (sections 3 to 5): from page 0, each page finished before the
  next, in the order section 4 gives the findings; each request after the
  first is the first with `fills` set to the reference the page before named;
  the loop ends at `no-next` or at `maxPages`. A page retained after the end is
  `acquisition-unexpected`.
- **The joined value and the read** (section 6): `{"end", "items", "last",
  "pages", "values"}`, the read applied once to it.
- **The bounds** (section 7), measured in §11's third phase in a document of
  the format alone: `maxPages` when an integer, the pages retained, each
  page's `result` nesting; a next reference's bytes when read; the joined
  value's nesting when formed.
- **The preparation** (section 8): `formatVersion` `"4.1.0"`, the paged
  source's outcome `paging`, its lineage's `receipts`, and `cites` in the
  order of `sources` whenever a paged source is declared.
- **Sessions**: §5.2's two forms hold over the unpaged sources' receipts; a
  receipt of any other source, a research source's included, that names a
  paged source's session is `acquisition-sessions`.
- **A binding document** is read for format `4.1.0` (the vector
  `paged-unbound-paged` binds one); format `4.0.0` keeps its refusal
  (section 2).
- `verify-preparation --capabilities` names `"4.1.0"` among `formatVersions`,
  interface version 4.
- The `sdk/dialect` package is unchanged: it refuses a dialect that names a
  source, `sources-unread`, so every paged dialect.

The Spec's `conformance/dialect/format-4.1/` at
`7510ea12d7079b60073aac498ca9c21ec1362cd4` is vendored byte for byte under
`vectors/dialect-format-4.1/spec`, its two top-level files held to the Spec
commit's digests and every vector's files to the manifest's
(`internal/runner/dialect_format41_test.go`). Of the 88 vectors, 87 agree:
the result and its code, and of a reading the digest, the facts, each named
outcome with its `paging`, the cites in order and the unsigned parameters.
`paged-research-in-paged-session` is not read: its research source writes the
typed `results` contract, which Runner's research reading does not read, so
its dialect is refused `input-malformed` before the paged session is
reached. The rule it checks is the one `paged-session-shared` checks for an
unpaged source, in the same place (`admit`, `prepareV2Mode`).

Nothing here prepares a paged source for a run: a run's acquisition, the
cursor kept between runs and the run record's pages
([paging.md](paging.md), section 4) wait for the format to be admitted on
the run path (section 2).
