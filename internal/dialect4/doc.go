// Package dialect4 holds what dialect format 4.0.0 adds to format 3.0.0
// (spec/dialect.md §2.2, proposed in the Spec's v0.5.2): the case's rule
// (§2.2.1), the parameter types decimal, string-list and address-range-list
// (§2.2.2), addresses and address ranges (§2.2.3), rule version 2.0.0
// (§2.2.4), the built-in derived values duration, distance and speed
// (§2.2.5), each target's derivation (§2.2.6) and the bounds of the base
// profile (§2.2.7).
//
// It is the one implementation of those constructs in this module: the
// public package sdk/dialect prepares a dialect without sources through it,
// and the Runner's own preparer calls it for every 4.0.0 construct of a
// dialect with sources. It reads values decoded by encoding/json with
// UseNumber, as both callers decode them, and imports nothing that reaches
// a network.
//
// It agrees with the Spec's staged format-4.0.0 vectors at v0.5.2. It is
// written by the same maintainer as the Spec's reference reader, so that
// agreement is not an independent implementation's, and it makes no
// conformance claim: format 4.0.0 is proposed.
package dialect4
