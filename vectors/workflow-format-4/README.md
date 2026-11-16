# Workflow stages of dialect formats 4.0.0 and 4.1.0 (staged, proposed)

`spec/` holds the Spec's hand-written vectors for a workflow stage whose dialect is of format `4.0.0`
or `4.1.0` (RFC 0020, the amendment proposal of 2026-11-15, staged for `0.6.0`), copied byte for byte
from the Spec repository's `conformance/dialect/workflow-format-4/` at commit
`0d5bdea79cea2f2c00fd2c802bac476e6386c002` (spec PR #332): its `README.md`, `manifest.json`, and each
case's workflow document and release, the release with every member Runner's `Release` declares
among them. The stages' dialects are the format vectors' own, read from their copies in
`vectors/dialect-format-4/` and `vectors/dialect-format-4.1/`. Nothing here was recorded from the
Runner's code: every digest is the Spec's, written by hand. Their relative links point into the Spec
repository, not this one.

The files are held to the SHA-256 their Spec commit gives them
(`internal/runner/workflow_format4_test.go`). Never convert their line endings (`.gitattributes`);
never edit them. A new Spec revision is taken by copying its files again, in one change, with the pin
above.

`TestWorkflowStagesOfFormat4AgreeWithTheSpecVectors` holds Runner to each case: the dialect digest, a
stage Runner reads, the release digest of RFC 0020 section 1, and the workflow reader's
`version-unread`. Two members of each stage are set aside before the stage is read: its `sample`,
which Runner's executor does not read yet (issue #170), and its `selection`, which `spec/workflow.md`
gives only a stage another stage consumes, while each vector's one stage is its workflow's result.
The reading is by the same maintainer as the Spec's reference reader: it is not an independent
implementation, and no conformance claim is made of it.
