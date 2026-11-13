// Package dialect reads a dialect document of the proposed format 4.0.0
// (spec/dialect.md §2.2, the Spec's v0.5.2) that names no source, and
// prepares cases through it in process: the case's copy and the case's rule
// of version 2.0.0, with the parameter types decimal, string-list and
// address-range-list and the built-in derived values duration, distance and
// speed.
//
// Read holds a document to its form once. Prepare, or PrepareBatch for many
// cases, then computes each case's preparation, in the canonical form
// spec/dialect.md §9.5 gives it, with its digest, the facts and evidence it
// writes, and the digest of the parameters the case gave, plain or keyed
// with a key the caller holds. A Dialect is read-only once read and is safe
// for concurrent use: no case needs setup of its own.
//
// The package reaches no network, starts no process and reads no file. It
// reads the base profile only: a document of the extended profile is refused
// profile-unsupported until the Spec decides the extended literal bound
// (spec issue #324). A document with sources is refused sources-unread: the
// Runner prepares those (jpack-runner verify-preparation reads one over
// fixtures), and moving that preparer here is later work. A document of an
// earlier format is refused version-unread.
//
// The package reads the proposed format 4.0.0 and agrees with the Spec's
// staged vectors at v0.5.2. It is written by the same maintainer as the
// Spec's reference reader, so it is not an independent implementation, and
// it makes no conformance claim. The Runner does not admit a recorded job
// on a dialect of format 4.0.0 until the Spec's workflow and recording
// schemas admit the format (spec issue #324).
//
// Experimental: the surface may change while format 4.0.0 is proposed.
package dialect
