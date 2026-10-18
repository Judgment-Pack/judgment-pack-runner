# Private operational execution payload

`judgment-pack-runner/execution-payload/1` is an experimental, private Runner
payload descriptor for the single-Pack run admission work in Runner #147. The
codec has no execution callers, HTTP routes, boot configuration, store schema or
permission effect. It neither interprets a Policy nor establishes approval,
dependency eligibility, authority freshness or an execution grant. It does not
change legacy releases, `releaseDigest`, preparation bindings or verification
exports. This profile is not an adopted portable Spec contract or a conformance
claim. It does not add public graph jobs.

`freezeExecutionPayload` takes a retained `Release`, an already prepared `Input`
and the operational `signingInputs` that would be passed to `evaluateWith`. It
returns an immutable descriptor: `Bytes()` returns a copy, and `Digest()` returns
`sha256:` followed by 64 lowercase hexadecimal characters, hashing exactly those
descriptor bytes with SHA-256. Component preimages remain with their owning
release and run. The caller must check the exact pinned Runtime executable,
release registration, preparation validity and every live execution condition
at the operational boundary. Creating this descriptor does none of those things.

The descriptor is a closed JSON object with these members:

| Member | Meaning |
| --- | --- |
| `profile` | Exactly `judgment-pack-runner/execution-payload/1`. |
| `releaseDigest` | The existing Runner `releaseDigest` of the full typed Release, separately from a Policy release-envelope digest. |
| `packDigest` | Release's SHA-256 of the exact Pack string's UTF-8 bytes, checked against those bytes. |
| `runtimeDigest` | Release's exact Runtime executable pin, with strict lowercase SHA-256 digest syntax. The codec does not read the executable. |
| `facts` | Mandatory reference to exactly `input.Facts`, including whitespace and JSON number spelling. |
| `evidence` | `{"presence":"omitted"}` when `len(input.Evidence)==0`; otherwise `{"presence":"supplied","value":REFERENCE}`. Supplied `null` has a reference and differs from omission. |
| `preparation` | Reference to the full retained typed Preparation, in the named Runner canonical encoding; omitted for a nil pointer. |
| `source` | Reference to the full retained typed SourceInput, in the same named Runner canonical encoding; omitted for a nil pointer. |
| `signing` | `{"mode":"off"}` when operational signing is absent; otherwise `{"mode":"ed25519","publicKey":KEY}`. KEY is the actual expected public key passed in `JPACK_SIGNING_PUBLIC_KEY`. |

A reference is exactly `{"encoding":ENCODING,"digest":DIGEST,"bytes":COUNT}`.
`COUNT` is the byte length of its preimage, and `DIGEST` hashes exactly that
preimage. The codec computes every reference; it never accepts caller-selected
counts, digests or descriptors. There is no descriptor decoding API in this
slice. Unknown/duplicate descriptor fields cannot be supplied through the typed
constructor.

`facts` and supplied `evidence.value` use `encoding: "exact-bytes"`. They are
bounded, checked for valid JSON syntax, UTF-8 and paired surrogate escapes, and
hashed without decoding, converting numbers, compacting, sorting or re-encoding
them. For example `9007199254740993`, `1.2300`, `1e+308`, `0`, `false` and `null`
remain exactly those bytes. Replacing `1.2300` with `1.23`, changing whitespace
or removing a trailing newline changes the commitment. Duplicate object members
are also committed exactly, without collapse; existing input admission and
Runtime own their interpretation and refusal. This codec's ability to describe
syntactically valid evidence `null` does not change existing input admission,
which refuses null evidence.

Preparation and SourceInput use encoding `judgment-pack-runner/canonical/1`,
a name for Runner's **existing**
`canonical(json.Marshal(typedValue))` machinery. This private name does not
change the encoding or make it the candidate Spec/Policy canonical profile.
Typed JSON encoding includes the existing `InputMapping.MarshalJSON` behavior,
its version-dependent members and omitempty rules. The existing helper decodes
using `json.Decoder.UseNumber` and marshals again with Go `encoding/json`:
object names sorted by Go string order, array order preserved, no insignificant
whitespace, JSON number lexemes preserved, strings escaped as Go JSON escapes
them (including `<`, `>`, `&`, U+2028 and U+2029). It performs no numeric
normalization, floating-point conversion or Policy safe-integer restriction.
Nil slices and pointers follow existing typed JSON encoding; unexported process
bookkeeping fields are outside that encoding. Typed encoding is bounded and
checked for duplicate JSON names and nesting beyond 64 before canonicalization.
This encoding is neither RFC 8785 nor the candidate Policy safe-integer domain.

The full descriptor contains only closed host-owned strings, bounded integers
and references, and uses the same existing Runner canonical helper. Facts and
evidence bytes never enter that canonicalization. The full Release uses the
existing typed canonical encoding, so `releaseDigest` continues to commit the
Pack, configuration and lock **string bytes**, plus all existing Release
metadata. The codec checks the full typed release encoding before computing the
legacy digest; it does not replace that identity with the descriptor digest.

The actual operational path writes Pack, configuration, lock and facts files
from their unchanged byte strings. It writes evidence and supplies `--evidence`
for every nonempty evidence slice, including `null`. For nonempty operational
preparation citations it writes `json.Marshal(preparation.Cites)` and supplies
`--cites`. A Runtime supporting preparation binding receives the existing
`preparationDigest` through `--binding`. Committing the full typed Preparation
covers those citations, their order, and the preparation binding's preimage.
Committing SourceInput additionally covers retained case, mappings, responses,
snapshots and imported Dialect/binding documents used in preparation checks.
Number spelling in raw fields inside those typed objects is preserved by the
named existing encoding; their whitespace is not preserved.

Operational signing is enabled by a nonempty `signingInputs.path`, matching
`evaluateWith`. The descriptor commits the expected Ed25519 public key, held to
Runner's existing public-key validation, because it changes the child's
environment. A key/public-key combination with only one supplied is refused;
the codec cannot freeze inconsistent operational signing inputs. It does not
open the key, test its current contents, sign anything or derive a governance
identity from it. Paths and secret key bytes are absent from the profile.
Changing the location of the same operational key does not change the digest.

The codec applies these byte bounds:

| Material | Maximum |
| --- | --- |
| Exact Pack bytes | 1 MiB |
| Exact configuration / lock bytes | 2 MiB each |
| Full Release typed JSON and canonical JSON | 8 MiB each |
| Exact facts / supplied evidence bytes | 2 MiB each |
| Preparation typed JSON and canonical JSON | 2 MiB each |
| SourceInput typed JSON and canonical JSON | 2 MiB each |
| Canonical descriptor JSON | 4 KiB |

Pack, configuration, lock and facts must contain one nonempty syntactically
valid JSON document. Evidence omission alone permits an empty slice. Bounds are
measured in bytes, include JSON quoting/escaping for typed encodings, and accept
the exact limit. Pack and Runtime pins are strictly checked for digest shape;
the legacy release digest and all reference digests are computed, never trusted
from input. Errors return no descriptor or digest and do not mutate caller-owned
material.

The small fixed vectors in
[`internal/runner/testdata/governance-payload`](../internal/runner/testdata/governance-payload/README.md)
publish exact descriptor bytes, digest values and component preimages for a
future Desk reader. They are synthetic codec vectors, not valid Policy releases
or operational evaluation examples. They were constructed independently once
with Python's standard library and a number-lexeme-preserving JSON writer;
tests consume fixed bytes and never regenerate them. Focused Go tests also
capture the existing `evaluateWith` file/argument/environment boundary with a
child that stops before evaluation. No live authority or installed host is used.
