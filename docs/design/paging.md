# Paging over a source, with a cursor kept between runs

Status: decided for #247, answer A (section 3); nothing in it is built. The
dialect amendment it needs is judgment-pack-spec #328; Runner builds section 4
after that amendment's text and vectors are settled. Section 5 is kept as the
record of the answers not taken.

## 1. What #247 asks

A job's source may declare a paging rule: where the next page's reference is
read from an answer, a bound on pages per run, and what ends the loop. A cursor
is kept between runs, per job and source: for a change query the reference the
last page returned, for a time-window query the window's end. It is stored with
the run that advanced it, and advances only when that run completes; a failed
or interrupted run leaves it. Each page is its own receipt, and the run record
counts the pages and names their receipts. Throttling inside the loop follows
the job's retry policy, and the pages already received are kept, not asked for
again. Memory is bounded by a page, not by the run. A reference the source says
has expired is answered by a full re-read, recorded as such.

## 2. What main holds

**Runner reaches a source before a run exists.** A run's input is frozen when
the run is queued, and the dispatcher only invokes the Runtime on it. Runner
calls a source while it prepares an occurrence of a trigger whose input is
`mapped-sources`: over a Gateway connection that is not durable, one `/acquire`
per operation source (`prepareLegacyOccurrence`, `background.go`); over a
durable one, through the source worker (`durable-sources.md`). The occurrence
is then dispatched into a run. [Retries](retries.md), section 1, made the same
correction for the retry policy. So "the run that used the cursor" is the run
made from the occurrence that acquired the pages: an advance has to be carried
from the occurrence into its run, and written in the transaction that records
the run completed (`finishRunIn`, `run_chain.go`), which is fenced by the
run's claim ([store-postgresql.md](store-postgresql.md), D3).

**One acquisition per source.** An input holds one retained response per source
name (`sources`, `mapping_v2_types.go`). [MAPPING-V2.md](../MAPPING-V2.md)
says "Each source is one acquisition", and lists multi-record and page
acquisition under its deferred scope. The sessions rule admits several sources
in one session at differing call indexes, or each alone at call index zero; it
has no form for several acquisitions of one source. A receipt that attests a
page (`acquisition.pageItems`) is refused (`mapping_v2_proof.go`), and the
dialect's check refers to this repository's deferred scope for it
(spec/dialect.md §5.2, check 1). `verify-run` recomputes a preparation from the
retained responses under the dialect's checks, so a page the dialect does not
define cannot be verified offline.

**The Runtime takes one facts document.** An evaluation is given `facts.json`,
an optional evidence document, the preparation's citations and the
preparation's binding (`evaluateWithInvoker`, `runtime.go`). A run's whole
input, every retained response included, is held to 2 MiB, and one acquisition
to 1 MiB (`combinedLimit`, `acquirePlannedOperation`).

**The gateway keeps no cursor; paging is the caller's.** Each `/acquire` is one
request and one receipt. A caller paging through a source asks each page in the
same session (call index 0, 1, 2 and so on), takes the next reference from the
page it was answered, and seals the session after the last page, so that a
verifier holds the pages as one sealed sequence. A page answered `429` or `503`
is not receipted and takes no call index; it is asked again in the same
session. An endpoint's other refusing statuses, an expired reference's `410`
among them, reach the caller as `400` `source failed`, like any other source
failure. Runner names a session of its own for each unattended acquisition,
takes the answer only as its first call, and seals it after.

**Throttling reaches the retry policy.** The gateway answers a throttled
endpoint `429` or `503` in its own form, with `Retry-After`; a job's retry
policy repeats such an attempt after its back-off (`transient`, `retries.go`),
and only when no earlier call of the attempt had answered.

**The postgresql kind prepares occurrences.** The background preparation, its
claim and its lease are served there; only the interactive preparation routes
answer `501 store_route_unsupported` ([postgresql-store.md](../postgresql-store.md)).
A cursor therefore has to be kept on both kinds.

## 3. The decision this needs

The issue's asks presuppose one run over several pages of one source. On main
that has no form: the dialect prepares one acquisition per source, and the
Runtime takes one facts document. Three answers:

| | A. A paged source in the dialect | B. One run per page | C. One page per run |
|---|---|---|---|
| What it is | An operation source declares paging: its pages are one session's calls 0 to n−1, each the next page's request formed from the page before, the read applied to each page and the results joined by a rule the dialect states | The loop is over occurrences: each page is acquired, prepared and evaluated as a run of its own; a page that names a next reference queues the next page's occurrence when its run completes | The cursor fills a case parameter of the source's request; each run reads one page; the next run goes on from the cursor |
| Change to the dialect | Yes: an amendment of the dialect (spec/dialect.md), vectors, and the reference Runner | None | None |
| One run counts pages and names receipts | Yes | No: one page per run | No: one page per run |
| What a decision covers | The pages read in the run | One page; page boundaries are the source's, not the pack's | One page |
| Memory | One page while acquiring; the run's input, held to 2 MiB, at evaluation (section 4.5) | One page | One page |
| Time windows | Read whole, page by page | Read whole, one run per page | Only a window that fits one page; a window of more pages cannot advance |
| Change references | Yes | Yes | Yes, one page per tick |

**Recommendation: A.** It is the only answer in which a run reads every page of
its window or its changes and is decided once, as the issue asks, and the only
one that keeps every page verifiable offline. B decides each page separately,
which is close to the bulk evaluation held back for now, and makes a decision
depend on where the source cuts its pages. C cannot advance a time window that
holds more than one page. A starts with a dialect amendment (the spec's RFC
process), after which Runner builds section 4. C is the subset of A that needs
no amendment, and could be built first for change references alone if the
maintainer wants something before the amendment.

## 4. What Runner builds under A

**4.1 The rule.** Under A, how one page leads to the next is the dialect's: the
mapping's paged source names where the next reference is read in a page, the
request member it fills, and its bound on pages per run, so that `verify-run`
forms each page's request again from the case and the page before it. The loop
ends at a page that names no next reference, at the bound, or at the
occurrence's deadline. What stays with the job is the cursor between runs: a
`paging` member at creation naming, for each paged source of its release's
mapping, the cursor's kind (`change` or `window`), the case parameter the
cursor fills (a `string` for a change reference, a `timestamp` for a window's
start), for a window the case parameter its end fills, where in the last page
the next run's reference is read (a change query's), and the cursor's first
value. An upgrade keeps the member and holds it to the new release's mapping.
Each refusal is `422` with its own code: `invalid_paging_rule` (form and
bounds), `paging_source_unknown` (no paged operation source of that name),
`paging_parameter_unknown` (no case parameter of that name and type).

**4.2 The cursor.** One row per job and paged source, in a table of its own on
both kinds (a new migration, and the sqlite schema raised), carrying the job's
scope identifier and held to it as the job is. It names the value and the run
that advanced it. An occurrence reads it when its input is seeded, and the
value fills the case parameter, so the case retained in the run says what the
first page was asked with. The occurrence, and then its run, carry the
advance: the source, the cursor read (`from`) and the value the pages say comes
next (`to`). The advance is written in the transaction that records the run
completed, under the run's claim, and only while the row still holds `from`: a
run that started from an older cursor and completes late never moves it back.
A run that fails or is interrupted, or whose claim was lost (its completion
writes nothing), leaves the row as it was, and the next occurrence reads the
same cursor again.

**4.3 Pages and receipts.** Each page is one `/acquire` and one receipt. The
run's record says, for each paged source, how many pages were read and the
citation of each, in their order, which are the preparation's citations for
that source.

**4.4 Throttling inside the loop.** A page answered `429` or `503` in the
gateway's form was not served, and the pages before it were. The occurrence
keeps the pages received in its record, and the retry policy schedules the
attempt to resume at the throttled page, in the same session, which is not
sealed until the loop ends: the received pages are not asked for again. This
widens the rule that an attempt is repeated only when no earlier call of it
answered, for a paged source alone, to "the throttled page is asked again, and
no page that answered is".

**4.5 Memory.** While acquiring, Runner holds one page: each page is verified as
it arrives, written to the occurrence's record, and the next is asked. At
evaluation the Runtime is given one facts document, the join of the pages'
reads, and the run's input holds every page, so the run is bounded by the
combined input limit (2 MiB), not by a page; with 1 MiB pages that is one or
two pages. Holding more pages per run than that needs the retained pages moved
out of the run's record into rows of their own and a verification export that
reads them one at a time, a change to the store and to the export, outside
this issue.

**4.6 An expired reference.** Runner cannot tell an expired reference from
another source failure when the gateway answers it `400` (section 2). The job's
rule may name the answer that says a reference expired, a member of a page and
its value; on such a page the occurrence starts the read again from the
cursor's first value, and the occurrence, the run and the journal record it as
a full re-read. An endpoint that refuses an expired reference with a status
fails the occurrence as today and leaves the cursor; the gateway classing that
status, as it classes `429` and `503`, would let Runner act on it.

**4.7 Tests.** Against an in-process stub of the gateway's `/acquire`: a source
of three pages; change references carried from run to run; a time window and
its end; a `429` with `Retry-After` on the second page, after which the first
page is not asked again; an expired reference read again in full; and, on both
kinds, a cursor left where it was by a failed run, an interrupted run and a run
whose claim was lost, and not moved back by a late completion.

## 5. Under B or C

**B.** The rule names the source, the case parameter, where the next reference
is read and a bound on pages per tick. A page's run that completes advances the
cursor (4.2) and, while the page named a next reference and the bound is not
reached, queues the next page's occurrence in the same transaction. Each run
holds one page and one receipt. Sections 4.2, 4.6 and 4.7 hold as written.

**C.** As B without the queuing: each tick reads one page from the cursor. A
time window is bounded by a length the rule names, and a window whose page
names a next reference fails its occurrence and leaves the cursor.

## 6. Not in it

- A cursor or high-water mark kept by the gateway (the gateway's choice is that
  paging is the caller's).
- Paging an acquisition over a durable connection, whose operations are never
  repeated ([durable-sources.md](durable-sources.md)).
- Evaluating records one by one (bulk evaluation).
