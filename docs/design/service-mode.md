# Service mode and caller identity

Status: built for #214 (service mode) and #236 (a signed caller identity on the
control API), as one change. Nothing in it changes a companion's start.

Until this change Runner ran one way: as Desk's companion, started by its host,
given one boot line on stdin, listening on a loopback port the system chose, and
stopping when stdin closed. Every request on its control API held the owner
bearer the boot line carried. This record adds a second way to run it, service
mode, and a second credential for its control API, a caller's identity token.

The gateway's `serve --config` made the same choices for its own listener first
(its ADR-0025 and `go/identity.go`). Runner writes them again rather than
sharing code, with the standard library only. **The two are meant to stay
aligned:** a rule changed in one is changed in the other, or the difference is
added to section 9.

## 1. The start

`jpack-runner serve --config <path>` starts service mode. The command takes
exactly those two arguments; anything else is refused before any file is read.
It reads no stdin. It runs until it is sent an interrupt or a termination
signal, then gives requests in flight up to 35 seconds, as a companion does
when its stdin closes (`cmd/jpack-runner/service.go`).

When it is ready it prints the companion's readiness line on standard output,
`{"protocol":"jobs/1","url":"http://<listen>"}`, with the address it bound.
On standard error it writes the notes a companion writes (records set aside,
the input root's binding) and, for an address other hosts can reach, one line
more (section 3).

`jpack-runner` with no argument is the companion, byte for byte as before: one
boot line on stdin, a loopback port the system chooses, a lifetime that is
stdin's, the owner bearer on every request.

## 2. The configuration file

The configuration is one file, held to the rules the signing key file is held
to (`ReadServiceConfig`, `internal/runner/service_files_unix.go`):

- an absolute, clean path, with no symbolic link anywhere in it, through
  directories alone;
- one regular file with one name, owned by the user Runner runs as, which
  neither its group nor others may read or write;
- at most 128 KiB, read once through the handle it was opened by, which must
  be the file checked at the path, and refused if its attributes change before
  the read ends;
- not inside Runner's state directory, compared by device and inode (a state
  directory that does not exist yet holds nothing).

As for the signing key, the modes of the directories on the path are not
checked: whoever can write one of them before Runner starts can put another
file in the configuration's place, or the key set's. The operator keeps both in
directories no one else can write.

A refusal never names the path or anything the file holds, the `listen`
address included. On a system where
ownership and modes cannot be read the Unix way, service mode is refused, as
the signing key is.

The file is one JSON object, with each member once at every depth and every
name spelled exactly. A member Runner does not read is refused, and a name that
differs from one it reads only by case is refused naming the one it resembles.
Its members:

- **The boot line's members**, read by the boot line's rules: `dir`,
  `runtime`, `workspace`, `owner`, `inputRoot`, `inputProfiles`,
  `cloudConnections`, `gatewayConnections`, `requireTestedReleases`,
  `requireComparableFacts`, `requireSuppliedReviewedSet`, `signingKey`,
  `signingPublicKey` and `requiredCapabilities`.
- **`token`**, the owner bearer, at least 32 characters. A configuration
  without `identity` needs it; one with `identity` refuses it (section 5).
- **`listen`**, required: where Runner listens (section 3).
- **`identity`**, optional: the identity every request is held to
  (section 4).
- **`acceptOtherHostsOverPlainHTTP`**, optional, `true` or `false`
  (section 3).

`executorOwnership` and `executorAdmission` are refused: both bind Runner to
the host that starts it as its companion
([executor ownership](executor-ownership.md)), and in service mode no host
starts it.

Every refusal comes before anything is opened: no state directory is made, no
store is opened, nothing is bound, and nothing is printed on standard output.

## 3. Where it listens

`listen` is an IP address and a port, written exactly as Runner binds and
prints it: IPv4 in dotted decimal, IPv6 in brackets, compressed and in lower
case, an IPv4 address never in IPv6 form, and a port from 1 to 65535 with no
sign and no leading zero. Any other spelling is refused, saying what the form
is, without repeating the value given. A name, `localhost` included, a zone and an empty host are refused, so no
resolver decides where Runner listens. Port 0 is refused: a service is found at
the port its operator chose, never one the system picks. There is no default;
the examples use `127.0.0.1`.

- **A loopback address** (`127.0.0.0/8` or `::1`) is taken.
  `acceptOtherHostsOverPlainHTTP: true` beside one is refused, so the
  acknowledgment is never in a file with nothing to acknowledge.
- **Any other address** is taken only when the configuration has both an
  `identity` and `acceptOtherHostsOverPlainHTTP: true`. Either alone, or
  neither, is refused with a sentence naming what is missing, so a changed
  address alone never opens the control API to other hosts. The start then
  writes a line on standard error saying that other hosts can reach this
  Runner, that every request but `GET /v1/health` is held to an identity
  token, and that it speaks plain HTTP.

**Runner terminates no TLS.** It speaks plain HTTP, and it cannot check that
anything in front of it terminates TLS. Off loopback, unless TLS terminates in
front of Runner, anyone on the network between a caller and Runner can read an
identity token and replay it until it expires, read every request and answer
(a trigger key in the answer that issues it among them), and change a request.
Runner limits neither the rate of requests nor the number of connections; what
is in front of it should. Its listener keeps the companion's bounds: 5 seconds
to read a request's headers, 40 to read the request, 245 to write the answer,
30 idle, and 16 KiB of headers.

## 4. Caller identity

`identity` is an object with exactly these members (`ParseIdentityConfig`,
`internal/runner/caller_identity.go`):

- `issuer`: the token issuer, as its tokens name it in `iss`;
- `audience`: the audience this Runner is named as in `aud`;
- `subjects`: the subjects whose requests Runner takes, 1 to 256, each once;
- `keys`: the absolute, clean path of the issuer's key set file.

The issuer and the audience are 1 to 1,024 bytes, a subject 1 to 256, none
with a control character. The issuer holds no `#`, which ends it in the name a
run records for a caller (section 7); an issuer as OpenID Connect defines one
has no fragment, so holds none. A subject holds no format character either (a
zero-width space, a direction mark, a byte order mark: Unicode category Cf),
and no white space at either end, so that no subject reads as another.
Case is significant: `Ops-Service` and `ops-service` are two subjects, as a
token's `sub` is compared byte for byte. A subject is refused when it, or the
name a run records for it, is the configuration's `owner`, and when it is
`workflow` or begins `trigger:`: those are the names Runner records as the
requester of the owner bearer's runs, of a workflow's and of a trigger's, and
a caller's runs must be told apart from them.

**The key set file** is a JSON Web Key Set (RFC 7517) of the issuer's public
keys. Since the keys are public, others may read the file, but neither its
group nor others may write it, and its owner is Runner's user or root. It is
placed as the configuration is: no symbolic link in its path, one regular
file, outside the state directory. It is at most 1 MiB and read once, at
start. Runner fetches nothing; a key not in the file is a key Runner does not
know until the next start. A key that carries a private member (`d`, `p`, `q`,
`dp`, `dq`, `qi`, `oth` or `k`) is refused, rather than read for its public
half, since the file is one others may read. Each key carries a `kid`, once in
the set, and is
one of:

- RSA of at least 2,048 bits, with an odd public exponent from 3 to 2^31 − 1,
  for `RS256`;
- EC on P-256, for `ES256`;
- OKP Ed25519, for `EdDSA`.

A key of another kind is refused rather than skipped, since a file Runner reads
only part of is a file its operator misreads. So is a `use` other than `sig`,
`key_ops` that do not include `verify`, an `alg` that is not its kind's, and
an encoded member that is not the canonical unpadded base64url of its bytes.

**Every request is held to a token** (`(*Identity).check`), but
`GET /v1/health` (section 6):

1. exactly one `Authorization` header, with the `Bearer` scheme in any case;
2. a JWT in compact form, at most 8,192 bytes, of three parts, each the
   canonical unpadded base64url of its bytes (no padding, no line break, no
   bit the encoding does not read);
3. a header that is one JSON object with each member once, names no `crit`
   extension, and names a `kid` the key set holds;
4. an `alg` that is that key's own. The algorithm is the key's, never the
   token's word, so `none` is refused, and so is a token that names another
   algorithm for the key;
5. a signature that key checks out over the first two parts (for `ES256`,
   exactly 64 bytes, `r` then `s`, so a signature is not taken again widened
   by a zero byte). An ECDSA signature `(r, s)` has a second valid form,
   `(r, n − s)`, which Runner takes, as most issuers' signers produce either:
   a token can be presented with either signature. Nothing depends on a
   token's bytes being unique, since Runner keeps no token and reads no
   `jti`;
6. then, and only then, the claims, one JSON object with each member once: the
   configured issuer as `iss`; the configured audience in `aud`, a string or an
   array of strings; a `sub`; an integer `exp`, with no fraction or exponent,
   that has not passed; and, when present, an integer `nbf` that has. Each
   time is held with 30 seconds' leeway, and to the integers a JSON number
   holds exactly;
7. a `sub` the configuration lists.

A request that fails any of them is answered `401` with
`WWW-Authenticate: Bearer` and
`{"message":"Runner authentication required: <reason>."}`, one reason. No reason
repeats any part of the token, nor the key id or algorithm it names. Such a request is
refused before any route and is not journaled, as a request without the owner
bearer is not: it can come from anyone who can reach the listener.

**What the check establishes, and what it does not.** It establishes that the
token was signed by a key the operator put in the key set file, names the
configured issuer and audience and a listed subject, and is within its
validity window by Runner's clock. It does not establish who holds the token. A
subject is the issuer's name for a caller, not a person Runner knows. A token
is a bearer credential: whoever holds it can present it until it expires,
Runner keeps no record of the tokens it has seen (no `jti` is read), and
nothing revokes one before it expires short of removing its key from the file
and starting Runner again. Runner bounds no token's lifetime: an `exp` however
far ahead is taken, so how long a token lasts is the issuer's choice. Nor does
it read `typ`: a JWT of another kind that the issuer signs for this audience,
with these claims, is taken as an identity token, so the audience is one the
issuer uses for this Runner's control API alone. Every listed subject may do everything the control
API offers, as the owner bearer may: there are no permissions that differ
between callers. Runner keeps no token: no record, journal entry, answer or
line on standard error holds one.

## 5. The owner bearer and the identity

The narrowest choice: **with `identity`, the identity token replaces the owner
bearer on the whole control API.** A configuration with `identity` refuses a
`token` member, so no owner bearer exists for that Runner; a request that
carries some other bearer is refused as any token that is not one. There is one
credential per Runner, no question of which of two wins, and no way around the
identity for whoever reads the configuration file.

**Trigger keys are unchanged.** A direct event delivery,
`POST /v1/triggers/{trigger}/events`, holds an identity token in
`Authorization` and the trigger's key in `X-Trigger-Token`, as a companion's
holds the owner bearer and the key. The key still decides which trigger the
delivery is for, and the key's narrow read of its own occurrence is unchanged.

Without `identity`, service mode holds every request to the owner bearer in
`token`, as a companion does, and its `401` is the companion's, byte for byte.
A request from a browser (one with an `Origin` header) is refused with `403`
either way, as before.

## 6. Health

In service mode, `GET /v1/health` (and `HEAD`) answers with no credential:
`200` with `{"healthy":true}`, or `503` with `{"healthy":false}` once the
dispatcher has stopped after a storage error. It says nothing else: not the
store, its directory, the workspace, the owner, a digest or a version. A
browser request is refused with `403`. A companion serves no such route: there
it is `404` with the bearer and `401` without. `GET /v1/status` keeps its
credential, and its answer, which names the state directory, the workspace and
the owner, is unchanged.

## 7. Who is recorded

With `identity`, the caller the token was checked for, its issuer and
subject, is the requester of what the request creates:

- **A run** (`POST /v1/jobs/{job}/runs`) has as `requestedBy`, a member
  every run already holds, the caller's issuer and subject, written
  `<issuer>#<subject>`, as in `https://issuer.example#ops-service`. Since an
  issuer holds no `#`, the first `#` ends it, and no issuer and subject write
  another pair's name. That name is also the scope its `Idempotency-Key` is
  held in: one key from two subjects makes two runs, and so does one key from
  the same subject of two issuers, as after a restart with another `identity`.
  Without the issuer, that restart would answer the second issuer's caller with
  the first's run, or refuse it as a conflict. A run a trigger starts is the
  trigger's, `trigger:<id>`, as before.
- **The journal** names the caller as
  `{"kind":"caller","issuer":"<iss>","subject":"<sub>"}` on every entry of a
  change the request made: `release.previewed`, `job.created`, `job.upgraded`,
  `trigger.configured`, `trigger.paused`, `trigger.resumed`,
  `trigger.key-rotated`, `occurrence.received` or `occurrence.skipped` for a
  record delivery, `occurrence.cancelled`, `occurrence.reconciled`, `run.queued` and
  `admission.refused`. An event delivery's occurrence stays the trigger
  credential's. Such an entry has `entryVersion` `"2"`, since it holds members
  an entry of version `"1"` never does; every other entry keeps `"1"`.
- **Refusals** are coalesced per caller: an identical refusal within a minute
  is one entry for one caller, and a separate entry for another.
- **A job and a trigger** gain no member. A job's `owner` stays the
  configuration's owner; the caller who created or changed a job or a trigger
  is named by the journal entry of that change.

Without `identity` every record and entry is as it was: `requestedBy` the
owner, the `installation` actor, `entryVersion` `"1"`.

A caller is not a person, and the journal still records no review
([activity journal](activity-journal.md), answer 2): a subject names what the
issuer signed for, which Runner has no way to relate to a person.

## 8. The store, and going back

The store's schema stays `"3"`: no table or column is added, and nothing is
migrated. A caller is written in the `by` member the journal already keeps, its
entry version in the column it already has, and a run's `requestedBy` in the
member it already has. A store a companion, or a service without `identity`,
writes is byte for byte what an earlier Runner writes.

Raising the schema was considered and not done. A raise in every store would
make every store this Runner opens refuse an earlier Runner, a companion's
included, though nothing in it changed; a raise only where a caller wrote is a
storage change of its own, which this change leaves out. Going back is safe for
the records, since an earlier Runner writes its own entries as the
installation's and rewrites no entry, but not for what it serves: **an earlier
Runner serves an entry a caller made as `{"kind":"caller"}`, with no issuer or
subject**, under `entryVersion` `"2"`, since it does not read those members.
The stored entry keeps them, and this Runner serves them again. The release
notes say so.

## 9. Where Runner differs from the gateway

- Runner holds `sub` to the configuration's list of subjects; the gateway
  takes any subject its issuer signed for.
- Runner refuses a request with more than one `Authorization` header.
- Runner keeps nothing of a token, not even its digest, where the gateway's
  receipts name the token's SHA-256 digest; and Runner reads no delegation.
- Runner's refusals never repeat the key id or the algorithm a token names.
- Runner refuses a `listen` written any way but its own form, on loopback too,
  rather than rewriting it, and never repeats the value; and refuses port 0.
- Runner refuses an issuer that holds `#`, a subject with a format character
  or white space at either end, and a key that carries a private member.
- Off loopback, what answers without a token is `GET /v1/health`, and nothing
  else; it says only whether Runner is healthy.
- Runner's listener keeps the companion's timeouts (section 3).

## 10. Not in this change

- Scope identifiers (#237, since built: [scope identifiers](scope-identifiers.md)),
  retries (#238), counts on run records (#239), any storage change, more than
  one Runner per store, and metrics.
- Permissions that differ between subjects.
- Desk in front of a service with `identity`: Desk's proxy holds the owner
  bearer, and nothing in this change makes it carry an identity token.
- A key set fetched from the issuer, or re-read while Runner runs.
