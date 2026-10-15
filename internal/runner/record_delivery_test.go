package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecordDeliveryFrozenRevisionAndRestart(t *testing.T) {
	s, cfg, j, r := automaticFixture(t)
	tr, e := s.configureTrigger(j.ID, "", 0, TriggerConfig{Name: "Reviewed records", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600})
	if e != nil {
		t.Fatal(e)
	}
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	event := EventDelivery{ID: "collection:record:revision:release", OccurredAt: time.Now().Add(-24 * time.Hour).Format(time.RFC3339), Input: sample()}
	if _, _, e = s.deliverRecord(tr.ID, "wrong-release", event); e == nil {
		t.Fatal("wrong release admitted")
	}
	o, replay, e := s.deliverRecord(tr.ID, r.ID, event)
	if e != nil || replay || o.State != "accepted" {
		t.Fatal(o, replay, e)
	}
	var actorRaw string
	if e = s.db.QueryRow("SELECT by_actor FROM events WHERE json_extract(concerns,'$.occurrence')=? ORDER BY sequence LIMIT 1", o.ID).Scan(&actorRaw); e != nil {
		t.Fatal(e)
	}
	var actor EventActor
	if json.Unmarshal([]byte(actorRaw), &actor) != nil || actor.Kind != "installation" {
		t.Fatal("owner delivery misattributed", actorRaw)
	}
	if o.Input != nil {
		t.Fatal("acknowledgement leaks source")
	}
	old, again, e := s.deliverRecord(tr.ID, r.ID, event)
	if e != nil || !again || old.ID != o.ID {
		t.Fatal("duplicate revision", old, again, e)
	}
	changed := event
	changed.Input.Facts = json.RawMessage(`{"changed":true}`)
	if _, _, e = s.deliverRecord(tr.ID, r.ID, changed); e == nil {
		t.Fatal("same revision changed payload")
	}
	changed.ID = "collection:record:revision-2:release"
	if _, _, e = s.deliverRecord(tr.ID, r.ID, changed); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	old, again, e = s.deliverRecord(tr.ID, r.ID, event)
	if e != nil || !again || old.ID != o.ID {
		t.Fatal("restart lost admission", e)
	}
	current, _, e := s.trigger(tr.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.setTriggerState(tr.ID, current.Revision, true, false); e != nil {
		t.Fatal(e)
	}
	if _, again, e = s.deliverRecord(tr.ID, r.ID, event); e != nil || !again {
		t.Fatal("paused reconciliation failed", e)
	}
	event.ID = "new-while-paused"
	if _, _, e = s.deliverRecord(tr.ID, r.ID, event); e == nil {
		t.Fatal("paused trigger admitted new revision")
	}
	if countRows(t, s, "occurrences") != 2 {
		t.Fatal("wrong occurrence count")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/triggers/"+tr.ID+"/deliveries", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	s.Handler("owner-secret").ServeHTTP(response, req)
	if response.Code != 401 {
		t.Fatal("owner delivery was not authenticated")
	}
}

func TestRecordDeliveryHTTPPreservesVersion3AndJournalsRefusals(t *testing.T) {
	input, _, _, _ := v2Fixture(t)
	three := asVersion3(input)
	w := newCoverWorld(t)
	tr, _ := w.eventTrigger("queue")
	job, err := w.s.job(tr.JobID)
	if err != nil {
		t.Fatal(err)
	}
	raw := encode(RecordDelivery{ReleaseID: job.ReleaseID, Event: EventDelivery{ID: "owner-v3", OccurredAt: now(), Input: three}})
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"mapping case", bytes.Replace(raw, []byte(`"mapping":`), []byte(`"Mapping":`), 1)},
		{"event case", bytes.Replace(raw, []byte(`"event":`), []byte(`"Event":`), 1)},
		{"empty provider", bytes.Replace(raw, []byte(`"version":3`), []byte(`"version":3,"provider":""`), 1)},
		{"null facts", bytes.Replace(raw, []byte(`"version":3`), []byte(`"version":3,"facts":null`), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Equal(raw, tc.raw) {
				t.Fatal("fixture was not changed")
			}
			for range 2 {
				got := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/deliveries", string(tc.raw), nil)
				if got.Code != 422 || !strings.Contains(got.Body.String(), `"invalid_source_input"`) {
					t.Fatal(got.Code, got.Body.String())
				}
			}
		})
	}
	var total int
	var by string
	if err = w.s.db.QueryRow("SELECT count(*),min(by_actor) FROM events WHERE kind='admission.refused' AND json_extract(concerns,'$.trigger')=?", tr.ID).Scan(&total, &by); err != nil || total != 1 || !strings.Contains(by, `"kind":"installation"`) {
		t.Fatal("refusal not coalesced/attributed", total, by, err)
	}
	control := serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/deliveries", string(raw), nil)
	if control.Code != 422 || !strings.Contains(control.Body.String(), `"mapping_mismatch"`) {
		t.Fatal("exact mapping did not reach frozen release check", control.Code, control.Body.String())
	}
}
