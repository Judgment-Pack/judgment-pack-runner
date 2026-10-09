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
	"strings"
	"testing"
)

// The installation's chain of runs is a trail file as the Runtime's own
// verifier reads one (runtime ADR-0047), and its checkpoints are the Runtime's.
// The test below holds Runner's chain to `jpack audit verify --trail --expect`
// of the Runtime under test. Runtime 0.25.0 and earlier have no such command
// and do not chain their trail: under them the test skips. CI pins 0.26.0, the
// first release that does, so it runs there (#35).

// pinnedRuntime is the Runtime executable a release's runs execute: Runner's
// pinned copy, named by the release's digest of it. A Runtime under test that
// cannot itself be executed, such as one of mode 0600, still runs as that copy.
func pinnedRuntime(s *Service, release Release) string {
	return filepath.Join(s.cfg.Dir, "runtimes", strings.TrimPrefix(release.RuntimeDigest, "sha256:"))
}

// absentAnswers are the messages with which a Runtime answers an invocation of
// a command or flag it does not have. The Runtime passes its command parser's
// message on as the one diagnostic of an invocation error: code
// JPS-INVOCATION-ARGUMENTS, exit status 3. Runtime 0.24.0 and 0.25.0 answer
// the probe with "unknown flag: --trail", since flags are read before the
// command they have no `audit verify` for.
var absentAnswers = []string{
	"unknown flag: --trail",
	"unknown flag: --expect",
	`unknown command "audit" for "jpack"`,
	`unknown command "verify" for "jpack audit"`,
}

// probeAuditVerify asks the Runtime at bin to verify a chain of one entry
// against that entry's checkpoint, with `jpack audit verify --trail --expect
// --format json`. It answers "" when the Runtime verifies it. It answers the
// Runtime's own words when the Runtime says, as an invocation error and in
// nothing else, that it has no such command or flag. Anything else is an
// error: a Runtime that could not be run, or that failed otherwise, is never
// taken for one without the command.
func probeAuditVerify(bin, dir string) (string, error) {
	line := entryLine(strings.Repeat("a", 32), 1, emptyDigest, "run_"+strings.Repeat("b", 32), emptyDigest)
	checkpoint, err := checkpointOf(line)
	if err != nil {
		return "", err
	}
	trail, held := filepath.Join(dir, "probe-chain.jsonl"), filepath.Join(dir, "probe-held.jsonl")
	if err = os.WriteFile(trail, joinLines(line), 0600); err != nil {
		return "", err
	}
	if err = os.WriteFile(held, joinLines(encode(checkpoint)), 0600); err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "audit", "verify", "--trail", trail, "--expect", held, "--format", "json")
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	ran := cmd.Run()
	var exit *exec.ExitError
	if ran != nil && !errors.As(ran, &exit) {
		return "", fmt.Errorf("the Runtime could not be run: %w", ran)
	}
	var answer struct {
		Command     string `json:"command"`
		Status      string `json:"status"`
		Scope       string `json:"scope"`
		Diagnostics []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err = json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		return "", fmt.Errorf("the Runtime's answer (%v) is not a JSON report: %s %s", ran, stdout.Bytes(), stderr.Bytes())
	}
	if ran == nil {
		if answer.Command == "audit verify" && answer.Status == "valid" && answer.Scope == "checkpoint" {
			return "", nil
		}
		return "", fmt.Errorf("the Runtime did not verify a chain of one entry against its checkpoint: %s", stdout.Bytes())
	}
	if exit.ExitCode() == 3 && answer.Status == "error" && len(answer.Diagnostics) == 1 && answer.Diagnostics[0].Code == "JPS-INVOCATION-ARGUMENTS" && slices.Contains(absentAnswers, answer.Diagnostics[0].Message) {
		return answer.Diagnostics[0].Message, nil
	}
	return "", fmt.Errorf("the Runtime failed (%v) otherwise than for an unknown command or flag: %s %s", ran, stdout.Bytes(), stderr.Bytes())
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
	// The Runtime asked is the one the runs execute: Runner's pinned copy.
	bin := pinnedRuntime(s, release)
	absent, e := probeAuditVerify(bin, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if absent != "" {
		t.Skipf("the Runtime under test answers `jpack audit verify --trail --expect` with %q: it has no such command, as Runtime 0.25.0 and earlier have not. "+
			"This test needs a Runtime that chains its trail (runtime #206 to #209): Runtime 0.26.0 or later, which CI pins (runner #35)", absent)
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

// Desk hands Runner's chain over from a private snapshot, using the Runtime's
// `audit checkpoint --trail --since` output as the holder's exact files. A
// later read can grow while the first snapshot and its handed-over bytes stay
// fixed; independently retained batches from both reads witness the later
// snapshot together.
func TestDeskCheckpointSinceHandsOverChangingChainSnapshotsExactly(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the published Runtime")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("JPACK_TEST_BIN must name the published Runtime by absolute path")
	}
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release := testRelease(t, s)
	if absent, err := probeAuditVerify(bin, t.TempDir()); err != nil {
		t.Fatal(err)
	} else if absent != "" {
		t.Skipf("the Runtime under test has no audit checkpoint/verify contract: %s", absent)
	}
	job, err := s.createJob("Desk checkpoint hand-over", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	add := func(first, count int) {
		t.Helper()
		for i := range count {
			run, _, err := s.submit(job.ID, fmt.Sprintf("handover-%d", first+i), sample())
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRun(t, s, run.ID); done.State != "completed" {
				t.Fatal(done.State, done.Problem)
			}
		}
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	checkpointSince := func(snapshot string, since int64) []byte {
		t.Helper()
		cmd := exec.Command(bin, "audit", "checkpoint", "--trail", snapshot, "--since", fmt.Sprint(since), "--limit", "300")
		cmd.Env, cmd.Dir = []string{"LANG=C", "LC_ALL=C"}, dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil || stderr.Len() != 0 {
			t.Fatalf("audit checkpoint --since %d: %v: %s", since, err, stderr.Bytes())
		}
		return stdout.Bytes()
	}
	expected := func(rows []chainRow) []byte {
		t.Helper()
		var checkpoints [][]byte
		for _, row := range rows {
			checkpoint, err := checkpointOf(row.line)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints = append(checkpoints, encode(checkpoint))
		}
		return joinLines(checkpoints...)
	}

	add(0, 2)
	firstBytes := chainOf(t, s)
	firstSnapshot := write("private-first.jsonl", firstBytes)
	firstHeldBytes := checkpointSince(firstSnapshot, 0)
	if want := expected(chainRows(t, s)); !bytes.Equal(firstHeldBytes, want) {
		t.Fatalf("the first hand-over is not the Runtime's exact checkpoint files:\n%s\n%s", firstHeldBytes, want)
	}
	firstHeld := write("holder-first.jsonl", firstHeldBytes)

	add(2, 2)
	laterRows := chainRows(t, s)
	laterBytes := chainOf(t, s)
	laterSnapshot := write("private-later.jsonl", laterBytes)
	if still, err := os.ReadFile(firstSnapshot); err != nil || !bytes.Equal(still, firstBytes) || !bytes.HasPrefix(laterBytes, firstBytes) {
		t.Fatalf("the private first snapshot changed, or is not the later snapshot's exact prefix: %v", err)
	}
	laterHeldBytes := checkpointSince(laterSnapshot, 2)
	if want := expected(laterRows[2:]); !bytes.Equal(laterHeldBytes, want) {
		t.Fatalf("the later hand-over is not the Runtime's exact checkpoint files:\n%s\n%s", laterHeldBytes, want)
	}
	laterHeld := write("holder-later.jsonl", laterHeldBytes)

	report, code := auditVerify(t, bin, laterSnapshot, firstHeld, laterHeld)
	if code != 0 || report.Status != "valid" || report.Scope != "checkpoint" || report.Lines != 4 || report.Coverage.Witnessed != 4 || report.Coverage.Unwitnessed != 0 ||
		report.Held == nil || report.Held.Supplied != 4 || report.Held.Matched != 4 || report.Held.Failed != 0 || len(report.Findings) != 0 {
		t.Fatalf("the later snapshot was not verified against both independently retained batches: %+v %d", report, code)
	}
}

// The probe asks the Runtime the runs execute, and takes only the Runtime's
// own answer for an unknown command or flag as the command's absence. A copy
// of the Runtime under test of mode 0600, which cannot be executed, fails the
// probe, and is never taken for a Runtime without the command; Runner runs its
// pinned copy of that file, and the probe of that copy answers as the probe of
// the Runtime under test's pinned copy does. A Runtime that fails otherwise
// fails the probe.
func TestTheAuditVerifyProbeTakesOnlyTheRuntimesAnswerForAbsence(t *testing.T) {
	answerOf := func(cfg Config) string {
		t.Helper()
		s, e := Open(cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		release := testRelease(t, s)
		absent, e := probeAuditVerify(pinnedRuntime(s, release), t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		return absent
	}
	cfg := testConfig(t)
	want := answerOf(cfg)
	b, e := os.ReadFile(cfg.Runtime)
	if e != nil {
		t.Fatal(e)
	}
	unrunnable := filepath.Join(t.TempDir(), "jpack")
	if e = os.WriteFile(unrunnable, b, 0600); e != nil {
		t.Fatal(e)
	}
	if absent, e := probeAuditVerify(unrunnable, t.TempDir()); e == nil || !strings.Contains(e.Error(), "could not be run") {
		t.Fatalf("a Runtime that cannot be run was not refused: absent=%q %v", absent, e)
	}
	again := testConfig(t)
	again.Runtime = unrunnable
	if got := answerOf(again); got != want {
		t.Fatalf("the pinned copy of the Runtime of mode 0600 answers %q, not %q", got, want)
	}

	released := `{"outputVersion":"2","tool":{"name":"jpack","version":"0.25.0"},"command":"jpack","status":"error","diagnostics":[{"code":"JPS-INVOCATION-ARGUMENTS","codeStability":"provisional","layer":"operation","severity":"error","instancePath":"","message":"unknown flag: --trail"}]}`
	for name, c := range map[string]struct {
		answer string
		status int
		absent string
	}{
		"as Runtime 0.24.0 and 0.25.0 answer":      {released, 3, "unknown flag: --trail"},
		"without the audit command":                {strings.Replace(released, "unknown flag: --trail", `unknown command \"audit\" for \"jpack\"`, 1), 3, `unknown command "audit" for "jpack"`},
		"for another flag":                         {strings.Replace(released, "--trail", "--format", 1), 3, ""},
		"for an unknown flag, with another status": {released, 1, ""},
		"for another invocation error":             {strings.Replace(released, "JPS-INVOCATION-ARGUMENTS", "JPS-INVOCATION-INPUT", 1), 3, ""},
		"for a trail it could not read":            {`{"command":"audit verify","status":"error","diagnostics":[{"code":"JPS-AUDIT-IO","message":"the trail could not be read"}]}`, 1, ""},
		"with no report":                           {"", 1, ""},
		"with a report of an invalid chain":        {`{"command":"audit verify","status":"invalid","scope":"checkpoint"}`, 1, ""},
		"with a report that is not of the chain":   {`{"command":"audit verify","status":"valid","scope":"one-supplied-chain"}`, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := filepath.Join(t.TempDir(), "answering-runtime")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '%s'\nexit %d\n", c.answer, c.status)
			if e := os.WriteFile(runtime, []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			absent, e := probeAuditVerify(runtime, t.TempDir())
			if c.absent != "" && (e != nil || absent != c.absent) {
				t.Fatalf("the Runtime's answer for an unknown command or flag was not taken: %q %v", absent, e)
			}
			if c.absent == "" && e == nil {
				t.Fatalf("a Runtime that failed otherwise was taken for one without the command: %q", absent)
			}
		})
	}
}
