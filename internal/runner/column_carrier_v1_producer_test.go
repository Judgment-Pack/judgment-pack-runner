package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Handwritten, disposable producer fixtures implement the proposed wire grammar,
// not Desk's still-required original-history lookup or production adapter.
const producerRevision = "sha256:07ff6dc41c4c15c56024072f527a8b7f209d86df76dacc8a2f5a6666fb920c90"
const producerValues = `{"Text":"  B  ","Integer":"7","Decimal":"1.2300","Boolean":"false","List":"A","Range":"1","String":"","Number":"-0","Evidence":"present"}`
const producerDeclarations = `[{ "type":"text", "name":"Text", "description":"\u0042" },{"name":"Integer","type":"integer"},{"name":"Decimal","type":"decimal"},{"name":"Boolean","type":"boolean"},{"name":"List","type":"text-list"},{"name":"Range","type":"range"},{"name":"String","type":"string"},{"name":"Number","type":"number"},{"name":"Evidence","type":"text"}]`
const producerFrameDeclarations = `[{"name":"Text","fieldJson":"{\"type\":\"text\",\"name\":\"Text\",\"description\":\"\\u0042\"}"},{"name":"Integer","fieldJson":"{\"name\":\"Integer\",\"type\":\"integer\"}"},{"name":"Decimal","fieldJson":"{\"name\":\"Decimal\",\"type\":\"decimal\"}"},{"name":"Boolean","fieldJson":"{\"name\":\"Boolean\",\"type\":\"boolean\"}"},{"name":"List","fieldJson":"{\"name\":\"List\",\"type\":\"text-list\"}"},{"name":"Range","fieldJson":"{\"name\":\"Range\",\"type\":\"range\"}"},{"name":"String","fieldJson":"{\"name\":\"String\",\"type\":\"string\"}"},{"name":"Number","fieldJson":"{\"name\":\"Number\",\"type\":\"number\"}"},{"name":"Evidence","fieldJson":"{\"name\":\"Evidence\",\"type\":\"text\"}"}]`
const producerTyped = `{"Text":{"status":"supported","value":"  B  "},"Integer":{"status":"supported","value":7},"Decimal":{"status":"supported","value":"1.2300"},"Boolean":{"status":"supported","value":false},"List":{"status":"supported","value":["A","B"]},"Range":{"status":"supported","value":{ "min":-0, "max":2 }},"String":{"status":"supported","value":""},"Number":{"status":"supported","value":-0},"Evidence":{"status":"supported","value":"present"}}`
const producerFrameTyped = `{"Text":{"status":"supported","valueJson":"\"  B  \""},"Integer":{"status":"supported","valueJson":"7"},"Decimal":{"status":"supported","valueJson":"\"1.2300\""},"Boolean":{"status":"supported","valueJson":"false"},"List":{"status":"supported","valueJson":"[\"A\",\"B\"]"},"Range":{"status":"supported","valueJson":"{ \"min\":-0, \"max\":2 }"},"String":{"status":"supported","valueJson":"\"\""},"Number":{"status":"supported","valueJson":"-0"},"Evidence":{"status":"supported","valueJson":"\"present\""}}`
const producerArgs = `{"tool":"read_reviewed_input","arguments":{"grant":"disposable-fixture"}}`

type producerFixture struct {
	input           Input
	profile         InputProfile
	key             ed25519.PrivateKey
	at              time.Time
	snapshot, frame string
}

func newProducerFixture(t *testing.T, kind, column string) producerFixture {
	t.Helper()
	p, key := testProfile(t)
	endpoint := "desk-local:reviewed-inputs"
	p.ID = "reviewed"
	p.Class = "generated"
	p.Source = "desk-inputs"
	p.Endpoint = &endpoint
	p.Tools = []string{"read_reviewed_input"}
	p.Adapter = AdapterPin{Name: "jpack-desk/reviewed-input", Version: "2", Digest: digest([]byte("disposable-producer-v2"))}
	p.RetainedColumnProducer = &RetainedColumnProducer{ContractVersion: "1", Grammar: columnProducerGrammar}
	leaf := `{"contractVersion":"1","column":"` + column + `","kind":"` + kind + `"}`
	if kind == "boolean" {
		leaf = `{"contractVersion":"1","column":"Boolean","kind":"boolean","trueValue":"true","falseValue":"false"}`
	}
	m := InputMapping{Version: 2, Profile: "base", Case: &CaseMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{{Name: "desk", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(producerArgs), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/value", Source: "/retained", Conversion: json.RawMessage(leaf)}}, Evidence: []EvidenceMapping{{Requirement: "receipt", Source: "/retained", Availability: json.RawMessage(`{"contractVersion":"1","column":"Evidence"}`)}}}}}}, Admits: &Admission{Facts: map[string][]string{"/value": {"generated"}}, Evidence: map[string][]string{"receipt": {"generated"}}}}
	snapshot := `{ "version":1,"class":"generated","recipeVersion":8,"values":{"Evidence":"present"},"opaque":{"historical":"\ud800"},"record":{"class":"generated","values":` + producerValues + `,"research":{"runId":"research_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","recipeVersion":7,"entity":{"fields":` + producerTyped + `}}},"research":{"id":"research_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","recipeVersion":7,"recipe":{"fields":` + producerDeclarations + `}} }` + "\n"
	frame := `{ "contractVersion":"1","origin":"generated","values":` + producerValues + `,"research":{"recordRecipeRevision":"` + producerRevision + `","declaration":{"recipeRevision":"` + producerRevision + `","fields":` + producerFrameDeclarations + `},"fields":` + producerFrameTyped + `}}`
	f := producerFixture{input: Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{}`)}}, profile: p, key: key, at: time.Now().UTC().Truncate(time.Second), snapshot: snapshot, frame: frame}
	f.sign(t)
	return f
}
func (f *producerFixture) carrier() []byte {
	return []byte(`{ "contractVersion":"1","originalSnapshotJson":` + string(encode(f.snapshot)) + `,"originalSnapshotSha256":` + string(encode(digest([]byte(f.snapshot)))) + `,"retained":` + f.frame + ` }`)
}
func (f *producerFixture) sign(t *testing.T) {
	t.Helper()
	carrier := f.carrier()
	signed := signResponse(t, f.profile, f.key, []byte(producerArgs), carrier, f.at, 0)
	// Restore the handwritten received representation after the signing helper's
	// JSON writer compacts RawMessage. The canonical receipt digest is unchanged.
	var parts map[string]json.RawMessage
	json.Unmarshal(signed, &parts)
	response := []byte(`{ "result":` + string(carrier) + `, "receipt":` + string(parts["receipt"]) + `, "salts":` + string(parts["salts"]) + ` }`)
	f.input.Source.Sources = map[string]SourceValue{"desk": {Response: response}}
}
func producerHTTP(t *testing.T, input Input, p InputProfile) (int, InputPlan, string) {
	t.Helper()
	body := encode(input)
	if v, ok := input.Source.Sources["desk"]; ok && len(v.Response) > 0 {
		compacted := encode(v.Response)
		body = bytes.Replace(body, compacted, v.Response, 1)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inputs/next", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer producer-test")
	response := httptest.NewRecorder()
	s := &Service{cfg: Config{InputProfiles: []InputProfile{p}}}
	s.Handler("producer-test").ServeHTTP(response, req)
	var plan InputPlan
	json.Unmarshal(response.Body.Bytes(), &plan)
	return response.Code, plan, response.Body.String()
}
func producerPreparationFixture(i Input, p InputProfile) PreparationFixture {
	return PreparationFixture{Dialect: encode(i.Source.Mapping), Case: mustColumnCaseBytes(i.Source), Sources: encode(i.Source.Sources), Profiles: encode([]InputProfile{p}), Preparation: encode(i.Preparation)}
}
func checkProducerOffline(t *testing.T, i Input, p InputProfile) {
	t.Helper()
	if r := VerifyPreparation(producerPreparationFixture(i, p)); r.Result != ResultReading {
		t.Fatalf("offline generated entry: %+v", r)
	}
	b := columnBundle(i)
	b.Release.InputProfiles = []InputProfile{p}
	b.ReleaseDigest = releaseDigest(b.Release)
	v, e := VerifyInputs(encode(b), []InputProfile{p}, b.ReleaseDigest)
	if e != nil || !v.Checked() || v.Classes.Generated != 2 {
		t.Fatalf("VerifyInputs generated: %v %+v", e, v)
	}
}
func TestColumnProducerActualEntriesAllKindsAndOriginalTypes(t *testing.T) {
	cases := []struct{ kind, column, facts, input string }{
		{"text", "Text", `{"value":"  B  "}`, `"  B  "`}, {"decimal", "Decimal", `{"value":"1.2300"}`, `"1.2300"`}, {"number", "Number", `{"value":-0}`, `"-0"`}, {"boolean", "Boolean", `{"value":false}`, `"false"`},
		{"research-value", "Text", `{"value":"  B  "}`, `"  B  "`}, {"research-value", "Integer", `{"value":7}`, `7`}, {"research-value", "Decimal", `{"value":"1.2300"}`, `"1.2300"`}, {"research-value", "Boolean", `{"value":false}`, `false`}, {"research-value", "List", `{"value":["A","B"]}`, `["A","B"]`}, {"research-value", "Range", `{"value":{"max":2,"min":-0}}`, `{ "min":-0, "max":2 }`}, {"research-value", "String", `{"value":""}`, `""`}, {"research-value", "Number", `{"value":-0}`, `-0`},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"-"+tc.column, func(t *testing.T) {
			f := newProducerFixture(t, tc.kind, tc.column)
			noResponse := cloneInput(f.input)
			noResponse.Source.Sources = nil
			code, plan, body := producerHTTP(t, noResponse, f.profile)
			if code != 200 || plan.Next == nil || plan.Next.Profile != f.profile.ID || plan.Next.Source != "desk-inputs" || !sameJSON(plan.Next.Arguments, []byte(producerArgs)) {
				t.Fatalf("actual planned producer: %d %s", code, body)
			}
			code, plan, body = producerHTTP(t, f.input, f.profile)
			if code != 200 || plan.Input == nil {
				t.Fatalf("actual generated preparation: %d %s", code, body)
			}
			prepared := *plan.Input
			if string(prepared.Facts) != tc.facts || string(prepared.Evidence) != `{"receipt":"present"}` {
				t.Fatalf("actual values: %s %s", prepared.Facts, prepared.Evidence)
			}
			line := prepared.Preparation.Lineage[0]
			if line.Class != "generated" || !line.GeneratedInfluence || line.Receipt == nil || line.ColumnTransformation == nil || columnInputJSON(t, prepared, 0) == nil || *columnInputJSON(t, prepared, 0) != tc.input {
				t.Fatalf("generated exact lineage: %s", encode(line))
			}
			identity := line.ColumnTransformation.ResearchIdentity
			if tc.kind == "research-value" && (identity == nil || identity.RecipeRevision != producerRevision) {
				t.Fatalf("missing original revision: %s", encode(line))
			}
			value := prepared.Source.Sources["desk"]
			if len(value.Response) != 0 || value.RetainedResponseJSON != string(f.input.Source.Sources["desk"].Response) {
				t.Fatal("received raw envelope was rewritten")
			}
			var raw map[string]json.RawMessage
			json.Unmarshal([]byte(value.RetainedResponseJSON), &raw)
			carrier, e := readColumnCarrier(raw["result"])
			if e != nil || string(carrier.OriginalSnapshot) != f.snapshot || string(carrier.RawFrame) != f.frame || carrier.Frame.ProjectedFrameSHA256() != digest([]byte(f.frame)) {
				t.Fatalf("raw snapshot/frame identities lost: %v", e)
			}
			checkProducerOffline(t, prepared, f.profile)
			changed := cloneInput(prepared)
			changed.Preparation.Lineage[0].ColumnTransformation.Input.Ref.FrameSHA256 = "sha256:" + strings.Repeat("0", 64)
			if r := VerifyPreparation(producerPreparationFixture(changed, f.profile)); r.Code != codeDiffers {
				t.Fatalf("offline trusted supplied lineage: %+v", r)
			}
			b := columnBundle(changed)
			b.Release.InputProfiles = []InputProfile{f.profile}
			b.ReleaseDigest = releaseDigest(b.Release)
			if _, e := VerifyInputs(encode(b), []InputProfile{f.profile}, b.ReleaseDigest); e == nil {
				t.Fatal("VerifyInputs trusted supplied lineage")
			}
		})
	}
}
func TestColumnProducerAvailabilityAndOmissions(t *testing.T) {
	for _, availability := range []string{"present", "absent", "unknown"} {
		t.Run(availability, func(t *testing.T) {
			f := newProducerFixture(t, "text", "Text")
			f.snapshot = strings.ReplaceAll(f.snapshot, `"Evidence":"present"`, `"Evidence":"`+availability+`"`)
			f.snapshot = strings.ReplaceAll(f.snapshot, `"value":"present"`, `"value":"`+availability+`"`)
			f.frame = strings.ReplaceAll(f.frame, `"Evidence":"present"`, `"Evidence":"`+availability+`"`)
			f.frame = strings.ReplaceAll(f.frame, `"valueJson":"\"present\""`, `"valueJson":"\"`+availability+`\""`)
			f.sign(t)
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code != 200 || plan.Input == nil || string(plan.Input.Evidence) != `{"receipt":"`+availability+`"}` {
				t.Fatalf("availability: %d %s", code, body)
			}
			checkProducerOffline(t, *plan.Input, f.profile)
		})
	}
	for _, status := range []string{"interpretation", "missing", "conflicting", "inaccessible", "stale"} {
		t.Run(status, func(t *testing.T) {
			f := newProducerFixture(t, "research-value", "Text")
			f.snapshot = strings.Replace(f.snapshot, `"Text":{"status":"supported"`, `"Text":{"status":"`+status+`"`, 1)
			f.frame = strings.Replace(f.frame, `"Text":{"status":"supported"`, `"Text":{"status":"`+status+`"`, 1)
			f.sign(t)
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code != 200 || plan.Input == nil {
				t.Fatalf("status omission: %d %s", code, body)
			}
			want := `{}`
			if status == "interpretation" {
				want = `{"value":"  B  "}`
			}
			if string(plan.Input.Facts) != want {
				t.Fatalf("status semantics: %s", plan.Input.Facts)
			}
			checkProducerOffline(t, *plan.Input, f.profile)
		})
	}
}

func TestColumnProducerSignedRelationRefusalsAcrossEntries(t *testing.T) {
	cases := []struct {
		name   string
		change func(*producerFixture)
	}{
		{"extra-frame-value", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"values":{`, `"values":{"Extra":"X",`, 1)
		}},
		{"changed-raw-column", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"values":{"Text":"  B  "`, `"values":{"Text":"changed"`, 1)
		}},
		{"extra-frame-field", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"fields":{"Text":`, `"fields":{"Extra":{"status":"supported","valueJson":"null"},"Text":`, 1)
		}},
		{"original-value-absent", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `"Number":{"status":"supported","value":-0}`, `"Number":{"status":"supported"}`, 1)
		}},
		{"omitted-unselected-declaration", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `,{"name":"Integer","fieldJson":"{\"name\":\"Integer\",\"type\":\"integer\"}"}`, "", 1)
		}},
		{"extra-declaration", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, producerFrameDeclarations, strings.TrimSuffix(producerFrameDeclarations, "]")+`,{"name":"Extra","fieldJson":"{\"name\":\"Extra\",\"type\":\"text\"}"}]`, 1)
		}},
		{"reordered-declarations", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `{"name":"Integer","type":"integer"},{"name":"Decimal","type":"decimal"}`, `{"name":"Decimal","type":"decimal"},{"name":"Integer","type":"integer"}`, 1)
		}},
		{"unselected-field-metadata", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `{"name":"Integer","type":"integer"}`, `{"name":"Integer","type":"integer","description":"changed"}`, 1)
		}},
		{"field-escape-spelling", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `"description":"\u0042"`, `"description":"B"`, 1)
		}},
		{"missing-original-revision", func(f *producerFixture) { f.snapshot = strings.Replace(f.snapshot, `"recipeVersion":7,`, "", 1) }},
		{"current-revision-substitution", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `"recipeVersion":7,`, `"recipeVersion":8,`, 1)
		}},
		{"both-frame-current-revision", func(f *producerFixture) {
			f.frame = strings.ReplaceAll(f.frame, producerRevision, "sha256:"+strings.Repeat("0", 64))
		}},
		{"original-run-substitution", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `"runId":"research_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"runId":"research_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, 1)
		}},
		{"filtered-values-substitution", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, producerValues, `{"Evidence":"present"}`, 1)
		}},
		{"unselected-value-omitted", func(f *producerFixture) { f.frame = strings.Replace(f.frame, `"String":"",`, "", 1) }},
		{"typed-negative-zero", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"Number":{"status":"supported","valueJson":"-0"}`, `"Number":{"status":"supported","valueJson":"0"}`, 1)
		}},
		{"typed-status", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"Text":{"status":"supported"`, `"Text":{"status":"missing"`, 1)
		}},
		{"typed-null-substitution", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"Number":{"status":"supported","valueJson":"-0"}`, `"Number":{"status":"supported","valueJson":"null"}`, 1)
		}},
		{"typed-absence-substitution", func(f *producerFixture) {
			f.frame = strings.Replace(f.frame, `"Number":{"status":"supported","valueJson":"-0"}`, `"Number":{"status":"supported"}`, 1)
		}},
		{"snapshot-decoded-duplicate", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, `"opaque":{"historical":"\ud800"}`, `"opaque":{"historical":"\ud800","\u0068istorical":false}`, 1)
		}},
		{"duplicate-declaration", func(f *producerFixture) {
			f.snapshot = strings.Replace(f.snapshot, producerDeclarations, strings.TrimSuffix(producerDeclarations, "]")+`,{"name":"Integer","type":"integer"}]`, 1)
		}},
		{"invalid-revision-literal", func(f *producerFixture) {
			f.snapshot = strings.ReplaceAll(f.snapshot, `"recipeVersion":7`, `"recipeVersion":7.0`)
		}},
		{"unsafe-revision", func(f *producerFixture) {
			f.snapshot = strings.ReplaceAll(f.snapshot, `"recipeVersion":7`, `"recipeVersion":9007199254740992`)
			f.frame = strings.ReplaceAll(f.frame, producerRevision, "sha256:8870061c2039c232b872e16b131b9e08e11d3868d26f730a46c2082174ce5b47")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProducerFixture(t, "text", "Text")
			code, baseline, body := producerHTTP(t, f.input, f.profile)
			if code != 200 || baseline.Input == nil {
				t.Fatalf("baseline: %d %s", code, body)
			}
			tc.change(&f)
			f.sign(t)
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code == 200 || plan.Input != nil || plan.Next != nil {
				t.Fatalf("signed inconsistent producer emitted input: %d %s", code, body)
			}
			// Same newly signed inconsistent carrier must fail both offline public entries,
			// before it can be compared with the previously prepared values.
			invalid := *baseline.Input
			invalid.Source = f.input.Source
			if r := VerifyPreparation(producerPreparationFixture(invalid, f.profile)); r.Result == ResultReading {
				t.Fatalf("offline accepted signed inconsistent original relation: %+v", r)
			}
			b := columnBundle(invalid)
			b.Release.InputProfiles = []InputProfile{f.profile}
			b.ReleaseDigest = releaseDigest(b.Release)
			if _, e := VerifyInputs(encode(b), []InputProfile{f.profile}, b.ReleaseDigest); e == nil {
				t.Fatal("VerifyInputs accepted signed inconsistent original relation")
			}
		})
	}
}
func TestColumnProducerTrustRefusedBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*producerFixture)
	}{
		{"missing-annotation", func(f *producerFixture) { f.profile.RetainedColumnProducer = nil }},
		{"unsupported-grammar", func(f *producerFixture) { f.profile.RetainedColumnProducer.Grammar = "desk-reviewed-input/other" }},
		{"old-adapter", func(f *producerFixture) { f.profile.Adapter.Version = "1" }},
		{"wrong-source", func(f *producerFixture) { f.profile.Source = "caller-inputs" }},
		{"wrong-class", func(f *producerFixture) {
			f.profile.Class = "record"
			f.input.Source.Mapping.Admits.Facts["/value"] = []string{"generated", "record"}
			f.input.Source.Mapping.Admits.Evidence["receipt"] = []string{"generated", "record"}
		}},
		{"wrong-endpoint", func(f *producerFixture) { endpoint := "elsewhere"; f.profile.Endpoint = &endpoint }},
		{"extra-tool", func(f *producerFixture) { f.profile.Tools = append(f.profile.Tools, "other") }},
		{"missing-draft-selector", func(f *producerFixture) { f.input.Source.Mapping.Profile = "" }},
		{"wrong-source-pointer", func(f *producerFixture) { f.input.Source.Mapping.Sources[0].Read.Copy.Facts[0].Source = "/other" }},
		{"wrong-profile-digest", func(f *producerFixture) {
			f.input.Source.Mapping.Sources[0].ProfileDigest = "sha256:" + strings.Repeat("0", 64)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProducerFixture(t, "text", "Text")
			tc.change(&f)
			if tc.name != "wrong-profile-digest" {
				f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
			}
			f.input.Source.Sources = nil
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code == 200 || plan.Next != nil || plan.Input != nil {
				t.Fatalf("untrusted producer reached acquisition: %d %s", code, body)
			}
		})
	}
}
func TestColumnProducerOriginalReceiptChecksPreserved(t *testing.T) {
	for _, name := range []string{"changed-snapshot-old-receipt", "changed-frame-old-receipt", "wrong-public-key", "wrong-request", "stale", "receipt-source", "receipt-adapter", "receipt-endpoint"} {
		t.Run(name, func(t *testing.T) {
			f := newProducerFixture(t, "text", "Text")
			switch name {
			case "changed-snapshot-old-receipt":
				f.input.Source.Sources["desk"] = SourceValue{Response: bytes.Replace(f.input.Source.Sources["desk"].Response, []byte(`\\ud800`), []byte(`\\ud801`), 1)}
			case "changed-frame-old-receipt":
				f.input.Source.Sources["desk"] = SourceValue{Response: bytes.Replace(f.input.Source.Sources["desk"].Response, []byte(`"values":{"Text":"  B  "`), []byte(`"values":{"Text":"changed"`), 1)}
			case "wrong-public-key":
				other, _ := testProfile(t)
				f.profile.PublicKey = other.PublicKey
				f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
			case "wrong-request":
				f.input.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"tool":"read_reviewed_input","arguments":{"grant":"different"}}`)
			case "stale":
				f.at = f.at.Add(-time.Hour)
				f.sign(t)
			default:
				var response map[string]any
				v, e := readInputJSON(f.input.Source.Sources["desk"].Response)
				if e != nil {
					t.Fatal(e)
				}
				response = v.(map[string]any)
				r := response["receipt"].(map[string]any)
				switch name {
				case "receipt-source":
					r["source"] = "elsewhere"
				case "receipt-adapter":
					r["acquisition"].(map[string]any)["adapter"].(map[string]any)["version"] = "1"
				case "receipt-endpoint":
					r["acquisition"].(map[string]any)["endpoint"] = "elsewhere"
				}
				signReceipt(t, r, f.key)
				f.input.Source.Sources["desk"] = SourceValue{Response: encode(response)}
			}
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code == 200 || plan.Input != nil {
				t.Fatalf("original receipt check bypassed: %d %s", code, body)
			}
		})
	}
}

func TestColumnProducerRawEnvelopeVariantsAndUnsolicitedText(t *testing.T) {
	f := newProducerFixture(t, "research-value", "Number")
	code, plan, body := producerHTTP(t, f.input, f.profile)
	if code != 200 || plan.Input == nil {
		t.Fatalf("baseline: %d %s", code, body)
	}
	baseline := *plan.Input
	for _, name := range []string{"changed-retained-envelope", "both-envelope-forms", "duplicate-retained-envelope", "empty-retained-text", "null-retained-text", "wrong-case-retained-member", "ordinary-source-member"} {
		t.Run(name, func(t *testing.T) {
			input := cloneInput(baseline)
			input.Facts = nil
			input.Evidence = nil
			input.Preparation = nil
			v := input.Source.Sources["desk"]
			switch name {
			case "changed-retained-envelope":
				v.RetainedResponseJSON = strings.Replace(v.RetainedResponseJSON, `"values":{"Text":"  B  "`, `"values":{"Text":"changed"`, 1)
			case "both-envelope-forms":
				v.Response = json.RawMessage(v.RetainedResponseJSON)
			case "duplicate-retained-envelope":
				v.RetainedResponseJSON = strings.Replace(v.RetainedResponseJSON, `{ "result":`, `{ "result":null,"result":`, 1)
			case "ordinary-source-member":
				input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/value", Source: "/originalSnapshotSha256"}}, Evidence: []EvidenceMapping{}}}
			}
			input.Source.Sources["desk"] = v
			if name == "empty-retained-text" || name == "null-retained-text" || name == "wrong-case-retained-member" {
				raw := encode(input)
				if name == "wrong-case-retained-member" {
					raw = bytes.Replace(raw, []byte(`"retainedResponseJson"`), []byte(`"RetainedResponseJson"`), 1)
				} else {
					replace := []byte(`""`)
					if name == "null-retained-text" {
						replace = []byte(`null`)
					}
					raw = bytes.Replace(raw, encode(v.RetainedResponseJSON), replace, 1)
				}
				var decoded Input
				if e := strictJSON(raw, &decoded); e == nil {
					t.Fatal("invalid optional exact-text form accepted")
				}
				return
			}
			status, plan, body := producerHTTP(t, input, f.profile)
			if status == 200 || plan.Input != nil {
				t.Fatalf("ambiguous/unsolicited retained text accepted: %d %s", status, body)
			}
		})
	}
	// Equivalent outer serialization is inspectable local evidence, not a newly
	// authenticated byte identity: the receipt's canonical result domain is equal.
	equivalent := cloneInput(baseline)
	v := equivalent.Source.Sources["desk"]
	v.RetainedResponseJSON = "\n\t" + v.RetainedResponseJSON + "\n"
	equivalent.Source.Sources["desk"] = v
	checkProducerOffline(t, equivalent, f.profile)
}
func TestColumnProducerRawAbsenceNullAndEscapedClasses(t *testing.T) {
	for _, name := range []string{"absent", "null", "escaped-classes"} {
		t.Run(name, func(t *testing.T) {
			f := newProducerFixture(t, "research-value", "Number")
			switch name {
			case "absent":
				f.snapshot = strings.Replace(f.snapshot, `"Number":{"status":"supported","value":-0}`, `"Number":{"status":"missing"}`, 1)
				f.frame = strings.Replace(f.frame, `"Number":{"status":"supported","valueJson":"-0"}`, `"Number":{"status":"missing"}`, 1)
			case "null":
				f.snapshot = strings.Replace(f.snapshot, `"Number":{"status":"supported","value":-0}`, `"Number":{"status":"missing","value":null}`, 1)
				f.frame = strings.Replace(f.frame, `"Number":{"status":"supported","valueJson":"-0"}`, `"Number":{"status":"missing","valueJson":"null"}`, 1)
			case "escaped-classes":
				f.snapshot = strings.ReplaceAll(f.snapshot, `"class":"generated"`, `"class":"\u0067enerated"`)
			}
			f.sign(t)
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code != 200 || plan.Input == nil {
				t.Fatalf("exact optional raw state: %d %s", code, body)
			}
			tr := plan.Input.Preparation.Lineage[0].ColumnTransformation
			if name == "absent" && (tr.Input.State != "missing" || tr.Input.Ref != nil) {
				t.Fatal("absent raw input rewritten")
			}
			if name == "null" && (tr.Input.State != "present" || columnInputJSON(t, *plan.Input, 0) == nil || *columnInputJSON(t, *plan.Input, 0) != "null") {
				t.Fatal("null raw input rewritten")
			}
			checkProducerOffline(t, *plan.Input, f.profile)
		})
	}
}
func TestColumnProducerCompleteRetainedEnvelopeAccounting(t *testing.T) {
	f := newProducerFixture(t, "text", "Text")
	f.snapshot = strings.Replace(f.snapshot, `"opaque":`, `"pad":"`+strings.Repeat(`\"`, 250000)+`","opaque":`, 1)
	f.sign(t)
	before := len(encode(f.input.Source.Mapping)) + len(f.input.Source.Case) + len(encode(f.input.Source.Sources))
	v := f.input.Source.Sources["desk"]
	v.RetainedResponseJSON = string(v.Response)
	both := map[string]SourceValue{"desk": v}
	after := len(encode(f.input.Source.Mapping)) + len(f.input.Source.Case) + len(encode(both))
	t.Logf("complete-envelope witness: originalSnapshot=%d response=%d beforeExactText=%d afterExactText=%d base=%d", len(f.snapshot), len(v.Response), before, after, MaxBody)
	sole := map[string]SourceValue{"desk": {RetainedResponseJSON: string(v.Response)}}
	soleBytes := len(encode(f.input.Source.Mapping)) + len(f.input.Source.Case) + len(encode(sole))
	t.Logf("sole-text layout measurement: originalSnapshot=%d completeEnvelope=%d base=%d", len(f.snapshot), soleBytes, MaxBody)
	max := newProducerFixture(t, "text", "Text")
	padBytes := 524288 - len(max.snapshot) - len(`"pad":"",`)
	max.snapshot = strings.Replace(max.snapshot, `"opaque":`, `"pad":"`+strings.Repeat(`\"`, padBytes/2)+`","opaque":`, 1)
	if len(max.snapshot) < 524288 {
		max.snapshot += " "
	}
	max.sign(t)
	soleMax := map[string]SourceValue{"desk": {RetainedResponseJSON: string(max.input.Source.Sources["desk"].Response)}}
	maxBytes := len(encode(max.input.Source.Mapping)) + len(max.input.Source.Case) + len(encode(soleMax))
	t.Logf("sole-text maximum-snapshot witness: originalSnapshot=%d completeEnvelope=%d base=%d", len(max.snapshot), maxBytes, MaxBody)
	if len(max.snapshot) != 524288 || maxBytes <= MaxBody {
		t.Fatal("sole-text maximum witness does not show remaining bound")
	}

	if len(f.snapshot) > 524288 || before > MaxBody || after <= MaxBody {
		t.Fatal("accounting fixture does not isolate retained text/escaping growth")
	}
	code, plan, body := producerHTTP(t, f.input, f.profile)
	if code != 200 || plan.Input == nil {
		t.Fatalf("sole text did not remove unnecessary duplicate: %d %s", code, body)
	}
	checkProducerOffline(t, *plan.Input, f.profile)
	code, plan, body = producerHTTP(t, max.input, max.profile)
	if code == 200 || plan.Input != nil || !strings.Contains(body, "complete dialect, case and acquisitions exceed base limit") {
		t.Fatalf("sole-text escaped envelope not counted: %d %s", code, body)
	}
	max.input.Source.Sources = soleMax
	if r := VerifyPreparation(producerPreparationFixture(max.input, max.profile)); r.Code != codeOverBound {
		t.Fatalf("offline sole retained bytes not counted: %+v", r)
	}
}

func TestColumnProducerBoundedRevisionIdentity(t *testing.T) {
	for _, tc := range []struct{ version, identity string }{{"1", "sha256:ef9d26d79a6f5f1a147bd95bc610f13406581d98f48a971070513b20b595f6ae"}, {"9007199254740991", "sha256:8612e0274562722e1b37beae400e672c6965f59223c729cce29dda2709e8aff3"}} {
		t.Run(tc.version, func(t *testing.T) {
			f := newProducerFixture(t, "research-value", "Integer")
			f.snapshot = strings.ReplaceAll(f.snapshot, `"recipeVersion":7`, `"recipeVersion":`+tc.version)
			f.frame = strings.ReplaceAll(f.frame, producerRevision, tc.identity)
			f.sign(t)
			code, plan, body := producerHTTP(t, f.input, f.profile)
			if code != 200 || plan.Input == nil {
				t.Fatalf("supported original revision identity: %d %s", code, body)
			}
			identity := plan.Input.Preparation.Lineage[0].ColumnTransformation.ResearchIdentity.RecipeRevision
			if identity != tc.identity || len(identity) != 71 {
				t.Fatalf("incorrect bounded revision identity: %s", identity)
			}
			checkProducerOffline(t, *plan.Input, f.profile)
		})
	}
}

func TestColumnProducerDraftTransportProjectionVectors(t *testing.T) {
	schema, e := os.ReadFile("testdata/retained-column-producer-draft/schema.json")
	if e != nil || !json.Valid(schema) {
		t.Fatalf("Draft projection schema: %v", e)
	}
	raw, e := os.ReadFile("testdata/retained-column-producer-draft/cases.json")
	if e != nil {
		t.Fatal(e)
	}
	var corpus struct {
		Cases []struct {
			Name, Definition string
			Value            json.RawMessage
			Valid            bool
		}
	}
	if e = json.Unmarshal(raw, &corpus); e != nil {
		t.Fatal(e)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			valid := false
			switch c.Definition {
			case "producerAnnotation":
				var pin RetainedColumnProducer
				if strictJSON(c.Value, &pin) == nil {
					f := newProducerFixture(t, "text", "Text")
					f.profile.RetainedColumnProducer = &pin
					valid = columnProducerProfile(f.profile) == nil
				}
			case "retainedPreparationSource":
				var v SourceValue
				valid = strictJSON(c.Value, &v) == nil && v.RetainedResponseJSON != "" && len(v.Response) == 0 && len(v.Snapshot) == 0
			default:
				t.Fatalf("unknown Draft projection definition: %s", c.Definition)
			}
			if valid != c.Valid {
				t.Fatalf("Draft projection parser differs: got %t want %t", valid, c.Valid)
			}
		})
	}
	// Old optional absence retains exact historical profile/source encoding.
	p, _ := testProfile(t)
	if bytes.Contains(encode(p), []byte("retainedColumnProducer")) {
		t.Fatal("legacy profile encoding gained Draft member")
	}
	v := SourceValue{Response: json.RawMessage(`{"result":{},"receipt":{},"salts":{}}`)}
	if bytes.Contains(encode(v), []byte("retainedResponseJson")) {
		t.Fatal("legacy acquisition encoding gained Draft member")
	}
	for _, raw := range []string{`null`, `{"RetainedColumnProducer":{"contractVersion":"1","grammar":"desk-reviewed-input/1"}}`} {
		if raw == "null" {
			var p InputProfile
			if json.Unmarshal([]byte(raw), &p) != nil {
				t.Fatal("legacy null profile decoder behavior changed")
			}
			continue
		}
		var profile InputProfile
		if strictJSON([]byte(raw), &profile) == nil {
			t.Fatal("misspelled new producer annotation accepted")
		}
	}
}
