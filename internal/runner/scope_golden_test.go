package runner

import (
	"encoding/json"
	"testing"
)

// A record about a job created under no scope identifier is written byte for
// byte as before scope identifiers: these are the bytes an earlier Runner
// writes for each, and a store of such jobs holds no member scope anywhere,
// and stays at its schema (docs/design/scope-identifiers.md, section 6).
func TestARecordWithoutAScopeIsWrittenAsBefore(t *testing.T) {
	at := "2026-01-02T03:04:05Z"
	for _, c := range []struct {
		name, got, want string
	}{
		{"job", string(encode(Job{SchemaVersion: "1", ID: "job_golden", Name: "Golden", ReleaseID: "release_golden", Revision: 1, CreatedAt: at, Workspace: "journal", Owner: "journal-owner"})),
			`{"schemaVersion":"1","id":"job_golden","name":"Golden","releaseId":"release_golden","revision":1,"createdAt":"2026-01-02T03:04:05Z","workspace":"journal","owner":"journal-owner"}`},
		{"trigger", string(encode(Trigger{ID: "trg_golden", JobID: "job_golden", Revision: 1, Authority: "local", Config: eventTriggerConfig(), Paused: true, CreatedAt: at, UpdatedAt: at})),
			`{"id":"trg_golden","jobId":"job_golden","revision":1,"authority":"local","config":{"name":"Deliveries","kind":"event","missed":"skip","overlap":"queue","queueSeconds":3600},"paused":true,"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z","hasKey":false}`},
		{"occurrence", string(encode(Occurrence{QueueSeconds: 3600, ID: "occ_golden", JobID: "job_golden", ReleaseID: "release_golden", JobRevision: 1, TriggerID: "trg_golden", TriggerRevision: 1, Kind: "event", EventID: "first", ReceivedAt: at, ExpiresAt: at, State: "accepted"})),
			`{"queueSeconds":3600,"id":"occ_golden","jobId":"job_golden","releaseId":"release_golden","jobRevision":1,"triggerId":"trg_golden","triggerRevision":1,"kind":"event","eventId":"first","receivedAt":"2026-01-02T03:04:05Z","expiresAt":"2026-01-02T03:04:05Z","state":"accepted"}`},
		{"run", runText(Run{SchemaVersion: "1", ID: "run_golden", JobID: "job_golden", ReleaseID: "release_golden", Revision: 1, State: "queued", Input: Input{Facts: json.RawMessage(`{"a":1}`)}, CreatedAt: at, RequestedBy: "journal-owner"}),
			`{"schemaVersion":"1","id":"run_golden","jobId":"job_golden","releaseId":"release_golden","revision":1,"state":"queued","input":{"facts":{"a":1}},"createdAt":"2026-01-02T03:04:05Z","requestedBy":"journal-owner","attempt":0}`},
		{"entry detail", string(encode(EventDetail{TriggerKind: "event"})), `{"triggerKind":"event"}`},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, c.got, c.want)
		}
	}

	cfg := journalConfig(t)
	s := runJournalSteps(t, cfg, false).s
	for table, column := range map[string]string{"jobs": "record", "job_creations": "record", "job_revisions": "record", "triggers": "record", "trigger_revisions": "record", "occurrences": "record", "runs": "record", "events": "detail"} {
		var n, scoped int
		if e := s.db.QueryRow("SELECT count(*), count(CASE WHEN instr("+column+",'\"scope\"')>0 THEN 1 END) FROM "+table).Scan(&n, &scoped); e != nil {
			t.Fatal(e)
		}
		if n == 0 || scoped != 0 {
			t.Errorf("%s: %d of %d rows hold a scope", table, scoped, n)
		}
	}
	var schema, versions string
	if e := s.db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema); e != nil || schema != "3" {
		t.Fatalf("schema %q %v", schema, e)
	}
	if versions = entryVersionsBesideResults(t, s.db); versions != "1" {
		t.Fatalf("entry versions %q", versions)
	}
}
