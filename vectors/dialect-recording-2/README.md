# Dialect-recording schema version 2 (staged, proposed)

`spec/schema/` holds the Spec's `schema/dialect-recording/2/` (`preparation.schema.json`,
`export.schema.json`), and `spec/vectors/` its `conformance/dialect/recording-2/`, copied byte for
byte from the Spec repository at commit `0d5bdea79cea2f2c00fd2c802bac476e6386c002` (spec PR #332,
RFC 0018's amendment of 2026-11-15, staged for `0.6.0`). Version 2 is the recording form of a
preparation of dialect formats `4.0.0` and `4.1.0`. Nothing here was recorded from the Runner's code.

The files are held to the SHA-256 their Spec commit gives them, and each vector to the manifest's
(`internal/runner/recording2_schema_test.go`). That test reads the schema with the subset of JSON
Schema it uses and holds that reading to the manifest's answer for every vector, before a run's
preparation of either format is validated against it (`internal/runner/run_format4_test.go`).
Never convert their line endings (`.gitattributes`); never edit them. A new Spec revision is taken by
copying its files again, in one change, with the pins.
