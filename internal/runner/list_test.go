package runner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

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
