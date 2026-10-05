package runner

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// scheduleTrigger configures an enabled schedule trigger on the job, with the
// given constant input.
func (w *coverWorld) scheduleTrigger(value string) Trigger {
	w.t.Helper()
	c := scheduleTriggerConfig(time.Now())
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, c)
	must(w.t, e)
	tr, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(w.t, e)
	if value != "" {
		tr.Config.Input.Value = value
		must(w.t, w.s.writeTrigger(tr, "", false))
	}
	return tr
}

// fileTrigger configures an enabled file trigger on the job that reads
// input.json in the input root, holding first.
func (w *coverWorld) fileTrigger(first string) Trigger {
	w.t.Helper()
	must(w.t, os.WriteFile(filepath.Join(w.cfg.InputRoot, "input.json"), []byte(first), 0600))
	c := TriggerConfig{Name: "Changes", Kind: "file", WatchPath: "input.json", StableSeconds: 1, Input: &AutomaticInput{Kind: "input-file", Path: "input.json"}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, c)
	must(w.t, e)
	tr, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(w.t, e)
	return tr
}

// fileChanged writes then into the watched file, and ticks the trigger until
// the change is stable and admitted.
func (w *coverWorld) fileChanged(tr Trigger, then string) error {
	w.t.Helper()
	must(w.t, os.WriteFile(filepath.Join(w.cfg.InputRoot, "input.json"), []byte(then), 0600))
	at := time.Now()
	tr, hash := storedTrigger(w.t, w.s, tr.ID)
	if e := w.s.fileTick(tr, hash, at); e != nil {
		return e
	}
	tr, hash = storedTrigger(w.t, w.s, tr.ID)
	return w.s.fileTick(tr, hash, at.Add(2*time.Second))
}

// admitted admits an occurrence of tr to the job, as a trigger's own path
// does, after set adjusts it.
func (w *coverWorld) admitted(tr Trigger, set func(*Occurrence)) Occurrence {
	w.t.Helper()
	o := newOccurrence(tr, w.job, time.Now())
	o.Input = &Input{Facts: journalFacts}
	if set != nil {
		set(&o)
	}
	admit(w.t, w.s, tr, &o, "admitted-"+o.ID)
	return storedOccurrence(w.t, w.s, o.ID)
}

// waiting is an occurrence waiting for its sources, as a durable preparation
// leaves one.
func (w *coverWorld) waiting() Occurrence {
	w.t.Helper()
	o := w.admitted(w.scheduleTrigger(""), nil)
	o.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: o.Input}
	o.State = "waiting"
	must(w.t, w.s.saveOccurrence(o))
	return storedOccurrence(w.t, w.s, o.ID)
}

// leftRunning leaves a run running, as a process that stopped before it
// recorded the run's end does: its end is refused by the store.
func (w *coverWorld) leftRunning() Run {
	w.t.Helper()
	release := journalRelease(w.t, w.s, waitingRuntime(w.t, w.cfg.Dir, filepath.Join(w.t.TempDir(), "pid")))
	j, e := w.s.createJob("Left", release.ID)
	must(w.t, e)
	r, _, e := w.s.submit(j.ID, "left", journalInput())
	must(w.t, e)
	refuse(w.t, w.s, `CREATE TRIGGER left_running BEFORE UPDATE OF state ON runs WHEN NEW.state<>'running' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`)
	stop, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := w.s.dispatchNext(stop); done <- e }()
	waitForRunState(w.t, w.s, r.ID, "running")
	cancel()
	if <-done == nil {
		w.t.Fatal("the run's end was recorded")
	}
	refuse(w.t, w.s, "DROP TRIGGER left_running")
	r, e = w.s.run(r.ID)
	must(w.t, e)
	return r
}

// leftPreparing leaves a legacy preparation preparing, as a process that
// stopped during the acquisition does.
func (w *coverWorld) leftPreparing() Occurrence {
	w.t.Helper()
	o := w.admitted(w.scheduleTrigger(""), func(o *Occurrence) {
		o.Input = nil
		o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
	})
	refuse(w.t, w.s, `CREATE TRIGGER left_preparing BEFORE UPDATE OF state ON occurrences WHEN OLD.state='preparing' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`)
	if w.s.prepareLegacyOccurrence(context.Background(), o.ID) == nil {
		w.t.Fatal("the acquisition's end was recorded")
	}
	refuse(w.t, w.s, "DROP TRIGGER left_preparing")
	return storedOccurrence(w.t, w.s, o.ID)
}

// durable makes the job's release one whose source is acquired by a durable
// operation, served by a stand-in source worker, and admits an occurrence
// that will prepare it.
func (w *coverWorld) durable() (Occurrence, *asyncFixture) {
	w.t.Helper()
	input, p, key, _ := v2Fixture(w.t)
	input.Source.Mapping.Sources[0].Read.Rule = []byte(strings.ReplaceAll(string(vendorRule()), `"registry"`, `"intake-form"`))
	f := &asyncFixture{p: p, key: key, t: w.t, requests: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	w.t.Cleanup(server.Close)
	tokenFile := filepath.Join(w.t.TempDir(), "token")
	must(w.t, os.WriteFile(tokenFile, []byte(strings.Repeat("a", 64)), 0600))
	w.s.cfg.InputProfiles = []InputProfile{p}
	w.s.cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: "http://127.0.0.1:1", Durable: true, OperationsURL: server.URL, OperationsTokenFile: tokenFile}}
	w.release.InputMapping = &input.Source.Mapping
	w.release.InputProfiles = []InputProfile{p}
	_, e := w.s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(w.release)), w.release.ID)
	must(w.t, e)
	c := constantTrigger(time.Now())
	c.PreparationSeconds = 3600
	c.Input = &AutomaticInput{Kind: "mapped-sources", Case: input.Source.Case}
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, c)
	must(w.t, e)
	o := w.admitted(tr, func(o *Occurrence) { o.Input = nil; o.PendingInput = c.Input })
	return o, f
}

type changeSeen struct{ kind, from, to, by string }

func seen(entries []Event) []changeSeen {
	var all []changeSeen
	for _, e := range entries {
		from := string(e.From)
		if e.From == nil {
			from = "-"
		}
		to := strings.Trim(string(e.To), `"`)
		if e.To == nil {
			to = "-"
		}
		all = append(all, changeSeen{e.Kind, strings.Trim(from, `"`), to, e.By.Kind})
	}
	return all
}

// Every path by which Runner changes a record's state writes the entry of
// that change, from the state the store held to the state it wrote, by whom
// Runner can say: one row a path, so that a writer that leaves out any one
// entry fails here by name.
func TestEveryTransitionWritesItsEntry(t *testing.T) {
	ctx := context.Background()
	step := func(w *coverWorld, id string, until func(Occurrence) bool) {
		w.t.Helper()
		for i := 0; i < 8; i++ {
			if until(stepPreparation(w.t, w.s, id)) {
				return
			}
		}
		w.t.Fatal("the preparation did not get there:", storedOccurrence(w.t, w.s, id))
	}
	cases := []struct {
		name  string
		setup func(w *coverWorld) func()
		want  []changeSeen
	}{
		{"a trigger is configured", func(w *coverWorld) func() {
			return func() { _, e := w.s.configureTrigger(w.job.ID, "", 0, eventTriggerConfig()); must(w.t, e) }
		}, []changeSeen{{"trigger.configured", "null", "paused", "installation"}}},
		{"an enabled trigger is configured again", func(w *coverWorld) func() {
			tr, _ := w.eventTrigger("queue")
			return func() { _, e := w.s.configureTrigger(w.job.ID, tr.ID, tr.Revision, eventTriggerConfig()); must(w.t, e) }
		}, []changeSeen{{"trigger.configured", "enabled", "paused", "installation"}}},
		{"a job is created with its trigger", func(w *coverWorld) func() {
			r := journalRelease(w.t, w.s, w.release.RuntimeDigest)
			return func() {
				c := eventTriggerConfig()
				_, e := w.s.createJobConfigured("With trigger", r.ID, &c)
				must(w.t, e)
			}
		}, []changeSeen{{"job.created", "-", "-", "installation"}, {"trigger.configured", "null", "paused", "installation"}}},
		{"a preview stores a release", func(w *coverWorld) func() {
			return func() { journalRelease(w.t, w.s, w.release.RuntimeDigest) }
		}, []changeSeen{{"release.previewed", "-", "-", "installation"}}},
		{"an event trigger is resumed, its key issued", func(w *coverWorld) func() {
			tr, e := w.s.configureTrigger(w.job.ID, "", 0, eventTriggerConfig())
			must(w.t, e)
			return func() { _, _, e := w.s.setTriggerState(tr.ID, tr.Revision, false, true); must(w.t, e) }
		}, []changeSeen{{"trigger.resumed", "paused", "enabled", "installation"}}},
		{"a trigger is paused", func(w *coverWorld) func() {
			tr, _ := w.eventTrigger("queue")
			return func() { _, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false); must(w.t, e) }
		}, []changeSeen{{"trigger.paused", "enabled", "paused", "installation"}}},
		{"a paused trigger's key is rotated", func(w *coverWorld) func() {
			tr, _ := w.eventTrigger("queue")
			tr, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false)
			must(w.t, e)
			return func() { _, _, e := w.s.rotateTriggerKey(tr.ID, tr.Revision); must(w.t, e) }
		}, []changeSeen{{"trigger.key-rotated", "paused", "paused", "installation"}}},
		{"an event is admitted", func(w *coverWorld) func() {
			tr, key := w.eventTrigger("queue")
			return func() { _, _, e := w.s.event(tr.ID, key, delivery("one")); must(w.t, e) }
		}, []changeSeen{{"occurrence.received", "null", "accepted", "trigger-credential"}}},
		{"an event is skipped for overlap", func(w *coverWorld) func() {
			tr, key := w.eventTrigger("skip")
			_, _, e := w.s.event(tr.ID, key, delivery("one"))
			must(w.t, e)
			return func() { _, _, e := w.s.event(tr.ID, key, delivery("two")); must(w.t, e) }
		}, []changeSeen{{"occurrence.skipped", "null", "skipped", "trigger-credential"}}},
		{"a schedule falls due", func(w *coverWorld) func() {
			tr := w.scheduleTrigger("")
			at := time.Now().UTC().Truncate(time.Second)
			tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, "", false))
			return func() { must(w.t, w.s.scheduleTick(tr, "", at)) }
		}, []changeSeen{{"occurrence.received", "null", "accepted", "trigger"}}},
		{"a schedule's input cannot be read", func(w *coverWorld) func() {
			tr := w.scheduleTrigger("{")
			at := time.Now().UTC().Truncate(time.Second)
			tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, "", false))
			return func() { must(w.t, w.s.scheduleTick(tr, "", at)) }
		}, []changeSeen{{"occurrence.failed", "null", "failed", "trigger"}}},
		{"a schedule's missed time is skipped", func(w *coverWorld) func() {
			tr := w.scheduleTrigger("")
			at := time.Now().UTC().Truncate(time.Second)
			tr.NextAt = at.Add(-5 * time.Minute).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, "", false))
			return func() { must(w.t, w.s.scheduleTick(tr, "", at)) }
		}, []changeSeen{{"occurrence.skipped", "null", "skipped", "trigger"}}},
		{"a watched file changes", func(w *coverWorld) func() {
			tr := w.fileTrigger(`{"facts":{"version":1}}`)
			return func() { must(w.t, w.fileChanged(tr, `{"facts":{"version":2}}`)) }
		}, []changeSeen{{"occurrence.received", "null", "accepted", "trigger"}}},
		{"a watched file changes to inputs that cannot be read", func(w *coverWorld) func() {
			tr := w.fileTrigger(`{"facts":{"version":1}}`)
			return func() { must(w.t, w.fileChanged(tr, `{"facts":`)) }
		}, []changeSeen{{"occurrence.failed", "null", "failed", "trigger"}}},
		{"a cloud signal is admitted", func(w *coverWorld) func() {
			tr, conn := w.cloudTrigger("skip")
			return func() {
				if ack, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now())); !ack || e != nil {
					w.t.Fatal(ack, e)
				}
			}
		}, []changeSeen{{"occurrence.received", "null", "accepted", "trigger"}}},
		{"a cloud signal to a paused trigger is skipped", func(w *coverWorld) func() {
			tr, conn := w.cloudTrigger("skip")
			tr, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false)
			must(w.t, e)
			return func() {
				if ack, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now())); !ack || e != nil {
					w.t.Fatal(ack, e)
				}
			}
		}, []changeSeen{{"occurrence.skipped", "null", "skipped", "trigger"}}},
		{"a late cloud signal is skipped", func(w *coverWorld) func() {
			tr, conn := w.cloudTrigger("skip")
			return func() {
				if ack, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now().Add(-5*time.Minute))); !ack || e != nil {
					w.t.Fatal(ack, e)
				}
			}
		}, []changeSeen{{"occurrence.skipped", "null", "skipped", "trigger"}}},
		{"a cloud signal past its queue time expires", func(w *coverWorld) func() {
			tr, conn := w.cloudTrigger("latest")
			return func() {
				if ack, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now().Add(-2*time.Hour))); !ack || e != nil {
					w.t.Fatal(ack, e)
				}
			}
		}, []changeSeen{{"occurrence.expired", "null", "expired", "trigger"}}},
		{"an occurrence is submitted", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), nil)
			return func() { must(w.t, w.s.dispatchOccurrence(o, time.Now())) }
		}, []changeSeen{{"run.queued", "null", "queued", "trigger"}, {"occurrence.submitted", "accepted", "submitted", "runner"}}},
		{"an occurrence whose run was admitted before a crash is submitted", func(w *coverWorld) func() {
			tr := w.scheduleTrigger("")
			o := w.admitted(tr, nil)
			_, _, e := w.s.submitInternal(o.JobID, o.ID, *o.Input, &TriggerOrigin{OccurrenceID: o.ID, TriggerID: tr.ID, TriggerRevision: tr.Revision, Kind: "schedule"})
			must(w.t, e)
			return func() { must(w.t, w.s.dispatchOccurrence(o, time.Now())) }
		}, []changeSeen{{"occurrence.submitted", "accepted", "submitted", "runner"}}},
		{"an occurrence expires before it is submitted", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), nil)
			return func() { must(w.t, w.s.dispatchOccurrence(o, time.Now().Add(2*time.Hour))) }
		}, []changeSeen{{"occurrence.expired", "accepted", "expired", "runner"}}},
		{"an occurrence's run is refused", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), func(o *Occurrence) { o.Input = &Input{} })
			return func() { must(w.t, w.s.dispatchOccurrence(o, time.Now())) }
		}, []changeSeen{{"occurrence.failed", "accepted", "failed", "runner"}}},
		{"durable: an occurrence starts waiting for its sources", func(w *coverWorld) func() {
			o, _ := w.durable()
			return func() { step(w, o.ID, func(o Occurrence) bool { return o.State == "waiting" }) }
		}, []changeSeen{{"occurrence.preparing", "accepted", "waiting", "runner"}}},
		{"durable: its sources are acquired and it is ready", func(w *coverWorld) func() {
			o, f := w.durable()
			step(w, o.ID, func(o Occurrence) bool { return o.State == "waiting" })
			f.mu.Lock()
			f.ready = true
			f.mu.Unlock()
			return func() { step(w, o.ID, func(o Occurrence) bool { return o.State == "accepted" }) }
		}, []changeSeen{{"occurrence.ready", "waiting", "accepted", "runner"}}},
		{"durable: an answer from elsewhere needs attention", func(w *coverWorld) func() {
			o, f := w.durable()
			step(w, o.ID, func(o Occurrence) bool { return o.State == "waiting" })
			f.mu.Lock()
			f.ready, f.elsewhere = true, true
			f.mu.Unlock()
			return func() { step(w, o.ID, func(o Occurrence) bool { return o.State == "needs-attention" }) }
		}, []changeSeen{{"occurrence.needs-attention", "waiting", "needs-attention", "runner"}}},
		{"durable: its deadline passes", func(w *coverWorld) func() {
			o, _ := w.durable()
			step(w, o.ID, func(o Occurrence) bool { return o.State == "waiting" })
			o = storedOccurrence(w.t, w.s, o.ID)
			o.Preparation.Deadline = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			must(w.t, w.s.saveOccurrence(o))
			return func() { step(w, o.ID, func(o Occurrence) bool { return o.State == "expired" }) }
		}, []changeSeen{{"occurrence.expired", "waiting", "expired", "runner"}}},
		{"durable: an occurrence expires before it prepares", func(w *coverWorld) func() {
			o, _ := w.durable()
			o.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			_, e := w.s.db.Exec("UPDATE occurrences SET record=? WHERE id=?", string(encode(o)), o.ID)
			must(w.t, e)
			return func() { step(w, o.ID, func(o Occurrence) bool { return o.State == "expired" }) }
		}, []changeSeen{{"occurrence.expired", "accepted", "expired", "runner"}}},
		{"legacy: an occurrence acquires its inputs", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), func(o *Occurrence) {
				o.Input = nil
				o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
			})
			return func() { must(w.t, w.s.prepareLegacyOccurrence(ctx, o.ID)) }
		}, []changeSeen{{"occurrence.preparing", "accepted", "preparing", "runner"}, {"occurrence.ready", "preparing", "accepted", "runner"}}},
		{"legacy: an acquisition fails", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), func(o *Occurrence) {
				o.Input = nil
				o.PendingInput = &AutomaticInput{Kind: "constant", Value: "{"}
			})
			return func() { must(w.t, w.s.prepareLegacyOccurrence(ctx, o.ID)) }
		}, []changeSeen{{"occurrence.preparing", "accepted", "preparing", "runner"}, {"occurrence.failed", "preparing", "failed", "runner"}}},
		{"legacy: an occurrence expires before it prepares", func(w *coverWorld) func() {
			o := w.admitted(w.scheduleTrigger(""), func(o *Occurrence) {
				o.Input = nil
				o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
				o.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			})
			return func() { must(w.t, w.s.prepareLegacyOccurrence(ctx, o.ID)) }
		}, []changeSeen{{"occurrence.expired", "accepted", "expired", "runner"}}},
		{"a waiting occurrence needs attention", func(w *coverWorld) func() {
			o := w.waiting()
			return func() { must(w.t, w.s.stopPreparation(o, "needs-attention", "A required source is unavailable.")) }
		}, []changeSeen{{"occurrence.needs-attention", "waiting", "needs-attention", "runner"}}},
		{"the owner cancels a waiting occurrence", func(w *coverWorld) func() {
			o := w.waiting()
			return func() { _, e := w.s.cancelOccurrence(ctx, o.ID); must(w.t, e) }
		}, []changeSeen{{"occurrence.cancelled", "waiting", "cancelled", "installation"}}},
		{"the owner cancels one that needs attention", func(w *coverWorld) func() {
			o := w.waiting()
			must(w.t, w.s.stopPreparation(o, "needs-attention", "A required source is unavailable."))
			return func() { _, e := w.s.cancelOccurrence(ctx, o.ID); must(w.t, e) }
		}, []changeSeen{{"occurrence.cancelled", "needs-attention", "cancelled", "installation"}}},
		{"the owner reconciles it", func(w *coverWorld) func() {
			o := w.waiting()
			must(w.t, w.s.stopPreparation(o, "needs-attention", "A required source is unavailable."))
			return func() { _, e := w.s.reconcileOccurrence(o.ID); must(w.t, e) }
		}, []changeSeen{{"occurrence.reconciled", "needs-attention", "waiting", "installation"}}},
		{"a run is submitted", func(w *coverWorld) func() {
			return func() { _, _, e := w.s.submit(w.job.ID, "one", journalInput()); must(w.t, e) }
		}, []changeSeen{{"run.queued", "null", "queued", "installation"}}},
		{"a run is evaluated and completes", func(w *coverWorld) func() {
			_, _, e := w.s.submit(w.job.ID, "one", journalInput())
			must(w.t, e)
			return func() { _, e := w.s.dispatchNext(ctx); must(w.t, e) }
		}, []changeSeen{{"run.started", "queued", "running", "runner"}, {"run.completed", "running", "completed", "runner"}}},
		{"a run is evaluated and fails", func(w *coverWorld) func() {
			r := journalRelease(w.t, w.s, failingRuntime(w.t, w.cfg.Dir))
			j, e := w.s.createJob("Fails", r.ID)
			must(w.t, e)
			_, _, e = w.s.submit(j.ID, "one", journalInput())
			must(w.t, e)
			return func() { _, e := w.s.dispatchNext(ctx); must(w.t, e) }
		}, []changeSeen{{"run.started", "queued", "running", "runner"}, {"run.failed", "running", "failed", "runner"}}},
		{"an automatic run expires in the queue", func(w *coverWorld) func() {
			tr := w.scheduleTrigger("")
			_, _, e := w.s.submitInternal(w.job.ID, "late", journalInput(), &TriggerOrigin{OccurrenceID: "occ_late", TriggerID: tr.ID, TriggerRevision: tr.Revision, Kind: "schedule", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)})
			must(w.t, e)
			return func() { _, e := w.s.dispatchNext(ctx); must(w.t, e) }
		}, []changeSeen{{"run.expired", "queued", "failed", "runner"}}},
		{"a run Runner sees stop is interrupted", func(w *coverWorld) func() {
			r := journalRelease(w.t, w.s, waitingRuntime(w.t, w.cfg.Dir, filepath.Join(w.t.TempDir(), "pid")))
			j, e := w.s.createJob("Waits", r.ID)
			must(w.t, e)
			run, _, e := w.s.submit(j.ID, "one", journalInput())
			must(w.t, e)
			return func() {
				stop, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { _, e := w.s.dispatchNext(stop); done <- e }()
				waitForRunState(w.t, w.s, run.ID, "running")
				cancel()
				must(w.t, <-done)
			}
		}, []changeSeen{{"run.started", "queued", "running", "runner"}, {"run.interrupted", "running", "interrupted", "runner"}}},
		{"a restart finds a run running and a preparation preparing", func(w *coverWorld) func() {
			w.leftRunning()
			w.leftPreparing()
			return func() {
				w.s.Close()
				w.s = openJournal(w.t, w.cfg)
			}
		}, []changeSeen{{"runner.stopped", "-", "-", "runner"}, {"runner.started", "-", "-", "runner"}, {"run.interrupted", "running", "interrupted", "runner"}, {"occurrence.failed", "preparing", "failed", "runner"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newCoverWorld(t)
			change := c.setup(w)
			before := len(w.entries())
			change()
			if got := seen(w.entries()[before:]); !slices.Equal(got, c.want) {
				t.Fatalf("wrote %v, want %v", got, c.want)
			}
		})
	}
}

// For every writer, a change and its entry commit together or not at all:
// the store refusing the state write leaves no entry, and refusing the entry
// leaves the state as it was. Once the store allows them, the change and its
// entry are written.
func TestEveryWriterCommitsItsChangeWithItsEntry(t *testing.T) {
	ctx := context.Background()
	count := func(w *coverWorld, query string, args ...any) int {
		var n int
		must(w.t, w.s.db.QueryRow(query, args...).Scan(&n))
		return n
	}
	type writer struct {
		name, kind, stateFault string
		// setup prepares the change and returns it, and what shows the
		// store as it was before it.
		setup func(w *coverWorld) (func() error, func() bool)
	}
	writers := []writer{
		{"saveRelease", "release.previewed", `BEFORE INSERT ON releases`, func(w *coverWorld) (func() error, func() bool) {
			n := count(w, "SELECT count(*) FROM releases")
			r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), RuntimeDigest: w.release.RuntimeDigest, Tests: "not-run"}
			return func() error { return w.s.saveRelease(r) }, func() bool { return count(w, "SELECT count(*) FROM releases") == n }
		}},
		{"createJobConfigured: the job", "job.created", `BEFORE INSERT ON jobs`, func(w *coverWorld) (func() error, func() bool) {
			r := journalRelease(w.t, w.s, w.release.RuntimeDigest)
			c := eventTriggerConfig()
			return func() error { _, e := w.s.createJobConfigured("J", r.ID, &c); return e },
				func() bool { return count(w, "SELECT count(*) FROM jobs WHERE release_id=?", r.ID) == 0 }
		}},
		{"createJobConfigured: its trigger", "trigger.configured", `BEFORE INSERT ON triggers`, func(w *coverWorld) (func() error, func() bool) {
			r := journalRelease(w.t, w.s, w.release.RuntimeDigest)
			c := eventTriggerConfig()
			return func() error { _, e := w.s.createJobConfigured("J", r.ID, &c); return e },
				func() bool { return count(w, "SELECT count(*) FROM jobs WHERE release_id=?", r.ID) == 0 }
		}},
		{"configureTrigger", "trigger.configured", `BEFORE UPDATE OF record ON triggers`, func(w *coverWorld) (func() error, func() bool) {
			tr, _ := w.eventTrigger("queue")
			return func() error {
					_, e := w.s.configureTrigger(w.job.ID, tr.ID, tr.Revision, eventTriggerConfig())
					return e
				},
				func() bool {
					held, _ := storedTrigger(w.t, w.s, tr.ID)
					return held.Revision == tr.Revision && !held.Paused
				}
		}},
		{"setTriggerState", "trigger.paused", `BEFORE UPDATE OF record ON triggers`, func(w *coverWorld) (func() error, func() bool) {
			tr, _ := w.eventTrigger("queue")
			return func() error { _, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false); return e },
				func() bool { held, _ := storedTrigger(w.t, w.s, tr.ID); return !held.Paused }
		}},
		{"rotateTriggerKey", "trigger.key-rotated", `BEFORE UPDATE OF key_hash ON triggers`, func(w *coverWorld) (func() error, func() bool) {
			tr, _ := w.eventTrigger("queue")
			_, hash := storedTrigger(w.t, w.s, tr.ID)
			return func() error { _, _, e := w.s.rotateTriggerKey(tr.ID, tr.Revision); return e },
				func() bool { _, held := storedTrigger(w.t, w.s, tr.ID); return held == hash }
		}},
		{"admitOccurrence: an event", "occurrence.received", `BEFORE INSERT ON occurrences`, func(w *coverWorld) (func() error, func() bool) {
			tr, key := w.eventTrigger("queue")
			return func() error { _, _, e := w.s.event(tr.ID, key, delivery("one")); return e },
				func() bool { return count(w, "SELECT count(*) FROM occurrences") == 0 }
		}},
		{"admitOccurrence: a schedule", "occurrence.received", `BEFORE INSERT ON occurrences`, func(w *coverWorld) (func() error, func() bool) {
			tr := w.scheduleTrigger("")
			at := time.Now().UTC().Truncate(time.Second)
			tr.NextAt = at.Add(-time.Second).Format(time.RFC3339)
			must(w.t, w.s.writeTrigger(tr, "", false))
			return func() error { return w.s.scheduleTick(tr, "", at) },
				func() bool {
					held, _ := storedTrigger(w.t, w.s, tr.ID)
					return count(w, "SELECT count(*) FROM occurrences") == 0 && held.NextAt == tr.NextAt
				}
		}},
		{"admitOccurrence: a file change", "occurrence.received", `BEFORE INSERT ON occurrences`, func(w *coverWorld) (func() error, func() bool) {
			tr := w.fileTrigger(`{"facts":{"version":1}}`)
			n := 0
			return func() error {
					n++
					return w.fileChanged(tr, `{"facts":{"version":`+strings.Repeat("2", n)+`}}`)
				},
				func() bool { return count(w, "SELECT count(*) FROM occurrences") == 0 }
		}},
		{"admitOccurrence: a cloud signal", "occurrence.received", `BEFORE INSERT ON occurrences`, func(w *coverWorld) (func() error, func() bool) {
			tr, conn := w.cloudTrigger("skip")
			return func() error { _, e := w.s.receiveCloud(conn, cloudSignal(tr, time.Now())); return e },
				func() bool { return count(w, "SELECT count(*) FROM occurrences") == 0 }
		}},
		{"submitInternal", "run.queued", `BEFORE INSERT ON runs`, func(w *coverWorld) (func() error, func() bool) {
			n := 0
			return func() error { n++; _, _, e := w.s.submit(w.job.ID, strings.Repeat("k", n), journalInput()); return e },
				func() bool { return count(w, "SELECT count(*) FROM runs") == 0 }
		}},
		{"changeRun: a run starts", "run.started", `BEFORE UPDATE OF state ON runs WHEN NEW.state='running'`, func(w *coverWorld) (func() error, func() bool) {
			r, _, e := w.s.submit(w.job.ID, "one", journalInput())
			must(w.t, e)
			return func() error { _, e := w.s.dispatchNext(ctx); return e },
				func() bool { held, _ := w.s.run(r.ID); return held.State == "queued" }
		}},
		{"changeRun: a run expires in the queue", "run.expired", `BEFORE UPDATE OF state ON runs WHEN NEW.state='failed'`, func(w *coverWorld) (func() error, func() bool) {
			tr := w.scheduleTrigger("")
			r, _, e := w.s.submitInternal(w.job.ID, "late", journalInput(), &TriggerOrigin{OccurrenceID: "occ_late", TriggerID: tr.ID, TriggerRevision: tr.Revision, Kind: "schedule", ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)})
			must(w.t, e)
			return func() error { _, e := w.s.dispatchNext(ctx); return e },
				func() bool { held, _ := w.s.run(r.ID); return held.State == "queued" }
		}},
		{"finishRun", "run.completed", `BEFORE UPDATE OF state ON runs WHEN NEW.state='completed'`, func(w *coverWorld) (func() error, func() bool) {
			r, _, e := w.s.submit(w.job.ID, "one", journalInput())
			must(w.t, e)
			r.State, r.StartedAt, r.Attempt = "running", now(), 1
			must(w.t, w.s.changeRun(r))
			done := r
			done.State, done.FinishedAt, done.AuditBytes = "completed", now(), []byte(`{"recordVersion":"1"}`)
			return func() error { return w.s.finishRun(done) },
				func() bool {
					held, _ := w.s.run(r.ID)
					_, chained, _ := w.s.runEntrySequence(r.ID)
					return held.State == "running" && !chained
				}
		}},
		{"changeOccurrence: the dispatcher", "occurrence.expired", `BEFORE UPDATE OF state ON occurrences WHEN NEW.state='expired'`, func(w *coverWorld) (func() error, func() bool) {
			o := w.admitted(w.scheduleTrigger(""), nil)
			return func() error { return w.s.dispatchOccurrence(o, time.Now().Add(2*time.Hour)) },
				func() bool { return storedOccurrence(w.t, w.s, o.ID).State == "accepted" }
		}},
		{"changeOccurrence: a preparation's progress", "occurrence.needs-attention", `BEFORE UPDATE OF state ON occurrences WHEN NEW.state='needs-attention'`, func(w *coverWorld) (func() error, func() bool) {
			o := w.waiting()
			return func() error { return w.s.stopPreparation(o, "needs-attention", "A required source is unavailable.") },
				func() bool { return storedOccurrence(w.t, w.s, o.ID).State == "waiting" }
		}},
		{"changeOccurrence: the owner", "occurrence.cancelled", `BEFORE UPDATE OF state ON occurrences WHEN NEW.state='cancelled'`, func(w *coverWorld) (func() error, func() bool) {
			o := w.waiting()
			return func() error { _, e := w.s.cancelOccurrence(ctx, o.ID); return e },
				func() bool { return storedOccurrence(w.t, w.s, o.ID).State == "waiting" }
		}},
		{"changeOccurrence: the durable preparation", "occurrence.ready", `BEFORE UPDATE OF state ON occurrences WHEN NEW.state='accepted'`, func(w *coverWorld) (func() error, func() bool) {
			o, f := w.durable()
			stepPreparation(w.t, w.s, o.ID)
			f.mu.Lock()
			f.ready = true
			f.mu.Unlock()
			return func() error {
					for i := 0; i < 8; i++ {
						held := storedOccurrence(w.t, w.s, o.ID)
						if held.State == "accepted" {
							return nil
						}
						if held.Preparation != nil {
							held.Preparation.NextCheck = ""
							if e := w.s.saveOccurrence(held); e != nil {
								return e
							}
						}
						if e := w.s.prepareOccurrence(ctx); e != nil {
							return e
						}
					}
					return nil
				},
				func() bool { return storedOccurrence(w.t, w.s, o.ID).State == "waiting" }
		}},
	}
	for _, c := range writers {
		for _, fault := range []struct{ name, statement string }{
			{"the state write fails", `CREATE TRIGGER fault ` + c.stateFault + ` BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`},
			{"the entry write fails", `CREATE TRIGGER fault BEFORE INSERT ON events WHEN NEW.kind='` + c.kind + `' BEGIN SELECT RAISE(ABORT,'the test refuses this entry'); END`},
		} {
			t.Run(c.name+", "+fault.name, func(t *testing.T) {
				w := newCoverWorld(t)
				change, unchanged := c.setup(w)
				before := len(w.entries())
				refuse(t, w.s, fault.statement)
				if e := change(); e == nil {
					t.Fatal("the refused change was recorded")
				}
				if !unchanged() {
					t.Fatal("the state changed without its entry")
				}
				if added := w.entries()[before:]; len(added) != 0 {
					t.Fatalf("an entry was written without its change: %v", kindsOf(added))
				}
				refuse(t, w.s, "DROP TRIGGER fault")
				w.s.unhealthy.Store(false)
				must(t, change())
				if added := kindsOf(w.entries()[before:]); !slices.Contains(added, c.kind) {
					t.Fatalf("the change, once allowed, wrote %v", added)
				}
			})
		}
	}
}

// The restart is a writer too: the store refusing an interruption's state, or
// its entry, or the start's own entry, fails the start and leaves the run
// running with no entry; the next start marks it.
func TestARestartCommitsItsMarksWithTheirEntries(t *testing.T) {
	for name, fault := range map[string]string{
		"the run's state write fails":   `CREATE TRIGGER fault BEFORE UPDATE OF state ON runs WHEN NEW.state='interrupted' BEGIN SELECT RAISE(ABORT,'refused'); END`,
		"the run's entry write fails":   `CREATE TRIGGER fault BEFORE INSERT ON events WHEN NEW.kind='run.interrupted' BEGIN SELECT RAISE(ABORT,'refused'); END`,
		"the start's entry write fails": `CREATE TRIGGER fault BEFORE INSERT ON events WHEN NEW.kind='runner.started' BEGIN SELECT RAISE(ABORT,'refused'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			w := newCoverWorld(t)
			left := w.leftRunning()
			w.s.Close()
			w.s = nil
			before := len(w.entries())
			db, e := sql.Open("sqlite", filepath.Join(w.cfg.Dir, "runs.sqlite"))
			must(t, e)
			defer db.Close()
			_, e = db.Exec(fault)
			must(t, e)
			if s, e := Open(w.cfg); e == nil {
				s.Close()
				t.Fatal("the start was not refused")
			}
			var state string
			must(t, db.QueryRow("SELECT state FROM runs WHERE id=?", left.ID).Scan(&state))
			if added := w.entries()[before:]; state != "running" || len(added) != 0 {
				t.Fatalf("after a refused start: %s, %v", state, kindsOf(added))
			}
			_, e = db.Exec("DROP TRIGGER fault")
			must(t, e)
			w.s = openJournal(t, w.cfg)
			if got := kindsOf(w.entries()[before:]); !slices.Equal(got, []string{"runner.started", "run.interrupted"}) {
				t.Fatal(got)
			}
		})
	}
}
