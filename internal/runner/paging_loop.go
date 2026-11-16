package runner

// The run path of a paged source (docs/design/paging.md, section 4): a
// dialect of format 4.1.0 names the pages' rule (RFC 0033), and a job names,
// for each of its paged sources, the cursor kept between runs. An occurrence
// fills the cursor into the case, acquires the source's pages in one gateway
// session, each request after the first formed from the page before, and
// carries the cursor's advance into its run; the run that completes writes
// it (finishRunIn), under its claim and only while the cursor still holds
// what the occurrence read.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// plannedPaging is a paged source's rule and read, which the loop forms each
// request and reads each page's next reference with.
type plannedPaging struct {
	rule       *pagingRule
	sourceRead SourceRead
}

// readPage reads page i's value as the walk does (RFC 0033 section 4, step
// 5): the read's unwrap, then an items array at items and a next reference
// spelled as noNext declares, read-failed otherwise. next is "" at a page
// that names none.
func readPage(read SourceRead, rule *pagingRule, result json.RawMessage, i int) (any, []any, string, error) {
	itemTokens, _ := pointerTokens(rule.Items, true)
	nextTokens, _ := pointerTokens(rule.Next, false)
	parent, member := nextTokens[:len(nextTokens)-1], nextTokens[len(nextTokens)-1]
	artifact, err := readInputJSON(result)
	if err != nil {
		return nil, nil, "", withCode(codeReadFailed, err)
	}
	v, err := unwrapRead(SourceRead{Unwrap: read.Unwrap}, artifact)
	if err != nil {
		return nil, nil, "", err
	}
	pageItems, ok := pointerRead(v, itemTokens)
	list, isList := pageItems.([]any)
	if !ok || !isList {
		return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d holds no items array at %s", i, rule.Items))
	}
	holder, ok := pointerRead(v, parent)
	object, isObject := holder.(map[string]any)
	if !ok || !isObject {
		return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d holds no object where %s names its next reference", i, rule.Next))
	}
	reference, named := object[member]
	switch text, isText := reference.(string); {
	case !named && rule.NoNext == "absent", named && reference == nil && rule.NoNext == "null", isText && text == "" && rule.NoNext == "empty-string":
		return v, list, "", nil
	case isText && text != "":
		if len(text) > maxReferenceBytes {
			return nil, nil, "", withCode(codeOverBound, fmt.Errorf("page %d names a next reference over %d bytes", i, maxReferenceBytes))
		}
		return v, list, text, nil
	}
	return nil, nil, "", withCode(codeReadFailed, fmt.Errorf("page %d's next reference is neither a string nor the end noNext declares", i))
}

// HeldPages are the pages an attempt received before the gateway answered
// a throttle (docs/design/paging.md, section 4.4): the source, the session
// they are calls 0 to n-1 of, which is left unsealed, the first page's
// request they were asked from, and the instant the attempt's windows end
// at. The occurrence keeps them, and its next attempt asks its windows to
// the same instant, so that its first page's request is the one held, and
// asks the throttled page in the same session and no page before it.
type HeldPages struct {
	Source  string            `json:"source"`
	Profile string            `json:"profile"`
	Session string            `json:"session"`
	First   json.RawMessage   `json:"first"`
	At      string            `json:"at,omitempty"`
	Pages   []json.RawMessage `json:"pages"`
}

// PagedRead is what an occurrence, and then its run, says of one paged
// source (docs/design/paging.md, sections 4.2, 4.3 and 4.6): the pages read
// and each page's receipt in call order, how the loop ended, whether the
// read started again from the cursor's first value because the source said
// the reference had expired, and the cursor's advance. The receipts say each
// page was answered; continuity between runs is not established by them
// (RFC 0033 section 9): the first page's request is asserted from the cursor.
type PagedRead struct {
	Source     string         `json:"source"`
	Pages      int            `json:"pages"`
	Receipts   []Citation     `json:"receipts"`
	End        string         `json:"end"`
	FullReRead bool           `json:"fullReRead,omitempty"`
	Cursor     *CursorAdvance `json:"cursor,omitempty"`
}

// CursorAdvance is a paged read's cursor: the value the occurrence read from
// the store (Read, absent when no run had advanced it, the job's first value
// then filling the case), the value it filled into the case (From), the
// value the pages say comes next (To, absent when they say none), and, when
// the read ended at the bound and left pages unread, why the cursor is held
// where it was (Held, "bound"). The run that completes writes To while the
// store still holds Read.
type CursorAdvance struct {
	Read string `json:"read,omitempty"`
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Held string `json:"held,omitempty"`
}

// JobPaging is a job's paging member: a cursor rule for each paged source
// of its release's dialect the job keeps a cursor for.
type JobPaging struct {
	Sources []CursorRule `json:"sources"`
}

// CursorRule is a job's cursor for one paged source (docs/design/paging.md,
// section 4.1): its kind, change or window; the case parameter the cursor
// fills, a string for a change reference and a timestamp for a window's
// start; for a window the case parameter its end fills, with the instant
// the occurrence is prepared; for a change query where in the last page the
// next run's reference is read; and the cursor's first value.
type CursorRule struct {
	Source    string `json:"source"`
	Kind      string `json:"kind"`
	Parameter string `json:"parameter"`
	End       string `json:"end,omitempty"`
	Reference string `json:"reference,omitempty"`
	First     string `json:"first"`
}

// PagingCursor is a job's cursor row for one paged source: its value, the
// run that advanced it and when, and the job's scope identifier, to which
// the row is held as the job is.
type PagingCursor struct {
	JobID     string `json:"jobId"`
	Source    string `json:"source"`
	Value     string `json:"value"`
	RunID     string `json:"runId"`
	UpdatedAt string `json:"updatedAt"`
	Scope     string `json:"scope,omitempty"`
}

var timestampForm = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]Z$`)

// validateCursorRules holds a job's paging member to its release's mapping
// (docs/design/paging.md, section 4.1): each rule names a paged operation
// source of the mapping once, paging_source_unknown otherwise; a case
// parameter of the type its kind fills, paging_parameter_unknown otherwise;
// and is of its form, invalid_paging_rule otherwise.
func validateCursorRules(paging *JobPaging, r Release) error {
	if paging == nil {
		return nil
	}
	rules := paging.Sources
	invalid := func(format string, a ...any) error {
		return bad("invalid_paging_rule", "A job's paging: "+fmt.Sprintf(format, a...))
	}
	if len(rules) == 0 || len(rules) > maxPagedSources {
		return invalid("name 1 to %d paged sources", maxPagedSources)
	}
	m := r.InputMapping
	if m == nil || m.FormatVersion != format41 {
		return bad("paging_source_unknown", "A job's paging names paged sources, and only a dialect of format 4.1.0 has one.")
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		var source *MappingSource
		for i := range m.Sources {
			if m.Sources[i].Name == rule.Source && len(m.Sources[i].Paging) > 0 {
				source = &m.Sources[i]
			}
		}
		if source == nil {
			return bad("paging_source_unknown", "A job's paging names "+rule.Source+", which is no paged operation source of the release's dialect.")
		}
		if seen[rule.Source] {
			return invalid("the source %s is named twice", rule.Source)
		}
		seen[rule.Source] = true
		param := func(name, typ string) error {
			if m.Case == nil {
				return bad("paging_parameter_unknown", "A job's paging names the case parameter "+name+", and the dialect's case has none.")
			}
			p, ok := m.Case.Parameters[name]
			if !ok || p.Type != typ {
				return bad("paging_parameter_unknown", "A job's paging names "+name+", which is no case parameter of type "+typ+".")
			}
			return nil
		}
		if rule.First == "" || len(rule.First) > maxReferenceBytes {
			return invalid("the cursor's first value for %s is 1 to %d bytes", rule.Source, maxReferenceBytes)
		}
		switch rule.Kind {
		case "change":
			if rule.End != "" {
				return invalid("a change cursor names no end")
			}
			if tokens, err := pointerTokens(rule.Reference, false); err != nil || len(tokens) == 0 {
				return invalid("a change cursor's reference is a pointer into the last page, other than the root")
			}
			if err := param(rule.Parameter, "string"); err != nil {
				return err
			}
		case "window":
			if rule.Reference != "" {
				return invalid("a window cursor names no reference: its next value is the window's end")
			}
			if !timestampForm.MatchString(rule.First) {
				return invalid("a window cursor's first value is a timestamp")
			}
			if rule.End == rule.Parameter {
				return invalid("a window's start and end are two case parameters")
			}
			if err := param(rule.Parameter, "timestamp"); err != nil {
				return err
			}
			if err := param(rule.End, "timestamp"); err != nil {
				return err
			}
		default:
			return invalid("a cursor's kind is change or window")
		}
	}
	return nil
}

// pagingEntryVersion is the shape of a journal entry that holds fullReRead
// or cursorHeld, whatever its by, scope, attempt and result, which an entry
// of "1" to "5" never holds.
const pagingEntryVersion = "6"

// maxPagedSources bounds a job's paging member.
const maxPagedSources = 16

// cursorSeed fills each cursor a job names into the case of an occurrence's
// input (docs/design/paging.md, section 4.2): the cursor's value, or its
// first value when no run has advanced it, at the case parameter's pointer,
// and, for a window, the instant at at its end's. The case retained in the
// run then says what the first page was asked with.
func cursorSeed(input *Input, rules []CursorRule, cursors map[string]PagingCursor, at time.Time) (map[string]*CursorAdvance, error) {
	if len(rules) == 0 || input.Source == nil {
		return nil, nil
	}
	m := input.Source.Mapping
	caseRaw := input.Source.Case
	if len(caseRaw) == 0 {
		caseRaw = json.RawMessage(`{}`)
	}
	value, err := readInputJSON(caseRaw)
	if err != nil {
		return nil, inputProblem("invalid case JSON")
	}
	object, ok := value.(map[string]any)
	if !ok || m.Case == nil {
		return nil, inputProblem("case must be one object")
	}
	advances := map[string]*CursorAdvance{}
	for _, rule := range rules {
		a := &CursorAdvance{From: rule.First}
		if c, ok := cursors[rule.Source]; ok {
			a.Read, a.From = c.Value, c.Value
		}
		writeTarget(object, m.Case.Parameters[rule.Parameter].Pointer, a.From)
		if rule.Kind == "window" {
			a.To = at.UTC().Format(windowForm)
			// A window is never asked inverted: a cursor after the
			// instant (a first value in the future, or a clock behind
			// the Runner that moved the cursor) fails the occurrence
			// and leaves the cursor.
			if !windowAfter(a.From, a.To, true) {
				return nil, bad("paging_window_inverted", "The window cursor of "+rule.Source+" starts at "+a.From+", after the instant the occurrence is prepared ("+a.To+"). The window was not asked and the cursor is left.")
			}
			writeTarget(object, m.Case.Parameters[rule.End].Pointer, a.To)
		}
		advances[rule.Source] = a
	}
	input.Source.Case = encode(object)
	return advances, nil
}

// windowForm is the form of a window cursor's value: a timestamp of the
// case's form, in UTC to the second.
const windowForm = "2006-01-02T15:04:05Z"

// windowAfter says whether a window from from to to is not inverted: to is
// strictly after from, or, when equal admits it, the same instant (an empty
// window, which is asked but never advances the cursor). A value that does
// not read as a timestamp is neither.
func windowAfter(from, to string, equal bool) bool {
	f, err := time.Parse(windowForm, from)
	if err != nil {
		return false
	}
	t, err := time.Parse(windowForm, to)
	if err != nil {
		return false
	}
	return t.After(f) || equal && t.Equal(f)
}

// referenceExpired is the code of the gateway's 410 that says the reference
// a paged request sent is no longer held (gateway SPEC, "When an acquisition
// does not happen").
const referenceExpired = "source_reference_expired"

// expiredReference says whether err is the gateway's 410 in its own form
// with the code source_reference_expired.
func expiredReference(err error) bool {
	var g *gatewayAnswer
	return errors.As(err, &g) && g.own && g.status == 410 && g.code == referenceExpired
}

// occurrencePaging is what one attempt's acquisition carries for its paged
// sources: the job's cursors and what was read of them, the pages held from
// an earlier attempt in and those to hold for the next out, whether the
// caller keeps them (an occurrence does; a request has no next attempt), and
// what each paged source read.
type occurrencePaging struct {
	rules    []CursorRule
	cursors  map[string]PagingCursor
	at       time.Time
	hold     bool
	held     *HeldPages
	advances map[string]*CursorAdvance
	reread   map[string]bool
	inner    map[string]bool
	restarts map[string]bool
	reads    map[string]*pagedAcquired
}

// pagedAcquired is one paged source's pages as acquired, and the last page's
// value, which a change cursor's next reference is read from.
type pagedAcquired struct {
	cites []Citation
	end   string
	last  any
}

// acquirePages asks a paged source's pages (docs/design/paging.md, sections
// 4.3 to 4.5): in one gateway session, call i the page i request, formed
// from the first by fills set to the reference page i-1 named, until a page
// names none or maxPages are read. Each page is checked to be call i of the
// session before the next is asked, and kept as its envelope: the run's
// input retains every page, so the pages are bounded by what is left of the
// combined input limit (room), checked as each arrives. A page the gateway
// answers 429 or 503 in its own form was not served: when the caller keeps
// them, the pages before it are held in p, the session is left unsealed,
// and the attempt's error is the gateway's, which the job's retry policy
// reads (retries.go). The next attempt, given them, asks the throttled page
// and none before it. Any other end seals the session and holds nothing.
func (s *Service) acquirePages(ctx context.Context, next PlannedSource, p *occurrencePaging, room int) (SourceValue, error) {
	rule, read := next.paged.rule, next.paged.sourceRead
	gateway := s.gatewayFor(next.Profile)
	if gateway == "" {
		return SourceValue{}, bad("gateway_connection_required", "The Gateway connection is no longer available.")
	}
	q0, err := readInputJSON(next.Arguments)
	if err != nil {
		return SourceValue{}, inputProblem("invalid planned request")
	}
	session, pages := id("job."), []json.RawMessage(nil)
	if h := p.held; h != nil && h.Source == next.Name && h.Profile == next.Profile && sameJSON(h.First, next.Arguments) && len(h.Pages) > 0 {
		session, pages = h.Session, append(pages, h.Pages...)
	} else if h != nil {
		s.sealHeld(ctx, h)
	}
	p.held = nil
	seal := func() { sealSession(ctx, gateway, session) }
	fills, _ := pointerTokens(rule.Fills, false)
	seen := map[string]bool{}
	if v, ok := pointerRead(q0, fills); ok {
		if text, ok := v.(string); ok {
			seen[text] = true
		}
	}
	out := &pagedAcquired{}
	q := q0
	size := 0
	for i := 0; ; i++ {
		if i >= len(pages) {
			args, err := proofCanon(encode(q))
			if err != nil {
				seal()
				return SourceValue{}, withCode(codeMalformed, err)
			}
			raw, err := automaticCall(ctx, gateway+"/acquire", encode(map[string]any{"session": session, "source": next.Source, "arguments": json.RawMessage(args)}), 1<<20)
			if err != nil {
				if _, throttled := transient(err); throttled && p.hold && len(pages) > 0 {
					// Not served: the pages that were are held, in the
					// session left open, with the instant the windows
					// were asked to. A throttled first page holds
					// nothing: the next attempt asks it afresh.
					p.held = &HeldPages{Source: next.Name, Profile: next.Profile, Session: session, First: next.Arguments, At: p.at.UTC().Format(windowForm), Pages: pages}
					return SourceValue{}, err
				}
				if i > 0 && expiredReference(err) {
					// The reference that expired is the source's own
					// next reference, not the cursor (restart).
					if p.inner == nil {
						p.inner = map[string]bool{}
					}
					p.inner[next.Name] = true
				}
				seal()
				return SourceValue{}, err
			}
			pages = append(pages, raw)
		}
		var envelope struct {
			Receipt struct {
				Session   string `json:"sessionId"`
				Index     int    `json:"callIndex"`
				Signature string `json:"signature"`
			} `json:"receipt"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(pages[i], &envelope) != nil || envelope.Receipt.Session != session || envelope.Receipt.Index != i {
			seal()
			return SourceValue{}, bad("gateway_receipt_mismatch", "The Gateway response does not belong to this acquisition.")
		}
		if size += len(pages[i]); size > room {
			seal()
			return SourceValue{}, bad("input_too_large", "A paged source's pages exceed the combined input limit.")
		}
		out.cites = append(out.cites, Citation{SessionID: session, CallIndex: i, Signature: envelope.Receipt.Signature})
		v, _, reference, err := readPage(read, rule, envelope.Result, i)
		if err != nil {
			seal()
			return SourceValue{}, sourceFailure(next.Name, err)
		}
		out.last = v
		if reference == "" {
			out.end = "no-next"
			break
		}
		if i+1 == rule.bound {
			out.end = "bound"
			break
		}
		if seen[reference] {
			seal()
			return SourceValue{}, sourceFailure(next.Name, withCode(codeReadFailed, fmt.Errorf("page %d names a next reference already followed", i)))
		}
		seen[reference] = true
		q = filled(q0, fills, reference)
	}
	// A change cursor's read that reached its last page names the next
	// run's reference there: a last page without one fails the occurrence,
	// so a misnamed reference never passes as a cursor that stays put.
	if out.end == "no-next" {
		for _, rule := range p.rules {
			if rule.Source == next.Name && rule.Kind == "change" {
				if _, err := changeReference(rule, out.last); err != nil {
					seal()
					return SourceValue{}, sourceFailure(next.Name, err)
				}
			}
		}
	}
	seal()
	if p.reads == nil {
		p.reads = map[string]*pagedAcquired{}
	}
	p.reads[next.Name] = out
	return SourceValue{Pages: pages[:len(out.cites)]}, nil
}

// changeReference is the next run's reference a change cursor reads in the
// last page of a read that ended at no-next (docs/design/paging.md, section
// 7): a string of 1 to maxReferenceBytes bytes at the rule's reference,
// read-failed when the page names none there, over-bound when it is longer.
func changeReference(rule CursorRule, last any) (string, error) {
	tokens, _ := pointerTokens(rule.Reference, false)
	v, ok := pointerRead(last, tokens)
	text, isText := v.(string)
	switch {
	case !ok || !isText || text == "":
		return "", withCode(codeReadFailed, fmt.Errorf("the last page names no change reference at %s, where the job's cursor reads the next run's", rule.Reference))
	case len(text) > maxReferenceBytes:
		return "", withCode(codeOverBound, fmt.Errorf("the last page's change reference at %s is over %d bytes", rule.Reference, maxReferenceBytes))
	}
	return text, nil
}

// gatewayFor is the URL of the gateway connection of a profile, or "".
func (s *Service) gatewayFor(profile string) string {
	gateway := ""
	for _, g := range s.cfg.GatewayConnections {
		if g.Profile == profile {
			gateway = g.URL
		}
	}
	return gateway
}

// sealSession seals a gateway session, bounded, as an acquisition's
// housekeeping is: it never retries anything.
func sealSession(ctx context.Context, gateway, session string) {
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	_, _ = automaticCall(cleanup, gateway+"/seal", encode(map[string]string{"session": session}), 64<<10)
	stop()
}

// sealHeld seals the session of pages held for an attempt that will not
// resume them.
func (s *Service) sealHeld(ctx context.Context, h *HeldPages) {
	if h == nil {
		return
	}
	if gateway := s.gatewayFor(h.Profile); gateway != "" {
		sealSession(ctx, gateway, h.Session)
	}
}

// restart starts a paged source's read again after the gateway said a
// reference it was asked with had expired (docs/design/paging.md, sections
// 4.6 and 7), once per source, in a new session. A later page's reference
// is the source's own next reference: the read starts again from the same
// cursor value, which is no full re-read. Page 0's is the cursor's: the
// case's cursor parameter is filled with the first value, and the source is
// planned again from it, only for a change cursor's read that did not start
// from the first value. Any other expired reference fails the occurrence
// and leaves the cursor.
func (p *occurrencePaging) restart(input *Input, source string) bool {
	if p == nil || input.Source == nil || p.restarts[source] {
		return false
	}
	if p.inner[source] {
		delete(p.inner, source)
		p.restarted(source)
		return true
	}
	if input.Source.Mapping.Case == nil {
		return false
	}
	advance := p.advances[source]
	for _, rule := range p.rules {
		if rule.Source != source || rule.Kind != "change" || advance == nil || advance.From == rule.First {
			continue
		}
		value, err := readInputJSON(input.Source.Case)
		object, ok := value.(map[string]any)
		if err != nil || !ok {
			return false
		}
		writeTarget(object, input.Source.Mapping.Case.Parameters[rule.Parameter].Pointer, rule.First)
		input.Source.Case = encode(object)
		advance.From = rule.First
		if p.reread == nil {
			p.reread = map[string]bool{}
		}
		p.reread[source] = true
		p.restarted(source)
		return true
	}
	return false
}

// restarted records that a source's read was started again, which it is
// once.
func (p *occurrencePaging) restarted(source string) {
	if p.restarts == nil {
		p.restarts = map[string]bool{}
	}
	p.restarts[source] = true
}

// result is what the occurrence carries into its run for each paged source
// it acquired (docs/design/paging.md, sections 4.2, 4.3 and 4.6): the pages
// and their receipts, the loop's end, a full re-read, and, for a source the
// job keeps a cursor for, its advance. A change cursor's next value is read
// from the last page; a window's is its end. A read the bound ended, which
// left pages unread, advances neither, and the next run reads from the same
// cursor again.
func (p *occurrencePaging) result() []PagedRead {
	if p == nil {
		return nil
	}
	var out []PagedRead
	byName := map[string]CursorRule{}
	for _, r := range p.rules {
		byName[r.Source] = r
	}
	for _, name := range sortedKeys(p.reads) {
		a := p.reads[name]
		read := PagedRead{Source: name, Pages: len(a.cites), Receipts: a.cites, End: a.end, FullReRead: p.reread[name]}
		if adv, ok := p.advances[name]; ok {
			c := *adv
			rule := byName[name]
			if rule.Kind == "change" {
				c.To, _ = changeReference(rule, a.last)
			}
			if a.end != "no-next" {
				// The bound left pages unread: the cursor is held
				// where it was, and the read says so.
				c.To, c.Held = "", "bound"
			}
			// A window advances only when its end is strictly after
			// its start: an empty window moves nothing, and an inverted
			// one, which cursorSeed refuses, never moves it back.
			if rule.Kind == "window" && c.To != "" && !windowAfter(c.From, c.To, false) {
				c.To = ""
			}
			read.Cursor = &c
		}
		out = append(out, read)
	}
	return out
}

// advanceCursors writes, in the transaction that records r completed, the
// advance of each cursor its occurrence read (docs/design/paging.md, section
// 4.2), only while the row still holds what the occurrence read: a run that
// started from an older cursor and completes late never moves it back. A run
// that ended otherwise writes nothing, and one whose claim was lost writes
// nothing at all (finishRunIn).
func advanceCursors(tx runTx, r Run) error {
	if r.State != "completed" || r.Trigger == nil {
		return nil
	}
	for _, read := range r.Trigger.Paging {
		if read.Cursor == nil || read.Cursor.To == "" {
			continue
		}
		c := PagingCursor{JobID: r.JobID, Source: read.Source, Value: read.Cursor.To, RunID: r.ID, UpdatedAt: now(), Scope: r.Scope}
		if _, err := tx.advancePagingCursor(c, read.Cursor.Read); err != nil {
			return err
		}
	}
	return nil
}

// errPagingRule is the refusal of a paging member that is not a list of
// rules, case apart.
var errPagingRule = bad("invalid_paging_rule", "A job's paging names its sources, a list of cursor rules, one per paged source.")

// jobPaging reads POST /v1/jobs's paging member: absent, none; named, a
// member sources, a list of rules, each of exactly its members (decodeRaw),
// named in its own case and not null.
func jobPaging(raw []byte, rules *JobPaging) (*JobPaging, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return nil, errPagingRule
	}
	for name := range members {
		if name != "paging" && strings.EqualFold(name, "paging") {
			return nil, errPagingRule
		}
	}
	held, named := members["paging"]
	if !named {
		return nil, nil
	}
	if rules == nil || bytes.Equal(bytes.TrimSpace(held), []byte("null")) {
		return nil, errPagingRule
	}
	return rules, nil
}

// occurrencePagingOf is the paged sources' state of an occurrence's attempt:
// its job's cursor rules and cursors, the instant a window ends at, and the
// pages held from its last attempt. hold says whether the job's retry policy
// can give it another attempt, which pages received before a throttle are
// held for.
func (s *Service) occurrencePagingOf(o Occurrence, hold bool) (*occurrencePaging, error) {
	p := &occurrencePaging{at: s.instant(), hold: hold, held: o.HeldPages}
	// An attempt that resumes held pages asks its windows to the instant
	// the held pages were asked to (HeldPages).
	if h := o.HeldPages; h != nil && h.At != "" {
		if at, err := time.Parse(windowForm, h.At); err == nil {
			p.at = at
		}
	}
	j, err := s.job(o.JobID)
	if err != nil {
		return nil, err
	}
	if j.Paging == nil {
		return p, nil
	}
	p.rules = j.Paging.Sources
	if p.cursors, err = s.store.pagingCursors(o.JobID); err != nil {
		return nil, err
	}
	return p, nil
}
