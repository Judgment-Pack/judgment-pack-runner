package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const calculatorArgs = `{"tool":"convert","arguments":{"amount":"1200.00","currency":"EUR"}}`

func calculatorProfile(t *testing.T) (InputProfile, ed25519.PrivateKey) {
	t.Helper()
	p, key := testProfile(t)
	p.ID = "fx"
	p.Source = "fx/live"
	p.Tools = []string{"convert"}
	p.Calculator = &CalculatorPin{"fx-convert", "2.1.0"}
	return p, key
}

func calculatorRule() json.RawMessage {
	return json.RawMessage(`{"ruleVersion":"1","parameters":{},"clauses":[{"when":{"op":"exists","field":"/converted"},"claim":{"facts":[{"pointer":"/invoice/converted","from":"/converted"}],"evidence":{"fx-rate":"present"},"acquisitionStatus":"resolved"},"reason":"converted"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"fx-rate":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-value"}]}`)
}

// calculation is the answer's calculation member, which tests change.
func calculation(at time.Time) map[string]any {
	return map[string]any{
		"calculator": map[string]any{"name": "fx-convert", "version": "2.1.0"},
		"status":     "computed",
		"inputs":     map[string]any{"amount": "1200.00", "currency": "EUR"},
		"asOf":       map[string]any{"ecb-rates": at.Add(-time.Hour).Format(time.RFC3339)},
	}
}

func calculatorFixture(t *testing.T) (Input, InputProfile, ed25519.PrivateKey, time.Time) {
	t.Helper()
	p, key := calculatorProfile(t)
	at := time.Now().UTC().Truncate(time.Second)
	m := InputMapping{Version: 2,
		Case: &CaseMapping{Parameters: map[string]Parameter{"invoiceAmount": {Pointer: "/amount", Type: "string"}, "invoiceCurrency": {Pointer: "/currency", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{{Name: "fx", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300,
			Arguments:   json.RawMessage(`{"tool":"convert","arguments":{"amount":{"$param":"invoiceAmount"},"currency":{"$param":"invoiceCurrency"}}}`),
			Read:        SourceRead{Rule: calculatorRule()},
			Calculation: &CalculationBinding{Inputs: map[string]string{"amount": "invoiceAmount", "currency": "invoiceCurrency"}, Tables: map[string]int{"ecb-rates": 86400}}}}}
	input := Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"amount":"1200.00","currency":"EUR"}`), Sources: map[string]SourceValue{}}}
	answer(t, &input, p, key, at, calculation(at))
	return input, p, key, at
}

// answer signs a calculator's answer carrying calc, and converted.
func answer(t *testing.T, i *Input, p InputProfile, key ed25519.PrivateKey, at time.Time, calc any) {
	t.Helper()
	result := encode(map[string]any{"calculation": calc, "converted": "1302.36"})
	i.Source.Sources["fx"] = SourceValue{Response: signResponse(t, p, key, []byte(calculatorArgs), result, at, 0)}
}

func TestCalculatedValueIsDerivedAndItsLineageSaysFromWhat(t *testing.T) {
	input, p, _, at := calculatorFixture(t)
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if string(got.Facts) != `{"invoice":{"converted":"1302.36"}}` || string(got.Evidence) != `{"fx-rate":"present"}` {
		t.Fatal(string(got.Facts), string(got.Evidence))
	}
	want := `{"asOf":{"ecb-rates":"` + at.Add(-time.Hour).Format(time.RFC3339) + `"},"calculator":{"name":"fx-convert","version":"2.1.0"},"inputs":[{"name":"amount","parameter":"invoiceAmount","pointer":"/amount","source":"case"},{"name":"currency","parameter":"invoiceCurrency","pointer":"/currency","source":"case"}],"status":"computed"}`
	if len(got.Preparation.Lineage) != 2 {
		t.Fatal(string(encode(got.Preparation.Lineage)))
	}
	for _, l := range got.Preparation.Lineage {
		if l.Calculation == nil || string(canonTest(t, encode(l.Calculation))) != want || l.Reason != "converted" {
			t.Fatal(string(encode(l)))
		}
	}
	again, e := normalizeV2(got, []InputProfile{p}, at)
	if e != nil || !sameJSON(encode(again), encode(got)) {
		t.Fatal("not repeatable", e)
	}
}

func TestCalculatorAnswerIsHeldToItsBinding(t *testing.T) {
	input, p, key, at := calculatorFixture(t)
	hour := at.Add(-time.Hour).Format(time.RFC3339)
	notComputed := func(status string, change func(c map[string]any)) func(c map[string]any) {
		return func(c map[string]any) { c["status"] = status; change(c) }
	}
	unboundNull := func(c map[string]any) { c["inputs"].(map[string]any)["rounding"] = nil }
	unnamedTable := func(c map[string]any) { c["asOf"].(map[string]any)["holidays"] = hour }
	// Each answer is refused for its own reason, named by the message, so a
	// refusal that a later check happens to make does not hide a removed one.
	refused := map[string]struct {
		change func(c map[string]any)
		reason string
	}{
		"another calculator":                 {func(c map[string]any) { c["calculator"] = map[string]any{"name": "fx-other", "version": "2.1.0"} }, "another calculator or version"},
		"another version":                    {func(c map[string]any) { c["calculator"] = map[string]any{"name": "fx-convert", "version": "2.1.1"} }, "another calculator or version"},
		"calculator with a third member":     {func(c map[string]any) { c["calculator"].(map[string]any)["build"] = "x" }, "another calculator or version"},
		"a fifth member":                     {func(c map[string]any) { c["note"] = "x" }, "exactly calculator, status, inputs and asOf"},
		"no asOf member":                     {func(c map[string]any) { delete(c, "asOf") }, "exactly calculator, status, inputs and asOf"},
		"inputs not an object":               {func(c map[string]any) { c["inputs"] = []any{} }, "exactly calculator, status, inputs and asOf"},
		"an unknown status":                  {func(c map[string]any) { c["status"] = "partial" }, "not computed, input-missing or cannot-compute"},
		"an input of another value":          {func(c map[string]any) { c["inputs"].(map[string]any)["amount"] = "1200.01" }, "amount does not equal its parameter"},
		"an input of another type":           {func(c map[string]any) { c["inputs"].(map[string]any)["amount"] = 1200 }, "amount does not equal its parameter"},
		"an input the mapping does not bind": {func(c map[string]any) { c["inputs"].(map[string]any)["rounding"] = "half-even" }, "an input the mapping does not bind"},
		// Not computed, nothing else holds these: an unbound input is null, as
		// an absent parameter would be, and no table is held to its limit.
		"input-missing with an unbound null input":     {notComputed("input-missing", unboundNull), "an input the mapping does not bind"},
		"cannot-compute with an unbound null input":    {notComputed("cannot-compute", unboundNull), "an input the mapping does not bind"},
		"input-missing with a table it does not name":  {notComputed("input-missing", unnamedTable), "a table the mapping does not name"},
		"cannot-compute with a table it does not name": {notComputed("cannot-compute", unnamedTable), "a table the mapping does not name"},
		"a computed answer missing an input":           {func(c map[string]any) { delete(c["inputs"].(map[string]any), "currency") }, "must echo every input"},
		"a table the mapping does not name":            {unnamedTable, "a table the mapping does not name"},
		"a computed answer missing a table":            {func(c map[string]any) { c["asOf"] = map[string]any{} }, "must report every table"},
		"a table without an instant": {func(c map[string]any) {
			c["asOf"].(map[string]any)["ecb-rates"] = at.Add(-time.Hour).Format("2006-01-02T15:04:05") + ".5Z"
		}, "ecb-rates has no as-of instant"},
		"a table older than its limit": {func(c map[string]any) {
			c["asOf"].(map[string]any)["ecb-rates"] = at.Add(-86401 * time.Second).Format(time.RFC3339)
		}, "ecb-rates is older than its limit or from the future"},
		"a table from the future": {func(c map[string]any) {
			c["asOf"].(map[string]any)["ecb-rates"] = at.Add(31 * time.Second).Format(time.RFC3339)
		}, "ecb-rates is older than its limit or from the future"},
	}
	for name, r := range refused {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			c := calculation(at)
			r.change(c)
			answer(t, &i, p, key, at, c)
			_, e := normalizeV2(i, []InputProfile{p}, at)
			var problem *apiError
			if e == nil || !errors.As(e, &problem) || problem.Code != "input_preparation_failed" || !strings.Contains(problem.Message, r.reason) {
				t.Fatalf("not refused for %q: %v", r.reason, e)
			}
		})
	}
	for name, asOf := range map[string]time.Time{"at its limit": at.Add(-86400 * time.Second), "30 seconds ahead": at.Add(30 * time.Second)} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			c := calculation(at)
			c["asOf"].(map[string]any)["ecb-rates"] = asOf.Format(time.RFC3339)
			answer(t, &i, p, key, at, c)
			if _, e := normalizeV2(i, []InputProfile{p}, at); e != nil {
				t.Fatal(e)
			}
		})
	}
	t.Run("an answer without a calculation member", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Sources["fx"] = SourceValue{Response: signResponse(t, p, key, []byte(calculatorArgs), []byte(`{"converted":"1302.36"}`), at, 0)}
		if _, e := normalizeV2(i, []InputProfile{p}, at); e == nil {
			t.Fatal("accepted")
		}
	})
}

func TestCalculationThatCouldNotComputeIsUnknownUnderItsOwnReason(t *testing.T) {
	input, p, key, at := calculatorFixture(t)
	// A source that needs the converted amount is skipped when there is none.
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "ledger", Kind: "operation", Profile: "vendors", ProfileDigest: "", MaxAge: 300, Parameters: map[string]Parameter{"converted": {From: "fx", Pointer: "/invoice/converted", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"converted":{"$param":"converted"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/ledger", "/ledger"}}, Evidence: []EvidenceMapping{}}}})
	vendors, _ := testProfile(t)
	input.Source.Mapping.Sources[1].ProfileDigest = profileHash(vendors)
	profiles := []InputProfile{p, vendors}
	for _, status := range []string{"input-missing", "cannot-compute"} {
		t.Run(status, func(t *testing.T) {
			i := cloneInput(input)
			c := calculation(at)
			c["status"] = status
			delete(c["inputs"].(map[string]any), "currency")
			// Not computed: a table is not held to its limit, and the answer's
			// value is not read, though it carries one.
			c["asOf"].(map[string]any)["ecb-rates"] = at.Add(-400 * 24 * time.Hour).Format(time.RFC3339)
			answer(t, &i, p, key, at, c)
			got, e := normalizeV2(i, profiles, at)
			if e != nil {
				t.Fatal(e)
			}
			if string(got.Facts) != `{}` || string(got.Evidence) != `{"fx-rate":"unknown"}` {
				t.Fatal(string(got.Facts), string(got.Evidence))
			}
			fx := got.Preparation.Outcomes[1]
			if fx.Name != "fx" || fx.Status != "unknown" || fx.Reason != "calculation-"+status || got.Preparation.Outcomes[2].Reason != "dependency-unavailable" {
				t.Fatal(string(encode(got.Preparation.Outcomes)))
			}
			for _, l := range got.Preparation.Lineage {
				if l.Source != "fx" {
					continue
				}
				if l.Reason != "calculation-"+status || strings.Join(l.Basis, ",") != "/calculation/status" || l.Calculation == nil || l.Calculation.Status != status || len(l.Calculation.Inputs) != 1 || l.Calculation.Inputs[0].Name != "amount" {
					t.Fatal(string(encode(l)))
				}
			}
		})
	}
	t.Run("an echoed input is still held to its parameter", func(t *testing.T) {
		i := cloneInput(input)
		c := calculation(at)
		c["status"] = "cannot-compute"
		c["inputs"].(map[string]any)["amount"] = "9.99"
		answer(t, &i, p, key, at, c)
		if _, e := normalizeV2(i, profiles, at); e == nil {
			t.Fatal("an answer about other inputs became an unknown")
		}
	})
}

func TestACalculatedSourceSkippedBehindAnotherCarriesNoCalculation(t *testing.T) {
	input, p, key, at := calculatorFixture(t)
	tax, taxKey := testProfile(t)
	tax.ID = "tax"
	tax.Source = "tax/live"
	tax.Tools = []string{"tax"}
	tax.Calculator = &CalculatorPin{"tax-calc", "1.0.0"}
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "tax", Kind: "operation", Profile: tax.ID, ProfileDigest: profileHash(tax), MaxAge: 300,
		Parameters:  map[string]Parameter{"converted": {From: "fx", Pointer: "/invoice/converted", Type: "string"}},
		Arguments:   json.RawMessage(`{"tool":"tax","arguments":{"amount":{"$param":"converted"}}}`),
		Read:        SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/invoice/tax", "/tax"}}, Evidence: []EvidenceMapping{}}},
		Calculation: &CalculationBinding{Inputs: map[string]string{"amount": "converted"}, Tables: map[string]int{}}})
	profiles := []InputProfile{p, tax}
	computed := cloneInput(input)
	taxAnswer := encode(map[string]any{"calculation": map[string]any{"calculator": map[string]any{"name": "tax-calc", "version": "1.0.0"}, "status": "computed", "inputs": map[string]any{"amount": "1302.36"}, "asOf": map[string]any{}}, "tax": "65.12"})
	computed.Source.Sources["tax"] = SourceValue{Response: signResponse(t, tax, taxKey, []byte(`{"tool":"tax","arguments":{"amount":"1302.36"}}`), taxAnswer, at, 1)}
	got, e := normalizeV2(computed, profiles, at)
	if e != nil {
		t.Fatal(e)
	}
	// An input taken from an earlier source names that source and its fact.
	last := got.Preparation.Lineage[len(got.Preparation.Lineage)-1]
	if string(got.Facts) != `{"invoice":{"converted":"1302.36","tax":"65.12"}}` || last.Source != "tax" || last.Calculation == nil ||
		string(canonTest(t, encode(last.Calculation.Inputs))) != `[{"name":"amount","parameter":"converted","pointer":"/invoice/converted","source":"fx"}]` {
		t.Fatal(string(got.Facts), string(encode(last)))
	}
	// When the first could not compute, the second acquires nothing, and its
	// entries say why without a calculation member.
	skipped := cloneInput(input)
	c := calculation(at)
	c["status"] = "cannot-compute"
	answer(t, &skipped, p, key, at, c)
	got, e = normalizeV2(skipped, profiles, at)
	if e != nil {
		t.Fatal(e)
	}
	seen := 0
	for _, l := range got.Preparation.Lineage {
		if l.Source == "tax" {
			seen++
			if l.Reason != "dependency-unavailable" || l.Calculation != nil {
				t.Fatal(string(encode(l)))
			}
		}
	}
	if seen == 0 {
		t.Fatal("no lineage for the skipped source")
	}
}

// The export in testdata/mapping-v2-before-calculators was written by Runner
// before calculators existed: TestV2RealRuntimeReleaseRunOfflineAndRestart on
// main at a71e4be, with JPACK_V2_TEST_EXPORT_DIR set. Verifying it recomputes
// its profile, mapping and release digests and its lineage with this code, so
// it verifies only if they encode as they did.
func TestARunRecordedBeforeCalculatorsVerifiesUnchanged(t *testing.T) {
	raw, profiles, release := beforeCalculators(t)
	if e := VerifyRun(raw, profiles, release); e != nil {
		t.Fatal("a run recorded before calculators no longer verifies:", e)
	}
	var bundle VerificationBundle
	if e := json.Unmarshal(raw, &bundle); e != nil {
		t.Fatal(e)
	}
	if bundle.Run.Input.Source.Mapping.Sources[0].ProfileDigest != profileHash(profiles[0]) || bytes.Contains(raw, []byte(`"calculat`)) {
		t.Fatal("the frozen profile digest differs from this code's")
	}
}

// Its three targets were read from a record operation. The case's parameter,
// which chose the operation's request, is a dependency and is not counted.
func TestARunRecordedBeforeCalculatorsCountsItsTargetsByClass(t *testing.T) {
	raw, profiles, release := beforeCalculators(t)
	v, e := VerifyInputs(raw, profiles, release)
	if e != nil || v.Classes != (InputClasses{Record: 3}) {
		t.Fatal(e, v.Classes)
	}
	var bundle VerificationBundle
	if e = json.Unmarshal(raw, &bundle); e != nil {
		t.Fatal(e)
	}
	if len(bundle.Run.Input.Source.Mapping.Case.Parameters) != 1 || len(bundle.Run.Input.Preparation.Lineage) != 3 {
		t.Fatal("the fixture no longer has a case parameter and three targets")
	}
}

func beforeCalculators(t *testing.T) (raw []byte, profiles []InputProfile, release string) {
	t.Helper()
	dir := "testdata/mapping-v2-before-calculators/"
	raw, e := os.ReadFile(dir + "run.json")
	if e != nil {
		t.Fatal(e)
	}
	rawProfiles, e := os.ReadFile(dir + "profiles.json")
	if e != nil {
		t.Fatal(e)
	}
	trusted, e := os.ReadFile(dir + "release-digest.txt")
	if e != nil {
		t.Fatal(e)
	}
	if profiles, e = ParseInputProfiles(rawProfiles); e != nil {
		t.Fatal(e)
	}
	return raw, profiles, strings.TrimSpace(string(trusted))
}

func TestCalculatorProfileAndBindingGoTogetherBeforeAnyAcquisition(t *testing.T) {
	input, p, _, at := calculatorFixture(t)
	plain := p
	plain.Calculator = nil
	unbound := cloneInput(input)
	unbound.Source.Mapping.Sources[0].Calculation = nil
	bound := cloneInput(input)
	bound.Source.Mapping.Sources[0].ProfileDigest = profileHash(plain)
	for name, c := range map[string]struct {
		i Input
		p InputProfile
	}{"calculator without binding": {unbound, p}, "binding without calculator": {bound, plain}} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(c.i)
			i.Source.Sources = map[string]SourceValue{}
			_, e := normalizeV2Mode(i, []InputProfile{c.p}, at, true)
			var needed *acquisitionNeeded
			if e == nil || errors.As(e, &needed) || !strings.Contains(e.Error(), "go together") {
				t.Fatalf("planned an acquisition: %v", e)
			}
		})
	}
	bindings := map[string]func(b *CalculationBinding){
		"an input bound to an undeclared parameter": func(b *CalculationBinding) { b.Inputs["amount"] = "total" },
		"an input bound to runAt":                   func(b *CalculationBinding) { b.Inputs["amount"] = "runAt" },
		"an input of an invalid name":               func(b *CalculationBinding) { b.Inputs["1amount"] = "invoiceAmount" },
		"no inputs member":                          func(b *CalculationBinding) { b.Inputs = nil },
		"no tables member":                          func(b *CalculationBinding) { b.Tables = nil },
		"a table without a limit":                   func(b *CalculationBinding) { b.Tables["ecb-rates"] = 0 },
		"a table limit past ten years":              func(b *CalculationBinding) { b.Tables["ecb-rates"] = maxTableAge + 1 },
		"a table of an invalid name":                func(b *CalculationBinding) { b.Tables["ecb/rates"] = 60 },
	}
	for name, change := range bindings {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			change(i.Source.Mapping.Sources[0].Calculation)
			if e := i.Source.Mapping.validate(); e == nil {
				t.Fatal("binding accepted")
			}
		})
	}
	t.Run("a binding on a selected file", func(t *testing.T) {
		m := InputMapping{Version: 2, Sources: []MappingSource{{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/x", "/x"}}, Evidence: []EvidenceMapping{}}}, Calculation: &CalculationBinding{Inputs: map[string]string{}, Tables: map[string]int{}}}}}
		if e := m.validate(); e == nil || !strings.Contains(e.Error(), "only an operation") {
			t.Fatal(e)
		}
	})
	t.Run("the largest limit is admitted", func(t *testing.T) {
		i := cloneInput(input)
		i.Source.Mapping.Sources[0].Calculation.Tables["ecb-rates"] = maxTableAge
		if e := i.Source.Mapping.validate(); e != nil {
			t.Fatal(e)
		}
	})
	profiles := map[string]func(p *InputProfile){
		"a generated calculator":         func(p *InputProfile) { p.Class = "generated" },
		"a calculator of another shape":  func(p *InputProfile) { p.Shape = "command" },
		"a calculator without a name":    func(p *InputProfile) { p.Calculator.Name = "" },
		"a calculator without a version": func(p *InputProfile) { p.Calculator.Version = "" },
		"a calculator name past 256 bytes": func(p *InputProfile) {
			p.Calculator.Name = strings.Repeat("n", 257)
		},
	}
	for name, change := range profiles {
		t.Run(name, func(t *testing.T) {
			q, _ := calculatorProfile(t)
			change(&q)
			if e := validateProfiles([]InputProfile{q}); e == nil {
				t.Fatal("profile accepted")
			}
		})
	}
}

func TestProfilesMappingsAndLineageWithoutACalculatorEncodeAsBefore(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	for name, raw := range map[string][]byte{"profile": encode(p), "mapping": encode(input.Source.Mapping), "lineage": encode(got.Preparation.Lineage)} {
		if bytes.Contains(raw, []byte(`"calculator"`)) || bytes.Contains(raw, []byte(`"calculation"`)) {
			t.Fatalf("%s gained a member: %s", name, raw)
		}
	}
	parsed, e := ParseInputProfiles(encode([]InputProfile{p}))
	if e != nil || profileHash(parsed[0]) != profileHash(p) {
		t.Fatal("a profile without a calculator changed digest", e)
	}
}

func TestCalculatedRunIsEvaluatedAndVerifiedOffline(t *testing.T) {
	cfg := testConfig(t)
	input, p, key, at := calculatorFixture(t)
	cfg.InputProfiles = []InputProfile{p}
	// The real pack reads one object and two evidence requirements; the
	// calculator answers with them beside its calculation member.
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}}
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	result := map[string]any{"calculation": calculation(at)}
	_ = json.Unmarshal(encode(sample()), &result)
	input.Source.Sources["fx"] = SourceValue{Response: signResponse(t, p, key, []byte(calculatorArgs), encode(result), at, 0)}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	release, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Calculated", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, _, e := s.submit(job.ID, "calc-1", input)
	if e != nil {
		t.Fatal(e)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" {
		t.Fatal(done.Problem)
	}
	bundle := verificationExport(release, done, 2)
	if e = VerifyRun(encode(bundle), []InputProfile{p}, bundle.ReleaseDigest); e != nil {
		t.Fatal("offline verification", e)
	}
	l := bundle.Run.Input.Preparation.Lineage[0]
	if l.Calculation == nil || l.Calculation.Status != "computed" {
		t.Fatal(string(encode(l)))
	}
	l.Calculation.AsOf["ecb-rates"] = at.Format(time.RFC3339)
	if e = VerifyRun(encode(bundle), []InputProfile{p}, bundle.ReleaseDigest); e == nil {
		t.Fatal("an altered calculation lineage verified")
	}
}
