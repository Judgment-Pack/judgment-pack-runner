package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// verify-run reports each preparation vector (vectors/preparation, runner
// #75) as the vectors' manifest records it: the report on standard output,
// byte for byte, and the refusal, with nothing else on standard error. The
// manifest's digest, and the vectors' bytes against the Runner's own
// computation, are held by TestPreparationVectorsAreByteExact
// (internal/runner). No Runtime is run: no vector asks for a re-execution.
func TestVerifyRunReportsEachPreparationVector(t *testing.T) {
	dir := "../../vectors/preparation/"
	raw, err := os.ReadFile(dir + "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Vectors []struct {
			Name      string `json:"name"`
			VerifyRun struct {
				Arguments []string `json:"arguments"`
				Exit      int      `json:"exit"`
				Status    *string  `json:"status"`
				Report    *string  `json:"report"`
				Error     *string  `json:"error"`
			} `json:"verifyRun"`
		} `json:"vectors"`
	}
	if err = json.Unmarshal(raw, &m); err != nil || len(m.Vectors) == 0 {
		t.Fatal("no vectors:", err)
	}
	for _, v := range m.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			want := v.VerifyRun
			// File arguments name the vector's files, relative to its directory.
			args := append([]string(nil), want.Arguments...)
			for i := 1; i < len(args); i++ {
				switch args[i-1] {
				case "--file", "--profiles", "--expect", "--chain":
					args[i] = filepath.Join(dir, v.Name, args[i])
				}
			}
			var stdout, stderr bytes.Buffer
			err := verifyRun(args, &stdout, &stderr)
			report := ""
			if want.Report != nil {
				report = *want.Report + "\n"
			}
			if stdout.String() != report {
				t.Fatalf("verify-run reports\n%s\nnot\n%s", stdout.String(), report)
			}
			if want.Status != nil {
				var status struct {
					Status string `json:"status"`
				}
				if json.Unmarshal(stdout.Bytes(), &status) != nil || status.Status != *want.Status {
					t.Fatalf("the report's status is not %s", *want.Status)
				}
			}
			if want.Error == nil {
				if err != nil || want.Exit != 0 || stderr.Len() == 0 {
					t.Fatal("verify-run refuses a vector the manifest says it verifies:", err, want.Exit, stderr.String())
				}
				return
			}
			// Only the finding of a disposition that differs from the audit record
			// says anything on standard error: what was checked beside it.
			if err == nil || err.Error() != *want.Error || want.Exit != 1 || (stderr.Len() != 0) != (want.Status != nil && *want.Status == "audit-disposition-mismatch" && want.Report != nil && strings.Contains(*want.Report, `"targetsByClass"`)) {
				t.Fatalf("verify-run refuses with %v and says %q, not %q", err, stderr.String(), *want.Error)
			}
		})
	}
}

// Beside a disposition finding the chain check stands on its own. On the
// disposition-differs-from-audit vector, copied to a scratch directory: a held
// checkpoint that is not the run's entry's fails the chain check, and the
// sentences claim no agreement; a held checkpoints file that is missing, and a
// run with no chain entry under --require-witnessed, leave the checked
// preparation's report and sentences in place, the report with no runChain
// member and a sentence saying the check did not run; the finding, the status
// and the exit are the finding's throughout.
func TestVerifyRunKeepsThePreparationBesideAChainCheckThatFailsOrDoesNotRun(t *testing.T) {
	src := "../../vectors/preparation/disposition-differs-from-audit/"
	dir := t.TempDir()
	for _, name := range []string{"run.json", "profiles.json", "release-digest.txt", "public-key.txt", "checkpoints.jsonl"} {
		b, err := os.ReadFile(src + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	args := func(held string) []string {
		return []string{"--public-key", read("public-key.txt"), "--require-signed", "--expect", held, "--require-witnessed", "--require-bound"}
	}
	verify := func(held string) (string, string, error) {
		return verifyFiles(filepath.Join(dir, "run.json"), filepath.Join(dir, "profiles.json"), read("release-digest.txt"), args(held)...)
	}
	checked := func(out, said string) {
		t.Helper()
		for _, want := range []string{`"binding":{"status":"matches"}`, `"targetsByClass":{"asserted":0,"record":3,"generated":0}`, `"unsignedFreshness":`, `"status":"audit-disposition-mismatch"`, `"retainedDisposition":"differs-from-audit"`} {
			if !strings.Contains(out, want) {
				t.Fatalf("the report lacks %s: %s", want, out)
			}
		}
		for _, want := range []string{"audit-disposition-mismatch: the retained disposition differs from the audit record. The run's preparation was verified independently of it.", "freshness for 1 source were judged"} {
			if !strings.Contains(said, want) {
				t.Fatalf("the line lacks %q: %s", want, said)
			}
		}
	}
	// A held checkpoint that is not the entry's: the check failed.
	held := filepath.Join(dir, "checkpoints.jsonl")
	raw := read("checkpoints.jsonl")
	altered := strings.Replace(raw, "sha256:ae3a1b35215f61316e8b6343cb51c2b12f1e6bfbf63eab414ca3394750565dd1", "sha256:"+strings.Repeat("0", 64), 1)
	if altered == raw {
		t.Fatal("fixture has no checkpoint digest to alter")
	}
	if err := os.WriteFile(held, []byte(altered+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, said, err := verify(held)
	if !errors.Is(err, runner.ErrAuditDispositionMismatch) {
		t.Fatal(err)
	}
	checked(out, said)
	if !strings.Contains(out, `"runChain":{`) || !strings.Contains(out, `"status":"invalid"`) || !strings.Contains(said, "The check of the export's entry in the installation's chain of runs failed") || strings.Contains(said, "agrees") || strings.Contains(said, "cover it") || strings.Contains(said, "consistent") || strings.Contains(said, "binds this run") {
		t.Fatal("a failed chain check is reported as a success:", out, said)
	}
	// Held checkpoints that cannot be read: the check did not run.
	out, said, err = verify(filepath.Join(dir, "missing.jsonl"))
	if !errors.Is(err, runner.ErrAuditDispositionMismatch) || !strings.Contains(err.Error(), "missing.jsonl") {
		t.Fatal(err)
	}
	checked(out, said)
	if strings.Contains(out, "runChain") || !strings.Contains(said, "The check of the export's entry in the installation's chain of runs did not run:") || strings.Contains(said, "agrees") || strings.Contains(said, "cover it") {
		t.Fatal("a chain check that did not run left a chain result:", out, said)
	}
	// A run with no chain entry, under --require-witnessed: an export
	// recorded before Runner chained its runs, its disposition changed.
	old := "../../internal/runner/testdata/mapping-v2-before-run-chain/"
	rawRun, err := os.ReadFile(old + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle, run map[string]json.RawMessage
	if err = json.Unmarshal(rawRun, &bundle); err != nil || json.Unmarshal(bundle["run"], &run) != nil {
		t.Fatal(err)
	}
	result := bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	if bytes.Equal(result, run["result"]) {
		t.Fatal("fixture has no disposition to change")
	}
	run["result"] = result
	bundle["run"], _ = json.Marshal(run)
	changed, _ := json.Marshal(bundle)
	file := filepath.Join(dir, "unchained.json")
	if err = os.WriteFile(file, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, _ := os.ReadFile(old + "release-digest.txt")
	out, said, err = verifyFiles(file, old+"profiles.json", strings.TrimSpace(string(digest)), "--require-witnessed")
	if !errors.Is(err, runner.ErrAuditDispositionMismatch) || !errors.Is(err, runner.ErrNoChainEntry) {
		t.Fatal(err)
	}
	if strings.Contains(out, "runChain") || !strings.Contains(out, `"targetsByClass"`) || !strings.Contains(out, `"status":"audit-disposition-mismatch"`) || !strings.Contains(said, "did not run") || !strings.Contains(said, "The run's preparation was verified independently of it.") {
		t.Fatal(out, said)
	}
}
