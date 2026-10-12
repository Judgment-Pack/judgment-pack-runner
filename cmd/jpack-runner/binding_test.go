package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// What the line on standard error says of the record's binding of the
// preparation (runner #55), and of freshness when the signed record commits
// the instant.
const (
	unboundSaid        = " The run's audit record carries no binding of its preparation: the Runtime that evaluated it took none (before 0.31.0), or Runner passed none then. --require-bound refuses it."
	uncheckedSaid      = " The record's binding is the SHA-256 of the export's preparation, but no signature checked here covers the record: the binding commits the preparation only under a record signature that verifies (--public-key)."
	committedSaid      = " The record's binding is the SHA-256 of the export's preparation, and the record's signature verifies: the preparation, its instant included, is the one Runner handed the Runtime for the record that was signed. Against the operator, who holds the key, that is no more than the signature is: a preparation recomputed, and its record signed again with the new binding, verifies as well; only a witnessed chain (--require-witnessed), against a checkpoint held independently from before, shows it. Nor does the binding carry a time: nothing establishes that the instant was the admission instant."
	committedFreshness = " against preparation.verifiedAt, which is committed in the signed record: the record's binding is the SHA-256 of the export's preparation, and its signature verifies."
	mismatchRefusal    = "binding-mismatch: the record's binding is not the SHA-256 of the export's preparation: the preparation, preparation.verifiedAt included, is not the one the record binds"
	foreignRefusal     = "binding-foreign-kind: the record's binding is not of Runner's scheme, judgment-pack-runner/preparation/1, so it commits nothing verify-run can check, and no digest was compared"
	unboundRefusal     = "unbound: the run's audit record carries no binding of its preparation, and --require-bound refuses it: nothing in the record commits the preparation, or preparation.verifiedAt"
)

// takesBinding says whether the Runtime at bin takes a binding, from the
// release it names in its own `version` answer: 0.31.0 and later do. A
// Runtime that names none (0.0.0-dev) is taken to take none, as Runner takes
// it.
func takesBinding(bin string) (bool, error) {
	dir, err := os.MkdirTemp("", "jpack-version-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(bin, "version", "--format", "json")
	cmd.Env, cmd.Dir = []string{"LANG=C", "LC_ALL=C"}, dir
	out, err := cmd.Output()
	var answer struct {
		Tool struct {
			Version string `json:"version"`
		} `json:"tool"`
	}
	if err != nil || json.Unmarshal(out, &answer) != nil {
		return false, fmt.Errorf("the Runtime under test did not answer `version`: %v %s", err, out)
	}
	var major, minor, patch int
	if _, err = fmt.Sscanf(answer.Tool.Version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false, fmt.Errorf("the Runtime under test names its release %q", answer.Tool.Version)
	}
	return major > 0 || minor >= 31, nil
}

// bindingOfTheRuntime is the binding verify-run reports of an untouched
// export of a mapping-v2 run that the Runtime under test recorded: matches
// when it takes a binding, absent when it does not.
var bindingOfTheRuntime = sync.OnceValue(func() string {
	takes, err := takesBinding(os.Getenv("JPACK_TEST_BIN"))
	if err != nil {
		panic(err)
	}
	if takes {
		return runner.BindingMatches
	}
	return runner.BindingAbsent
})

// boundSaid is what the line ends with for an untouched export of a run the
// Runtime under test recorded, checked under no key.
func boundSaid() string {
	if bindingOfTheRuntime() == runner.BindingMatches {
		return uncheckedSaid
	}
	return unboundSaid
}

// withBinding is a report line with its binding member, which sorts first.
func withBinding(report, status string) string {
	return `{"binding":{"status":"` + status + `"},` + strings.TrimPrefix(report, "{")
}

// bindingRuntime is the Runtime under test, when it takes a binding; the test
// is skipped otherwise. CI's pin does, and the test must run there.
func bindingRuntime(t *testing.T) {
	t.Helper()
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	takes, err := takesBinding(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !takes {
		t.Skip("JPACK_TEST_BIN takes no binding: set it to a Runtime of 0.31.0 or later")
	}
}

// preparationSHA is the SHA-256 of an export's run.input.preparation as a
// verifier recomputes it from the export alone, written here independently of
// Runner's code: the object read with its numbers as written, and written
// again with its members sorted, without insignificant whitespace, its
// strings escaped as Go's encoding/json escapes them.
func preparationSHA(t *testing.T, bundle map[string]json.RawMessage) string {
	t.Helper()
	var run struct {
		Input struct {
			Preparation json.RawMessage `json:"preparation"`
		} `json:"input"`
	}
	if err := json.Unmarshal(bundle["run"], &run); err != nil || len(run.Input.Preparation) == 0 {
		t.Fatal("the export has no preparation:", err)
	}
	d := json.NewDecoder(bytes.NewReader(run.Input.Preparation))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return sha(b)
}

// recordOf is the record's line a version-3, 4 or 5 export carries.
func recordOf(t *testing.T, bundle map[string]json.RawMessage) []byte {
	t.Helper()
	var run struct {
		AuditBytes []byte `json:"auditBytes"`
	}
	if err := json.Unmarshal(bundle["run"], &run); err != nil || len(run.AuditBytes) == 0 {
		t.Fatal("the export carries no record's bytes:", err)
	}
	return run.AuditBytes
}

// reSigned is a version-5 export whose record's binding, as recorded, is
// replaced by binding, with the chain entry made again and the record signed
// again with the installation's key: what an operator who rewrote the record
// would export. The signature and the chain then hold, and the binding is
// what is checked.
func reSigned(t *testing.T, bundle map[string]json.RawMessage, binding string) map[string]json.RawMessage {
	t.Helper()
	line := recordOf(t, bundle)
	var members struct {
		Trail   string          `json:"trail"`
		Binding json.RawMessage `json:"binding"`
	}
	if err := json.Unmarshal(line, &members); err != nil || members.Trail == "" || len(members.Binding) == 0 {
		t.Fatal("the record is not a chained record with a binding:", err)
	}
	changed := bytes.Replace(line, append([]byte(`"binding":`), members.Binding...), []byte(`"binding":`+binding), 1)
	if bytes.Equal(changed, line) && binding != string(members.Binding) {
		t.Fatal("the binding was not replaced")
	}
	return withSidecar(t, withRecord(t, bundle, changed), signatureLine(seed1, changed, 1, members.Trail))
}

// A run the Runtime binds (0.31.0 and later), signed with the installation's
// key and witnessed by a held checkpoint: the record carries the binding
// Runner passed, whose digest is the SHA-256 of the export's preparation as a
// verifier recomputes it alone. verify-run reports it matching, and says the
// instant freshness was judged against is committed in the signed record, and
// how far that goes. The audit's case (#55), the preparation recomputed one
// second later, is now refused, under --require-signed and without it; the
// instant moved alone is refused as before. A binding of another scheme is
// refused before any digest is compared, even one whose digest is the
// preparation's and whose record is signed again; and the record's member is
// compared as recorded, so Runner's binding rewritten with its members in
// another order is not taken for it.
func TestVerifyRunHoldsTheRecordsBindingToThePreparation(t *testing.T) {
	bindingRuntime(t)
	input, s := calculatorOnACopy(t)
	profiles := []runner.InputProfile{s.profile}
	log := filepath.Join(t.TempDir(), "signing.log")
	e := exportedRunKeyed(t, input, profiles, "", "?version=5", "", signingRuntime(t, log))
	if string(e.bundle["version"]) != "5" {
		helper, _ := os.ReadFile(log)
		t.Fatalf("not a version-5 export: %s %s", e.bundle["version"], helper)
	}
	prepared := preparationSHA(t, e.bundle)
	want := `"binding":{"kind":"judgment-pack-runner/preparation/1","digest":"` + prepared + `"}`
	if line := recordOf(t, e.bundle); bytes.Count(line, []byte(`"binding":`)) != 1 || !bytes.Contains(line, []byte(want)) {
		t.Fatalf("the record does not carry the preparation's binding as passed: %s", line)
	}
	var chain struct {
		Checkpoint json.RawMessage `json:"checkpoint"`
	}
	if err := json.Unmarshal(e.bundle["chain"], &chain); err != nil {
		t.Fatal(err)
	}
	held := file(t, "held.jsonl", append(chain.Checkpoint, '\n'))
	required := []string{"--public-key", key1, "--require-signed", "--expect", held, "--require-witnessed", "--require-bound"}
	judged := `[{"source":"fx","checks":["acquisition","calculation"],"targets":3}]`

	out, human, err := verifyWith(t, e.bundle, e.digest, profiles, required...)
	if err != nil || !strings.HasPrefix(out, `{"binding":{"status":"matches"},"exactBytes":"matches-record","exportVersion":5,`) || !strings.HasSuffix(out, `"status":"verified-inputs","targetsByClass":{"asserted":0,"record":3,"generated":0}`+freshnessMember(verifiedAtOf(t, e.bundle), judged)+"}\n") {
		t.Fatal(err, out)
	}
	if !strings.HasPrefix(human, inputsLine+" Acquisition and calculator freshness for 1 source were judged"+committedFreshness+" The export's bytes of the audit record parse to the record.") || !strings.HasSuffix(human, committedSaid+"\n") || strings.Contains(human, "no signature covers") {
		t.Fatal("human output:", human)
	}
	// Without the key, the same binding matches, and the line says no
	// signature checked here covers it.
	out, human, err = verifyWith(t, e.bundle, e.digest, profiles)
	if err != nil || !strings.HasPrefix(out, `{"binding":{"status":"matches"},`) || !strings.Contains(human, oneCalculation) || !strings.HasSuffix(human, uncheckedSaid+"\n") {
		t.Fatal(err, out, human)
	}

	moved, later := movedBySecond(t, e.bundle)
	if preparationSHA(t, moved) == prepared || !bytes.Equal(recordOf(t, moved), recordOf(t, e.bundle)) {
		t.Fatal("the fixture did not move the preparation alone")
	}
	for name, extra := range map[string][]string{
		"signed and witnessed required": required,
		"signature required":            {"--public-key", key1, "--require-signed"},
		"nothing required":              nil,
	} {
		t.Run("moved by one second, "+name, func(t *testing.T) {
			out, human, err := verifyWith(t, moved, e.digest, profiles, extra...)
			if !errors.Is(err, errBindingMismatch) || err.Error() != mismatchRefusal || human != "" {
				t.Fatal(err, human)
			}
			if !strings.HasPrefix(out, `{"binding":{"status":"mismatch"},"exactBytes":"matches-record","exportVersion":5,`) || !strings.HasSuffix(out, `"status":"binding-mismatch","targetsByClass":{"asserted":0,"record":3,"generated":0}`+freshnessMember(later, judged)+"}\n") {
				t.Fatal(out)
			}
		})
	}
	alone := map[string]json.RawMessage{}
	for k, v := range e.bundle {
		alone[k] = v
	}
	alone["run"] = json.RawMessage(strings.Replace(string(e.bundle["run"]), `"verifiedAt":"`+verifiedAtOf(t, e.bundle)+`"`, `"verifiedAt":"`+later+`"`, 1))
	if out, human, err := verifyWith(t, alone, e.digest, profiles, required...); err == nil || err.Error() != "retained lineage does not match recomputation" || out != "" || human != "" {
		t.Fatal(err, out, human)
	}

	signed := []string{"--public-key", key1, "--require-signed"}
	for name, c := range map[string]struct {
		binding, status, refused, refusal string
		is                                error
	}{
		"another scheme, the preparation's digest": {`{"kind":"another-companion/preparation/1","digest":"` + prepared + `"}`, "foreign-kind", "binding-foreign-kind", foreignRefusal, errBindingForeignKind},
		"another scheme, another digest":           {`{"kind":"another-companion/preparation/1","digest":"` + sha(nil) + `"}`, "foreign-kind", "binding-foreign-kind", foreignRefusal, errBindingForeignKind},
		"Runner's scheme, another digest":          {`{"kind":"judgment-pack-runner/preparation/1","digest":"` + sha(nil) + `"}`, "mismatch", "binding-mismatch", mismatchRefusal, errBindingMismatch},
		"Runner's binding, its members reordered":  {`{"digest":"` + prepared + `","kind":"judgment-pack-runner/preparation/1"}`, "mismatch", "binding-mismatch", mismatchRefusal, errBindingMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			rewritten := reSigned(t, e.bundle, c.binding)
			out, human, err := verifyWith(t, rewritten, e.digest, profiles, signed...)
			if !errors.Is(err, c.is) || err.Error() != c.refusal || human != "" {
				t.Fatal(err, human)
			}
			sig := `"signature":{"findings":[],"findingsTotal":0,"keyId":"` + keyID1 + `","publicKey":"` + key1 + `","readableLines":1,"status":"signed","unreadableLines":0}`
			if !strings.HasPrefix(out, `{"binding":{"status":"`+c.status+`"},`) || !strings.Contains(out, sig) || !strings.Contains(out, `"status":"`+c.refused+`"`) {
				t.Fatal(out)
			}
		})
	}
	// The rewrite itself is sound: Runner's own binding, written again in
	// place and signed again, verifies.
	if out, _, err := verifyWith(t, reSigned(t, e.bundle, `{"kind":"judgment-pack-runner/preparation/1","digest":"`+prepared+`"}`), e.digest, profiles, signed...); err != nil || !strings.HasPrefix(out, `{"binding":{"status":"matches"},`) {
		t.Fatal(err, out)
	}
}

// A run that a Runtime without the binding (before 0.31.0) evaluated
// completes, and its record carries none: verify-run reports it absent, keeps
// naming the instant freshness was judged against as the export's, and
// refuses it only with --require-bound.
func TestVerifyRunReportsARecordTheRuntimeDidNotBind(t *testing.T) {
	earlier := os.Getenv("JPACK_EARLIER_TEST_BIN")
	if earlier == "" {
		t.Skip("set JPACK_EARLIER_TEST_BIN to a Runtime before 0.31.0")
	}
	if takes, err := takesBinding(earlier); err != nil || takes {
		t.Fatalf("JPACK_EARLIER_TEST_BIN must be a Runtime before 0.31.0: %v", err)
	}
	input, s := calculatorOnACopy(t)
	profiles := []runner.InputProfile{s.profile}
	e := exportedRunKeyed(t, input, profiles, "", "?version=4", "", earlier)
	if bytes.Contains(recordOf(t, e.bundle), []byte(`"binding"`)) {
		t.Fatal("the earlier Runtime's record carries a binding")
	}
	judged := freshnessMember(verifiedAtOf(t, e.bundle), `[{"source":"fx","checks":["acquisition","calculation"],"targets":3}]`)
	out, human, err := verifyWith(t, e.bundle, e.digest, profiles)
	if err != nil || !strings.HasPrefix(out, `{"binding":{"status":"absent"},"exactBytes":"matches-record","exportVersion":4,`) || !strings.HasSuffix(out, `"status":"verified-inputs","targetsByClass":{"asserted":0,"record":3,"generated":0}`+judged+"}\n") {
		t.Fatal(err, out)
	}
	if !strings.HasPrefix(human, inputsLine+oneCalculation) || !strings.HasSuffix(human, unboundSaid+"\n") {
		t.Fatal("human output:", human)
	}
	for _, extra := range [][]string{{"--require-bound"}, {"--require-bound", "--runtime=" + filepath.Join(t.TempDir(), "absent")}} {
		out, human, err = verifyWith(t, e.bundle, e.digest, profiles, extra...)
		if !errors.Is(err, errUnbound) || err.Error() != unboundRefusal || human != "" || !strings.HasPrefix(out, `{"binding":{"status":"absent"},`) || !strings.HasSuffix(out, `"status":"unbound","targetsByClass":{"asserted":0,"record":3,"generated":0}`+judged+"}\n") {
			t.Fatal(extra, err, out, human)
		}
	}
}

// Exports Runner made before it passed a binding verify as they did, with the
// binding reported absent; --require-bound refuses them, and an export without
// lineage, which has no preparation to bind. No Runtime is needed.
func TestRequireBoundRefusesARecordThatBindsNoPreparation(t *testing.T) {
	dir := "../../internal/runner/testdata/mapping-v2-before-calculators/"
	trusted, err := os.ReadFile(dir + "release-digest.txt")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(string(trusted))
	judged := freshnessMember("2026-10-01T18:48:51Z", `[{"source":"vendor","checks":["acquisition"],"targets":3}]`)
	out, human, err := verifyFiles(dir+"run.json", dir+"profiles.json", digest)
	if err != nil || out != strings.TrimSuffix(reportBound(runner.BindingAbsent, "verified-inputs", "matches-audit", inputsOnly, 0, 3, 0), "}\n")+judged+"}\n" || !strings.HasSuffix(human, oneAcquisition+notInExport+unboundSaid+"\n") {
		t.Fatal(err, out, human)
	}
	out, human, err = verifyFiles(dir+"run.json", dir+"profiles.json", digest, "--require-bound")
	if !errors.Is(err, errUnbound) || err.Error() != unboundRefusal || human != "" || out != strings.TrimSuffix(reportBound(runner.BindingAbsent, "unbound", "matches-audit", inputsOnly, 0, 3, 0), "}\n")+judged+"}\n" {
		t.Fatal(err, out, human)
	}
	v := readNotMappedVector(t, "no-mapping-run")
	out, human, err = v.verify("--require-bound")
	if !errors.Is(err, errUnbound) || err.Error() != "unbound: the run's job has no mapping v2, so its export has no preparation for its record to bind, and --require-bound refuses it" || human != "" || !strings.HasPrefix(out, `{"exactBytes":"matches-record",`) || strings.Contains(out, `"binding"`) || !strings.HasSuffix(out, `"status":"unbound"}`+"\n") {
		t.Fatal(err, out, human)
	}
	if out, _, err = v.verify(); err != nil || strings.Contains(out, `"binding"`) {
		t.Fatal("an export without lineage reports a binding:", err, out)
	}
}

// What the line says of the binding, and of freshness, for each binding and
// signature; and what is refused, and in which words, for each binding, with
// and without --require-bound.
func TestTheBindingIsNamedAndRefusedByWhatItIs(t *testing.T) {
	signed := &runner.SignatureReport{Status: runner.SignatureSigned}
	for _, c := range []struct {
		binding   string
		signature *runner.SignatureReport
		said      string
		committed bool
	}{
		{runner.BindingMatches, signed, committedSaid, true},
		{runner.BindingMatches, nil, uncheckedSaid, false},
		{runner.BindingMatches, &runner.SignatureReport{Status: runner.SignatureNotChecked}, uncheckedSaid, false},
		{runner.BindingMatches, &runner.SignatureReport{Status: runner.SignatureUnsigned}, uncheckedSaid, false},
		{runner.BindingMatches, &runner.SignatureReport{Status: runner.SignatureInvalid}, uncheckedSaid, false},
		{runner.BindingAbsent, signed, unboundSaid, false},
		{runner.BindingAbsent, nil, unboundSaid, false},
		{runner.BindingMismatch, signed, "", false},
		{runner.BindingForeignKind, signed, "", false},
	} {
		b := &runner.BindingReport{Status: c.binding}
		if got := bindingSentence(b, c.signature); got != c.said || committed(b, c.signature) != c.committed {
			t.Fatalf("%s %+v: %q", c.binding, c.signature, got)
		}
	}
	if bindingSentence(nil, signed) != "" || committed(nil, signed) {
		t.Fatal("an export without lineage is said to be bound")
	}
	f := &runner.UnsignedFreshness{Against: "preparation.verifiedAt", Sources: []runner.FreshnessSource{{Source: "s", Checks: []string{"acquisition", "calculation"}}}}
	if freshnessSentence(f, true) != " Acquisition and calculator freshness for 1 source were judged"+committedFreshness || freshnessSentence(f, false) != oneCalculation || freshnessSentence(nil, true) != "" {
		t.Fatal(freshnessSentence(f, true), freshnessSentence(f, false))
	}
	for _, c := range []struct {
		binding      string // "" for an export without lineage
		requireBound bool
		status       string
		is           error
	}{
		{runner.BindingMatches, false, "", nil},
		{runner.BindingMatches, true, "", nil},
		{runner.BindingAbsent, false, "", nil},
		{runner.BindingAbsent, true, "unbound", errUnbound},
		{runner.BindingMismatch, false, "binding-mismatch", errBindingMismatch},
		{runner.BindingMismatch, true, "binding-mismatch", errBindingMismatch},
		{runner.BindingForeignKind, false, "binding-foreign-kind", errBindingForeignKind},
		{runner.BindingForeignKind, true, "binding-foreign-kind", errBindingForeignKind},
		{"", false, "", nil},
		{"", true, "unbound", errUnbound},
	} {
		var b *runner.BindingReport
		inputs := runner.InputsNotMapped
		if c.binding != "" {
			b, inputs = &runner.BindingReport{Status: c.binding}, ""
		}
		status, err := bindingRefusal(b, inputs, c.requireBound)
		if status != c.status || c.is == nil && err != nil || c.is != nil && !errors.Is(err, c.is) {
			t.Fatalf("%q %v: %q %v", c.binding, c.requireBound, status, err)
		}
	}
}

// The digest the tests recompute from an export alone is the SHA-256 of the
// canonical form the guide states: a check of the helper against a
// hand-written preparation, its members sorted, its number as written, and &,
// < and > escaped.
func TestThePreparationDigestHelperWritesTheCanonicalForm(t *testing.T) {
	bundle := map[string]json.RawMessage{"run": json.RawMessage(`{"input":{"preparation":{"version":2,"verifiedAt":"2026-10-11T12:00:00Z","verification":"a&b <c>","cites":[],"amount":1200.50}}}`)}
	written := `{"amount":1200.50,"cites":[],"verification":"a\u0026b \u003cc\u003e","verifiedAt":"2026-10-11T12:00:00Z","version":2}`
	sum := sha256.Sum256([]byte(written))
	if got := preparationSHA(t, bundle); got != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal(got)
	}
}
