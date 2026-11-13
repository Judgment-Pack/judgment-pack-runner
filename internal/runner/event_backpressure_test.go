package runner

// Back-pressure on the token event route (#211). While the store's queue of
// pending occurrences is full, an event delivered through a trigger's
// credential is refused, 429 queue_full and retryable, and nothing is
// admitted: its ID stays unused, so the sender can deliver the same bytes
// again while their occurredAt is in the five-minute window. A schedule, a
// file change or a cloud signal has no sender to refuse, and is still
// recorded skipped.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"
)

const (
	eventQueueFull = `{"error":{"code":"queue_full","message":"The local queue is full. Deliver the same event again after queued occurrences are submitted, while its occurredAt is within the last five minutes.","retryable":true}}` + "\n"
	eventExpired   = `{"error":{"code":"event_expired","message":"Supply an event timestamp from the last five minutes.","retryable":false}}` + "\n"
	eventConflict  = `{"error":{"code":"event_conflict","message":"This event ID was already received with a different payload.","retryable":false}}` + "\n"
)

// sender delivers bodies to an event trigger with its credential, and reads
// what the store holds under an event ID through the owner's read.
type sender struct {
	w   *coverWorld
	tr  Trigger
	key string
}

func (s sender) post(body string) *httptest.ResponseRecorder {
	return serve(s.w.t, s.w.s, "POST", "/v1/triggers/"+s.tr.ID+"/events", body, map[string]string{"X-Trigger-Token": s.key})
}

func (s sender) held(event string) int {
	return serve(s.w.t, s.w.s, "GET", "/v1/triggers/"+s.tr.ID+"/deliveries/"+url.PathEscape(event), "", nil).Code
}

// fullQueue configures an enabled event trigger that queues and fills the
// store's queue to queueLimit-room pending occurrences.
func fullQueue(t *testing.T, room int) (sender, []Occurrence) {
	w := newCoverWorld(t)
	tr, key := w.eventTrigger("queue")
	var queued []Occurrence
	for range queueLimit - room {
		queued = append(queued, w.admitted(tr, nil))
	}
	return sender{w, tr, key}, queued
}

// makeRoom ends queued occurrences, as their expiry does.
func makeRoom(t *testing.T, s *Service, queued []Occurrence) {
	t.Helper()
	for _, o := range queued {
		o.State, o.Reason, o.Input = "expired", "queue-expired", nil
		must(t, s.saveOccurrence(o))
	}
}

func pendingOccurrences(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	must(t, sqliteDB(s).QueryRow("SELECT count(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting')").Scan(&n))
	return n
}

// An event at a full queue is refused with exact bytes, again and again, and
// leaves nothing behind but one journaled refusal: no occurrence under its
// ID. Once the queue has room the same bytes are admitted, and from then on
// the ID is the admitted event's: it replays, at a full queue too, and
// another payload under it conflicts.
func TestAnEventAtAFullQueueIsRefusedAndItsIDStaysUnused(t *testing.T) {
	s, queued := fullQueue(t, 0)
	const event = "sender-event-7"
	body := string(encode(delivery(event)))
	before := len(s.w.entries())
	for i := range 3 {
		got := s.post(body)
		if got.Code != 429 || got.Body.String() != eventQueueFull || got.Header().Get("Retry-After") != "" {
			t.Fatalf("delivery %d to a full queue answered %d %q %v", i, got.Code, got.Body.String(), got.Header())
		}
	}
	if code := s.held(event); code != 404 || countRows(t, s.w.s, "occurrences") != queueLimit {
		t.Fatal("a refused event was held", code, countRows(t, s.w.s, "occurrences"))
	}
	added := s.w.entries()[before:]
	if len(added) != 1 || added[0].Kind != "admission.refused" || added[0].Request != "deliver-event" || added[0].Code != "queue_full" || added[0].Status != 429 || !sameActor(added[0].By, credentialActor(s.tr.ID, s.tr.keyRevision)) {
		t.Fatalf("three refusals journaled as %+v", added)
	}

	makeRoom(t, s.w.s, queued[:1])
	got := s.post(body)
	code, a := answerOf(t, got.Code, got.Body.Bytes())
	if code != 202 || a.Replayed || a.Occurrence.State != "accepted" || a.Occurrence.EventID != event {
		t.Fatal("the same bytes once the queue had room", code, got.Body.String())
	}
	admitted := a.Occurrence.ID
	if n := pendingOccurrences(t, s.w.s); n != queueLimit {
		t.Fatal("pending after the admission", n)
	}
	// The queue is full again: the admitted event still replays.
	got = s.post(body)
	if code, a = answerOf(t, got.Code, got.Body.Bytes()); code != 200 || !a.Replayed || a.Occurrence.ID != admitted {
		t.Fatal("the admitted event delivered again", code, got.Body.String())
	}
	other := delivery(event)
	other.Input = Input{Facts: json.RawMessage(`{"other":true}`)}
	if got = s.post(string(encode(other))); got.Code != 409 || got.Body.String() != eventConflict {
		t.Fatal("another payload under the admitted ID", got.Code, got.Body.String())
	}
	if code := s.held(event); code != 200 || countRows(t, s.w.s, "occurrences") != queueLimit+1 {
		t.Fatal("after the replay and the conflict", code, countRows(t, s.w.s, "occurrences"))
	}
}

// A sender that keeps an event's ID and timestamp is refused for the queue
// while the timestamp is in its window, and once the window closes is
// refused for the timestamp, the answer any old event gets, whether or not
// the queue has room: the earlier refusals held nothing that answers for it.
func TestAnEventRefusedAtAFullQueueIsRefusedForItsAgeOnceItsWindowCloses(t *testing.T) {
	s, queued := fullQueue(t, 0)
	const event = "late-sender-event"
	at := time.Now().Add(-5*time.Minute + 3*time.Second)
	body := string(encode(EventDelivery{ID: event, OccurredAt: at.UTC().Format(time.RFC3339Nano), Input: journalInput()}))
	if got := s.post(body); got.Code != 429 || got.Body.String() != eventQueueFull {
		t.Fatalf("%s after its occurredAt, inside the window: %d %q", time.Since(at), got.Code, got.Body.String())
	}
	time.Sleep(time.Until(at.Add(5*time.Minute + 100*time.Millisecond)))
	if got := s.post(body); got.Code != 422 || got.Body.String() != eventExpired {
		t.Fatalf("%s after its occurredAt, the window closed: %d %q", time.Since(at), got.Code, got.Body.String())
	}
	makeRoom(t, s.w.s, queued[:1])
	if got := s.post(body); got.Code != 422 || got.Body.String() != eventExpired {
		t.Fatalf("the window closed and the queue has room: %d %q", got.Code, got.Body.String())
	}
	if code := s.held(event); code != 404 || pendingOccurrences(t, s.w.s) != queueLimit-1 {
		t.Fatal("an event refused for its window was held", code, pendingOccurrences(t, s.w.s))
	}
}

// Senders at once at a queue with room for five, two of each of twenty
// events, have five events admitted and no more. Each event is admitted once
// and replayed to its twin, or refused to both; a refused event holds
// nothing, and the same bytes are admitted once the queue has room. Run
// under -race.
func TestConcurrentSendersAtAFullQueueNeverPassTheLimitNorLoseAnID(t *testing.T) {
	const room, events = 5, 20
	s, queued := fullQueue(t, room)
	bodies := map[string]string{}
	var ids []string
	for i := range events {
		id := fmt.Sprintf("burst-%02d", i)
		ids = append(ids, id)
		bodies[id] = string(encode(delivery(id)))
	}
	type answer struct {
		code int
		body string
	}
	burst := func(ids []string) map[string][]answer {
		var mu sync.Mutex
		var all sync.WaitGroup
		got := map[string][]answer{}
		start := make(chan struct{})
		for _, id := range ids {
			for range 2 {
				all.Add(1)
				go func() {
					defer all.Done()
					<-start
					r := s.post(bodies[id])
					mu.Lock()
					got[id] = append(got[id], answer{r.Code, r.Body.String()})
					mu.Unlock()
				}()
			}
		}
		close(start)
		all.Wait()
		return got
	}
	admittedOnce := func(id string, pair []answer) {
		t.Helper()
		slices.SortFunc(pair, func(a, b answer) int { return a.code - b.code })
		if len(pair) != 2 || pair[0].code != 200 || pair[1].code != 202 {
			t.Fatal("an admitted event's pair", id, pair)
		}
		_, replay := answerOf(t, pair[0].code, []byte(pair[0].body))
		_, first := answerOf(t, pair[1].code, []byte(pair[1].body))
		if !replay.Replayed || first.Replayed || first.Occurrence.State != "accepted" || replay.Occurrence.ID != first.Occurrence.ID {
			t.Fatal("an admitted event's pair", id, pair)
		}
	}
	var refused []string
	for id, pair := range burst(ids) {
		switch {
		case len(pair) == 2 && pair[0].code == 429 && pair[1].code == 429:
			if pair[0].body != eventQueueFull || pair[1].body != eventQueueFull {
				t.Fatal("a refused event's pair", id, pair)
			}
			if code := s.held(id); code != 404 {
				t.Fatal("a refused event was held", id, code)
			}
			refused = append(refused, id)
		default:
			admittedOnce(id, pair)
		}
	}
	if len(refused) != events-room || pendingOccurrences(t, s.w.s) != queueLimit {
		t.Fatal("senders at once admitted", events-len(refused), "with room for", room, "pending", pendingOccurrences(t, s.w.s))
	}
	makeRoom(t, s.w.s, queued)
	for id, pair := range burst(refused) {
		admittedOnce(id, pair)
	}
	var held int
	must(t, sqliteDB(s.w.s).QueryRow("SELECT count(*) FROM occurrences WHERE trigger_id=? AND identity LIKE 'event:burst-%'", s.tr.ID).Scan(&held))
	if held != events || pendingOccurrences(t, s.w.s) != events {
		t.Fatal("after the queue had room", held, pendingOccurrences(t, s.w.s))
	}
}

// A schedule falling due, a watched file changing and a cloud signal
// arriving at a full queue have no sender to refuse: each is recorded
// skipped, reason queue-full, as before.
func TestSchedulesFilesAndCloudSignalsAtAFullQueueAreStillSkipped(t *testing.T) {
	w := newCoverWorld(t)
	schedule := w.scheduleTrigger("")
	file := w.fileTrigger(`{"facts":{"version":1}}`)
	cloud, conn := w.cloudTrigger("skip")
	for range queueLimit {
		w.admitted(schedule, nil)
	}
	skipped := func(name, trigger string) {
		t.Helper()
		o := latestOccurrence(t, w.s)
		if o.TriggerID != trigger || o.State != "skipped" || o.Reason != "queue-full" || o.Input != nil || o.PendingInput != nil {
			t.Fatalf("%s at a full queue: %+v", name, o)
		}
	}
	at := time.Now().UTC().Truncate(time.Second)
	tr, hash := storedTrigger(t, w.s, schedule.ID)
	tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
	must(t, w.s.writeTrigger(tr, hash, false))
	must(t, w.s.scheduleTick(tr, hash, at))
	skipped("a schedule", schedule.ID)
	must(t, w.fileChanged(file, `{"facts":{"version":2}}`))
	skipped("a file change", file.ID)
	if ack, e := w.s.receiveCloud(conn, cloudSignal(cloud, time.Now())); !ack || e != nil {
		t.Fatal("a cloud signal at a full queue", ack, e)
	}
	skipped("a cloud signal", cloud.ID)
	if n := pendingOccurrences(t, w.s); n != queueLimit {
		t.Fatal("pending", n)
	}
}
