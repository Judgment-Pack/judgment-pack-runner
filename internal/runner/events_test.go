package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const journalBearer = "journal-test-bearer-6d1f0c2b9e8a7f3d5c4b2a1908f7e6d5"

// journalFacts carry a marker that no entry may hold: the journal names what
// changed, never a run's inputs.
var journalFacts = json.RawMessage(`{"request":{"marker":"facts-never-journaled-7f3a"}}`)

func journalInput() Input { return Input{Facts: journalFacts} }

// journalConfig is a store with no Runtime of its own to run, and no
// dispatcher or automation: a test makes each change itself. Releases name the
// fake Runtimes a test pins.
func journalConfig(t *testing.T) Config {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	placeholder := filepath.Join(t.TempDir(), "never-run")
	if e = os.WriteFile(placeholder, []byte("not a Runtime: no test runs it\n"), 0600); e != nil {
		t.Fatal(e)
	}
	return Config{Dir: dir, Runtime: placeholder, Workspace: "journal", Owner: "journal-owner", disableAutomation: true, disableDispatcher: true}
}

func openJournal(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

// fakeRuntime pins a shell script into the store as a Runtime, and returns its
// digest, for a release to name. The script sets its own PATH: a Runtime is
// given none.
func fakeRuntime(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-runtime")
	if e := os.WriteFile(path, []byte("#!/bin/sh\nPATH=/usr/bin:/bin\nexport PATH\numask 077\n"+body+"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	_, hash, e := pinExecutable(path, filepath.Join(dir, "runtimes"))
	if e != nil {
		t.Fatal(e)
	}
	return hash
}

const journalPack = `{"id":"journal","version":"1.0.0","title":"Journal","specVersion":"0.2.0-draft"}`
const journalDisposition = `{"kind":"outcome","outcomeId":"proceed","reasons":[],"handoff":{"state":"none"}}`

// completingRuntime answers an operational evaluation as the Runtime does,
// with a one-line audit record of the facts it was given.
func completingRuntime(t *testing.T, dir string) string {
	return fakeRuntime(t, dir, `mkdir -p audit
printf '{"recordVersion":"1","kind":"evaluation","run":"fake","reviewed":true,"pack":{"digest":"%s"},"inputs":{"facts":%s,"evidenceSupplied":false},"disposition":`+journalDisposition+`}\n' '`+digest([]byte(journalPack))+`' "$(cat facts.json)" > audit/evaluations.jsonl
printf '%s\n' '{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"0.2.0-draft","rehearsal":false,"disposition":`+journalDisposition+`}'`)
}

// failingRuntime refuses every evaluation.
func failingRuntime(t *testing.T, dir string) string { return fakeRuntime(t, dir, "exit 3") }

// waitingRuntime writes its process id to pidFile and waits, until it is
// stopped.
func waitingRuntime(t *testing.T, dir, pidFile string) string {
	return fakeRuntime(t, dir, "echo $$ > '"+pidFile+"'\nexec sleep 30")
}

func journalRelease(t *testing.T, s *Service, runtime string) Release {
	t.Helper()
	r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: runtime, CreatedAt: now(), Sample: journalInput(), Tests: "not-run"}
	if e := s.saveRelease(r); e != nil {
		t.Fatal(e)
	}
	return r
}

func eventTriggerConfig() TriggerConfig {
	return TriggerConfig{Name: "Deliveries", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
}

func scheduleTriggerConfig(at time.Time) TriggerConfig {
	return TriggerConfig{Name: "Every minute", Kind: "schedule", Schedule: &Schedule{Kind: "interval", EverySeconds: 60, Timezone: "UTC", StartAt: at.Add(-10 * time.Minute).Format(time.RFC3339)}, Input: &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
}

func delivery(id string) EventDelivery {
	return EventDelivery{ID: id, OccurredAt: time.Now().UTC().Format(time.RFC3339), Input: journalInput()}
}

// journalEntries reads every entry of a store's journal through its own
// connection, as the routes serve them: the store may be open or closed.
func journalEntries(t *testing.T, cfg Config) []Event {
	t.Helper()
	db, e := sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var all []Event
	for after := int64(0); ; {
		page, e := eventPageFrom(db, after, "1=1")
		if e != nil {
			t.Fatal(e)
		}
		all = append(all, page.Items...)
		if !page.More {
			return all
		}
		after = page.Next
	}
}

func kindsOf(entries []Event) []string {
	kinds := []string{}
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func storedOccurrence(t *testing.T, s *Service, id string) Occurrence {
	t.Helper()
	o, e := s.occurrence(id)
	if e != nil {
		t.Fatal(e)
	}
	return o
}

func storedTrigger(t *testing.T, s *Service, id string) (Trigger, string) {
	t.Helper()
	tr, hash, e := s.trigger(id)
	if e != nil {
		t.Fatal(e)
	}
	return tr, hash
}

func admit(t *testing.T, s *Service, tr Trigger, o *Occurrence, identity string) {
	t.Helper()
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, tr, o, identity, ""); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}

func waitForRunState(t *testing.T, s *Service, id, state string) Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, e := s.run(id)
		if e != nil {
			t.Fatal(e)
		}
		if r.State == state {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, state)
	return Run{}
}

func refuse(t *testing.T, s *Service, statement string) {
	t.Helper()
	if _, e := s.db.Exec(statement); e != nil {
		t.Fatal(e)
	}
}

func serve(t *testing.T, s *Service, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+journalBearer)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler(journalBearer).ServeHTTP(w, req)
	return w
}

// journalWorld is the state a scenario of every kind of change builds.
type journalWorld struct {
	t                           *testing.T
	cfg                         Config
	s                           *Service
	release, failing, waiting   Release
	job, failingJob, waitingJob Job
	event, schedule             Trigger
	token, oldToken             string
	first, second, third        Occurrence
	prepared, legacy            string
	completed, interrupted      Run
	left                        Run
}

type journalStep struct {
	name  string
	do    func(w *journalWorld)
	want  []string
	check func(w *journalWorld, added []Event)
}

func sameActor(a, b EventActor) bool { return bytes.Equal(encode(a), encode(b)) }

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}

// journalSteps makes every kind of change Runner records, one step at a time,
// each with the entries it must add, in order, and nothing else.
func journalSteps() []journalStep {
	at := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	return []journalStep{
		{"a new store begins its journal and records its start", func(w *journalWorld) {
			w.s = openJournal(w.t, w.cfg)
		}, []string{"journal.began", "runner.started"}, func(w *journalWorld, added []Event) {
			if added[0].Held == nil || *added[0].Held != (EventHeld{}) || added[0].By.Kind != "runner" {
				w.t.Fatalf("a new store's journal began with %+v", added[0])
			}
			if added[1].Version == "" || added[1].RuntimeDigest != w.s.runtimeDigest {
				w.t.Fatalf("runner.started does not name the build and Runtime: %+v", added[1])
			}
		}},
		{"a preview stores a release", func(w *journalWorld) {
			w.release = journalRelease(w.t, w.s, completingRuntime(w.t, w.cfg.Dir))
		}, []string{"release.previewed"}, func(w *journalWorld, added []Event) {
			e := added[0]
			if e.Concerns != (EventConcerns{Release: w.release.ID}) || e.PackDigest != w.release.PackDigest || e.Tests != "not-run" || !sameActor(e.By, w.s.installation()) || e.To != nil {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a job is created with an event trigger", func(w *journalWorld) {
			c := eventTriggerConfig()
			j, e := w.s.createJobConfigured("Journal", w.release.ID, &c)
			must(w.t, e)
			w.job = j
			w.event, _ = storedTrigger(w.t, w.s, j.InitialTriggerID)
		}, []string{"job.created", "trigger.configured"}, func(w *journalWorld, added []Event) {
			e := added[1]
			if e.Concerns != (EventConcerns{Job: w.job.ID, Release: w.release.ID, Trigger: w.event.ID}) || *e.Revision != 1 || string(e.PreviousRevision) != "null" || string(e.From) != "null" || string(e.To) != `"paused"` || e.TriggerKind != "event" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"repeating the creation changes nothing", func(w *journalWorld) {
			c := eventTriggerConfig()
			_, e := w.s.createJobConfigured("Journal", w.release.ID, &c)
			must(w.t, e)
		}, nil, nil},
		{"the trigger's configuration is saved", func(w *journalWorld) {
			var e error
			w.event, e = w.s.configureTrigger(w.job.ID, w.event.ID, w.event.Revision, eventTriggerConfig())
			must(w.t, e)
		}, []string{"trigger.configured"}, func(w *journalWorld, added []Event) {
			if e := added[0]; *e.Revision != 2 || string(e.PreviousRevision) != "1" || string(e.From) != `"paused"` {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"the trigger is resumed, and its first key issued", func(w *journalWorld) {
			var e error
			w.event, w.token, e = w.s.setTriggerState(w.event.ID, w.event.Revision, false, true)
			must(w.t, e)
		}, []string{"trigger.resumed"}, func(w *journalWorld, added []Event) {
			if e := added[0]; !e.KeyIssued || *e.Revision != 3 || string(e.From) != `"paused"` || string(e.To) != `"enabled"` || !sameActor(e.By, w.s.installation()) {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"resuming it again changes nothing", func(w *journalWorld) {
			_, _, e := w.s.setTriggerState(w.event.ID, w.event.Revision, false, true)
			must(w.t, e)
		}, nil, nil},
		{"its key is rotated", func(w *journalWorld) {
			var e error
			w.oldToken = w.token
			w.event, w.token, e = w.s.rotateTriggerKey(w.event.ID, w.event.Revision)
			must(w.t, e)
		}, []string{"trigger.key-rotated"}, func(w *journalWorld, added []Event) {
			if e := added[0]; *e.Revision != 4 || string(e.PreviousRevision) != "3" || string(e.From) != `"enabled"` || string(e.To) != `"enabled"` {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"an event is delivered", func(w *journalWorld) {
			o, _, e := w.s.event(w.event.ID, w.token, delivery("first"))
			must(w.t, e)
			w.first = o
		}, []string{"occurrence.received"}, func(w *journalWorld, added []Event) {
			e := added[0]
			if e.By.Kind != "trigger-credential" || e.By.Trigger != w.event.ID || string(e.By.KeyRevision) != "4" || e.OccurrenceKind != "event" || string(e.From) != "null" || e.Concerns.Occurrence != w.first.ID {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"the same event again changes nothing", func(w *journalWorld) {
			if _, replay, e := w.s.event(w.event.ID, w.token, delivery("first")); e != nil || !replay {
				w.t.Fatal(e, replay)
			}
		}, nil, nil},
		{"the occurrence is submitted", func(w *journalWorld) {
			must(w.t, w.s.dispatchOccurrence(storedOccurrence(w.t, w.s, w.first.ID), time.Now()))
		}, []string{"run.queued", "occurrence.submitted"}, func(w *journalWorld, added []Event) {
			queued, submitted := added[0], added[1]
			if !sameActor(queued.By, triggerActor(w.event.ID, 4)) || queued.Concerns.Occurrence != w.first.ID || submitted.Concerns.Run != queued.Concerns.Run || submitted.Concerns.Run == "" {
				w.t.Fatalf("%+v %+v", queued, submitted)
			}
		}},
		{"the dispatcher evaluates the run, which completes", func(w *journalWorld) {
			if worked, e := w.s.dispatchNext(ctx); e != nil || !worked {
				w.t.Fatal(worked, e)
			}
			w.completed = waitForRunState(w.t, w.s, storedOccurrence(w.t, w.s, w.first.ID).RunID, "completed")
		}, []string{"run.started", "run.completed"}, func(w *journalWorld, added []Event) {
			sequence, chained, e := w.s.runEntrySequence(w.completed.ID)
			if e != nil || !chained || added[1].ChainSequence != sequence || string(added[0].From) != `"queued"` || string(added[1].From) != `"running"` {
				w.t.Fatalf("%+v %v", added, e)
			}
		}},
		{"a second event is delivered", func(w *journalWorld) {
			o, _, e := w.s.event(w.event.ID, w.token, delivery("second"))
			must(w.t, e)
			w.second = o
		}, []string{"occurrence.received"}, nil},
		{"its queue time runs out before it is submitted", func(w *journalWorld) {
			must(w.t, w.s.dispatchOccurrence(storedOccurrence(w.t, w.s, w.second.ID), time.Now().Add(2*time.Hour)))
		}, []string{"occurrence.expired"}, func(w *journalWorld, added []Event) {
			if e := added[0]; e.Reason != "queue-expired" || string(e.From) != `"accepted"` || e.By.Kind != "runner" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a third event is delivered", func(w *journalWorld) {
			o, _, e := w.s.event(w.event.ID, w.token, delivery("third"))
			must(w.t, e)
			w.third = o
		}, []string{"occurrence.received"}, nil},
		{"it cannot be submitted, since the job is not the one it was admitted for", func(w *journalWorld) {
			o := storedOccurrence(w.t, w.s, w.third.ID)
			o.JobRevision++
			must(w.t, w.s.dispatchOccurrence(o, time.Now()))
		}, []string{"occurrence.failed"}, nil},
		{"a schedule trigger is configured", func(w *journalWorld) {
			var e error
			w.schedule, e = w.s.configureTrigger(w.job.ID, "", 0, scheduleTriggerConfig(at))
			must(w.t, e)
		}, []string{"trigger.configured"}, nil},
		{"it is resumed", func(w *journalWorld) {
			var e error
			w.schedule, _, e = w.s.setTriggerState(w.schedule.ID, w.schedule.Revision, false, true)
			must(w.t, e)
		}, []string{"trigger.resumed"}, func(w *journalWorld, added []Event) {
			if e := added[0]; e.KeyIssued || e.NextAt == "" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a time it missed is skipped", func(w *journalWorld) {
			tr, hash := storedTrigger(w.t, w.s, w.schedule.ID)
			tr.NextAt = at.Add(-5 * time.Minute).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, hash, false))
			must(w.t, w.s.scheduleTick(tr, hash, at))
		}, []string{"occurrence.skipped"}, func(w *journalWorld, added []Event) {
			if e := added[0]; e.Reason != "missed" || !sameActor(e.By, triggerActor(w.schedule.ID, w.schedule.Revision)) || e.MissedFrom == "" || e.ScheduledAt == "" || e.OccurrenceKind != "schedule" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a time whose input cannot be read fails as it is admitted", func(w *journalWorld) {
			tr, hash := storedTrigger(w.t, w.s, w.schedule.ID)
			tr.Config.Input.Value = "{"
			tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, hash, false))
			must(w.t, w.s.scheduleTick(tr, hash, at))
		}, []string{"occurrence.failed"}, func(w *journalWorld, added []Event) {
			if e := added[0]; string(e.From) != "null" || e.Reason == "" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"the schedule trigger is paused", func(w *journalWorld) {
			tr, _ := storedTrigger(w.t, w.s, w.schedule.ID)
			var e error
			w.schedule, _, e = w.s.setTriggerState(tr.ID, tr.Revision, true, false)
			must(w.t, e)
		}, []string{"trigger.paused"}, nil},
		{"an occurrence with inputs to acquire is admitted", func(w *journalWorld) {
			tr, _ := storedTrigger(w.t, w.s, w.schedule.ID)
			o := newOccurrence(tr, w.job, time.Now())
			o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
			admit(w.t, w.s, tr, &o, "legacy")
			w.legacy = o.ID
		}, []string{"occurrence.received"}, nil},
		{"it acquires them and is ready", func(w *journalWorld) {
			must(w.t, w.s.prepareLegacyOccurrence(ctx, w.legacy))
		}, []string{"occurrence.preparing", "occurrence.ready"}, nil},
		{"an occurrence waits for its sources", func(w *journalWorld) {
			o := storedOccurrence(w.t, w.s, w.legacy)
			o.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: o.Input}
			o.State = "waiting"
			must(w.t, w.s.saveOccurrence(o))
			w.prepared = o.ID
		}, []string{"occurrence.preparing"}, func(w *journalWorld, added []Event) {
			if added[0].Deadline == "" {
				w.t.Fatalf("%+v", added[0])
			}
		}},
		{"a source needs attention", func(w *journalWorld) {
			must(w.t, w.s.stopPreparation(storedOccurrence(w.t, w.s, w.prepared), "needs-attention", "A required source is unavailable."))
		}, []string{"occurrence.needs-attention"}, func(w *journalWorld, added []Event) {
			if added[0].Reason != "A required source is unavailable." {
				w.t.Fatalf("%+v", added[0])
			}
		}},
		{"the owner reconciles it", func(w *journalWorld) {
			_, e := w.s.reconcileOccurrence(w.prepared)
			must(w.t, e)
		}, []string{"occurrence.reconciled"}, func(w *journalWorld, added []Event) {
			if !sameActor(added[0].By, w.s.installation()) || string(added[0].From) != `"needs-attention"` {
				w.t.Fatalf("%+v", added[0])
			}
		}},
		{"the owner cancels it", func(w *journalWorld) {
			_, e := w.s.cancelOccurrence(ctx, w.prepared)
			must(w.t, e)
		}, []string{"occurrence.cancelled"}, nil},
		{"a release whose Runtime fails, and its job", func(w *journalWorld) {
			w.failing = journalRelease(w.t, w.s, failingRuntime(w.t, w.cfg.Dir))
			j, e := w.s.createJob("Failing", w.failing.ID)
			must(w.t, e)
			w.failingJob = j
		}, []string{"release.previewed", "job.created"}, nil},
		{"a submission is queued", func(w *journalWorld) {
			_, _, e := w.s.submit(w.failingJob.ID, "fails", journalInput())
			must(w.t, e)
		}, []string{"run.queued"}, func(w *journalWorld, added []Event) {
			if !sameActor(added[0].By, w.s.installation()) || added[0].Revision != nil {
				w.t.Fatalf("%+v", added[0])
			}
		}},
		{"the dispatcher evaluates it, and it fails", func(w *journalWorld) {
			_, e := w.s.dispatchNext(ctx)
			must(w.t, e)
		}, []string{"run.started", "run.failed"}, func(w *journalWorld, added []Event) {
			if added[1].Reason == "" {
				w.t.Fatalf("%+v", added[1])
			}
		}},
		{"an automatic run is queued after its queue time", func(w *journalWorld) {
			origin := &TriggerOrigin{OccurrenceID: "occ_late", TriggerID: w.schedule.ID, TriggerRevision: w.schedule.Revision, Kind: "schedule", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
			_, _, e := w.s.submitInternal(w.job.ID, "late", journalInput(), origin)
			must(w.t, e)
		}, []string{"run.queued"}, nil},
		{"the dispatcher finds it expired", func(w *journalWorld) {
			_, e := w.s.dispatchNext(ctx)
			must(w.t, e)
		}, []string{"run.expired"}, func(w *journalWorld, added []Event) {
			if e := added[0]; string(e.From) != `"queued"` || string(e.To) != `"failed"` || e.Reason == "" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a release whose Runtime waits, its job, and a submission", func(w *journalWorld) {
			w.waiting = journalRelease(w.t, w.s, waitingRuntime(w.t, w.cfg.Dir, filepath.Join(w.t.TempDir(), "pid")))
			j, e := w.s.createJob("Waiting", w.waiting.ID)
			must(w.t, e)
			w.waitingJob = j
			w.interrupted, _, e = w.s.submit(j.ID, "stopped", journalInput())
			must(w.t, e)
		}, []string{"release.previewed", "job.created", "run.queued"}, nil},
		{"Runner stops during its evaluation, and sees it stop", func(w *journalWorld) {
			stop, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { _, e := w.s.dispatchNext(stop); done <- e }()
			waitForRunState(w.t, w.s, w.interrupted.ID, "running")
			cancel()
			must(w.t, <-done)
			var e error
			w.interrupted, e = w.s.run(w.interrupted.ID)
			must(w.t, e)
		}, []string{"run.started", "run.interrupted"}, func(w *journalWorld, added []Event) {
			r := w.interrupted
			i := added[1].Interruption
			if r.State != "interrupted" || r.InterruptedAt == "" || r.InterruptedAt != r.FinishedAt || i == nil || !i.Seen || i.At != r.InterruptedAt || i.LastKnownRunning != "" || i.Restart != 0 {
				w.t.Fatalf("%+v %+v", r, i)
			}
		}},
		{"an event with a key no longer current is refused", func(w *journalWorld) {
			if got := serve(w.t, w.s, "POST", "/v1/triggers/"+w.event.ID+"/events", string(encode(delivery("stale"))), map[string]string{"X-Trigger-Token": w.oldToken}); got.Code != 401 {
				w.t.Fatal(got.Code, got.Body)
			}
		}, []string{"admission.refused"}, func(w *journalWorld, added []Event) {
			e := added[0]
			if e.Request != "deliver-event" || e.Status != 401 || e.Code != "invalid_trigger_token" || !sameActor(e.By, w.s.installation()) || e.CoalescedSeconds != 60 || e.Concerns != (EventConcerns{Job: w.job.ID, Release: w.release.ID, Trigger: w.event.ID}) {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"the same refusal within a minute is not written again", func(w *journalWorld) {
			if got := serve(w.t, w.s, "POST", "/v1/triggers/"+w.event.ID+"/events", string(encode(delivery("stale"))), map[string]string{"X-Trigger-Token": w.oldToken}); got.Code != 401 {
				w.t.Fatal(got.Code, got.Body)
			}
		}, nil, nil},
		{"an event with the current key to a paused trigger is refused, the credential's", func(w *journalWorld) {
			tr, _ := storedTrigger(w.t, w.s, w.event.ID)
			var e error
			w.event, _, e = w.s.setTriggerState(tr.ID, tr.Revision, true, false)
			must(w.t, e)
			if got := serve(w.t, w.s, "POST", "/v1/triggers/"+w.event.ID+"/events", string(encode(delivery("paused"))), map[string]string{"X-Trigger-Token": w.token}); got.Code != 409 {
				w.t.Fatal(got.Code, got.Body)
			}
		}, []string{"trigger.paused", "admission.refused"}, func(w *journalWorld, added []Event) {
			if e := added[1]; e.By.Kind != "trigger-credential" || string(e.By.KeyRevision) != "4" || e.Code != "trigger_paused" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a submission without an idempotency key is refused", func(w *journalWorld) {
			if got := serve(w.t, w.s, "POST", "/v1/jobs/"+w.job.ID+"/runs", `{"facts":{}}`, nil); got.Code != 400 {
				w.t.Fatal(got.Code, got.Body)
			}
		}, []string{"admission.refused"}, func(w *journalWorld, added []Event) {
			if e := added[0]; e.Request != "submit-run" || e.Code != "idempotency_key_required" {
				w.t.Fatalf("%+v", e)
			}
		}},
		{"a submission to a job the store does not hold is answered, and not journaled", func(w *journalWorld) {
			if got := serve(w.t, w.s, "POST", "/v1/jobs/job_unknown/runs", `{"facts":{}}`, map[string]string{"Idempotency-Key": "k"}); got.Code != 404 {
				w.t.Fatal(got.Code, got.Body)
			}
		}, nil, nil},
		{"a run Runner cannot record the end of is left running", func(w *journalWorld) {
			left, _, e := w.s.submit(w.waitingJob.ID, "left", journalInput())
			must(w.t, e)
			refuse(w.t, w.s, `CREATE TRIGGER journal_test_fault BEFORE UPDATE OF state ON runs WHEN NEW.state<>'running' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`)
			stop, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { _, e := w.s.dispatchNext(stop); done <- e }()
			waitForRunState(w.t, w.s, left.ID, "running")
			cancel()
			if e = <-done; e == nil {
				w.t.Fatal("the refused end was recorded")
			}
			refuse(w.t, w.s, "DROP TRIGGER journal_test_fault")
			w.left, e = w.s.run(left.ID)
			must(w.t, e)
		}, []string{"run.queued", "run.started"}, nil},
		{"an occurrence is left preparing", func(w *journalWorld) {
			tr, _ := storedTrigger(w.t, w.s, w.schedule.ID)
			o := newOccurrence(tr, w.job, time.Now())
			o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
			admit(w.t, w.s, tr, &o, "left-preparing")
			refuse(w.t, w.s, `CREATE TRIGGER journal_test_fault BEFORE UPDATE OF state ON occurrences WHEN OLD.state='preparing' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`)
			if e := w.s.prepareLegacyOccurrence(ctx, o.ID); e == nil {
				w.t.Fatal("the refused end was recorded")
			}
			refuse(w.t, w.s, "DROP TRIGGER journal_test_fault")
		}, []string{"occurrence.received", "occurrence.preparing"}, nil},
		{"Runner stops", func(w *journalWorld) {
			w.s.Close()
		}, []string{"runner.stopped"}, nil},
		{"Runner starts again and finds what was left running", func(w *journalWorld) {
			w.s = openJournal(w.t, w.cfg)
		}, []string{"runner.started", "run.interrupted", "occurrence.failed"}, func(w *journalWorld, added []Event) {
			started, run, occurrence := added[0], added[1], added[2]
			r, e := w.s.run(w.left.ID)
			must(w.t, e)
			if i := run.Interruption; i == nil || i.Seen || i.At != "" || i.LastKnownRunning != w.left.StartedAt || i.LastKnownRunning == started.At || i.Restart != started.Sequence {
				w.t.Fatalf("%+v %+v", run.Interruption, started)
			}
			if r.State != "interrupted" || r.InterruptedAt != "" || r.FinishedAt != started.At {
				w.t.Fatalf("%+v", r)
			}
			if i := occurrence.Interruption; i == nil || i.Seen || i.LastKnownRunning == "" || i.Restart != started.Sequence || string(occurrence.From) != `"preparing"` {
				w.t.Fatalf("%+v", occurrence)
			}
		}},
	}
}

// runJournalSteps makes every change of journalSteps and, when check is set,
// holds each to the entries it must add.
func runJournalSteps(t *testing.T, cfg Config, check bool) *journalWorld {
	t.Helper()
	w := &journalWorld{t: t, cfg: cfg}
	t.Cleanup(func() {
		if w.s != nil {
			w.s.Close()
		}
	})
	before := 0
	for _, step := range journalSteps() {
		step.do(w)
		entries := journalEntries(t, cfg)
		added := entries[before:]
		before = len(entries)
		if !check {
			continue
		}
		want := step.want
		if want == nil {
			want = []string{}
		}
		if got := kindsOf(added); !slices.Equal(got, want) {
			t.Fatalf("%s: wrote %v, want %v", step.name, got, want)
		}
		if step.check != nil {
			step.check(w, added)
		}
	}
	return w
}

// Every change of state Runner makes writes exactly one entry, of its kind,
// and a request that changes nothing writes none. Between them the steps make
// every kind of change.
func TestEveryChangeOfStateWritesExactlyOneEntry(t *testing.T) {
	cfg := journalConfig(t)
	runJournalSteps(t, cfg, true)
	covered := map[string]bool{}
	for _, e := range journalEntries(t, cfg) {
		covered[e.Kind] = true
	}
	for _, kind := range eventKinds {
		if !covered[kind] {
			t.Errorf("no step made a change of kind %s", kind)
		}
	}
	if len(covered) != len(eventKinds) {
		t.Errorf("entries of %d kinds, of %d", len(covered), len(eventKinds))
	}
}

// A change and its entry commit together or not at all: a state write that
// fails leaves no entry, and an entry write that fails leaves the state as it
// was. Each fault is the store's own refusal, through a trigger.
func TestAFailedChangeLeavesNeitherItsStateNorItsEntry(t *testing.T) {
	type transition struct {
		name, stateFault, kind string
		setup                  func(t *testing.T, s *Service) func() error
		unchanged              func(t *testing.T, s *Service) bool
	}
	var runID, occurrenceID, triggerID string
	transitions := []transition{
		{"a run starts", `CREATE TRIGGER fault BEFORE UPDATE OF state ON runs WHEN NEW.state='running' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`, "run.started",
			func(t *testing.T, s *Service) func() error {
				release := journalRelease(t, s, completingRuntime(t, s.cfg.Dir))
				j, e := s.createJob("Faults", release.ID)
				must(t, e)
				r, _, e := s.submit(j.ID, "fault", journalInput())
				must(t, e)
				runID = r.ID
				return func() error { _, e := s.dispatchNext(context.Background()); return e }
			},
			func(t *testing.T, s *Service) bool {
				r, e := s.run(runID)
				must(t, e)
				return r.State == "queued"
			}},
		{"an occurrence expires", `CREATE TRIGGER fault BEFORE UPDATE OF state ON occurrences WHEN NEW.state='expired' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`, "occurrence.expired",
			func(t *testing.T, s *Service) func() error {
				release := journalRelease(t, s, completingRuntime(t, s.cfg.Dir))
				c := eventTriggerConfig()
				j, e := s.createJobConfigured("Faults", release.ID, &c)
				must(t, e)
				tr, _ := storedTrigger(t, s, j.InitialTriggerID)
				_, token, e := s.setTriggerState(tr.ID, tr.Revision, false, true)
				must(t, e)
				o, _, e := s.event(tr.ID, token, delivery("fault"))
				must(t, e)
				occurrenceID = o.ID
				return func() error {
					return s.dispatchOccurrence(storedOccurrence(t, s, occurrenceID), time.Now().Add(2*time.Hour))
				}
			},
			func(t *testing.T, s *Service) bool { return storedOccurrence(t, s, occurrenceID).State == "accepted" }},
		{"a trigger is paused", `CREATE TRIGGER fault BEFORE UPDATE OF record ON triggers WHEN json_extract(NEW.record,'$.paused')=1 BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`, "trigger.paused",
			func(t *testing.T, s *Service) func() error {
				release := journalRelease(t, s, completingRuntime(t, s.cfg.Dir))
				c := eventTriggerConfig()
				j, e := s.createJobConfigured("Faults", release.ID, &c)
				must(t, e)
				tr, _ := storedTrigger(t, s, j.InitialTriggerID)
				tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
				must(t, e)
				triggerID = tr.ID
				return func() error { _, _, e := s.setTriggerState(tr.ID, tr.Revision, true, false); return e }
			},
			func(t *testing.T, s *Service) bool { tr, _ := storedTrigger(t, s, triggerID); return !tr.Paused }},
	}
	for _, c := range transitions {
		for _, fault := range []struct{ name, statement string }{
			{"the state write fails", c.stateFault},
			{"the entry write fails", `CREATE TRIGGER fault BEFORE INSERT ON events WHEN NEW.kind='` + c.kind + `' BEGIN SELECT RAISE(ABORT,'the test refuses this entry'); END`},
		} {
			t.Run(c.name+", "+fault.name, func(t *testing.T) {
				cfg := journalConfig(t)
				s := openJournal(t, cfg)
				defer s.Close()
				change := c.setup(t, s)
				before := len(journalEntries(t, cfg))
				refuse(t, s, fault.statement)
				if e := change(); e == nil {
					t.Fatal("the refused change was recorded")
				}
				if !c.unchanged(t, s) {
					t.Fatal("the state changed without its entry")
				}
				if added := journalEntries(t, cfg)[before:]; len(added) != 0 {
					t.Fatalf("an entry was written without its change: %v", kindsOf(added))
				}
				refuse(t, s, "DROP TRIGGER fault")
				if e := change(); e != nil {
					t.Fatal(e)
				}
				if added := kindsOf(journalEntries(t, cfg)[before:]); !slices.Contains(added, c.kind) {
					t.Fatalf("the change, once allowed, wrote %v", added)
				}
			})
		}
	}
}

// A process that is killed during an evaluation writes no runner.stopped, and
// leaves its run running. The next start records itself, then the
// interruption, with the window the stop lies in: from the run's start, the
// last time Runner recorded it alive, to the restart's own entry. Nothing the
// killed process wrote is lost, and nothing is written twice, then or at the
// start after.
func TestARestartAfterACrashLosesAndRepeatsNoEntry(t *testing.T) {
	if os.Getenv("JOURNAL_CRASH_CHILD") == "1" {
		cfg := Config{Dir: os.Getenv("JOURNAL_CRASH_DIR"), Runtime: os.Getenv("JOURNAL_CRASH_RUNTIME"), Workspace: "journal", Owner: "journal-owner", disableAutomation: true, disableDispatcher: true}
		s := openJournal(t, cfg)
		release := journalRelease(t, s, waitingRuntime(t, cfg.Dir, os.Getenv("JOURNAL_CRASH_PID")))
		j, e := s.createJob("Crash", release.ID)
		must(t, e)
		r, _, e := s.submit(j.ID, "crash", journalInput())
		must(t, e)
		go s.dispatchNext(context.Background())
		waitForRunState(t, s, r.ID, "running")
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if b, e := os.ReadFile(os.Getenv("JOURNAL_CRASH_PID")); e == nil && bytes.HasSuffix(b, []byte("\n")) {
				os.Exit(57)
			}
		}
		t.Fatal("the evaluation did not start")
	}
	cfg := journalConfig(t)
	pidFile := filepath.Join(t.TempDir(), "runtime.pid")
	child := exec.Command(os.Args[0], "-test.run=^TestARestartAfterACrashLosesAndRepeatsNoEntry$")
	child.Env = append(os.Environ(), "JOURNAL_CRASH_CHILD=1", "JOURNAL_CRASH_DIR="+cfg.Dir, "JOURNAL_CRASH_RUNTIME="+cfg.Runtime, "JOURNAL_CRASH_PID="+pidFile)
	out, e := child.CombinedOutput()
	if b, err := os.ReadFile(pidFile); err == nil {
		// The killed process's evaluation outlives it: end it by its own id.
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	}
	if code, ok := e.(*exec.ExitError); !ok || code.ExitCode() != 57 {
		t.Fatalf("the crash was not made: %v %s", e, out)
	}
	crashed := journalEntries(t, cfg)
	if got, want := kindsOf(crashed), []string{"journal.began", "runner.started", "release.previewed", "job.created", "run.queued", "run.started"}; !slices.Equal(got, want) {
		t.Fatalf("the killed process wrote %v, want %v", got, want)
	}
	s := openJournal(t, cfg)
	restarted := journalEntries(t, cfg)
	if !slices.EqualFunc(restarted[:len(crashed)], crashed, func(a, b Event) bool { return bytes.Equal(encode(a), encode(b)) }) {
		t.Fatal("an entry the killed process wrote changed")
	}
	added := restarted[len(crashed):]
	if got := kindsOf(added); !slices.Equal(got, []string{"runner.started", "run.interrupted"}) {
		t.Fatalf("the restart wrote %v", got)
	}
	started, interrupted := added[0], added[1]
	r, e := s.run(interrupted.Concerns.Run)
	must(t, e)
	i := interrupted.Interruption
	if i == nil || i.Seen || i.At != "" || i.LastKnownRunning != r.StartedAt || i.LastKnownRunning == started.At || i.Restart != started.Sequence {
		t.Fatalf("the interruption is not given its own window: %+v, run started %s, restart %+v", i, r.StartedAt, started)
	}
	if r.State != "interrupted" || r.InterruptedAt != "" || r.FinishedAt != started.At {
		t.Fatalf("%+v", r)
	}
	s.Close()
	s = openJournal(t, cfg)
	defer s.Close()
	again := journalEntries(t, cfg)
	if !slices.EqualFunc(again[:len(restarted)], restarted, func(a, b Event) bool { return bytes.Equal(encode(a), encode(b)) }) {
		t.Fatal("an entry changed at the next start")
	}
	if got := kindsOf(again[len(restarted):]); !slices.Equal(got, []string{"runner.stopped", "runner.started"}) {
		t.Fatalf("the next start wrote %v", got)
	}
	for n, e := range again {
		if e.Sequence != int64(n+1) {
			t.Fatalf("entry %d has sequence %d", n+1, e.Sequence)
		}
	}
}

// storeBeforeTheJournal is the schema Runner v0.5.0 kept a store in
// (store.go, run_chain.go and triggers_store.go at 0ba6bcd).
const storeBeforeTheJournal = `PRAGMA journal_mode=WAL;
 CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE briefs (subject TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE releases (id TEXT PRIMARY KEY, record TEXT NOT NULL);
 CREATE TABLE jobs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL);
 CREATE TABLE runs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL, caller TEXT NOT NULL, idem TEXT NOT NULL, request_digest TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL, UNIQUE(job_id,caller,idem));
 CREATE INDEX runs_state ON runs(state,seq);
 CREATE TABLE triggers(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,record TEXT NOT NULL,key_hash TEXT NOT NULL DEFAULT '',observed TEXT NOT NULL DEFAULT '',pending TEXT NOT NULL DEFAULT '',pending_at TEXT NOT NULL DEFAULT '');
 CREATE TABLE trigger_revisions(trigger_id TEXT NOT NULL,revision INTEGER NOT NULL,record TEXT NOT NULL,PRIMARY KEY(trigger_id,revision));
 CREATE TABLE occurrences(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,trigger_id TEXT NOT NULL,identity TEXT NOT NULL,payload_digest TEXT NOT NULL,state TEXT NOT NULL,record TEXT NOT NULL,UNIQUE(trigger_id,identity));
 CREATE TABLE occurrence_keys(occurrence_id TEXT PRIMARY KEY,key_hash TEXT NOT NULL);
 CREATE TABLE run_chain (sequence INTEGER PRIMARY KEY CHECK (sequence > 0), run TEXT UNIQUE NOT NULL, line BLOB NOT NULL);`

// earlierRunnerOpens is the check Runner v0.5.0 holds a store's metadata to
// as it opens it (store.go:136-147 at 0ba6bcd), and says whether it passes.
func earlierRunnerOpens(t *testing.T, db *sql.DB, cfg Config) bool {
	t.Helper()
	for k, v := range map[string]string{"schema": "1", "workspace": cfg.Workspace, "owner": cfg.Owner, "inputRoot": cfg.InputRoot} {
		if _, e := db.Exec("INSERT OR IGNORE INTO metadata VALUES (?,?)", k, v); e != nil {
			t.Fatal(e)
		}
		var held string
		if e := db.QueryRow("SELECT value FROM metadata WHERE key=?", k).Scan(&held); e != nil {
			t.Fatal(e)
		}
		if held != v {
			return false
		}
	}
	return true
}

// A store an earlier Runner kept begins its journal when this one opens it:
// the first entry says when, and what the store held, and nothing is made up
// for anything before it. Its schema is raised to "2", which the earlier
// Runner's own check refuses, so no Runner that writes no entries changes it
// after the journal began.
func TestAStoreFromBeforeTheJournalServesWhenItBeganAndNothingInvented(t *testing.T) {
	cfg := journalConfig(t)
	// Runner made the store's file private, as it does.
	must(t, os.WriteFile(filepath.Join(cfg.Dir, "runs.sqlite"), nil, 0600))
	db, e := sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	must(t, e)
	_, e = db.Exec(storeBeforeTheJournal)
	must(t, e)
	release := Release{SchemaVersion: "1", ID: "release_legacy", Pack: journalPack, PackDigest: digest([]byte(journalPack)), Tests: "not-run", CreatedAt: "2026-09-01T00:00:00Z"}
	job := Job{SchemaVersion: "1", ID: "job_legacy", Name: "Legacy", ReleaseID: release.ID, Revision: 1, CreatedAt: "2026-09-01T00:00:01Z", Workspace: cfg.Workspace, Owner: cfg.Owner}
	trigger := Trigger{ID: "trg_legacy", JobID: job.ID, Revision: 3, Authority: "local", Config: eventTriggerConfig(), Paused: true, HasKey: true}
	occurrence := Occurrence{ID: "occ_legacy", JobID: job.ID, ReleaseID: release.ID, JobRevision: 1, TriggerID: trigger.ID, TriggerRevision: 3, Kind: "event", ReceivedAt: "2026-09-01T00:00:02Z", ExpiresAt: "2026-09-01T01:00:02Z", State: "submitted", RunID: "run_legacy"}
	run := Run{SchemaVersion: "1", ID: "run_legacy", JobID: job.ID, ReleaseID: release.ID, Revision: 1, State: "failed", Input: journalInput(), CreatedAt: "2026-09-01T00:00:03Z", StartedAt: "2026-09-01T00:00:04Z", FinishedAt: "2026-09-01T00:00:05Z", RequestedBy: "trigger:trg_legacy", Attempt: 1, Problem: "an earlier failure"}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO metadata VALUES ('schema','1'),('workspace',?),('owner',?),('inputRoot','')", []any{cfg.Workspace, cfg.Owner}},
		{"INSERT INTO releases VALUES (?,?)", []any{release.ID, string(encode(release))}},
		{"INSERT INTO jobs(id,release_id,record) VALUES (?,?,?)", []any{job.ID, release.ID, string(encode(job))}},
		{"INSERT INTO triggers(id,job_id,record,key_hash) VALUES (?,?,?,?)", []any{trigger.ID, job.ID, string(encode(trigger)), digest([]byte("an earlier key"))}},
		{"INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES (?,?,?,?,?,?,?)", []any{occurrence.ID, job.ID, trigger.ID, "event:legacy", "", occurrence.State, string(encode(occurrence))}},
		{"INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", []any{run.ID, job.ID, "trigger:trg_legacy", "occ_legacy", "", run.State, string(encode(run))}},
	} {
		_, e = db.Exec(statement.query, statement.args...)
		must(t, e)
	}
	if !earlierRunnerOpens(t, db, cfg) {
		t.Fatal("the fixture is not a store the earlier Runner opens")
	}
	must(t, db.Close())

	s := openJournal(t, cfg)
	entries := journalEntries(t, cfg)
	if got := kindsOf(entries); !slices.Equal(got, []string{"journal.began", "runner.started"}) {
		t.Fatalf("the journal of an earlier store began with %v", got)
	}
	if held := entries[0].Held; held == nil || *held != (EventHeld{Jobs: 1, Triggers: 1, Occurrences: 1, Runs: 1}) {
		t.Fatalf("the journal does not say what the store held when it began: %+v", entries[0])
	}
	var schema string
	must(t, s.db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema))
	if schema != "2" {
		t.Fatal("the schema is", schema)
	}
	tr, _ := storedTrigger(t, s, trigger.ID)
	if !tr.HasKey || tr.keyRevision != nil {
		t.Fatal("a key issued before the journal has a revision:", tr.keyRevision)
	}
	got := serve(t, s, "GET", "/v1/jobs/"+job.ID+"/events", "", nil)
	var page EventPage
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil {
		t.Fatal(got.Code, got.Body)
	}
	if page.JournalBegan == "" || page.JournalBegan != entries[0].At || len(page.Items) != 0 || page.Next != 0 || page.More {
		t.Fatalf("a job from before the journal is served %s", got.Body)
	}
	s.Close()

	db, e = sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	must(t, e)
	defer db.Close()
	if earlierRunnerOpens(t, db, cfg) {
		t.Fatal("the earlier Runner's check passes a store the journal began in")
	}
}

// No entry holds a secret, a token, a digest of either, or a run's inputs: a
// key's issue and rotation, and a refusal of an earlier key, name revisions
// only.
func TestNoEntryHoldsASecret(t *testing.T) {
	cfg := journalConfig(t)
	w := runJournalSteps(t, cfg, false)
	var served bytes.Buffer
	for after := int64(0); ; {
		got := serve(t, w.s, "GET", "/v1/events?after="+strconv.FormatInt(after, 10), "", nil)
		var page EventPage
		if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil {
			t.Fatal(got.Code, got.Body)
		}
		served.Write(got.Body.Bytes())
		if !page.More {
			break
		}
		after = page.Next
	}
	rows, e := w.s.db.Query("SELECT * FROM events")
	must(t, e)
	columns, e := rows.Columns()
	must(t, e)
	var stored bytes.Buffer
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		must(t, rows.Scan(pointers...))
		for _, v := range values {
			stored.WriteString(strings.TrimSpace(string(encode(v))))
			if b, ok := v.([]byte); ok {
				stored.Write(b)
			}
			if s, ok := v.(string); ok {
				stored.WriteString(s)
			}
		}
	}
	must(t, rows.Err())
	rows.Close()
	if w.token == "" || w.oldToken == "" || w.token == w.oldToken {
		t.Fatal("the scenario issued no keys")
	}
	for name, secret := range map[string]string{
		"the trigger's key":        w.token,
		"its earlier key":          w.oldToken,
		"the key's digest":         strings.TrimPrefix(digest([]byte(w.token)), "sha256:"),
		"the earlier key's digest": strings.TrimPrefix(digest([]byte(w.oldToken)), "sha256:"),
		"the owner bearer":         journalBearer,
		"a run's inputs":           "facts-never-journaled-7f3a",
	} {
		for where, held := range map[string][]byte{"served": served.Bytes(), "stored": stored.Bytes()} {
			if bytes.Contains(held, []byte(secret)) {
				t.Errorf("the journal, as %s, holds %s", where, name)
			}
		}
	}
}

// An interruption Runner saw is served with the run, beside finishedAt, and
// in run lists; a verification export does not carry it, since version 2's
// readers decode strictly.
func TestAnInterruptionRunnerSawIsServedWithTheRunAndNotExported(t *testing.T) {
	cfg := journalConfig(t)
	w := runJournalSteps(t, cfg, false)
	r := w.interrupted
	got := serve(t, w.s, "GET", "/v1/runs/"+r.ID, "", nil)
	var served map[string]json.RawMessage
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &served) != nil {
		t.Fatal(got.Code, got.Body)
	}
	if string(served["interruptedAt"]) != string(encode(r.InterruptedAt)) || string(served["finishedAt"]) != string(encode(r.FinishedAt)) {
		t.Fatalf("the run is served %s", got.Body)
	}
	list := serve(t, w.s, "GET", "/v1/jobs/"+r.JobID+"/runs", "", nil)
	if list.Code != 200 || !bytes.Contains(list.Body.Bytes(), []byte(`"interruptedAt":`+string(encode(r.InterruptedAt)))) {
		t.Fatalf("the run list does not say when the run was interrupted: %s", list.Body)
	}
	release, e := w.s.release(r.ReleaseID)
	must(t, e)
	if exported := encode(verificationExport(release, r, 2)); bytes.Contains(exported, []byte("interruptedAt")) {
		t.Fatal("a version-2 export carries interruptedAt")
	}
	completed := serve(t, w.s, "GET", "/v1/runs/"+w.completed.ID, "", nil)
	if completed.Code != 200 || bytes.Contains(completed.Body.Bytes(), []byte("interruptedAt")) {
		t.Fatalf("a run that was not interrupted is served %s", completed.Body)
	}
}

// The routes read the cursor strictly, answer 404 for a job the store does
// not hold, and page forward.
func TestTheJournalRoutesReadTheirCursorAndJob(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	release := journalRelease(t, s, completingRuntime(t, cfg.Dir))
	j, e := s.createJob("Routes", release.ID)
	must(t, e)
	for _, c := range []struct {
		path   string
		status int
		code   string
	}{
		{"/v1/events?after=-1", 400, "invalid_cursor"},
		{"/v1/events?after=%2B1", 400, "invalid_cursor"},
		{"/v1/events?after=x", 400, "invalid_cursor"},
		{"/v1/events?after=1&after=2", 400, "invalid_cursor"},
		{"/v1/events?after=%zz", 400, "invalid_cursor"},
		{"/v1/events?job=" + j.ID + "&job=" + j.ID, 400, "invalid_filter"},
		{"/v1/events?job=job_unknown", 404, "not_found"},
		{"/v1/jobs/job_unknown/events", 404, "not_found"},
		{"/v1/jobs/" + j.ID + "/events?after=x", 400, "invalid_cursor"},
	} {
		got := serve(t, s, "GET", c.path, "", nil)
		if got.Code != c.status || !bytes.Contains(got.Body.Bytes(), []byte(`"code":"`+c.code+`"`)) {
			t.Errorf("%s: %d %s", c.path, got.Code, got.Body)
		}
	}
	var page EventPage
	got := serve(t, s, "GET", "/v1/events?after=999", "", nil)
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil || len(page.Items) != 0 || page.Next != 999 || page.More {
		t.Fatalf("past the end: %s", got.Body)
	}
	got = serve(t, s, "GET", "/v1/jobs/"+j.ID+"/events", "", nil)
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil || !slices.Equal(kindsOf(page.Items), []string{"release.previewed", "job.created"}) || page.Next != page.Items[1].Sequence {
		t.Fatalf("the job's entries: %s", got.Body)
	}
	if got := serve(t, s, "GET", "/v1/events", "", map[string]string{"Origin": "https://example.invalid"}); got.Code != 403 {
		t.Fatal("a browser origin was served", got.Code)
	}
	req := httptest.NewRequest("GET", "/v1/events", nil)
	w := httptest.NewRecorder()
	s.Handler(journalBearer).ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("served without the owner bearer", w.Code)
	}
}
