package runner

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/cloudgoogle"
)

// coverWorld is a store with a job of a release whose Runtime completes, and
// an installation input root, for tests that each start from their own.
type coverWorld struct {
	t       *testing.T
	cfg     Config
	s       *Service
	release Release
	job     Job
}

func newCoverWorld(t *testing.T) *coverWorld {
	t.Helper()
	cfg := journalConfig(t)
	root, e := filepath.EvalSymlinks(t.TempDir())
	must(t, e)
	cfg.InputRoot = root
	w := &coverWorld{t: t, cfg: cfg}
	w.s = openJournal(t, cfg)
	t.Cleanup(func() {
		if w.s != nil {
			w.s.Close()
		}
	})
	w.release = journalRelease(t, w.s, completingRuntime(t, cfg.Dir))
	w.job, e = w.s.createJob("Coverage", w.release.ID)
	must(t, e)
	return w
}

func (w *coverWorld) entries() []Event { return journalEntries(w.t, w.cfg) }

// eventTrigger configures an event trigger on the job, enabled, and returns
// it with its key.
func (w *coverWorld) eventTrigger(overlap string) (Trigger, string) {
	w.t.Helper()
	c := eventTriggerConfig()
	c.Overlap = overlap
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, c)
	must(w.t, e)
	tr, key, e := w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(w.t, e)
	return tr, key
}

// cloudTrigger installs a cloud connection and binds an enabled cloud
// trigger on the job to it.
func (w *coverWorld) cloudTrigger(missed string) (Trigger, CloudConnection) {
	w.t.Helper()
	conn := CloudConnection{ID: "google", Subscription: "projects/example-project/subscriptions/desk-inbox", CredentialsFile: filepath.Join(w.t.TempDir(), "adc.json")}
	w.s.cfg.CloudConnections = []CloudConnection{conn}
	c := scheduleTriggerConfig(time.Now())
	c.Kind, c.Schedule, c.Missed = "cloud", nil, missed
	c.Cloud = &CloudBinding{Connection: conn.ID, Subscription: conn.Subscription, Job: "projects/example-project/locations/us-central1/jobs/daily"}
	tr, e := w.s.configureTrigger(w.job.ID, "", 0, c)
	must(w.t, e)
	tr, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
	must(w.t, e)
	return tr, conn
}

func signalData(raw []byte) cloudgoogle.Delivery {
	var d cloudgoogle.Delivery
	d.Message.Data = base64.StdEncoding.EncodeToString(raw)
	return d
}

// Every branch on which an admission route refuses a request that names
// something the store holds writes exactly one admission.refused, naming the
// request, its answer and who made it; a request that names nothing held, or
// is refused because Runner or its store failed, writes none.
func TestEveryAdmissionRefusalIsJournaledOnce(t *testing.T) {
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	type want struct{ request, code, by string }
	type refusal struct {
		name   string
		status int
		want   *want
		do     func(w *coverWorld) int
	}
	post := func(w *coverWorld, path, body string, header map[string]string) int {
		return serve(w.t, w.s, "POST", path, body, header).Code
	}
	submit := func(body string, key string) func(w *coverWorld) int {
		return func(w *coverWorld) int {
			h := map[string]string{}
			if key != "" {
				h["Idempotency-Key"] = key
			}
			return post(w, "/v1/jobs/"+w.job.ID+"/runs", body, h)
		}
	}
	deliver := func(setup func(w *coverWorld, tr Trigger, key string), body func(key string) string, token func(key string) string) func(w *coverWorld) int {
		return func(w *coverWorld) int {
			tr, key := w.eventTrigger("queue")
			if setup != nil {
				setup(w, tr, key)
			}
			return post(w, "/v1/triggers/"+tr.ID+"/events", body(key), map[string]string{"X-Trigger-Token": token(key)})
		}
	}
	valid := func(id string) func(string) string {
		return func(string) string {
			return string(encode(EventDelivery{ID: id, OccurredAt: now(), Input: journalInput()}))
		}
	}
	theKey := func(key string) string { return key }
	create := func(body func(w *coverWorld) string) func(w *coverWorld) int {
		return func(w *coverWorld) int { return post(w, "/v1/jobs", body(w), nil) }
	}
	otherRelease := func(w *coverWorld, tests string) Release {
		r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", RuntimeDigest: w.release.RuntimeDigest, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Tests: tests}
		must(w.t, w.s.saveRelease(r))
		return r
	}
	signal := func(d func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery)) func(w *coverWorld) int {
		return func(w *coverWorld) int {
			c, delivery := d(w)
			if ack, e := w.s.receiveCloud(c, delivery); !ack || e == nil {
				w.t.Fatal("the signal was not discarded", ack, e)
			}
			return 0
		}
	}
	cases := []refusal{
		{"submit-run: no idempotency key", 400, &want{"submit-run", "idempotency_key_required", "installation"}, submit(`{"facts":{}}`, "")},
		{"submit-run: a body that is not JSON", 400, &want{"submit-run", "invalid_request", "installation"}, submit(`{`, "k")},
		{"submit-run: an unsupported member", 400, &want{"submit-run", "invalid_request", "installation"}, submit(`{"facts":{},"unexpected":true}`, "k")},
		{"submit-run: no facts", 422, &want{"submit-run", "invalid_input", "installation"}, submit(`{"evidence":{"intake-form":"present"}}`, "k")},
		{"submit-run: evidence of another value", 422, &want{"submit-run", "invalid_evidence", "installation"}, submit(`{"facts":{},"evidence":{"intake-form":"maybe"}}`, "k")},
		{"submit-run: a mapping the job does not have", 422, &want{"submit-run", "mapping_mismatch", "installation"}, submit(`{"source":{"mapping":{"version":2}}}`, "k")},
		{"submit-run: a key used with other inputs", 409, &want{"submit-run", "idempotency_conflict", "installation"}, func(w *coverWorld) int {
			_, _, e := w.s.submit(w.job.ID, "k", journalInput())
			must(w.t, e)
			return submit(`{"facts":{"other":true}}`, "k")(w)
		}},
		{"submit-run: the queue is full", 429, &want{"submit-run", "queue_full", "installation"}, func(w *coverWorld) int {
			for i := 0; i < queueLimit; i++ {
				_, _, e := w.s.submit(w.job.ID, "fill-"+strconv.Itoa(i), journalInput())
				must(w.t, e)
			}
			return submit(`{"facts":{}}`, "full")(w)
		}},
		{"submit-run: a job the store does not hold", 404, nil, func(w *coverWorld) int {
			return post(w, "/v1/jobs/job_unknown/runs", `{"facts":{}}`, map[string]string{"Idempotency-Key": "k"})
		}},
		{"submit-run: the dispatcher stopped", 503, nil, func(w *coverWorld) int {
			w.s.unhealthy.Store(true)
			return submit(`{"facts":{}}`, "k")(w)
		}},
		{"deliver-event: a body that is not JSON", 400, &want{"deliver-event", "invalid_request", "installation"}, deliver(nil, func(string) string { return "{" }, theKey)},
		{"deliver-event: a key that is not the trigger's", 401, &want{"deliver-event", "invalid_trigger_token", "installation"}, deliver(nil, valid("e"), func(string) string { return strings.Repeat("0", 64) })},
		{"deliver-event: no event ID", 422, &want{"deliver-event", "invalid_event", "trigger-credential"}, deliver(nil, valid(""), theKey)},
		{"deliver-event: an event ID with another payload", 409, &want{"deliver-event", "event_conflict", "trigger-credential"}, deliver(func(w *coverWorld, tr Trigger, key string) {
			_, _, e := w.s.event(tr.ID, key, EventDelivery{ID: "e", OccurredAt: now(), Input: Input{Facts: json.RawMessage(`{"other":true}`)}})
			must(w.t, e)
		}, valid("e"), theKey)},
		{"deliver-event: a paused trigger", 409, &want{"deliver-event", "trigger_paused", "trigger-credential"}, deliver(func(w *coverWorld, tr Trigger, key string) {
			_, _, e := w.s.setTriggerState(tr.ID, tr.Revision, true, false)
			must(w.t, e)
		}, valid("e"), theKey)},
		{"deliver-event: an event too old", 422, &want{"deliver-event", "event_expired", "trigger-credential"}, deliver(nil, func(string) string {
			return string(encode(EventDelivery{ID: "e", OccurredAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Input: journalInput()}))
		}, theKey)},
		{"deliver-event: computed preparation", 422, &want{"deliver-event", "invalid_event", "trigger-credential"}, deliver(nil, func(string) string {
			return `{"id":"e","occurredAt":"` + now() + `","input":{"facts":{},"preparation":{}}}`
		}, theKey)},
		{"deliver-event: a mapping the job does not have", 422, &want{"deliver-event", "mapping_mismatch", "trigger-credential"}, deliver(nil, func(string) string {
			return `{"id":"e","occurredAt":"` + now() + `","input":{"source":{"mapping":{"version":2}}}}`
		}, theKey)},
		{"deliver-event: inputs that are not valid", 422, &want{"deliver-event", "invalid_input", "trigger-credential"}, deliver(nil, func(string) string {
			return `{"id":"e","occurredAt":"` + now() + `","input":{"evidence":{}}}`
		}, theKey)},
		{"deliver-event: Runner is unavailable", 503, nil, deliver(func(w *coverWorld, tr Trigger, key string) { w.s.unhealthy.Store(true) }, valid("e"), theKey)},
		{"deliver-event: a trigger the store does not hold", 401, nil, func(w *coverWorld) int {
			return post(w, "/v1/triggers/trg_unknown/events", valid("e")(""), map[string]string{"X-Trigger-Token": strings.Repeat("0", 64)})
		}},
		{"create-job: an unsupported member", 400, &want{"create-job", "invalid_request", "installation"}, create(func(w *coverWorld) string {
			return `{"name":"x","releaseId":"` + otherRelease(w, "not-run").ID + `","reviewed":true,"unexpected":true}`
		})},
		{"create-job: not reviewed", 422, &want{"create-job", "review_required", "installation"}, create(func(w *coverWorld) string {
			return `{"name":"x","releaseId":"` + otherRelease(w, "not-run").ID + `","reviewed":false}`
		})},
		{"create-job: a release whose tests did not pass", 409, &want{"create-job", "release_not_ready", "installation"}, create(func(w *coverWorld) string {
			return `{"name":"x","releaseId":"` + otherRelease(w, "failed").ID + `","reviewed":true}`
		})},
		{"create-job: a release another job used", 409, &want{"create-job", "release_already_used", "installation"}, create(func(w *coverWorld) string {
			return `{"name":"another name","releaseId":"` + w.release.ID + `","reviewed":true}`
		})},
		{"create-job: an untested release where tests are required", 409, &want{"create-job", "release_untested", "installation"}, create(func(w *coverWorld) string {
			w.s.cfg.RequireTestedReleases = true
			return `{"name":"x","releaseId":"` + otherRelease(w, "not-run").ID + `","reviewed":true}`
		})},
		{"create-job: an invalid trigger", 422, &want{"create-job", "invalid_trigger", "installation"}, create(func(w *coverWorld) string {
			c := eventTriggerConfig()
			c.Name = ""
			return `{"name":"x","releaseId":"` + otherRelease(w, "not-run").ID + `","reviewed":true,"trigger":` + string(encode(c)) + `}`
		})},
		{"create-job: a release the store does not hold", 404, nil, create(func(*coverWorld) string {
			return `{"name":"x","releaseId":"release_unknown","reviewed":true}`
		})},
		{"create-job: a body that names no release", 400, nil, create(func(*coverWorld) string { return `{` })},
		{"cloud-signal: over its size limit", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			_, conn := w.cloudTrigger("skip")
			var d cloudgoogle.Delivery
			d.Message.Data = strings.Repeat("A", 5000)
			return conn, d
		})},
		{"cloud-signal: an envelope that is not base64", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			_, conn := w.cloudTrigger("skip")
			var d cloudgoogle.Delivery
			d.Message.Data = "!"
			return conn, d
		})},
		{"cloud-signal: an identity that is not a signal", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			_, conn := w.cloudTrigger("skip")
			return conn, signalData([]byte(`{"version":2}`))
		})},
		{"cloud-signal: a time in the future", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			tr, conn := w.cloudTrigger("skip")
			return conn, cloudSignal(tr, time.Now().Add(time.Hour))
		})},
		{"cloud-signal: a job no trigger is bound to", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			_, conn := w.cloudTrigger("skip")
			return conn, signalData(encode(cloudgoogle.Signal{Version: 1, Job: "projects/example-project/locations/us-central1/jobs/unbound", ScheduledAt: time.Now().UTC().Format(time.RFC3339Nano)}))
		})},
		{"cloud-signal: a replaced connection", 0, &want{"cloud-signal", "", "cloud-connection"}, signal(func(w *coverWorld) (CloudConnection, cloudgoogle.Delivery) {
			tr, conn := w.cloudTrigger("skip")
			conn.Subscription = "projects/example-project/subscriptions/replaced"
			return conn, cloudSignal(tr, time.Now())
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newCoverWorld(t)
			before := len(w.entries())
			status := c.do(w)
			if status != c.status {
				t.Fatalf("answered %d, want %d", status, c.status)
			}
			var refused []Event
			for _, e := range w.entries()[before:] {
				if e.Kind == "admission.refused" {
					refused = append(refused, e)
				}
			}
			if c.want == nil {
				if len(refused) != 0 {
					t.Fatalf("journaled %+v", refused)
				}
				return
			}
			if len(refused) != 1 {
				t.Fatalf("%d refusals journaled", len(refused))
			}
			e := refused[0]
			if e.Request != c.want.request || e.Code != c.want.code || e.Status != c.status || e.By.Kind != c.want.by || e.CoalescedSeconds != 60 {
				t.Fatalf("%+v", e)
			}
			if c.want.request == "cloud-signal" && (e.Reason == "" || e.By.Connection != "google") {
				t.Fatalf("%+v", e)
			}
		})
	}
}

// The store-wide route for a job serves what the job's route does, its
// release's preview included, and Runner's own entries besides.
func TestTheStoreWideRouteForAJobHasItsReleasesEntries(t *testing.T) {
	w := newCoverWorld(t)
	var page EventPage
	got := serve(t, w.s, "GET", "/v1/events?job="+w.job.ID, "", nil)
	must(t, json.Unmarshal(got.Body.Bytes(), &page))
	if kinds := kindsOf(page.Items); got.Code != 200 || !slices.Equal(kinds, []string{"journal.began", "runner.started", "release.previewed", "job.created"}) {
		t.Fatalf("%d %v", got.Code, kinds)
	}
	if page.Items[2].Concerns != (EventConcerns{Release: w.release.ID}) {
		t.Fatalf("%+v", page.Items[2])
	}
}

// A refusal of a delivery whose credential was authenticated is the
// credential's as it was then: a rotation of the key before the refusal is
// recorded does not make it the installation's, nor the next key's.
func TestARefusalIsTheCredentialsThatWasAuthenticated(t *testing.T) {
	w := newCoverWorld(t)
	tr, key := w.eventTrigger("queue")
	if tr.keyRevision == nil || *tr.keyRevision != 2 {
		t.Fatal("the key was not issued at revision 2:", tr.keyRevision)
	}
	_, _, refusal := w.s.event(tr.ID, key, EventDelivery{ID: "", OccurredAt: time.Now().UTC().Format(time.RFC3339), Input: journalInput()})
	if refusal == nil {
		t.Fatal("the delivery was not refused")
	}
	rotated, _, e := w.s.rotateTriggerKey(tr.ID, tr.Revision)
	must(t, e)
	if *rotated.keyRevision != 3 {
		t.Fatal(*rotated.keyRevision)
	}
	before := len(w.entries())
	w.s.refusedEvent(tr.ID, refusal)
	added := w.entries()[before:]
	if len(added) != 1 || added[0].By.Kind != "trigger-credential" || string(added[0].By.KeyRevision) != "2" || added[0].Code != "invalid_event" {
		t.Fatalf("%+v", added)
	}
}

// A migration is all or nothing: one that fails, at its last statement or at
// its entry, leaves a store from before the journal as it was, with no journal
// table, no key revision column and its schema "1", and the next start that
// succeeds migrates it whole.
func TestAMigrationThatFailsLeavesTheStoreAsItWas(t *testing.T) {
	for name, fault := range map[string]string{
		"the schema's raise fails": `CREATE TRIGGER fault BEFORE INSERT ON metadata WHEN NEW.key='schema' AND NEW.value='2' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`,
		"the first entry fails":    `CREATE TABLE events (unrelated TEXT)`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := journalConfig(t)
			path := filepath.Join(cfg.Dir, "runs.sqlite")
			must(t, os.WriteFile(path, nil, 0600))
			db, e := sql.Open("sqlite", path)
			must(t, e)
			_, e = db.Exec(storeBeforeTheJournal)
			must(t, e)
			_, e = db.Exec("INSERT INTO metadata VALUES ('schema','1'),('workspace',?),('owner',?),('inputRoot','')", cfg.Workspace, cfg.Owner)
			must(t, e)
			_, e = db.Exec(fault)
			must(t, e)
			must(t, db.Close())
			if s, e := Open(cfg); e == nil {
				s.Close()
				t.Fatal("the store was opened")
			}
			db, e = sql.Open("sqlite", path)
			must(t, e)
			defer db.Close()
			var schema string
			must(t, db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema))
			var columns, journal int
			must(t, db.QueryRow("SELECT count(*) FROM pragma_table_info('triggers') WHERE name='key_revision'").Scan(&columns))
			must(t, db.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name='kind'").Scan(&journal))
			if schema != "1" || columns != 0 || journal != 0 {
				t.Fatalf("after a failed migration: schema %s, key_revision %d, journal table %d", schema, columns, journal)
			}
			_, e = db.Exec("DROP TRIGGER IF EXISTS fault")
			must(t, e)
			_, e = db.Exec("DROP TABLE IF EXISTS events")
			must(t, e)
			s := openJournal(t, cfg)
			defer s.Close()
			if got := kindsOf(journalEntries(t, cfg)); !slices.Equal(got, []string{"journal.began", "runner.started"}) {
				t.Fatal(got)
			}
		})
	}
}
