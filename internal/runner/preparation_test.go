package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type asyncFixture struct {
	mu                sync.Mutex
	ready, bad, stale bool
	requests          map[string][]byte
	cancelled         int
	p                 InputProfile
	key               ed25519.PrivateKey
	t                 *testing.T
}

func (f *asyncFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		f.cancelled++
		write(w, 200, map[string]any{"state": "cancelled"})
		return
	}
	var req struct {
		ID, Deadline, Source string
		Arguments            json.RawMessage
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		w.WriteHeader(400)
		return
	}
	held := encode(req)
	if old, ok := f.requests[req.ID]; ok && !bytes.Equal(old, held) {
		f.t.Error("replay changed request")
	}
	f.requests[req.ID] = held
	if !f.ready {
		write(w, 202, map[string]any{"id": req.ID, "deadline": req.Deadline, "state": "running"})
		return
	}
	args := req.Arguments
	if f.bad {
		args = []byte(`{"tool":"wrong"}`)
	}
	at := time.Now()
	if f.stale {
		at = at.Add(-time.Hour)
	}
	response := signResponse(f.t, f.p, f.key, args, []byte(`{"id":7,"description":"fresh","detail":"ready"}`), at, 0)
	var v map[string]any
	json.Unmarshal(response, &v)
	receipt := v["receipt"].(map[string]any)
	receipt["sessionId"] = "async." + req.ID
	signReceipt(f.t, receipt, f.key)
	write(w, 200, map[string]any{"id": req.ID, "deadline": req.Deadline, "state": "completed", "response": v})
}
func preparationFixture(t *testing.T) (*Service, Config, Occurrence, *asyncFixture) {
	t.Helper()
	input, p, key, _ := v2Fixture(t)
	input.Source.Mapping.Sources[0].Read.Rule = []byte(strings.ReplaceAll(string(vendorRule()), `"registry"`, `"intake-form"`))
	f := &asyncFixture{p: p, key: key, t: t, requests: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	s, cfg, j, r := automaticFixture(t)
	cfg.InputProfiles = []InputProfile{p}
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte(strings.Repeat("a", 64)), 0600)
	cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: "http://127.0.0.1:1", Durable: true, OperationsURL: server.URL, OperationsTokenFile: tokenFile}}
	s.cfg = cfg
	r.InputMapping = &input.Source.Mapping
	r.InputProfiles = []InputProfile{p}
	if _, e := s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(r)), r.ID); e != nil {
		t.Fatal(e)
	}
	c := constantTrigger(time.Now())
	c.PreparationSeconds = 3600
	c.Input = &AutomaticInput{Kind: "mapped-sources", Case: input.Source.Case}
	tr, e := s.configureTrigger(j.ID, "", 0, c)
	if e != nil {
		t.Fatal(e)
	}
	o := newOccurrence(tr, j, time.Now())
	o.PendingInput = c.Input
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.admitOccurrence(tx, tr, &o, "fixture", ""); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	return s, cfg, o, f
}
func stepPreparation(t *testing.T, s *Service, id string) Occurrence {
	t.Helper()
	o, e := s.occurrence(id)
	if e != nil {
		t.Fatal(e)
	}
	if o.Preparation != nil {
		o.Preparation.NextCheck = ""
		if e = s.saveOccurrence(o); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.prepareOccurrence(context.Background()); e != nil {
		t.Fatal(e)
	}
	o, e = s.occurrence(id)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func TestDurablePreparationRestartsAndEvaluatesOnce(t *testing.T) {
	s, cfg, o, f := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	if o.State != "waiting" || len(o.Preparation.Tasks) != 1 || countRows(t, s, "runs") != 0 {
		t.Fatal(o)
	}
	operation := o.Preparation.Tasks[0].ID
	s.Close()
	var e error
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o = stepPreparation(t, s, o.ID)
	if o.State != "waiting" || o.Preparation.Tasks[0].ID != operation {
		t.Fatal("lost operation identity", o)
	}
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	o = stepPreparation(t, s, o.ID)
	if o.Preparation.Tasks[0].State != "completed" {
		t.Fatal(o)
	}
	o = stepPreparation(t, s, o.ID)
	if o.State != "accepted" || o.ReadyUntil == "" || o.Input == nil {
		t.Fatal(o)
	}
	for range 2 {
		if e = s.dispatchOccurrence(o, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	o, _ = s.occurrence(o.ID)
	run := waitRun(t, s, o.RunID)
	if run.State != "completed" || countRows(t, s, "runs") != 1 {
		t.Fatal(run)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal("new operation on restart")
	}
}
func TestDurablePreparationRejectsTamperedAndStaleProof(t *testing.T) {
	for _, kind := range []string{"arguments", "freshness"} {
		t.Run(kind, func(t *testing.T) {
			s, _, o, f := preparationFixture(t)
			f.mu.Lock()
			f.ready = true
			f.bad = kind == "arguments"
			f.stale = kind == "freshness"
			f.mu.Unlock()
			o = stepPreparation(t, s, o.ID)
			if o.State != "needs-attention" || countRows(t, s, "runs") != 0 {
				t.Fatal(o)
			}
		})
	}
}
func TestDurablePreparationCancellationFencesLateResult(t *testing.T) {
	s, _, o, f := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	stale := o
	if _, e := s.cancelOccurrence(context.Background(), o.ID); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	// A response already in flight cannot revive the cancelled occurrence.
	if e := s.advancePreparation(context.Background(), stale); e != nil && !errorsIsStopped(e) {
		t.Fatal(e)
	}
	o, _ = s.occurrence(o.ID)
	if o.State != "cancelled" || countRows(t, s, "runs") != 0 {
		t.Fatal(o)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled != 1 {
		t.Fatal("cancellation was not forwarded")
	}
}
func errorsIsStopped(e error) bool { return e == errPreparationStopped }
func TestDurablePreparationExpiryAndRedaction(t *testing.T) {
	s, _, o, _ := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	public := string(encode(publicOccurrence(o)))
	if strings.Contains(public, "SELECT") || strings.Contains(public, "http://") || strings.Contains(public, "pendingInput") || strings.Contains(public, `"input"`) {
		t.Fatal("private request escaped", public)
	}
	o.Preparation.Deadline = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	s.saveOccurrence(o)
	o = stepPreparation(t, s, o.ID)
	if o.State != "expired" || countRows(t, s, "runs") != 0 {
		t.Fatal(o)
	}
}
func TestDurablePreparationCheckAgainReusesIdentity(t *testing.T) {
	s, _, o, f := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	op := o.Preparation.Tasks[0].ID
	o.State = "needs-attention"
	s.saveOccurrence(o)
	if _, e := s.reconcileOccurrence(o.ID); e != nil {
		t.Fatal(e)
	}
	o = stepPreparation(t, s, o.ID)
	if o.State != "waiting" || o.Preparation.Tasks[0].ID != op {
		t.Fatal(o)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal("reconciliation created another call")
	}
}
func TestDurablePreparationCompletedSourcesSurviveRestart(t *testing.T) {
	s, cfg, o, f := preparationFixture(t)
	r, _ := s.release(o.ReleaseID)
	r.InputMapping.Sources = append(r.InputMapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: f.p.ID, ProfileDigest: profileHash(f.p), MaxAge: 300, Arguments: []byte(`{"tool":"lookup","arguments":{}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/detail", "/detail"}}, Evidence: []EvidenceMapping{}}}})
	s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(r)), r.ID)
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	o = stepPreparation(t, s, o.ID)
	first := o.Preparation.Tasks[0].ID
	f.mu.Lock()
	f.ready = false
	f.mu.Unlock()
	o = stepPreparation(t, s, o.ID)
	if len(o.Preparation.Tasks) != 2 {
		t.Fatal(o)
	}
	s.Close()
	var e error
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o = stepPreparation(t, s, o.ID)
	if o.Preparation.Tasks[0].ID != first || o.Preparation.Tasks[0].State != "completed" {
		t.Fatal("lost completed source", o)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 {
		t.Fatal("replayed completed source")
	}
}
func TestDurablePreparationOtherOccurrencesProgressWhileWaiting(t *testing.T) {
	s, _, o, _ := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	other := o
	other.ID = id("occ_")
	other.State = "accepted"
	other.Preparation = nil
	// A second durable occurrence can advance while the first retains no worker.
	if _, e := s.db.Exec("INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES(?,?,?,?,?,?,?)", other.ID, other.JobID, other.TriggerID, "second", "", other.State, string(encode(other))); e != nil {
		t.Fatal(e)
	}
	if e := s.prepareOccurrence(context.Background()); e != nil {
		t.Fatal(e)
	}
	other, _ = s.occurrence(other.ID)
	if other.State != "waiting" || len(other.Preparation.Tasks) != 1 {
		t.Fatal("slow source blocked other work", other)
	}
}

func TestAutomaticRunRechecksFreshnessAfterQueue(t *testing.T) {
	s, cfg, o, f := preparationFixture(t)
	// Stop the worker before inserting a previously accepted, now stale run.
	s.cancel()
	<-s.done
	release, e := s.release(o.ReleaseID)
	if e != nil {
		t.Fatal(e)
	}
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	args := []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`)
	input := Input{Source: &SourceInput{Mapping: *release.InputMapping, Case: []byte(`{"id":7}`), Sources: map[string]SourceValue{"vendor": {Response: signResponse(t, f.p, f.key, args, []byte(`{"id":7,"description":"verified then"}`), at, 0)}}}}
	input, e = normalizeV2(input, []InputProfile{f.p}, at)
	if e != nil {
		t.Fatal(e)
	}
	run := Run{SchemaVersion: "1", ID: id("run_"), JobID: o.JobID, ReleaseID: o.ReleaseID, State: "queued", Input: input, CreatedAt: at.Format(time.RFC3339), Trigger: &TriggerOrigin{OccurrenceID: o.ID, TriggerID: o.TriggerID, ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}}
	if _, e = s.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record)VALUES(?,?,?,?,?,?,?)", run.ID, run.JobID, "test", "old", digest(encode(input)), "queued", string(encode(run))); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	run = waitRun(t, s, run.ID)
	if run.State != "failed" || !strings.Contains(run.Problem, "stale") || len(run.Audit) > 0 || len(run.Result) > 0 {
		t.Fatal("stale source reached evaluation", run)
	}
}

func TestUncertainPreparationBlocksSkipOverlap(t *testing.T) {
	s, _, o, _ := preparationFixture(t)
	o = stepPreparation(t, s, o.ID)
	o.State = "needs-attention"
	if e := s.saveOccurrence(o); e != nil {
		t.Fatal(e)
	}
	tr, _, e := s.trigger(o.TriggerID)
	if e != nil {
		t.Fatal(e)
	}
	tr.Config.Overlap = "skip"
	job, e := s.job(o.JobID)
	if e != nil {
		t.Fatal(e)
	}
	next := newOccurrence(tr, job, time.Now())
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, tr, &next, "uncertain-overlap", ""); e != nil {
		t.Fatal(e)
	}
	if next.State != "skipped" || next.Reason != "overlap" {
		t.Fatal("uncertain provider work did not block overlap", next)
	}
}
