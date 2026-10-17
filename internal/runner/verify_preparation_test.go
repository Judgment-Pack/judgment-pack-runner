package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// vectorFixture is a vector as verify-preparation is given it: its dialect,
// bound (mapping.json), or, imported, published (unbound.json) with its
// binding document; its case, acquisitions and profiles; and its
// preparation.json, which preparation.sha256 pins.
func vectorFixture(t *testing.T, v preparationVector, imported bool) PreparationFixture {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(preparationVectors + v.Name + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	f := PreparationFixture{Dialect: read("mapping.json"), Case: read("case.json"), Profiles: read("profiles.json"), Preparation: read("preparation.json")}
	if PreparationPin(f.Preparation, string(read("preparation.sha256"))) != nil {
		t.Fatal("preparation.sha256 does not pin preparation.json")
	}
	if imported {
		f.Dialect, f.Binding = read("unbound.json"), read("binding.json")
	}
	if _, ok := v.Files["sources.json"]; ok {
		f.Sources = read("sources.json")
	}
	return f
}

// withPreparation is f given another preparation.
func withPreparation(f PreparationFixture, preparation []byte) PreparationFixture {
	f.Preparation = preparation
	return f
}

// editJSON is raw decoded, changed by change, and encoded again.
func editJSON(t *testing.T, raw []byte, change func(v any) any) []byte {
	t.Helper()
	v, err := readInputJSONDepth(raw, 64)
	if err != nil {
		t.Fatal(err)
	}
	return encode(change(v))
}

// object is the object at path in v.
func object(t *testing.T, v any, path ...string) map[string]any {
	t.Helper()
	for _, name := range path {
		v = v.(map[string]any)[name]
	}
	o, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("no object at %v", path)
	}
	return o
}

// requireReport holds a report to its result and code, and its line to §11's
// form for them.
func requireReport(t *testing.T, r PreparationReport, result, code string) {
	t.Helper()
	if r.Result != result || r.Code != code {
		t.Fatalf("the report is %s %s (%s), not %s %s", r.Result, r.Code, r.Reason, result, code)
	}
	if result != ResultReading {
		if want := `{"` + result + `":"` + code + `","result":"` + result + `"}`; string(r.Line()) != want || r.Reason == "" {
			t.Fatalf("the report's line is %s, not %s, or it says no reason", r.Line(), want)
		}
	}
}

// codeNamed is the code a refusal's sentence names at its end: "(code)."
var codeNamed = regexp.MustCompile(`\(([a-z]+(?:-[a-z]+)+)\)\. No decision was requested\.$`)

// Every vector, given as a fixture is (preparation.json, its digest apart),
// gives the answer its manifest records, through Runner's own preparation at
// the vector's verifiedAt: a reading of preparation.sha256, binding
// not-checked, whose facts and evidence are the ones the export retains and
// whose counts, unsigned parameters and unsigned freshness are the members
// verify-run reports; or, for a vector whose inputs were changed after the
// run (retained "recorded"), the finding verify-run names, with its sentence,
// or, where verify-run refuses the release's frozen profile first, the
// finding of the binding that names it (recordedFinding).
// A vector bound at import reads the same published with its binding document
// as bound. Where the export retains another preparation (retained
// "altered"), that preparation is found preparation-differs: the old
// preparation under a changed binding among them.
func TestVerifyPreparationReadsEachVector(t *testing.T) {
	for _, v := range readPreparationVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			export, err := os.ReadFile(preparationVectors + v.Name + "/run.json")
			if err != nil {
				t.Fatal(err)
			}
			forms := []bool{false}
			if v.Import != nil {
				forms = append(forms, true)
			}
			var lines []string
			for _, imported := range forms {
				r := VerifyPreparation(vectorFixture(t, v, imported))
				lines = append(lines, string(r.Line()))
				if v.Preparation.Retained == "recorded" {
					code, named := recordedFinding(t, *v.VerifyRun.Error)
					requireReport(t, r, ResultFinding, code)
					if named && r.Reason != *v.VerifyRun.Error {
						t.Fatalf("the finding says %q, not verify-run's %q", r.Reason, *v.VerifyRun.Error)
					}
					continue
				}
				requireReport(t, r, ResultReading, "")
				if r.Digest != v.Preparation.SHA256 || !bytes.Contains(r.Line(), []byte(`"binding":"not-checked"`)) || !bytes.Contains(r.Line(), []byte(`"digest":"`+v.Preparation.SHA256+`"`)) {
					t.Fatalf("the reading is %s, not of %s with its binding not checked", r.Line(), v.Preparation.SHA256)
				}
				if v.Preparation.Retained == "equal" {
					for name, got := range map[string]json.RawMessage{"facts": r.Facts, "evidence": r.Evidence} {
						held := member(export, "run", "input", name)
						if len(held) == 0 {
							held = json.RawMessage(`{}`)
						}
						if !sameJSON(got, held) {
							t.Fatalf("the reading's %s are %s, not the export's %s", name, got, held)
						}
					}
				}
				// verify-run's report of a vector it verifies; one that refuses
				// for the export's disposition says nothing of the inputs.
				if v.VerifyRun.Report != nil && strings.Contains(*v.VerifyRun.Report, `"targetsByClass"`) {
					var report map[string]json.RawMessage
					if err := json.Unmarshal([]byte(*v.VerifyRun.Report), &report); err != nil {
						t.Fatal(err)
					}
					var line map[string]json.RawMessage
					if err := json.Unmarshal(r.Line(), &line); err != nil {
						t.Fatal(err)
					}
					for _, name := range []string{"targetsByClass", "unsignedParameters", "unsignedFreshness"} {
						if (len(report[name]) == 0) != (len(line[name]) == 0) || len(line[name]) > 0 && !sameJSON(report[name], line[name]) {
							t.Fatalf("the reading's %s is %s, not verify-run's %s", name, line[name], report[name])
						}
					}
				}
			}
			if len(lines) == 2 && lines[0] != lines[1] {
				t.Fatalf("bound at import, the vector reads otherwise than bound:\n%s\n%s", lines[1], lines[0])
			}
			if v.Preparation.Retained == "altered" {
				retained := member(export, "run", "input", "preparation")
				for _, imported := range forms {
					requireReport(t, VerifyPreparation(withPreparation(vectorFixture(t, v, imported), retained)), ResultFinding, codeDiffers)
				}
			}
		})
	}
}

// vectorNamed is the manifest's vector of that name.
func vectorNamed(t *testing.T, name string) preparationVector {
	t.Helper()
	for _, v := range readPreparationVectors(t) {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no vector %s", name)
	return preparationVector{}
}

// Each fault is refused or found by its name, with nothing reported but its
// code: one change to a vector each. A changed retained response, under its
// receipt as it was, is acquisition-invalid; a binding that names a
// profile the verifier is not given by that id and digest, or one of another
// shape or adapter, is profile-mismatch; a binding of another dialect, a
// verifiedAt that is no instant, and a document of another form are
// input-malformed; a receipt outside its window at verifiedAt is
// acquisition-stale; a kind the verifier does not read is kind-unread; a
// document over its bound is input-over-bound.
func TestVerifyPreparationRefusesAndFindsByName(t *testing.T) {
	valid := vectorNamed(t, "valid")
	imported := vectorNamed(t, "unbound-bound-at-import")
	research := vectorNamed(t, "gtm-company-person-valid-types-all-six")
	firstSource := func(f *PreparationFixture, change func(response map[string]any)) {
		f.Sources = editJSON(t, f.Sources, func(v any) any {
			for _, name := range sortedKeys(v.(map[string]any)) {
				change(object(t, v, name, "response"))
				break
			}
			return v
		})
	}
	firstBinding := func(f *PreparationFixture, change func(entry map[string]any)) {
		f.Binding = editJSON(t, f.Binding, func(v any) any {
			for _, name := range sortedKeys(object(t, v, "sources")) {
				change(object(t, v, "sources", name))
				break
			}
			return v
		})
	}
	// anotherProfile adds a profile, the first changed, and binds the first
	// unbound source to it.
	anotherProfile := func(f *PreparationFixture, change func(p *InputProfile)) {
		profiles, err := ParseInputProfiles(f.Profiles)
		if err != nil {
			t.Fatal(err)
		}
		other := profiles[0]
		other.ID = "another"
		change(&other)
		f.Profiles = encode(append(profiles, other))
		firstBinding(f, func(entry map[string]any) { entry["profile"], entry["profileDigest"] = "another", profileHash(other) })
	}
	at := func(f *PreparationFixture, verifiedAt string) {
		*f = withPreparation(*f, bytes.Replace(f.Preparation, []byte(`"verifiedAt":"2026-10-14T14:58:26Z"`), []byte(`"verifiedAt":`+verifiedAt), 1))
	}
	cases := []struct {
		name     string
		vector   preparationVector
		imported bool
		change   func(f *PreparationFixture)
		result   string
		code     string
	}{
		{"a changed retained response", valid, false, func(f *PreparationFixture) {
			firstSource(f, func(r map[string]any) { r["result"] = map[string]any{"changed": true} })
		}, ResultFinding, codeInvalid},
		{"a receipt's signature changed", valid, false, func(f *PreparationFixture) {
			firstSource(f, func(r map[string]any) {
				receipt := r["receipt"].(map[string]any)
				receipt["signature"] = strings.Repeat("0", 128)
			})
		}, ResultFinding, codeInvalid},
		{"a wrong profile digest in the binding", imported, true, func(f *PreparationFixture) {
			firstBinding(f, func(entry map[string]any) { entry["profileDigest"] = "sha256:" + strings.Repeat("1", 64) })
		}, ResultFinding, codeMismatch},
		{"a wrong AI profile digest in a research binding", research, true, func(f *PreparationFixture) {
			firstBinding(f, func(entry map[string]any) { entry["profileDigest"] = "sha256:" + strings.Repeat("1", 64) })
		}, ResultFinding, codeMismatch},
		{"a binding to a profile not given", imported, true, func(f *PreparationFixture) {
			firstBinding(f, func(entry map[string]any) { entry["profile"] = "not-given" })
		}, ResultFinding, codeMismatch},
		{"a binding to a profile of another shape", imported, true, func(f *PreparationFixture) {
			anotherProfile(f, func(p *InputProfile) { p.Shape, p.Tools = "mcp", []string{"lookup"} })
		}, ResultFinding, codeMismatch},
		{"a binding to a profile of another adapter", imported, true, func(f *PreparationFixture) {
			anotherProfile(f, func(p *InputProfile) { p.Adapter.Name = "adapter-other" })
		}, ResultFinding, codeMismatch},
		{"a binding of another dialect", imported, true, func(f *PreparationFixture) {
			f.Binding = editJSON(t, f.Binding, func(v any) any {
				v.(map[string]any)["dialect"] = "sha256:" + strings.Repeat("2", 64)
				return v
			})
		}, ResultRefusal, codeMalformed},
		{"a binding beside a dialect with no unbound source", imported, false, func(f *PreparationFixture) {
			b, err := os.ReadFile(preparationVectors + imported.Name + "/binding.json")
			if err != nil {
				t.Fatal(err)
			}
			f.Binding = b
		}, ResultRefusal, codeMalformed},
		{"an unbound dialect without its binding", imported, true, func(f *PreparationFixture) { f.Binding = nil }, ResultRefusal, codeMalformed},
		{"a verifiedAt of a day that does not exist", valid, false, func(f *PreparationFixture) { at(f, `"2026-02-30T00:00:00Z"`) }, ResultRefusal, codeMalformed},
		{"a verifiedAt with a fraction", valid, false, func(f *PreparationFixture) { at(f, `"2026-10-14T14:58:26.000Z"`) }, ResultRefusal, codeMalformed},
		{"a verifiedAt with an offset", valid, false, func(f *PreparationFixture) { at(f, `"2026-10-14T14:58:26+00:00"`) }, ResultRefusal, codeMalformed},
		{"a verifiedAt at a leap second", valid, false, func(f *PreparationFixture) { at(f, `"2026-10-14T23:59:60Z"`) }, ResultRefusal, codeMalformed},
		{"a verifiedAt that is a number", valid, false, func(f *PreparationFixture) { at(f, `1760453906`) }, ResultRefusal, codeMalformed},
		{"no verifiedAt", valid, false, func(f *PreparationFixture) {
			*f = withPreparation(*f, bytes.Replace(f.Preparation, []byte(`"verifiedAt":"2026-10-14T14:58:26Z",`), nil, 1))
		}, ResultRefusal, codeMalformed},
		{"a receipt stale at verifiedAt", valid, false, func(f *PreparationFixture) { at(f, `"2026-10-16T12:00:00Z"`) }, ResultFinding, codeStale},
		{"a receipt from after verifiedAt", valid, false, func(f *PreparationFixture) { at(f, `"2026-10-13T12:00:00Z"`) }, ResultFinding, codeStale},
		{"no acquisitions", valid, false, func(f *PreparationFixture) { f.Sources = nil }, ResultFinding, codeMissing},
		{"an acquisition for no source", valid, false, func(f *PreparationFixture) {
			f.Sources = editJSON(t, f.Sources, func(v any) any {
				o := v.(map[string]any)
				for _, name := range sortedKeys(o) {
					o["another"] = o[name]
					break
				}
				return o
			})
		}, ResultFinding, codeUnexpected},
		{"a case parameter missing", valid, false, func(f *PreparationFixture) { f.Case = []byte(`{}`) }, ResultFinding, codeParameter},
		{"the dialect's pinned profile digest changed", valid, false, func(f *PreparationFixture) {
			f.Dialect = editJSON(t, f.Dialect, func(v any) any {
				v.(map[string]any)["sources"].([]any)[0].(map[string]any)["profileDigest"] = "sha256:" + strings.Repeat("0", 64)
				return v
			})
		}, ResultFinding, codeMismatch},
		{"a member more in the preparation", valid, false, func(f *PreparationFixture) {
			*f = withPreparation(*f, bytes.Replace(f.Preparation, []byte(`{"cites"`), []byte(`{"another":1,"cites"`), 1))
		}, ResultFinding, codeDiffers},
		{"a source of kind selected-file", vectorNamed(t, "escaped-strings"), false, func(f *PreparationFixture) {
			f.Dialect = editJSON(t, f.Dialect, func(v any) any {
				v.(map[string]any)["sources"] = []any{map[string]any{"name": "file", "kind": "selected-file", "provider": "local-file", "read": map[string]any{"copy": map[string]any{"facts": []any{}, "evidence": []any{}}}}}
				return v
			})
		}, ResultRefusal, codeKindUnread},
		{"a dialect with a member twice", valid, false, func(f *PreparationFixture) {
			f.Dialect = bytes.Replace(f.Dialect, []byte(`{"case"`), []byte(`{"version":2,"case"`), 1)
		}, ResultRefusal, codeMalformed},
		{"a profile whose key is refused", valid, false, func(f *PreparationFixture) {
			f.Profiles = regexp.MustCompile(`"publicKey":"[0-9a-f]{64}"`).ReplaceAll(f.Profiles, []byte(`"publicKey":"`+strings.Repeat("0", 64)+`"`))
		}, ResultRefusal, codeMalformed},
		{"an acquisition without its receipt", valid, false, func(f *PreparationFixture) {
			firstSource(f, func(r map[string]any) { delete(r, "receipt") })
		}, ResultRefusal, codeMalformed},
		{"an acquisition whose salts are of another form", valid, false, func(f *PreparationFixture) {
			firstSource(f, func(r map[string]any) { r["salts"] = map[string]any{"args": "zz"} })
		}, ResultRefusal, codeMalformed},
		{"a dialect over its bound", valid, false, func(f *PreparationFixture) {
			f.Dialect = append(f.Dialect, bytes.Repeat([]byte(" "), maxPublishedDialect)...)
		}, ResultRefusal, codeOverBound},
		{"a number literal of 129 characters", valid, false, func(f *PreparationFixture) {
			f.Case = bytes.Replace(f.Case, []byte(`{`), []byte(`{"long":`+strings.Repeat("1", 129)+`,`), 1)
		}, ResultRefusal, codeOverBound},
		{"a result nested 33 levels", valid, false, func(f *PreparationFixture) {
			firstSource(f, func(r map[string]any) {
				var deep any = 0
				for range 33 {
					deep = []any{deep}
				}
				r["result"] = deep
			})
		}, ResultRefusal, codeOverBound},
	}
	// What a binding's finding says: the fault is the binding document's,
	// found as it binds, and not a profile's found later.
	says := map[string]string{
		"a wrong profile digest in the binding":             "the binding document binds source",
		"a wrong AI profile digest in a research binding":   "by a digest that is not the digest",
		"a binding to a profile not given":                  "the binding document binds source",
		"a binding to a profile of another shape":           "of shape mcp",
		"a binding to a profile of another adapter":         "whose adapter is adapter-other",
		"the dialect's pinned profile digest changed":       "trusted input profile changed",
		"a binding beside a dialect with no unbound source": "has no unbound source",
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := vectorFixture(t, c.vector, c.imported)
			c.change(&f)
			r := VerifyPreparation(f)
			requireReport(t, r, c.result, c.code)
			if !strings.Contains(r.Reason, says[c.name]) {
				t.Fatalf("the reason %q does not say %q", r.Reason, says[c.name])
			}
		})
	}
}

// historicalFixture is the fixture of acquiredDependentInput's run, its three
// receipts served at served, prepared at served.
func historicalFixture(t *testing.T, served time.Time) PreparationFixture {
	t.Helper()
	p, key := skippedSourceProfile(t)
	input := acquiredDependentInput(t, p, key, served, true)
	prepared, err := prepareV2(input, []InputProfile{p}, served, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := canonical(encode(prepared.Preparation))
	if err != nil {
		t.Fatal(err)
	}
	return PreparationFixture{Dialect: encode(input.Source.Mapping), Case: input.Source.Case, Sources: encode(input.Source.Sources), Profiles: encode([]InputProfile{p}), Preparation: preparation}
}

// The instant is the preparation's verifiedAt, and never the clock. Receipts
// served two days ago, of sources whose maxAge is five minutes, are stale
// now and fresh at the instant they were prepared at: they read. Receipts
// served now, fresh now, are stale an hour earlier: a preparation that names
// that instant is found acquisition-stale, and not read at the current time.
func TestVerifyPreparationRecomputesAtTheHistoricalInstant(t *testing.T) {
	then := time.Now().UTC().Truncate(time.Second).Add(-48 * time.Hour)
	f := historicalFixture(t, then)
	r := VerifyPreparation(f)
	requireReport(t, r, ResultReading, "")
	if r.Digest != digest(f.Preparation) || r.UnsignedFreshness == nil || r.UnsignedFreshness.VerifiedAt != then.Format("2006-01-02T15:04:05Z") {
		t.Fatalf("the receipts of two days ago do not read at their own instant: %s", r.Line())
	}
	now := time.Now().UTC().Truncate(time.Second)
	f = historicalFixture(t, now)
	stamp := []byte(`"verifiedAt":"` + now.Format("2006-01-02T15:04:05Z") + `"`)
	earlier := bytes.Replace(f.Preparation, stamp, []byte(`"verifiedAt":"`+now.Add(-time.Hour).Format("2006-01-02T15:04:05Z")+`"`), 1)
	if bytes.Equal(earlier, f.Preparation) {
		t.Fatal("the preparation names no instant to move")
	}
	requireReport(t, VerifyPreparation(withPreparation(f, earlier)), ResultFinding, codeStale)
}

// A report is written as §11 writes it: one object in the preparation's
// canonical form, members sorted, the reading's members exactly, and the
// exit status of its result.
func TestAPreparationReportIsWrittenInTheCanonicalForm(t *testing.T) {
	r := VerifyPreparation(vectorFixture(t, vectorNamed(t, "escaped-strings"), false))
	requireReport(t, r, ResultReading, "")
	var members map[string]json.RawMessage
	if err := json.Unmarshal(r.Line(), &members); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sortedKeys(members), " "); got != "binding digest evidence facts result targetsByClass" {
		t.Fatalf("the reading has the members %s", got)
	}
	if again, err := canonical(r.Line()); err != nil || !bytes.Equal(again, r.Line()) || !bytes.Contains(r.Line(), []byte(`A\u0026B \u003cx\u003e café ☃`)) {
		t.Fatalf("the reading is not in the canonical form: %s", r.Line())
	}
	for result, status := range map[string]int{ResultReading: 0, ResultRefusal: 1, ResultFinding: 2} {
		if (PreparationReport{Result: result}).ExitStatus() != status {
			t.Fatalf("a %s does not exit %d", result, status)
		}
	}
}

// What verify-preparation --capabilities prints is a closed object, exactly
// these members: a change to any is a change to the interface, and to
// PreparationVerifierVersion with it.
func TestThePreparationVerifiersCapabilities(t *testing.T) {
	want := `{"bounds":{"bindingBytes":65536,"clauses":64,"conditions":64,"dialectBytes":131072,"documentDepth":64,"envelopeBytes":2097152,"evidenceTargets":128,"exponent":308,"factTargets":256,"numberCharacters":128,"parameters":64,"preparationBytes":8388608,"profilesBytes":65536,"retainedResponses":64,"sources":16,"unwrap":4,"unwrappedBytes":200000,"valueDepth":32},` +
		`"command":"verify-preparation","dialectVersions":[2,3],"formatVersions":["2.0.0","3.0.0"],"instant":"preparation.verifiedAt",` +
		`"report":{"binding":["not-checked"],"exitStatus":{"couldNotRun":3,"finding":2,"reading":0,"refusal":1},"findings":["profile-mismatch","parameter-invalid","read-failed","acquisition-missing","acquisition-unexpected","acquisition-invalid","acquisition-stale","acquisition-sessions","calculation-invalid","admission-refused","preparation-differs"],"form":"spec/dialect.md §11","preconditions":["preparation-digest"],"refusals":["input-over-bound","input-malformed","kind-unread"],"results":["reading","refusal","finding"],"version":1},` +
		`"runner":"v0.0.0-test","shapes":["airbyte","http","mcp"],"sourceKinds":["operation","research"],"version":1}`
	if got := string(PreparationVerifierCapabilities("v0.0.0-test")); got != want {
		t.Fatalf("the capabilities are\n%s\nnot\n%s", got, want)
	}
}

// The expected preparation's digest is a precondition (PreparationPin), and
// no step of the reading: the preparation it names passes; one it does not
// name, of another form, or changed by a byte, does not, and the reason names
// both digests. A preparation over its bound is not digested: the reading
// refuses it, input-over-bound.
func TestThePreparationPin(t *testing.T) {
	f := vectorFixture(t, vectorNamed(t, "valid"), false)
	pin := digest(f.Preparation)
	if err := PreparationPin(f.Preparation, pin); err != nil {
		t.Fatal(err)
	}
	other := digest([]byte("another preparation"))
	for name, c := range map[string]struct {
		preparation []byte
		pin         string
	}{
		"another digest":           {f.Preparation, other},
		"a digest of another form": {f.Preparation, strings.ToUpper(pin)},
		"a preparation changed":    {append(append([]byte(nil), f.Preparation...), ' '), pin},
		"no digest":                {f.Preparation, ""},
	} {
		err := PreparationPin(c.preparation, c.pin)
		if err == nil || !strings.Contains(err.Error(), digest(c.preparation)) || !strings.Contains(err.Error(), "it is not read") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	over := bytes.Repeat([]byte(" "), maxPreparationDocument+1)
	if err := PreparationPin(over, other); err != nil {
		t.Fatal("a preparation over its bound was digested:", err)
	}
	requireReport(t, VerifyPreparation(withPreparation(f, over)), ResultRefusal, codeOverBound)
}

// The refusals whose sentences are kept carry the codes §11 gives them: a
// receipt that is no object, or none, is of another form; a number a read
// unwraps over its bound, and a value nested deeper than a read applies to,
// are over the bound; a string that holds no JSON value is read-failed.
func TestTheSentencesKeptCarryTheirCodes(t *testing.T) {
	for receipt := range receiptSentences {
		input, profiles, at := validWithReceipt(t, receipt)
		_, err := prepareV2(input, profiles, at, false, nil)
		if result, code := verificationOutcome(err); err == nil || result != ResultRefusal || code != codeMalformed {
			t.Fatalf("a receipt %q is %s %s: %v", receipt, result, code, err)
		}
	}
	for held, want := range map[string]string{
		`{"vendor":1e309}`: codeOverBound,
		`{"vendor":` + strings.Repeat("1", 129) + `}`:           codeOverBound,
		strings.Repeat("[", 40) + "1" + strings.Repeat("]", 40): codeOverBound,
		`{"vendor":`: codeReadFailed,
	} {
		input, profiles, at := unwrapped(t, held)
		_, err := prepareV2(input, profiles, at, false, nil)
		if _, code := verificationOutcome(err); err == nil || code != want {
			t.Fatalf("an unwrapped %q is %s, not %s: %v", held, code, want, err)
		}
	}
}
