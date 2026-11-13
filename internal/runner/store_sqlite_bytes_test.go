package runner

// The sqlite kind's bytes and schema are the ones a Runner before the widened
// store contract (docs/design/store-interface.md, section 9) wrote: a store
// such a Runner left opens, reads and closes through the contract without a
// byte of its file changing, and a store this Runner makes has the schema
// that Runner made, statement for statement.
//
// The schema and the rows below are written by hand from the statements of
// that Runner (v0.31.0), not read back from a run: a change to a statement of
// the kind, or to how it reads a row, fails here.

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// earlierSchema is sqlite_master of a store a v0.31.0 Runner made and
// migrated, in the order it made each object: its type, name and SQL (none
// for an index SQLite makes for a key).
var earlierSchema = [][3]string{
	{"table", "metadata", "CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_metadata_1", ""},
	{"table", "briefs", "CREATE TABLE briefs (subject TEXT PRIMARY KEY, record TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_briefs_1", ""},
	{"table", "releases", "CREATE TABLE releases (id TEXT PRIMARY KEY, record TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_releases_1", ""},
	{"table", "jobs", "CREATE TABLE jobs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_jobs_1", ""},
	{"index", "sqlite_autoindex_jobs_2", ""},
	{"table", "sqlite_sequence", "CREATE TABLE sqlite_sequence(name,seq)"},
	{"table", "runs", "CREATE TABLE runs (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, job_id TEXT NOT NULL, caller TEXT NOT NULL, idem TEXT NOT NULL, request_digest TEXT NOT NULL, state TEXT NOT NULL, record TEXT NOT NULL, UNIQUE(job_id,caller,idem))"},
	{"index", "sqlite_autoindex_runs_1", ""},
	{"index", "sqlite_autoindex_runs_2", ""},
	{"index", "runs_state", "CREATE INDEX runs_state ON runs(state,seq)"},
	{"table", "triggers", "CREATE TABLE triggers(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,record TEXT NOT NULL,key_hash TEXT NOT NULL DEFAULT '',observed TEXT NOT NULL DEFAULT '',pending TEXT NOT NULL DEFAULT '',pending_at TEXT NOT NULL DEFAULT '', key_revision INTEGER)"},
	{"index", "sqlite_autoindex_triggers_1", ""},
	{"table", "trigger_revisions", "CREATE TABLE trigger_revisions(trigger_id TEXT NOT NULL,revision INTEGER NOT NULL,record TEXT NOT NULL,PRIMARY KEY(trigger_id,revision))"},
	{"index", "sqlite_autoindex_trigger_revisions_1", ""},
	{"table", "occurrences", "CREATE TABLE occurrences(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,job_id TEXT NOT NULL,trigger_id TEXT NOT NULL,identity TEXT NOT NULL,payload_digest TEXT NOT NULL,state TEXT NOT NULL,record TEXT NOT NULL,UNIQUE(trigger_id,identity))"},
	{"index", "sqlite_autoindex_occurrences_1", ""},
	{"index", "sqlite_autoindex_occurrences_2", ""},
	{"table", "occurrence_keys", "CREATE TABLE occurrence_keys(occurrence_id TEXT PRIMARY KEY,key_hash TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_occurrence_keys_1", ""},
	{"index", "occurrences_state", "CREATE INDEX occurrences_state ON occurrences(state,seq)"},
	{"index", "triggers_job", "CREATE INDEX triggers_job ON triggers(job_id)"},
	{"index", "occurrences_job", "CREATE INDEX occurrences_job ON occurrences(job_id,seq)"},
	{"index", "triggers_cloud_binding", "CREATE UNIQUE INDEX triggers_cloud_binding ON triggers(json_extract(record,'$.config.cloud.connection'),json_extract(record,'$.config.cloud.job')) WHERE json_extract(record,'$.config.kind')='cloud'"},
	{"table", "run_chain", "CREATE TABLE run_chain (sequence INTEGER PRIMARY KEY CHECK (sequence > 0), run TEXT UNIQUE NOT NULL, line BLOB NOT NULL)"},
	{"index", "sqlite_autoindex_run_chain_1", ""},
	{"table", "events", "CREATE TABLE events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, entry_version TEXT NOT NULL, kind TEXT NOT NULL, at TEXT NOT NULL, by_actor TEXT NOT NULL, concerns TEXT NOT NULL, revision INTEGER, from_state TEXT, to_state TEXT, reason TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '{}', job_id TEXT NOT NULL DEFAULT '', release_id TEXT NOT NULL DEFAULT '', refusal TEXT NOT NULL DEFAULT '')"},
	{"index", "events_job", "CREATE INDEX events_job ON events(job_id,sequence)"},
	{"index", "events_release", "CREATE INDEX events_release ON events(release_id,sequence)"},
	{"index", "events_refusal", "CREATE INDEX events_refusal ON events(refusal,sequence) WHERE refusal<>''"},
	{"table", "job_creations", "CREATE TABLE job_creations (release_id TEXT PRIMARY KEY, record TEXT NOT NULL)"},
	{"index", "sqlite_autoindex_job_creations_1", ""},
	{"table", "job_revisions", "CREATE TABLE job_revisions (job_id TEXT NOT NULL, revision INTEGER NOT NULL, release_id TEXT UNIQUE NOT NULL, record TEXT NOT NULL, request_digest TEXT NOT NULL DEFAULT '', PRIMARY KEY(job_id,revision))"},
	{"index", "sqlite_autoindex_job_revisions_1", ""},
	{"index", "sqlite_autoindex_job_revisions_2", ""},
}

// earlierRows are the rows of a store such a Runner wrote: one record of
// each kind it keeps, as it encodes them.
var earlierRows = []struct {
	statement string
	args      []any
}{
	{"INSERT INTO metadata VALUES (?,?)", []any{"workspace", "earlier"}},
	{"INSERT INTO metadata VALUES (?,?)", []any{"owner", "earlier-owner"}},
	{"INSERT INTO metadata VALUES (?,?)", []any{"inputRoot", ""}},
	{"INSERT INTO metadata VALUES (?,?)", []any{"schema", "3"}},
	{"INSERT INTO releases VALUES (?,?)", []any{"release_earlier", `{"schemaVersion":"1","id":"release_earlier","pack":"{}","packDigest":"sha256:4444444444444444444444444444444444444444444444444444444444444444","packId":"earlier","packVersion":"1.0.0","title":"Earlier","runtimeDigest":"sha256:5555555555555555555555555555555555555555555555555555555555555555","createdAt":"2026-11-01T10:00:00Z","sample":{},"tests":"passed"}`}},
	{"INSERT INTO jobs(id,release_id,record) VALUES (?,?,?)", []any{"job_earlier", "release_earlier", `{"initialTriggerId":"trg_earlier","initialTriggerDigest":"sha256:6666666666666666666666666666666666666666666666666666666666666666","schemaVersion":"1","id":"job_earlier","name":"Earlier","releaseId":"release_earlier","revision":1,"createdAt":"2026-11-01T10:01:00Z","workspace":"earlier","owner":"earlier-owner"}`}},
	{"INSERT INTO job_creations VALUES (?,?)", []any{"release_earlier", `{"initialTriggerId":"trg_earlier","initialTriggerDigest":"sha256:6666666666666666666666666666666666666666666666666666666666666666","schemaVersion":"1","id":"job_earlier","name":"Earlier","releaseId":"release_earlier","revision":1,"createdAt":"2026-11-01T10:01:00Z","workspace":"earlier","owner":"earlier-owner"}`}},
	{"INSERT INTO job_revisions(job_id,revision,release_id,record) VALUES (?,?,?,?)", []any{"job_earlier", 1, "release_earlier", `{"initialTriggerId":"trg_earlier","initialTriggerDigest":"sha256:6666666666666666666666666666666666666666666666666666666666666666","schemaVersion":"1","id":"job_earlier","name":"Earlier","releaseId":"release_earlier","revision":1,"createdAt":"2026-11-01T10:01:00Z","workspace":"earlier","owner":"earlier-owner"}`}},
	{"INSERT INTO triggers(id,job_id,record,key_hash,key_revision) VALUES (?,?,?,?,?)", []any{"trg_earlier", "job_earlier", `{"id":"trg_earlier","jobId":"job_earlier","revision":2,"authority":"local","config":{"name":"Deliveries","kind":"event","missed":"skip","overlap":"queue","queueSeconds":3600},"paused":false,"createdAt":"2026-11-01T10:01:00Z","updatedAt":"2026-11-01T10:02:00Z","hasKey":true}`, "sha256:7777777777777777777777777777777777777777777777777777777777777777", 2}},
	{"INSERT INTO trigger_revisions VALUES (?,?,?)", []any{"trg_earlier", 1, `{"id":"trg_earlier","jobId":"job_earlier","revision":1,"authority":"local","config":{"name":"Deliveries","kind":"event","missed":"skip","overlap":"queue","queueSeconds":3600},"paused":true,"createdAt":"2026-11-01T10:01:00Z","updatedAt":"2026-11-01T10:01:00Z","hasKey":false}`}},
	{"INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES (?,?,?,?,?,?,?)", []any{"occ_earlier", "job_earlier", "trg_earlier", "event:first", "sha256:8888888888888888888888888888888888888888888888888888888888888888", "submitted", `{"queueSeconds":3600,"id":"occ_earlier","jobId":"job_earlier","releaseId":"release_earlier","jobRevision":1,"triggerId":"trg_earlier","triggerRevision":2,"kind":"event","eventId":"first","receivedAt":"2026-11-01T10:03:00Z","expiresAt":"2026-11-01T11:03:00Z","state":"submitted","runId":"run_earlier"}`}},
	{"INSERT INTO occurrence_keys VALUES (?,?)", []any{"occ_earlier", "sha256:7777777777777777777777777777777777777777777777777777777777777777"}},
	{"INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", []any{"run_earlier", "job_earlier", "trigger:trg_earlier", "occ_earlier", "sha256:9999999999999999999999999999999999999999999999999999999999999999", "queued", `{"trigger":{"occurrenceId":"occ_earlier","triggerId":"trg_earlier","triggerRevision":2,"kind":"event","eventId":"first","expiresAt":"2026-11-01T11:03:00Z"},"schemaVersion":"1","id":"run_earlier","jobId":"job_earlier","releaseId":"release_earlier","revision":1,"state":"queued","input":{"facts":{"a":1}},"createdAt":"2026-11-01T10:04:00Z","requestedBy":"trigger:trg_earlier","attempt":0}`}},
	{"INSERT INTO run_chain VALUES (?,?,?)", []any{1, "run_chained", []byte(`{"entryVersion":"1"}`)}},
	{"INSERT INTO briefs VALUES (?,?)", []any{"job:job_earlier", `{"version":1,"subjects":{}}`}},
	{"INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{"1", "journal.began", "2026-11-01T09:59:00Z", `{"kind":"runner"}`, `{}`, nil, nil, nil, "", `{"held":{"jobs":0,"triggers":0,"occurrences":0,"runs":0}}`, "", "", ""}},
	{"INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{"1", "job.created", "2026-11-01T10:01:00Z", `{"kind":"installation","owner":"earlier-owner"}`, `{"job":"job_earlier","release":"release_earlier"}`, nil, nil, nil, "", `{}`, "job_earlier", "release_earlier", ""}},
	{"INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{"1", "trigger.resumed", "2026-11-01T10:02:00Z", `{"kind":"installation","owner":"earlier-owner"}`, `{"job":"job_earlier","release":"release_earlier","trigger":"trg_earlier"}`, 2, "paused", "enabled", "", `{"previousRevision":1,"keyIssued":true}`, "job_earlier", "release_earlier", ""}},
	{"INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{"1", "run.queued", "2026-11-01T10:04:00Z", `{"kind":"trigger","trigger":"trg_earlier","revision":2}`, `{"job":"job_earlier","release":"release_earlier","trigger":"trg_earlier","occurrence":"occ_earlier","run":"run_earlier"}`, 2, nil, "queued", "", `{}`, "job_earlier", "release_earlier", ""}},
	{"INSERT INTO events(entry_version,kind,at,by_actor,concerns,revision,from_state,to_state,reason,detail,job_id,release_id,refusal) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", []any{"1", "admission.refused", "2026-11-01T10:05:00Z", `{"kind":"installation","owner":"earlier-owner"}`, `{"job":"job_earlier","release":"release_earlier"}`, nil, nil, nil, "", `{"request":"submit-run","status":409,"code":"idempotency_conflict","coalescedSeconds":60}`, "job_earlier", "release_earlier", "submit-run\x00\x00job_earlier\x00\x00release_earlier\x00idempotency_conflict\x00"}},
}

// earlierEntries are the journal's entries as the routes serve them, the
// rows above read back: what Desk and every reader of the journal read.
const earlierEntries = `[{"sequence":1,"entryVersion":"1","kind":"journal.began","at":"2026-11-01T09:59:00Z","by":{"kind":"runner"},"concerns":{},"held":{"jobs":0,"triggers":0,"occurrences":0,"runs":0}},` +
	`{"sequence":2,"entryVersion":"1","kind":"job.created","at":"2026-11-01T10:01:00Z","by":{"kind":"installation","owner":"earlier-owner"},"concerns":{"job":"job_earlier","release":"release_earlier"}},` +
	`{"sequence":3,"entryVersion":"1","kind":"trigger.resumed","at":"2026-11-01T10:02:00Z","by":{"kind":"installation","owner":"earlier-owner"},"concerns":{"job":"job_earlier","release":"release_earlier","trigger":"trg_earlier"},"revision":2,"from":"paused","to":"enabled","previousRevision":1,"keyIssued":true},` +
	`{"sequence":4,"entryVersion":"1","kind":"run.queued","at":"2026-11-01T10:04:00Z","by":{"kind":"trigger","trigger":"trg_earlier","revision":2},"concerns":{"job":"job_earlier","release":"release_earlier","trigger":"trg_earlier","occurrence":"occ_earlier","run":"run_earlier"},"revision":2,"from":null,"to":"queued"},` +
	`{"sequence":5,"entryVersion":"1","kind":"admission.refused","at":"2026-11-01T10:05:00Z","by":{"kind":"installation","owner":"earlier-owner"},"concerns":{"job":"job_earlier","release":"release_earlier"},"request":"submit-run","status":409,"code":"idempotency_conflict","coalescedSeconds":60}]`

// writeEarlierStore makes, in dir, the store a v0.31.0 Runner leaves: its
// schema, made by its statements, and the rows above, in WAL mode, closed.
func writeEarlierStore(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, sqliteStoreFile)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;"); err != nil {
		t.Fatal(err)
	}
	for _, o := range earlierSchema {
		if o[2] == "" || o[1] == "sqlite_sequence" {
			continue
		}
		if _, err = db.Exec(o[2]); err != nil {
			t.Fatal(o[1], err)
		}
	}
	for _, r := range earlierRows {
		if _, err = db.Exec(r.statement, r.args...); err != nil {
			t.Fatal(r.statement, err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// schemaOf is a database's sqlite_master, in the order its objects were
// made.
func schemaOf(t *testing.T, db *sql.DB) [][3]string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,coalesce(sql,'') FROM sqlite_master ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var objects [][3]string
	for rows.Next() {
		var o [3]string
		if err = rows.Scan(&o[0], &o[1], &o[2]); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

func TestANewSQLiteStoreHasTheEarlierSchema(t *testing.T) {
	k := openSQLiteForTest(t, privateStateDir(t))
	defer k.close()
	got := schemaOf(t, k.db)
	if len(got) != len(earlierSchema) {
		t.Fatalf("the schema has %d objects, the earlier %d: %q", len(got), len(earlierSchema), got)
	}
	for i := range got {
		if got[i] != earlierSchema[i] {
			t.Errorf("object %d is %q, the earlier %q", i, got[i], earlierSchema[i])
		}
	}
}

func TestAnEarlierSQLiteStoreOpensReadsAndClosesByteForByte(t *testing.T) {
	dir := privateStateDir(t)
	path := writeEarlierStore(t, dir)
	before := fileDigest(t, path)
	k, err := openSQLiteStore(storeOpening{dir: dir, workspace: "earlier", owner: "earlier-owner", migrate: true})
	if err != nil {
		t.Fatal(err)
	}
	readEarlierStore(t, k)
	must(t, k.close())
	if after := fileDigest(t, path); after != before {
		t.Fatal("opening, reading and closing an earlier store changed its file")
	}
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() != 0 {
		t.Fatal("the store left a write-ahead log of", wal.Size(), "bytes")
	}
}

// readEarlierStore reads every record of the earlier store through the
// contract, and the journal as its routes serve it.
func readEarlierStore(t *testing.T, st *sqliteStore) {
	t.Helper()
	if r, err := st.release("release_earlier"); err != nil || r.Title != "Earlier" || r.PackDigest != "sha256:4444444444444444444444444444444444444444444444444444444444444444" {
		t.Fatal("release:", r.Title, err)
	}
	if j, err := st.job("job_earlier"); err != nil || j.InitialTriggerID != "trg_earlier" || j.Revision != 1 {
		t.Fatal("job:", j.ID, err)
	}
	if r, err := st.run("run_earlier"); err != nil || r.State != "queued" || r.Trigger == nil || r.Trigger.OccurrenceID != "occ_earlier" {
		t.Fatal("run:", r.State, err)
	}
	held, err := st.requestedRun("job_earlier", "trigger:trg_earlier", "occ_earlier")
	if err != nil || held.digest != "sha256:9999999999999999999999999999999999999999999999999999999999999999" {
		t.Fatal("request:", held.digest, err)
	}
	if line, err := st.entryLine("run_chained"); err != nil || string(line) != `{"entryVersion":"1"}` {
		t.Fatal("chain:", string(line), err)
	}
	tr, hash, err := st.trigger("trg_earlier")
	if err != nil || !tr.HasKey || tr.keyRevision == nil || *tr.keyRevision != 2 || hash != "sha256:7777777777777777777777777777777777777777777777777777777777777777" {
		t.Fatal("trigger:", tr.ID, hash, err)
	}
	if ids, err := st.enabledTriggers(); err != nil || len(ids) != 1 || ids[0] != "trg_earlier" {
		t.Fatal("enabled triggers:", ids, err)
	}
	o, payload, err := st.identifiedOccurrence("trg_earlier", "event:first")
	if err != nil || o.RunID != "run_earlier" || payload != "sha256:8888888888888888888888888888888888888888888888888888888888888888" {
		t.Fatal("occurrence:", o.ID, payload, err)
	}
	if o, err := st.keyedOccurrence("occ_earlier", "trg_earlier", hash); err != nil || o.State != "submitted" {
		t.Fatal("keyed occurrence:", o.State, err)
	}
	if items, next, err := st.occurrencePage("job_earlier", 1<<62, false, 50); err != nil || len(items) != 1 || next != 0 {
		t.Fatal("occurrences:", len(items), next, err)
	}
	page, err := eventPageFrom(st, 0, journalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encode(page.Items)); got != earlierEntries {
		t.Fatalf("the journal reads\n%s\nwant\n%s", got, earlierEntries)
	}
	if page.JournalBegan != "2026-11-01T09:59:00Z" || page.More || page.Next != 5 {
		t.Fatal("the page:", page.JournalBegan, page.More, page.Next)
	}
}
