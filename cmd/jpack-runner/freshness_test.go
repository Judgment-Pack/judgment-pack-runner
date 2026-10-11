package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// What the line on standard error says of the instant freshness was judged
// against (#55), after what it says of the sources judged.
const (
	freshnessSaid   = " against preparation.verifiedAt, which the export supplies and no signature covers: freshness is consistent with the instant the export supplies; nothing establishes that instant was the admission instant."
	oneAcquisition  = " Acquisition freshness for 1 source was judged" + freshnessSaid
	twoAcquisitions = " Acquisition freshness for 2 sources was judged" + freshnessSaid
	oneCalculation  = " Acquisition and calculator freshness for 1 source were judged" + freshnessSaid
)

// freshnessMember is the report's unsignedFreshness member, with the comma
// before it, for the instant verifiedAt and the sources judged against it.
func freshnessMember(verifiedAt, sources string) string {
	return `,"unsignedFreshness":{"against":"preparation.verifiedAt","verifiedAt":"` + verifiedAt + `","sources":` + sources + `}`
}

// verifiedAtOf is the instant an export's preparation retains.
func verifiedAtOf(t *testing.T, bundle map[string]json.RawMessage) string {
	t.Helper()
	var run struct {
		Input struct {
			Preparation struct {
				VerifiedAt string `json:"verifiedAt"`
			} `json:"preparation"`
		} `json:"input"`
	}
	if err := json.Unmarshal(bundle["run"], &run); err != nil || run.Input.Preparation.VerifiedAt == "" {
		t.Fatal("the export retains no preparation instant:", err)
	}
	return run.Input.Preparation.VerifiedAt
}

// The sentence counts the sources whose acquisition was judged against the
// instant, and those of them whose calculator's tables were too, and says the
// same of the instant whatever the counts. It adds nothing when no source was
// acquired.
func TestTheFreshnessSentenceCountsWhatWasJudged(t *testing.T) {
	judged := func(checks ...[]string) *runner.UnsignedFreshness {
		f := &runner.UnsignedFreshness{Against: "preparation.verifiedAt", VerifiedAt: "2026-10-11T12:00:00Z"}
		for _, c := range checks {
			f.Sources = append(f.Sources, runner.FreshnessSource{Source: "s", Checks: c, Targets: 1})
		}
		return f
	}
	acquisition, calculation := []string{"acquisition"}, []string{"acquisition", "calculation"}
	for _, c := range []struct {
		freshness *runner.UnsignedFreshness
		want      string
	}{
		{nil, ""},
		{judged(acquisition), oneAcquisition},
		{judged(acquisition, acquisition), twoAcquisitions},
		{judged(calculation), oneCalculation},
		{judged(calculation, calculation), " Acquisition and calculator freshness for 2 sources were judged" + freshnessSaid},
		{judged(acquisition, calculation, acquisition), " Acquisition freshness for 3 sources, and calculator freshness for 1 of them, were judged" + freshnessSaid},
	} {
		if got := freshnessSentence(c.freshness); got != c.want {
			t.Fatalf("%+v: %q", c.freshness, got)
		}
	}
}

// calculatorOnACopy is the audit's case (#55): a calculated source whose
// answer a copy mapping reads, so no rule reads a parameter and the
// unsigned-parameter report has nothing to say. Its signed answer carries the
// pack's request fact and two evidence requirements beside its calculation.
func calculatorOnACopy(t *testing.T) (runner.Input, signer) {
	t.Helper()
	s := newSigner(t)
	s.profile.ID, s.profile.Source, s.profile.Tools = "fx", "fx/live", []string{"convert"}
	s.profile.Calculator = &runner.CalculatorPin{Name: "fx-convert", Version: "2.1.0"}
	read := runner.SourceRead{Copy: &runner.CopyMapping{
		Facts:    []runner.FactMapping{{Target: "/request", Source: "/facts/request"}},
		Evidence: []runner.EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}},
	}}
	fx := s.source(t, "fx", `{"tool":"convert","arguments":{"amount":{"$param":"invoiceAmount"},"currency":{"$param":"invoiceCurrency"}}}`, read)
	fx.Calculation = &runner.CalculationBinding{Inputs: map[string]string{"amount": "invoiceAmount", "currency": "invoiceCurrency"}, Tables: map[string]int{"ecb-rates": 86400}}
	m := runner.InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"},
		Case:    &runner.CaseMapping{Parameters: map[string]runner.Parameter{"invoiceAmount": {Pointer: "/amount", Type: "string"}, "invoiceCurrency": {Pointer: "/currency", Type: "string"}}, Facts: []runner.FactMapping{}, Evidence: []runner.EvidenceMapping{}},
		Sources: []runner.MappingSource{fx}}
	answer := `{"calculation":{"calculator":{"name":"fx-convert","version":"2.1.0"},"status":"computed","inputs":{"amount":"1200.00","currency":"EUR"},"asOf":{"ecb-rates":"` + s.at.Add(-time.Hour).Format("2006-01-02T15:04:05Z") + `"}},"facts":` + facts + `,"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`
	return runner.Input{Source: &runner.SourceInput{Mapping: m, Case: json.RawMessage(`{"amount":"1200.00","currency":"EUR"}`), Sources: map[string]runner.SourceValue{
		"fx": s.response(t, `{"tool":"convert","arguments":{"amount":"1200.00","currency":"EUR"}}`, answer, 0),
	}}}, s
}

// movedBySecond is the export with its preparation recomputed one second
// later, as the audit did: its verifiedAt and the runAt each outcome retains
// among its parameters. Nothing reads runAt, so nothing else changes. The
// record's bytes, their signature and the chain entry are as they were.
func movedBySecond(t *testing.T, bundle map[string]json.RawMessage) (map[string]json.RawMessage, string) {
	t.Helper()
	at, err := time.Parse("2006-01-02T15:04:05Z", verifiedAtOf(t, bundle))
	if err != nil {
		t.Fatal(err)
	}
	later := at.Add(time.Second).Format("2006-01-02T15:04:05Z")
	var run, input, preparation map[string]json.RawMessage
	var outcomes []map[string]json.RawMessage
	if json.Unmarshal(bundle["run"], &run) != nil || json.Unmarshal(run["input"], &input) != nil || json.Unmarshal(input["preparation"], &preparation) != nil || json.Unmarshal(preparation["outcomes"], &outcomes) != nil {
		t.Fatal("the export has no preparation to move")
	}
	preparation["verifiedAt"], _ = json.Marshal(later)
	for _, o := range outcomes {
		var parameters map[string]json.RawMessage
		if err = json.Unmarshal(o["parameters"], &parameters); err != nil {
			t.Fatal(err)
		}
		parameters["runAt"], _ = json.Marshal(later)
		o["parameters"], _ = json.Marshal(parameters)
	}
	preparation["outcomes"], _ = json.Marshal(outcomes)
	input["preparation"], _ = json.Marshal(preparation)
	run["input"], _ = json.Marshal(input)
	moved := map[string]json.RawMessage{}
	for k, v := range bundle {
		moved[k] = v
	}
	moved["run"], _ = json.Marshal(run)
	return moved, later
}

// The audit's reproduction (#55), end to end: a calculated run on a copy
// mapping, signed by the installation's key and witnessed by a held
// checkpoint, verified with both required. The report and the line name the
// instant its acquisition's and its calculator's freshness were judged
// against. Recomputed one second later, the export verifies just as well,
// under the same signature and checkpoint, and the report names the moved
// instant: freshness is consistent with the instant the export supplies, and
// verify-run says so rather than refusing. The instant moved alone, without
// recomputing the preparation, is refused as before. A report that refuses
// carries the member too: without the held checkpoint, under another key, and
// when a re-execution decides otherwise than a version-2 export says.
func TestVerifyRunNamesTheInstantFreshnessWasJudgedAgainst(t *testing.T) {
	input, s := calculatorOnACopy(t)
	profiles := []runner.InputProfile{s.profile}
	log := filepath.Join(t.TempDir(), "signing.log")
	runtime := signingRuntime(t, log)
	e := exportedRunKeyed(t, input, profiles, "", "?version=5", "", runtime)
	if string(e.bundle["version"]) != "5" {
		helper, _ := os.ReadFile(log)
		t.Fatalf("not a version-5 export: %s %s", e.bundle["version"], helper)
	}
	var chain struct {
		Checkpoint json.RawMessage `json:"checkpoint"`
	}
	if err := json.Unmarshal(e.bundle["chain"], &chain); err != nil {
		t.Fatal(err)
	}
	held := file(t, "held.jsonl", append(chain.Checkpoint, '\n'))
	required := []string{"--public-key", key1, "--require-signed", "--expect", held, "--require-witnessed"}
	signed := `"signature":{"findings":[],"findingsTotal":0,"keyId":"` + keyID1 + `","publicKey":"` + key1 + `","readableLines":1,"status":"signed","unreadableLines":0}`
	witnessed := `"held":{"matched":1,"supplied":1,"through":1},"scope":"checkpoint","status":"valid","witnessed":true}`
	judged := `[{"source":"fx","checks":["acquisition","calculation"],"targets":3}]`
	moved, later := movedBySecond(t, e.bundle)
	for name, c := range map[string]struct {
		bundle map[string]json.RawMessage
		at     string
	}{"as exported": {e.bundle, verifiedAtOf(t, e.bundle)}, "moved by one second": {moved, later}} {
		t.Run(name, func(t *testing.T) {
			out, human, err := verifyWith(t, c.bundle, e.digest, profiles, required...)
			if err != nil || !strings.HasSuffix(out, `"status":"verified-inputs","targetsByClass":{"asserted":0,"record":3,"generated":0}`+freshnessMember(c.at, judged)+"}\n") || !strings.Contains(out, signed) || !strings.Contains(out, witnessed) || strings.Contains(out, "unsignedParameters") {
				t.Fatal(err, out)
			}
			if !strings.HasPrefix(human, inputsLine+oneCalculation+" The export's bytes of the audit record parse to the record.") {
				t.Fatal("human output:", human)
			}
		})
	}
	if verifiedAtOf(t, moved) == verifiedAtOf(t, e.bundle) || string(moved["chain"]) != string(e.bundle["chain"]) {
		t.Fatal("the fixture did not move the instant alone")
	}
	alone := map[string]json.RawMessage{}
	for k, v := range e.bundle {
		alone[k] = v
	}
	alone["run"] = json.RawMessage(strings.Replace(string(e.bundle["run"]), `"verifiedAt":"`+verifiedAtOf(t, e.bundle)+`"`, `"verifiedAt":"`+later+`"`, 1))
	if out, human, err := verifyWith(t, alone, e.digest, profiles, required...); err == nil || errors.Is(err, errUnsigned) || err.Error() != "retained lineage does not match recomputation" || out != "" || human != "" {
		t.Fatal(err, out, human)
	}
	member := `"targetsByClass":{"asserted":0,"record":3,"generated":0}` + freshnessMember(later, judged) + "}\n"
	unwitnessed := []string{"--public-key", key1, "--require-signed", "--require-witnessed"}
	out, human, err := verifyWith(t, moved, e.digest, profiles, unwitnessed...)
	if !errors.Is(err, errUnwitnessed) || err.Error() != "unwitnessed: no held checkpoint covers the run's entry, at sequence 1, and --require-witnessed refuses it: nothing here establishes the entry against the operator, who keeps the chain" || !strings.HasSuffix(out, `"status":"unwitnessed",`+member) || human != "" {
		t.Fatal(err, out, human)
	}
	otherKey := []string{"--public-key", key2, "--require-signed", "--expect", held, "--require-witnessed"}
	out, human, err = verifyWith(t, moved, e.digest, profiles, otherKey...)
	if !errors.Is(err, errSignatureInvalid) || err.Error() != "signature-invalid: signature-invalid at line 1: sidecar line 1: the signature does not verify under the key in force" || !strings.HasSuffix(out, `"status":"signature-invalid",`+member) || human != "" {
		t.Fatal(err, out, human)
	}
	// The moved export as version 2, with its answer changed in its result
	// and its audit record alike, which a version-2 export does not hold to
	// bytes: the release's Runtime, the signing one here, decides otherwise.
	var run map[string]json.RawMessage
	if err = json.Unmarshal(moved["run"], &run); err != nil {
		t.Fatal(err)
	}
	delete(run, "auditBytes")
	delete(run, "auditSignatures")
	for _, k := range []string{"result", "audit"} {
		changed := strings.Replace(string(run[k]), `"outcomeId":"proceed"`, `"outcomeId":"decline-redirect"`, 1)
		if changed == string(run[k]) {
			t.Fatalf("the run's %s has no answer to change", k)
		}
		run[k] = json.RawMessage(changed)
	}
	v2 := map[string]json.RawMessage{"version": json.RawMessage("2"), "releaseDigest": moved["releaseDigest"], "release": moved["release"]}
	v2["run"], _ = json.Marshal(run)
	t.Setenv("TMPDIR", t.TempDir())
	out, human, err = verifyWith(t, v2, e.digest, profiles, "--runtime="+runtime)
	if !errors.Is(err, runner.ErrDispositionDiffers) || !strings.HasPrefix(out, `{"exactBytes":"not-in-export","exportVersion":2,"retainedDisposition":"differs-from-re-execution",`) || !strings.HasSuffix(out, `"status":"disposition-differs",`+member) || human != "" {
		t.Fatal(err, out, human)
	}
}
