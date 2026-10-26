// Package retainedcolumns is an internal experiment for Draft RFC 0029 at Spec
// dd2cc94461950563729d1bbf672d785afeb1b02c. It interprets already selected
// conversion leaves and retained frames without acquiring or evaluating anything.
// It does not validate a whole dialect or establish generated-input review,
// source authenticity, capability negotiation, or preparation validity.
//
// ParsePlan accepts a closed conversion object or availability object. ParseFrame
// requires an explicit profile and measures the received projected bytes. The
// caller must separately negotiate support and account for the complete carrier.
// Frames never represent original source bytes. Preview preserves candidate JSON
// spelling; Derive additionally enforces the existing canonical claim domain and
// returns only a recomputed transformation fragment, never a preparation.
// The experimental reference amendment is separately byte-pinned in testdata;
// it changes transformation input custody without claiming normative adoption.
package retainedcolumns
