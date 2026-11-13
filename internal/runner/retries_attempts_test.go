package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attemptsElsewhere is a store that says an attempt directory that does not
// exist cannot be taken to hold no audit record, as the second kind says
// (docs/design/store-postgresql.md, D11), over the store it wraps.
type attemptsElsewhere struct{ runStore }

func (attemptsElsewhere) missingAttemptHoldsNoAudit() bool { return false }

func (k attemptsElsewhere) heldTo(sc storeScope) runStore {
	return attemptsElsewhere{k.runStore.heldTo(sc)}
}

// retryAnswer is the retry route's answer for r's attempt.
func retryAnswer(t *testing.T, s *Service, r Run, attempt string) (int, string) {
	t.Helper()
	got := serve(t, s, "POST", "/v1/runs/"+r.ID+"/retry", `{"attempt":`+attempt+`}`, nil)
	return got.Code, got.Body.String()
}

// A run interrupted before its Runtime started has no attempt directory. The
// sqlite kind keeps every attempt's directory in its state directory, so it
// takes a missing one to hold no audit record, as it always did, and the run
// is retried; a kind that says no takes it to be unknown, and the retry is
// refused as for a directory whose state cannot be read (D11).
func TestARetryTakesAMissingAttemptDirectoryAsTheStoreSays(t *testing.T) {
	s, cfg, r := interruptedRun(t, "", false)
	directory := filepath.Join(cfg.Dir, "attempts", r.ID)
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("the attempt's directory exists:", err)
	}
	if !s.store.missingAttemptHoldsNoAudit() {
		t.Fatal("the sqlite kind does not take a missing attempt directory to hold no audit record")
	}
	if code, body := retryAnswer(t, s, r, "1"); code != 202 {
		t.Fatalf("the sqlite kind refused a retry whose attempt directory is missing: %d %s", code, body)
	}

	s, cfg, r = interruptedRun(t, "", false)
	s.store = attemptsElsewhere{s.store}
	directory = filepath.Join(cfg.Dir, "attempts", r.ID)
	code, body := retryAnswer(t, s, r, "1")
	if code != 409 || !strings.Contains(body, "run_not_retryable") || !strings.Contains(body, "cannot be read") {
		t.Fatalf("a retry whose attempt directory is missing: %d %s", code, body)
	}
	held, err := s.run(r.ID)
	must(t, err)
	if runText(held) != runText(r) {
		t.Fatal("a refused retry changed the run")
	}
	// A later attempt's retry asks for every earlier attempt's directory:
	// the second's held, the first's missing, is refused.
	later := r
	later.Attempt, later.Attempts = 2, []Attempt{{Number: 1, Outcome: "interrupted", Reason: runAttemptRestarted}}
	must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(later, runExpected{}); return e }))
	must(t, os.MkdirAll(filepath.Join(cfg.Dir, "attempts", attemptDirectory(later)), 0700))
	if code, body := retryAnswer(t, s, later, "2"); code != 409 || !strings.Contains(body, "cannot be read") {
		t.Fatalf("a retry whose first attempt's directory is missing: %d %s", code, body)
	}
	must(t, s.store.update(func(tx runTx) error { _, e := tx.putRun(r, runExpected{}); return e }))
	// The directory held, with no trail in it, holds no audit record.
	must(t, os.MkdirAll(directory, 0700))
	if code, body := retryAnswer(t, s, r, "1"); code != 202 {
		t.Fatalf("a retry whose attempt directory holds no trail: %d %s", code, body)
	}
}
