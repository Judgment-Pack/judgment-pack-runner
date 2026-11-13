# Preparing in process: `sdk/dialect`

`github.com/Judgment-Pack/judgment-pack-runner/sdk/dialect` reads a dialect of
the proposed format `4.0.0` (`spec/dialect.md` §2.2, the Spec's v0.5.2) that
names no source, and prepares cases through it inside the calling program:
the case's copy and its rule of version `2.0.0`. It reaches no network, starts
no process and reads no file. It is experimental while the format is
proposed.

The package reads the proposed format `4.0.0` and agrees with the Spec's
staged vectors at v0.5.2. It is by the same maintainer as the Spec's reference
reader, not an independent implementation, and makes no conformance claim.

```go
d, err := dialect.Read(document) // once: holds the document to its form
if err != nil {
	return err // an *dialect.Error, its Code one of those below
}
for i, r := range d.PrepareBatch(cases, time.Now()) {
	if r.Err != nil {
		continue // this case was not prepared: r.Err is an *dialect.Error
	}
	p := r.Preparation
	_ = p.Bytes()    // the preparation, in §9.5's canonical form
	_ = p.Digest()   // sha256:<hex> of Bytes
	_ = p.Facts()    // the facts document, for the Runtime
	_ = p.Evidence() // the evidence document
	_ = p.ParameterDigest() // sha256:<hex> of the case's parameters, not runAt
	keyed, err := p.KeyedParameterDigest(keyID, key) // HMAC-SHA-256 under the caller's key
	_, _, _ = i, keyed, err
}
```

- `Read` once per dialect. A `Dialect` is read-only afterwards and safe for
  concurrent use; a case needs no setup of its own.
- `Prepare` prepares one case at an instant, truncated to the second, which is
  the preparation's `verifiedAt` and the `runAt` its rule reads.
  `PrepareBatch` prepares many at one instant on up to `GOMAXPROCS`
  goroutines and returns their results in the order of the cases, each what
  `Prepare` gives for it.
- The parameter digest holds the case's parameters by name, in `canon`: a
  list's elements in their order, a decimal's digits as written, and an
  integer by its value (`-0` as `0`). It does not change with the order of the case's
  members, its whitespace or its escapes, nor with `runAt`. The keyed form is
  labelled `hmac-sha256` with the caller's key identifier, as the Runtime's
  keyed decision record is; the package keeps the key nowhere. As the Runtime
  does, it refuses a key identifier that holds the plain parameter digest or
  the preparation's digest, as hexadecimal or unpadded base64: that catches
  one plausible mistake and is not a defence, so use an opaque identifier.

| code | meaning |
|---|---|
| `input-malformed` | the dialect or the case is not of its form |
| `input-over-bound` | over a bound of §12 or §2.2.7 (`base` profile) |
| `parameter-invalid` | a case parameter is missing or not of its type |
| `read-failed` | the case's copy or rule could not be applied |
| `admission-refused` | a target does not admit the class `asserted` |
| `profile-unsupported` | the `extended` profile, until the Spec decides its literal bound (spec issue #324) |
| `version-unread` | a dialect of format `3.0.0` or earlier |
| `sources-unread` | a dialect that names a source: `jpack-runner verify-preparation` reads those over fixtures |

The package prepares; it records nothing. The Runner does not admit a job, a
release or a workflow on a dialect of format `4.0.0` until the Spec's workflow
and recording schemas admit the format (spec issue #324), and the package
does not evaluate the Runtime.

The end-to-end vector's case, with a duration, a distance, a speed and an
address-range match, costs about 0.3 ms of wall time per candidate in a batch
of 1000 on an 8-core desktop processor, and about 0.7 ms alone; the distance
is most of it. [The design note](design/dialect-format-4.md) gives the
measurement, the row results and what the Spec leaves open.
