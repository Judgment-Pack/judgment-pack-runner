# Proposed workflow admission experiment

This internal experiment implements proposed workflow version 2 from Spec
[Draft PR #210](https://github.com/Judgment-Pack/judgment-pack-spec/pull/210).
It does not adopt the proposal, change version-1 exports, expose a public workflow
API or claim conformance. Runner #121 and Spec #209 remain open.

## Retained state and custody

Every acquisition follows a durable, complete begin plan. Received source and
evaluation bytes enter SQLite custody before validation or accounting. Admission
then follows the plan's canonical order even when results arrive in reverse order.
Custody bytes are not an asserted historical baseline: only the reconstructed
proof prefix determines each candidate's files and actual bytes.

A failure to retain the proof or mandatory attempt material leaves the last valid
prefix and the acquired material in custody. It does not fabricate a portable
refusal, truncate the response or automatically acquire it again. Recovery consumes
retained material; it does not repeat source requests or Runtime evaluation.

Branches have separate revision-checked heads. Reselection retires descendants in
the new branch while retaining their immutable history and counting it against the
limits. The previous branch remains byte-for-byte unchanged. Explicit retries use
new epochs; they cannot retroactively change an earlier refusal's byte calculation.

The driver shares Runner's existing preparation, operation acquisition and Runtime
invocation. It chooses no AI model or permission. A future Desk research host must
use Desk's shared context, Plan, approval and error boundaries.

## Recordings

Run the isolated tests with an independently provisioned Runtime:

```sh
JPACK_TEST_BIN=/path/to/pinned/jpack \
JPACK_WORKFLOW_V2_RECORDING_DIR=/tmp/workflow-recordings \
go test -race -count=1 -v ./internal/runner -run '^TestWorkflowV2'
```

CI builds Runtime `785ea58927255b775a774e29952c560591429cd3` (`v0.34.0`), requires
both journey tests to pass rather than skip, and retains the recordings and log.
The export directory is opt-in and never points to installed user data.
It compares those recordings with the independently authored experimental reader
at Spec `abd9da216e2897969a3b9ff850ec98de27a5de78`, preserving the comparison report
even when a check fails. This is a reviewed proposal dependency, not an adopted
conformance suite.

Each snapshot contains `bundle/` plus separate expected pins, historical profiles
and path hashes. Comparison traces retain actual candidate file bytes outside the
bundle. A reader must derive its own candidates; neither the trace nor the manifest
is authority for an admission decision.

- The operation journey executes two published dialects: three companies, an
  explicit selection of the first and third, two people and a completed-zero child.
  A subsequent branch selects the previously unselected company. Tests check exact
  child arguments, parent lineage, retained counts and unchanged parent bytes.
- The research journey prepares typed company and person fields from actual local
  TLS responses, including answer, search and page channels, then evaluates both
  stages using Runtime. The person case comes from the selected company record.
- Boundary and recovery tests use actual preparation and Runtime output with
  asserted synthetic case inputs where no source is required. Their manifests
  explicitly distinguish those cases from acquired source recordings.

All source content is synthetic. The signed source stand-ins are not installed
Gateway adapters or live models, and these are not Desk acceptance recordings.
Runtime evaluation does not establish the accuracy of the source content.

## Independent verification and remaining delivery

The separately authored Python reader is governed by the published
[independence protocol](https://github.com/Judgment-Pack/judgment-pack-spec/issues/209#issuecomment-6243575419).
Its author receives real workflow recordings only after an initial algorithm
freeze. Approved encoding dependencies and incidental exposure are recorded. The
exercise uses the same AI vendor; it does not satisfy a cross-vendor claim.

Agreement must compare exact candidates, prefixes, path bytes, retained and active
counts, refusals and reports, and retain discrepancies rather than modifying
expected results to fit an implementation. The proposed run-version restriction
and failed-preparation timestamp rule require explicit contract review.

`tools/check_workflow_recordings.py` supplies separately retained test pins to a
pinned reader checkout and compares its independent measurements with actual
writer candidates. It contains no workflow admission algorithm. It is a test
harness for controlled recordings, not a public loader for untrusted bundles.
Count-only refusals can have no materialized acceptance candidate; that absence
is reported explicitly while the retained refusal is compared. Missing required
carriers and unmatched available file maps fail the comparison.

The first comparison exposed three defects: the reader used dialect encoding
instead of retained Release encoding; the operation fixture supplied an invalid
whole-query template; and Runner omitted the derived empty evidence object when
calling Runtime. The original recordings and first result are preserved. Runner
now checks `adapter-http` query shapes during preparation, supplies empty evidence
explicitly, and holds retained facts and evidence to the recomputed objects.
Existing invalid records are refused; verification does not rewrite them.

These are consistency checks. Synthetic source receipts, a complete audit and
matching implementations do not prove source truth or authentic chronology.

Remaining adoption gates include completed independent agreement, adversarial
review and the Spec decision. Public routes, the offline artifact loader/command,
Desk controls and its shared research adapter follow that decision. An embedded
run check alone is not workflow verification.
