package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// exportedRun completes one mapping v2 run with the real Runtime, through the
// runner's HTTP API, and returns its verification export. Its mapping reads
// the case only, and the case has a value for each of its three targets.
func exportedRun(t *testing.T) (bundle map[string]json.RawMessage, digest string) {
	t.Helper()
	return exportedRunOf(t, `{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
}

// exportedRunOf is exportedRun with the case given.
func exportedRunOf(t *testing.T, kase string) (bundle map[string]json.RawMessage, digest string) {
	t.Helper()
	return exportedRunWith(t, caseInput(kase), nil)
}

// caseInput is a mapping v2 input that reads the case given only: its request
// and two evidence requirements.
func caseInput(kase string) runner.Input {
	return runner.Input{Source: &runner.SourceInput{
		Mapping: runner.InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &runner.CaseMapping{
			Facts:    []runner.FactMapping{{Target: "/request", Source: "/facts/request"}},
			Evidence: []runner.EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}},
		}},
		Case: json.RawMessage(kase),
	}}
}

// exportedRunWith completes one run of input as its release's sample, with the
// installation's trusted profiles given, and returns its verification export.
func exportedRunWith(t *testing.T, input runner.Input, profiles []runner.InputProfile) (bundle map[string]json.RawMessage, digest string) {
	t.Helper()
	bundle, digest, _ = exportedRunIn(t, input, profiles, "", "")
	return bundle, digest
}

// exportedRunAs is exportedRunOf with the pack's identifier replaced, unless
// packID is empty, and the export asked for with query. It also returns the
// record the Runtime wrote to the run's attempt audit trail: its line, without
// the newline that ends it.
func exportedRunAs(t *testing.T, kase, packID, query string) (bundle map[string]json.RawMessage, digest string, line []byte) {
	t.Helper()
	return exportedRunIn(t, caseInput(kase), nil, packID, query)
}

// exportedRunIn is exportedRunWith with exportedRunAs's pack identifier, query
// and returned line.
func exportedRunIn(t *testing.T, input runner.Input, profiles []runner.InputProfile, packID, query string) (bundle map[string]json.RawMessage, digest string, line []byte) {
	t.Helper()
	e := exportedRunFull(t, input, profiles, packID, query)
	return e.bundle, e.digest, e.line
}

// exported is one completed run's verification export, its trusted release
// digest, the record's line its attempt's audit trail holds, and the
// installation's chain of runs as GET /v1/run-chain served it after the run.
type exported struct {
	bundle map[string]json.RawMessage
	digest string
	line   []byte
	chain  []byte
	// dir is the store's state directory.
	dir string
}

// exportedRunFull is exportedRunIn, with the installation's chain of runs.
func exportedRunFull(t *testing.T, input runner.Input, profiles []runner.InputProfile, packID, query string) (out exported) {
	t.Helper()
	return exportedRunKeyed(t, input, profiles, packID, query, "")
}

// exportedRunKeyed is exportedRunFull, with the installation's signing key
// given, unless it is empty.
func exportedRunKeyed(t *testing.T, input runner.Input, profiles []runner.InputProfile, packID, query, signingKey string) (out exported) {
	t.Helper()
	var bundle map[string]json.RawMessage
	var digest string
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := runner.Open(runner.Config{Dir: dir, Runtime: bin, Workspace: "test-workspace", Owner: "local-owner", InputProfiles: profiles, SigningKey: signingKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := s.Handler("test")
	call := func(method, path string, body any, key string, want int) json.RawMessage {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatal(method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	pack, err := os.ReadFile("../../internal/runner/testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	if packID != "" {
		pack = bytes.Replace(pack, []byte(`"id": "https://example.invalid/judgment-packs/data-request-intake-triage",`), []byte(`"id": "`+packID+`",`), 1)
	}
	var id struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	json.Unmarshal(call("POST", "/v1/previews", map[string]any{"pack": string(pack), "input": input}, "", 201), &id)
	json.Unmarshal(call("POST", "/v1/jobs", map[string]any{"name": "Verified", "releaseId": id.ID, "reviewed": true}, "", 201), &id)
	json.Unmarshal(call("POST", "/v1/jobs/"+id.ID+"/runs", input, "one", 202), &id)
	for deadline := time.Now().Add(10 * time.Second); id.State != "completed"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) || id.State == "failed" || id.State == "interrupted" {
			t.Fatal("run did not complete:", id.State)
		}
		json.Unmarshal(call("GET", "/v1/runs/"+id.ID, nil, "", 200), &id)
	}
	if err = json.Unmarshal(call("GET", "/v1/runs/"+id.ID+"/verification"+query, nil, "", 200), &bundle); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(bundle["releaseDigest"], &digest); err != nil {
		t.Fatal(err)
	}
	trail, err := os.ReadFile(filepath.Join(dir, "attempts", id.ID, "audit", "evaluations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return exported{bundle, digest, bytes.TrimSuffix(trail, []byte("\n")), call("GET", "/v1/run-chain", nil, "", 200), dir}
}

// verify runs the command on an export, as an operator would, with no trusted
// profiles: the fixture's mapping reads its case only.
func verify(t *testing.T, bundle map[string]json.RawMessage, digest string, extra ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	file, profiles := filepath.Join(dir, "run.json"), filepath.Join(dir, "profiles.json")
	raw, _ := json.Marshal(bundle)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profiles, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	return verifyFiles(file, profiles, digest, extra...)
}

// verifyFiles runs the command on an export and profiles already on disk, and
// returns its report on standard output, its line on standard error and its error.
func verifyFiles(file, profiles, digest string, extra ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := verifyRun(append([]string{"--file", file, "--profiles", profiles, "--release-digest", digest}, extra...), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

// report is the report line verify-run writes of a version-2 export for a
// status, a retained disposition, a scope, and counts of asserted, record and
// generated targets.
func report(status, disposition, scope string, asserted, record, generated int) string {
	return fmt.Sprintf(`{"exactBytes":"not-in-export","exportVersion":2,"retainedDisposition":%q,"scope":%q,"status":%q,"targetsByClass":{"asserted":%d,"record":%d,"generated":%d}}`+"\n", disposition, scope, status, asserted, record, generated)
}

// notInExport is how the line on standard error ends for a version-2 export.
const notInExport = " Exact-byte checks were not possible: a version-2 export carries no original bytes of the audit record, so nothing here gives the digest a gateway receipt names it by."

const (
	inputsOnly = "retained input derivation and audit binding; not sealed-session completeness or policy truth"
	reExecuted = "retained input derivation, audit binding, and the retained disposition against a re-execution by the release's Runtime; not sealed-session completeness or policy truth"
)

// verified-inputs must not be readable as covering the result. A retained
// disposition changed after the run is not detected, and the report says it was
// not checked, in the members a program reads and in the sentence a person reads.
func TestVerifyRunSaysTheDispositionWasNotChecked(t *testing.T) {
	bundle, digest := exportedRun(t)
	var run map[string]json.RawMessage
	if err := json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	if bytes.Equal(changed, run["result"]) {
		t.Fatal("fixture has no disposition to change")
	}
	for name, result := range map[string]json.RawMessage{"retained": run["result"], "changed after the run": changed} {
		t.Run(name, func(t *testing.T) {
			run["result"] = result
			bundle["run"], _ = json.Marshal(run)
			out, human, err := verify(t, bundle, digest)
			if err != nil {
				t.Fatal(err)
			}
			if out != report("verified-inputs", "not-checked", inputsOnly, 3, 0, 0) {
				t.Fatal(out)
			}
			if !strings.Contains(human, "disposition was not checked") || !strings.Contains(human, "does not say the run decided what its record says") {
				t.Fatal("human output:", human)
			}
		})
	}
}

// With the release's Runtime the report says the disposition was checked, and
// a disposition changed after the run is a failure with a report of its own.
func TestVerifyRunReExecutesWithTheReleaseRuntime(t *testing.T) {
	bundle, digest := exportedRun(t)
	t.Setenv("TMPDIR", t.TempDir())
	runtime := "--runtime=" + os.Getenv("JPACK_TEST_BIN")
	out, human, err := verify(t, bundle, digest, runtime)
	if err != nil || out != report("verified-disposition", "matches-re-execution", reExecuted, 3, 0, 0) {
		t.Fatal(err, out)
	}
	if human != "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true. The run's inputs are the operator's own: nothing here checks them against a source."+notInExport+"\n" {
		t.Fatal("human output:", human)
	}
	var run map[string]json.RawMessage
	if err = json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	run["result"] = bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	bundle["run"], _ = json.Marshal(run)
	out, human, err = verify(t, bundle, digest, runtime)
	if !errors.Is(err, runner.ErrDispositionDiffers) || out != report("disposition-differs", "differs-from-re-execution", reExecuted, 3, 0, 0) || human != "" {
		t.Fatal(err, out, human)
	}
}

// verified-inputs and verified-disposition read the same whether a run's
// inputs were derived from signed receipts or asserted. When they were
// asserted, the report counts them and the line on standard error says so; and
// --require-sourced refuses the run, before any re-execution.
func TestVerifyRunSaysWhenInputsAreAsserted(t *testing.T) {
	bundle, digest := exportedRun(t)
	out, human, err := verify(t, bundle, digest)
	if err != nil || out != report("verified-inputs", "not-checked", inputsOnly, 3, 0, 0) {
		t.Fatal(err, out)
	}
	if human != "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. The run's inputs are the operator's own: nothing here checks them against a source."+notInExport+"\n" {
		t.Fatal("human output:", human)
	}
	// The executable named does not exist: a refusal after the re-execution
	// would fail on it, not refuse the inputs.
	absent := "--runtime=" + filepath.Join(t.TempDir(), "absent")
	for name, extra := range map[string][]string{"inputs only": {"--require-sourced"}, "with a runtime": {"--require-sourced", absent}} {
		t.Run(name, func(t *testing.T) {
			out, human, err := verify(t, bundle, digest, extra...)
			if !errors.Is(err, errInputsAsserted) || err.Error() != "inputs-asserted: the run's inputs are all asserted, 3 of 3, and --require-sourced refuses them: nothing here checks them against a source" {
				t.Fatal(err)
			}
			if out != report("inputs-asserted", "not-checked", inputsOnly, 3, 0, 0) || human != "" {
				t.Fatal(out, human)
			}
		})
	}
}

// A run whose inputs were all derived from signed receipts reads as before,
// with its counts, and --require-sourced accepts it. The export was written by
// an earlier Runner; its case parameter, which chose the request, is not counted.
func TestVerifyRunAcceptsInputsDerivedFromReceipts(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-before-calculators/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--require-sourced"}} {
		out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)), extra...)
		if err != nil || out != report("verified-inputs", "not-checked", inputsOnly, 0, 3, 0) {
			t.Fatal(extra, err, out)
		}
		if human != "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says."+notInExport+"\n" {
			t.Fatal(extra, "human output:", human)
		}
	}
}

// What the line on standard error adds, and what --require-sourced refuses,
// for every mix of classes: all asserted, some, one, or none.
func TestAssertedInputsAreNamedAndRefusedByCount(t *testing.T) {
	for _, c := range []struct {
		classes  runner.InputClasses
		sentence string
		refusal  string
	}{
		{runner.InputClasses{Asserted: 3}, " The run's inputs are the operator's own: nothing here checks them against a source.", "inputs-asserted: the run's inputs are all asserted, 3 of 3, and --require-sourced refuses them: nothing here checks them against a source"},
		{runner.InputClasses{Asserted: 1}, " The run's inputs are the operator's own: nothing here checks them against a source.", "inputs-asserted: the run's inputs are all asserted, 1 of 1, and --require-sourced refuses them: nothing here checks them against a source"},
		{runner.InputClasses{Asserted: 2, Generated: 1}, " 2 of the run's 3 inputs are the operator's own: nothing here checks those against a source.", "inputs-asserted: 2 of the run's 3 inputs are asserted, and --require-sourced refuses them: nothing here checks them against a source"},
		{runner.InputClasses{Asserted: 1, Record: 2}, " 1 of the run's 3 inputs is the operator's own: nothing here checks it against a source.", "inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source"},
		{runner.InputClasses{Record: 2, Generated: 1}, "", ""},
		{runner.InputClasses{Generated: 3}, "", ""},
		{runner.InputClasses{}, "", ""},
	} {
		err := requireSourcedInputs(c.classes)
		if assertedSentence(c.classes) != c.sentence || c.refusal == "" && err != nil || c.refusal != "" && (!errors.Is(err, errInputsAsserted) || err.Error() != c.refusal) {
			t.Fatal(c.classes, assertedSentence(c.classes), err)
		}
	}
}

// A target the run has no value for is counted as its source's class, so a
// case that leaves every mapped target out is still the operator's say, and
// --require-sourced still refuses it.
func TestVerifyRunRefusesAbsentAssertedInputs(t *testing.T) {
	bundle, digest := exportedRunOf(t, `{}`)
	var run struct {
		Input struct {
			Preparation struct {
				Lineage []struct {
					Present bool `json:"present"`
				} `json:"lineage"`
			} `json:"preparation"`
		} `json:"input"`
	}
	if err := json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	for _, l := range run.Input.Preparation.Lineage {
		if l.Present {
			t.Fatal("the fixture has a value for a target")
		}
	}
	out, human, err := verify(t, bundle, digest)
	if err != nil || out != report("verified-inputs", "not-checked", inputsOnly, 3, 0, 0) || !strings.HasSuffix(human, " The run's inputs are the operator's own: nothing here checks them against a source."+notInExport+"\n") {
		t.Fatal(err, out, human)
	}
	out, human, err = verify(t, bundle, digest, "--require-sourced")
	if !errors.Is(err, errInputsAsserted) || out != report("inputs-asserted", "not-checked", inputsOnly, 3, 0, 0) || human != "" {
		t.Fatal(err, out, human)
	}
}
