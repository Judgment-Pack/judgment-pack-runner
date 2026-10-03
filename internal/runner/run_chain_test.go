package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// chainRow is one row of the store's chain of runs.
type chainRow struct {
	sequence int64
	run      string
	line     []byte
}

// chainRows reads the store's chain of runs, in sequence order.
func chainRows(t *testing.T, s *Service) []chainRow {
	t.Helper()
	rows, e := s.db.Query("SELECT sequence,run,line FROM run_chain ORDER BY sequence")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var all []chainRow
	for rows.Next() {
		var r chainRow
		if e = rows.Scan(&r.sequence, &r.run, &r.line); e != nil {
			t.Fatal(e)
		}
		all = append(all, r)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	return all
}

// entryLine is the line Runner writes for an entry: its members in their
// order, compact, as a reader of the documented layout expects.
func entryLine(trail string, sequence int64, previous, run, auditDigest string) []byte {
	return fmt.Appendf(nil, `{"entryVersion":"1","trail":%q,"sequence":%d,"previous":%q,"kind":"run","run":%q,"auditDigest":%q}`, trail, sequence, previous, run, auditDigest)
}

// emptyDigest is the SHA-256 of nothing, which the first entry links to.
const emptyDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// holdChain holds the store's rows to the chain Runner writes: entries at
// sequences 1 to n, of one trail, each linked to the line before it (the first
// to the SHA-256 of nothing), each naming the run of its row and the digest of
// the bytes want gives for that run, in the order runs gives, line for line as
// entryLine writes it. It returns the rows.
func holdChain(t *testing.T, s *Service, runs []string, want func(run string) []byte) []chainRow {
	t.Helper()
	rows := chainRows(t, s)
	if len(rows) != len(runs) {
		t.Fatalf("the chain holds %d entries, not %d", len(rows), len(runs))
	}
	trail := ""
	previous := emptyDigest
	for i, row := range rows {
		e, err := ParseChainEntry(row.line)
		if err != nil {
			t.Fatal(i, err, string(row.line))
		}
		if i == 0 {
			trail = e.Trail
		}
		expected := entryLine(trail, int64(i+1), previous, runs[i], digest(want(runs[i])))
		if row.sequence != int64(i+1) || row.run != runs[i] || !bytes.Equal(row.line, expected) {
			t.Fatalf("entry %d is not as Runner writes it:\n%s\n%s", i+1, row.line, expected)
		}
		previous = digest(row.line)
	}
	if len(rows) > 0 && !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(trail) {
		t.Fatal("the trail is not 128 random bits in hex:", trail)
	}
	return rows
}

// chainOf is GET /v1/run-chain's answer: every entry's line, each ended by a
// newline.
func chainOf(t *testing.T, s *Service) []byte {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/run-chain", nil)
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	s.Handler("test").ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/jsonl" {
		t.Fatal(w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	return w.Body.Bytes()
}

// joinLines is lines, each ended by a newline: a chain as a file holds it.
func joinLines(lines ...[]byte) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.Write(l)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// Each completed run is given one entry, in the order its run completed,
// binding its id to the SHA-256 of the audit record's line the Runtime wrote.
// A run that failed is given none. The chain is served as a trail file.
func TestCompletedRunsAreChained(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	release, e := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: sample()})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Chained", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	var runs []string
	for _, key := range []string{"one", "two", "three"} {
		run, _, e := s.submit(job.ID, key, sample())
		if e != nil {
			t.Fatal(e)
		}
		if done := waitRun(t, s, run.ID); done.State != "completed" || !bytes.Equal(done.AuditBytes, trailLine(t, cfg.Dir, run.ID)) {
			t.Fatal(done.State, done.Problem)
		}
		runs = append(runs, run.ID)
	}
	line := func(run string) []byte { return trailLine(t, cfg.Dir, run) }
	rows := holdChain(t, s, runs, line)
	// The digest is of the Runtime's line, which holds the pack identifier's &
	// as it is, and not of Runner's encoding of the record.
	if !bytes.Contains(line(runs[0]), []byte("?edition=A&B")) {
		t.Fatal("the fixture's record is not one whose re-encoding has other bytes")
	}
	if got := chainOf(t, s); !bytes.Equal(got, joinLines(rows[0].line, rows[1].line, rows[2].line)) {
		t.Fatalf("the served chain is not the entries' lines: %s", got)
	}
	// A run that fails keeps no record and is given no entry.
	if e = os.WriteFile(s.runtime+".changed", []byte("changed executable"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(s.runtime+".changed", s.runtime); e != nil {
		t.Fatal(e)
	}
	failed, _, e := s.submit(job.ID, "drift", sample())
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRun(t, s, failed.ID); done.State != "failed" {
		t.Fatal(done.State)
	}
	if _, chained, e := s.runEntrySequence(failed.ID); e != nil || chained {
		t.Fatal("a failed run was given an entry", e)
	}
	holdChain(t, s, runs, line)
}

// The chain is in the store, not in memory: after a restart the next entry
// continues its trail and sequence and links to the last entry's line.
func TestTheChainContinuesAcrossARestart(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	release := testRelease(t, s)
	job, e := s.createJob("Restarted", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	var runs []string
	for _, key := range []string{"before", "after"} {
		run, _, e := s.submit(job.ID, key, sample())
		if e != nil {
			t.Fatal(e)
		}
		if done := waitRun(t, s, run.ID); done.State != "completed" {
			t.Fatal(done.State, done.Problem)
		}
		runs = append(runs, run.ID)
		s.Close()
		if s, e = Open(cfg); e != nil {
			t.Fatal(e)
		}
	}
	holdChain(t, s, runs, func(run string) []byte { return trailLine(t, cfg.Dir, run) })
}

// Runs submitted at once are dispatched one by one, and each completed run is
// given one entry: no sequence repeated or skipped, every link sound.
func TestConcurrentRunsAreEachChainedOnce(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	release := testRelease(t, s)
	job, e := s.createJob("Concurrent", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, _, e := s.submit(job.ID, fmt.Sprint("key-", i), sample())
			if e != nil {
				t.Error(e)
				return
			}
			ids[i] = run.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if done := waitRun(t, s, id); done.State != "completed" {
			t.Fatal(done.State, done.Problem)
		}
	}
	// The dispatcher takes runs in queue order; the chain's order is theirs.
	var runs []string
	for _, row := range chainRows(t, s) {
		runs = append(runs, row.run)
	}
	holdChain(t, s, runs, func(run string) []byte { return trailLine(t, cfg.Dir, run) })
	seen := map[string]bool{}
	for _, run := range runs {
		seen[run] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatal("a completed run has no entry:", id)
		}
	}
}

// placeholderStore is a store whose Runtime is never run and whose dispatcher
// does not start, for appending entries directly.
func placeholderStore(t *testing.T) *Service {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	placeholder := filepath.Join(t.TempDir(), "never-run")
	if e = os.WriteFile(placeholder, []byte("not a Runtime: no test runs it\n"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := Open(Config{Dir: dir, Runtime: placeholder, Workspace: "chain", Owner: "owner", disableAutomation: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	return s
}

// syntheticRun is the id of a run that only the chain names, and the bytes its
// entry binds.
func syntheticRun(i int) (string, []byte) {
	return fmt.Sprintf("run_%032x", i+1), fmt.Appendf(nil, `{"synthetic":%d}`, i)
}

// appendEntry appends one entry in a transaction of its own.
func appendEntry(s *Service, run string, record []byte) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = appendRunEntry(tx, run, record); e != nil {
		return e
	}
	return tx.Commit()
}

// Appends made at once, each in its own transaction, neither repeat nor skip a
// sequence: each reads the last entry inside its own transaction, and the
// store serializes them.
func TestConcurrentAppendsNeitherRepeatNorSkip(t *testing.T) {
	s := placeholderStore(t)
	runs := make([]string, 64)
	records := map[string][]byte{}
	for i := range runs {
		run, record := syntheticRun(i)
		runs[i], records[run] = run, record
	}
	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := appendEntry(s, run, records[run]); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	var order []string
	for _, row := range chainRows(t, s) {
		order = append(order, row.run)
	}
	holdChain(t, s, order, func(run string) []byte { return records[run] })
	// A run is given one entry at most.
	if e := appendEntry(s, runs[0], records[runs[0]]); e == nil {
		t.Fatal("a run was given a second entry")
	}
	// The sequence is the table's key, a guard beside the serialized appends:
	// the store refuses a second row at a sequence, and one before the first,
	// whatever writes it.
	for _, sequence := range []int64{1, 0} {
		if _, e := s.db.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)", sequence, "run_"+strings.Repeat("f", 32), []byte("{}")); e == nil {
			t.Fatal("the store holds a row at sequence", sequence)
		}
	}
}

// The chain is served whole, in order, however many pages it takes.
func TestTheChainIsServedWhole(t *testing.T) {
	s := placeholderStore(t)
	var lines [][]byte
	// One transaction, so the store syncs once: what is tested is the reading.
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	for i := range 2345 {
		run, record := syntheticRun(i)
		if e = appendRunEntry(tx, run, record); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	for _, row := range chainRows(t, s) {
		lines = append(lines, row.line)
	}
	if got := chainOf(t, s); !bytes.Equal(got, joinLines(lines...)) {
		t.Fatalf("the served chain has %d lines, not %d", bytes.Count(got, []byte("\n")), len(lines))
	}
}

// Runner never chains after a last entry it did not write, or one not at its
// own sequence, and never exports a stored entry that does not read as one.
func TestRunnerRefusesAnEntryItDidNotWrite(t *testing.T) {
	for name, line := range map[string]string{
		"not an entry":          `{"note":"edited"}`,
		"at another sequence":   string(entryLine(strings.Repeat("a", 32), 7, emptyDigest, "run_"+strings.Repeat("0", 32), emptyDigest)),
		"with an unknown kind":  strings.Replace(string(entryLine(strings.Repeat("a", 32), 1, emptyDigest, "run_"+strings.Repeat("0", 32), emptyDigest)), `"run","run"`, `"discontinuity","run"`, 1),
		"one byte of no entry ": "{",
	} {
		t.Run(name, func(t *testing.T) {
			s := placeholderStore(t)
			if _, e := s.db.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (1,'run_x',?)", []byte(line)); e != nil {
				t.Fatal(e)
			}
			run, record := syntheticRun(1)
			if e := appendEntry(s, run, record); e != errChainHead {
				t.Fatal(e)
			}
		})
	}
	// Nor does it write an entry it would not read back: here one whose run's
	// id makes it longer than any entry.
	s := placeholderStore(t)
	if e := appendEntry(s, "run_"+strings.Repeat("x", maxChainLine), []byte("{}")); e != errEntryUnreadable {
		t.Fatal(e)
	}
	if rows := chainRows(t, s); len(rows) != 0 {
		t.Fatal("an entry Runner would not read was written")
	}
}

// The next entry links to the last entry's line as stored, byte for byte, and
// never to an encoding of it: an entry spelled otherwise, with the same
// members, is followed by one whose previous is the SHA-256 of that spelling.
func TestTheChainLinksToTheStoredBytes(t *testing.T) {
	s := placeholderStore(t)
	trail := strings.Repeat("a", 32)
	first, record := syntheticRun(0)
	spelled := []byte(`{ "entryVersion" : "1", "trail" : "` + trail + `", "sequence" : 1, "previous" : "` + emptyDigest + `", "kind" : "run", "run" : "` + first + `", "auditDigest" : "` + digest(record) + `" }`)
	if _, e := s.db.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (1,?,?)", first, spelled); e != nil {
		t.Fatal(e)
	}
	next, record := syntheticRun(1)
	if e := appendEntry(s, next, record); e != nil {
		t.Fatal(e)
	}
	if rows := chainRows(t, s); len(rows) != 2 || !bytes.Equal(rows[1].line, entryLine(trail, 2, digest(spelled), next, digest(record))) || digest(spelled) == digest(entryLine(trail, 1, emptyDigest, first, digest(record))) {
		t.Fatalf("the next entry does not link to the stored bytes: %s", rows[1].line)
	}
}

// fixtureExport is the version-3 export that Runner made before the chain
// existed, with the record's line the Runtime wrote, the trusted release
// digest and the profiles.
func fixtureExport(t *testing.T) (export, line []byte, release string, profiles []InputProfile) {
	t.Helper()
	dir := "testdata/mapping-v2-before-run-chain/"
	export, e := os.ReadFile(dir + "run.json")
	if e != nil {
		t.Fatal(e)
	}
	trail, e := os.ReadFile(dir + "audit-record.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	trusted, e := os.ReadFile(dir + "release-digest.txt")
	if e != nil {
		t.Fatal(e)
	}
	rawProfiles, e := os.ReadFile(dir + "profiles.json")
	if e != nil {
		t.Fatal(e)
	}
	if profiles, e = ParseInputProfiles(rawProfiles); e != nil {
		t.Fatal(e)
	}
	return export, bytes.TrimSuffix(trail, []byte("\n")), strings.TrimSpace(string(trusted)), profiles
}

// chained is a store holding the fixture's run, whose entry has before other
// runs' entries ahead of it and after behind it, and what checking it needs.
type chained struct {
	s        *Service
	run      string
	line     []byte // the audit record's line
	sequence int64  // the run's entry's
	release  string
	profiles []InputProfile
	v3       []byte // the fixture's own export
}

func chainedFixture(t *testing.T, before, after int) chained {
	t.Helper()
	export, line, release, profiles := fixtureExport(t)
	cfg, b := fixtureStore(t, export)
	cfg.disableAutomation = true
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	for i := range before {
		run, record := syntheticRun(i)
		if e = appendEntry(s, run, record); e != nil {
			t.Fatal(e)
		}
	}
	if e = appendEntry(s, b.Run.ID, b.Run.AuditBytes); e != nil {
		t.Fatal(e)
	}
	for i := range after {
		run, record := syntheticRun(before + i)
		if e = appendEntry(s, run, record); e != nil {
			t.Fatal(e)
		}
	}
	return chained{s: s, run: b.Run.ID, line: line, sequence: int64(before + 1), release: release, profiles: profiles, v3: export}
}

// export is the run's verification export asked for with query.
func (c chained) export(t *testing.T, query string) []byte {
	t.Helper()
	return get(t, c.s.Handler("test"), "/v1/runs/"+c.run+"/verification"+query, 200)
}

// verified is the run's version-4 export, verified.
func (c chained) verified(t *testing.T) VerifiedRun {
	t.Helper()
	v, e := VerifyInputs(c.export(t, "?version=4"), c.profiles, c.release)
	if e != nil || v.ExportVersion() != 4 {
		t.Fatal(e, v.ExportVersion())
	}
	return v
}

// checkpointAt is the checkpoint of the store's entry at a sequence.
func (c chained) checkpointAt(t *testing.T, sequence int64) Checkpoint {
	t.Helper()
	for _, row := range chainRows(t, c.s) {
		if row.sequence == sequence {
			cp, e := checkpointOf(row.line)
			if e != nil {
				t.Fatal(e)
			}
			return cp
		}
	}
	t.Fatal("no entry at", sequence)
	return Checkpoint{}
}

// findingNames is the names of a report's findings.
func findingNames(r RunChainReport) []string {
	var names []string
	for _, f := range r.Findings {
		names = append(names, f.Name)
	}
	return names
}

// A version-4 export is version 3 with the run's entry and the entry's
// checkpoint, exactly as stored; versions 2 and 3 of a chained run are what
// they were before the chain existed.
func TestVersion4ExportCarriesTheRunsEntry(t *testing.T) {
	c := chainedFixture(t, 2, 1)
	raw := c.export(t, "?version=4")
	var v4 VerificationBundle
	if e := strictJSON(raw, &v4); e != nil {
		t.Fatal(e)
	}
	rows := chainRows(t, c.s)
	stored := rows[c.sequence-1]
	want := Checkpoint{CheckpointVersion: "1", RecordDigest: digest(stored.line), Sequence: c.sequence, Trail: c.checkpointAt(t, 1).Trail}
	if v4.Version != 4 || v4.Chain == nil || !bytes.Equal(v4.Chain.Entry, stored.line) || v4.Chain.Checkpoint != want {
		t.Fatalf("version 4 does not carry the run's entry: %s", raw)
	}
	e, _ := ParseChainEntry(v4.Chain.Entry)
	if e.Run != c.run || e.AuditDigest != digest(c.line) || e.Sequence != c.sequence || e.Previous != digest(rows[c.sequence-2].line) {
		t.Fatalf("the entry does not bind the run to its record's line: %+v", e)
	}
	// The checkpoint encodes as the Runtime's does, in its canonical form.
	if got := string(encode(v4.Chain.Checkpoint)); got != fmt.Sprintf(`{"checkpointVersion":"1","recordDigest":%q,"sequence":%d,"trail":%q}`, want.RecordDigest, want.Sequence, want.Trail) {
		t.Fatal(got)
	}
	// Version 4 is version 3 with the chain member, and version 3 is the export
	// Runner made of this run before the chain existed.
	if v3 := c.export(t, "?version=3"); !bytes.Equal(v3, c.v3) {
		t.Fatalf("version 3 of a chained run changed")
	}
	v4.Version, v4.Chain = 3, nil
	var again bytes.Buffer
	if e := json.NewEncoder(&again).Encode(v4); e != nil || !bytes.Equal(again.Bytes(), c.v3) {
		t.Fatal("version 4 is not version 3 with the chain member", e)
	}
	v := c.verified(t)
	entry, checkpoint, ok := v.ChainEntry()
	if !ok || entry.Run != c.run || checkpoint != want || v.RecordDigest() != digest(c.line) {
		t.Fatal(ok, entry, checkpoint, v.RecordDigest())
	}
}

// The export Runner made before the chain existed is served unchanged, as
// whatever version is asked, for a run that has no entry: it is unchained, and
// version 4 is not made up for it. It verifies as it did.
func TestARunRecordedBeforeTheChainIsUnchained(t *testing.T) {
	export, line, release, profiles := fixtureExport(t)
	cfg, b := fixtureStore(t, export)
	cfg.disableAutomation = true
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	h := s.Handler("test")
	for _, query := range []string{"?version=3", "?version=4"} {
		if got := get(t, h, "/v1/runs/"+b.Run.ID+"/verification"+query, 200); !bytes.Equal(got, export) {
			t.Fatalf("the export asked for with %q differs from the earlier Runner's", query)
		}
	}
	v2 := b
	v2.Version, v2.Run.AuditBytes = 2, nil
	var want bytes.Buffer
	if e = json.NewEncoder(&want).Encode(v2); e != nil {
		t.Fatal(e)
	}
	for _, query := range []string{"", "?version=2"} {
		if got := get(t, h, "/v1/runs/"+b.Run.ID+"/verification"+query, 200); !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("version 2%s of an unchained run changed", query)
		}
	}
	v, e := VerifyInputs(export, profiles, release)
	if _, _, ok := v.ChainEntry(); e != nil || ok || v.ExportVersion() != 3 || v.RecordDigest() != digest(line) {
		t.Fatal(e, ok, v.ExportVersion())
	}
	if _, e = v.CheckRunChain(nil, nil); e != ErrNoChainEntry {
		t.Fatal(e)
	}
}

// Checking the entry alone, along the chain, and against held checkpoints:
// what each checks, and when the entry is witnessed.
func TestTheEntryIsCheckedWithAndWithoutAHeldCheckpoint(t *testing.T) {
	c := chainedFixture(t, 2, 2) // the run's entry is 3 of 5
	v := c.verified(t)
	chain := chainOf(t, c.s)
	own := c.checkpointAt(t, 3)
	other := c.checkpointAt(t, 5)
	other.Trail = strings.Repeat("f", 32)
	changed := c.checkpointAt(t, 5)
	changed.RecordDigest = emptyDigest
	beyond := c.checkpointAt(t, 5)
	beyond.Sequence = 6
	for name, want := range map[string]struct {
		chain     []byte
		held      []Checkpoint
		scope     string
		findings  []string
		witnessed bool
		through   int64
	}{
		"the entry alone":                      {nil, nil, ScopeOneSuppliedEntry, nil, false, 0},
		"along the chain":                      {chain, nil, ScopeOneSuppliedChain, nil, false, 0},
		"held at the entry":                    {nil, []Checkpoint{own}, ScopeCheckpoint, nil, true, 3},
		"held at the entry, along the chain":   {chain, []Checkpoint{own}, ScopeCheckpoint, nil, true, 3},
		"held after the entry":                 {chain, []Checkpoint{c.checkpointAt(t, 5)}, ScopeCheckpoint, nil, true, 5},
		"held before the entry":                {chain, []Checkpoint{c.checkpointAt(t, 2)}, ScopeCheckpoint, nil, false, 2},
		"held before and after":                {chain, []Checkpoint{c.checkpointAt(t, 5), c.checkpointAt(t, 1)}, ScopeCheckpoint, nil, true, 5},
		"held of another chain":                {chain, []Checkpoint{other}, ScopeCheckpoint, []string{FindingCheckpointTrailMismatch}, false, 0},
		"held of another entry":                {chain, []Checkpoint{changed}, ScopeCheckpoint, []string{FindingCheckpointRecordMismatch}, false, 0},
		"held beyond the chain":                {chain, []Checkpoint{beyond}, ScopeCheckpoint, []string{FindingCheckpointBeyondChain}, false, 0},
		"held at the entry, of another entry ": {nil, []Checkpoint{{"1", emptyDigest, 3, own.Trail}}, ScopeCheckpoint, []string{FindingCheckpointRecordMismatch}, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			r, e := v.CheckRunChain(nil, want.held)
			if want.chain != nil {
				r, e = v.CheckRunChain(bytes.NewReader(want.chain), want.held)
			}
			if e != nil {
				t.Fatal(e)
			}
			status := "valid"
			if want.findings != nil {
				status = "invalid"
			}
			if r.Scope != want.scope || r.Status != status || r.Witnessed != want.witnessed || fmt.Sprint(findingNames(r)) != fmt.Sprint(want.findings) || r.Checkpoint != own {
				t.Fatalf("%+v", r)
			}
			if want.held != nil && (r.Held == nil || r.Held.Supplied != len(want.held) || r.Held.Through != want.through || r.Held.Matched != len(want.held)-len(want.findings)) {
				t.Fatalf("%+v", r.Held)
			}
			if want.chain == nil && r.Chain != nil || want.chain != nil && (r.Chain == nil || r.Chain.Lines != 5 || r.Chain.Head == nil || *r.Chain.Head != c.checkpointAt(t, 5)) {
				t.Fatalf("%+v", r.Chain)
			}
		})
	}
	// A held checkpoint at another sequence than the entry's is checked along
	// the chain, and without one it is refused, not passed over.
	if _, e := v.CheckRunChain(nil, []Checkpoint{c.checkpointAt(t, 5)}); e == nil || !strings.Contains(e.Error(), "supplied with --chain") {
		t.Fatal(e)
	}
}

// rewriteChain is the store's chain as an operator who removed the entry at a
// sequence would rewrite it to look whole: every later entry moved up one,
// with its sequence and previous recomputed under the same trail.
func rewriteChain(t *testing.T, s *Service, removed int64) {
	t.Helper()
	rows := chainRows(t, s)
	if _, e := s.db.Exec("DELETE FROM run_chain"); e != nil {
		t.Fatal(e)
	}
	previous := emptyDigest
	var sequence int64
	for _, row := range rows {
		if row.sequence == removed {
			continue
		}
		e, err := ParseChainEntry(row.line)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		line := entryLine(e.Trail, sequence, previous, e.Run, e.AuditDigest)
		if _, err = s.db.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)", sequence, e.Run, line); err != nil {
			t.Fatal(err)
		}
		previous = digest(line)
	}
}

// A run the store no longer holds is still named by its entry, and asking for
// its export says so. An entry removed breaks the chain where it was. An entry
// removed with the chain rewritten to close the gap is consistent, and fails
// against a checkpoint held from before; so does a chain cut short.
func TestADeletedRunShows(t *testing.T) {
	t.Run("the run removed, its entry kept", func(t *testing.T) {
		c := chainedFixture(t, 1, 0)
		if _, e := c.s.db.Exec("DELETE FROM runs WHERE id=?", c.run); e != nil {
			t.Fatal(e)
		}
		h := c.s.Handler("test")
		got := get(t, h, "/v1/runs/"+c.run+"/verification?version=4", 410)
		if !bytes.Contains(got, []byte(`"run_not_held"`)) || !bytes.Contains(got, []byte("recorded at sequence 2 of the installation's chain of runs")) {
			t.Fatal(string(got))
		}
		if chain := chainOf(t, c.s); !bytes.Contains(chain, []byte(`"run":"`+c.run+`"`)) {
			t.Fatal("the chain no longer names the run")
		}
		// A run the chain never named is not found, as before.
		get(t, h, "/v1/runs/run_"+strings.Repeat("9", 32)+"/verification", 404)
	})
	t.Run("an entry removed", func(t *testing.T) {
		c := chainedFixture(t, 2, 1)
		v := c.verified(t)
		if _, e := c.s.db.Exec("DELETE FROM run_chain WHERE sequence=2"); e != nil {
			t.Fatal(e)
		}
		r, e := v.CheckRunChain(bytes.NewReader(chainOf(t, c.s)), nil)
		if e != nil || r.Status != "invalid" || r.Findings[0] != (ChainFinding{FindingSequenceMismatch, 2, "the entry's sequence is 3"}) || r.Findings[1].Name != FindingPreviousMismatch {
			t.Fatalf("%+v %v", r, e)
		}
	})
	t.Run("an entry removed, the chain rewritten", func(t *testing.T) {
		c := chainedFixture(t, 2, 2)
		held, head := c.checkpointAt(t, 3), c.checkpointAt(t, 5)
		rewriteChain(t, c.s, 2)
		v := c.verified(t) // the rewritten entry, at sequence 2 now
		chain := chainOf(t, c.s)
		if r, e := v.CheckRunChain(bytes.NewReader(chain), nil); e != nil || r.Status != "valid" {
			t.Fatalf("the rewritten chain is not consistent, so this shows nothing: %+v %v", r, e)
		}
		for name, w := range map[string]struct {
			held    Checkpoint
			finding string
		}{
			"held at the run's entry": {held, FindingCheckpointRecordMismatch},
			"held at the head":        {head, FindingCheckpointBeyondChain},
		} {
			r, e := v.CheckRunChain(bytes.NewReader(chain), []Checkpoint{w.held})
			if e != nil || r.Status != "invalid" || r.Witnessed || fmt.Sprint(findingNames(r)) != "["+w.finding+"]" {
				t.Fatalf("%s: %+v %v", name, r, e)
			}
		}
	})
	t.Run("the last entries removed", func(t *testing.T) {
		c := chainedFixture(t, 0, 2)
		head := c.checkpointAt(t, 3)
		v := c.verified(t)
		if _, e := c.s.db.Exec("DELETE FROM run_chain WHERE sequence>1"); e != nil {
			t.Fatal(e)
		}
		chain := chainOf(t, c.s)
		if r, e := v.CheckRunChain(bytes.NewReader(chain), nil); e != nil || r.Status != "valid" {
			t.Fatalf("%+v %v", r, e)
		}
		if r, e := v.CheckRunChain(bytes.NewReader(chain), []Checkpoint{head}); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingCheckpointBeyondChain+"]" {
			t.Fatalf("%+v %v", r, e)
		}
	})
}

// withBundle is the export changed by change, encoded as Runner encodes one.
func withBundle(t *testing.T, raw []byte, change func(*VerificationBundle)) []byte {
	t.Helper()
	var b VerificationBundle
	if e := strictJSON(raw, &b); e != nil {
		t.Fatal(e)
	}
	change(&b)
	return encode(b)
}

// forge is what an operator who rewrote a run's entry with the export would
// supply: the entry with the members given changed, and its checkpoint
// recomputed to match.
func forge(t *testing.T, b *VerificationBundle, change func(*ChainEntry)) {
	t.Helper()
	e, err := ParseChainEntry(b.Chain.Entry)
	if err != nil {
		t.Fatal(err)
	}
	change(&e)
	b.Chain.Entry = entryLine(e.Trail, e.Sequence, e.Previous, e.Run, e.AuditDigest)
	if b.Chain.Checkpoint, err = checkpointOf(b.Chain.Entry); err != nil {
		t.Fatal(err)
	}
}

// The audit record's bytes changed in the export, to bytes that still parse to
// its record, no longer have the digest the entry names. Changed with the entry
// and its checkpoint to match, they pass a check of the entry alone, and fail
// along the chain and against a checkpoint held from before.
func TestAChangedRecordIsDetected(t *testing.T) {
	c := chainedFixture(t, 1, 1)
	raw := c.export(t, "?version=4")
	respaced := append([]byte(" "), c.line...)
	changed := withBundle(t, raw, func(b *VerificationBundle) { b.Run.AuditBytes = respaced })
	if _, e := VerifyInputs(changed, c.profiles, c.release); e == nil || e.Error() != "the run's chain entry names other bytes of the audit record than the export carries" {
		t.Fatal(e)
	}
	forged := withBundle(t, raw, func(b *VerificationBundle) {
		b.Run.AuditBytes = respaced
		forge(t, b, func(e *ChainEntry) { e.AuditDigest = digest(respaced) })
	})
	v, e := VerifyInputs(forged, c.profiles, c.release)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := v.CheckRunChain(nil, nil); e != nil || r.Status != "valid" || r.Scope != ScopeOneSuppliedEntry || r.Witnessed {
		t.Fatalf("a consistent forgery is what one supplied entry cannot tell apart: %+v %v", r, e)
	}
	chain := chainOf(t, c.s)
	if r, e := v.CheckRunChain(bytes.NewReader(chain), nil); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingEntryMismatch+"]" {
		t.Fatalf("%+v %v", r, e)
	}
	if r, e := v.CheckRunChain(nil, []Checkpoint{c.checkpointAt(t, 2)}); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingCheckpointRecordMismatch+"]" || r.Witnessed {
		t.Fatalf("%+v %v", r, e)
	}
}

// Another run's entry in the export is refused. An entry rewritten to name
// this run at another run's sequence passes a check of the entry alone, and
// fails along the chain and against a checkpoint held from before.
func TestAnEntryMovedToAnotherRunIsDetected(t *testing.T) {
	c := chainedFixture(t, 1, 1)
	raw := c.export(t, "?version=4")
	rows := chainRows(t, c.s)
	moved := withBundle(t, raw, func(b *VerificationBundle) {
		b.Chain.Entry = rows[0].line
		b.Chain.Checkpoint = c.checkpointAt(t, 1)
	})
	if _, e := VerifyInputs(moved, c.profiles, c.release); e == nil || e.Error() != "the run's chain entry names another run" {
		t.Fatal(e)
	}
	// The entry at sequence 1, rewritten to name this run and its record.
	forged := withBundle(t, raw, func(b *VerificationBundle) {
		b.Chain.Entry = rows[0].line
		forge(t, b, func(e *ChainEntry) { e.Run, e.AuditDigest = c.run, digest(c.line) })
	})
	v, e := VerifyInputs(forged, c.profiles, c.release)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := v.CheckRunChain(nil, nil); e != nil || r.Status != "valid" || r.Checkpoint.Sequence != 1 {
		t.Fatalf("%+v %v", r, e)
	}
	if r, e := v.CheckRunChain(bytes.NewReader(chainOf(t, c.s)), nil); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingEntryMismatch+"]" {
		t.Fatalf("%+v %v", r, e)
	}
	if r, e := v.CheckRunChain(nil, []Checkpoint{c.checkpointAt(t, 1)}); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingCheckpointRecordMismatch+"]" {
		t.Fatalf("%+v %v", r, e)
	}
	// A chain that names the run twice is refused however the entries link.
	twice := entryLine(c.checkpointAt(t, 1).Trail, 4, digest(rows[2].line), c.run, digest(c.line))
	chain := joinLines(rows[0].line, rows[1].line, rows[2].line, twice)
	if r, e := c.verified(t).CheckRunChain(bytes.NewReader(chain), nil); e != nil || fmt.Sprint(findingNames(r)) != "["+FindingRunNamedTwice+"]" {
		t.Fatalf("%+v %v", r, e)
	}
}

// A version-4 export's entry is held to its run and its checkpoint; versions 2
// and 3 carry none.
func TestVerifyHoldsTheEntryToItsRun(t *testing.T) {
	c := chainedFixture(t, 1, 0)
	raw := c.export(t, "?version=4")
	line := func(b *VerificationBundle) string { return string(b.Chain.Entry) }
	for name, want := range map[string]struct {
		change  func(*VerificationBundle)
		refusal string
	}{
		"no entry in version 4": {func(b *VerificationBundle) { b.Chain = nil }, "a version-4 export carries its run's entry in the installation's chain of runs"},
		"an entry in version 3": {func(b *VerificationBundle) { b.Version = 3 }, "a version-3 export carries no entry of the installation's chain of runs"},
		"an entry in version 2": {func(b *VerificationBundle) { b.Version, b.Run.AuditBytes = 2, nil }, "a version-2 export carries no entry of the installation's chain of runs"},
		"no record bytes":       {func(b *VerificationBundle) { b.Run.AuditBytes = nil }, "a version-4 export carries the original bytes of a completed run's audit record, as one line"},
		"not an entry": {func(b *VerificationBundle) {
			b.Chain.Entry = []byte(strings.Replace(line(b), `"kind":"run"`, `"kind":"runs"`, 1))
		},
			`the run's chain entry is not one Runner writes: an entry's kind is "run"`},
		"an entry of two lines": {func(b *VerificationBundle) { b.Chain.Entry = append(b.Chain.Entry, '\n') },
			"the run's chain entry is not one Runner writes: an entry is one line of 1 to 1024 bytes"},
		"another checkpoint": {func(b *VerificationBundle) { b.Chain.Checkpoint.Sequence = 1 }, "the export's checkpoint is not its run's chain entry's"},
		"another trail":      {func(b *VerificationBundle) { b.Chain.Checkpoint.Trail = strings.Repeat("0", 32) }, "the export's checkpoint is not its run's chain entry's"},
		"another digest":     {func(b *VerificationBundle) { b.Chain.Checkpoint.RecordDigest = emptyDigest }, "the export's checkpoint is not its run's chain entry's"},
		"another version":    {func(b *VerificationBundle) { b.Chain.Checkpoint.CheckpointVersion = "2" }, "the export's checkpoint is not its run's chain entry's"},
		"an entry too long": {func(b *VerificationBundle) {
			b.Chain.Entry = append([]byte("{"+strings.Repeat(" ", maxChainLine)), b.Chain.Entry[1:]...)
		}, "the run's chain entry exceeds 1024 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := VerifyInputs(withBundle(t, raw, want.change), c.profiles, c.release); e == nil || e.Error() != want.refusal {
				t.Fatal(e)
			}
		})
	}
	// An entry spelled otherwise, with the same members, is read as JSON reads
	// it, as the Runtime reads a chained line; its digest is of its own bytes.
	spelled := withBundle(t, raw, func(b *VerificationBundle) {
		b.Chain.Entry = append([]byte(`{ "entryVersion" : "1",`), b.Chain.Entry[len(`{"entryVersion":"1",`):]...)
		b.Chain.Checkpoint.RecordDigest = digest(b.Chain.Entry)
	})
	if _, e := VerifyInputs(spelled, c.profiles, c.release); e != nil {
		t.Fatal(e)
	}
}

// An entry and a checkpoint are read by their members, exactly, as the Runtime
// reads a chained line and a checkpoint.
func TestEntriesAndCheckpointsAreReadByTheirMembers(t *testing.T) {
	trail, run := strings.Repeat("a", 32), "run_"+strings.Repeat("b", 32)
	good := string(entryLine(trail, 9007199254740990, emptyDigest, run, emptyDigest))
	if e, err := ParseChainEntry([]byte(good)); err != nil || e != (ChainEntry{"1", trail, 9007199254740990, emptyDigest, "run", run, emptyDigest}) {
		t.Fatal(e, err)
	}
	for _, accepted := range []string{
		strings.Replace(good, `"kind"`, `"kind"`, 1),
		strings.Replace(good, `,"run":`, ` , "run" :`, 1),
		strings.Replace(good, `{"entryVersion":"1",`, `{`, 1)[:len(good)-len(`"entryVersion":"1",`)-1] + `,"entryVersion":"1"}`,
	} {
		if _, err := ParseChainEntry([]byte(accepted)); err != nil {
			t.Error(accepted, err)
		}
	}
	for name, refused := range map[string]string{
		"a member twice":        strings.Replace(good, `"kind":"run"`, `"kind":"run","kind":"run"`, 1),
		"a member it lacks":     strings.Replace(good, `,"kind":"run"`, ``, 1),
		"a member it does not":  strings.Replace(good, `"kind":"run"`, `"kind":"run","at":"now"`, 1),
		"a name by case":        strings.Replace(good, `"kind"`, `"Kind"`, 1),
		"another version":       strings.Replace(good, `"entryVersion":"1"`, `"entryVersion":"2"`, 1),
		"a numeric version":     strings.Replace(good, `"entryVersion":"1"`, `"entryVersion":1`, 1),
		"sequence 0":            strings.Replace(good, `9007199254740990`, `0`, 1),
		"sequence 2^53-1":       strings.Replace(good, `9007199254740990`, `9007199254740991`, 1),
		"a leading zero":        strings.Replace(good, `9007199254740990`, `09`, 1),
		"a fraction":            strings.Replace(good, `9007199254740990`, `9.0`, 1),
		"an exponent":           strings.Replace(good, `9007199254740990`, `9e0`, 1),
		"a sequence as text":    strings.Replace(good, `9007199254740990`, `"9"`, 1),
		"an upper-case trail":   strings.Replace(good, trail, strings.ToUpper(trail), 1),
		"a short trail":         strings.Replace(good, trail, trail[1:], 1),
		"a digest without sha":  strings.Replace(good, `"previous":"sha256:`, `"previous":"`, 1),
		"another kind":          strings.Replace(good, `"kind":"run"`, `"kind":"discontinuity"`, 1),
		"an empty run":          strings.Replace(good, run, "", 1),
		"a run that is no text": strings.Replace(good, `"`+run+`"`, "7", 1),
		"trailing text":         good + "x",
		"two values":            good + good,
		"an array":              "[" + good + "]",
		"a carriage return":     good + "\r",
		"too long":              "{" + strings.Repeat(" ", maxChainLine) + good[1:],
		"an empty line":         "",
		"invalid UTF-8 in name": strings.Replace(good, `"kind"`, "\"kin\xff\"", 1),
	} {
		if _, err := ParseChainEntry([]byte(refused)); err == nil {
			t.Error(name, refused)
		}
	}
	checkpoint := fmt.Sprintf(`{"checkpointVersion":"1","recordDigest":%q,"sequence":3,"trail":%q}`, emptyDigest, trail)
	held, err := ParseCheckpoints([]byte("\n" + checkpoint + "\n  \n" + strings.Replace(checkpoint, `"sequence":3`, `"sequence":4`, 1) + "\r\n"))
	if err != nil || len(held) != 2 || held[0] != (Checkpoint{"1", emptyDigest, 3, trail}) || held[1].Sequence != 4 || string(encode(held[0])) != checkpoint {
		t.Fatal(held, err)
	}
	for name, refused := range map[string]string{
		"none":            "\n \n",
		"another version": strings.Replace(checkpoint, `"checkpointVersion":"1"`, `"checkpointVersion":"2"`, 1),
		"a member more":   strings.Replace(checkpoint, `"sequence":3`, `"sequence":3,"note":"x"`, 1),
		"a member twice":  strings.Replace(checkpoint, `"sequence":3`, `"sequence":3,"sequence":3`, 1),
		"a member less":   strings.Replace(checkpoint, `"sequence":3,`, ``, 1),
		"sequence 0":      strings.Replace(checkpoint, `"sequence":3`, `"sequence":0`, 1),
		"one bad line":    checkpoint + "\n{}",
	} {
		if _, err := ParseCheckpoints([]byte(refused)); err == nil {
			t.Error(name)
		}
	}
}

// A chain is read over its exact bytes: a line Runner did not write, or one
// that did not complete, is a finding, and the next line still links to the
// bytes that are there.
func TestAChainIsReadOverItsExactBytes(t *testing.T) {
	c := chainedFixture(t, 1, 1)
	v := c.verified(t)
	rows := chainRows(t, c.s)
	trail := c.checkpointAt(t, 1).Trail
	spelled := bytes.Replace(entryLine(trail, 3, digest(rows[1].line), "run_"+strings.Repeat("d", 32), emptyDigest), []byte(`","`), []byte(`" , "`), -1)
	for name, want := range map[string]struct {
		chain    []byte
		findings []string
	}{
		"as served":                      {joinLines(rows[0].line, rows[1].line, rows[2].line), nil},
		"cut inside its last line":       {append(joinLines(rows[0].line, rows[1].line), rows[2].line[:10]...), []string{FindingIncompleteLastLine}},
		"with a return before a newline": {joinLines(rows[0].line, append(bytes.Clone(rows[1].line), '\r'), rows[2].line), []string{FindingEntryMalformed, FindingEntryMismatch, FindingPreviousMismatch}},
		"with a line of no entry":        {joinLines(rows[0].line, []byte("{}"), rows[1].line, rows[2].line), []string{FindingEntryMalformed, FindingEntryMismatch, FindingSequenceMismatch, FindingPreviousMismatch, FindingSequenceMismatch}},
		"with a line too long":           {joinLines(rows[0].line, bytes.Repeat([]byte("x"), 70000), rows[1].line, rows[2].line), []string{FindingLineTooLong, FindingEntryMismatch, FindingSequenceMismatch, FindingPreviousMismatch, FindingSequenceMismatch}},
		"empty":                          {nil, []string{FindingEntryBeyondChain}},
		"cut before the run's entry":     {joinLines(rows[0].line), []string{FindingEntryBeyondChain}},
		"of another trail": {joinLines(rows[0].line, rows[1].line, entryLine(strings.Repeat("c", 32), 3, digest(rows[1].line), "run_"+strings.Repeat("d", 32), emptyDigest)),
			[]string{FindingTrailMismatch}},
		// An entry spelled otherwise is read by its members, and the next links
		// to its own bytes, not to an encoding of it.
		"with an entry spelled otherwise": {joinLines(rows[0].line, rows[1].line, spelled, entryLine(trail, 4, digest(spelled), "run_"+strings.Repeat("e", 32), emptyDigest)), nil},
	} {
		t.Run(name, func(t *testing.T) {
			r, e := v.CheckRunChain(bytes.NewReader(append([]byte{}, want.chain...)), nil)
			if e != nil || fmt.Sprint(findingNames(r)) != fmt.Sprint(want.findings) || r.FindingsTotal != len(want.findings) {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
	// A held checkpoint naming a line that is not an entry fails.
	r, e := v.CheckRunChain(bytes.NewReader(joinLines(rows[0].line, rows[1].line, []byte("{}"))), []Checkpoint{c.checkpointAt(t, 3)})
	if e != nil || fmt.Sprint(findingNames(r)) != "["+FindingEntryMalformed+" "+FindingCheckpointNotAnEntry+"]" || r.Held.Matched != 0 {
		t.Fatalf("%+v %v", r, e)
	}
}

// A report lists the first hundred findings and counts them all.
func TestAReportListsAHundredFindings(t *testing.T) {
	c := chainedFixture(t, 0, 0)
	v := c.verified(t)
	rows := chainRows(t, c.s)
	chain := joinLines(append([][]byte{rows[0].line}, bytes.Split(bytes.Repeat([]byte("{}\n"), 150), []byte("\n"))[:150]...)...)
	r, e := v.CheckRunChain(bytes.NewReader(chain), nil)
	if e != nil || len(r.Findings) != 100 || r.FindingsTotal != 150 || r.Status != "invalid" {
		t.Fatal(len(r.Findings), r.FindingsTotal, e)
	}
}

// A version-4 export is held to the 8 MiB version 2 is held to, beside the
// members that carry the record's bytes and the chain entry; MaxExportSize is
// the most one can hold.
func TestVersion4IsHeldToVersion2sLimitBesideItsMembers(t *testing.T) {
	c := chainedFixture(t, 0, 0)
	raw := c.export(t, "?version=4")
	var b VerificationBundle
	if e := json.Unmarshal(raw, &b); e != nil {
		t.Fatal(e)
	}
	encoded := encode(b)
	beside := len(`,"auditBytes":""`) + len(encode(b.Run.AuditBytes)) - 2 + len(`,"chain":`) + len(encode(b.Chain))
	pad := func(to int) []byte {
		return append(bytes.Clone(encoded), bytes.Repeat([]byte(" "), to-len(encoded))...)
	}
	if _, e := VerifyInputs(pad(8<<20+beside), c.profiles, c.release); e != nil {
		t.Fatal(e)
	}
	if _, e := VerifyInputs(pad(8<<20+beside+1), c.profiles, c.release); e == nil || e.Error() != "verification bundle exceeds 8 MiB" {
		t.Fatal(e)
	}
	most := encode(RunChainExport{Entry: make([]byte, maxChainLine), Checkpoint: Checkpoint{"1", emptyDigest, maxSafeInteger - 1, strings.Repeat("a", 32)}})
	if MaxExportSize != 8<<20+len(`,"auditBytes":""`)+(maxOutput+2)/3*4+len(`,"chain":`)+len(most) {
		t.Fatal("MaxExportSize is not the most an export holds")
	}
}

// A stored entry that does not read as one is not exported, nor passed over.
func TestAnEntryThatDoesNotReadIsNotExported(t *testing.T) {
	c := chainedFixture(t, 0, 0)
	if _, e := c.s.db.Exec("UPDATE run_chain SET line=? WHERE run=?", []byte(`{"edited":true}`), c.run); e != nil {
		t.Fatal(e)
	}
	get(t, c.s.Handler("test"), "/v1/runs/"+c.run+"/verification?version=4", 500)
	if got := c.export(t, "?version=3"); !bytes.Equal(got, c.v3) {
		t.Fatal("version 3 depends on the entry")
	}
}
