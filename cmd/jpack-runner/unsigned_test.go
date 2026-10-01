package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// A run whose rules read parameters no signed request commits reports them,
// and says so; --require-sourced refuses the targets whose rule reads a case
// parameter, and not those whose rule reads runAt alone. The export's vendor
// rule reads runAt and committed parameters; its registry rule reads region,
// which no request commits, and vendorId, which vendor's request commits.
func TestVerifyRunReportsUnsignedParameters(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-unsigned-parameters/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(string(trusted))
	unsigned := `,"unsignedParameters":[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"}],"targets":1}]}` + "\n"
	withUnsigned := func(report string) string { return strings.TrimSuffix(report, "}\n") + unsigned }
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", digest)
	if err != nil || out != withUnsigned(report("verified-inputs", "not-checked", inputsOnly, 0, 3, 0)) {
		t.Fatal(err, out)
	}
	if human != "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says. 1 of the run's 3 inputs was derived by a rule that reads a parameter from the case or a local file, which no signed request commits: nothing here checks that parameter against a source. 2 of the run's 3 inputs were derived by a rule that reads runAt, which rests on the export's own verification time.\n" {
		t.Fatal("human output:", human)
	}
	out, human, err = verifyFiles(dir+"run.json", dir+"profiles.json", digest, "--require-sourced")
	if !errors.Is(err, errParametersUnsigned) || err.Error() != "parameters-unsigned: 1 of the run's 3 inputs was derived by a rule that reads a parameter from the case or a local file, which no signed request commits, and --require-sourced refuses it: nothing here checks that parameter against a source" {
		t.Fatal(err)
	}
	if out != withUnsigned(report("parameters-unsigned", "not-checked", inputsOnly, 0, 3, 0)) || human != "" {
		t.Fatal(out, human)
	}
}

// What the line on standard error adds, and what --require-sourced refuses, for
// each kind of unsigned parameter and mix of sources.
func TestUnsignedParametersAreNamedAndRefusedByKind(t *testing.T) {
	param := func(name, kind string) runner.UnsignedParameter {
		return runner.UnsignedParameter{Name: name, Kind: kind}
	}
	source := func(targets int, ps ...runner.UnsignedParameter) runner.UnsignedParameters {
		return runner.UnsignedParameters{Source: "s", Parameters: ps, Targets: targets}
	}
	caseRead := " derived by a rule that reads a parameter from the case or a local file, which no signed request commits: nothing here checks that parameter against a source."
	clockRead := " derived by a rule that reads runAt, which rests on the export's own verification time."
	refusal := func(inputs, them string) string {
		return "parameters-unsigned: " + inputs + " derived by a rule that reads a parameter from the case or a local file, which no signed request commits, and --require-sourced refuses " + them + ": nothing here checks that parameter against a source"
	}
	for name, c := range map[string]struct {
		classes  runner.InputClasses
		unsigned []runner.UnsignedParameters
		sentence string
		status   string
		refusal  string
	}{
		"none": {runner.InputClasses{Record: 3}, nil, "", "", ""},
		"runAt alone": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(3, param("runAt", "runAt"))},
			" All of the run's inputs were" + clockRead, "", ""},
		"a case parameter": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(3, param("region", "case"))},
			" All of the run's inputs were" + caseRead, "parameters-unsigned", refusal("All of the run's inputs were", "them")},
		"a local file's fact": {runner.InputClasses{Asserted: 1, Record: 2}, []runner.UnsignedParameters{source(2, param("ref", "local-file"))},
			" 2 of the run's 3 inputs were" + caseRead, "inputs-asserted", "inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source"},
		"a case parameter and runAt in one rule": {runner.InputClasses{Record: 2, Generated: 2}, []runner.UnsignedParameters{source(2, param("region", "case"), param("runAt", "runAt"))},
			" 2 of the run's 4 inputs were" + caseRead + " 2 of the run's 4 inputs were" + clockRead, "parameters-unsigned", refusal("2 of the run's 4 inputs were", "them")},
		"runAt in one source, a case parameter in another": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(2, param("runAt", "runAt")), source(1, param("region", "case"))},
			" 1 of the run's 3 inputs was" + caseRead + " 2 of the run's 3 inputs were" + clockRead, "parameters-unsigned", refusal("1 of the run's 3 inputs was", "it")},
		"one input of one": {runner.InputClasses{Generated: 1}, []runner.UnsignedParameters{source(1, param("region", "case"))},
			" All of the run's inputs were" + caseRead, "parameters-unsigned", refusal("All of the run's inputs were", "them")},
		"asserted as well": {runner.InputClasses{Asserted: 1, Record: 1}, []runner.UnsignedParameters{source(1, param("region", "case"))},
			" 1 of the run's 2 inputs was" + caseRead, "inputs-asserted", "inputs-asserted: 1 of the run's 2 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := unsignedSentence(c.classes, c.unsigned); got != c.sentence {
				t.Fatal(got)
			}
			status, err := refuseUnsourced(c.classes, c.unsigned)
			if status != c.status || c.refusal == "" && err != nil || c.refusal != "" && (err == nil || err.Error() != c.refusal) {
				t.Fatal(status, err)
			}
			if status == "parameters-unsigned" && !errors.Is(err, errParametersUnsigned) || status == "inputs-asserted" && !errors.Is(err, errInputsAsserted) {
				t.Fatal("refusal does not wrap its status:", err)
			}
		})
	}
}

// With the release's Runtime, verified-disposition and disposition-differs
// report the same, and the line says the same. The export's release froze the
// released Runtime v0.24.0 by its digest; a Runtime built otherwise, as CI
// builds it from source, has another, and this is skipped.
func TestVerifyRunReportsUnsignedParametersWithTheReleaseRuntime(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	dir := "../../internal/runner/testdata/mapping-v2-unsigned-parameters/"
	raw, err := os.ReadFile(dir + "run.json")
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var bundle map[string]json.RawMessage
	var release struct {
		RuntimeDigest string `json:"runtimeDigest"`
	}
	if err = json.Unmarshal(raw, &bundle); err != nil || json.Unmarshal(bundle["release"], &release) != nil {
		t.Fatal("export:", err)
	}
	if release.RuntimeDigest != fmt.Sprintf("sha256:%x", sha256.Sum256(executable)) {
		t.Skip("JPACK_TEST_BIN is not the Runtime this export's release froze")
	}
	t.Setenv("TMPDIR", t.TempDir())
	unsigned := `,"unsignedParameters":[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"}],"targets":1}]}` + "\n"
	withUnsigned := func(report string) string { return strings.TrimSuffix(report, "}\n") + unsigned }
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)), "--runtime="+bin)
	if err != nil || out != withUnsigned(report("verified-disposition", "matches-re-execution", reExecuted, 0, 3, 0)) {
		t.Fatal(err, out)
	}
	if human != "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true. 1 of the run's 3 inputs was derived by a rule that reads a parameter from the case or a local file, which no signed request commits: nothing here checks that parameter against a source. 2 of the run's 3 inputs were derived by a rule that reads runAt, which rests on the export's own verification time.\n" {
		t.Fatal("human output:", human)
	}
	var run map[string]json.RawMessage
	if err = json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	run["result"] = bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	bundle["run"], _ = json.Marshal(run)
	changed := filepath.Join(t.TempDir(), "run.json")
	if raw, err = json.Marshal(bundle); err != nil || os.WriteFile(changed, raw, 0600) != nil {
		t.Fatal(err)
	}
	out, human, err = verifyFiles(changed, dir+"profiles.json", strings.TrimSpace(string(trusted)), "--runtime="+bin)
	if !errors.Is(err, runner.ErrDispositionDiffers) || out != withUnsigned(report("disposition-differs", "differs-from-re-execution", reExecuted, 0, 3, 0)) || human != "" {
		t.Fatal(err, out, human)
	}
}
