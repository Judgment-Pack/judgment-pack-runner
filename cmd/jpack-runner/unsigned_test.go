package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// The unsigned parameters of the two-source run that unsignedInput builds, and
// testdata/mapping-v2-unsigned-parameters retains, and what the line says of
// them: vendor's rule reads runAt; registry's reads region, which no request
// carries, and vendorId, which its request carries only in text that names
// another parameter.
const (
	unsignedMember = `,"unsignedParameters":[{"source":"vendor","parameters":[{"name":"runAt","kind":"runAt"}],"targets":2},{"source":"registry","parameters":[{"name":"region","kind":"case"},{"name":"vendorId","kind":"ambiguous-text"}],"targets":1}]}` + "\n"
	operatorRead   = " derived by a rule that reads a parameter resting on the operator's say, which its source's receipt does not commit: nothing here checks that parameter against a source."
	clockRead      = " derived by a rule that reads runAt, or a value derived from it, which rests on the export's own verification time."
	unsignedLine   = " 1 of the run's 3 inputs was" + operatorRead + " 2 of the run's 3 inputs were" + clockRead + notInExport + "\n"
	unsignedRefuse = "parameters-unsigned: 1 of the run's 3 inputs was derived by a rule that reads a parameter resting on the operator's say, which its source's receipt does not commit, and --require-sourced refuses it: nothing here checks that parameter against a source"
	inputsLine     = "verified-inputs: the run's inputs match its record. Its retained disposition matches its audit record; the evaluator was not replayed."
	dispositionLn  = "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true."
)

// withUnsigned is a report line with the unsigned parameters above added.
func withUnsigned(report string) string { return strings.TrimSuffix(report, "}\n") + unsignedMember }

// signer is an installation's trusted profile and its gateway's key, which signs
// version 3 acquisition receipts as the runner's own tests sign them.
type signer struct {
	profile runner.InputProfile
	key     ed25519.PrivateKey
	at      time.Time
}

func newSigner(t *testing.T) signer {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "postgresql://vendor-records"
	return signer{runner.InputProfile{ID: "vendors", PublicKey: hex.EncodeToString(public), Class: "record", Source: "postgres/live", Authority: "test", Shape: "mcp", Adapter: runner.AdapterPin{Name: "dbhub", Version: "1.2.3", Digest: sha256Of([]byte("adapter-image"))}, Endpoint: &endpoint, Tools: []string{"execute_sql", "lookup"}}, key, time.Now().UTC().Truncate(time.Second)}
}

func sha256Of(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func canonical(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	b, err := derivation.Canon(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// source is an operation of the signer's profile.
func (s signer) source(t *testing.T, name, arguments string, read runner.SourceRead) runner.MappingSource {
	return runner.MappingSource{Name: name, Kind: "operation", Profile: s.profile.ID, ProfileDigest: sha256Of(canonical(t, s.profile)), MaxAge: 300, Arguments: json.RawMessage(arguments), Read: read}
}

// response is the signed answer result to the request args, the index-th call
// of one session.
func (s signer) response(t *testing.T, args, result string, index int) runner.SourceValue {
	t.Helper()
	salt := bytes.Repeat([]byte{byte(index + 1)}, 32)
	var previous any
	if index > 0 {
		previous = strings.Repeat("a", 128)
	}
	public, _ := hex.DecodeString(s.profile.PublicKey)
	id := sha256.Sum256(public)
	stamp := s.at.Format(time.RFC3339)
	receipt := map[string]any{"receiptVersion": "3", "kind": "acquisition", "sessionId": "mapping.session", "callIndex": index, "prevSignature": previous, "source": s.profile.Source, "resultDigest": sha256Of(canonical(t, json.RawMessage(result))), "servedAt": stamp, "authority": s.profile.Authority, "keyId": hex.EncodeToString(id[:])[:32], "argumentsCommitment": sha256Of(append(append(salt, "args:"...), canonical(t, json.RawMessage(args))...)), "caller": nil, "acquisition": map[string]any{"shape": s.profile.Shape, "adapter": s.profile.Adapter, "endpoint": s.profile.Endpoint, "statement": nil, "snapshot": "query-123", "peerIdentity": nil, "schema": nil, "upstreamToken": nil, "observedAt": stamp}}
	receipt["signature"] = hex.EncodeToString(ed25519.Sign(s.key, append([]byte("judgment-pack-gateway/receipt/3:"), canonical(t, receipt)...)))
	raw, err := json.Marshal(map[string]any{"receipt": receipt, "result": json.RawMessage(result), "salts": map[string]string{"args": hex.EncodeToString(salt)}})
	if err != nil {
		t.Fatal(err)
	}
	return runner.SourceValue{Response: raw}
}

// rule is a rule over the triage pack's targets: when holds, the request fact
// if asked for and the evidence named are present; otherwise it is unknown.
func rule(parameters, when string, fact bool, evidence ...string) runner.SourceRead {
	facts := `[]`
	if fact {
		facts = `[{"pointer":"/request","from":"/facts/request"}]`
	}
	present, unknown := map[string]string{}, map[string]string{}
	for _, e := range evidence {
		present[e], unknown[e] = "present", "unknown"
	}
	p, _ := json.Marshal(present)
	u, _ := json.Marshal(unknown)
	return runner.SourceRead{Rule: json.RawMessage(`{"ruleVersion":"1","parameters":` + parameters + `,"clauses":[{"when":` + when + `,"claim":{"facts":` + facts + `,"evidence":` + string(p) + `,"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":` + string(u) + `,"acquisitionStatus":"unknown"},"reason":"no-match"}]}`)}
}

const facts = `{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}}`

// unsignedInput is the two-source run of testdata/mapping-v2-unsigned-parameters:
// vendor's request commits vendorId and window, and its rule reads them and
// runAt; registry's request carries vendorId only in text that names suffix,
// and its rule reads vendorId and region.
func unsignedInput(t *testing.T, s signer) runner.Input {
	cases := map[string]runner.Parameter{"vendorId": {Pointer: "/id", Type: "integer"}, "window": {Pointer: "/window", Type: "integer"}, "suffix": {Pointer: "/suffix", Type: "integer"}, "region": {Pointer: "/region", Type: "string"}}
	m := runner.InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &runner.CaseMapping{Parameters: cases, Facts: []runner.FactMapping{}, Evidence: []runner.EvidenceMapping{}}, Sources: []runner.MappingSource{
		s.source(t, "vendor", `{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT * FROM vendors WHERE id = {{vendorId}}"},"window":{"$param":"window"}}}`, rule(`{"vendorId":"integer","window":"integer","runAt":"timestamp"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"freshWithin","field":"/observedAt","asOf":"runAt","maxAge":"window"}]}`, true, "intake-form")),
		s.source(t, "registry", `{"tool":"lookup","arguments":{"key":{"$text":"{{vendorId}}{{suffix}}"}}}`, rule(`{"vendorId":"integer","region":"string"}`, `{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"equalsParam","field":"/region","param":"region"}]}`, false, "sponsor-endorsement")),
	}}
	vendor := `{"id":7,"observedAt":"` + s.at.Format("2006-01-02T15:04:05Z") + `","facts":` + facts + `}`
	return runner.Input{Source: &runner.SourceInput{Mapping: m, Case: json.RawMessage(`{"id":7,"window":86400,"suffix":3,"region":"north"}`), Sources: map[string]runner.SourceValue{
		"vendor":   s.response(t, `{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7","window":86400}}`, vendor, 0),
		"registry": s.response(t, `{"tool":"lookup","arguments":{"key":"73"}}`, `{"id":7,"region":"north"}`, 1),
	}}}
}

// verifyWith runs the command on an export with the profiles given.
func verifyWith(t *testing.T, bundle map[string]json.RawMessage, digest string, profiles []runner.InputProfile, extra ...string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	raw, _ := json.Marshal(bundle)
	trusted, _ := json.Marshal(profiles)
	if os.WriteFile(filepath.Join(dir, "run.json"), raw, 0600) != nil || os.WriteFile(filepath.Join(dir, "profiles.json"), trusted, 0600) != nil {
		t.Fatal("cannot write the export")
	}
	return verifyFiles(filepath.Join(dir, "run.json"), filepath.Join(dir, "profiles.json"), digest, extra...)
}

// A run, completed by whichever Runtime is given, whose rules read parameters
// their receipts do not commit: each report and line says so, with a matching
// and with a differing disposition, and --require-sourced refuses the target
// whose rule reads the operator's parameters, not those that read runAt alone.
func TestVerifyRunReportsUnsignedParametersEndToEnd(t *testing.T) {
	s := newSigner(t)
	bundle, digest := exportedRunWith(t, unsignedInput(t, s), []runner.InputProfile{s.profile})
	profiles := []runner.InputProfile{s.profile}
	t.Setenv("TMPDIR", t.TempDir())
	runtime := "--runtime=" + os.Getenv("JPACK_TEST_BIN")
	out, human, err := verifyWith(t, bundle, digest, profiles)
	if err != nil || out != withUnsigned(report("verified-inputs", "matches-audit", inputsOnly, 0, 3, 0)) || human != inputsLine+unsignedLine {
		t.Fatal(err, out, human)
	}
	out, human, err = verifyWith(t, bundle, digest, profiles, runtime)
	if err != nil || out != withUnsigned(report("verified-disposition", "matches-re-execution", reExecuted, 0, 3, 0)) || human != dispositionLn+unsignedLine {
		t.Fatal(err, out, human)
	}
	out, human, err = verifyWith(t, bundle, digest, profiles, "--require-sourced")
	if !errors.Is(err, errParametersUnsigned) || err.Error() != unsignedRefuse || out != withUnsigned(report("parameters-unsigned", "matches-audit", inputsOnly, 0, 3, 0)) || human != "" {
		t.Fatal(err, out, human)
	}
	var run map[string]json.RawMessage
	if err = json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(run["result"], []byte(`"outcomeId":"proceed"`), []byte(`"outcomeId":"decline-redirect"`), 1)
	if bytes.Equal(changed, run["result"]) {
		t.Fatal("fixture has no disposition to change")
	}
	run["result"] = changed
	bundle["run"], _ = json.Marshal(run)
	out, human, err = verifyWith(t, bundle, digest, profiles, runtime)
	if !errors.Is(err, runner.ErrAuditDispositionMismatch) || out != auditDispositionMismatchReport || human != "" {
		t.Fatal(err, out, human)
	}
}

// A run with an asserted target and a target derived from an unsigned case
// parameter: the report has both members, the line both sentences, and
// --require-sourced refuses it as inputs-asserted.
func TestVerifyRunRefusesAnAssertedTargetBeforeAnUnsignedOne(t *testing.T) {
	s := newSigner(t)
	m := runner.InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"},
		Case:    &runner.CaseMapping{Parameters: map[string]runner.Parameter{"region": {Pointer: "/region", Type: "string"}}, Facts: []runner.FactMapping{{Target: "/request", Source: "/facts/request"}}, Evidence: []runner.EvidenceMapping{}},
		Sources: []runner.MappingSource{s.source(t, "registry", `{"tool":"lookup","arguments":{}}`, rule(`{"region":"string"}`, `{"op":"equalsParam","field":"/region","param":"region"}`, false, "intake-form", "sponsor-endorsement"))}}
	input := runner.Input{Source: &runner.SourceInput{Mapping: m, Case: json.RawMessage(`{"region":"north","facts":` + facts + `}`), Sources: map[string]runner.SourceValue{
		"registry": s.response(t, `{"tool":"lookup","arguments":{}}`, `{"region":"north"}`, 0)}}}
	bundle, digest := exportedRunWith(t, input, []runner.InputProfile{s.profile})
	member := `,"unsignedParameters":[{"source":"registry","parameters":[{"name":"region","kind":"case"}],"targets":2}]}` + "\n"
	both := func(report string) string { return strings.TrimSuffix(report, "}\n") + member }
	out, human, err := verifyWith(t, bundle, digest, []runner.InputProfile{s.profile})
	if err != nil || out != both(report("verified-inputs", "matches-audit", inputsOnly, 1, 2, 0)) || human != inputsLine+" 1 of the run's 3 inputs is the operator's own: nothing here checks it against a source. 2 of the run's 3 inputs were"+operatorRead+notInExport+"\n" {
		t.Fatal(err, out, human)
	}
	out, human, err = verifyWith(t, bundle, digest, []runner.InputProfile{s.profile}, "--require-sourced")
	if !errors.Is(err, errInputsAsserted) || err.Error() != "inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source" || out != both(report("inputs-asserted", "matches-audit", inputsOnly, 1, 2, 0)) || human != "" {
		t.Fatal(err, out, human)
	}
}

// The export in testdata/mapping-v2-unsigned-parameters, which the runner's
// TestVerifyInputsReportsUnsignedParameters wrote from the same two-source run
// as unsignedInput's, verifies without a Runtime and reads the same.
func TestVerifyRunReportsUnsignedParameters(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-unsigned-parameters/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(string(trusted))
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", digest)
	if err != nil || out != withUnsigned(report("verified-inputs", "matches-audit", inputsOnly, 0, 3, 0)) || human != inputsLine+unsignedLine {
		t.Fatal(err, out, human)
	}
	out, human, err = verifyFiles(dir+"run.json", dir+"profiles.json", digest, "--require-sourced")
	if !errors.Is(err, errParametersUnsigned) || err.Error() != unsignedRefuse || out != withUnsigned(report("parameters-unsigned", "matches-audit", inputsOnly, 0, 3, 0)) || human != "" {
		t.Fatal(err, out, human)
	}
}

// With the Runtime that the fixture's release froze, the released v0.24.0,
// verified-disposition reads the same. Any other Runtime has another digest,
// as CI's, built from source, does, and this is then skipped:
// TestVerifyRunReportsUnsignedParametersEndToEnd runs the same with CI's.
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
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", strings.TrimSpace(string(trusted)), "--runtime="+bin)
	if err != nil || out != withUnsigned(report("verified-disposition", "matches-re-execution", reExecuted, 0, 3, 0)) || human != dispositionLn+unsignedLine {
		t.Fatal(err, out, human)
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
	refusal := func(inputs, them string) string {
		return "parameters-unsigned: " + inputs + " derived by a rule that reads a parameter resting on the operator's say, which its source's receipt does not commit, and --require-sourced refuses " + them + ": nothing here checks that parameter against a source"
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
		"a value derived from runAt alone": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(3, param("ref", "upstream-runAt"))},
			" All of the run's inputs were" + clockRead, "", ""},
		"a case parameter": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(3, param("region", "case"))},
			" All of the run's inputs were" + operatorRead, "parameters-unsigned", refusal("All of the run's inputs were", "them")},
		"a parameter in ambiguous text": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(1, param("vendorId", "ambiguous-text"))},
			" 1 of the run's 3 inputs was" + operatorRead, "parameters-unsigned", refusal("1 of the run's 3 inputs was", "it")},
		"a value derived from a case parameter": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(2, param("ref", "upstream"))},
			" 2 of the run's 3 inputs were" + operatorRead, "parameters-unsigned", refusal("2 of the run's 3 inputs were", "them")},
		"a local file's fact": {runner.InputClasses{Asserted: 1, Record: 2}, []runner.UnsignedParameters{source(2, param("ref", "local-file"))},
			" 2 of the run's 3 inputs were" + operatorRead, "inputs-asserted", "inputs-asserted: 1 of the run's 3 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source"},
		"a case parameter and runAt in one rule": {runner.InputClasses{Record: 2, Generated: 2}, []runner.UnsignedParameters{source(2, param("region", "case"), param("runAt", "runAt"))},
			" 2 of the run's 4 inputs were" + operatorRead + " 2 of the run's 4 inputs were" + clockRead, "parameters-unsigned", refusal("2 of the run's 4 inputs were", "them")},
		"runAt in one source, a case parameter in another": {runner.InputClasses{Record: 3}, []runner.UnsignedParameters{source(2, param("runAt", "runAt")), source(1, param("region", "case"))},
			" 1 of the run's 3 inputs was" + operatorRead + " 2 of the run's 3 inputs were" + clockRead, "parameters-unsigned", refusal("1 of the run's 3 inputs was", "it")},
		"one input of one": {runner.InputClasses{Generated: 1}, []runner.UnsignedParameters{source(1, param("region", "case"))},
			" All of the run's inputs were" + operatorRead, "parameters-unsigned", refusal("All of the run's inputs were", "them")},
		"asserted as well": {runner.InputClasses{Asserted: 1, Record: 1}, []runner.UnsignedParameters{source(1, param("region", "case"))},
			" 1 of the run's 2 inputs was" + operatorRead, "inputs-asserted", "inputs-asserted: 1 of the run's 2 inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := unsignedSentence(c.classes, c.unsigned); got != c.sentence {
				t.Fatal(got)
			}
			status, err := refuseUnsourced("", c.classes, c.unsigned)
			if status != c.status || c.refusal == "" && err != nil || c.refusal != "" && (err == nil || err.Error() != c.refusal) {
				t.Fatal(status, err)
			}
			if status == "parameters-unsigned" && !errors.Is(err, errParametersUnsigned) || status == "inputs-asserted" && !errors.Is(err, errInputsAsserted) {
				t.Fatal("refusal does not wrap its status:", err)
			}
		})
	}
}
