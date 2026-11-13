package runner

// The scope identifier (docs/design/scope-identifiers.md): an optional,
// opaque label a job is created under and keeps for as long as it exists.
// Its triggers, occurrences, runs and journal entries carry it; the store-wide
// listings can be filtered by it; and a request that names a job, a run, a
// trigger or an occurrence must name the scope that record carries, or none
// when it carries none. It is a label and a filter, not an authorization
// boundary: every request the control API admits may name any scope, or
// list every scope's records by naming none.
//
// Its form is the gateway's for its own scope identifier (its SPEC.md §1.2a,
// go/scope.go), written again here with the standard library: the two rules
// are meant to stay the same, so that one identifier is taken by both.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxScopeBytes bounds a scope identifier, in bytes of UTF-8: the gateway's
// bound.
const maxScopeBytes = 128

// scopeUnicodeVersion is the edition of Unicode whose assignments the form
// is held to, the gateway's: the standard library's tables, which a test holds
// to this edition.
const scopeUnicodeVersion = "15.0.0"

// scopedStoreSchema is the schema of a store that holds a job created under a
// scope identifier. The transaction that creates the first such job raises a
// store from storeSchema to it, so that every earlier Runner, which would
// serve that job's records to any request and drop the scope from each record
// it rewrites, refuses the store from then on. A store that holds none stays
// at storeSchema, byte for byte. It is not manualStoreSchema, the schema of a
// store prepared for the manual-admission gate, which no Runner without that
// gate opens: a store at that schema stays refused here.
const scopedStoreSchema = "5"

// errScopeForm is the refusal of a scope identifier out of form. It repeats
// nothing of the value.
var errScopeForm = &apiError{400, "invalid_scope", "Name a scope identifier at most once, as 1 to 128 bytes of UTF-8 with no control, invisible, private-use, unassigned or noncharacter code point."}

// errScopeNotTaken refuses a scope named on a route that neither names a job,
// a run, a trigger or an occurrence nor lists them, rather than answer it as
// if no scope had been named.
var errScopeNotTaken = &apiError{400, "invalid_scope", "This route takes no scope identifier. A new job's scope identifier is the scope member of its creation."}

// isScopeIdentifier is the form: 1 to 128 bytes of UTF-8 with no code point
// scopeRefuses refuses. Nothing is folded, trimmed or normalized: two scope
// identifiers are the same exactly when their bytes are.
func isScopeIdentifier(s string) bool {
	if len(s) == 0 || len(s) > maxScopeBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if scopeRefuses(r) {
			return false
		}
	}
	return true
}

// scopeRefuses says whether r is a code point the gateway's display policy 1
// refuses, or a tab or line feed, which that policy admits in a description
// and an identifier, one line, does not: General_Category Cc (tab and line
// feed among them), Bidi_Control, Default_Ignorable_Code_Point, Co, Cn and
// Noncharacter_Code_Point, as Unicode 15.0.0 assigns them.
func scopeRefuses(r rune) bool {
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Bidi_Control, r) || defaultIgnorable(r) ||
		unicode.Is(unicode.Co, r) || unassigned(r) || unicode.Is(unicode.Noncharacter_Code_Point, r)
}

// defaultIgnorable is Default_Ignorable_Code_Point, derived as
// DerivedCoreProperties.txt derives it, from the properties the standard
// library holds: Other_Default_Ignorable_Code_Point, Cf and
// Variation_Selector, less White_Space, U+FFF9 to U+FFFB, U+13430 to U+1343F
// and Prepended_Concatenation_Mark.
func defaultIgnorable(r rune) bool {
	if !unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Variation_Selector, r) {
		return false
	}
	return !unicode.Is(unicode.White_Space, r) && !(r >= 0xFFF9 && r <= 0xFFFB) && !(r >= 0x13430 && r <= 0x1343F) && !unicode.Is(unicode.Prepended_Concatenation_Mark, r)
}

// unassigned is General_Category Cn: a code point in no other category.
func unassigned(r rune) bool {
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}

// scopeParameter reads the scope query parameter of a raw query, as
// url.ParseQuery decodes it: the value, whether one was named, or
// errScopeForm for one named twice or out of form. A query that does not
// parse is answered as before unless one of its pieces is a scope, which is
// refused rather than dropped, so that a malformed scope is never read as
// none.
func scopeParameter(raw string) (string, bool, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		for _, piece := range strings.FieldsFunc(raw, func(c rune) bool { return c == '&' || c == ';' }) {
			key, _, _ := strings.Cut(piece, "=")
			if k, e := url.QueryUnescape(key); e == nil && k == "scope" {
				return "", false, errScopeForm
			}
		}
	}
	values, named := q["scope"]
	if !named {
		return "", false, nil
	}
	if len(values) != 1 || !isScopeIdentifier(values[0]) {
		return "", false, errScopeForm
	}
	return values[0], true, nil
}

// scopeContext is the key of the scope a request named, in its context.
type scopeContext struct{}

// scopeNamed is the scope identifier a request named, "" for none: what the
// store-wide listings filter by.
func scopeNamed(ctx context.Context) string {
	scope, _ := ctx.Value(scopeContext{}).(string)
	return scope
}

// routeMux is where the control API's routes are registered.
type routeMux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// scopedMux registers each route through Service.scoped, so that no route
// answers a request without its scope being read.
type scopedMux struct {
	mux *http.ServeMux
	s   *Service
}

func (m scopedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, m.s.scoped(handler))
}

// scopeListings are the routes whose answer a scope filters: the store-wide
// listings. Each answers only the records that carry the scope named.
var scopeListings = map[string]bool{"GET /v1/jobs": true, "GET /v1/runs": true, "GET /v1/events": true}

// unheldRecord is an id no record carries: id makes every id of 32
// hexadecimal digits after a prefix, and this is shorter.
const unheldRecord = "unheld"

// scoped reads the scope a request names before its route answers it. On a
// route that names a job, a run, a trigger or an occurrence (in its path, or
// /v1/events in its job parameter) the record must carry the scope named, or
// none when none is named. A request that names a record under another scope
// is handed to its route naming unheldRecord in that record's place, so that
// the route answers it as it answers a request for a record the store does not
// hold, in its own order and byte for byte, and changes and journals nothing,
// as it does for that request. On a store-wide listing the scope is the filter
// the route applies. Any other route refuses a scope.
func (s *Service) scoped(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, named, err := scopeParameter(r.URL.RawQuery)
		if err != nil {
			failure(w, err)
			return
		}
		name, table, id := addressedRecord(r)
		switch {
		case table != "":
			held, found, err := s.heldScope(table, id)
			if err != nil {
				// A scope that cannot be read is not taken to match.
				failure(w, err)
				return
			}
			if found && held != scope {
				r = unheld(r, name)
			}
		case scopeListings[r.Pattern]:
		case named:
			failure(w, errScopeNotTaken)
			return
		}
		if named {
			r = r.WithContext(context.WithValue(r.Context(), scopeContext{}, scope))
		}
		handler(w, r)
	}
}

// addressedRecord is the path value (or, for /v1/events, the query
// parameter) that names the record a request addresses, the record's table and
// its id: its job, else its run, else its trigger, else its occurrence, in its
// path; or, for /v1/events, the job its job parameter names, when its query
// parses and names job once, not empty. "" when it names none. A query that
// does not parse, or names job more than once, names no record: the route
// refuses it whatever the ids, and one named empty is none, so that each is
// answered as it is for ids the store does not hold.
func addressedRecord(r *http.Request) (string, string, string) {
	for _, named := range []struct{ value, table string }{{"job", "jobs"}, {"run", "runs"}, {"trigger", "triggers"}, {"occurrence", "occurrences"}} {
		if id := r.PathValue(named.value); id != "" {
			return named.value, named.table, id
		}
	}
	if r.Pattern == "GET /v1/events" {
		if q, e := url.ParseQuery(r.URL.RawQuery); e == nil && len(q["job"]) == 1 && q["job"][0] != "" {
			return "", "jobs", q["job"][0]
		}
	}
	return "", "", ""
}

// unheld is r naming unheldRecord in place of the record its path value name
// names, or, for name "", its job parameter, which addressedRecord reads only
// when it is named once.
func unheld(r *http.Request, name string) *http.Request {
	r = r.Clone(r.Context())
	if name != "" {
		r.SetPathValue(name, unheldRecord)
		return r
	}
	q := r.URL.Query()
	q.Set("job", unheldRecord)
	r.URL.RawQuery = q.Encode()
	return r
}

// heldScope is the scope identifier the record held under id in table
// carries, "" for none, and whether the store holds the record.
func (s *Service) heldScope(table, id string) (string, bool, error) {
	scope, err := s.store.recordScope(table, id)
	if absent(err) {
		return "", false, nil
	}
	return scope, err == nil, err
}

// jobScope reads the scope member of a job's creation: absent is none, and a
// member written null, empty or out of form is refused. So is a member whose
// name only folds to scope (Scope, SCOPE, ſcope), which the decoder takes as
// it, so that a reader that looks for scope as written never reads another
// value than Runner does. (A scope named twice as written is refused before,
// as every duplicate member is: decodeRaw.)
func jobScope(raw []byte, scope *string) (string, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return "", errScopeForm
	}
	for name := range members {
		if name != "scope" && strings.EqualFold(name, "scope") {
			return "", errScopeForm
		}
	}
	if scope == nil {
		if _, named := members["scope"]; named {
			return "", errScopeForm
		}
		return "", nil
	}
	if !isScopeIdentifier(*scope) {
		return "", errScopeForm
	}
	return *scope, nil
}
