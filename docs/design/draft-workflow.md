# Draft workflow implementation

Runner #121 tracks execution and offline verification of Spec's **Draft** RFC 0020. The implementation is private: there is no workflow API, CLI, complete offline verifier or conformance claim. The draft pins Runner `66e264e` and Runtime `785ea589` (`v0.34.0`); named formats retain their published identity when bound.

## Documents, freezing and selection

The reader checks closed forms, bounds, ordered stages, one root, earlier-parent references, selection policy, typed case bindings and output declarations. Cases and payloads keep their separate literal-preserving serialization. Projection copies only declared values into a new object; missing optional values are omitted, while present mistyped values fail. Explicit empty output arrays remain distinct from absent, null or malformed outputs. Equal payloads retain separate output identities.

Stage freezing uses releases validated by Runner's existing Runtime preview path. It checks published/binding/bound/pack/release/Runtime pins, independently supplied installation profiles, named identity and declared selection outcomes before acquisition. Research assignments must match the declared model and capabilities. Generated profiles and research targets require generated admission. A copied parent's influence follows overlapping targets and declared parameters transitively; selecting a subfield cannot erase its container's influence.

Selection binds the entire offered set, including unselected records, and reserves all direct consumers atomically in selected-parent order. Missing/mistyped projections remain explicit failures. Projection, instance and recording bounds retain an unconsumed `limit-exceeded` selection without partial children. Exact repeats return the existing admission; changed consumed selections require another branch. Reviewer identity and approval are recorded assertions, not authentication.

## Durable attempts and source checkpoints

`workflowStore` commits state and retained blobs in one SQLite transaction with a revision compare-and-swap. Retained artifacts are immutable; old refused selections and earlier source checkpoints remain in custody outside the current recording. The pending-activity column is migrated transactionally for earlier selection-only stores.

The private driver admits one attempt, retaining it as interrupted and pending before acquiring. Each received source record is checkpointed before another source request or evaluation. Once preparation succeeds it is checkpointed before Runtime invocation. Checkpoints can extend acquisitions but cannot replace their case, mapping, import, earlier answers or an already prepared input. Callback request and response buffers are detached from retained state.

Runner's host adapter shares the existing single-call Gateway acquisition protocol and pinned Runtime evaluation path. It makes no automatic acquisition retry. A complete response with a mismatched receipt is retained alongside its interruption. Ordinary transport failures remain interrupted; explicit stopping is cancelled; definite availability failures are separate. Preparation/admission failures are recomputed at their recorded instant. A complete Core error keeps exact stdout, including its newline; missing or incomplete replies cannot become evaluation errors or empty successes.

An exclusive installation recovery pass clears pending activity without replay. A new failed/interrupted attempt requires an explicit actor/time assertion and receives fresh source records. Earlier attempts remain unchanged. Terminal cancellation and consumed sets cannot be overwritten. A failed checkpoint stops the driver before another acquisition. This is not an exactly-once guarantee across external systems.

The writer admits and commits in stage and selected-parent order. It does not let later work consume global budgets ahead of earlier stages or retry earlier work after later admissions. Parallel acquisition requires a separate durable ordered-result buffer.

Completed exports use the existing independent input verifier and preparation-binding checks. Exact export/audit bytes, canonical preparation and original source members are retained. Optional signature sidecars are scoped to an instance, attempt and retained run, so repeated audit bytes cannot assign a later signature to earlier work. Signature verification remains a separate check. Failed output extraction or bounds preserve the successful evaluation without emitting a false empty result.

## Validation and remaining delivery

Tests cover interrupted recovery and explicit retry, failed checkpoint writes, cancellation, invalid/mismatched receipts, source-buffer reuse, immutable prefixes, per-attempt signatures and real Runtime evaluation. The synthetic operation journey binds two published version-3 HTTP-shaped generated dialects, finds three companies, selects the first and third, and retains two people plus one completed-zero child: three instances and five output envelopes. Private root fields and unselected companies do not enter child requests. Its source is a signed test stand-in; this is not live research, a Desk acceptance recording or workflow verification.

The recording helper counts actual draft §11 files rather than deduplicated database blobs. It returns owned file bytes but does not publish them. The complete writer must also admit recording trust material, expected report and README.

Remaining work:

- The ordered full offline workflow reader, including independently trusted historical profiles, complete inventory and failure checks, selection predicates, projections, transitive influence and output comparisons. Embedded-run verification alone is insufficient.
- Public execution/selection/retry/stop routes, exclusive startup recovery and Desk selection/progress UI.
- Desk research adapters using its shared execution, context, Plan, permission and error boundaries; Runner does not start an AI loop or grant acquisition authority.
- The independent operation and research acceptance recordings, negative fixtures and optional Runtime replay. No Draft conformance claim is available.
