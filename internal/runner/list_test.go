package runner

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManualQueuedRunListsKeepPrivatePreimages(t *testing.T) {
	s := manualServiceFixture(t)
	job := manualSavedJob(t, s)
	manualGateAuthority(t, s, nil)
	const token = "owner-list-secret"
	const key = "original-private-list-key"
	const intentID = "intent_private_list"
	const facts = `{ "number":9007199254740993.00, "private":"fact-secret" }`
	const evidence = `{ "private-proof":"present" }`
	raw := `{ "facts": ` + facts + `, "evidence": ` + evidence + ` }`
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+job.ID+"/runs", strings.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set(ManualIntentHeader, intentID)
	accepted := httptest.NewRecorder()
	s.Handler(token).ServeHTTP(accepted, request)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("real manual queue: %d %s", accepted.Code, accepted.Body.String())
	}
	var run Run
	if err := json.Unmarshal(accepted.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.QueryRow("SELECT record FROM runs WHERE id=?", run.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, `"manualAdmission"`) {
		t.Fatal("fixture did not retain private admission metadata")
	}
	for _, path := range []string{"/v1/runs", "/v1/jobs/" + job.ID + "/runs"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			s.Handler(token).ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("list: %d %s", response.Code, response.Body.String())
			}
			var page struct {
				Items []map[string]json.RawMessage `json:"items"`
				Next  int64                        `json:"next"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Next != 0 {
				t.Fatal("manual queue list page", response.Body.String())
			}
			expected := map[string]any{"schemaVersion": run.SchemaVersion, "id": run.ID, "jobId": job.ID, "releaseId": job.ReleaseID, "revision": job.Revision, "state": "queued", "createdAt": run.CreatedAt, "requestedBy": s.cfg.Owner, "attempt": 0, "jobName": job.Name}
			item := page.Items[0]
			if len(item) != len(expected) {
				t.Fatalf("unexpected public summary fields: %s", encode(item))
			}
			for name, want := range expected {
				if !bytes.Equal(item[name], encode(want)) {
					t.Fatalf("public summary %s: %s != %s", name, item[name], encode(want))
				}
			}
		})
	}
	var after string
	if err := s.db.QueryRow("SELECT record FROM runs WHERE id=?", run.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("listing changed private durable record")
	}
	held, err := s.run(run.ID)
	if err != nil || held.manual == nil || held.manual.request != raw || held.manual.intentID != intentID || held.manual.key != key || held.manual.resource == nil || held.manual.registration == nil || !bytes.Equal(held.Input.Facts, []byte(facts)) || !bytes.Equal(held.Input.Evidence, []byte(evidence)) {
		t.Fatal("listing lost exact private durable preimages", err)
	}
}

func TestFilteredListsAcrossPages(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &Service{db: db}
	for _, q := range []string{triggerSchema, `CREATE TABLE releases(id TEXT,record BLOB)`, `CREATE TABLE jobs(seq INTEGER PRIMARY KEY,id TEXT,release_id TEXT,record BLOB)`, `CREATE TABLE runs(seq INTEGER PRIMARY KEY,id TEXT,job_id TEXT,state TEXT,record BLOB)`} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO releases VALUES(?,?)`, "release", encode(map[string]string{"title": "Intake policy", "packVersion": "1.2.0"}))
	for i := 1; i <= 110; i++ {
		name := "Other"
		if i%2 == 0 {
			name = "Target_%"
		}
		exec(`INSERT INTO jobs VALUES(?,?,?,?)`, i, fmt.Sprint(i), "release", encode(map[string]string{"id": fmt.Sprint(i), "name": name}))
	}
	first, next, e := s.filteredRecords("jobs", "", 0, recordFilter{Search: "target_%"})
	if e != nil || len(first) != 50 || next != 12 {
		t.Fatalf("first: %d %d %v", len(first), next, e)
	}
	second, last, e := s.filteredRecords("jobs", "", next, recordFilter{Search: "target_%"})
	if e != nil || len(second) != 5 || last != 0 {
		t.Fatalf("second: %d %d %v", len(second), last, e)
	}
	if string(first[0]) == string(second[0]) {
		t.Fatal("repeated page")
	}
	if !strings.Contains(string(first[0]), `"packVersion":"1.2.0"`) {
		t.Fatal("release summary missing")
	}
	no, _, e := s.filteredRecords("jobs", "", 0, recordFilter{Search: "%' OR 1=1--"})
	if e != nil || len(no) != 0 {
		t.Fatal("search not literal", e)
	}
	for i := 1; i <= 60; i++ {
		state, kind := "completed", "outcome"
		if i%2 == 0 {
			state = "failed"
		}
		if i == 3 {
			kind = "unresolved"
		}
		result := map[string]any{"disposition": map[string]any{"kind": kind, "handoff": map[string]string{"state": "none"}}, "private": "should not be listed"}
		record := map[string]any{"id": fmt.Sprint(i), "jobId": "2", "state": state, "createdAt": "2026-09-26T12:00:00Z", "input": map[string]string{"secret": "value"}, "audit": "private", "result": result}
		exec(`INSERT INTO runs VALUES(?,?,?,?,?)`, i, fmt.Sprint(i), "2", state, encode(record))
	}
	runs, _, e := s.filteredRecords("runs", "", 0, recordFilter{Search: "Target", State: "completed", Review: true})
	if e != nil || len(runs) != 1 {
		t.Fatalf("attention filter %d %v", len(runs), e)
	}
	var summary map[string]json.RawMessage
	if e = json.Unmarshal(runs[0], &summary); e != nil {
		t.Fatal(e)
	}
	if summary["input"] != nil || summary["audit"] != nil || strings.Contains(string(summary["result"]), "private") {
		t.Fatal("list leaked input/audit/result details")
	}
	if string(summary["jobName"]) != `"Target_%"` {
		t.Fatal("job label missing")
	}
	scoped, _, e := s.filteredRecords("runs", "4", 0, recordFilter{})
	if e != nil || len(scoped) != 0 {
		t.Fatal("job scope lost")
	}
	// Request the final page that contains job 2; it must carry only five recent executions.
	jobs, _, e := s.filteredRecords("jobs", "", 12, recordFilter{Search: "Target_%"})
	if e != nil {
		t.Fatal(e)
	}
	var item map[string]json.RawMessage
	json.Unmarshal(jobs[len(jobs)-1], &item)
	var recent []map[string]json.RawMessage
	json.Unmarshal(item["recentRuns"], &recent)
	if len(recent) != 5 || string(recent[0]["id"]) != `"60"` {
		t.Fatal("recent runs ordering/limit", string(item["recentRuns"]))
	}
	for _, r := range recent {
		if len(r) != 3 {
			t.Fatal("recent run includes non-summary fields")
		}
	}
}
