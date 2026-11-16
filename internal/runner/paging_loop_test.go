package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pagedCall is one acquisition the paged feed was asked: its session, the
// request's change reference, window and next reference, and the status it
// was answered.
type pagedCall struct {
	session, since, from, until, cursor string
	status                              int
}

// pagedFeed is a stub gateway whose source pages (RFC 0033): page k holds
// the items 2k and 2k+1 and names ref-(k+1) next, and the last of its pages
// names none and a change reference after-<since>. It answers each call of
// a session as the session's next call index, its receipt chained to the
// call before, and a call answer gives another status with the gateway's
// own refusal: a 410 with source_reference_expired, a 429 with a wait.
type pagedFeed struct {
	t      *testing.T
	p      InputProfile
	key    ed25519.PrivateKey
	pages  int
	answer func(c pagedCall) int
	url    string
	mu     sync.Mutex
	calls  []pagedCall
	chain  map[string][]string
	sealed map[string]bool
}

func newPagedFeed(t *testing.T, pages int, answer func(c pagedCall) int) *pagedFeed {
	t.Helper()
	p, key := testProfile(t)
	f := &pagedFeed{t: t, p: p, key: key, pages: pages, answer: answer, chain: map[string][]string{}, sealed: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *pagedFeed) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session   string          `json:"session"`
		Arguments json.RawMessage `json:"arguments"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/seal" {
		f.sealed[req.Session] = true
		w.Write([]byte(`{}`))
		return
	}
	var args struct {
		Arguments struct {
			Since, From, Until, Cursor string
		} `json:"arguments"`
	}
	json.Unmarshal(req.Arguments, &args)
	c := pagedCall{session: req.Session, since: args.Arguments.Since, from: args.Arguments.From, until: args.Arguments.Until, cursor: args.Arguments.Cursor, status: 200}
	if f.answer != nil {
		c.status = f.answer(c)
	}
	f.calls = append(f.calls, c)
	if c.status != 200 {
		code := "gateway_busy"
		if c.status == 410 {
			code = referenceExpired
		} else {
			w.Header().Set("Retry-After", "1")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.status)
		fmt.Fprintf(w, `{"code":%q,"error":"not served"}`, code)
		return
	}
	k := 0
	if c.cursor != "" {
		k, _ = strconv.Atoi(strings.TrimPrefix(c.cursor, "ref-"))
	}
	page := map[string]any{"items": []int{2 * k, 2*k + 1}}
	if k+1 < f.pages {
		page["next"] = "ref-" + strconv.Itoa(k+1)
	} else {
		page["latest"] = "after-" + c.since
	}
	chain := f.chain[req.Session]
	index := len(chain)
	var previous any
	if index > 0 {
		previous = chain[index-1]
	}
	salt := bytes.Repeat([]byte{byte(index + 1)}, 32)
	result := encode(page)
	at := time.Now().UTC().Format(time.RFC3339)
	receipt := map[string]any{"receiptVersion": "3", "kind": "acquisition", "sessionId": req.Session, "callIndex": index, "prevSignature": previous, "source": f.p.Source, "resultDigest": digest(canonTest(f.t, result)), "servedAt": at, "authority": f.p.Authority, "keyId": keyID(f.p.PublicKey), "argumentsCommitment": digest(append(append(salt, []byte("args:")...), canonTest(f.t, req.Arguments)...)), "caller": nil, "acquisition": map[string]any{"shape": f.p.Shape, "adapter": f.p.Adapter, "endpoint": f.p.Endpoint, "statement": nil, "snapshot": "query-123", "peerIdentity": nil, "schema": nil, "upstreamToken": nil, "observedAt": at}}
	signReceipt(f.t, receipt, f.key)
	f.chain[req.Session] = append(chain, receipt["signature"].(string))
	json.NewEncoder(w).Encode(map[string]any{"receipt": receipt, "result": json.RawMessage(result), "salts": map[string]string{"args": hex.EncodeToString(salt)}})
}

// taken are the calls the feed was asked since the last, and forgets them.
func (f *pagedFeed) taken() []pagedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func (f *pagedFeed) isSealed(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sealed[session]
}

// pagedFeedDialect is a dialect of format 4.1.0 whose one source, changes,
// is the feed's, paged by its next reference into the request's cursor: its
// case names a change reference, since, and a window, from and until, each
// passed to the source.
func pagedFeedDialect(p InputProfile) string {
	return `{"formatVersion":"4.1.0","id":"urn:judgmentpack:dialect:paged-feed","version":"0.1.0",
 "case":{"parameters":{"since":{"pointer":"/since","type":"string"},"from":{"pointer":"/from","type":"timestamp"},"until":{"pointer":"/until","type":"timestamp"}},"facts":[],"evidence":[]},
 "sources":[{"name":"changes","kind":"operation","profile":"` + p.ID + `","profileDigest":"` + profileHash(p) + `","maxAge":300,
  "arguments":{"tool":"lookup","arguments":{"since":{"$param":"since"},"from":{"$param":"from"},"until":{"$param":"until"}}},
  "paging":{"next":"/next","noNext":"absent","fills":"/arguments/cursor","items":"/items","maxPages":8},
  "read":{"copy":{"facts":[{"target":"/changes/end","source":"/end"},{"target":"/changes/items","source":"/items"},{"target":"/changes/pages","source":"/pages"}],"evidence":[]}}}]}`
}

// unversionedRuntime is completingRuntime for a run of a preparation, its
// evidence and citations in its record, naming no release, as a build
// without its release's version does: the run is evaluated without a
// binding (bindingStatusOf).
func unversionedRuntime(t *testing.T, dir string) string {
	return fakeRuntime(t, dir, `if [ "$1" = version ]; then printf '%s\n' '{"tool":{"version":"0.0.0"}}'; exit 0; fi
mkdir -p audit
printf '{"recordVersion":"1","kind":"evaluation","run":"fake","reviewed":true,"pack":{"digest":"%s"},"inputs":{"facts":%s,"evidence":%s,"evidenceSupplied":true},"cites":%s,"disposition":`+journalDisposition+`}\n' '`+digest([]byte(journalPack))+`' "$(cat facts.json)" "$(cat evidence.json)" "$(cat cites.json)" > audit/evaluations.jsonl
printf '%s\n' '{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"0.2.0-draft","rehearsal":false,"disposition":`+journalDisposition+`}'`)
}

// changeCursor keeps the change reference the last page names, from start.
var changeCursor = &JobPaging{Sources: []CursorRule{{Source: "changes", Kind: "change", Parameter: "since", Reference: "/latest", First: "start"}}}

// pagedWorld is a job on a release of the feed's dialect, keeping the cursor
// paging names, with a schedule trigger whose occurrences acquire from the
// feed; its runs are evaluated by a Runtime that completes them.
type pagedWorld struct {
	s     *Service
	feed  *pagedFeed
	job   Job
	tr    Trigger
	clock *time.Time
	input *AutomaticInput
}

func newPagedWorld(t *testing.T, policy *RetryPolicy, paging *JobPaging, feed *pagedFeed) *pagedWorld {
	t.Helper()
	cfg := journalConfig(t)
	cfg.disableAutomation = true
	cfg.InputRoot = t.TempDir()
	s, e := Open(cfg)
	must(t, e)
	t.Cleanup(func() { s.Close() })
	clock := time.Now().UTC().Truncate(time.Second)
	s.clock = func() time.Time { return clock }
	r := journalRelease(t, s, unversionedRuntime(t, s.cfg.Dir))
	var m InputMapping
	must(t, strictJSON([]byte(pagedFeedDialect(feed.p)), &m))
	cfg.InputProfiles = []InputProfile{feed.p}
	cfg.GatewayConnections = []GatewayConnection{{Profile: feed.p.ID, URL: feed.url}}
	s.cfg = cfg
	r.InputMapping, r.InputProfiles = &m, []InputProfile{feed.p}
	r.Dialect, e = releaseDialectIdentity(r)
	must(t, e)
	_, e = sqliteDB(s).Exec("UPDATE releases SET record=? WHERE id=?", string(encode(r)), r.ID)
	must(t, e)
	j, e := s.createJobPaged(requester{}, "Paging", r.ID, "", nil, policy, paging)
	must(t, e)
	c := constantTrigger(time.Now())
	c.Input = &AutomaticInput{Kind: "mapped-sources", Case: json.RawMessage(`{"since":"seed","from":"2026-01-01T00:00:00Z","until":"2026-01-01T00:00:00Z"}`)}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	must(t, e)
	return &pagedWorld{s: s, feed: feed, job: j, tr: tr, clock: &clock, input: c.Input}
}

// prepare admits an occurrence under key and lets the preparation workers
// take one turn.
func (w *pagedWorld) prepare(t *testing.T, key string) Occurrence {
	t.Helper()
	o := newOccurrence(w.tr, w.job, time.Now())
	o.PendingInput = w.input
	must(t, w.s.store.update(func(tx runTx) error { _, err := w.s.admitOccurrence(tx, w.tr, o, key, ""); return err }))
	return w.step(t, o.ID)
}

func (w *pagedWorld) step(t *testing.T, id string) Occurrence {
	t.Helper()
	must(t, w.s.prepareOccurrence(t.Context()))
	o, e := w.s.occurrence(id)
	must(t, e)
	return o
}

// run dispatches an accepted occurrence and evaluates its run.
func (w *pagedWorld) run(t *testing.T, o Occurrence) Run {
	t.Helper()
	must(t, w.s.dispatchOccurrence(o, time.Now()))
	_, e := w.s.dispatchNext(t.Context())
	must(t, e)
	o, e = w.s.occurrence(o.ID)
	must(t, e)
	r, e := w.s.run(o.RunID)
	must(t, e)
	return r
}

// cursor is the job's cursor of changes, and the run that wrote it.
func (w *pagedWorld) cursor(t *testing.T) (string, string) {
	t.Helper()
	held, e := w.s.store.pagingCursors(w.job.ID)
	must(t, e)
	return held["changes"].Value, held["changes"].RunID
}

// onePagedRead is an occurrence's one paged read, checked to be the session's
// calls 0 to pages-1.
func onePagedRead(t *testing.T, o Occurrence, pages int, end string) PagedRead {
	t.Helper()
	if o.State != "accepted" || o.Input == nil || len(o.Paging) != 1 {
		t.Fatalf("the occurrence is not accepted with its paged read: %s %s %+v", o.State, o.Reason, o.Paging)
	}
	read := o.Paging[0]
	if read.Source != "changes" || read.Pages != pages || read.End != end || len(read.Receipts) != pages {
		t.Fatalf("the paged read: %+v", read)
	}
	for i, c := range read.Receipts {
		if c.SessionID != read.Receipts[0].SessionID || c.CallIndex != i || c.Signature == "" {
			t.Fatalf("receipt %d is not call %d of the read's session: %+v", i, i, c)
		}
	}
	if got := len(o.Input.Source.Sources["changes"].Pages); got != pages {
		t.Fatalf("the input holds %d pages, want %d", got, pages)
	}
	return read
}

// RFC 0033 and docs/design/paging.md, sections 4.2 and 4.3: an occurrence
// fills the job's cursor into the case and reads every page in one session,
// request i formed from page i-1 by fills, each page its own receipt, and
// seals it; the cursor is written by the run that completes, which names the
// pages and their receipts, and the next occurrence reads from it.
func TestAPagedSourceIsReadInOneSessionAndItsCursorMovesWithItsRun(t *testing.T) {
	w := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 3, nil))
	o := w.prepare(t, "first")
	read := onePagedRead(t, o, 3, "no-next")
	if read.FullReRead || read.Cursor == nil || *read.Cursor != (CursorAdvance{From: "start", To: "after-start"}) {
		t.Fatalf("the read's cursor: %+v", read.Cursor)
	}
	calls := w.feed.taken()
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	for i, c := range calls {
		want := ""
		if i > 0 {
			want = "ref-" + strconv.Itoa(i)
		}
		if c.session != calls[0].session || c.since != "start" || c.cursor != want {
			t.Fatalf("call %d: %+v, want the cursor %q in the first call's session", i, c, want)
		}
	}
	if !w.feed.isSealed(calls[0].session) {
		t.Fatal("the read's session was left unsealed")
	}
	if value, _ := w.cursor(t); value != "" {
		t.Fatal("the cursor moved before a run completed:", value)
	}
	r := w.run(t, o)
	if r.State != "completed" || r.Trigger == nil || len(r.Trigger.Paging) != 1 || r.Trigger.Paging[0].Pages != 3 || len(r.Trigger.Paging[0].Receipts) != 3 {
		t.Fatalf("the run: %s %s %+v", r.State, r.Problem, r.Trigger)
	}
	if value, by := w.cursor(t); value != "after-start" || by != r.ID {
		t.Fatalf("the cursor is %q by %s, want after-start by %s", value, by, r.ID)
	}
	next := onePagedRead(t, w.prepare(t, "second"), 3, "no-next")
	if *next.Cursor != (CursorAdvance{Read: "after-start", From: "after-start", To: "after-after-start"}) {
		t.Fatalf("the next read's cursor: %+v", next.Cursor)
	}
	if calls = w.feed.taken(); len(calls) != 3 || calls[0].since != "after-start" {
		t.Fatalf("the next read was not asked from the cursor: %+v", calls)
	}
}

// Section 4.4: a page the gateway throttles was not served. Under the job's
// retry policy, the pages before it are held in their session, left open,
// and the next attempt asks the throttled page in it and no page before it;
// without a policy the occurrence fails and the session is sealed.
func TestAThrottledPageIsAskedAgainInItsSessionAndNoPageBeforeIt(t *testing.T) {
	throttle := func() func(pagedCall) int {
		once := false
		return func(c pagedCall) int {
			if c.cursor == "ref-1" && !once {
				once = true
				return 429
			}
			return 200
		}
	}
	w := newPagedWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, changeCursor, newPagedFeed(t, 3, throttle()))
	o := w.prepare(t, "first")
	if o.State != "accepted" || o.Input != nil || o.HeldPages == nil || len(o.HeldPages.Pages) != 1 || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "transient" {
		t.Fatalf("the throttled attempt: %s %s %+v %+v", o.State, o.Reason, o.HeldPages, o.Attempts)
	}
	session := o.HeldPages.Session
	if w.feed.isSealed(session) {
		t.Fatal("the session of the held pages was sealed")
	}
	*w.clock = w.clock.Add(2 * time.Second)
	o = w.step(t, o.ID)
	read := onePagedRead(t, o, 3, "no-next")
	if read.Receipts[0].SessionID != session || o.HeldPages != nil {
		t.Fatalf("the pages were not read in the held session: %+v %+v", read, o.HeldPages)
	}
	var asked []string
	for _, c := range w.feed.taken() {
		if c.session != session {
			t.Fatalf("a call outside the held session: %+v", c)
		}
		asked = append(asked, c.cursor+":"+strconv.Itoa(c.status))
	}
	if want := []string{":200", "ref-1:429", "ref-1:200", "ref-2:200"}; !slices.Equal(asked, want) {
		t.Fatalf("the pages asked: %v, want %v", asked, want)
	}
	if !w.feed.isSealed(session) {
		t.Fatal("the session was left unsealed after its last page")
	}

	once := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 3, throttle()))
	if o := once.prepare(t, "first"); o.State != "failed" || o.HeldPages != nil {
		t.Fatalf("a throttle without a policy: %s %+v", o.State, o.HeldPages)
	}
	if calls := once.feed.taken(); len(calls) != 2 || !once.feed.isSealed(calls[0].session) {
		t.Fatalf("a throttle without a policy left its session open: %+v", calls)
	}
}

// Section 4.6: a source that answers 410 source_reference_expired to a read
// from a cursor's value is read again in full from the cursor's first value,
// and the occurrence's acceptance says so, in an entry of version 6; the
// run that completes moves the cursor from the value it read. A 410 to a
// read from the first value fails the occurrence and leaves the cursor.
func TestAnExpiredReferenceIsReadAgainFromTheCursorsFirstValue(t *testing.T) {
	stale := func(w *pagedWorld) {
		must(t, w.s.store.update(func(tx runTx) error {
			_, err := tx.advancePagingCursor(PagingCursor{JobID: w.job.ID, Source: "changes", Value: "stale", RunID: "run_earlier", UpdatedAt: now()}, "")
			return err
		}))
	}
	w := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 2, func(c pagedCall) int {
		if c.since == "stale" {
			return 410
		}
		return 200
	}))
	stale(w)
	o := w.prepare(t, "first")
	read := onePagedRead(t, o, 2, "no-next")
	if !read.FullReRead || *read.Cursor != (CursorAdvance{Read: "stale", From: "start", To: "after-start"}) {
		t.Fatalf("the re-read: %+v %+v", read, read.Cursor)
	}
	if calls := w.feed.taken(); len(calls) != 3 || calls[0].since != "stale" || calls[0].status != 410 || calls[1].since != "start" || calls[1].session == calls[0].session {
		t.Fatalf("the calls: %+v", calls)
	}
	var held struct {
		Since string `json:"since"`
	}
	must(t, json.Unmarshal(o.Input.Source.Case, &held))
	if held.Since != "start" {
		t.Fatal("the retained case does not say the read started from the first value:", held.Since)
	}
	found := false
	for _, e := range occurrenceEntries(t, w.s, o.ID) {
		if len(e.FullReRead) > 0 {
			found = true
			if e.EntryVersion != pagingEntryVersion || !slices.Equal(e.FullReRead, []string{"changes"}) || e.Kind != "occurrence.ready" {
				t.Fatalf("the re-read's entry: %+v", e)
			}
		} else if e.EntryVersion == pagingEntryVersion {
			t.Fatalf("an entry of version %s without a re-read: %+v", pagingEntryVersion, e)
		}
	}
	if !found {
		t.Fatal("no entry says the source was read again")
	}
	if r := w.run(t, o); r.State != "completed" {
		t.Fatal("the run:", r.State, r.Problem)
	}
	if value, _ := w.cursor(t); value != "after-start" {
		t.Fatal("the cursor:", value)
	}

	expired := newPagedWorld(t, nil, changeCursor, newPagedFeed(t, 2, func(pagedCall) int { return 410 }))
	stale(expired)
	if o := expired.prepare(t, "first"); o.State != "failed" || len(o.Paging) != 0 {
		t.Fatalf("an expired first value: %s %+v", o.State, o.Paging)
	}
	if calls := expired.feed.taken(); len(calls) != 2 || calls[1].since != "start" {
		t.Fatalf("the first value was not asked once: %+v", calls)
	}
	if value, _ := expired.cursor(t); value != "stale" {
		t.Fatal("a failed re-read moved the cursor:", value)
	}
}

// Section 4.2, a window: the occurrence asks from the cursor to the instant
// it is prepared, and the run that completes moves the cursor to that
// instant. A read the bound ended, which left pages unread, moves no cursor.
func TestAWindowCursorReadsToTheInstantItIsPrepared(t *testing.T) {
	window := &JobPaging{Sources: []CursorRule{{Source: "changes", Kind: "window", Parameter: "from", End: "until", First: "2026-11-01T00:00:00Z"}}}
	w := newPagedWorld(t, nil, window, newPagedFeed(t, 2, nil))
	end := w.clock.UTC().Format("2006-01-02T15:04:05Z")
	o := w.prepare(t, "first")
	read := onePagedRead(t, o, 2, "no-next")
	if *read.Cursor != (CursorAdvance{From: "2026-11-01T00:00:00Z", To: end}) {
		t.Fatalf("the window: %+v", read.Cursor)
	}
	for _, c := range w.feed.taken() {
		if c.from != "2026-11-01T00:00:00Z" || c.until != end {
			t.Fatalf("a call outside the window: %+v", c)
		}
	}
	if r := w.run(t, o); r.State != "completed" {
		t.Fatal("the run:", r.State, r.Problem)
	}
	if value, _ := w.cursor(t); value != end {
		t.Fatalf("the cursor is %q, want %s", value, end)
	}

	bounded := newPagedWorld(t, nil, window, newPagedFeed(t, 10, nil))
	o = bounded.prepare(t, "first")
	if read := onePagedRead(t, o, 8, "bound"); read.Cursor == nil || read.Cursor.To != "" {
		t.Fatalf("a bound read names a next value: %+v", read.Cursor)
	}
	if r := bounded.run(t, o); r.State != "completed" {
		t.Fatal("the run:", r.State, r.Problem)
	}
	if value, _ := bounded.cursor(t); value != "" {
		t.Fatal("a bound read moved the cursor:", value)
	}
}

// Section 7, a window: a window is never asked inverted and never moves its
// cursor back. A first value in the future, or a clock behind the cursor (a
// Runner whose clock is behind the one that moved it), fails the occurrence
// before any page is asked and leaves the cursor; a cursor at the instant
// itself asks an empty window, whose run completes and moves nothing.
func TestAWindowIsNeverAskedInvertedOrMovedBack(t *testing.T) {
	window := func(first string) *JobPaging {
		return &JobPaging{Sources: []CursorRule{{Source: "changes", Kind: "window", Parameter: "from", End: "until", First: first}}}
	}
	future := newPagedWorld(t, nil, window("2099-01-01T00:00:00Z"), newPagedFeed(t, 2, nil))
	if o := future.prepare(t, "first"); o.State != "failed" || !strings.Contains(o.Reason, "2099-01-01T00:00:00Z") || len(o.Paging) != 0 {
		t.Fatalf("a first value in the future: %s %q %+v", o.State, o.Reason, o.Paging)
	}
	if calls := future.feed.taken(); len(calls) != 0 {
		t.Fatalf("an inverted window was asked: %+v", calls)
	}
	if value, _ := future.cursor(t); value != "" {
		t.Fatal("an inverted window moved the cursor:", value)
	}

	behind := newPagedWorld(t, nil, window("2026-01-01T00:00:00Z"), newPagedFeed(t, 2, nil))
	ahead := behind.clock.Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	held := func(w *pagedWorld, value string) {
		must(t, w.s.store.update(func(tx runTx) error {
			_, err := tx.advancePagingCursor(PagingCursor{JobID: w.job.ID, Source: "changes", Value: value, RunID: "run_elsewhere", UpdatedAt: now()}, "")
			return err
		}))
	}
	held(behind, ahead)
	if o := behind.prepare(t, "first"); o.State != "failed" || len(o.Paging) != 0 {
		t.Fatalf("a clock behind the cursor: %s %q %+v", o.State, o.Reason, o.Paging)
	}
	if calls := behind.feed.taken(); len(calls) != 0 {
		t.Fatalf("an inverted window was asked: %+v", calls)
	}
	if value, by := behind.cursor(t); value != ahead || by != "run_elsewhere" {
		t.Fatalf("the cursor is %q by %s, want %s left", value, by, ahead)
	}

	empty := newPagedWorld(t, nil, window("2026-01-01T00:00:00Z"), newPagedFeed(t, 1, nil))
	at := empty.clock.UTC().Format("2006-01-02T15:04:05Z")
	held(empty, at)
	o := empty.prepare(t, "first")
	if read := onePagedRead(t, o, 1, "no-next"); *read.Cursor != (CursorAdvance{Read: at, From: at}) {
		t.Fatalf("an empty window names a next value: %+v", read.Cursor)
	}
	if r := empty.run(t, o); r.State != "completed" {
		t.Fatal("the run:", r.State, r.Problem)
	}
	if value, by := empty.cursor(t); value != at || by != "run_elsewhere" {
		t.Fatalf("an empty window's run wrote the cursor: %q by %s", value, by)
	}
}

// Section 4.4, a window: an attempt that resumes held pages asks them with
// the window the first attempt asked, whatever the clock says then, so the
// throttled page is asked in the held session and no page before it.
func TestAThrottledWindowResumesTheSameWindow(t *testing.T) {
	once := false
	window := &JobPaging{Sources: []CursorRule{{Source: "changes", Kind: "window", Parameter: "from", End: "until", First: "2026-11-01T00:00:00Z"}}}
	w := newPagedWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, window, newPagedFeed(t, 3, func(c pagedCall) int {
		if c.cursor == "ref-1" && !once {
			once = true
			return 429
		}
		return 200
	}))
	end := w.clock.UTC().Format("2006-01-02T15:04:05Z")
	o := w.prepare(t, "first")
	if o.State != "accepted" || o.HeldPages == nil || len(o.HeldPages.Pages) != 1 {
		t.Fatalf("the throttled attempt: %s %s %+v", o.State, o.Reason, o.HeldPages)
	}
	session := o.HeldPages.Session
	*w.clock = w.clock.Add(2 * time.Second)
	o = w.step(t, o.ID)
	read := onePagedRead(t, o, 3, "no-next")
	if read.Receipts[0].SessionID != session || *read.Cursor != (CursorAdvance{From: "2026-11-01T00:00:00Z", To: end}) {
		t.Fatalf("the resumed window: %+v %+v", read.Receipts, read.Cursor)
	}
	var asked []string
	for _, c := range w.feed.taken() {
		if c.session != session || c.until != end {
			t.Fatalf("a call outside the held session or window: %+v, want until %s", c, end)
		}
		asked = append(asked, c.cursor+":"+strconv.Itoa(c.status))
	}
	if want := []string{":200", "ref-1:429", "ref-1:200", "ref-2:200"}; !slices.Equal(asked, want) {
		t.Fatalf("the pages asked: %v, want %v", asked, want)
	}
}

// Section 4.1: a job's paging names paged sources of its release's dialect,
// each once, with a case parameter of the type its kind fills, in its form;
// each refusal is coded, and POST /v1/jobs reads the member in its own case
// and never null.
func TestAJobsPagingIsHeldToItsReleasesDialect(t *testing.T) {
	feed := newPagedFeed(t, 1, nil)
	w := newPagedWorld(t, nil, changeCursor, feed)
	rule := func(change func(*CursorRule)) *JobPaging {
		r := CursorRule{Source: "changes", Kind: "change", Parameter: "since", Reference: "/latest", First: "start"}
		change(&r)
		return &JobPaging{Sources: []CursorRule{r}}
	}
	for code, cases := range map[string][]*JobPaging{
		"paging_source_unknown": {rule(func(r *CursorRule) { r.Source = "other" })},
		"paging_parameter_unknown": {
			rule(func(r *CursorRule) { r.Parameter = "missing" }),
			rule(func(r *CursorRule) { r.Parameter = "from" }),
			rule(func(r *CursorRule) {
				r.Kind, r.Reference, r.Parameter, r.End, r.First = "window", "", "since", "until", "2026-11-01T00:00:00Z"
			}),
		},
		"invalid_paging_rule": {
			{},
			{Sources: []CursorRule{changeCursor.Sources[0], changeCursor.Sources[0]}},
			rule(func(r *CursorRule) { r.Kind = "offset" }),
			rule(func(r *CursorRule) { r.First = "" }),
			rule(func(r *CursorRule) { r.Reference = "" }),
			rule(func(r *CursorRule) { r.End = "until" }),
			rule(func(r *CursorRule) {
				r.Kind, r.Reference, r.Parameter, r.End, r.First = "window", "", "from", "until", "yesterday"
			}),
			rule(func(r *CursorRule) {
				r.Kind, r.Reference, r.Parameter, r.End, r.First = "window", "", "from", "from", "2026-11-01T00:00:00Z"
			}),
		},
	} {
		for _, paging := range cases {
			_, err := w.s.createJobPaged(requester{}, "Refused", w.job.ReleaseID, "", nil, nil, paging)
			var refusal *apiError
			if !errors.As(err, &refusal) || refusal.Code != code {
				t.Errorf("%+v: %v, want %s", paging, err, code)
			}
		}
	}
	for _, body := range []string{`{"paging":null}`, `{"Paging":{"sources":[]}}`} {
		var req struct {
			Paging *JobPaging `json:"paging"`
		}
		json.Unmarshal([]byte(body), &req)
		if _, err := jobPaging([]byte(body), req.Paging); err != errPagingRule {
			t.Errorf("%s: %v", body, err)
		}
	}
	// A release of an earlier format names no paged source.
	plain := journalRelease(t, w.s, completingRuntime(t, w.s.cfg.Dir))
	if _, err := w.s.createJobPaged(requester{}, "Refused", plain.ID, "", nil, nil, changeCursor); err == nil || err.(*apiError).Code != "paging_source_unknown" {
		t.Fatal("a release of no paged source:", err)
	}
	if held, err := w.s.job(w.job.ID); err != nil || held.Paging == nil || !slices.Equal(held.Paging.Sources, changeCursor.Sources) {
		t.Fatalf("the job's paging was not kept: %+v %v", held.Paging, err)
	}
}
