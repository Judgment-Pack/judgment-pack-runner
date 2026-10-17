package runner

// The record delivery contract (#70, docs/design/record-delivery.md). A
// caller's durable outbox delivers immutable record revisions to an event
// trigger through the owner's POST /v1/triggers/{trigger}/deliveries, each
// bound to the job's reviewed release, and reconciles each by its event ID
// through GET /v1/triggers/{trigger}/deliveries/{event}. The tests hold each
// clause on the store and, where a run is needed, on the pinned Runtime, over
// loopback only.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordRevisions are three revisions of one record, which the triage pack
// decides three ways: a run's own decision names the revision whose bytes it
// was given.
var recordRevisions = []struct{ completeness, appropriateness, outcome string }{
	{"complete", "pass", "proceed"},
	{"incomplete", "pass", "clarify-return"},
	{"complete", "hard-fail", "decline-redirect"},
}

func recordRevision(n int) Input {
	r := recordRevisions[n]
	return Input{Facts: json.RawMessage(fmt.Sprintf(`{"request":{"type":"data-access","completeness":%q,"appropriateness":%q,"embargoedInformationToUnauthorizedRecipients":false}}`, r.completeness, r.appropriateness)), Evidence: json.RawMessage(`{"intake-form":"present","sponsor-endorsement":"present"}`)}
}

// recordWorld is a store with a job of the triage pack, whose release the
// pinned Runtime checked, and an enabled event trigger that queues, as a
// reviewed record subscription is configured. Nothing runs by itself: a test
// makes accepted occurrences into runs (tick) and evaluates them (evaluate).
type recordWorld struct {
	t       *testing.T
	cfg     Config
	s       *Service
	release Release
	job     Job
	trigger Trigger
	// outbox is the caller's: each envelope's exact bytes, by event ID,
	// persisted before the envelope's first delivery.
	outbox map[string]string
	// occurredAt is when the outbox recorded its revisions, a day before the
	// test: an outbox may wait through downtime.
	occurredAt string
}

func newRecordWorld(t *testing.T) *recordWorld {
	t.Helper()
	cfg := testConfig(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	w := &recordWorld{t: t, cfg: cfg, outbox: map[string]string{}, occurredAt: time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)}
	w.s = openJournal(t, cfg)
	t.Cleanup(func() { w.s.Close() })
	w.release = testRelease(t, w.s)
	var e error
	w.job, e = w.s.createJob("Collection", w.release.ID)
	must(t, e)
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, TriggerConfig{Name: "Reviewed records", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600})
	must(t, e)
	w.trigger, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, e)
	return w
}

// envelope is a record delivery's bytes.
func (w *recordWorld) envelope(event string, revision int, release string) string {
	return string(encode(RecordDelivery{ReleaseID: release, Event: EventDelivery{ID: event, OccurredAt: w.occurredAt, Input: recordRevision(revision)}}))
}

// persist records an envelope in the outbox, before its first delivery, and
// returns its bytes.
func (w *recordWorld) persist(event string, revision int, release string) string {
	body := w.envelope(event, revision, release)
	w.outbox[event] = body
	return body
}

// recordAnswer is what the delivery routes answer.
type recordAnswer struct {
	Occurrence Occurrence `json:"occurrence"`
	Replayed   bool       `json:"replayed"`
	Error      struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func answerOf(t *testing.T, code int, body []byte) (int, recordAnswer) {
	t.Helper()
	var a recordAnswer
	if e := json.Unmarshal(body, &a); e != nil {
		t.Fatal(code, string(body))
	}
	return code, a
}

func (w *recordWorld) deliver(body string) (int, recordAnswer) {
	w.t.Helper()
	got := serve(w.t, w.s, "POST", "/v1/triggers/"+w.trigger.ID+"/deliveries", body, nil)
	return answerOf(w.t, got.Code, got.Body.Bytes())
}

// reconcile reads a delivery by its event ID. The answer never carries the
// delivery's input.
func (w *recordWorld) reconcile(event string) (int, recordAnswer) {
	w.t.Helper()
	got := serve(w.t, w.s, "GET", "/v1/triggers/"+w.trigger.ID+"/deliveries/"+url.PathEscape(event), "", nil)
	if bytes.Contains(got.Body.Bytes(), []byte(`"input"`)) {
		w.t.Fatal("a reconciliation answered with the delivery's input", got.Body.String())
	}
	return answerOf(w.t, got.Code, got.Body.Bytes())
}

// tick makes each accepted occurrence a run, as automation does each second.
func (w *recordWorld) tick() {
	w.t.Helper()
	must(w.t, w.s.automationTick(time.Now()))
}

// evaluate takes every queued run through the pinned Runtime, in order.
func (w *recordWorld) evaluate() {
	w.t.Helper()
	for {
		worked, e := w.s.dispatchNext(context.Background())
		must(w.t, e)
		if !worked {
			return
		}
	}
}

func (w *recordWorld) restart() {
	w.t.Helper()
	w.s.Close()
	w.s = openJournal(w.t, w.cfg)
}

func (w *recordWorld) run(id string) Run {
	w.t.Helper()
	r, e := w.s.run(id)
	must(w.t, e)
	return r
}

// decided is a completed run's outcome, as the Runtime answered it.
func decided(t *testing.T, r Run) string {
	t.Helper()
	if r.State != "completed" {
		t.Fatal(r.ID, r.State, r.Problem)
	}
	var result struct {
		Disposition struct {
			OutcomeID string `json:"outcomeId"`
		} `json:"disposition"`
	}
	if e := json.Unmarshal(r.Result, &result); e != nil {
		t.Fatal(e)
	}
	return result.Disposition.OutcomeID
}

// entriesOf are the journal's entries of a kind that concern an occurrence.
func entriesOf(t *testing.T, cfg Config, kind, occurrence string) int {
	t.Helper()
	n := 0
	for _, e := range journalEntries(t, cfg) {
		if e.Kind == kind && e.Concerns.Occurrence == occurrence {
			n++
		}
	}
	return n
}

// The same envelope delivered again, before its run is made, after and once
// it is evaluated, answers its occurrence, and no second occurrence, run or
// journal entry is made.
func TestARecordRevisionDeliveredAgainIsItsOccurrenceAndOneRun(t *testing.T) {
	w := newRecordWorld(t)
	body := w.persist("collection-a:record-7:revision-1", 0, w.release.ID)
	code, first := w.deliver(body)
	if code != 202 || first.Replayed || first.Occurrence.State != "accepted" || first.Occurrence.EventID != "collection-a:record-7:revision-1" || first.Occurrence.ReleaseID != w.release.ID || first.Occurrence.JobRevision != 1 {
		t.Fatal(code, first)
	}
	for _, step := range []func(){func() {}, w.tick, w.evaluate} {
		step()
		code, again := w.deliver(body)
		if code != 200 || !again.Replayed || again.Occurrence.ID != first.Occurrence.ID {
			t.Fatal("a repeated delivery was not its occurrence", code, again)
		}
	}
	w.tick()
	w.evaluate()
	code, held := w.reconcile("collection-a:record-7:revision-1")
	if code != 200 || held.Occurrence.ID != first.Occurrence.ID || held.Occurrence.State != "submitted" || held.Occurrence.RunID == "" {
		t.Fatal(code, held)
	}
	if outcome := decided(t, w.run(held.Occurrence.RunID)); outcome != "proceed" {
		t.Fatal(outcome)
	}
	if countRows(t, w.s, "occurrences") != 1 || countRows(t, w.s, "runs") != 1 {
		t.Fatal("a repeated delivery made a second occurrence or run")
	}
	if entriesOf(t, w.cfg, "occurrence.received", first.Occurrence.ID) != 1 || entriesOf(t, w.cfg, "run.queued", first.Occurrence.ID) != 1 {
		t.Fatal("a repeated delivery was journaled again")
	}
}

// Deliveries of one revision at the same moment admit one occurrence: one
// answer is the admission, every other its replay. Revisions of other
// records delivered at the same moment are each their own occurrence, and
// the trigger's overlap policy, queue, gives each its own run.
func TestConcurrentDeliveriesOfOneRevisionAdmitOneOccurrence(t *testing.T) {
	w := newRecordWorld(t)
	h := w.s.Handler(journalBearer)
	post := func(body string) (int, recordAnswer) {
		req := httptest.NewRequest("POST", "/v1/triggers/"+w.trigger.ID+"/deliveries", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+journalBearer)
		got := httptest.NewRecorder()
		h.ServeHTTP(got, req)
		var a recordAnswer
		if json.Unmarshal(got.Body.Bytes(), &a) != nil {
			return got.Code, recordAnswer{}
		}
		return got.Code, a
	}
	type answer struct {
		event string
		code  int
		a     recordAnswer
	}
	events := []string{"collection-a:record-7:revision-1", "collection-a:record-8:revision-1", "collection-a:record-9:revision-1"}
	for i, event := range events {
		w.persist(event, i, w.release.ID)
	}
	answers := make(chan answer, 16*len(events))
	var all sync.WaitGroup
	for range 16 {
		for _, event := range events {
			all.Add(1)
			go func() {
				defer all.Done()
				code, a := post(w.outbox[event])
				answers <- answer{event, code, a}
			}()
		}
	}
	all.Wait()
	close(answers)
	admitted := map[string]string{}
	replays := map[string]int{}
	for got := range answers {
		switch {
		case got.code == 202 && !got.a.Replayed && got.a.Occurrence.State == "accepted":
			if admitted[got.event] != "" {
				t.Fatal("one revision was admitted twice", got.event)
			}
			admitted[got.event] = got.a.Occurrence.ID
		case got.code == 200 && got.a.Replayed:
			replays[got.event]++
		default:
			t.Fatal("a concurrent delivery was answered", got.code, got.a)
		}
	}
	for _, event := range events {
		if admitted[event] == "" || replays[event] != 15 {
			t.Fatal("concurrent deliveries diverged", event, admitted, replays)
		}
		if code, held := w.reconcile(event); code != 200 || held.Occurrence.ID != admitted[event] {
			t.Fatal("a concurrent delivery replayed another occurrence", event, code, held)
		}
	}
	if countRows(t, w.s, "occurrences") != len(events) {
		t.Fatal("concurrent deliveries admitted", countRows(t, w.s, "occurrences"))
	}
	w.tick()
	w.evaluate()
	for i, event := range events {
		_, held := w.reconcile(event)
		if outcome := decided(t, w.run(held.Occurrence.RunID)); outcome != recordRevisions[i].outcome {
			t.Fatal(event, outcome)
		}
	}
	if countRows(t, w.s, "runs") != len(events) {
		t.Fatal("runs made", countRows(t, w.s, "runs"))
	}
}

// A changed revision of a record is a new event ID: its own occurrence and
// run, bound to the same release and job revision, while the earlier
// revision's occurrence and run stay as they were.
func TestAChangedRevisionIsANewOccurrenceBoundToTheSameRelease(t *testing.T) {
	w := newRecordWorld(t)
	code, first := w.deliver(w.persist("collection-a:record-7:revision-1", 0, w.release.ID))
	if code != 202 {
		t.Fatal(code, first)
	}
	w.tick()
	w.evaluate()
	_, first = w.reconcile("collection-a:record-7:revision-1")
	earlier := w.run(first.Occurrence.RunID)
	code, second := w.deliver(w.persist("collection-a:record-7:revision-2", 1, w.release.ID))
	if code != 202 || second.Replayed || second.Occurrence.ID == first.Occurrence.ID || second.Occurrence.ReleaseID != w.release.ID || second.Occurrence.JobRevision != first.Occurrence.JobRevision || second.Occurrence.InputDigest == first.Occurrence.InputDigest {
		t.Fatal(code, second)
	}
	w.tick()
	w.evaluate()
	_, second = w.reconcile("collection-a:record-7:revision-2")
	later := w.run(second.Occurrence.RunID)
	if later.ID == earlier.ID || later.ReleaseID != w.release.ID || later.Revision != earlier.Revision || decided(t, later) != "clarify-return" {
		t.Fatal("the changed revision's run", later.ReleaseID, later.Revision)
	}
	if again := w.run(earlier.ID); !bytes.Equal(encode(again), encode(earlier)) || decided(t, again) != "proceed" {
		t.Fatal("the earlier revision's run changed")
	}
}

// A run evaluates the bytes its delivery froze at admission. The record
// changes twice while its first revision waits in the queue: each run is
// given its own revision's bytes, which its occurrence's input digest names,
// and the Runtime decides each as that revision. The bytes are taken once,
// from the delivery, into the occurrence (admitBoundEvent), moved into the
// run in the run's own admission (dispatchOccurrence, submitInternal) and
// written for the Runtime from the run (dispatchNext): Runner holds no later
// record to read.
func TestARunEvaluatesTheBytesFrozenAtItsAdmission(t *testing.T) {
	w := newRecordWorld(t)
	events := []string{"collection-a:record-7:revision-1", "collection-a:record-7:revision-2", "collection-a:record-7:revision-3"}
	digests := map[string]string{}
	for i, event := range events {
		code, a := w.deliver(w.persist(event, i, w.release.ID))
		if code != 202 || a.Occurrence.InputDigest != digest(encode(recordRevision(i))) {
			t.Fatal(code, a)
		}
		digests[event] = a.Occurrence.InputDigest
	}
	w.tick()
	w.evaluate()
	for i, event := range events {
		_, held := w.reconcile(event)
		r := w.run(held.Occurrence.RunID)
		if r.Trigger == nil || r.Trigger.EventID != event || r.Trigger.OccurrenceID != held.Occurrence.ID || r.Trigger.InputDigest != digests[event] || held.Occurrence.InputDigest != digests[event] {
			t.Fatal("the run does not name its delivery", event, r.Trigger)
		}
		if !sameJSON(r.Input.Facts, recordRevision(i).Facts) || !sameJSON(r.Input.Evidence, recordRevision(i).Evidence) {
			t.Fatal("the run holds another revision's input", event, string(r.Input.Facts))
		}
		given, e := os.ReadFile(filepath.Join(w.cfg.Dir, "attempts", r.ID, "facts.json"))
		must(t, e)
		if !bytes.Equal(given, r.Input.Facts) {
			t.Fatal("the Runtime was given other facts than the run holds", event, string(given))
		}
		if outcome := decided(t, r); outcome != recordRevisions[i].outcome {
			t.Fatal("the Runtime decided another revision", event, outcome)
		}
		// Once its run holds them, the occurrence no longer keeps the bytes.
		if o := storedOccurrence(t, w.s, held.Occurrence.ID); o.Input != nil {
			t.Fatal("a submitted occurrence kept its input", event)
		}
	}
}

// A changed release is a reviewed job upgrade. A revision bound to the
// earlier release is then refused by name; the earlier revision's delivery
// still answers its occurrence and run; its event ID under the new release is
// another payload; a revision bound to the new release runs under it; and the
// earlier run keeps its release, job revision, input and result.
func TestAChangedReleaseLeavesEarlierRunsTheirReleaseAndInput(t *testing.T) {
	w := newRecordWorld(t)
	firstEvent := "collection-a:record-7:revision-1:" + w.release.ID
	first := w.persist(firstEvent, 0, w.release.ID)
	if code, a := w.deliver(first); code != 202 {
		t.Fatal(code, a)
	}
	w.tick()
	w.evaluate()
	_, held := w.reconcile(firstEvent)
	earlier := w.run(held.Occurrence.RunID)
	if decided(t, earlier) != "proceed" {
		t.Fatal(earlier.Result)
	}
	tr, _ := storedTrigger(t, w.s, w.trigger.ID)
	_, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false)
	must(t, e)
	next := testRelease(t, w.s)
	upgraded, e := w.s.upgradeJob(context.Background(), w.job.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: w.release.ID, BaseRevision: 1, Reviewed: true})
	must(t, e)
	if upgraded.ReleaseID != next.ID || upgraded.Revision != 2 {
		t.Fatal(upgraded)
	}
	tr, _ = storedTrigger(t, w.s, w.trigger.ID)
	_, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, e)

	staleEvent := "collection-a:record-7:revision-2:" + w.release.ID
	if code, a := w.deliver(w.persist(staleEvent, 1, w.release.ID)); code != 409 || a.Error.Code != "release_changed" {
		t.Fatal("a revision bound to the earlier release", code, a)
	}
	if code, _ := w.reconcile(staleEvent); code != 404 {
		t.Fatal("a refused revision was admitted", code)
	}
	code, again := w.deliver(first)
	if code != 200 || !again.Replayed || again.Occurrence.ID != held.Occurrence.ID || again.Occurrence.ReleaseID != w.release.ID || again.Occurrence.JobRevision != 1 || again.Occurrence.RunID != earlier.ID {
		t.Fatal("the earlier delivery after the upgrade", code, again)
	}
	if code, a := w.deliver(w.envelope(firstEvent, 0, next.ID)); code != 409 || a.Error.Code != "event_conflict" {
		t.Fatal("an event ID moved to another release", code, a)
	}
	freshEvent := "collection-a:record-7:revision-2:" + next.ID
	code, fresh := w.deliver(w.persist(freshEvent, 1, next.ID))
	if code != 202 || fresh.Occurrence.ReleaseID != next.ID || fresh.Occurrence.JobRevision != 2 {
		t.Fatal(code, fresh)
	}
	w.tick()
	w.evaluate()
	_, fresh = w.reconcile(freshEvent)
	later := w.run(fresh.Occurrence.RunID)
	if later.ReleaseID != next.ID || later.Revision != 2 || decided(t, later) != "clarify-return" {
		t.Fatal("the new revision's run", later.ReleaseID, later.Revision)
	}
	kept := w.run(earlier.ID)
	if kept.ReleaseID != w.release.ID || kept.Revision != 1 || !bytes.Equal(encode(kept.Input), encode(earlier.Input)) || !bytes.Equal(kept.Result, earlier.Result) || !bytes.Equal(encode(kept.Trigger), encode(earlier.Trigger)) {
		t.Fatal("the earlier run changed with the release")
	}
	if countRows(t, w.s, "runs") != 2 {
		t.Fatal("runs made", countRows(t, w.s, "runs"))
	}
}

// A restart keeps the queue: an occurrence accepted and not yet run is run
// after it, once. An evaluation that was in progress when the process
// stopped is interrupted and not repeated, and its delivery still answers
// its occurrence and run. No delivery made again makes a run.
func TestTheQueueAndAnInFlightDeliverySurviveARestart(t *testing.T) {
	w := newRecordWorld(t)
	inFlight := "collection-a:record-8:revision-1"
	code, a := w.deliver(w.persist(inFlight, 1, w.release.ID))
	if code != 202 {
		t.Fatal(code, a)
	}
	w.tick()
	running := w.run(storedOccurrence(t, w.s, a.Occurrence.ID).RunID)
	running.State, running.StartedAt, running.Attempt = "running", now(), 1
	must(t, w.s.changeRun(running))
	queued := []string{"collection-a:record-7:revision-1", "collection-a:record-9:revision-1"}
	for _, event := range queued {
		if code, a := w.deliver(w.persist(event, 0, w.release.ID)); code != 202 || a.Occurrence.State != "accepted" {
			t.Fatal(code, a)
		}
	}
	w.restart()
	for _, event := range queued {
		if code, held := w.reconcile(event); code != 200 || held.Occurrence.State != "accepted" || held.Occurrence.RunID != "" {
			t.Fatal("the queue was not kept", event, code, held)
		}
	}
	if code, held := w.reconcile(inFlight); code != 200 || held.Occurrence.State != "submitted" || held.Occurrence.RunID != running.ID {
		t.Fatal("the in-flight delivery", code, held)
	}
	if r := w.run(running.ID); r.State != "interrupted" {
		t.Fatal("the evaluation in progress", r.State)
	}
	w.tick()
	w.evaluate()
	for _, event := range queued {
		_, held := w.reconcile(event)
		if held.Occurrence.State != "submitted" || decided(t, w.run(held.Occurrence.RunID)) != "proceed" {
			t.Fatal("a queued delivery after the restart", event, held)
		}
	}
	for event, body := range w.outbox {
		if code, again := w.deliver(body); code != 200 || !again.Replayed {
			t.Fatal("a delivery made again after the restart", event, code, again)
		}
	}
	w.tick()
	w.evaluate()
	if r := w.run(running.ID); r.State != "interrupted" {
		t.Fatal("the interrupted run was repeated", r.State)
	}
	if countRows(t, w.s, "runs") != 3 || countRows(t, w.s, "occurrences") != 3 {
		t.Fatal("runs made", countRows(t, w.s, "runs"))
	}
}

// A process that stopped after it made an occurrence's run and before it
// recorded the run on the occurrence leaves the occurrence accepted. After a
// restart, past the occurrence's queue time, the run it made is found: the
// occurrence is neither expired nor run again.
func TestADeliveryWhoseRunWasMadeBeforeAStopIsNeitherRunAgainNorExpired(t *testing.T) {
	w := newRecordWorld(t)
	body := w.persist("collection-a:record-7:revision-1", 0, w.release.ID)
	code, a := w.deliver(body)
	if code != 202 {
		t.Fatal(code, a)
	}
	// The run is made as dispatchOccurrence makes it; the occurrence is not
	// recorded after it.
	o := storedOccurrence(t, w.s, a.Occurrence.ID)
	origin := &TriggerOrigin{OccurrenceID: o.ID, TriggerID: o.TriggerID, TriggerRevision: o.TriggerRevision, Kind: o.Kind, EventID: o.EventID, InputDigest: o.InputDigest, ExpiresAt: o.ExpiresAt}
	made, _, e := w.s.submitInternal(o.JobID, o.ID, *o.Input, origin)
	must(t, e)
	w.restart()
	must(t, w.s.automationTick(time.Now().Add(2*time.Hour)))
	held := storedOccurrence(t, w.s, o.ID)
	if held.State != "submitted" || held.RunID != made.ID || countRows(t, w.s, "runs") != 1 {
		t.Fatal("the run made before the stop", held.State, held.Reason, held.RunID)
	}
	w.evaluate()
	if code, again := w.deliver(body); code != 200 || !again.Replayed || again.Occurrence.RunID != made.ID {
		t.Fatal(code, again)
	}
	if decided(t, w.run(made.ID)) != "proceed" || countRows(t, w.s, "runs") != 1 {
		t.Fatal("the run was made again")
	}
}

// The same event ID with another payload is refused by name, event_conflict,
// whichever member changed, the bound release included, through either
// route, before and after a restart; the retained occurrence is unchanged and
// the original envelope still answers it.
func TestAChangedPayloadUnderOneEventIdentityIsRefusedByName(t *testing.T) {
	w := newCoverWorld(t)
	tr, key := w.eventTrigger("queue")
	const event = "collection-a:record-7:revision-1"
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	envelope := func(release string, occurredAt time.Time, input Input) string {
		return string(encode(RecordDelivery{ReleaseID: release, Event: EventDelivery{ID: event, OccurredAt: occurredAt.Format(time.RFC3339), Input: input}}))
	}
	deliver := func(body string) (int, recordAnswer) {
		got := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/deliveries", body, nil)
		return answerOf(t, got.Code, got.Body.Bytes())
	}
	original := envelope(w.job.ReleaseID, at, journalInput())
	code, first := deliver(original)
	if code != 202 {
		t.Fatal(code, first)
	}
	retained := storedOccurrence(t, w.s, first.Occurrence.ID)
	facts := journalInput()
	facts.Facts = json.RawMessage(`{"request":{"marker":"another revision"}}`)
	evidence := journalInput()
	evidence.Evidence = json.RawMessage(`{"intake-form":"present"}`)
	other := journalRelease(t, w.s, w.release.RuntimeDigest)
	changed := map[string]string{
		"facts":      envelope(w.job.ReleaseID, at, facts),
		"evidence":   envelope(w.job.ReleaseID, at, evidence),
		"occurredAt": envelope(w.job.ReleaseID, at.Add(time.Second), journalInput()),
		"releaseId":  envelope(other.ID, at, journalInput()),
	}
	check := func() {
		t.Helper()
		for member, body := range changed {
			if code, a := deliver(body); code != 409 || a.Error.Code != "event_conflict" || a.Error.Retryable {
				t.Fatal("a changed "+member+" under the same event ID", code, a)
			}
		}
		token := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/events", string(encode(EventDelivery{ID: event, OccurredAt: now(), Input: journalInput()})), map[string]string{"X-Trigger-Token": key})
		if code, a := answerOf(t, token.Code, token.Body.Bytes()); code != 409 || a.Error.Code != "event_conflict" {
			t.Fatal("a credential's delivery under the same event ID", code, a)
		}
		if code, a := deliver(original); code != 200 || !a.Replayed || a.Occurrence.ID != first.Occurrence.ID {
			t.Fatal("the original envelope", code, a)
		}
		if held := storedOccurrence(t, w.s, first.Occurrence.ID); !bytes.Equal(encode(held), encode(retained)) {
			t.Fatal("a refused delivery changed the retained occurrence")
		}
		if countRows(t, w.s, "occurrences") != 1 {
			t.Fatal("a refused delivery was admitted")
		}
	}
	check()
	w.s.Close()
	w.s = openJournal(t, w.cfg)
	check()
	// Refusals alike within a minute are journaled once, across the restart:
	// the first, the owner's.
	var refused []Event
	for _, e := range w.entries() {
		if e.Kind == "admission.refused" && e.Concerns.Trigger == tr.ID {
			refused = append(refused, e)
		}
	}
	if len(refused) != 1 || refused[0].Request != "deliver-event" || refused[0].Code != "event_conflict" || refused[0].Status != 409 || refused[0].By.Kind != "installation" {
		t.Fatalf("%+v", refused)
	}
}

// While the store's queue of pending occurrences is full, a record delivery
// is refused, 429 queue_full and retryable, and nothing is admitted: its
// event ID stays free, and the same envelope is admitted once the queue has
// room. A credential's delivery is still recorded skipped, as the events
// route says.
func TestARecordDeliveryIsRefusedNotSkippedWhileTheQueueIsFull(t *testing.T) {
	w := newCoverWorld(t)
	tr, key := w.eventTrigger("queue")
	var queued []Occurrence
	for range queueLimit {
		queued = append(queued, w.admitted(tr, nil))
	}
	const event = "collection-a:record-7:revision-1"
	body := string(encode(RecordDelivery{ReleaseID: w.job.ReleaseID, Event: EventDelivery{ID: event, OccurredAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Input: journalInput()}}))
	deliver := func() (int, recordAnswer) {
		got := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/deliveries", body, nil)
		return answerOf(t, got.Code, got.Body.Bytes())
	}
	before := len(w.entries())
	for range 2 {
		if code, a := deliver(); code != 429 || a.Error.Code != "queue_full" || !a.Error.Retryable {
			t.Fatal("a record delivery to a full queue", code, a)
		}
	}
	read := serve(t, w.s, "GET", "/v1/triggers/"+tr.ID+"/deliveries/"+url.PathEscape(event), "", nil)
	if read.Code != 404 || countRows(t, w.s, "occurrences") != queueLimit {
		t.Fatal("a refused record delivery was admitted", read.Code, read.Body.String())
	}
	var refused []Event
	for _, e := range w.entries()[before:] {
		if e.Kind != "admission.refused" {
			t.Fatal("a refused record delivery was journaled as", e.Kind)
		}
		refused = append(refused, e)
	}
	if len(refused) != 1 || refused[0].Request != "deliver-event" || refused[0].Code != "queue_full" || refused[0].Status != 429 || refused[0].By.Kind != "installation" {
		t.Fatalf("%+v", refused)
	}
	token := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/events", string(encode(delivery("credential-event"))), map[string]string{"X-Trigger-Token": key})
	if code, a := answerOf(t, token.Code, token.Body.Bytes()); code != 202 || a.Occurrence.State != "skipped" || a.Occurrence.Reason != "queue-full" {
		t.Fatal("a credential's delivery to a full queue", code, a)
	}
	room := queued[0]
	room.State, room.Reason, room.Input = "expired", "queue-expired", nil
	must(t, w.s.saveOccurrence(room))
	code, a := deliver()
	if code != 202 || a.Replayed || a.Occurrence.State != "accepted" || a.Occurrence.EventID != event {
		t.Fatal("the same envelope once the queue had room", code, a)
	}
}

// A caller reconciles a delivery by its event ID, over loopback, whether its
// acknowledgement arrived or was lost on the way back, or the delivery never
// arrived; and reads the run it became. No reconciliation, and no delivery
// made again, makes a second run, before or after a restart.
func TestACallerReconcilesAnAcknowledgedOrUncertainDeliveryWithoutASecondRun(t *testing.T) {
	w := newRecordWorld(t)
	var current atomic.Pointer[Service]
	current.Store(w.s)
	var drop atomic.Bool
	listener := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := current.Load().Handler(journalBearer)
		if !drop.CompareAndSwap(true, false) {
			h.ServeHTTP(rw, r)
			return
		}
		// The delivery is handled, and its answer is lost on the way back.
		h.ServeHTTP(httptest.NewRecorder(), r)
		if conn, _, e := rw.(http.Hijacker).Hijack(); e == nil {
			conn.Close()
		}
	}))
	defer listener.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	call := func(method, path, body string) (int, []byte, error) {
		req, e := http.NewRequest(method, listener.URL+path, strings.NewReader(body))
		must(t, e)
		req.Header.Set("Authorization", "Bearer "+journalBearer)
		res, e := client.Do(req)
		if e != nil {
			return 0, nil, e
		}
		defer res.Body.Close()
		raw, e := io.ReadAll(res.Body)
		return res.StatusCode, raw, e
	}
	deliver := func(body string) (int, recordAnswer) {
		code, raw, e := call("POST", "/v1/triggers/"+w.trigger.ID+"/deliveries", body)
		must(t, e)
		return answerOf(t, code, raw)
	}
	reconcile := func(event string) (int, recordAnswer) {
		code, raw, e := call("GET", "/v1/triggers/"+w.trigger.ID+"/deliveries/"+url.PathEscape(event), "")
		must(t, e)
		if bytes.Contains(raw, []byte(`"input"`)) {
			t.Fatal("a reconciliation answered with the delivery's input")
		}
		return answerOf(t, code, raw)
	}
	acknowledged := "collection-a:record-7:revision-1"
	lost := "collection-a/record 8:revision-1"
	unsent := "collection-a/record 9:revision-1?"

	code, ack := deliver(w.persist(acknowledged, 0, w.release.ID))
	if code != 202 {
		t.Fatal(code, ack)
	}
	if code, held := reconcile(acknowledged); code != 200 || held.Occurrence.ID != ack.Occurrence.ID || held.Occurrence.State != "accepted" {
		t.Fatal("an acknowledged delivery", code, held)
	}

	drop.Store(true)
	if _, _, e := call("POST", "/v1/triggers/"+w.trigger.ID+"/deliveries", w.persist(lost, 1, w.release.ID)); e == nil {
		t.Fatal("the acknowledgement was not lost")
	}
	code, uncertain := reconcile(lost)
	if code != 200 || uncertain.Occurrence.State != "accepted" || uncertain.Occurrence.EventID != lost {
		t.Fatal("a delivery whose acknowledgement was lost", code, uncertain)
	}
	if code, again := deliver(w.outbox[lost]); code != 200 || !again.Replayed || again.Occurrence.ID != uncertain.Occurrence.ID {
		t.Fatal("a delivery made again after its acknowledgement was lost", code, again)
	}

	w.persist(unsent, 2, w.release.ID)
	if code, a := reconcile(unsent); code != 404 || a.Error.Code != "not_found" {
		t.Fatal("a delivery that never arrived", code, a)
	}
	if code, a := deliver(w.outbox[unsent]); code != 202 || a.Replayed {
		t.Fatal("a delivery that never arrived, made", code, a)
	}

	w.restart()
	current.Store(w.s)
	w.tick()
	w.evaluate()
	runs := map[string]string{}
	for i, event := range []string{acknowledged, lost, unsent} {
		code, held := reconcile(event)
		if code != 200 || held.Occurrence.State != "submitted" || held.Occurrence.RunID == "" {
			t.Fatal("a delivery after the restart", event, code, held)
		}
		code, raw, e := call("GET", "/v1/runs/"+held.Occurrence.RunID, "")
		must(t, e)
		var r Run
		if code != 200 || json.Unmarshal(raw, &r) != nil || r.Trigger == nil || r.Trigger.EventID != event || decided(t, r) != recordRevisions[i].outcome {
			t.Fatal("the run a delivery became", event, code, string(raw))
		}
		runs[event] = r.ID
	}
	for event, body := range w.outbox {
		code, again := deliver(body)
		if code != 200 || !again.Replayed || again.Occurrence.RunID != runs[event] {
			t.Fatal("a delivery made again once run", event, code, again)
		}
	}
	w.tick()
	w.evaluate()
	if countRows(t, w.s, "runs") != 3 || countRows(t, w.s, "occurrences") != 3 {
		t.Fatal("a reconciliation made a run", countRows(t, w.s, "runs"))
	}
}

// A record delivery that carries an acquisition is held to the one receipt
// rule at admission and again when its run is made and evaluated; the
// reviewed mapping's admission of a generated source is the run's lineage;
// what a caller computed is never taken for what Runner verifies; and nothing
// on this path asks a provider: not for a source missing, refused or aged,
// and not when the same envelope is delivered again.
func TestARecordDeliveryCarryingAnAcquisitionKeepsTheReceiptRuleAndAcquiresNothing(t *testing.T) {
	cfg := testConfig(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	var asked atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(rw, "no acquisition is expected", 500)
	}))
	defer gateway.Close()
	input, p, key, at := v2Fixture(t)
	// A generated source, which the reviewed mapping admits to each target its
	// read fills.
	p.Class = "generated"
	m := &input.Source.Mapping
	m.UnmappedEvidence = []string{"sensitive-data-approvals"}
	m.Admits = &Admission{Facts: map[string][]string{"/request": {"generated"}}, Evidence: map[string][]string{"intake-form": {"generated"}, "sponsor-endorsement": {"generated"}}}
	m.Sources[0].ProfileDigest = profileHash(p)
	m.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	cfg.InputProfiles = []InputProfile{p}
	cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: gateway.URL}}
	s := openJournal(t, cfg)
	defer s.Close()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	must(t, e)
	release, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	must(t, e)
	job, e := s.createJob("Vendors", release.ID)
	must(t, e)
	tr, e := s.configureTrigger(job.ID, "", 0, TriggerConfig{Name: "Reviewed vendors", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600})
	must(t, e)
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(t, e)
	occurredAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	envelope := func(event string, i Input) string {
		return string(encode(RecordDelivery{ReleaseID: release.ID, Event: EventDelivery{ID: event, OccurredAt: occurredAt, Input: i}}))
	}
	deliver := func(body string) (int, recordAnswer) {
		got := serve(t, s, "POST", "/v1/triggers/"+tr.ID+"/deliveries", body, nil)
		return answerOf(t, got.Code, got.Body.Bytes())
	}

	altered := cloneInput(input)
	v, _ := readInputJSON(altered.Source.Sources["vendor"].Response)
	v.(map[string]any)["result"] = map[string]any{"facts": map[string]any{"request": map[string]any{"type": "data-access"}}}
	altered.Source.Sources["vendor"] = SourceValue{Response: encode(v)}
	missing := cloneInput(input)
	missing.Source.Sources = nil
	widened := cloneInput(input)
	widened.Source.Mapping.Admits.Facts["/request"] = []string{"generated", "record"}
	asserted := cloneInput(input)
	asserted.Facts = json.RawMessage(`{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":true}}`)
	prepared := cloneInput(input)
	verified, e := s.normalizeInput(input)
	must(t, e)
	prepared.Preparation = verified.Preparation
	for _, c := range []struct {
		name  string
		input Input
		code  string
	}{
		{"a result other than the one its receipt signs", altered, "input_preparation_failed"},
		{"a serving time of a fraction of a second", resigned(t, input, key, func(r map[string]any) {
			r["servedAt"] = at.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
		}), "input_preparation_failed"},
		{"a receipt older than the source's maxAge, in an envelope a day old", resigned(t, input, key, func(r map[string]any) {
			old := at.Add(-10 * time.Minute).Format(time.RFC3339)
			r["servedAt"] = old
			r["acquisition"].(map[string]any)["observedAt"] = old
		}), "input_preparation_failed"},
		{"no response for a source", missing, "input_preparation_failed"},
		{"a mapping that admits more than the reviewed one", widened, "mapping_mismatch"},
		{"facts other than the verified derivation", asserted, "invalid_source_input"},
		{"a preparation the caller computed", prepared, "invalid_event"},
	} {
		event := "vendors:vendor-7:refused:" + strings.ReplaceAll(c.name, " ", "-")
		if code, a := deliver(envelope(event, c.input)); code != 422 || a.Error.Code != c.code {
			t.Fatal(c.name, code, a)
		}
		if got := serve(t, s, "GET", "/v1/triggers/"+tr.ID+"/deliveries/"+url.PathEscape(event), "", nil); got.Code != 404 {
			t.Fatal(c.name, "was admitted")
		}
	}

	body := envelope("vendors:vendor-7:revision-1", input)
	code, a := deliver(body)
	if code != 202 || a.Occurrence.State != "accepted" {
		t.Fatal(code, a)
	}
	// Nothing waits for a source: the background preparation has no work.
	must(t, s.prepareOccurrence(context.Background()))
	must(t, s.automationTick(time.Now()))
	for {
		worked, e := s.dispatchNext(context.Background())
		must(t, e)
		if !worked {
			break
		}
	}
	held := storedOccurrence(t, s, a.Occurrence.ID)
	r, e := s.run(held.RunID)
	must(t, e)
	if decided(t, r) != "proceed" || !bytes.Contains(r.Audit, []byte(`"cites"`)) {
		t.Fatal("the delivered acquisition's run", r.State, r.Problem)
	}
	prep := r.Input.Preparation
	if prep == nil || len(prep.Cites) != 1 || prep.Cites[0].SessionID != "mapping.session" || prep.Cites[0].CallIndex != 0 || len(prep.Lineage) != 3 {
		t.Fatal("the run's preparation", prep)
	}
	for _, l := range prep.Lineage {
		if l.Class != "generated" || !l.GeneratedInfluence || l.Receipt == nil || *l.Receipt != prep.Cites[0] {
			t.Fatalf("lineage %+v", l)
		}
	}
	bundle := verificationExport(release, r, 2)
	if e = VerifyRun(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest); e != nil {
		t.Fatal("the delivered run's export", e)
	}
	if code, again := deliver(body); code != 200 || !again.Replayed || again.Occurrence.RunID != r.ID {
		t.Fatal(code, again)
	}
	must(t, s.prepareOccurrence(context.Background()))
	must(t, s.automationTick(time.Now()))
	if worked, e := s.dispatchNext(context.Background()); e != nil || worked || countRows(t, s, "runs") != 1 {
		t.Fatal("the same envelope delivered again made a run", e)
	}
	if asked.Load() != 0 {
		t.Fatal("a record delivery asked a provider", asked.Load())
	}
}
