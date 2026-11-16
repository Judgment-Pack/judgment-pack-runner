package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// #308. An occurrence's held pages (the pages, the open session, the first
// page's request and the profile) are private, as a task's request and
// profile are: the occurrence routes and the event-replay answer do not
// serve them, while the record keeps them for the next attempt.
func TestTheRoutesDoNotServeAnOccurrencesHeldPages(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	c := eventTriggerConfig()
	j, e := s.createJobWith(requester{}, "Held pages", journalRelease(t, s, completingRuntime(t, s.cfg.Dir)).ID, "", &c, nil)
	must(t, e)
	triggers, e := s.store.jobTriggers(j.ID)
	must(t, e)
	if len(triggers) != 1 {
		t.Fatal("the job's trigger was not made:", triggers)
	}
	tr, key, e := s.setTriggerState(triggers[0].ID, triggers[0].Revision, false, true)
	must(t, e)
	token := map[string]string{"X-Trigger-Token": key}
	body := string(encode(delivery("held")))
	got := serve(t, s, "POST", "/v1/triggers/"+tr.ID+"/events", body, token)
	if got.Code != 202 {
		t.Fatal("the event was not accepted:", got.Code, got.Body.String())
	}
	var answer struct{ Occurrence Occurrence }
	must(t, json.Unmarshal(got.Body.Bytes(), &answer))
	id := answer.Occurrence.ID
	held := `{"source":"changes","profile":"private-profile","session":"private-session","first":{"since":"private-first"},"at":"2026-01-01T00:00:00Z","pages":[{"private":"page"}]}`
	if _, e = sqliteDB(s).Exec("UPDATE occurrences SET record=json_set(record,'$.heldPages',json(?)) WHERE id=?", held, id); e != nil {
		t.Fatal(e)
	}
	stored, e := s.occurrence(id)
	must(t, e)
	if stored.HeldPages == nil || stored.HeldPages.Session != "private-session" {
		t.Fatal("the record does not hold the pages the test wrote:", stored.HeldPages)
	}
	for _, c := range []struct {
		name               string
		method, path, body string
		header             map[string]string
		code               int
	}{
		{"the event's replay", "POST", "/v1/triggers/" + tr.ID + "/events", body, token, 200},
		{"the job's occurrences", "GET", "/v1/jobs/" + j.ID + "/occurrences", "", nil, 200},
		{"the delivery's read", "GET", "/v1/triggers/" + tr.ID + "/deliveries/held", "", nil, 200},
	} {
		got := serve(t, s, c.method, c.path, c.body, c.header)
		if got.Code != c.code || !strings.Contains(got.Body.String(), id) {
			t.Fatalf("%s: %d %s", c.name, got.Code, got.Body.String())
		}
		for _, private := range []string{"heldPages", "private-session", "private-profile", "private-first", `"private":"page"`} {
			if strings.Contains(got.Body.String(), private) {
				t.Errorf("%s serves the held pages (%s): %s", c.name, private, got.Body.String())
			}
		}
	}
}
