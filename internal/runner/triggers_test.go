package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func instant(s string) time.Time {
	v, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return v
}
func TestScheduleDSTAndBoundaries(t *testing.T) {
	c := Schedule{Kind: "daily", Timezone: "America/Toronto", Time: "02:30", StartAt: "2026-01-01T00:00:00Z"}
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
	if got := c.next(instant("2026-03-08T00:00:00Z")); !got.Equal(instant("2026-03-09T06:30:00Z")) {
		t.Fatal("nonexistent local time", got)
	}
	c.Time = "01:30"
	if got := c.next(instant("2026-11-01T00:00:00Z")); !got.Equal(instant("2026-11-01T05:30:00Z")) {
		t.Fatal("first repeated time", got)
	}
	if got := c.next(instant("2026-11-01T05:30:00Z")); !got.Equal(instant("2026-11-02T06:30:00Z")) {
		t.Fatal("ran twice at repeated time", got)
	}
	if got := c.latest(instant("2026-11-01T06:31:00Z")); !got.Equal(instant("2026-11-01T05:30:00Z")) {
		t.Fatal("latest repeated time", got)
	}
	c = Schedule{Kind: "interval", EverySeconds: 86400, Timezone: "America/Toronto", StartAt: "2026-03-07T12:00:00Z"}
	if got := c.next(instant(c.StartAt)); !got.Equal(instant("2026-03-08T12:00:00Z")) {
		t.Fatal("elapsed interval shifted", got)
	}
	c.EndAt = "2026-03-08T12:00:00Z"
	if !c.next(instant(c.StartAt)).IsZero() {
		t.Fatal("end is exclusive")
	}
	for _, bad := range []Schedule{{Kind: "interval", EverySeconds: 0, Timezone: "UTC", StartAt: c.StartAt}, {Kind: "daily", Time: "25:00", Timezone: "UTC", StartAt: c.StartAt}, {Kind: "daily", Time: "01:00", Timezone: "Local", StartAt: c.StartAt}} {
		if bad.validate() == nil {
			t.Fatal("invalid schedule accepted", bad)
		}
	}
}
func automaticFixture(t *testing.T) (*Service, Config, Job, Release) {
	t.Helper()
	cfg := testConfig(t)
	cfg.disableAutomation = true
	cfg.InputRoot = t.TempDir()
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	r := testRelease(t, s)
	j, e := s.createJob("Automation", r.ID)
	if e != nil {
		t.Fatal(e)
	}
	return s, cfg, j, r
}
func constantTrigger(at time.Time) TriggerConfig {
	return TriggerConfig{Name: "Every minute", Kind: "schedule", Schedule: &Schedule{Kind: "interval", EverySeconds: 60, Timezone: "UTC", StartAt: at.Add(-10 * time.Minute).Format(time.RFC3339)}, Input: &AutomaticInput{Kind: "constant", Value: string(encode(sample()))}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
}
func countRows(t *testing.T, s *Service, table string) int {
	t.Helper()
	var n int
	if e := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func latestOccurrence(t *testing.T, s *Service) Occurrence {
	t.Helper()
	var raw string
	if e := s.db.QueryRow("SELECT record FROM occurrences ORDER BY seq DESC LIMIT 1").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var o Occurrence
	if e := json.Unmarshal([]byte(raw), &o); e != nil {
		t.Fatal(e)
	}
	return o
}
func TestSchedulePausedMissedAndReconciliation(t *testing.T) {
	s, cfg, j, _ := automaticFixture(t)
	at := time.Now().UTC().Truncate(time.Second)
	c := constantTrigger(at)
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	if !tr.Paused {
		t.Fatal("trigger enabled implicitly")
	}
	if e = s.automationTick(at); e != nil {
		t.Fatal(e)
	}
	if countRows(t, s, "occurrences") != 0 {
		t.Fatal("paused occurrence")
	}
	if _, _, e = s.setTriggerState(tr.ID, tr.Revision, false, false); e == nil {
		t.Fatal("enabled without review")
	}
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	tr.NextAt = at.Add(-5 * time.Minute).Format(time.RFC3339)
	if e = s.writeTrigger(tr, "", false); e != nil {
		t.Fatal(e)
	}
	if e = s.automationTick(at); e != nil {
		t.Fatal(e)
	}
	o := latestOccurrence(t, s)
	if o.State != "skipped" || o.Reason != "missed" || o.RunID != "" || o.MissedFrom == "" {
		t.Fatal(o)
	}
	tr, _, _ = s.trigger(tr.ID)
	tr.Config.Missed = "latest"
	tr.NextAt = at.Add(-3 * time.Minute).Format(time.RFC3339)
	s.writeTrigger(tr, "", false)
	if e = s.automationTick(at); e != nil {
		t.Fatal(e)
	}
	o = latestOccurrence(t, s)
	if o.State != "submitted" || o.RunID == "" {
		t.Fatal(o)
	}
	done := waitRun(t, s, o.RunID)
	if done.State != "completed" || done.Trigger == nil || done.Trigger.OccurrenceID != o.ID {
		t.Fatal(done.State, done.Problem)
	}
	// Simulate the crash window after durable run admission, before linking the occurrence.
	o.State = "accepted"
	o.RunID = ""
	o.ExpiresAt = at.Add(-time.Hour).Format(time.RFC3339)
	o.Input = nil
	if e = s.saveOccurrence(o); e != nil {
		t.Fatal(e)
	}
	s.Close()
	again, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if e = again.automationTick(at.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	restored, _ := again.run(done.ID)
	if restored.ID != done.ID {
		t.Fatal("lost run")
	}
	var linked string
	if e = again.db.QueryRow("SELECT json_extract(record,'$.runId') FROM occurrences WHERE id=?", o.ID).Scan(&linked); e != nil || linked != done.ID {
		t.Fatal("admission not reconciled", e, linked)
	}
	var n int
	again.db.QueryRow("SELECT count(*) FROM runs WHERE idem=?", o.ID).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate evaluation", n)
	}
}
func TestEventCredentialsReplayConflictAndPause(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	c := TriggerConfig{Name: "Intake event", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	tr, key, e := s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil || len(key) != 64 {
		t.Fatal(e)
	}
	event := EventDelivery{ID: "event-1", OccurredAt: now(), Input: sample()}
	if _, _, e = s.event(tr.ID, strings.Repeat("a", 64), event); e == nil {
		t.Fatal("bad key admitted")
	}
	o, replay, e := s.event(tr.ID, key, event)
	if e != nil || replay || o.State != "accepted" || o.Input != nil {
		t.Fatal(o, replay, e)
	}
	old, replay, e := s.event(tr.ID, key, event)
	if e != nil || !replay || old.ID != o.ID {
		t.Fatal("retry duplicated", e)
	}
	event.Input.Facts = json.RawMessage(`{}`)
	if _, _, e = s.event(tr.ID, key, event); e == nil {
		t.Fatal("changed payload reused ID")
	}
	event.ID = "old-event"
	event.OccurredAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	if _, _, e = s.event(tr.ID, key, event); e == nil {
		t.Fatal("stale delivery admitted")
	}
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, true, false)
	if e != nil {
		t.Fatal(e)
	}
	event.ID = "paused"
	event.OccurredAt = now()
	if _, _, e = s.event(tr.ID, key, event); e == nil {
		t.Fatal("paused delivery admitted")
	}
	if e = s.automationTick(time.Now()); e != nil {
		t.Fatal(e)
	}
	o = latestOccurrence(t, s)
	if o.RunID == "" {
		t.Fatal("pause cancelled accepted work")
	}
	tr, newKey, e := s.rotateTriggerKey(tr.ID, tr.Revision)
	if e != nil || newKey == key {
		t.Fatal(e)
	}
	if _, _, e = s.event(tr.ID, key, event); e == nil {
		t.Fatal("rotated key remains valid")
	}
}
func TestFileChangeStableFreshAndBounded(t *testing.T) {
	s, cfg, j, _ := automaticFixture(t)
	file := filepath.Join(cfg.InputRoot, "input.json")
	os.WriteFile(file, encode(sample()), 0600)
	c := TriggerConfig{Name: "File intake", Kind: "file", WatchPath: "input.json", StableSeconds: 2, Input: &AutomaticInput{Kind: "input-file", Path: "input.json"}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	at := time.Now()
	s.automationTick(at)
	if countRows(t, s, "occurrences") != 0 {
		t.Fatal("existing baseline ran")
	}
	changed := sample()
	changed.Evidence = nil
	os.WriteFile(file, encode(changed), 0600)
	if e = s.automationTick(at.Add(time.Second)); e != nil {
		t.Fatal(e)
	}
	if countRows(t, s, "occurrences") != 0 {
		t.Fatal("unstable file ran")
	}
	if e = s.automationTick(at.Add(4 * time.Second)); e != nil {
		t.Fatal(e)
	}
	o := latestOccurrence(t, s)
	done := waitRun(t, s, o.RunID)
	if len(done.Input.Evidence) != 0 {
		t.Fatal("reused old evidence")
	}
	s.automationTick(at.Add(8 * time.Second))
	if countRows(t, s, "occurrences") != 1 {
		t.Fatal("duplicate change")
	}
	if _, e = s.readAutomaticFile("../outside.json"); e == nil {
		t.Fatal("path escaped root")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	os.WriteFile(outside, []byte(`{}`), 0600)
	if os.Symlink(outside, filepath.Join(cfg.InputRoot, "escape.json")) == nil {
		if _, e = s.readAutomaticFile("escape.json"); e == nil {
			t.Fatal("symlink escaped root")
		}
	}
	os.WriteFile(file, []byte(`{`), 0600)
	s.automationTick(at.Add(9 * time.Second))
	s.automationTick(at.Add(12 * time.Second))
	if latestOccurrence(t, s).State != "failed" {
		t.Fatal("invalid file silently reused inputs")
	}
}
func TestInitialTriggerAtomicAndExpiry(t *testing.T) {
	s, _, _, r := automaticFixture(t)
	r.ID = "rel_fixture_other"
	s.db.Exec("INSERT INTO releases VALUES(?,?)", r.ID, string(encode(r)))
	c := constantTrigger(time.Now())
	bad := c
	bad.Schedule = &Schedule{Kind: "bad"}
	before := countRows(t, s, "jobs")
	if _, e := s.createJobConfigured("New", r.ID, &bad); e == nil {
		t.Fatal("bad initial trigger")
	}
	if countRows(t, s, "jobs") != before {
		t.Fatal("partial job creation")
	}
	j, e := s.createJobConfigured("New", r.ID, &c)
	if e != nil || j.InitialTriggerID == "" {
		t.Fatal(e)
	}
	again, e := s.createJobConfigured("New", r.ID, &c)
	if e != nil || again.ID != j.ID {
		t.Fatal("creation retry", e)
	}
	origin := &TriggerOrigin{TriggerID: j.InitialTriggerID, OccurrenceID: "expired", ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	run, _, e := s.submitInternal(j.ID, "expired", sample(), origin)
	if e != nil {
		t.Fatal(e)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "failed" || len(done.Audit) > 0 || done.Attempt != 0 {
		t.Fatal("expired input evaluated", done)
	}
}

func TestAutomaticMappingReadsFreshFilesAndKeepsExactValues(t *testing.T) {
	s, cfg, _, r := automaticFixture(t)
	os.WriteFile(filepath.Join(cfg.InputRoot, "source.json"), []byte(`{"n":9007199254740993,"proof":"present"}`), 0600)
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			var mapping InputMapping
			raw := `{"version":1,"provider":"local-file","facts":[{"target":"/n","source":"/n"}],"evidence":[{"requirement":"proof","source":"/proof"}]}`
			if version == 2 {
				raw = `{"version":2,"case":{"facts":[],"evidence":[]},"sources":[{"name":"file","kind":"selected-file","provider":"local-file","read":{"copy":{"facts":[{"target":"/n","source":"/n"}],"evidence":[{"requirement":"proof","source":"/proof"}]}}}]}`
			}
			if e := json.Unmarshal([]byte(raw), &mapping); e != nil {
				t.Fatal(e)
			}
			r.InputMapping = &mapping
			c := &AutomaticInput{Kind: "mapped-files", Files: map[string]string{"file": "source.json"}}
			// v2 forbids unsafe binary64 integers; a rejected value must never round.
			input, e := s.automaticInput(c, r)
			if version == 2 {
				if e == nil {
					t.Fatal("unsafe v2 integer rounded")
				}
				os.WriteFile(filepath.Join(cfg.InputRoot, "source.json"), []byte(`{"n":7,"proof":"present"}`), 0600)
				input, e = s.automaticInput(c, r)
			}
			if e != nil {
				t.Fatal(e)
			}
			mapped, e := s.normalizeInput(input)
			if e != nil {
				t.Fatal(e)
			}
			want := "9007199254740993"
			if version == 2 {
				want = "7"
			}
			if !strings.Contains(string(mapped.Facts), want) {
				t.Fatal(string(mapped.Facts))
			}
			os.WriteFile(filepath.Join(cfg.InputRoot, "source.json"), []byte(`{"n":8,"proof":"absent"}`), 0600)
			fresh, e := s.automaticInput(c, r)
			if e != nil {
				t.Fatal(e)
			}
			mapped, e = s.normalizeInput(fresh)
			if e != nil || !strings.Contains(string(mapped.Facts), "8") || !strings.Contains(string(mapped.Evidence), "absent") {
				t.Fatal(e, string(mapped.Facts), string(mapped.Evidence))
			}
			os.WriteFile(filepath.Join(cfg.InputRoot, "source.json"), []byte(`{"n":9007199254740993,"proof":"present"}`), 0600)
		})
	}
}
func TestEventsOverlapExpiryAndRootBinding(t *testing.T) {
	s, cfg, j, _ := automaticFixture(t)
	tr, e := s.configureTrigger(j.ID, "", 0, TriggerConfig{Name: "One at a time", Kind: "event", Missed: "skip", Overlap: "skip", QueueSeconds: 60})
	if e != nil {
		t.Fatal(e)
	}
	tr, key, e := s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	first, _, e := s.event(tr.ID, key, EventDelivery{ID: "first", OccurredAt: now(), Input: sample()})
	if e != nil {
		t.Fatal(e)
	}
	skipped, _, e := s.event(tr.ID, key, EventDelivery{ID: "second", OccurredAt: now(), Input: sample()})
	if e != nil || skipped.State != "skipped" || skipped.Reason != "overlap" {
		t.Fatal(skipped, e)
	}
	if e = s.automationTick(time.Now().Add(2 * time.Minute)); e != nil {
		t.Fatal(e)
	}
	held, _, e := s.findOccurrence(tr.ID, "event:first")
	if e != nil || held.State != "expired" || held.ID != first.ID || countRows(t, s, "runs") != 0 {
		t.Fatal(held, e)
	}
	s.Close()
	cfg.InputRoot = t.TempDir()
	if changed, e := Open(cfg); e == nil {
		changed.Close()
		t.Fatal("changed input root reused enabled paths")
	}
}

func TestOneTimeFractionalDeadlineRunsOnceAndCompletes(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	at := time.Now().Add(time.Minute).Truncate(time.Second).Add(750 * time.Millisecond)
	c := constantTrigger(at)
	c.Schedule = &Schedule{Kind: "once", Timezone: "UTC", StartAt: at.Format(time.RFC3339Nano)}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	if !instant(tr.NextAt).Equal(at) {
		t.Fatal("fractional deadline truncated", tr.NextAt)
	}
	if e = s.automationTick(at.Add(-500 * time.Millisecond)); e != nil || countRows(t, s, "occurrences") != 0 {
		t.Fatal("fired before deadline", e)
	}
	if e = s.automationTick(at.Add(10 * time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	o := latestOccurrence(t, s)
	if o.RunID == "" {
		t.Fatal(o)
	}
	waitRun(t, s, o.RunID)
	if e = s.automationTick(at.Add(time.Second)); e != nil {
		t.Fatal("duplicate occurrence", e)
	}
	tr, _, e = s.trigger(tr.ID)
	if e != nil || tr.NextAt != "" || countRows(t, s, "occurrences") != 1 {
		t.Fatal("one time did not finish", tr, e)
	}
}

func TestConcurrentEventRetriesAdmitOneOccurrence(t *testing.T) {
	s, _, j, _ := automaticFixture(t)
	tr, e := s.configureTrigger(j.ID, "", 0, TriggerConfig{Name: "Concurrent ingress", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 60})
	if e != nil {
		t.Fatal(e)
	}
	tr, key, e := s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	delivery := EventDelivery{ID: "same-id", OccurredAt: now(), Input: sample()}
	results := make(chan string, 12)
	for range 12 {
		go func() {
			o, _, e := s.event(tr.ID, key, delivery)
			if e != nil {
				results <- "error:" + e.Error()
				return
			}
			results <- o.ID
		}()
	}
	first := <-results
	for range 11 {
		if got := <-results; got != first {
			t.Fatal("concurrent retry diverged", got, first)
		}
	}
	if !strings.HasPrefix(first, "occ_") || countRows(t, s, "occurrences") != 1 {
		t.Fatal(first)
	}
}
