# Draft workflow implementation

Runner issue #121 tracks execution and offline verification of Spec's **Draft** RFC 0020. The implemented primitives in `workflow_document.go`, `workflow_selection.go` and `workflow_influence.go` are private and have no API or CLI entry point. They do not implement a workflow executor or verifier and make no conformance claim.

The document reader checks closed members, integer and byte domains, version discovery, ordered stages, one root, earlier-parent references, terminal result, selection policy, typed case bindings and output declarations. It reuses the existing dialect canonicalizer. Cases and payloads keep their distinct literal-preserving serialization, including `-0` and the escaping required by dialect §9.5. Projection copies only declared values into a new object; missing optional values are omitted, while present mistyped values fail. Output extraction distinguishes an explicit empty array from absent, null or malformed output. Equal payloads remain separate entries.

Closed output sets bind every offered envelope in order. Selection checks reject reordered, duplicate or foreign membership and bind unselected records too. Reviewer identity and assertion are recorded statements, not authentication. Disposition selection compares the declared predicate with independently checked audit dispositions for the entire offered set. Empty selection is explicit. A retained `limit-exceeded` refusal is readable, but these helpers do not determine or consume an admission.

Copied parent influence is kept outside the unchanged preparation lineage. All overlapping case targets, declared case parameters and transitive source-parameter targets retain the parent summary, including parameters absent from request text. Generated influence requires explicit generated admission before acquisition. Output summaries include every overlapping fact contributor and inherited summary; selecting a subfield cannot erase its container's influence. Full many-output pointers preserve contributions when the appended index exceeds the declaration's pointer bounds.

## Remaining integration

- Freeze published, binding, bound, pack, release and Runtime identities before acquisition, and check declared outcomes and generated influence against actual artifacts.
- Transactionally reserve every selected child, keep each attempt and enforce consumed-selection immutability, retries, stopping, bounds and branch inventory.
- Retain output lineage, preparation/audit/run artifacts and evaluation-error envelopes. Do not treat a failed child as an empty successful child.
- Implement the ordered offline verifier phases, including independently trusted historical profiles, exact preparation verification, closed inventory and transitive influence checks. These private helpers' error values are not its report format.
- Record the operation acceptance fixture (three companies, select first and third, two people plus one empty child; three instances, five output envelopes), then its negative/recovery cases and Desk selection UI.

The initial document reader follows the earlier workflow draft, which pins Runner `89cf441` and Runtime `80a8176`, permits integer dialect families 2/3, and conditionally defers research release encoding. Runner's current named Dialect and research support do not silently update that workflow contract. Exact release encoding, named-family selection and evaluation-error retention need an explicit Spec amendment before that additional workflow scope can be frozen or claimed. Existing dialect and run formats are unchanged here.

Spec PR #201 now updates the draft reference encodings and defines named references and retained evaluation-error artifacts. Integrating that amendment and recording execution remain separate work.
