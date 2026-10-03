package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// The installation's chain of runs is a trail file as the Runtime's own
// verifier reads one (runtime ADR-0047), and its checkpoints are the Runtime's.
// The test below holds Runner's chain to `jpack audit verify --trail --expect`
// of the Runtime under test. Runtime 0.25.0 and earlier have no such command
// and do not chain their trail: under them the test skips, and it runs in CI
// once CI pins a Runtime release that does (#35).

// auditVerifyTakesATrail says whether the Runtime at bin has `jpack audit
// verify` with --trail and --expect. A Runtime without it answers its root
// help instead, which names neither.
func auditVerifyTakesATrail(t *testing.T, bin string) bool {
	t.Helper()
	cmd := exec.Command(bin, "audit", "verify", "--help")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = t.TempDir()
	out, _ := cmd.CombinedOutput()
	return bytes.Contains(out, []byte("--trail string")) && bytes.Contains(out, []byte("--expect stringArray"))
}

// auditVerifyReport is what the test reads of `jpack audit verify --format
// json`.
type auditVerifyReport struct {
	Status   string      `json:"status"`
	Scope    string      `json:"scope"`
	Lines    int64       `json:"lines"`
	Trail    string      `json:"trail"`
	Head     *Checkpoint `json:"head"`
	Coverage struct {
		LegacyPrefix int64 `json:"legacyPrefix"`
		Chained      int64 `json:"chained"`
		Unchained    int64 `json:"unchained"`
		Uncovered    int64 `json:"uncovered"`
		Damaged      int64 `json:"damaged"`
		Witnessed    int64 `json:"witnessed"`
		Unwitnessed  int64 `json:"unwitnessed"`
	} `json:"coverage"`
	Held *struct {
		Supplied int `json:"supplied"`
		Matched  int `json:"matched"`
		Failed   int `json:"failed"`
	} `json:"held"`
	Findings []struct {
		Name string `json:"name"`
		Line int64  `json:"line"`
	} `json:"findings"`
}

// findings is the report's findings, as name@line, in order.
func (r auditVerifyReport) findings() []string {
	all := []string{}
	for _, f := range r.Findings {
		all = append(all, fmt.Sprintf("%s@%d", f.Name, f.Line))
	}
	return all
}

// auditVerify runs the Runtime's verifier on a trail file, held to the
// checkpoints in held, and answers its report and exit status.
func auditVerify(t *testing.T, bin, trail string, held ...string) (auditVerifyReport, int) {
	t.Helper()
	args := []string{"audit", "verify", "--trail", trail, "--format", "json"}
	for _, h := range held {
		args = append(args, "--expect", h)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = filepath.Dir(trail)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	var exit *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var r auditVerifyReport
	if e := json.Unmarshal(stdout.Bytes(), &r); e != nil {
		t.Fatalf("%v: %s %s", e, stdout.Bytes(), stderr.Bytes())
	}
	return r, code
}

// relinked is a chain of the entries given, as an operator who rewrote the
// chain would write it to look whole: each at its place, linked to the line
// before it, under the same trail.
func relinked(entries ...ChainEntry) []byte {
	var lines [][]byte
	previous := emptyDigest
	for i, e := range entries {
		line := entryLine(e.Trail, int64(i+1), previous, e.Run, e.AuditDigest)
		lines = append(lines, line)
		previous = digest(line)
	}
	return joinLines(lines...)
}

// Runner's chain of runs is read by the Runtime's verifier as a chained trail,
// every line of it, and held to the checkpoints Runner's exports give; a chain
// rewritten or cut short is consistent on its own, as the verifier says, and
// fails against the checkpoints held from before. Each run keeps its record's
// line exactly, and the digest its entry binds is the one by which the
// Runtime's verifier names the record in the attempt's own trail.
func TestTheRuntimesVerifierReadsTheChainOfRuns(t *testing.T) {
	cfg := testConfig(t)
	if !auditVerifyTakesATrail(t, cfg.Runtime) {
		t.Skip("the Runtime under test (JPACK_TEST_BIN) has no `jpack audit verify --trail --expect`, as Runtime 0.25.0 and earlier have not: " +
			"this test needs a Runtime that chains its trail (runtime #206 to #209), and runs in CI once CI pins a Runtime release that does (runner #35)")
	}
	bin := cfg.Runtime
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// Mapping v2 runs, whose verification export carries their entry.
	input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false,"vendor":"A&B <x> café ☃"}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
	release, e := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Interoperable", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	var runs []string
	for i := range 4 {
		run, _, e := s.submit(job.ID, fmt.Sprint("run-", i), input)
		if e != nil {
			t.Fatal(e)
		}
		if done := waitRun(t, s, run.ID); done.State != "completed" {
			t.Fatal(done.State, done.Problem)
		}
		runs = append(runs, run.ID)
	}
	rows := holdChain(t, s, runs, func(run string) []byte { return trailLine(t, cfg.Dir, run) })
	entries := make([]ChainEntry, len(rows))
	for i, row := range rows {
		if entries[i], e = ParseChainEntry(row.line); e != nil {
			t.Fatal(e)
		}
	}

	// The record's bytes, as kept and as the Runtime's verifier names them.
	for i, run := range runs {
		r, e := s.run(run)
		if e != nil || !bytes.Equal(r.AuditBytes, trailLine(t, cfg.Dir, run)) {
			t.Fatal("the run does not keep its record's line exactly:", run, e)
		}
		if !bytes.Contains(r.AuditBytes, []byte("?edition=A&B")) || bytes.Equal(r.AuditBytes, encode(r.Audit)) {
			t.Fatal("the record is not one whose re-encoding has other bytes")
		}
		got, code := auditVerify(t, bin, filepath.Join(cfg.Dir, "attempts", run, "audit", "evaluations.jsonl"))
		if code != 0 || got.Status != "valid" || got.Lines != 1 || got.Coverage.Chained != 1 || got.Head == nil || got.Head.Sequence != 1 || got.Head.RecordDigest != entries[i].AuditDigest {
			t.Fatalf("the attempt's trail does not name the record by the digest its entry binds: %+v %d", got, code)
		}
	}

	// The chain as served, and the checkpoints a holder kept from the exports
	// of the second run and of the last.
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		path := filepath.Join(dir, name)
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	exported := func(run string) Checkpoint {
		var b VerificationBundle
		if e := json.Unmarshal(get(t, s.Handler("test"), "/v1/runs/"+run+"/verification?version=4", 200), &b); e != nil || b.Version != 4 || b.Chain == nil {
			t.Fatal("no version-4 export", e)
		}
		return b.Chain.Checkpoint
	}
	second, last := exported(runs[1]), exported(runs[3])
	chain := write("chain.jsonl", chainOf(t, s))
	held := write("held.jsonl", joinLines(encode(second), encode(last)))

	got, code := auditVerify(t, bin, chain)
	if code != 0 || got.Status != "valid" || got.Scope != "one-supplied-chain" || got.Lines != 4 || got.Trail != entries[0].Trail || got.Head == nil || *got.Head != last ||
		got.Coverage.LegacyPrefix != 0 || got.Coverage.Chained != 4 || got.Coverage.Unchained != 0 || got.Coverage.Uncovered != 0 || got.Coverage.Damaged != 0 || got.Coverage.Witnessed != 0 {
		t.Fatalf("the Runtime does not read the chain as a chained trail: %+v %d", got, code)
	}
	got, code = auditVerify(t, bin, chain, held)
	if code != 0 || got.Status != "valid" || got.Scope != "checkpoint" || got.Coverage.Witnessed != 4 || got.Coverage.Unwitnessed != 0 || got.Held == nil || got.Held.Supplied != 2 || got.Held.Matched != 2 || got.Held.Failed != 0 || len(got.Findings) != 0 {
		t.Fatalf("the chain is not held to the exports' checkpoints: %+v %d", got, code)
	}

	// A chain rewritten, or cut short, is consistent on its own, and fails
	// against the checkpoints held from before.
	changed := entries[1]
	changed.AuditDigest = digest([]byte("another record"))
	// Under the held checkpoint at sequence 2 the entries cut from the end are
	// still the ones first written: they are witnessed, and nothing after.
	for name, want := range map[string]struct {
		chain     []byte
		lines     int64
		matched   int
		witnessed int64
		findings  []string
	}{
		"the second run's entry removed, the chain rewritten": {relinked(entries[0], entries[2], entries[3]), 3, 0, 0,
			[]string{"checkpoint-record-mismatch@2", "checkpoint-beyond-trail@4"}},
		"the second run's record changed, the chain rewritten": {relinked(entries[0], changed, entries[2], entries[3]), 4, 0, 0,
			[]string{"checkpoint-record-mismatch@2", "checkpoint-record-mismatch@4"}},
		"the last entry removed": {joinLines(rows[0].line, rows[1].line, rows[2].line), 3, 1, 2,
			[]string{"checkpoint-beyond-trail@4"}},
		"under another trail": {relinked(func() []ChainEntry {
			other := slices.Clone(entries)
			for i := range other {
				other[i].Trail = "0123456789abcdef0123456789abcdef"
			}
			return other
		}()...), 4, 0, 0, []string{"checkpoint-trail-mismatch@2", "checkpoint-trail-mismatch@4"}},
	} {
		t.Run(name, func(t *testing.T) {
			path := write("rewritten.jsonl", want.chain)
			got, code := auditVerify(t, bin, path)
			if code != 0 || got.Status != "valid" || got.Lines != want.lines || got.Coverage.Chained != want.lines {
				t.Fatalf("the rewritten chain is not consistent on its own: %+v %d", got, code)
			}
			got, code = auditVerify(t, bin, path, held)
			if code != 1 || got.Status != "invalid" || got.Coverage.Witnessed != want.witnessed || got.Held == nil || got.Held.Matched != want.matched || got.Held.Failed != 2-want.matched {
				t.Fatalf("the rewritten chain was not refused against the held checkpoints: %+v %d", got, code)
			}
			if names := got.findings(); !slices.Equal(names, want.findings) {
				t.Fatalf("findings %v, not %v", names, want.findings)
			}
		})
	}
}
