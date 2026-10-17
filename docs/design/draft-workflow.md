# Draft workflow implementation

Runner issue #121 tracks execution and offline verification of Spec's **Draft** RFC 0020. The implemented primitives in `workflow_document.go`, `workflow_selection.go` , `workflow_influence.go` and `workflow_freeze.go` are private and have no API or CLI entry point. They do not implement a workflow executor or verifier and make no conformance claim.

The document reader checks closed members, integer and byte domains, version discovery, ordered stages, one root, earlier-parent references, terminal result, selection policy, typed case bindings and output declarations. It reuses the existing dialect canonicalizer. Cases and payloads keep their distinct literal-preserving serialization, including `-0` and the escaping required by dialect §9.5. Projection copies only declared values into a new object; missing optional values are omitted, while present mistyped values fail. Output extraction distinguishes an explicit empty array from absent, null or malformed output. Equal payloads remain separate entries.

Closed output sets bind every offered envelope in order. Selection checks reject reordered, duplicate or foreign membership and bind unselected records too. Reviewer identity and assertion are recorded statements, not authentication. Disposition selection compares the declared predicate with independently checked audit dispositions for the entire offered set. Empty selection is explicit. A retained `limit-exceeded` refusal is readable, but these helpers do not determine or consume an admission.

Copied parent influence is kept outside the unchanged preparation lineage. All overlapping case targets, declared case parameters and transitive source-parameter targets retain the parent summary, including parameters absent from request text. Generated influence requires explicit generated admission before acquisition. Output summaries include every overlapping fact contributor and inherited summary; selecting a subfield cannot erase its container's influence. Full many-output pointers preserve contributions when the appended index exceeds the declaration's pointer bounds.

## Remaining integration

- Wire checked stage snapshots into the durable execution admission; no endpoint exposes these private helpers yet.
- Transactionally reserve every selected child, keep each attempt and enforce consumed-selection immutability, retries, stopping, bounds and branch inventory.
- Retain output lineage, preparation/audit/run artifacts and evaluation-error envelopes. Do not treat a failed child as an empty successful child.
- Implement the ordered offline verifier phases, including independently trusted historical profiles, exact preparation verification, closed inventory and transitive influence checks. These private helpers' error values are not its report format.
- Record the operation acceptance fixture (three companies, select first and third, two people plus one empty child; three instances, five output envelopes), then its negative/recovery cases and Desk selection UI.

The reader now follows Spec PR #201's named reference amendment. Stage freezing consumes releases already checked by Runner's existing Runtime preview path; it does not substitute another Core validator. It checks every published/binding/bound/pack/release/Runtime pin, all independently supplied installation profiles, named identity and declared outcome selection before returning any stage. Research assignments must match every declared capability and model. Generated-profile operations and research targets require exactly generated admission. Caller-owned bytes are copied into the snapshot; no source is acquired. This is writer admission, not the offline verifier's ordered refusal/finding report.

The draft pins Runner `66e264e` and Runtime `785ea589` (`v0.34.0`). Named formats retain published identity during binding. Retained evaluation-error artifacts, durable instances and the full verifier still require implementation and recorded acceptance; no workflow conformance is claimed.
