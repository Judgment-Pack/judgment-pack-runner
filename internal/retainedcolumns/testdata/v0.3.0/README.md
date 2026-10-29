# Published retained-column/1 fixtures, Spec v0.3.0

Imported byte-for-byte from the public v0.3.0 artifact archive, source
27cb29bf13e4a2d58d69fe19c7ebfeb762b014fc (the signed `v0.3.0` tag). The release
inventory (`releases/v0.3.0.retained-column.json` in the archive, imported here as
`release-inventory.json`) and the archive's `release-identity.json` bind the
selected schema and complete corpus. Historical Draft resources in the parent
directory retain their original bytes and evidence.

Schema SHA256: 0cb49f238c78391cb163a65474cc9af962df2bb1024a55c5310c2b784b2f0557.
Corpus SHA256: 17b16ff7a9b74fe4cc81eb6f6129a6aabdd9834621d9e756aa2d4c1a3f245863.
Inventory SHA256: c8e489b2cabd0ee0b794a8b6a817f06f901e338348e32ec23c8c83d7c40dfe58.
Archive SHA256: 6469a866b9c0e1671de3462c1b1823a1738561f6b84e52b470382fc620530ce9.

The schema and corpus bytes are unchanged from the `v0.3.0-rc.2` candidate this
directory replaced; only the release identity and inventory moved to the final
tag. The candidate's identity (`0.3.0-rc.2` at
bcac15b7537ce21879201b5e5dfe795496225121) is refused by the installed consumer.

The complete preparation examples use a fixed synthetic installation, whose
version string the published corpus still spells `0.3.0-rc.2`. Tests may select
that installation only in private test code; production never accepts it.
No fixture is rewritten or re-signed. Preparation-only export fixtures establish
preparation/boundary behavior, not a trusted release, chain, signature or replay.
See the test coverage report for the exact rows and owners exercised.
