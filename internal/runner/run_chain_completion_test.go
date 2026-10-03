package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wrappedRuntime is an executable that runs the Runtime at real and then
// rewrites the operational audit trail it wrote, $f, with change, a shell
// command that keeps the file and its private mode. A rehearsal writes no
// trail, and is left as the Runtime answered.
func wrappedRuntime(t *testing.T, real, change string) string {
	t.Helper()
	real, e := filepath.Abs(real)
	if e != nil {
		t.Fatal(e)
	}
	script := "#!/bin/sh\nPATH=/usr/bin:/bin\nexport PATH\n'" + strings.ReplaceAll(real, "'", `'\''`) + "' \"$@\" || exit $?\n" +
		"f=audit/evaluations.jsonl\n[ -f \"$f\" ] || exit 0\n" + change + "\n"
	path := filepath.Join(t.TempDir(), "wrapped-runtime")
	if e = os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}

// A trail that still parses as the record, but is not its one line ended by a
// newline, has no line that is the record: no exact bytes can be kept, so the
// evaluation is refused and the run fails, with no entry, rather than
// completing without either. The dispatcher goes on.
func TestARunWhoseRecordsBytesCannotBeKeptFails(t *testing.T) {
	for name, change := range map[string]string{
		// The case a review found: the record's line, its newline removed.
		"its newline removed":             `printf '%s' "$(cat "$f")" > "$f.new" && cat "$f.new" > "$f"`,
		"ended by a carriage return":      `printf '%s\r\n' "$(cat "$f")" > "$f.new" && cat "$f.new" > "$f"`,
		"followed by an empty line":       `printf '\n' >> "$f"`,
		"preceded by an empty line":       `printf '\n%s\n' "$(cat "$f")" > "$f.new" && cat "$f.new" > "$f"`,
		"written over two lines":          `awk '{sub(/,/, ",\n"); print}' "$f" > "$f.new" && cat "$f.new" > "$f"`,
		"ended by a newline and a space":  `printf ' ' >> "$f"`,
		"one line of the record, no more": `true`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Runtime = wrappedRuntime(t, cfg.Runtime, change)
			s, e := Open(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			release := testRelease(t, s)
			job, e := s.createJob("Unkept", release.ID)
			if e != nil {
				t.Fatal(e)
			}
			run, _, e := s.submit(job.ID, "one", sample())
			if e != nil {
				t.Fatal(e)
			}
			done := waitRun(t, s, run.ID)
			trail, e := os.ReadFile(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "evaluations.jsonl"))
			if e != nil || !json.Valid(trail) {
				t.Fatalf("the trail does not parse as the record: %q %v", trail, e)
			}
			if change == "true" {
				// The control: the Runtime's own trail, through the same wrapper.
				if done.State != "completed" || string(done.AuditBytes)+"\n" != string(trail) {
					t.Fatal(done.State, done.Problem)
				}
				holdChain(t, s, []string{run.ID}, func(string) []byte { return done.AuditBytes })
				return
			}
			if recordLine(trail) != nil {
				t.Fatalf("the trail is still one line: %q", trail)
			}
			if done.State != "failed" || done.Problem != "operational audit record is not one line ended by a newline, so its exact bytes cannot be kept" || done.AuditBytes != nil || done.Audit != nil {
				t.Fatalf("state=%s auditBytes=%d problem=%q", done.State, len(done.AuditBytes), done.Problem)
			}
			if rows := chainRows(t, s); len(rows) != 0 {
				t.Fatal("a run that did not complete was given an entry")
			}
			if s.unhealthy.Load() {
				t.Fatal("the dispatcher stopped for a run that failed")
			}
		})
	}
}

// A completed run is not recorded without its entry: finishRun refuses a
// completion whose record's exact bytes, one line, were not kept, and the run
// stays as it was, with no entry. A run that did not complete needs none.
func TestACompletionWithoutTheRecordsBytesIsNotRecorded(t *testing.T) {
	s := placeholderStore(t)
	run, record := syntheticRun(0)
	running := Run{ID: run, State: "running"}
	if _, e := s.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,'job','caller','key','digest','running',?)", run, string(encode(running))); e != nil {
		t.Fatal(e)
	}
	for name, kept := range map[string][]byte{
		"none":              nil,
		"empty":             {},
		"two lines":         []byte("{}\n{}"),
		"a newline":         []byte("{}\n"),
		"a carriage return": []byte("{}\r"),
	} {
		completed := running
		completed.State, completed.AuditBytes = "completed", kept
		if e := s.finishRun(completed); e != errNoAuditBytes {
			t.Fatal(name, e)
		}
		if got, e := s.run(run); e != nil || got.State != "running" {
			t.Fatal(name, "a completion was recorded without its entry:", got.State, e)
		}
		if rows := chainRows(t, s); len(rows) != 0 {
			t.Fatal(name, "an entry was written")
		}
	}
	failed := running
	failed.State = "failed"
	if e := s.finishRun(failed); e != nil {
		t.Fatal(e)
	}
	if rows := chainRows(t, s); len(rows) != 0 {
		t.Fatal("a failed run was given an entry")
	}
	completed := running
	completed.State, completed.AuditBytes = "completed", record
	if e := s.finishRun(completed); e != nil {
		t.Fatal(e)
	}
	if got, e := s.run(run); e != nil || got.State != "completed" {
		t.Fatal(got.State, e)
	}
	holdChain(t, s, []string{run}, func(string) []byte { return record })
}

// A completion whose entry cannot be appended is not recorded. Here the store
// refuses the entry, through a trigger, after the Runtime evaluated the run:
// the run is left as the dispatcher last saved it, running, with no entry, and
// the dispatcher stops, as on any storage error. After a restart the run reads
// interrupted, and the next completed run's entry follows the last one, with
// no sequence skipped.
func TestAFailedAppendRecordsNoCompletion(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	release := testRelease(t, s)
	job, e := s.createJob("Refused", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	first, _, e := s.submit(job.ID, "first", sample())
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRun(t, s, first.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	if _, e = s.db.Exec(`CREATE TRIGGER refuse_entry BEFORE INSERT ON run_chain BEGIN SELECT RAISE(ABORT, 'the test refuses this entry'); END`); e != nil {
		t.Fatal(e)
	}
	refused, _, e := s.submit(job.ID, "refused", sample())
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		t.Fatal("the dispatcher did not stop when the append failed")
	}
	if got, e := s.run(refused.ID); e != nil || got.State != "running" || got.AuditBytes != nil {
		t.Fatalf("a completion was recorded without its entry: state=%s auditBytes=%d %v", got.State, len(got.AuditBytes), e)
	}
	if _, chained, e := s.runEntrySequence(refused.ID); e != nil || chained {
		t.Fatal("the refused entry was written", e)
	}
	if !s.unhealthy.Load() {
		t.Fatal("the runner does not say it is unhealthy")
	}
	if _, e = s.db.Exec("DROP TRIGGER refuse_entry"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = Open(cfg); e != nil {
		t.Fatal(e)
	}
	if got, e := s.run(refused.ID); e != nil || got.State != "interrupted" || got.AuditBytes != nil {
		t.Fatal("after a restart the run does not read interrupted:", got.State, e)
	}
	after, _, e := s.submit(job.ID, "after", sample())
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRun(t, s, after.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	holdChain(t, s, []string{first.ID, after.ID}, func(run string) []byte { return trailLine(t, cfg.Dir, run) })
}
