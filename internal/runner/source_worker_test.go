package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func workerFixture(t *testing.T) (*SourceWorker, sourceRequest, chan struct{}, *atomic.Int32) {
	t.Helper()
	barrier := make(chan struct{})
	calls := &atomic.Int32{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/seal" {
			write(w, 200, map[string]bool{"sealed": true})
			return
		}
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		select {
		case <-barrier:
		case <-r.Context().Done():
			return
		}
		write(w, 200, map[string]any{"result": map[string]string{"value": "retained"}, "receipt": map[string]any{"sessionId": "async." + strings.Repeat("a", 32), "callIndex": 0}, "salts": map[string]string{"args": strings.Repeat("b", 64)}})
	}))
	t.Cleanup(gateway.Close)
	worker, e := OpenSourceWorker(SourceWorkerConfig{Dir: filepath.Join(t.TempDir(), "caller"), Gateway: gateway.URL})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(worker.Close)
	request := sourceRequest{ID: strings.Repeat("a", 32), Gateway: gateway.URL, Source: "lookup", Arguments: json.RawMessage(`{"customer":"private-request"}`), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	return worker, request, barrier, calls
}
func awaitWorker(t *testing.T, s *SourceWorker, req sourceRequest, want string) sourceStatus {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		status, _, e := s.submit(req)
		if e != nil {
			t.Fatal(e)
		}
		if status.State == want {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("source worker never reached", want)
	return sourceStatus{}
}
func awaitSourceCall(t *testing.T, calls *atomic.Int32) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("source did not start once", calls.Load())
	}
}
func TestSourceWorkerRetainsCallerProofAndNeverRepeatsClaim(t *testing.T) {
	s, req, barrier, calls := workerFixture(t)
	for range 5 {
		if _, code, e := s.submit(req); e != nil || code != 202 {
			t.Fatal(code, e)
		}
	}
	awaitSourceCall(t, calls)
	changed := req
	changed.Arguments = []byte(`{"customer":"different"}`)
	if _, code, e := s.submit(changed); e == nil || code != 409 {
		t.Fatal("changed request admitted", code, e)
	}
	close(barrier)
	done := awaitWorker(t, s, req, "completed")
	if !strings.Contains(string(done.Response), `"salts"`) {
		t.Fatal("caller proof lost")
	}
	cfg := s.cfg
	s.Close()
	again, e := OpenSourceWorker(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	replay := awaitWorker(t, again, req, "completed")
	if string(done.Response) != string(replay.Response) || calls.Load() != 1 {
		t.Fatal("restart changed proof or repeated source")
	}
	if _, e = OpenSourceWorker(SourceWorkerConfig{Dir: cfg.Dir, Gateway: cfg.Gateway}); e == nil {
		t.Fatal("two source workers own same caller store")
	}
}
func TestSourceWorkerInterruptedCallIsNotReplayed(t *testing.T) {
	s, req, _, calls := workerFixture(t)
	if _, _, e := s.submit(req); e != nil {
		t.Fatal(e)
	}
	awaitSourceCall(t, calls)
	cfg := s.cfg
	s.Close()
	again, e := OpenSourceWorker(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	awaitWorker(t, again, req, "needs-attention")
	if calls.Load() != 1 {
		t.Fatal("uncertain acquisition repeated")
	}
}
func TestSourceWorkerCancellationAndAuthorization(t *testing.T) {
	s, req, barrier, _ := workerFixture(t)
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	for _, path := range []string{"/operations", "/operations/" + req.ID, "/operations/" + req.ID + "/cancel"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(string(encode(req))))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("unauthed caller admitted", w.Code)
		}
	}
	body, code, e := operationCall(context.Background(), server.URL+"/operations", encode(req), s.TokenFile())
	if e != nil || code != 202 {
		t.Fatal(code, e, string(body))
	}
	_, code, e = operationCall(context.Background(), server.URL+"/operations/"+req.ID+"/cancel", []byte(`{}`), s.TokenFile())
	if e != nil || code != 200 {
		t.Fatal(code, e)
	}
	close(barrier)
	status := awaitWorker(t, s, req, "cancelled")
	if len(status.Response) > 0 {
		t.Fatal("cancelled response exposed")
	}
	r := httptest.NewRequest("GET", "/operations/"+req.ID, nil)
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("browser origin admitted")
	}
}
func TestSourceWorkerRejectsInvalidAdmissionWithoutState(t *testing.T) {
	s, req, _, calls := workerFixture(t)
	invalid := []sourceRequest{req, req, req, req}
	invalid[0].Deadline = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	invalid[1].Deadline = time.Now().Add(8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	invalid[2].Gateway = "http://127.0.0.1:1"
	invalid[3].Source = "lookup/write"
	for _, request := range invalid {
		if _, code, e := s.submit(request); e == nil || code != 400 {
			t.Fatal("invalid request admitted", code, e)
		}
	}
	var count int
	s.db.QueryRow("SELECT count(*) FROM source_operations").Scan(&count)
	if count != 0 || calls.Load() != 0 {
		t.Fatal("invalid input retained or acquired")
	}
}
func TestSourceWorkerRefusesSignerStoreAndPublicToken(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "receipts"), 0700)
	if s, e := OpenSourceWorker(SourceWorkerConfig{Dir: filepath.Join(dir, "operations"), Gateway: "http://127.0.0.1:1"}); e == nil {
		s.Close()
		t.Fatal("caller state placed in signer store")
	}
	p := filepath.Join(t.TempDir(), "token")
	os.WriteFile(p, []byte(strings.Repeat("a", 64)), 0644)
	if _, e := readPrivateToken(p); e == nil {
		t.Fatal("non-private token accepted")
	}
}

func TestSourceWorkerExpiryWinsCompletionWaitingToCommit(t *testing.T) {
	s, req, barrier, _ := workerFixture(t)
	req.Deadline = time.Now().Add(200 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	ready, retain := make(chan struct{}), make(chan struct{})
	s.beforeRetain = func() { close(ready); <-retain }
	if _, _, e := s.submit(req); e != nil {
		t.Fatal(e)
	}
	close(barrier)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("completion did not reach barrier")
	}
	time.Sleep(250 * time.Millisecond)
	status, _, e := s.submit(req)
	if e != nil || status.State != "expired" {
		t.Fatal(status, e)
	}
	close(retain)
	s.workers.Wait()
	status, _, e = s.submit(req)
	if e != nil || status.State != "expired" || len(status.Response) > 0 {
		t.Fatal("expired operation resurrected", status, e)
	}
}
func TestSourceWorkerSessionCollisionRequiresAttention(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&request)
		if r.URL.Path == "/seal" {
			write(w, 200, map[string]bool{"sealed": true})
			return
		}
		write(w, 200, map[string]any{"receipt": map[string]any{"sessionId": "async." + strings.Repeat("c", 32), "callIndex": 1}, "result": map[string]any{}, "salts": map[string]any{}})
	}))
	defer gateway.Close()
	s, e := OpenSourceWorker(SourceWorkerConfig{Dir: filepath.Join(t.TempDir(), "caller"), Gateway: gateway.URL})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	req := sourceRequest{ID: strings.Repeat("c", 32), Gateway: gateway.URL, Source: "records", Arguments: []byte(`{}`), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	status := awaitWorker(t, s, req, "needs-attention")
	if len(status.Response) > 0 {
		t.Fatal("colliding proof exposed as complete")
	}
}
