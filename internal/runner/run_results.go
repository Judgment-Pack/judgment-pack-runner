package runner

// Run results (docs/design/run-results.md): the counts a finished run carries,
// the closed list of error classes a run or an occurrence that ended without a
// decision carries beside its reason, and the filters by outcome, error class
// and journal kind.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// resultEntryVersion is the shape of an entry that holds disposition,
// errorClass or counts, which an entry of an earlier version never holds,
// whatever its by, scope and attempt. Every other entry keeps its version.
const resultEntryVersion = "5"

// The error classes, a closed list: a class is added only with a change of
// docs/design/run-results.md.
const (
	classSource        = "source"
	classPreparation   = "preparation"
	classEvaluation    = "evaluation"
	classConfiguration = "configuration"
	classLimit         = "limit"
	classInterrupted   = "interrupted"
	classInternal      = "internal"
)

// errorClasses are the error classes, in the record's order.
var errorClasses = []string{classSource, classPreparation, classEvaluation, classConfiguration, classLimit, classInterrupted, classInternal}

// reasonClasses is the class of every reason code a run, an occurrence or an
// attempt is given that names a failure. Each code has exactly one class; a
// code that names no failure is in notFailureReasons instead
// (TestEveryReasonCodeHasOneClass).
var reasonClasses = map[string]string{
	runQueueExpired:            classLimit,
	runProfileSetAside:         classConfiguration,
	runBindingRefused:          classInternal,
	runPreparedByEarlierRunner: classPreparation,
	occurrenceAttemptLimit:     classLimit,
	occurrenceWaitTooLong:      classLimit,
	runAttemptStopped:          classInterrupted,
	runAttemptRestarted:        classInterrupted,
}

// notFailureReasons are the reason codes of an occurrence a trigger's own
// rule skipped: it has no error class.
var notFailureReasons = []string{"missed", "latest-after-missed", "trigger-paused", "queue-full", "overlap"}

// sourceFailureCodes are the codes of a failed call to a Gateway connection
// (retries.md, section 2): a source acquisition that failed.
var sourceFailureCodes = []string{"gateway_unavailable", "gateway_refused", "gateway_response_too_large", "gateway_receipt_mismatch"}

// acquisitionClass is the class of an occurrence whose input could not be
// acquired, read or prepared: source for a failed call to a Gateway
// connection, configuration for a connection no longer installed and for a
// gateway that refused the call 401 or 403 (Runner's credential, or the
// connection's access to its provider, was refused), and preparation for
// every other failure. A class says where the failure arose, not whether
// another attempt would help: that is the job's retry policy's (retries.go).
func acquisitionClass(err error) string {
	var answer *gatewayAnswer
	if errors.As(err, &answer) && (answer.status == http.StatusUnauthorized || answer.status == http.StatusForbidden) {
		return classConfiguration
	}
	var api *apiError
	if errors.As(err, &api) {
		switch {
		case slices.Contains(sourceFailureCodes, api.Code):
			return classSource
		case api.Code == "gateway_connection_required":
			return classConfiguration
		}
	}
	return classPreparation
}

// submissionClass is the class of an occurrence whose run Runner refused to
// queue: internal for an answer of 500 or more, Runner's own failure, and
// preparation for a refusal of the input.
func submissionClass(api *apiError) string {
	if api.Status >= 500 {
		return classInternal
	}
	return classPreparation
}

// evaluationStart marks the context of a run's evaluation (store.go):
// invokeStarting sets it once the Runtime's evaluation process has started.
// A failure before that is Runner's own, not the evaluation's.
type evaluationStart struct{}

// markingStart is ctx marked for a run's evaluation, and the mark.
func markingStart(ctx context.Context) (context.Context, *bool) {
	started := new(bool)
	return context.WithValue(ctx, evaluationStart{}, started), started
}

// noteStart sets ctx's mark, if it has one, when the process invokeStarting
// started under label is the evaluation's.
func noteStart(ctx context.Context, label string) {
	if started, ok := ctx.Value(evaluationStart{}).(*bool); ok && label == evaluationLabel {
		*started = true
	}
}

// RunCounts are what a finished run's record holds, counted
// (docs/design/run-results.md, section 2).
type RunCounts struct {
	// Sources are the sources the run's preparation records an
	// outcome for: none for a run whose input holds no preparation.
	Sources *int `json:"sources,omitempty"`
	// Decisions are the decisions the record holds: 1 for a completed run,
	// 0 for any other.
	Decisions int `json:"decisions"`
	// ByKind are the decisions by disposition kind; ByOutcome those of kind
	// outcome, by outcome identifier.
	ByKind    map[string]int `json:"byKind"`
	ByOutcome map[string]int `json:"byOutcome"`
}

// EventDisposition is a decision as a journal entry names it: its kind and,
// for kind outcome, its outcome identifier. Its reasons and handoff are the
// run's to serve.
type EventDisposition struct {
	Kind      string `json:"kind"`
	OutcomeID string `json:"outcomeId,omitempty"`
}

// runDisposition reads the decision a run's result holds, if it holds one.
func runDisposition(r Run) (kind, outcome string, held bool) {
	if r.State != "completed" || len(r.Result) == 0 {
		return "", "", false
	}
	raw := member(r.Result, "disposition")
	if len(raw) == 0 {
		return "", "", false
	}
	var d struct {
		Kind      any `json:"kind"`
		OutcomeID any `json:"outcomeId"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return "", "", true
	}
	kind, _ = d.Kind.(string)
	if kind == "outcome" {
		outcome, _ = d.OutcomeID.(string)
	}
	return kind, outcome, true
}

// countsOf counts what a finished run's record holds.
func countsOf(r Run) *RunCounts {
	c := &RunCounts{ByKind: map[string]int{}, ByOutcome: map[string]int{}}
	if p := r.Input.Preparation; p != nil {
		n := len(p.Outcomes)
		c.Sources = &n
	}
	if kind, outcome, held := runDisposition(r); held {
		c.Decisions++
		if kind != "" {
			c.ByKind[kind]++
		}
		if outcome != "" {
			c.ByOutcome[outcome]++
		}
	}
	return c
}

// finishedRunState says whether a run in state has ended its attempt.
func finishedRunState(state string) bool {
	return state == "completed" || state == "failed" || state == "interrupted"
}

// withResults gives a run that ends its attempt its counts and, when it
// ended without a decision, its error class: the one its writer named, else
// its reason code's, else interrupted for an interrupted run. A run that has
// not ended has neither.
func withResults(r Run) Run {
	if !finishedRunState(r.State) {
		r.Counts, r.ErrorClass = nil, ""
		return r
	}
	r.Counts = countsOf(r)
	switch {
	case r.State == "completed":
		r.ErrorClass = ""
	case r.ErrorClass != "":
	case reasonClasses[r.Reason] != "":
		r.ErrorClass = reasonClasses[r.Reason]
	case r.State == "interrupted":
		r.ErrorClass = classInterrupted
	}
	return r
}

// endedOccurrence says whether an occurrence in state ended without a run, as
// a failure: an error class is carried by such an occurrence alone.
func endedOccurrence(state string) bool { return state == "failed" || state == "expired" }

// withOccurrenceClass gives an occurrence that ended without a run its error
// class: the one its writer named, else its reason code's. Any other has none.
func withOccurrenceClass(o Occurrence) Occurrence {
	switch {
	case !endedOccurrence(o.State):
		o.ErrorClass = ""
	case o.ErrorClass == "" && reasonClasses[o.Reason] != "":
		o.ErrorClass = reasonClasses[o.Reason]
	}
	return o
}

// resultFilter is what a listing's outcome, errorClass and, on the journal,
// kind parameters ask for. Each is any of its values; together, all of them.
type resultFilter struct {
	Outcomes, Classes, Kinds []string
}

// maxFilterValues bounds how often a filter may be named.
const maxFilterValues = 10

// maxOutcomeFilter is the longest outcome identifier a filter names, in
// bytes: the bound a workflow's outcome rule has.
const maxOutcomeFilter = 256

var errInvalidResultFilter = &apiError{400, "invalid_filter", "Name an outcome of 1 to 256 bytes without control characters, an error class of the documented list, or a journal kind, each at most 10 times."}

// readResultFilter reads the run filters from a raw query, and the journal's
// kind when kinds is set. An empty, malformed or unknown value, or one named
// more than maxFilterValues times, is invalid_filter; so is a piece of a query
// that does not parse whose name is one of the filters, which is refused
// rather than dropped, so that a filter is never read as none.
func readResultFilter(raw string, kinds bool) (resultFilter, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		for _, piece := range strings.FieldsFunc(raw, func(c rune) bool { return c == '&' || c == ';' }) {
			key, _, _ := strings.Cut(piece, "=")
			if k, e := url.QueryUnescape(key); e == nil && (k == "outcome" || k == "errorClass" || kinds && k == "kind") {
				return resultFilter{}, errInvalidResultFilter
			}
		}
	}
	f := resultFilter{Outcomes: q["outcome"], Classes: q["errorClass"]}
	if kinds {
		f.Kinds = q["kind"]
	}
	if len(f.Outcomes) > maxFilterValues || len(f.Classes) > maxFilterValues || len(f.Kinds) > maxFilterValues {
		return resultFilter{}, errInvalidResultFilter
	}
	for _, o := range f.Outcomes {
		if !wellFormedOutcome(o) {
			return resultFilter{}, errInvalidResultFilter
		}
	}
	for _, c := range f.Classes {
		if !slices.Contains(errorClasses, c) {
			return resultFilter{}, errInvalidResultFilter
		}
	}
	for _, k := range f.Kinds {
		if !slices.Contains(eventKinds, k) {
			return resultFilter{}, errInvalidResultFilter
		}
	}
	return f, nil
}

// wellFormedOutcome says whether o can name an outcome identifier in a filter:
// 1 to maxOutcomeFilter bytes of UTF-8 with no control character.
func wellFormedOutcome(o string) bool {
	return o != "" && len(o) <= maxOutcomeFilter && utf8.ValidString(o) && strings.IndexFunc(o, unicode.IsControl) < 0
}

// empty says whether f asks for nothing.
func (f resultFilter) empty() bool {
	return len(f.Outcomes) == 0 && len(f.Classes) == 0 && len(f.Kinds) == 0
}
