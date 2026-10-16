# Offline preparation verification

The jpack-runner verify-preparation command replays retained Dialect inputs without
starting Runner, opening a store, calling Runtime, acquiring a source, or asking
a model. It uses the same preparation implementation as live runs and verify-run.

Pass --dialect, --case, --profiles, --preparation, and --expected-digest with the
corresponding files and the trusted SHA-256 of the exact expected preparation.
For retained acquisitions, pass --acquisitions; absence means {}.
For an unbound definition, also pass --binding.

Profiles belong to the fixture; verification does not install them as authority.
The expected digest must be supplied independently, for example from the reviewed
Exchange catalog. Recalculation uses the preparation's historical verifiedAt,
never today's time.

The command supports integer families 2 and 3 and named formats 2.0.0 and 3.0.0,
with operation, research, and selected local-file sources. Discover support with
verify-preparation --capabilities; this response is not a verification report.

Standard output is one canonical §11 report: reading, refusal, or finding.
Exit statuses are respectively 0, 1, and 2. Status 3 means the command could not
run (usage, inaccessible file, or output failure). A reading always says
binding: not-checked because this command receives no signed run record.
Use verify-run for run-record binding, signature, and chain checks.

The verifier applies bounded input and exact-form checks before recomputation.
An unchanged artifact pair is not established by replay; the catalog consumer
owns release history and artifact identity checks. Successful replay establishes
agreement with retained inputs, not source truth, permission to run, or approval
to write.

Tests replay every staged preparation vector and cover malformed forms, bounds,
legacy optional members, historical time, finding precedence and CLI statuses.
