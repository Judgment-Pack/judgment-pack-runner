// Package retainedcolumns is an internal experiment for Draft RFC 0029 at Spec
// dd2cc94461950563729d1bbf672d785afeb1b02c. It interprets already selected
// conversion leaves and retained frames without acquiring or evaluating anything.
// It does not validate a whole dialect or establish generated-input review,
// source authenticity, capability negotiation, or preparation validity.
//
// ParsePlan accepts a closed conversion object or availability object. ParseFrame
// requires an explicit profile and measures the received projected bytes. The
// caller must separately negotiate support and account for the complete carrier.
// Raw values retain origin-specific UTF-8 limits: asserted/CSV16000 bytes,
// generated524288 bytes, fixed for both profiles. Complete original snapshot,
// frame and carrier budgets remain additional independent caller obligations;
// the generated per-cell ceiling is not a jointly reachable snapshot maximum.
// Frames never represent original source bytes. Preview preserves candidate JSON
// spelling; Derive additionally enforces the existing canonical claim domain and
// returns only a recomputed transformation fragment, never a preparation.
// The experimental reference amendment is separately byte-pinned in testdata;
// it changes transformation input custody without claiming normative adoption.
package retainedcolumns
