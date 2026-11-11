# Supplied reviewed sets

Status: implemented (#210, part 2). The README's
[Supplied reviewed sets](../../README.md#supplied-reviewed-sets) is the operator's
account; this note records the decisions and why.

A release's own lock shows only that its pack has not changed since the release was
made: Runner generates the configuration and runs the Runtime's `packs lock` over it.
`POST /v1/previews` can now also take `reviewedSet`, a reviewed-set lock made outside
Runner and the decision id the pack is locked under in it. The release is checked only
when the pinned Runtime's `packs covers` (Runtime 0.41.0, ADR-0072) says that lock
covers the pack's exact bytes under that id. Absent, nothing changes.

## Who reads the lock

The Runtime does. Runner writes the lock's bytes, as given, to
`reviewed-set.lock.json` in the release's private directory, with mode 0600 and
`O_EXCL` as it writes `pack.json`, and passes both by relative path as arguments to the
pinned Runtime, with no shell. Runner does not parse the lock: a second reader of the
format could accept what the Runtime refuses, or the reverse. Before anything is
written, the lock is held to 1 byte to 1 MiB, the bound every Runtime reader of a lock
applies, and the id to the decision id form, at most 128 characters. That form begins
with a lowercase letter, so the id is never read as a flag, which matters because the
Runtime's flag parser takes the next argument as a value even when it begins with `-`.

`packs covers` is given no `--config`. A release's configuration is Runner's own and is
never the one the supplied lock pins, so `config-drift` would always be reported. The
check therefore says that this lock pins these bytes under this id, and no more: not
that a project's configuration declares the pack, and not that the lock is the one a
given project holds. The caller chooses the lock.

## Reading the answer

The answer is data, read to at most 64 KiB, and nothing is taken for a yes:

| Answer | Outcome |
|---|---|
| exit 0, status `valid`, `findings` present and empty, the id, pack digest and lock digest Runner gave, and an entry whose digest is the pack's | checked |
| exit 1, status `invalid`, findings whose names are of the decision id form, about the id, pack and lock Runner gave | 422 `pack_not_in_reviewed_set`, naming the findings |
| status `error` at exit 1 or 4, or `unsupported` at exit 2, whose first diagnostic code is of the `JPS-` form | 422 `invalid_reviewed_set`, naming the code |
| anything else, among it every answer whose `command` is not `packs covers` | 422 `reviewed_set_unsupported` |

A refusal carries only the code or finding names, which are held to their forms and to
64 characters. Neither the lock's bytes nor the Runtime's messages reach a refusal, the
journal or a log. Nothing is saved on any refusal.

## Detecting the capability

By the answer's `command` member. A Runtime before 0.41.0 answers an unknown `covers`
as `packs` does, with `command: "packs"` and `JPS-INVOCATION-ARGUMENTS` at exit 3, and
only a Runtime that has the verb writes `command: "packs covers"`. Asking the version
first would take a second invocation and say nothing of a development build. Exit 3
alone is also what the verb answers to an invocation it refuses. So no version is
compared, and a Runtime that lacks the verb is refused as `reviewed_set_unsupported`,
never passed.

## What the release keeps, and each attempt

The release keeps `suppliedReviewedSet`: the id, the lock's exact bytes and their
digest. Its own configuration and lock are unchanged and remain all that an evaluation
is given.

`packs covers` is not asked again at each attempt. Its answer depends only on the lock's
bytes, the id, the pack's bytes and the Runtime. Each attempt already checks the pack's
digest and the Runtime's digest, and now also checks that the supplied lock is the
bytes its digest names; an attempt of a release that fails it is not evaluated. Under
manual admission the same check is part of the release check every binding, launch and
run admission makes (`manualRelease`). Asking again would repeat a pure function of
bytes already checked by digest.

## What a run says

`reviewedSetOrigin` gains `supplied`, served with `suppliedLockDigest` when the run's
result or record says `reviewed: true` and its release keeps a supplied reviewed set.
Both are read off the run and its release whenever the run routes serve it, as
`release` already was, so nothing is stored on the run and earlier runs are served the
same way. A run whose release cannot be read is refused rather than served as resting
on the release's own lock.

## Admission

Under manual admission the option is refused as 409 `manual_profile_unsupported`: the
executor admits no `packs covers` invocation.

The boot member `requireSuppliedReviewedSet`, following `requireComparableFacts`,
refuses a new job from, or an upgrade to, a release not checked against a supplied
reviewed set, or one whose supplied lock is not its digest's, as 409
`release_reviewed_set_required`. Repeating the creation of an existing job still
returns it. The member is `true` or `false`; `null` or another spelling of its name
refuses the boot.

## Store and exports

The releases table holds each release as one JSON record, and the member is omitted
when absent, so existing records read unchanged and their release digests do not
change. A verification export carries the release, and so the supplied lock, under the
release digest a verifier is given; an earlier Runner's `verify-run`, which reads the
release strictly, refuses an export whose release has the member.

## Graphs

Runner releases packs only, so a reviewed set is never asked to cover a graph, and
`packs covers --graph-id` is not used.
