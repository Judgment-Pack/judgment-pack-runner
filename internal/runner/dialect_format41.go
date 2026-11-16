package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// format41 is the proposed dialect format 4.1.0 (the Spec's RFC 0033, in
// Review): format 4.0.0 with a paged operation source. Runner reads it where
// it reads format 4.0.0 (dialect_format4.go): in verify-preparation, and on
// the run path, where a paged source is acquired page by page in one
// gateway session (paging_loop.go).
const format41 = "4.1.0"

// fourFamily says whether a named format is of format 4.0.0's family: 4.0.0
// itself, or 4.1.0, which only adds the paged source.
func fourFamily(format string) bool { return format == format4 || format == format41 }

// The bounds of a paged source under the base profile (RFC 0033 section 7):
// the pages retained for one source, and maxPages, are Core §4.1's "Dialect
// retained responses"; a next reference is "Dialect text value bytes".
const (
	maxPages          = maxRetainedResponses
	maxReferenceBytes = 8192
)

// pagingRule is a paged source's paging member (RFC 0033 section 2), read
// strictly from the member as written.
type pagingRule struct {
	Next     string      `json:"next"`
	NoNext   string      `json:"noNext"`
	Fills    string      `json:"fills"`
	Items    string      `json:"items"`
	MaxPages json.Number `json:"maxPages"`
	// bound is MaxPages as an integer, once the form is held.
	bound int
}

var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// pagesOverRow is §11's third phase for maxPages: an integer literal above
// the row is over the bound; anything else is left to the form (phase 5).
func pagesOverRow(v any) bool {
	n, ok := v.(json.Number)
	if !ok || !integerLiteral.MatchString(string(n)) {
		return false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	return err != nil && n[0] != '-' || err == nil && i > maxPages
}

// pagedPhase3 is §11's third phase for a document of format 4.1.0 (RFC 0033
// section 7): maxPages when it is an integer, and in every acquisitions
// member that has pages, the number of pages when it is an array and the
// nesting of each page's result. It measures nothing in a document of
// another format.
func pagedPhase3(dialect, acquisitions any) string {
	d, _ := dialect.(map[string]any)
	if d["formatVersion"] != format41 {
		return ""
	}
	sources, _ := d["sources"].([]any)
	for _, s := range sources {
		source, _ := s.(map[string]any)
		paging, _ := source["paging"].(map[string]any)
		if pagesOverRow(paging["maxPages"]) {
			return fmt.Sprintf("a paged source's maxPages is over %d", maxPages)
		}
	}
	acq, _ := acquisitions.(map[string]any)
	for _, name := range sortedKeys(acq) {
		member, _ := acq[name].(map[string]any)
		pages, ok := member["pages"].([]any)
		if !ok {
			continue
		}
		if len(pages) > maxPages {
			return fmt.Sprintf("the acquisition %s retains more than %d pages", name, maxPages)
		}
		for i, p := range pages {
			page, _ := p.(map[string]any)
			if result, ok := page["result"]; ok && valueDepth(result) > maxValueDepth {
				return fmt.Sprintf("page %d of the acquisition %s holds a result nested more than %d levels", i, name, maxValueDepth)
			}
		}
	}
	return ""
}

// templateForm says whether an object of a request template is a template
// form, a parameter's or a text's (instantiate).
func templateForm(o map[string]any) bool {
	_, param := o["$param"]
	_, text := o["$text"]
	return param || text
}

// pagingOf holds a source's paging member to its form (RFC 0033 section 2):
// in format 4.1.0 alone, on an operation that has no calculation, an object
// of exactly its five members, and fills's parent an object, written in the
// source's arguments and lying within no template form. The profile's part,
// which member fills may name, is checked against the profile when the
// source is prepared (pagingFits). It returns nil for a source with none.
func pagingOf(format string, s MappingSource) (*pagingRule, error) {
	if len(s.Paging) == 0 {
		return nil, nil
	}
	malformed := func(format string, a ...any) (*pagingRule, error) {
		return nil, withCode(codeMalformed, fmt.Errorf("source %s: "+format, append([]any{s.Name}, a...)...))
	}
	switch {
	case format != format41:
		return malformed("paging is a member of format 4.1.0 alone")
	case s.Kind != "operation":
		return malformed("paging is an operation's alone")
	case s.Calculation != nil:
		return malformed("a calculated source cannot be paged")
	}
	var rule pagingRule
	d := json.NewDecoder(bytes.NewReader(s.Paging))
	d.UseNumber()
	d.DisallowUnknownFields()
	var members map[string]json.RawMessage
	if json.Unmarshal(s.Paging, &members) != nil || len(members) != 5 || d.Decode(&rule) != nil {
		return malformed("paging is an object of exactly next, noNext, fills, items and maxPages")
	}
	for _, k := range []string{"next", "noNext", "fills", "items"} {
		var text string
		if json.Unmarshal(members[k], &text) != nil {
			return malformed("paging's %s is a string", k)
		}
	}
	if bytes.HasPrefix(members["maxPages"], []byte(`"`)) || !integerLiteral.MatchString(rule.MaxPages.String()) {
		return malformed("paging's maxPages is an integer")
	}
	if pagesOverRow(rule.MaxPages) {
		return nil, withCode(codeOverBound, fmt.Errorf("source %s: paging's maxPages is over %d", s.Name, maxPages))
	}
	bound, _ := strconv.Atoi(rule.MaxPages.String())
	if bound < 1 {
		return malformed("paging's maxPages is at least 1")
	}
	rule.bound = bound
	if rule.NoNext != "absent" && rule.NoNext != "null" && rule.NoNext != "empty-string" {
		return malformed("paging's noNext is absent, null or empty-string")
	}
	if _, err := pointerTokens(rule.Items, true); err != nil {
		return malformed("paging's items is a pointer")
	}
	next, err := pointerTokens(rule.Next, false)
	if err != nil || len(next) == 0 {
		return malformed("paging's next is a pointer other than the root")
	}
	fills, err := pointerTokens(rule.Fills, false)
	if err != nil || len(fills) == 0 {
		return malformed("paging's fills is a pointer other than the root")
	}
	// fills's parent is literal: an object written in the arguments, not a
	// template form and within none, so that every page's request has it.
	var at any
	if json.Unmarshal(s.Arguments, &at) != nil {
		return malformed("its arguments are not JSON")
	}
	for _, token := range fills[:len(fills)-1] {
		o, ok := at.(map[string]any)
		if !ok || templateForm(o) {
			return malformed("paging's fills does not name a member of an object written in its arguments")
		}
		if at, ok = o[token]; !ok {
			return malformed("paging's fills does not name a member of an object written in its arguments")
		}
	}
	if o, ok := at.(map[string]any); !ok || templateForm(o) {
		return malformed("paging's fills does not name a member of an object written in its arguments")
	}
	return &rule, nil
}

// pagingFits holds fills to the members of a request its profile lets a
// next reference fill (RFC 0033 section 2): within a tool call's arguments
// under shape mcp; under shape http with the adapter adapter-http, one query
// member, or a member within an object body. Anything else does not fit the
// profile, profile-mismatch, as a request that does not fit its profile is.
func pagingFits(rule *pagingRule, p InputProfile) error {
	fills, _ := pointerTokens(rule.Fills, false)
	fits := false
	switch {
	case p.Shape == "mcp":
		fits = fills[0] == "arguments" && len(fills) >= 2
	case p.Shape == "http" && p.Adapter.Name == "adapter-http":
		fits = fills[0] == "query" && len(fills) == 2 || fills[0] == "body" && len(fills) >= 2
	}
	if !fits {
		return withCode(codeMismatch, fmt.Errorf("a next reference cannot fill %s under the profile %s", rule.Fills, p.ID))
	}
	return nil
}

// filled is q with the member at tokens set to value, every object on the
// way copied, and nothing else changed (RFC 0033 section 3). Its parent is
// an object in q (pagingOf).
func filled(q any, tokens []string, value string) any {
	o, _ := q.(map[string]any)
	copied := make(map[string]any, len(o)+1)
	for k, v := range o {
		copied[k] = v
	}
	if len(tokens) == 1 {
		copied[tokens[0]] = value
	} else {
		copied[tokens[0]] = filled(o[tokens[0]], tokens[1:], value)
	}
	return copied
}

// pagedRead is a paged source's walk: its joined value, the citation of each
// page in call order, the next references the loop followed, and its end.
type pagedRead struct {
	joined     map[string]any
	cites      []Citation
	references []string
	end        string
}

// outcome is the paged source's outcome member paging (RFC 0033 section 8).
func (r *pagedRead) outcome() json.RawMessage {
	refs := r.references
	if refs == nil {
		refs = []string{}
	}
	return encode(map[string]any{"end": r.end, "pages": len(r.cites), "references": refs})
}

// walkPages walks a paged source's pages from page 0 (RFC 0033 sections 3 to
// 7), finishing each before the next: the page is retained
// (acquisition-missing); its receipt verifies over its request, formed from
// q0 and the reference the page before named (acquisition-invalid); its
// times (acquisition-stale); it is call i of the session of page 0, chained
// to the page before, and page 0's session is named by no earlier source's
// receipt (acquisition-sessions); and its value read holds an items array and
// a next reference spelled as noNext declares (read-failed). The loop ends at
// a page that names no next reference, or at maxPages; a page retained after
// the end is acquisition-unexpected. The joined value is then formed, and its
// nesting measured. Nothing here asks a source: the pages are the ones given.
func walkPages(read SourceRead, rule *pagingRule, q0 any, pages []json.RawMessage, p InputProfile, public ed25519.PublicKey, at time.Time, maxAge int, earlier []Citation) (*pagedRead, error) {
	fills, _ := pointerTokens(rule.Fills, false)
	seen := map[string]bool{}
	if s, ok := pointerRead(q0, fills); ok {
		if text, ok := s.(string); ok {
			seen[text] = true
		}
	}
	out := &pagedRead{end: ""}
	items := []any{}
	values := []any{}
	q := q0
	var previous string
	for i := 0; ; i++ {
		if i >= len(pages) {
			return nil, withCode(codeMissing, fmt.Errorf("page %d, which the loop calls for, is not retained", i))
		}
		args, err := proofCanon(encode(q))
		if err != nil {
			return nil, withCode(codeMalformed, err)
		}
		result, cite, err := verifyAcquisition(pages[i], args, p, public, at, maxAge)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", i, err)
		}
		var envelope struct {
			Receipt struct {
				PrevSignature *string `json:"prevSignature"`
			} `json:"receipt"`
		}
		_ = json.Unmarshal(pages[i], &envelope)
		if i == 0 {
			for _, c := range earlier {
				if c.SessionID == cite.SessionID {
					return nil, withCode(codeSessions, errors.New("a paged source's session is its own: an earlier source's receipt names it"))
				}
			}
		}
		chained := i == 0 && envelope.Receipt.PrevSignature == nil || i > 0 && envelope.Receipt.PrevSignature != nil && *envelope.Receipt.PrevSignature == previous
		if cite.CallIndex != i || i > 0 && cite.SessionID != out.cites[0].SessionID || !chained {
			return nil, withCode(codeSessions, fmt.Errorf("page %d is not call %d of its source's session, chained to the page before", i, i))
		}
		previous = cite.Signature
		out.cites = append(out.cites, *cite)
		v, list, next, err := readPage(read, rule, result, i)
		if err != nil {
			return nil, err
		}
		items = append(items, list...)
		values = append(values, v)
		if next == "" {
			out.end = "no-next"
			break
		}
		if i+1 == rule.bound {
			out.end = "bound"
			break
		}
		if seen[next] {
			return nil, withCode(codeReadFailed, fmt.Errorf("page %d names a next reference already followed", i))
		}
		seen[next] = true
		out.references = append(out.references, next)
		q = filled(q0, fills, next)
	}
	if len(pages) > len(out.cites) {
		return nil, withCode(codeUnexpected, fmt.Errorf("a page is retained after the loop's end, page %d", len(out.cites)-1))
	}
	out.joined = map[string]any{"end": out.end, "items": items, "last": values[len(values)-1], "pages": json.Number(strconv.Itoa(len(values))), "values": values}
	if valueDepth(out.joined) > maxValueDepth {
		return nil, withCode(codeOverBound, fmt.Errorf("the joined value is nested more than %d levels", maxValueDepth))
	}
	return out, nil
}

// pagedAcquisitionForms holds the acquisitions to the forms format 4.1.0
// gives them (RFC 0033 section 4): a member that has pages is exactly
// {"pages": [...]}, of at least one element each §5.2's envelope, and only a
// paged source's, in a document of format 4.1.0; a paged source's member
// never has response. Every form is input-malformed.
func pagedAcquisitionForms(m InputMapping, pages map[string]json.RawMessage, responses map[string]SourceValue) error {
	malformed := func(format string, a ...any) error {
		return withCode(codeMalformed, fmt.Errorf(format, a...))
	}
	paged := map[string]bool{}
	for _, s := range m.Sources {
		paged[s.Name] = len(s.Paging) > 0
	}
	for _, name := range sortedKeys(pages) {
		if m.FormatVersion != format41 {
			return malformed("the acquisition %s has pages, a form of format 4.1.0 alone", name)
		}
		if !paged[name] {
			return malformed("the acquisition %s has pages, and %s is no paged source", name, name)
		}
		var list []json.RawMessage
		if json.Unmarshal(pages[name], &list) != nil || len(list) == 0 {
			return malformed("the acquisition %s's pages are not an array of at least one page", name)
		}
		for i, p := range list {
			if !operationEnvelopeForm(p) {
				return malformed("page %d of the acquisition %s is not an acquisition envelope", i, name)
			}
		}
	}
	for name := range responses {
		if paged[name] {
			return malformed("the acquisition %s of a paged source has response: its member is {\"pages\": [...]}", name)
		}
	}
	return nil
}

// splitPages takes the members that have pages out of the acquisitions as
// written: they are a paged source's, read offline alone, and every other
// member is decoded as §5.2's. A member that has pages and anything else is
// of neither form.
func splitPages(raw []byte) ([]byte, map[string]json.RawMessage, error) {
	var members map[string]map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return raw, nil, nil
	}
	pages := map[string]json.RawMessage{}
	for name, member := range members {
		held, ok := member["pages"]
		if !ok {
			continue
		}
		if len(member) != 1 {
			return nil, nil, withCode(codeMalformed, fmt.Errorf("the acquisition %s has pages and another member", name))
		}
		pages[name] = held
		delete(members, name)
	}
	if len(pages) == 0 {
		return raw, nil, nil
	}
	rest, err := json.Marshal(members)
	return rest, pages, err
}
