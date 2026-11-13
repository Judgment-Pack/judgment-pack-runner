package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// retryWorld is a job with a retry policy whose schedule trigger acquires its
// input from a stub gateway, over a connection that is not durable, and an
// occurrence of it accepted with its pending input. The gateway answers its
// nth acquisition call with answer(n): a status, and a Retry-After or none.
// The mapping acquires one operation source, and then each of more, in turn.
// The Service's clock is *clock.
type retryWorld struct {
	s     *Service
	cfg   Config
	o     Occurrence
	calls *atomic.Int32
	clock *time.Time
	tr    Trigger
	job   Job
}

func newRetryWorld(t *testing.T, policy *RetryPolicy, answer func(n int) (int, string), more ...string) *retryWorld {
	t.Helper()
	return newRetryWorldOf(t, testConfig(t), testRelease, policy, answer, more...)
}

// newRetryWorldOf is newRetryWorld over cfg, with the release release makes:
// a world whose acquisitions fail needs no Runtime to evaluate (journalConfig,
// journalRelease).
func newRetryWorldOf(t *testing.T, cfg Config, release func(*testing.T, *Service) Release, policy *RetryPolicy, answer func(n int) (int, string), more ...string) *retryWorld {
	t.Helper()
	input, p, key, _ := v2Fixture(t)
	for _, name := range more {
		input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: name, Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"of":"` + name + `"}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/" + name, Source: "/" + name}}, Evidence: []EvidenceMapping{}}}})
	}
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			w.Write([]byte(`{}`))
			return
		}
		n := int(calls.Add(1))
		var req struct {
			Session   string          `json:"session"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if status, after := answer(n); status != 200 {
			// An answer "proxy:<Retry-After>" is one something in front of
			// the gateway made, not in the gateway's form.
			foreign, ok := strings.CutPrefix(after, "proxy:")
			if ok {
				after = foreign
			}
			if after != "" {
				w.Header().Set("Retry-After", after)
			}
			if ok {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(status)
				w.Write([]byte(`<html><body>Service Unavailable</body></html>`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"not now"}`))
			return
		}
		response := signResponse(t, p, key, req.Arguments, []byte(`{"id":7,"description":"fresh"}`), time.Now(), 0)
		var v map[string]any
		json.Unmarshal(response, &v)
		receipt := v["receipt"].(map[string]any)
		receipt["sessionId"] = req.Session
		signReceipt(t, receipt, key)
		json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(server.Close)
	cfg.disableAutomation = true
	cfg.InputRoot = t.TempDir()
	s, e := Open(cfg)
	must(t, e)
	t.Cleanup(func() { s.Close() })
	clock := time.Now()
	s.clock = func() time.Time { return clock }
	r := release(t, s)
	j, e := s.createJobWith(requester{}, "Retrying", r.ID, "", nil, policy)
	must(t, e)
	cfg.InputProfiles = []InputProfile{p}
	cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: server.URL}}
	s.cfg = cfg
	r.InputMapping = &input.Source.Mapping
	r.InputProfiles = []InputProfile{p}
	_, e = sqliteDB(s).Exec("UPDATE releases SET record=? WHERE id=?", string(encode(r)), r.ID)
	must(t, e)
	c := constantTrigger(time.Now())
	c.Input = &AutomaticInput{Kind: "mapped-sources", Case: input.Source.Case}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	must(t, e)
	o := newOccurrence(tr, j, time.Now())
	o.PendingInput = c.Input
	must(t, s.store.update(func(tx runTx) error { _, err := s.admitOccurrence(tx, tr, o, "fixture", ""); return err }))
	return &retryWorld{s: s, cfg: cfg, o: o, calls: calls, clock: &clock, tr: tr, job: j}
}

// admit accepts another occurrence of the world's trigger, after the first.
func (w *retryWorld) admit(t *testing.T, key string) Occurrence {
	t.Helper()
	o := newOccurrence(w.tr, w.job, time.Now())
	o.PendingInput = w.o.PendingInput
	must(t, w.s.store.update(func(tx runTx) error { _, err := w.s.admitOccurrence(tx, w.tr, o, key, ""); return err }))
	return o
}

// step lets the preparation workers take one turn, at the world's clock.
func (w *retryWorld) step(t *testing.T) Occurrence {
	t.Helper()
	must(t, w.s.prepareOccurrence(context.Background()))
	o, e := w.s.occurrence(w.o.ID)
	must(t, e)
	return o
}

func (w *retryWorld) advance(d time.Duration) { *w.clock = w.clock.Add(d) }

// occurrenceEntries are the journal entries about the world's occurrence.
func occurrenceEntries(t *testing.T, s *Service, id string) []Event {
	t.Helper()
	rows, e := sqliteDB(s).Query("SELECT entry_version,kind,detail FROM events WHERE json_extract(concerns,'$.occurrence')=? ORDER BY sequence", id)
	must(t, e)
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var detail string
		must(t, rows.Scan(&ev.EntryVersion, &ev.Kind, &detail))
		must(t, json.Unmarshal([]byte(detail), &ev.EventDetail))
		out = append(out, ev)
	}
	return out
}

// A source that throttles, then answers: the first attempt is recorded
// transient with the gateway's status and wait, the occurrence waits for the
// longer of that wait and the back-off without being attempted, and the
// second attempt, made at its time, prepares the input.
func TestAThrottledAcquisitionIsAttemptedAgainAfterItsWait(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 5, MaxBackoffSeconds: 60}, func(n int) (int, string) {
		if n == 1 {
			return 429, "30"
		}
		return 200, ""
	})
	o := w.step(t)
	if w.calls.Load() != 1 || o.State != "accepted" || o.PendingInput == nil || o.Input != nil || len(o.Attempts) != 1 {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	first := o.Attempts[0]
	if first.Number != 1 || first.DelaySeconds != 0 || first.Outcome != "transient" || first.Reason != "gateway_refused" || first.Status != 429 || first.RetryAfterSeconds != 30 || first.StartedAt == "" || first.EndedAt == "" {
		t.Fatalf("%+v", first)
	}
	// The source's 30 seconds, longer than the 5 of the back-off.
	firstNext := o.NextAttemptAt
	if want := stamp(w.clock.Add(30 * time.Second)); firstNext != want {
		t.Fatalf("next attempt at %s, want %s", o.NextAttemptAt, want)
	}
	w.advance(29 * time.Second)
	if o = w.step(t); w.calls.Load() != 1 || o.State != "accepted" || len(o.Attempts) != 1 {
		t.Fatalf("attempted before its time: %d calls, %+v", w.calls.Load(), o)
	}
	w.advance(time.Second)
	o = w.step(t)
	if w.calls.Load() != 2 || o.State != "accepted" || o.Input == nil || o.PendingInput != nil || o.NextAttemptAt != "" || len(o.Attempts) != 2 {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	if second := o.Attempts[1]; second.Number != 2 || second.DelaySeconds != 30 || second.Outcome != "succeeded" || second.Status != 0 {
		t.Fatalf("%+v", second)
	}
	if o = w.step(t); w.calls.Load() != 2 {
		t.Fatal("attempted again after it succeeded")
	}
	// The journal: each attempt's end, with the attempt, at version 4.
	var kinds []string
	for _, ev := range occurrenceEntries(t, w.s, o.ID) {
		kinds = append(kinds, ev.Kind)
		if ev.EntryVersion != wantEntryVersion(ev) {
			t.Errorf("%s at version %s holds %+v", ev.Kind, ev.EntryVersion, ev.Attempt)
		}
		if ev.Kind == "occurrence.retry-scheduled" && (ev.Attempt.Number != 1 || ev.Attempt.Outcome != "transient" || ev.NextAt != firstNext) {
			t.Errorf("retry-scheduled detail %+v", ev.EventDetail)
		}
	}
	if got := strings.Join(kinds, " "); got != "occurrence.received occurrence.preparing occurrence.retry-scheduled occurrence.preparing occurrence.ready" {
		t.Fatal(got)
	}
}

// A source that never answers: each attempt waits for the back-off, which
// doubles to its bound, and the last the policy allows ends the occurrence
// failed with the limit as its reason. It is never attempted again, and no
// run is queued.
func TestAnAcquisitionThatNeverSucceedsEndsAtItsLimit(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 2, MaxBackoffSeconds: 3}, func(int) (int, string) { return 503, "" })
	o := w.step(t)
	if o.NextAttemptAt != stamp(w.clock.Add(2*time.Second)) {
		t.Fatalf("%+v", o)
	}
	w.advance(2 * time.Second)
	if o = w.step(t); o.NextAttemptAt != stamp(w.clock.Add(3*time.Second)) || o.Attempts[1].DelaySeconds != 2 {
		t.Fatalf("the back-off is not bounded: %+v", o)
	}
	w.advance(3 * time.Second)
	o = w.step(t)
	if w.calls.Load() != 3 || o.State != "failed" || o.Reason != occurrenceAttemptLimit || o.PendingInput != nil || o.NextAttemptAt != "" || len(o.Attempts) != 3 {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	for i, a := range o.Attempts {
		if a.Number != i+1 || a.Outcome != "transient" || a.Status != 503 {
			t.Fatalf("%+v", a)
		}
	}
	w.advance(time.Hour)
	if w.step(t); w.calls.Load() != 3 || countRows(t, w.s, "runs") != 0 {
		t.Fatal("attempted after its limit")
	}
}

// A wait the cap does not allow fails the occurrence rather than attempt it
// sooner than the source asked; so does a wait that would end after the
// occurrence's deadline. A Retry-After that reads as neither form is ignored.
func TestARetryWaitPastTheCapOrTheDeadlineFails(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(n int) (int, string) {
		if n == 1 {
			return 503, "soon"
		}
		return 429, "3601"
	})
	o := w.step(t)
	if o.State != "accepted" || o.NextAttemptAt != stamp(w.clock.Add(time.Second)) || o.Attempts[0].RetryAfterSeconds != 0 {
		t.Fatalf("a malformed Retry-After was not ignored: %+v", o)
	}
	w.advance(time.Second)
	if o = w.step(t); o.State != "failed" || o.Reason != occurrenceWaitTooLong || o.Attempts[1].RetryAfterSeconds != 3601 {
		t.Fatalf("%+v", o)
	}

	// The cap holds on its own, before a deadline far past it.
	far := Occurrence{Attempts: []Attempt{{Number: 1}}}
	if again, reason := closeAttempt(&far, RetryPolicy{MaxAttempts: 2, BackoffSeconds: 1, MaxBackoffSeconds: 1}, &gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 429, retryAfter: "3601"}, time.Now().Add(24*time.Hour), time.Now(), false); again || reason != occurrenceWaitTooLong {
		t.Fatalf("a wait past the cap: %v %q", again, reason)
	}
	if again, _ := closeAttempt(&far, RetryPolicy{MaxAttempts: 2, BackoffSeconds: 1, MaxBackoffSeconds: 1}, &gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 429, retryAfter: "3600"}, time.Now().Add(24*time.Hour), time.Now(), false); !again {
		t.Fatal("a wait at the cap is not taken")
	}

	expiry := time.Now().Add(10 * time.Second)
	held := Occurrence{Attempts: []Attempt{{Number: 1}}}
	if again, reason := closeAttempt(&held, RetryPolicy{MaxAttempts: 2, BackoffSeconds: 10, MaxBackoffSeconds: 10}, &gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 503}, expiry, expiry.Add(-10*time.Second), false); again || reason != occurrenceWaitTooLong {
		t.Fatalf("a wait to the deadline: %v %q", again, reason)
	}
}

// A failure off the closed list is not retried: it is recorded failed, and
// ends the occurrence with the sentence it ended with before. So is a
// failure after an earlier call of the attempt answered.
func TestAFailureOffTheListIsNotRetried(t *testing.T) {
	for _, status := range []int{400, 401, 404, 500, 502, 504} {
		w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(int) (int, string) { return status, "1" })
		o := w.step(t)
		if o.State != "failed" || !strings.Contains(o.Reason, "The Gateway refused the operation") || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "failed" || o.Attempts[0].Status != status || o.NextAttemptAt != "" {
			t.Fatalf("%d: %+v", status, o)
		}
	}
	for _, failure := range []error{
		bad("gateway_unavailable", "Gateway acquisition failed or timed out. It was not retried."),
		&gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 503, afterAcquisition: true},
	} {
		if _, retried := transient(failure); retried {
			t.Fatalf("%v is retried", failure)
		}
	}
	if _, retried := transient(&gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 503}); !retried {
		t.Fatal("a 503 is not retried")
	}
	// Nor is a 429 or a 503 not in the gateway's own form, nor one whose
	// form is near it.
	for _, answer := range []struct{ contentType, body string }{
		{"text/html", `<html>Service Unavailable</html>`},
		{"text/plain", `{"error":"not now"}`},
		{"application/json", `{"message":"not now"}`},
		{"application/json", `{"error":{"code":1}}`},
		{"application/json", `["error"]`},
		{"", `{"error":"not now"}`},
	} {
		if gatewayRefusal(answer.contentType, []byte(answer.body)) {
			t.Fatalf("%+v is the gateway's own", answer)
		}
	}
	if !gatewayRefusal("application/json; charset=utf-8", []byte(`{"error":"not now","refusal":"busy"}`)) {
		t.Fatal("the gateway's own refusal is not read as its own")
	}
	// Nor is an attempt cut short by its deadline or a stop, whatever it met.
	cut := Occurrence{Attempts: []Attempt{{Number: 1}}}
	if again, reason := closeAttempt(&cut, RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, &gatewayAnswer{err: bad("gateway_refused", "refused"), own: true, status: 503}, time.Now().Add(time.Hour), time.Now(), true); again || reason != "" || cut.Attempts[0].Outcome != "failed" {
		t.Fatalf("a cut attempt: %v %q %+v", again, reason, cut.Attempts[0])
	}
}

// An occurrence waiting for its next attempt is passed by: one admitted after
// it is prepared in its turn, and is not held up by the wait.
func TestAWaitingOccurrenceHoldsNoOtherUp(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 60, MaxBackoffSeconds: 60}, func(n int) (int, string) {
		if n == 1 {
			return 429, ""
		}
		return 200, ""
	})
	if o := w.step(t); o.State != "accepted" || o.NextAttemptAt == "" {
		t.Fatalf("%+v", o)
	}
	// The legacy path holds the wait itself, whoever hands it the occurrence.
	must(t, w.s.prepareLegacyOccurrence(context.Background(), w.o.ID))
	if w.calls.Load() != 1 {
		t.Fatal("an occurrence was attempted before its time:", w.calls.Load())
	}
	second := w.admit(t, "fixture-second")
	w.step(t)
	o, e := w.s.occurrence(second.ID)
	must(t, e)
	if w.calls.Load() != 2 || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "succeeded" {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	if first, e := w.s.occurrence(w.o.ID); e != nil || len(first.Attempts) != 1 || first.NextAttemptAt == "" {
		t.Fatalf("%+v %v", first, e)
	}
}

// A throttled call after an earlier call of the same attempt answered is not
// retried: attempting again would call the first call's source again.
func TestAThrottledCallAfterAnAnsweredOneIsNotRetried(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(n int) (int, string) {
		if n == 1 {
			return 200, ""
		}
		return 429, "1"
	}, "detail")
	o := w.step(t)
	if w.calls.Load() != 2 || o.State != "failed" || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "failed" || o.Attempts[0].Status != 429 || o.NextAttemptAt != "" {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	w.advance(time.Minute)
	if o = w.step(t); w.calls.Load() != 2 || o.State != "failed" {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
}

// A 503 that something in front of the gateway answered, not in the
// gateway's own form, may follow a call the gateway forwarded: it is not
// retried.
func TestA503NotInTheGatewaysFormIsNotRetried(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(int) (int, string) { return 503, "proxy:1" })
	o := w.step(t)
	if w.calls.Load() != 1 || o.State != "failed" || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "failed" || o.Attempts[0].Status != 503 || o.NextAttemptAt != "" {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	w.advance(time.Minute)
	if o = w.step(t); w.calls.Load() != 1 {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
}

// Without a policy, a throttled acquisition fails once, as it did, and the
// occurrence's record holds nothing it did not.
func TestWithoutAPolicyAThrottledAcquisitionFailsAsBefore(t *testing.T) {
	w := newRetryWorld(t, nil, func(int) (int, string) { return 429, "1" })
	o := w.step(t)
	if w.calls.Load() != 1 || o.State != "failed" || o.Reason != "The Gateway refused the operation. Check the installed connection and provider access." {
		t.Fatalf("%+v", o)
	}
	var raw string
	must(t, sqliteDB(w.s).QueryRow("SELECT record FROM occurrences WHERE id=?", o.ID).Scan(&raw))
	if strings.Contains(raw, `"attempts"`) || strings.Contains(raw, `"nextAttemptAt"`) {
		t.Fatal(raw)
	}
	w.advance(time.Hour)
	if w.step(t); w.calls.Load() != 1 {
		t.Fatal("attempted again")
	}
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != storeSchema {
		t.Fatalf("schema %s", schema)
	}
}

// The wait survives a restart: the next attempt's time is read from the
// record, and the attempt is made once, no earlier.
func TestARetryWaitSurvivesARestart(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 2, BackoffSeconds: 60, MaxBackoffSeconds: 60}, func(n int) (int, string) {
		if n == 1 {
			return 503, ""
		}
		return 200, ""
	})
	o := w.step(t)
	if o.NextAttemptAt == "" {
		t.Fatalf("%+v", o)
	}
	w.s.Close()
	base := w.cfg
	base.InputProfiles, base.GatewayConnections = nil, nil
	s, e := Open(base)
	must(t, e)
	defer s.Close()
	s.cfg = w.cfg
	s.clock = func() time.Time { return *w.clock }
	w.s = s
	if o = w.step(t); o.State != "accepted" || w.calls.Load() != 1 || o.NextAttemptAt == "" {
		t.Fatalf("after a restart: %d calls, %+v", w.calls.Load(), o)
	}
	w.advance(time.Minute)
	o = w.step(t)
	w.step(t)
	if w.calls.Load() != 2 || o.Input == nil || len(o.Attempts) != 2 || o.Attempts[1].Outcome != "succeeded" {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
}

// An attempt in flight at a crash is recovered as today, failed and not
// repeated, and its record is closed interrupted.
func TestAnAttemptInFlightAtACrashIsNotRetried(t *testing.T) {
	w := newRetryWorld(t, &RetryPolicy{MaxAttempts: 3, BackoffSeconds: 1, MaxBackoffSeconds: 1}, func(int) (int, string) { return 200, "" })
	o := w.o
	o.State = "preparing"
	openAttempt(&o, time.Now())
	must(t, w.s.saveOccurrence(o))
	w.s.Close()
	base := w.cfg
	base.InputProfiles, base.GatewayConnections = nil, nil
	s, e := Open(base)
	must(t, e)
	defer s.Close()
	s.cfg = w.cfg
	w.s = s
	o = w.step(t)
	if o.State != "failed" || !strings.Contains(o.Reason, "not repeated") || len(o.Attempts) != 1 || o.Attempts[0].Outcome != "interrupted" || w.calls.Load() != 0 {
		t.Fatalf("%d calls: %+v", w.calls.Load(), o)
	}
	entries := occurrenceEntries(t, s, o.ID)
	if last := entries[len(entries)-1]; last.Kind != "occurrence.failed" || last.Attempt == nil || last.Attempt.Outcome != "interrupted" || last.ErrorClass != classInterrupted || last.EntryVersion != resultEntryVersion {
		t.Fatalf("%+v", last)
	}
}

func TestRetryAfterReadsBothForms(t *testing.T) {
	at := time.Date(2026, 10, 21, 7, 28, 0, 0, time.UTC)
	for _, c := range []struct {
		value string
		wait  time.Duration
		read  bool
	}{
		{"120", 120 * time.Second, true},
		{" 0 ", 0, true},
		{"Wed, 21 Oct 2026 07:29:30 GMT", 90 * time.Second, true},
		{"Wednesday, 21-Oct-26 07:29:30 GMT", 90 * time.Second, true},
		{"Wed Oct 21 07:29:30 2026", 90 * time.Second, true},
		{"Wed, 21 Oct 2026 07:27:00 GMT", 0, false},
		{"Fri, 31 Dec 9999 23:59:59 GMT", retryWaitCap + time.Second, true},
		{"", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"12abc", 0, false},
		{"tomorrow", 0, false},
		{"99999999999999999999", retryWaitCap + time.Second, true},
	} {
		wait, read := retryAfter(c.value, at)
		if wait != c.wait || read != c.read {
			t.Errorf("%q: %v %v", c.value, wait, read)
		}
	}
	p := RetryPolicy{MaxAttempts: 10, BackoffSeconds: 3, MaxBackoffSeconds: 20}
	var waits []string
	for made := 1; made <= 5; made++ {
		waits = append(waits, p.backoff(made).String())
	}
	if got := strings.Join(waits, " "); got != "3s 6s 12s 20s 20s" {
		t.Fatal(got)
	}
}

// A policy is taken in form and bounds, as written, or refused; it is part of
// a repeated creation's replay; and it raises the store's schema.
func TestAJobIsCreatedWithARetryPolicyInForm(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	runtime := completingRuntime(t, cfg.Dir)
	create := func(release, retry string) *httptest.ResponseRecorder {
		body := `{"name":"Retrying","releaseId":"` + release + `","reviewed":true` + retry + `}`
		return serve(t, s, "POST", "/v1/jobs", body, nil)
	}
	release := journalRelease(t, s, runtime).ID
	for _, retry := range []string{
		`,"retry":null`,
		`,"retry":{}`,
		`,"retry":{"maxAttempts":0,"backoffSeconds":1,"maxBackoffSeconds":1}`,
		`,"retry":{"maxAttempts":11,"backoffSeconds":1,"maxBackoffSeconds":1}`,
		`,"retry":{"maxAttempts":2,"backoffSeconds":0,"maxBackoffSeconds":1}`,
		`,"retry":{"maxAttempts":2,"backoffSeconds":5,"maxBackoffSeconds":4}`,
		`,"retry":{"maxAttempts":2,"backoffSeconds":5,"maxBackoffSeconds":3601}`,
		`,"retry":{"maxAttempts":2,"backoffSeconds":5}`,
		`,"Retry":{"maxAttempts":2,"backoffSeconds":5,"maxBackoffSeconds":5}`,
		`,"retry":{"MaxAttempts":2,"backoffSeconds":5,"maxBackoffSeconds":5}`,
	} {
		if got := create(release, retry); got.Code != 422 || !strings.Contains(got.Body.String(), "invalid_retry_policy") {
			t.Fatalf("%s: %d %s", retry, got.Code, got.Body)
		}
	}
	if got := create(release, `,"retry":{"maxAttempts":2,"backoffSeconds":5,"maxBackoffSeconds":5,"jitter":1}`); got.Code != 400 {
		t.Fatalf("an unknown member: %d %s", got.Code, got.Body)
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("refused policies raised the schema to %s", schema)
	}
	// One attempt is the default: the job is made as without a policy.
	if got := create(journalRelease(t, s, runtime).ID, `,"retry":{"maxAttempts":1,"backoffSeconds":5,"maxBackoffSeconds":60}`); got.Code != 201 || strings.Contains(got.Body.String(), `"retry"`) {
		t.Fatalf("the default policy: %d %s", got.Code, got.Body)
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("the default policy raised the schema to %s", schema)
	}
	policy := `,"retry":{"maxAttempts":3,"backoffSeconds":5,"maxBackoffSeconds":60}`
	made := create(release, policy)
	if made.Code != 201 || !strings.Contains(made.Body.String(), `"retry":{"maxAttempts":3,"backoffSeconds":5,"maxBackoffSeconds":60}`) {
		t.Fatalf("%d %s", made.Code, made.Body)
	}
	if again := create(release, policy); again.Code != 201 || again.Body.String() != made.Body.String() {
		t.Fatalf("a repeated creation: %d %s", again.Code, again.Body)
	}
	for _, other := range []string{"", `,"retry":{"maxAttempts":4,"backoffSeconds":5,"maxBackoffSeconds":60}`} {
		if got := create(release, other); got.Code != 409 || !strings.Contains(got.Body.String(), "release_already_used") {
			t.Fatalf("%q: %d %s", other, got.Code, got.Body)
		}
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != retryStoreSchema {
		t.Fatalf("schema %s", schema)
	}
	// A store at 6 takes a scoped job, and stays at 6.
	_, e := s.createJobBy(requester{}, "Scoped", journalRelease(t, s, runtime).ID, "scope-a", nil)
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != retryStoreSchema {
		t.Fatalf("schema %s", schema)
	}
	s.Close()
	s = openJournal(t, cfg)
	var held Job
	must(t, json.Unmarshal(made.Body.Bytes(), &held))
	j, e := s.job(held.ID)
	must(t, e)
	if j.Retry == nil || *j.Retry != (RetryPolicy{3, 5, 60}) {
		t.Fatalf("%+v", j)
	}
	if status := serve(t, s, "GET", "/v1/status", "", nil); !strings.Contains(status.Body.String(), `"retries"`) {
		t.Fatal(status.Body)
	}
	// An upgrade names no policy and keeps the job's.
	upgrade := UpgradeJobRequest{ReleaseID: journalRelease(t, s, runtime).ID, BaseReleaseID: j.ReleaseID, BaseRevision: j.Revision, Reviewed: true}
	if got := serve(t, s, "POST", "/v1/jobs/"+j.ID+"/upgrade", string(encode(upgrade)), nil); got.Code != 200 {
		t.Fatalf("the upgrade: %d %s", got.Code, got.Body)
	}
	upgraded, e := s.job(j.ID)
	must(t, e)
	if upgraded.Revision != 2 || upgraded.Retry == nil || *upgraded.Retry != *j.Retry {
		t.Fatalf("%+v", upgraded)
	}
	var revisions, carrying int
	must(t, sqliteDB(s).QueryRow("SELECT count(*), count(CASE WHEN json_extract(record,'$.retry.maxAttempts')=3 THEN 1 END) FROM job_revisions WHERE job_id=?", j.ID).Scan(&revisions, &carrying))
	if revisions == 0 || carrying != revisions {
		t.Fatalf("revisions %d, carrying the policy %d", revisions, carrying)
	}
}

// A store at the retry schema is relocated, and keeps its job's policy and
// its schema.
func TestARetryStoreIsRelocated(t *testing.T) {
	cfg := journalConfig(t)
	cfg.InputRoot = relocationTempDir(t)
	s, e := Open(cfg)
	must(t, e)
	j, e := s.createJobWith(requester{}, "Retrying", journalRelease(t, s, s.runtimeDigest).ID, "", nil, &RetryPolicy{2, 5, 5})
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != retryStoreSchema {
		t.Fatalf("schema %s", schema)
	}
	s.Close()
	next := filepath.Join(relocationTempDir(t), "moved-project")
	must(t, os.Rename(cfg.InputRoot, next))
	request := RelocateRequest{Version: 1, ID: strings.Repeat("c", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: next}
	if result, e := RelocateWorkspace(request); e != nil || result.AlreadyApplied {
		t.Fatal(result, e)
	}
	cfg.InputRoot = next
	s, e = Open(cfg)
	must(t, e)
	defer s.Close()
	held, e := s.job(j.ID)
	must(t, e)
	if held.Retry == nil || *held.Retry != (RetryPolicy{2, 5, 5}) || storeSchemaOf(t, sqliteDB(s)) != retryStoreSchema {
		t.Fatalf("%+v under schema %s", held, storeSchemaOf(t, sqliteDB(s)))
	}
}

// interruptedRun is a run of a job that its first attempt left interrupted:
// stopped, as Runner saw it at an orderly stop, or found running at a
// restart.
func interruptedRun(t *testing.T, scope string, stopped bool) (*Service, Config, Run) {
	t.Helper()
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	runtime := completingRuntime(t, cfg.Dir)
	j, e := s.createJobBy(requester{}, "Retried", journalRelease(t, s, runtime).ID, scope, nil)
	must(t, e)
	r, _, e := s.submit(j.ID, "first", journalInput())
	must(t, e)
	if stopped {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, e = s.dispatchNext(ctx)
		must(t, e)
	} else {
		r.State, r.StartedAt, r.Attempt = "running", now(), 1
		must(t, s.changeRun(r))
		s.Close()
		s = openJournal(t, cfg)
	}
	t.Cleanup(func() { s.Close() })
	r, e = s.run(r.ID)
	must(t, e)
	if r.State != "interrupted" || r.Attempt != 1 {
		t.Fatalf("%+v", r)
	}
	return s, cfg, r
}

// An interrupted run is retried as the same run: its first attempt's record
// is kept, a repeated request changes nothing, its next attempt evaluates
// into its own directory, and the first attempt's is kept.
func TestAnInterruptedRunIsRetriedKeepingItsFirstAttempt(t *testing.T) {
	for _, stopped := range []bool{true, false} {
		s, cfg, r := interruptedRun(t, "", stopped)
		before := journalLength(t, s)
		retry := func(attempt string) *httptest.ResponseRecorder {
			return serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":`+attempt+`}`, nil)
		}
		got := retry("1")
		var served struct {
			State    string    `json:"state"`
			Attempt  int       `json:"attempt"`
			Attempts []Attempt `json:"attempts"`
		}
		must(t, json.Unmarshal(got.Body.Bytes(), &served))
		reason, ended := runAttemptRestarted, ""
		if stopped {
			reason, ended = runAttemptStopped, r.InterruptedAt
		}
		if got.Code != 202 || served.State != "queued" || served.Attempt != 1 || len(served.Attempts) != 1 {
			t.Fatalf("%d %s", got.Code, got.Body)
		}
		if a := served.Attempts[0]; a != (Attempt{Number: 1, StartedAt: r.StartedAt, EndedAt: ended, Outcome: "interrupted", Reason: reason}) {
			t.Fatalf("%+v", a)
		}
		if again := retry("1"); again.Code != 200 || again.Body.String() != got.Body.String() {
			t.Fatalf("a repeated retry: %d %s", again.Code, again.Body)
		}
		if other := retry("2"); other.Code != 409 || !strings.Contains(other.Body.String(), "attempt_mismatch") {
			t.Fatalf("%d %s", other.Code, other.Body)
		}
		if journalLength(t, s) != before+2 {
			t.Fatalf("%d entries after %d", journalLength(t, s), before)
		}
		var version, kind, from, by, detail string
		must(t, sqliteDB(s).QueryRow("SELECT entry_version,kind,from_state,by_actor,detail FROM events WHERE json_extract(concerns,'$.run')=? ORDER BY sequence DESC LIMIT 1 OFFSET 1", r.ID).Scan(&version, &kind, &from, &by, &detail))
		if version != attemptEntryVersion || kind != "run.queued" || from != "interrupted" || !strings.Contains(by, `"installation"`) || !strings.Contains(detail, `"attempt":{"number":1,`) {
			t.Fatal(version, kind, from, by, detail)
		}
		if schema := storeSchemaOf(t, sqliteDB(s)); schema != retryStoreSchema {
			t.Fatalf("schema %s", schema)
		}
		// The original request's key returns the run as it now is.
		replay, replayed, e := s.submit(r.JobID, "first", journalInput())
		if e != nil || !replayed || replay.ID != r.ID || replay.State != "queued" || len(replay.Attempts) != 1 {
			t.Fatalf("%+v %v %v", replay, replayed, e)
		}
		_, e = s.dispatchNext(context.Background())
		must(t, e)
		done, e := s.run(r.ID)
		must(t, e)
		if done.State != "completed" || done.Attempt != 2 || len(done.Attempts) != 1 {
			t.Fatalf("%+v", done)
		}
		if _, e = os.Stat(filepath.Join(cfg.Dir, "attempts", r.ID+".2")); e != nil {
			t.Fatal(e)
		}
		if stopped {
			if _, e = os.Stat(filepath.Join(cfg.Dir, "attempts", r.ID)); e != nil {
				t.Fatal("the first attempt's directory: ", e)
			}
		}
		if diagnostics := serve(t, s, "GET", "/v1/runs/"+r.ID+"/diagnostics", "", nil); diagnostics.Code != 200 {
			t.Fatalf("%d %s", diagnostics.Code, diagnostics.Body)
		}
		if again := retry("2"); again.Code != 409 || !strings.Contains(again.Body.String(), "run_not_retryable") {
			t.Fatalf("a completed run: %d %s", again.Code, again.Body)
		}
	}
}

// The event an interrupted trigger run came from, delivered again after the
// run is retried, replays its occurrence and queues no second run.
func TestAnEventDeliveredAgainAfterARetryQueuesNothing(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	runtime := completingRuntime(t, cfg.Dir)
	j, e := s.createJob("Delivered", journalRelease(t, s, runtime).ID)
	must(t, e)
	tr, e := s.configureTrigger(j.ID, "", 0, eventTriggerConfig())
	must(t, e)
	tr, key, e := s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, e)
	event := delivery("first-event")
	o, _, e := s.event(tr.ID, key, event)
	must(t, e)
	must(t, s.dispatchOccurrence(storedOccurrence(t, s, o.ID), time.Now()))
	o, e = s.occurrence(o.ID)
	must(t, e)
	if o.State != "submitted" || o.RunID == "" {
		t.Fatalf("%+v", o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e = s.dispatchNext(ctx)
	must(t, e)
	if got := serve(t, s, "POST", "/v1/runs/"+o.RunID+"/retry", `{"attempt":1}`, nil); got.Code != 202 {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	runs := func() (n int) {
		must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM runs").Scan(&n))
		return n
	}
	for _, dispatch := range []bool{false, true} {
		if dispatch {
			_, e = s.dispatchNext(context.Background())
			must(t, e)
		}
		again, replay, e := s.event(tr.ID, key, event)
		if e != nil || !replay || again.ID != o.ID || again.RunID != o.RunID || runs() != 1 {
			t.Fatalf("%+v %v %v, runs %d", again, replay, e, runs())
		}
	}
	if r, e := s.run(o.RunID); e != nil || r.State != "completed" || r.Attempt != 2 || len(r.Attempts) != 1 {
		t.Fatalf("%+v %v", r, e)
	}
}

// A run in any state but interrupted is not retried, nor one whose record
// holds an audit record, nor one at its last attempt; each refusal is
// journaled and changes nothing.
func TestARetryIsRefusedForARunNotInterrupted(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	runtime := completingRuntime(t, cfg.Dir)
	j, e := s.createJob("Retried", journalRelease(t, s, runtime).ID)
	must(t, e)
	r, _, e := s.submit(j.ID, "first", journalInput())
	must(t, e)
	refused := func(run Run, attempt, code string) {
		t.Helper()
		before := journalLength(t, s)
		got := serve(t, s, "POST", "/v1/runs/"+run.ID+"/retry", `{"attempt":`+attempt+`}`, nil)
		if got.Code != 409 || !strings.Contains(got.Body.String(), code) {
			t.Fatalf("%s: %d %s", run.State, got.Code, got.Body)
		}
		// Refusals of one run with one code in a minute are one entry.
		var n int
		must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM events WHERE kind='admission.refused' AND json_extract(detail,'$.request')='retry-run' AND json_extract(detail,'$.code')=? AND json_extract(concerns,'$.run')=?", code, run.ID).Scan(&n))
		if n != 1 || journalLength(t, s) > before+1 {
			t.Fatalf("the refusal was journaled %d times", n)
		}
		held, e := s.run(run.ID)
		must(t, e)
		if runText(held) != runText(run) {
			t.Fatal("a refused retry changed the run")
		}
	}
	refused(r, "0", "run_not_retryable")
	// Another run of the job, refused the same way within the minute, has
	// its own entry: the refusal names its run.
	other, _, e := s.submit(j.ID, "second", journalInput())
	must(t, e)
	refused(other, "0", "run_not_retryable")
	_, e = s.dispatchNext(context.Background())
	must(t, e)
	r, e = s.run(r.ID)
	must(t, e)
	if r.State != "completed" {
		t.Fatal(r.State)
	}
	refused(r, "1", "run_not_retryable")
	for _, held := range []func(*Run){
		func(r *Run) { r.State = "failed" },
		func(r *Run) { r.State, r.Audit, r.AuditBytes = "interrupted", nil, []byte(`{"audit":1}`) },
		func(r *Run) { r.State, r.Audit, r.AuditBytes, r.Attempt = "interrupted", nil, nil, maxRetryAttempts },
		func(r *Run) {
			r.State, r.Audit, r.AuditBytes, r.Attempt = "interrupted", nil, nil, 1
			r.Trigger = &TriggerOrigin{OccurrenceID: "occ_x", TriggerID: "trg_x", TriggerRevision: 1, Kind: "event", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
		},
	} {
		next := r
		held(&next)
		must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(next, anyState); return e }))
		refused(next, decimalRevision(next.Attempt), "run_not_retryable")
	}
	// A malformed request, empty, missing its attempt or oversized, is
	// refused and journaled as a refusal of the run it names.
	oversized := `{"attempt":1,"pad":"` + strings.Repeat("a", MaxBody) + `"}`
	for _, body := range []string{``, `{}`, `{"attempt":-1}`, `{"attempt":"1"}`, `{"attempt":1,"attempt":1}`, oversized} {
		if got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", body, nil); got.Code != 400 {
			t.Fatalf("%.40s: %d %s", body, got.Code, got.Body)
		}
	}
	var malformed int
	must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM events WHERE kind='admission.refused' AND json_extract(detail,'$.request')='retry-run' AND json_extract(detail,'$.code')='invalid_request' AND json_extract(concerns,'$.run')=?", r.ID).Scan(&malformed))
	if malformed != 1 {
		t.Fatal("malformed retries journaled:", malformed)
	}
	if got := serve(t, s, "POST", "/v1/runs/run_unheld/retry", `{"attempt":1}`, nil); got.Code != 404 {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
}

// refusedAsAudited asserts that a retry of r is refused run_not_retryable,
// journaled once for the run, and changes nothing.
func refusedAsAudited(t *testing.T, s *Service, r Run, attempt string) {
	t.Helper()
	got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":`+attempt+`}`, nil)
	if got.Code != 409 || !strings.Contains(got.Body.String(), "run_not_retryable") || !strings.Contains(got.Body.String(), "audit record") {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	var n int
	must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM events WHERE kind='admission.refused' AND json_extract(detail,'$.request')='retry-run' AND json_extract(detail,'$.code')='run_not_retryable' AND json_extract(concerns,'$.run')=?", r.ID).Scan(&n))
	if n != 1 {
		t.Fatal("refusals journaled:", n)
	}
	held, e := s.run(r.ID)
	must(t, e)
	if runText(held) != runText(r) {
		t.Fatal("a refused retry changed the run")
	}
}

// A run found running at a restart has no audit record in its record, but
// its attempt's Runtime may have written one: a trail with any content in
// any of its attempts' directories, or a directory whose state cannot be
// read, refuses the retry. An empty trail, or none, does not.
func TestARetryIsRefusedWhenAnAttemptLeftAnAuditTrail(t *testing.T) {
	s, cfg, r := interruptedRun(t, "", false)
	audit := filepath.Join(cfg.Dir, "attempts", r.ID, "audit")
	must(t, os.MkdirAll(audit, 0700))
	trail := filepath.Join(audit, "evaluations.jsonl")
	must(t, os.WriteFile(trail, []byte(`{"decision":"x"}`+"\n"), 0600))
	refusedAsAudited(t, s, r, "1")
	// A later attempt's retry reads the earlier attempts' directories too.
	later := r
	later.Attempt, later.Attempts = 2, []Attempt{{Number: 1, Outcome: "interrupted", Reason: runAttemptRestarted}}
	must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(later, anyState); return e }))
	refusedAsAudited(t, s, later, "2")
	must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(r, anyState); return e }))
	// A directory whose state cannot be read is taken to hold one.
	must(t, os.RemoveAll(audit))
	must(t, os.WriteFile(audit, []byte("not a directory"), 0600))
	refusedAsAudited(t, s, r, "1")
	must(t, os.Remove(audit))
	must(t, os.MkdirAll(audit, 0700))
	must(t, os.WriteFile(trail, nil, 0600))
	if got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":1}`, nil); got.Code != 202 {
		t.Fatalf("an empty trail: %d %s", got.Code, got.Body)
	}
}

// An evaluation that completed while its end was not recorded (a failed
// finish stops the dispatcher, and the run reads interrupted at the next
// start) left its audit trail: the run is not evaluated again.
func TestARetryIsRefusedAfterAFinishThatFailed(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	runtime := completingRuntime(t, cfg.Dir)
	j, e := s.createJob("Finished", journalRelease(t, s, runtime).ID)
	must(t, e)
	r, _, e := s.submit(j.ID, "first", journalInput())
	must(t, e)
	_, e = s.dispatchNext(context.Background())
	must(t, e)
	done, e := s.run(r.ID)
	must(t, e)
	if done.State != "completed" {
		t.Fatalf("%+v", done)
	}
	// The record as a failed finish leaves it: running, without its audit
	// record or its entry in the chain of runs.
	left := done
	left.State, left.FinishedAt, left.Audit, left.AuditBytes, left.AuditSignatures = "running", "", nil, nil, nil
	must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(left, anyState); return e }))
	_, e = sqliteDB(s).Exec("DELETE FROM run_chain WHERE run=?", r.ID)
	must(t, e)
	s.Close()
	s = openJournal(t, cfg)
	defer s.Close()
	interrupted, e := s.run(r.ID)
	must(t, e)
	if interrupted.State != "interrupted" || len(interrupted.Audit) > 0 || len(interrupted.AuditBytes) > 0 {
		t.Fatalf("%+v", interrupted)
	}
	if _, e = os.Stat(filepath.Join(cfg.Dir, "attempts", r.ID, "audit", "evaluations.jsonl")); e != nil {
		t.Fatal("the first attempt's trail: ", e)
	}
	refusedAsAudited(t, s, interrupted, "1")
	if _, e = s.dispatchNext(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(cfg.Dir, "attempts", r.ID+".2")); !os.IsNotExist(e) {
		t.Fatal("a second attempt was evaluated:", e)
	}
}

// The route names a run under its scope: a request under another scope, or
// under none, is answered as for a run the store does not hold, and changes
// and journals nothing.
func TestARetryNamesTheRunUnderItsScope(t *testing.T) {
	s, _, r := interruptedRun(t, "scope-a", true)
	unheld := serve(t, s, "POST", "/v1/runs/run_unheld/retry", `{"attempt":1}`, nil)
	before := journalLength(t, s)
	for _, path := range []string{"/v1/runs/" + r.ID + "/retry", "/v1/runs/" + r.ID + "/retry?scope=scope-b"} {
		got := serve(t, s, "POST", path, `{"attempt":1}`, nil)
		if got.Code != 404 || got.Body.String() != unheld.Body.String() {
			t.Fatalf("%s: %d %s", path, got.Code, got.Body)
		}
	}
	if held, e := s.run(r.ID); e != nil || held.State != "interrupted" || journalLength(t, s) != before {
		t.Fatalf("%+v %v", held, e)
	}
	if got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry?scope=scope-a", `{"attempt":1}`, nil); got.Code != 202 || !strings.Contains(got.Body.String(), `"scope":"scope-a"`) {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	var version string
	must(t, sqliteDB(s).QueryRow("SELECT entry_version FROM events WHERE kind='run.queued' AND from_state='interrupted'").Scan(&version))
	if version != attemptEntryVersion {
		t.Fatal(version)
	}
}

// The contract's retry is a case of the conformance suite
// (store_conformance_test.go): an interrupted run is queued again once.

// A record without a retry policy or an attempt is written byte for byte as
// before (the literals of TestARecordWithoutAScopeIsWrittenAsBefore), and a
// store whose jobs have no policy holds none of the new members anywhere,
// writes no entry of version 4, and stays at its schema.
func TestARecordWithoutARetryIsWrittenAsBefore(t *testing.T) {
	at := "2026-01-02T03:04:05Z"
	for _, c := range []struct{ name, got, want string }{
		{"job", string(encode(Job{SchemaVersion: "1", ID: "job_golden", Name: "Golden", ReleaseID: "release_golden", Revision: 1, CreatedAt: at, Workspace: "journal", Owner: "journal-owner"})),
			`{"schemaVersion":"1","id":"job_golden","name":"Golden","releaseId":"release_golden","revision":1,"createdAt":"2026-01-02T03:04:05Z","workspace":"journal","owner":"journal-owner"}`},
		{"occurrence", string(encode(Occurrence{QueueSeconds: 3600, ID: "occ_golden", JobID: "job_golden", ReleaseID: "release_golden", JobRevision: 1, TriggerID: "trg_golden", TriggerRevision: 1, Kind: "event", EventID: "first", ReceivedAt: at, ExpiresAt: at, State: "accepted"})),
			`{"queueSeconds":3600,"id":"occ_golden","jobId":"job_golden","releaseId":"release_golden","jobRevision":1,"triggerId":"trg_golden","triggerRevision":1,"kind":"event","eventId":"first","receivedAt":"2026-01-02T03:04:05Z","expiresAt":"2026-01-02T03:04:05Z","state":"accepted"}`},
		{"run", runText(Run{SchemaVersion: "1", ID: "run_golden", JobID: "job_golden", ReleaseID: "release_golden", Revision: 1, State: "queued", Input: Input{Facts: json.RawMessage(`{"a":1}`)}, CreatedAt: at, RequestedBy: "journal-owner"}),
			`{"schemaVersion":"1","id":"run_golden","jobId":"job_golden","releaseId":"release_golden","revision":1,"state":"queued","input":{"facts":{"a":1}},"createdAt":"2026-01-02T03:04:05Z","requestedBy":"journal-owner","attempt":0}`},
		{"exported run", string(encode(Run{SchemaVersion: "1", ID: "run_golden", JobID: "job_golden", ReleaseID: "release_golden", Revision: 1, State: "completed", Input: Input{Facts: json.RawMessage(`{"a":1}`)}, CreatedAt: at, RequestedBy: "journal-owner", Attempt: 2, Attempts: []Attempt{{Number: 1, Outcome: "interrupted"}}})),
			`{"schemaVersion":"1","id":"run_golden","jobId":"job_golden","releaseId":"release_golden","revision":1,"state":"completed","input":{"facts":{"a":1}},"createdAt":"2026-01-02T03:04:05Z","requestedBy":"journal-owner","attempt":2}`},
		{"entry detail", string(encode(EventDetail{TriggerKind: "event"})), `{"triggerKind":"event"}`},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, c.got, c.want)
		}
	}
	cfg := journalConfig(t)
	s := runJournalSteps(t, cfg, false).s
	for table, column := range map[string]string{"jobs": "record", "job_creations": "record", "job_revisions": "record", "occurrences": "record", "runs": "record", "events": "detail"} {
		var n, held int
		if e := sqliteDB(s).QueryRow("SELECT count(*), count(CASE WHEN instr("+column+",'\"retry\"')>0 OR instr("+column+",'\"attempts\"')>0 OR instr("+column+",'\"nextAttemptAt\"')>0 OR instr("+column+",'\"attempt\":{')>0 THEN 1 END) FROM "+table).Scan(&n, &held); e != nil {
			t.Fatal(e)
		}
		if n == 0 || held != 0 {
			t.Errorf("%s: %d of %d rows hold a retry member", table, held, n)
		}
	}
	if versions := entryVersionsBesideResults(t, sqliteDB(s)); versions != "1" {
		t.Fatalf("entry versions %q", versions)
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("schema %s", schema)
	}
}
