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
	"slices"
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
// where it was (Held, "bound"). In a run's record, a read whose advance the
// run that completed did not write, the cursor having moved on from Read,
// names neither To nor Unfinished, and Held "moved" (advanceCursors). A
// window cursor's read also names the
// unfinished window the store held, which it continued (Continued), and the
// unfinished window it leaves for the next run when the bound ended it with
// a position kept (Unfinished, docs/design/paging.md, section 7). The run
// that completes writes To, or Unfinished, while the store still holds Read
// and Continued.
type CursorAdvance struct {
	Read       string            `json:"read,omitempty"`
	From       string            `json:"from"`
	To         string            `json:"to,omitempty"`
	Held       string            `json:"held,omitempty"`
	Continued  *UnfinishedWindow `json:"continued,omitempty"`
	Unfinished *UnfinishedWindow `json:"unfinished,omitempty"`
}

// UnfinishedWindow is a window a read the bound ended left unfinished: the
// window's end, which the next run asks again, and the position inside it,
// the next reference the last page read named, which the next run's first
// request is formed from (through the case, RFC 0033 section 9). The cursor's
// value stays the window's start until the window is finished.
type UnfinishedWindow struct {
	End      string `json:"end"`
	Position string `json:"position"`
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
// next run's reference is read; and the cursor's first value. A window may
// name a position: the case parameter, of type string, that its source's
// request carries at the member its next reference fills, and the value it
// is filled with when a window starts (PositionFirst). A window the bound
// ends is then left unfinished, at a position the next run continues from.
type CursorRule struct {
	Source        string  `json:"source"`
	Kind          string  `json:"kind"`
	Parameter     string  `json:"parameter"`
	End           string  `json:"end,omitempty"`
	Reference     string  `json:"reference,omitempty"`
	First         string  `json:"first"`
	Position      string  `json:"position,omitempty"`
	PositionFirst *string `json:"positionFirst,omitempty"`
}

// PagingCursor is a job's cursor row for one paged source: its value, the
// run that advanced it and when, the job's scope identifier, to which the
// row is held as the job is, and, for a window the bound left unfinished,
// the window's end and the position the next run continues from.
type PagingCursor struct {
	JobID      string            `json:"jobId"`
	Source     string            `json:"source"`
	Value      string            `json:"value"`
	RunID      string            `json:"runId"`
	UpdatedAt  string            `json:"updatedAt"`
	Scope      string            `json:"scope,omitempty"`
	Unfinished *UnfinishedWindow `json:"unfinished,omitempty"`
}

// JobCursors is what GET /v1/jobs/{job}/cursors serves: for each paged
// source the job's paging names, in its order, the cursor kept between runs.
type JobCursors struct {
	JobID   string      `json:"jobId"`
	Cursors []JobCursor `json:"cursors"`
}

// JobCursor is one paged source's cursor: its kind, its value, the run that
// last moved it and when (none of the three while no run has: the next
// occurrence fills the rule's first value), and whether a window the bound
// ended is unfinished, with its end and the position the next run continues
// from.
type JobCursor struct {
	Source           string            `json:"source"`
	Kind             string            `json:"kind"`
	Value            string            `json:"value,omitempty"`
	RunID            string            `json:"runId,omitempty"`
	UpdatedAt        string            `json:"updatedAt,omitempty"`
	WindowUnfinished bool              `json:"windowUnfinished"`
	Unfinished       *UnfinishedWindow `json:"unfinished,omitempty"`
}

// jobCursorsOf is j's cursors, from its rows.
func jobCursorsOf(j Job, rows map[string]PagingCursor) JobCursors {
	out := JobCursors{JobID: j.ID, Cursors: []JobCursor{}}
	if j.Paging == nil {
		return out
	}
	for _, rule := range j.Paging.Sources {
		c := JobCursor{Source: rule.Source, Kind: rule.Kind}
		if row, ok := rows[rule.Source]; ok {
			c.Value, c.RunID, c.UpdatedAt, c.Unfinished = row.Value, row.RunID, row.UpdatedAt, row.Unfinished
		}
		c.WindowUnfinished = c.Unfinished != nil
		out.Cursors = append(out.Cursors, c)
	}
	return out
}

var timestampForm = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9]Z$`)

// validateJobPaging is validateCursorRules, and refuses a job's paging on a
// release whose mapping is acquired over durable Gateway connections
// (paging_durable_unsupported): that path neither seeds a cursor nor pages
// (docs/design/paging.md, section 6), so the job could never advance.
func (s *Service) validateJobPaging(paging *JobPaging, r Release) error {
	if err := validateCursorRules(paging, r); err != nil {
		return err
	}
	if paging != nil && s.durableMapping(r) {
		return bad("paging_durable_unsupported", "A job's paging keeps a cursor for a paged source, and this release's sources are acquired over durable Gateway connections, which are never paged.")
	}
	return nil
}

// validateCursorRules holds a job's paging member to its release's mapping
// (docs/design/paging.md, section 4.1): each rule names a paged operation
// source of the mapping once, paging_source_unknown otherwise; a case
// parameter of the type its kind fills, which the source's request names
// ($param), paging_parameter_unknown otherwise; and is of its form,
// invalid_paging_rule otherwise.
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
		committed := map[string]bool{}
		if request, err := readInputJSON(source.Arguments); err == nil {
			requestParameters(request, committed, map[string]bool{})
		}
		param := func(name, typ string) error {
			if m.Case == nil {
				return bad("paging_parameter_unknown", "A job's paging names the case parameter "+name+", and the dialect's case has none.")
			}
			p, ok := m.Case.Parameters[name]
			if !ok || p.Type != typ {
				return bad("paging_parameter_unknown", "A job's paging names "+name+", which is no case parameter of type "+typ+".")
			}
			if !committed[name] {
				return bad("paging_parameter_unknown", "A job's paging names "+name+", which the request of "+rule.Source+" does not carry ($param), so the cursor would never reach the source.")
			}
			return nil
		}
		if rule.First == "" || len(rule.First) > maxReferenceBytes {
			return invalid("the cursor's first value for %s is 1 to %d bytes", rule.Source, maxReferenceBytes)
		}
		if (rule.Position == "") != (rule.PositionFirst == nil) {
			return invalid("a window's position and positionFirst are named together")
		}
		switch rule.Kind {
		case "change":
			if rule.End != "" {
				return invalid("a change cursor names no end")
			}
			if rule.Position != "" {
				return invalid("a change cursor names no position: a read the bound ends holds it")
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
			if rule.Position != "" {
				if err := windowPosition(rule, source, param); err != nil {
					return err
				}
			}
		default:
			return invalid("a cursor's kind is change or window")
		}
	}
	return nil
}

// windowPosition holds a window's position to its source (docs/design/paging.md,
// section 7): a case parameter of type string, other than the window's start
// and end, that the request carries exactly at the member the next reference
// fills, so that a next run's first request, formed from the case, is the
// request of the page the bound left unread; and a positionFirst of at most
// maxReferenceBytes bytes.
func windowPosition(rule CursorRule, source *MappingSource, param func(name, typ string) error) error {
	if rule.Position == rule.Parameter || rule.Position == rule.End {
		return bad("invalid_paging_rule", "A job's paging: a window's position is a case parameter other than its start and end.")
	}
	if len(*rule.PositionFirst) > maxReferenceBytes {
		return bad("invalid_paging_rule", fmt.Sprintf("A job's paging: a window's positionFirst is at most %d bytes.", maxReferenceBytes))
	}
	if err := param(rule.Position, "string"); err != nil {
		return err
	}
	paged, err := pagingOf(format41, *source)
	request, e := readInputJSON(source.Arguments)
	if err != nil || paged == nil || e != nil {
		return bad("paging_source_unknown", "A job's paging names "+rule.Source+", whose paging cannot be read.")
	}
	fills, _ := pointerTokens(paged.Fills, false)
	held, _ := pointerRead(request, fills)
	form, ok := held.(map[string]any)
	if name, isText := form["$param"].(string); !ok || len(form) != 1 || !isText || name != rule.Position {
		return bad("paging_parameter_unknown", "A job's paging names the position "+rule.Position+", which the request of "+rule.Source+" does not carry ($param) at "+paged.Fills+", the member its next reference fills.")
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
		c, ok := cursors[rule.Source]
		if ok {
			a.Read, a.From = c.Value, c.Value
		}
		writeTarget(object, m.Case.Parameters[rule.Parameter].Pointer, a.From)
		if rule.Kind == "window" {
			a.To = at.UTC().Format(windowForm)
			// A window the bound left unfinished is asked again to its own
			// end, from the position kept, which fills the case as the
			// cursor does (RFC 0033 section 9): the case retained in the
			// run says where its first page began. A rule that names no
			// position reads the same window from its first page.
			position := ""
			if rule.PositionFirst != nil {
				position = *rule.PositionFirst
			}
			if ok && c.Unfinished != nil {
				held := *c.Unfinished
				a.Continued, a.To, position = &held, held.End, held.Position
			}
			if rule.Position != "" {
				writeTarget(object, m.Case.Parameters[rule.Position].Pointer, position)
			}
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
// what each paged source read. write, for an occurrence, writes the pages of
// the read in progress to its record (docs/design/paging.md, section 4.5):
// each page is written as it arrives, before the next is asked, so that the
// record holds every page received and an attempt that ends, however it
// ends, never leaves a page the source answered only in memory. A request
// has none.
type occurrencePaging struct {
	rules    []CursorRule
	cursors  map[string]PagingCursor
	at       time.Time
	hold     bool
	held     *HeldPages
	write    func(*HeldPages) error
	advances map[string]*CursorAdvance
	reread   map[string]bool
	inner    map[string]bool
	restarts map[string]bool
	reads    map[string]*pagedAcquired
}

// pagedAcquired is one paged source's pages as acquired, the last page's
// value, which a change cursor's next reference is read from, and, for a
// read the bound ended, the next reference that page named, where a window
// left unfinished is continued from.
type pagedAcquired struct {
	cites []Citation
	end   string
	last  any
	next  string
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
	// The pages held from the last attempt are in the record already; each
	// page received after them is written to it as it arrives.
	written := len(pages)
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
		// The page is verified: it is written to the occurrence's record
		// before the next is asked (section 4.5). A write refused because
		// the occurrence is no longer this attempt's (a restart or a lapsed
		// lease failed it) stops the read: no page is asked after it.
		if p.write != nil && i >= written {
			if err := p.write(&HeldPages{Source: next.Name, Profile: next.Profile, Session: session, First: next.Arguments, At: p.at.UTC().Format(windowForm), Pages: pages[:i+1]}); err != nil {
				seal()
				return SourceValue{}, err
			}
		}
		if reference == "" {
			out.end = "no-next"
			break
		}
		if i+1 == rule.bound {
			out.end, out.next = "bound", reference
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
		if rule.Source == source && rule.Kind == "window" && rule.Position != "" && advance != nil && advance.Continued != nil {
			// The position a window was continued from expired: the same
			// window is read again from its first page, its end kept.
			value, err := readInputJSON(input.Source.Case)
			object, ok := value.(map[string]any)
			if err != nil || !ok {
				return false
			}
			writeTarget(object, input.Source.Mapping.Case.Parameters[rule.Position].Pointer, *rule.PositionFirst)
			input.Source.Case = encode(object)
			if p.reread == nil {
				p.reread = map[string]bool{}
			}
			p.reread[source] = true
			p.restarted(source)
			return true
		}
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
				// The bound left pages unread. A window whose rule names a
				// position is left unfinished at the next reference the
				// last page named, its end kept, for the next run to
				// continue; any other cursor is held where it was, and the
				// read says so.
				if rule.Kind == "window" && rule.Position != "" && a.next != "" && windowAfter(c.From, c.To, false) {
					c.Unfinished = &UnfinishedWindow{End: c.To, Position: a.next}
					c.To = ""
				} else {
					c.To, c.Held = "", "bound"
				}
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

// cursorMoved is a paged read's held when the run that completes finds its
// job's cursor moved on from what its occurrence read: another run moved it
// since, and this run's advance is not written.
const cursorMoved = "moved"

// advanceCursors writes, in the transaction that records r completed, the
// advance of each cursor its occurrence read (docs/design/paging.md, section
// 4.2), only while the row still holds what the occurrence read: a run that
// started from an older cursor and completes late never moves it back. A run
// that ended otherwise writes nothing, and one whose claim was lost writes
// nothing at all (finishRunIn).
//
// An advance the row no longer admits is never dropped in silence: it
// answers r as it is recorded, each such read's cursor naming neither to
// nor unfinished and held "moved", and the sources, for the run.completed
// entry's cursorHeld. r itself is left as it is: the record answered is a
// copy, made again by every run of the transaction.
func advanceCursors(tx runTx, r Run) (Run, []string, error) {
	if r.State != "completed" || r.Trigger == nil {
		return r, nil, nil
	}
	var moved []string
	var paging []PagedRead
	for i, read := range r.Trigger.Paging {
		a := read.Cursor
		if a == nil || a.To == "" && a.Unfinished == nil {
			continue
		}
		// A window left unfinished keeps its start as the cursor's value,
		// beside its end and the position kept.
		c := PagingCursor{JobID: r.JobID, Source: read.Source, Value: a.To, RunID: r.ID, UpdatedAt: now(), Scope: r.Scope, Unfinished: a.Unfinished}
		if a.Unfinished != nil {
			c.Value = a.From
		}
		wrote, err := tx.advancePagingCursor(c, a.Read, a.Continued)
		if err != nil {
			return r, nil, err
		}
		if wrote {
			continue
		}
		if paging == nil {
			paging = slices.Clone(r.Trigger.Paging)
		}
		held := *a
		held.To, held.Unfinished, held.Held = "", nil, cursorMoved
		paging[i].Cursor = &held
		moved = append(moved, read.Source)
	}
	if paging != nil {
		origin := *r.Trigger
		origin.Paging = paging
		r.Trigger = &origin
	}
	return r, moved, nil
}

// errPagingRule is the refusal of a paging member that is not a list of
// rules, case apart.
var errPagingRule = bad("invalid_paging_rule", "A job's paging names its sources, a list of cursor rules, one per paged source.")

// jobPaging reads POST /v1/jobs's paging member: absent, none; named, a
// member sources, a list of rules, each of exactly its members (decodeRaw),
// every member at every level named in its own case and not null, so that
// a member the decoder would take for another by its case never is.
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
	var paging map[string]json.RawMessage
	if json.Unmarshal(held, &paging) != nil || !exactNames(paging, "sources") {
		return nil, errPagingRule
	}
	var sources []map[string]json.RawMessage
	if json.Unmarshal(paging["sources"], &sources) != nil {
		return nil, errPagingRule
	}
	for _, rule := range sources {
		if !exactNames(rule, "source", "kind", "parameter", "end", "reference", "first", "position", "positionFirst") {
			return nil, errPagingRule
		}
	}
	return rules, nil
}

// exactNames says whether every member of an object is one of names,
// spelled exactly so, and none is null.
func exactNames(object map[string]json.RawMessage, names ...string) bool {
	for name, value := range object {
		known := false
		for _, n := range names {
			known = known || name == n
		}
		if !known || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

// errHeldForJob is the claim of an occurrence's preparation not made, and
// nothing written, because its job keeps cursors and has a run, or an
// occurrence admitted before it, that has not settled (claimPreparation).
// The occurrence stays accepted, and is taken once that work has settled.
var errHeldForJob = errors.New("the occurrence's job has a run, or an occurrence admitted before it, that has not settled")

// errCursorsUnclaimed is the refusal to page an occurrence of a job that
// keeps cursors from cursors not read in the transaction that claimed it.
var errCursorsUnclaimed = errors.New("the job's cursors were not read with the occurrence's claim")

// heldForJob says, from r, whether o, accepted with input to prepare, waits
// for its job (docs/design/paging.md, section 7, "Overlap"): a job that keeps
// cursors (paged) with a run queued or running, or an occurrence admitted
// before o that is accepted, preparing, waiting or needing attention
// (jobActivityBefore). That work reads or moves the cursors o would read, so
// o reads them only once it has settled, whatever the trigger's overlap
// rule. An occurrence whose queue time has run out at at, or whose expiry
// cannot be read, is never held, and its preparation ends it as before; nor
// is one whose job record cannot be read, which its preparation fails for
// that record before any cursor is read (jobRetryPolicy,
// heldRecordOrReleased).
func heldForJob(r storeReads, o Occurrence, at time.Time) (paged, held bool, err error) {
	if !withinQueueTime(o, at) {
		return false, false, nil
	}
	j, err := r.job(o.JobID)
	if faultOf(err) == faultUnreadable || absent(err) {
		return false, false, nil
	}
	if err != nil || j.Paging == nil {
		return false, false, err
	}
	n, err := r.jobActivityBefore(o.JobID, o.ID)
	return true, n > 0, err
}

// withinQueueTime says whether an accepted occurrence's queue time has not
// run out at at, its expiry read.
func withinQueueTime(o Occurrence, at time.Time) bool {
	expires, err := time.Parse(time.RFC3339Nano, o.ExpiresAt)
	return err == nil && at.Before(expires)
}

// claimedCursors are the cursors of a paged occurrence's job, read in the
// transaction that claimed it (claimPreparation).
type claimedCursors struct{ rows map[string]PagingCursor }

// claimPreparation is claimOccurrence for an accepted occurrence's
// preparation. For a job that keeps cursors the same transaction checks the
// job's activity (heldForJob) and reads its cursors: an occurrence its job
// holds back is not claimed and nothing is written (errHeldForJob), and one
// claimed is paged from the cursors read with its claim. The check and the
// read are in the claim's transaction, which commits after every writer
// before it and before every one after it (the sqlite kind's one writer, the
// second kind's lock of journal_head, docs/design/store-postgresql.md, D13),
// so no other Runner of the store, nor another worker of this one, makes or
// ends the work they read in between: an occurrence of the job admitted
// later counts this one, accepted, as activity before it, and waits.
func (s *Service) claimPreparation(id string) (Occurrence, *claimedCursors, error) {
	claim, err := newOccurrenceClaim()
	if err != nil {
		return Occurrence{}, nil, err
	}
	var o Occurrence
	var read *claimedCursors
	err = s.store.update(func(tx runTx) error {
		var e error
		read = nil
		if o, e = tx.claimOccurrence(id, "accepted", claim); e != nil {
			return e
		}
		paged, held, e := heldForJob(tx, o, s.instant())
		if e != nil {
			return e
		}
		if held {
			return errHeldForJob
		}
		if !paged {
			return nil
		}
		rows, e := tx.pagingCursors(o.JobID)
		if e != nil {
			return e
		}
		read = &claimedCursors{rows: rows}
		return nil
	})
	return o, read, err
}

// occurrencePagingOf is the paged sources' state of an occurrence's attempt:
// its job's cursor rules, the cursors read with its claim (claimPreparation),
// the instant a window ends at, and the pages held from its last attempt.
// hold says whether the job's retry policy can give it another attempt,
// which pages received before a throttle are held for.
func (s *Service) occurrencePagingOf(o Occurrence, hold bool, claimed *claimedCursors) (*occurrencePaging, error) {
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
	// A job's paging is not changed while it has an occurrence open
	// (job_work_pending), so the job claimPreparation read keeps cursors as
	// this one does; cursors read since its claim are never paged from.
	if claimed == nil {
		return nil, errCursorsUnclaimed
	}
	p.rules = j.Paging.Sources
	p.cursors = claimed.rows
	return p, nil
}
