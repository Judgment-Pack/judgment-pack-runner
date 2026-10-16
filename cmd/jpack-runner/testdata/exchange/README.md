# Exchange example fixtures

The example entries of the Judgment Pack Exchange that hold fixtures, copied byte for
byte from [Judgment-Pack/judgment-pack-exchange](https://github.com/Judgment-Pack/judgment-pack-exchange)
at commit `852a4ad8fc0ee353e78dbf6c7f38544577a6bf10`, from its `tests/fixtures/`: each
tree's `entries/example/<entry>/`, with the entry's `entry.json`, its `dialect.json`
and its fixtures. Nothing else of the trees is copied, and nothing here is changed:
these are the Exchange's files, and a change to one is the Exchange's to make first.

`TestVerifyPreparationReadsTheExchangeFixtures` runs `jpack-runner verify-preparation`
on each fixture as the catalog's own tests run the specification's preparation
verifier (`tests/test_entries.py`, `verifier_results`): the entry's `dialect.json`,
and the fixture's `case.json`, `profiles.json` and `preparation.json`, with its
`sources.json` and `binding.json` when it has them, and the fixture's
`preparationDigest` as the expected digest. It requires what the catalog requires of
that verifier: a reading whose digest is the `preparationDigest`, or, for a fixture of
a kind the verifier does not read, the refusal `kind-unread`, which the catalog skips
naming the kind.

Every identifier, key and observation in them is synthetic, as the Exchange says of
its examples.
