package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func columnCase(t *testing.T, kind, value string) Input {
	t.Helper()
	leaf := `{"contractVersion":"1","column":"X","kind":"` + kind + `"}`
	if kind == "boolean" {
		leaf = `{"contractVersion":"1","column":"X","kind":"boolean","trueValue":"YES","falseValue":"NO"}`
	}
	return Input{Source: &SourceInput{Mapping: InputMapping{Version: 2, Profile: "base", Case: &CaseMapping{Facts: []FactMapping{{Target: "/value", Source: "/retained", Conversion: json.RawMessage(leaf)}}, Evidence: []EvidenceMapping{{Requirement: "receipt", Source: "/retained", Availability: json.RawMessage(`{"contractVersion":"1","column":"Evidence"}`)}}}}, Case: json.RawMessage(`{"origin":{"originalValues":{"X":` + value + `}},"retained":{"contractVersion":"1","origin":"asserted","values":{"X":` + value + `,"Evidence":"present"},"research":null}}`)}}
}
func columnFixture(i Input) PreparationFixture {
	return PreparationFixture{Dialect: encode(i.Source.Mapping), Case: mustColumnCaseBytes(i.Source), Sources: []byte(`{}`), Profiles: []byte(`[]`), Preparation: encode(i.Preparation)}
}
func columnBundle(i Input) VerificationBundle {
	r := Release{ID: "release-columns", Pack: `{}`, PackDigest: digest([]byte(`{}`)), InputMapping: &i.Source.Mapping}
	return VerificationBundle{Version: 2, Release: r, ReleaseDigest: releaseDigest(r), Run: Run{ReleaseID: r.ID, State: "queued", Input: i}}
}
func TestColumnCaseEntryPreparationAndOfflineVerification(t *testing.T) {
	cases := []struct{ kind, value, facts string }{
		{"text", `"  A  "`, `{"value":"  A  "}`}, {"decimal", `"1.2300"`, `{"value":"1.2300"}`},
		{"number", `"-0"`, `{"value":-0}`}, {"boolean", `"NO"`, `{"value":false}`},
		{"text", `null`, `{}`}, {"text", `" "`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.kind+tc.value, func(t *testing.T) {
			original := columnCase(t, tc.kind, tc.value)
			// This is the host's existing complete source-preparation endpoint. No Runtime,
			// model or acquisition is substituted for an asserted case.
			service := &Service{}
			req := httptest.NewRequest(http.MethodPost, "/v1/inputs/next", bytes.NewReader(encode(original)))
			req.Header.Set("Authorization", "Bearer columns-test")
			response := httptest.NewRecorder()
			service.Handler("columns-test").ServeHTTP(response, req)
			if response.Code != 200 {
				t.Fatalf("actual preparation endpoint: %d %s", response.Code, response.Body.String())
			}
			var plan InputPlan
			if e := json.Unmarshal(response.Body.Bytes(), &plan); e != nil || plan.Input == nil {
				t.Fatalf("no prepared input: %v %s", e, response.Body.String())
			}
			prepared := *plan.Input
			if string(prepared.Facts) != tc.facts || string(prepared.Evidence) != `{"receipt":"present"}` {
				t.Fatalf("wrong actual values: %s %s", prepared.Facts, prepared.Evidence)
			}
			if string(mustColumnCaseBytes(prepared.Source)) != string(mustColumnCaseBytes(original.Source)) {
				t.Fatal("original case/provenance bytes rewritten")
			}
			lineage := prepared.Preparation.Lineage
			if len(lineage) != 2 || lineage[0].ColumnTransformation == nil || lineage[1].ColumnTransformation == nil || lineage[0].Class != "asserted" || lineage[0].GeneratedInfluence {
				t.Fatalf("missing or wrong recomputed lineage: %s", encode(lineage))
			}
			var claim map[string]any
			json.Unmarshal(prepared.Preparation.Outcomes[0].Claim, &claim)
			if len(claim) != 5 || claim["columnTransformation"] != nil {
				t.Fatal("portable derived claim changed")
			}
			f := columnFixture(prepared)
			report := VerifyPreparation(f)
			if report.Result != ResultReading {
				t.Fatalf("offline entry refused: %+v", report)
			}
			b := columnBundle(prepared)
			verified, e := VerifyInputs(encode(b), nil, b.ReleaseDigest)
			if e != nil || !verified.Checked() || verified.Classes.Asserted != 2 {
				t.Fatalf("VerifyInputs: %v %+v", e, verified)
			}
			changed := cloneInput(prepared)
			changed.Preparation.Lineage[0].ColumnTransformation.Input.Ref.FrameSHA256 = "sha256:" + strings.Repeat("0", 64)
			if report := VerifyPreparation(columnFixture(changed)); report.Code != codeDiffers {
				t.Fatalf("retained transformation trusted: %+v", report)
			}
			b = columnBundle(changed)
			if _, e = VerifyInputs(encode(b), nil, b.ReleaseDigest); e == nil {
				t.Fatal("VerifyInputs trusted retained transformation")
			}
			wrong := columnFixture(prepared)
			wrong.Case = bytes.ReplaceAll(wrong.Case, []byte(`"Evidence":"present"`), []byte(`"Evidence":"absent"`))
			if r := VerifyPreparation(wrong); r.Code != codeDiffers {
				t.Fatalf("raw frame not replayed: %+v", r)
			}
		})
	}
}
func TestColumnCaseFailuresProduceNoPreparation(t *testing.T) {
	for _, tc := range []struct{ kind, value string }{{"decimal", `"1e2"`}, {"number", `"1.25"`}, {"number", `"1e3"`}, {"number", `"9007199254740992"`}, {"boolean", `"yes"`}, {"research-value", `"false"`}} {
		t.Run(tc.kind+tc.value, func(t *testing.T) {
			i := columnCase(t, tc.kind, tc.value)
			plan, e := nextInput(i, nil, time.Unix(0, 0))
			if e == nil || plan.Input != nil || !strings.Contains(e.Error(), "read-failed") {
				t.Fatalf("invalid candidate emitted preparation: %v %+v", e, plan)
			}
		})
	}
}
func TestColumnCaseOriginCannotAuthorizeGeneratedInput(t *testing.T) {
	i := columnCase(t, "text", `"X"`)
	i.Source.Case = bytes.ReplaceAll(i.Source.Case, []byte(`"origin":"asserted"`), []byte(`"origin":"generated"`))
	i.Source.Case = bytes.ReplaceAll(i.Source.Case, []byte(`"research":null`), []byte(`"research":{"recordRecipeRevision":"original","declaration":null,"fields":{}}`))
	if _, e := nextInput(i, nil, time.Unix(0, 0)); e == nil || !strings.Contains(e.Error(), "generated frames require verified producer provenance") {
		t.Fatalf("caller origin granted authority: %v", e)
	}
}
func TestColumnSourceRefusedBeforeAcquisition(t *testing.T) {
	i, p, _, at := v2Fixture(t)
	i.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/value", Source: "/retained", Conversion: json.RawMessage(`{"contractVersion":"1","column":"X","kind":"text"}`)}}, Evidence: []EvidenceMapping{}}}
	i.Source.Sources = nil
	if plan, e := nextInput(i, []InputProfile{p}, at); e == nil || plan.Next != nil || !strings.Contains(e.Error(), "explicit Draft base profile") {
		t.Fatalf("unsupported generated/source conversion acquired: %v %+v", e, plan)
	}
}
func TestColumnRulesAreClosedAndChangeIdentity(t *testing.T) {
	good := columnCase(t, "boolean", `"YES"`)
	before := mappingV2Hash(good.Source.Mapping)
	changed := cloneInput(good)
	changed.Source.Mapping.Case.Facts[0].Conversion = bytes.ReplaceAll(changed.Source.Mapping.Case.Facts[0].Conversion, []byte(`"YES"`), []byte(`"Y"`))
	if before == mappingV2Hash(changed.Source.Mapping) {
		t.Fatal("conversion missing from checked identity")
	}
	for _, raw := range []string{`{"target":"/v","source":"/r","Conversion":{"contractVersion":"1","column":"X","kind":"text"}}`, `{"target":"/v","source":"/r","conversion":null}`, `{"target":"/v","source":"/r","conversion":{"contractVersion":"1","column":"X"}}`, `{"target":"/v","source":"/r","conversion":{"contractVersion":"1","column":"X","kind":"text","extra":false}}`} {
		var f FactMapping
		e := strictJSON([]byte(raw), &f)
		if e == nil {
			e = validateColumnCopy(CopyMapping{Facts: []FactMapping{f}})
		}
		if e == nil {
			t.Fatalf("open conversion variant admitted: %s", raw)
		}
	}
	for _, family := range []int{2, 3} {
		m := good.Source.Mapping
		m.Version = family
		raw := encode(m)
		var round InputMapping
		if e := strictJSON(raw, &round); e != nil || round.Profile != "base" || mappingV2Hash(m) != mappingV2Hash(round) {
			t.Fatalf("profile/rule serialization family%d: %v", family, e)
		}
	}
}
func TestColumnBaseLimitsAndUnsupportedProfile(t *testing.T) {
	i := columnCase(t, "text", `"X"`)
	i.Source.Mapping.Profile = "extended"
	if _, e := nextInput(i, nil, time.Unix(0, 0)); e == nil || !strings.Contains(e.Error(), "extended retained-column") {
		t.Fatalf("unsupported Extended executed: %v", e)
	}
	f := columnFixture(i)
	if r := VerifyPreparation(f); r.Result != ResultRefusal || r.Code != "profile-unsupported" {
		t.Fatalf("different offline profile refusal: %+v", r)
	}
	i.Source.Mapping.Profile = "base"
	// Keep the case within its independent custody limit. The complete
	// dialect/case/acquisitions must exceed the aggregate instead of failing
	// while the fixture helper materializes an individually oversized case.
	i.Source.Case = json.RawMessage(`{"pad":"` + strings.Repeat("a", MaxBody-len(`{"pad":""}`)) + `"}`)
	f = columnFixture(i)
	if len(f.Case) != MaxBody || len(f.Dialect)+len(f.Case)+len(f.Sources) <= MaxBody {
		t.Fatal("aggregate fixture does not isolate the complete-artifact limit")
	}
	if _, e := nextInput(i, nil, time.Unix(0, 0)); e == nil || !strings.Contains(e.Error(), "complete dialect, case and acquisitions") {
		t.Fatalf("complete-artifact bound bypassed: %v", e)
	}
	if r := VerifyPreparation(f); r.Code != codeOverBound || !strings.Contains(r.Reason, "together over") {
		t.Fatalf("offline complete-artifact bound bypassed: %+v", r)
	}
}

func TestColumnOrdinaryNumbersRetainBounds(t *testing.T) {
	i := columnCase(t, "text", `"X"`)
	i.Source.Case = append([]byte(`{"unselected":1e309,`), i.Source.Case[1:]...)
	if _, e := nextInput(i, nil, time.Unix(0, 0)); e == nil || !strings.Contains(e.Error(), "numeric exponent exceeds mapping profile limits") {
		t.Fatalf("ordinary numeric artifact limits bypassed: %v", e)
	}
}
