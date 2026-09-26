package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperationalBriefPersistsWithFrozenRun(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	release := testRelease(t, s)
	job, e := s.createJob("Intake", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, _, e := s.submit(job.ID, "brief-run", sample())
	if e != nil {
		t.Fatal(e)
	}
	run = waitRun(t, s, run.ID)
	original, _ := json.Marshal(run)
	for _, kind := range []string{"job", "run"} {
		key := job.ID
		path := "/v1/jobs/" + key + "/briefs"
		if kind == "run" {
			key = run.ID
			path = "/v1/runs/" + key + "/briefs"
		}
		call := func(method string, c any) (int, BriefStore) {
			b, _ := json.Marshal(c)
			r := httptest.NewRequest(method, path, bytes.NewReader(b))
			r.Header.Set("Authorization", "Bearer test")
			w := httptest.NewRecorder()
			s.Handler("test").ServeHTTP(w, r)
			var reply struct {
				Content BriefStore `json:"content"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &reply)
			return w.Code, reply.Content
		}
		source, e := s.briefSnapshot(kind, key)
		if e != nil {
			t.Fatal(e)
		}
		c := BriefCommand{Action: "begin", Subject: kind + ":" + key, Snapshot: json.RawMessage(`{}`), Model: "fixture"}
		if code, _ := call(http.MethodPost, c); code != 409 {
			t.Fatal("forged source admitted", code)
		}
		c.Snapshot = source
		code, d := call(http.MethodPost, c)
		if code != 200 {
			t.Fatal(code)
		}
		c.Action = "complete"
		c.Token = d.Subjects[c.Subject].Pending.Token
		c.Text = BriefText{"Context.", "Findings.", "Unverified.", "Review."}
		if code, _ = call(http.MethodPost, c); code != 200 {
			t.Fatal(code)
		}
		s.Close()
		s, e = Open(cfg)
		if e != nil {
			t.Fatal(e)
		}
		code, d = call(http.MethodGet, nil)
		if code != 200 || len(d.Subjects[c.Subject].Revisions) != 1 {
			t.Fatal("brief lost after restart", code)
		}
		if !sameBriefJSON(d.Subjects[c.Subject].Revisions[0].Snapshot, source) {
			t.Fatal("frozen source changed")
		}
	}
	unchanged, _ := s.run(run.ID)
	b, _ := json.Marshal(unchanged)
	if !bytes.Equal(b, original) {
		t.Fatal("brief generation modified run")
	}
}
