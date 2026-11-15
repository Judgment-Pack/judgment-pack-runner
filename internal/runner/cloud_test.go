package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/cloudgoogle"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cloudFixture(t *testing.T) (*Service, Config, Job, Trigger, CloudConnection) {
	s, cfg, j, _ := automaticFixture(t)
	conn := CloudConnection{ID: "google", Subscription: "projects/example-project/subscriptions/desk-inbox", CredentialsFile: "/tmp/test-adc.json"}
	cfg.CloudConnections = []CloudConnection{conn}
	s.cfg.CloudConnections = cfg.CloudConnections
	c := constantTrigger(time.Now())
	c.Kind = "cloud"
	c.Schedule = nil
	c.Cloud = &CloudBinding{Connection: conn.ID, Subscription: conn.Subscription, Job: "projects/example-project/locations/us-central1/jobs/daily"}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	return s, cfg, j, tr, conn
}
func cloudSignal(tr Trigger, at time.Time) cloudgoogle.Delivery {
	var d cloudgoogle.Delivery
	d.AckID = "ack"
	d.Message.ID = "transport-1"
	d.Message.Data = base64.StdEncoding.EncodeToString(encode(cloudgoogle.Signal{Version: 1, Job: tr.Config.Cloud.Job, ScheduledAt: at.UTC().Format(time.RFC3339Nano)}))
	return d
}
func TestCloudRetriesExpiryAndRestart(t *testing.T) {
	s, cfg, _, tr, conn := cloudFixture(t)
	var e error
	tr, _, e = s.setTriggerState(tr.ID, tr.Revision, false, true)
	if e != nil {
		t.Fatal(e)
	}
	d := cloudSignal(tr, time.Now())
	if ack, e := s.receiveCloud(conn, d); !ack || e != nil {
		t.Fatal(ack, e)
	}
	o := latestOccurrence(t, s)
	if o.State != "accepted" || o.PendingInput == nil || o.Input != nil {
		t.Fatal(o)
	}
	if e = s.prepareOccurrence(context.Background()); e != nil {
		t.Fatal(e)
	}
	o = latestOccurrence(t, s)
	if o.State != "accepted" || o.PendingInput != nil || o.Input == nil {
		t.Fatal(o)
	}
	if e = s.dispatchOccurrence(o, time.Now()); e != nil {
		t.Fatal(e)
	}
	d.Message.ID = "scheduler-republish-different-pubsub-id"
	if ack, e := s.receiveCloud(conn, d); !ack || e != nil {
		t.Fatal(ack, e)
	}
	if countRows(t, s, "occurrences") != 1 || countRows(t, s, "runs") != 1 {
		t.Fatal("duplicate run")
	}
	s.Close()
	again, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if ack, e := again.receiveCloud(conn, d); !ack || e != nil {
		t.Fatal(ack, e)
	}
	if countRows(t, again, "occurrences") != 1 {
		t.Fatal("replayed after restart")
	}
	if ack, e := again.receiveCloud(conn, cloudSignal(tr, time.Now().Add(-2*time.Hour))); !ack || e != nil {
		t.Fatal(ack, e)
	}
	if latestOccurrence(t, again).State != "expired" {
		t.Fatal("stale signal admitted")
	}
}
func TestCloudPausedUnboundAndAdmissionFailure(t *testing.T) {
	s, _, _, tr, conn := cloudFixture(t)
	if ack, e := s.receiveCloud(conn, cloudSignal(tr, time.Now())); !ack || e != nil {
		t.Fatal(ack, e)
	}
	if o := latestOccurrence(t, s); o.State != "skipped" || o.Reason != "trigger-paused" || o.PendingInput != nil {
		t.Fatal(o)
	}
	if _, e := s.configureTrigger(tr.JobID, "", 0, tr.Config); e == nil {
		t.Fatal("duplicate cloud binding")
	}
	for _, v := range []string{`{"version":1,"job":"wrong"}`, `{"version":1,"job":"` + tr.Config.Cloud.Job + `","scheduledAt":"bad","facts":{}}`} {
		d := cloudSignal(tr, time.Now())
		d.Message.Data = base64.StdEncoding.EncodeToString([]byte(v))
		if ack, e := s.receiveCloud(conn, d); !ack || e == nil {
			t.Fatal("poison not refused")
		}
	}
	s.unhealthy.Store(true)
	if ack, _ := s.receiveCloud(conn, cloudSignal(tr, time.Now().Add(time.Second))); ack {
		t.Fatal("acknowledged without admission")
	}
	s.unhealthy.Store(false)
	_, e := sqliteDB(s).Exec("DROP TABLE occurrences")
	if e != nil {
		t.Fatal(e)
	}
	if ack, _ := s.receiveCloud(conn, cloudSignal(tr, time.Now())); ack {
		t.Fatal("acknowledged failed transaction")
	}
}

// usedCloudBindingRefused configures a cloud trigger on job, under scope,
// through its route on sv, then asks POST /v1/jobs for a job with the same
// binding from a new release under each of scopes ("" for none): each is
// refused as the trigger route refuses a used binding, 422
// cloud_binding_used, and held, the store's count of jobs and triggers,
// does not move (#279).
func usedCloudBindingRefused(t *testing.T, sv *Service, runtime string, job Job, scope string, scopes []string, held func() [2]int) {
	t.Helper()
	conn := CloudConnection{ID: "google", Subscription: "projects/example-project/subscriptions/desk-inbox", CredentialsFile: "/nonexistent/test-adc.json"}
	sv.cfg.CloudConnections = []CloudConnection{conn}
	cloud := TriggerConfig{Name: "Scheduled", Kind: "cloud", Cloud: &CloudBinding{Connection: conn.ID, Subscription: conn.Subscription, Job: "projects/example-project/locations/us-central1/jobs/daily"}, Input: &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}, Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	path := "/v1/jobs/" + job.ID + "/triggers"
	if scope != "" {
		path += "?scope=" + scope
	}
	if got := serve(t, sv, "POST", path, string(encode(map[string]any{"config": cloud})), nil); got.Code != 200 {
		t.Fatal(got.Code, got.Body.String())
	}
	before := held()
	for _, scope := range scopes {
		release := journalRelease(t, sv, runtime)
		body := map[string]any{"name": "Bound", "releaseId": release.ID, "reviewed": true, "trigger": cloud}
		if scope != "" {
			body["scope"] = scope
		}
		got := serve(t, sv, "POST", "/v1/jobs", string(encode(body)), nil)
		if got.Code != 422 || !strings.Contains(got.Body.String(), `"cloud_binding_used"`) {
			t.Fatalf("under %q, a job with a used cloud binding: %d %s", scope, got.Code, got.Body.String())
		}
	}
	if after := held(); after != before {
		t.Fatal("the refused creations made jobs or triggers:", before, after)
	}
}

// On the default kind: under the binding's own scope (none) and another.
func TestCreatingAJobWithAUsedCloudBindingIsRefused(t *testing.T) {
	cfg := journalConfig(t)
	sv := openJournal(t, cfg)
	t.Cleanup(sv.Close)
	runtime := completingRuntime(t, cfg.Dir)
	release := journalRelease(t, sv, runtime)
	j, err := sv.createJob("Held", release.ID)
	must(t, err)
	usedCloudBindingRefused(t, sv, runtime, j, "", []string{"", "team-a"}, func() (n [2]int) {
		must(t, sqliteDB(sv).QueryRow("SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM triggers)").Scan(&n[0], &n[1]))
		return n
	})
}
func TestCloudInterruptedAcquisitionNeverRepeats(t *testing.T) {
	s, cfg, _, tr, conn := cloudFixture(t)
	tr, _, _ = s.setTriggerState(tr.ID, tr.Revision, false, true)
	d := cloudSignal(tr, time.Now())
	if ack, e := s.receiveCloud(conn, d); !ack || e != nil {
		t.Fatal(e)
	}
	o := latestOccurrence(t, s)
	o.State = "preparing"
	if e := s.saveOccurrence(o); e != nil {
		t.Fatal(e)
	}
	s.Close()
	again, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if o = latestOccurrence(t, again); o.State != "failed" || o.PendingInput != nil || !strings.Contains(o.Reason, "not repeated") {
		t.Fatal(o)
	}
	if ack, e := again.receiveCloud(conn, d); !ack || e != nil {
		t.Fatal(e)
	}
	if e = again.prepareOccurrence(context.Background()); e != nil {
		t.Fatal(e)
	}
	if countRows(t, again, "runs") != 0 {
		t.Fatal("repeated uncertain work")
	}
}
func TestAutomaticGatewayFreshVerifiedAndNoRetry(t *testing.T) {
	input, p, key, _ := v2Fixture(t)
	input.Source.Sources = map[string]SourceValue{}
	var calls atomic.Int32
	var wrong atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			w.Write([]byte(`{}`))
			return
		}
		calls.Add(1)
		var req struct {
			Session   string          `json:"session"`
			Source    string          `json:"source"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		args := req.Arguments
		if wrong.Load() {
			args = []byte(`{"tool":"different"}`)
		}
		response := signResponse(t, p, key, args, []byte(`{"id":7,"description":"fresh"}`), time.Now(), 0)
		var v map[string]any
		json.Unmarshal(response, &v)
		receipt := v["receipt"].(map[string]any)
		receipt["sessionId"] = req.Session
		signReceipt(t, receipt, key)
		json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()
	s := &Service{cfg: Config{InputProfiles: []InputProfile{p}, GatewayConnections: []GatewayConnection{{Profile: p.ID, URL: srv.URL}}}}
	for range 2 {
		got, e := s.acquireAutomatic(context.Background(), cloneInput(input))
		if e != nil {
			t.Fatal(e)
		}
		normalized, e := normalizeV2(got, s.cfg.InputProfiles, time.Now())
		if e != nil || string(normalized.Facts) != `{"vendor":{"description":"fresh"}}` {
			t.Fatal(string(normalized.Facts), e)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("reused a release response")
	}
	wrong.Store(true)
	if _, e := s.acquireAutomatic(context.Background(), cloneInput(input)); e == nil {
		t.Fatal("accepted wrong signed arguments")
	}
	if calls.Load() != 3 {
		t.Fatal("retried refused acquisition")
	}
	s.cfg.GatewayConnections = nil
	if _, e := s.acquireAutomatic(context.Background(), cloneInput(input)); e == nil {
		t.Fatal("ignored revoked connection")
	}
	if calls.Load() != 3 {
		t.Fatal("called after revocation")
	}
}

type memoryCloud struct {
	deliveries chan []cloudgoogle.Delivery
	ack        func() error
}

func (m *memoryCloud) Pull(ctx context.Context, _ string) ([]cloudgoogle.Delivery, error) {
	select {
	case d := <-m.deliveries:
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *memoryCloud) Ack(context.Context, string, string) error { return m.ack() }
func TestGoogleRelayToDurableRunnerWithoutBrowser(t *testing.T) {
	s, _, _, tr, conn := cloudFixture(t)
	tr, _, _ = s.setTriggerState(tr.ID, tr.Revision, false, true)
	var published []cloudgoogle.Delivery
	relay := cloudgoogle.Relay(tr.Config.Cloud.Job, func(_ context.Context, signal cloudgoogle.Signal) error {
		d := cloudSignal(tr, time.Now())
		d.Message.Data = base64.StdEncoding.EncodeToString(encode(signal))
		published = append(published, d)
		return nil
	})
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for range 2 {
		req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		req.Header.Set("X-CloudScheduler-JobName", tr.Config.Cloud.Job)
		req.Header.Set("X-CloudScheduler-ScheduleTime", at)
		w := httptest.NewRecorder()
		relay.ServeHTTP(w, req)
		if w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	var acknowledged atomic.Int32
	inbox := &memoryCloud{deliveries: make(chan []cloudgoogle.Delivery, 1), ack: func() error {
		var n int
		if e := sqliteDB(s).QueryRow("SELECT count(*) FROM occurrences").Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			t.Error("acknowledgement before durable deduplicated admission")
		}
		acknowledged.Add(1)
		return nil
	}}
	inbox.deliveries <- published
	s.cloudFactory = func(context.Context, CloudConnection) (cloudTransport, error) { return inbox, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.background.Wait() }()
	s.startBackground(ctx)
	deadline := time.Now().Add(8 * time.Second)
	var o Occurrence
	for time.Now().Before(deadline) {
		if countRows(t, s, "occurrences") == 1 {
			o = latestOccurrence(t, s)
			if o.Input != nil && o.PendingInput == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if o.Input == nil || acknowledged.Load() != 2 {
		t.Fatal(o, acknowledged.Load())
	}
	if e := s.automationTick(time.Now()); e != nil {
		t.Fatal(e)
	}
	o = latestOccurrence(t, s)
	if o.RunID == "" {
		t.Fatal("cloud occurrence was not dispatched", o)
	}
	run := waitRun(t, s, o.RunID)
	if run.State != "completed" || run.Trigger.Kind != "cloud" {
		t.Fatal(run.State)
	}
	if countRows(t, s, "runs") != 1 {
		t.Fatal("duplicate evaluation")
	}
	if conn.ID == "" {
		t.Fatal("missing connection")
	}
}
