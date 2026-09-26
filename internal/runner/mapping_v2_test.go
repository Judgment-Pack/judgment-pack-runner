package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testProfile(t *testing.T) (InputProfile, ed25519.PrivateKey) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	endpoint := "postgresql://vendor-records"
	return InputProfile{ID: "vendors", PublicKey: hex.EncodeToString(pub), Class: "record", Source: "postgres/live", Authority: "test", Shape: "mcp", Adapter: AdapterPin{"dbhub", "1.2.3", digest([]byte("adapter-image"))}, Endpoint: &endpoint, Tools: []string{"execute_sql", "lookup"}}, key
}
func canonTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	b, e := proofCanon(raw)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func signResponse(t *testing.T, p InputProfile, key ed25519.PrivateKey, args, result []byte, at time.Time, index int) json.RawMessage {
	t.Helper()
	salt := bytes.Repeat([]byte{byte(index + 1)}, 32)
	previous := any(nil)
	if index > 0 {
		previous = strings.Repeat("a", 128)
	}
	receipt := map[string]any{"receiptVersion": "3", "kind": "acquisition", "sessionId": "mapping.session", "callIndex": index, "prevSignature": previous, "source": p.Source, "resultDigest": digest(canonTest(t, result)), "servedAt": at.Format(time.RFC3339), "authority": p.Authority, "keyId": keyID(p.PublicKey), "argumentsCommitment": digest(append(append(salt, []byte("args:")...), canonTest(t, args)...)), "caller": nil, "acquisition": map[string]any{"shape": p.Shape, "adapter": p.Adapter, "endpoint": p.Endpoint, "statement": nil, "snapshot": "query-123", "peerIdentity": nil, "schema": nil, "upstreamToken": nil, "observedAt": at.Format(time.RFC3339)}}
	signReceipt(t, receipt, key)
	return encode(map[string]any{"receipt": receipt, "result": json.RawMessage(result), "salts": map[string]string{"args": hex.EncodeToString(salt)}})
}
func signReceipt(t *testing.T, r map[string]any, key ed25519.PrivateKey) {
	delete(r, "signature")
	r["signature"] = hex.EncodeToString(ed25519.Sign(key, append([]byte("judgment-pack-gateway/receipt/3:"), canonTest(t, encode(r))...)))
}
func vendorRule() json.RawMessage {
	return json.RawMessage(`{"ruleVersion":"1","parameters":{"vendorId":"integer"},"clauses":[{"when":{"op":"all","of":[{"op":"equalsParam","field":"/id","param":"vendorId"},{"op":"exists","field":"/description"}]},"claim":{"facts":[{"pointer":"/vendor/description","from":"/description"}],"evidence":{"registry":"present"},"acquisitionStatus":"resolved"},"reason":"matching-vendor"},{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"registry":"unknown"},"acquisitionStatus":"unknown"},"reason":"wrong-subject"}]}`)
}
func v2Fixture(t *testing.T) (Input, InputProfile, ed25519.PrivateKey, time.Time) {
	t.Helper()
	p, key := testProfile(t)
	at := time.Now().UTC().Truncate(time.Second)
	m := InputMapping{Version: 2, Case: &CaseMapping{Parameters: map[string]Parameter{"vendorId": {Pointer: "/id", Type: "integer"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}, Sources: []MappingSource{{Name: "vendor", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"execute_sql","arguments":{"sql":{"$text":"SELECT * FROM vendors WHERE id = {{vendorId}}"}}}`), Read: SourceRead{Rule: vendorRule()}}}}
	response := signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":7,"description":"a verified vendor"}`), at, 0)
	return Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"id":7}`), Sources: map[string]SourceValue{"vendor": {Response: response}}}}, p, key, at
}
func cloneInput(i Input) Input { var o Input; _ = json.Unmarshal(encode(i), &o); return o }
func TestV2VerifiedProjectionAndAdversarialMutations(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	good, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if string(good.Facts) != `{"vendor":{"description":"a verified vendor"}}` || string(good.Evidence) != `{"registry":"present"}` || len(good.Preparation.Cites) != 1 || len(good.Preparation.Lineage) != 2 {
		t.Fatal(string(encode(good)))
	}
	again, e := normalizeV2(good, []InputProfile{p}, at)
	if e != nil || !sameJSON(encode(again), encode(good)) {
		t.Fatal("not repeatable", e)
	}
	tests := map[string]func(*Input, *InputProfile){
		"wrong-case":       func(i *Input, p *InputProfile) { i.Source.Case = []byte(`{"id":8}`) },
		"changed-endpoint": func(i *Input, p *InputProfile) { endpoint := "postgresql://other"; p.Endpoint = &endpoint },
		"changed-class":    func(i *Input, p *InputProfile) { p.Class = "generated" },
		"changed-key":      func(i *Input, p *InputProfile) { p.PublicKey = strings.Repeat("a", 64) },
		"changed-adapter":  func(i *Input, p *InputProfile) { p.Adapter.Digest = digest([]byte("changed")) },
		"mapping-digest":   func(i *Input, p *InputProfile) { i.Source.MappingDigest = digest([]byte("bad")) },
		"forged-facts":     func(i *Input, p *InputProfile) { i.Facts = []byte(`{"vendor":{"description":"fabricated"}}`) },
		"forged-evidence":  func(i *Input, p *InputProfile) { i.Evidence = []byte(`{"registry":"absent"}`) },
		"extra-response":   func(i *Input, p *InputProfile) { i.Source.Sources["extra"] = i.Source.Sources["vendor"] },
		"absent-response":  func(i *Input, p *InputProfile) { delete(i.Source.Sources, "vendor") },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			profile := p
			change(&i, &profile)
			if _, e := normalizeV2(i, []InputProfile{profile}, at); e == nil {
				t.Fatal("accepted mutation")
			}
		})
	}
	proofMutations := map[string]func(map[string]any){
		"result":               func(r map[string]any) { r["result"] = map[string]any{"id": 7, "description": "forged"} },
		"salt":                 func(r map[string]any) { r["salts"].(map[string]any)["args"] = strings.Repeat("b", 64) },
		"unsigned-added-field": func(r map[string]any) { r["receipt"].(map[string]any)["extra"] = "injected" },
		"signed-missing-schema": func(r map[string]any) {
			receipt := r["receipt"].(map[string]any)
			delete(receipt["acquisition"].(map[string]any), "schema")
			signReceipt(t, receipt, key)
		},
		"signed-action": func(r map[string]any) {
			receipt := r["receipt"].(map[string]any)
			receipt["kind"] = "action"
			signReceipt(t, receipt, key)
		},
		"signed-stale": func(r map[string]any) {
			receipt := r["receipt"].(map[string]any)
			receipt["acquisition"].(map[string]any)["observedAt"] = at.Add(-301 * time.Second).Format(time.RFC3339)
			signReceipt(t, receipt, key)
		},
		"signed-future": func(r map[string]any) {
			receipt := r["receipt"].(map[string]any)
			receipt["acquisition"].(map[string]any)["observedAt"] = at.Add(31 * time.Second).Format(time.RFC3339)
			signReceipt(t, receipt, key)
		},
	}
	for name, change := range proofMutations {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			v, _ := readInputJSON(i.Source.Sources["vendor"].Response)
			r := v.(map[string]any)
			change(r)
			i.Source.Sources["vendor"] = SourceValue{Response: encode(r)}
			if _, e := normalizeV2(i, []InputProfile{p}, at); e == nil {
				t.Fatal("accepted altered proof")
			}
		})
	}
}
func TestV2WrongSubjectCannotFeedChainedRequest(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: map[string]Parameter{"description": {From: "vendor", Pointer: "/vendor/description", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"description":{"$param":"description"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/detail", "/detail"}}, Evidence: []EvidenceMapping{}}}})
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":8,"description":"wrong vendor raw data"}`), at, 0)}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	if string(got.Facts) != `{}` || got.Preparation.Outcomes[2].Reason != "dependency-unavailable" {
		t.Fatal(string(encode(got)))
	}
	input.Source.Sources["detail"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"lookup","arguments":{"description":"wrong vendor raw data"}}`), []byte(`{"detail":"fabricated"}`), at, 1)}
	if _, e = normalizeV2(input, []InputProfile{p}, at); e == nil {
		t.Fatal("raw rejected value reached dependent operation")
	}
}
func TestV2GeneratedAdmissionAndPropagation(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	p.Class = "generated"
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p)
	if _, e := normalizeV2(input, []InputProfile{p}, at); e == nil {
		t.Fatal("generated output admitted by default")
	}
	input.Source.Mapping.Admits = &Admission{Facts: map[string][]string{"/vendor/description": {"generated"}}, Evidence: map[string][]string{"registry": {"generated"}}}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil || !got.Preparation.Lineage[0].GeneratedInfluence {
		t.Fatal(e)
	}
	second, _ := testProfile(t)
	second.PublicKey = p.PublicKey
	second.ID = "second"
	second.Source = "second/live"
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: second.ID, ProfileDigest: profileHash(second), MaxAge: 300, Parameters: map[string]Parameter{"description": {From: "vendor", Pointer: "/vendor/description", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"description":{"$param":"description"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/detail", "/detail"}}, Evidence: []EvidenceMapping{}}}})
	input.Source.Sources["detail"] = SourceValue{Response: signResponse(t, second, key, []byte(`{"tool":"lookup","arguments":{"description":"a verified vendor"}}`), []byte(`{"detail":"record chosen by model output"}`), at, 1)}
	if _, e = normalizeV2(input, []InputProfile{p, second}, at); e == nil {
		t.Fatal("generated influence laundered through record")
	}
	input.Source.Mapping.Admits.Facts["/detail"] = []string{"record", "generated"}
	if _, e = normalizeV2(input, []InputProfile{p, second}, at); e != nil {
		t.Fatal(e)
	}
}
func TestV2StrictRulesTemplatesAndNumericDomain(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	for name, change := range map[string]func(*Input){
		"string-splice": func(i *Input) {
			i.Source.Mapping.Case.Parameters["vendorId"] = Parameter{Pointer: "/id", Type: "string"}
		},
		"dynamic-tool": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = []byte(`{"tool":{"$param":"vendorId"},"arguments":{}}`)
		},
		"whole-sql-parameter": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = []byte(`{"tool":"execute_sql","arguments":{"sql":{"$param":"vendorId"}}}`)
		},
		"run-time-in-request": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = []byte(`{"tool":"execute_sql","arguments":{"at":{"$param":"runAt"}}}`)
		},
		"forward-dependency": func(i *Input) {
			i.Source.Mapping.Sources[0].Parameters = map[string]Parameter{"later": {From: "later", Pointer: "/id", Type: "integer"}}
		},
		"unknown-op": func(i *Input) {
			i.Source.Mapping.Sources[0].Read.Rule = bytes.Replace(vendorRule(), []byte(`"always"`), []byte(`"typo"`), 1)
		},
		"unknown-rule-field": func(i *Input) {
			i.Source.Mapping.Sources[0].Read.Rule = append([]byte(`{"extra":1,`), vendorRule()[1:]...)
		},
		"overlap":          func(i *Input) { i.Source.Mapping.Case.Facts = []FactMapping{{"/vendor", "/id"}} },
		"reserved-param":   func(i *Input) { i.Source.Mapping.Case.Parameters["runAt"] = Parameter{Pointer: "/id", Type: "integer"} },
		"fractional-param": func(i *Input) { i.Source.Case = []byte(`{"id":7.0}`) },
	} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			change(&i)
			if _, e := normalizeV2(i, []InputProfile{p}, at); e == nil {
				t.Fatal("invalid mapping accepted")
			}
		})
	}
	local := fileInput(`{"amount":1.25,"zero":0,"no":false,"nil":null}`, InputMapping{Version: 1, Provider: "local-file", Facts: []FactMapping{{"/amount", "/amount"}}, Evidence: []EvidenceMapping{}})
	if _, e := normalizeInput(local); e != nil {
		t.Fatal("v1 decimal compatibility", e)
	}
	m := InputMapping{Version: 2, Sources: []MappingSource{{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &CopyMapping{local.Source.Mapping.Facts, []EvidenceMapping{}}}}}}
	v2 := Input{Source: &SourceInput{Mapping: m, Sources: map[string]SourceValue{"file": {Snapshot: local.Source.Snapshot}}}}
	if _, e := normalizeV2(v2, nil, at); e == nil {
		t.Fatal("silently accepted fractional v2 claim")
	}
	v2.Source.Mapping.Sources[0].Read.Copy.Facts = []FactMapping{{"/zero", "/zero"}, {"/no", "/no"}, {"/nil", "/nil"}, {"/missing", "/missing"}}
	got, e := normalizeV2(v2, nil, at)
	if e != nil || string(got.Facts) != `{"nil":null,"no":false,"zero":0}` {
		t.Fatal(string(got.Facts), e)
	}
}

func TestV2RealRuntimeReleaseRunOfflineAndRestart(t *testing.T) {
	cfg := testConfig(t)
	input, p, key, at := v2Fixture(t)
	cfg.InputProfiles = []InputProfile{p}
	// The exact real pack consumes one projected object and two evidence requirements.
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
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
	if len(release.InputProfiles) != 1 || len(release.MappingWarnings) == 0 {
		t.Fatal("release trust/coverage absent")
	}
	job, e := s.createJob("Verified vendor", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, replayed, e := s.submit(job.ID, "v2-1", input)
	if e != nil || replayed {
		t.Fatal(e)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" {
		t.Fatal(done.Problem)
	}
	if !bytes.Contains(done.Audit, []byte(`"cites"`)) {
		t.Fatal("missing actual audit citations")
	}
	bundle := VerificationBundle{2, releaseDigest(release), release, done}
	raw := encode(bundle)
	if dir := os.Getenv("JPACK_V2_TEST_EXPORT_DIR"); dir != "" {
		for name, data := range map[string][]byte{"run.json": raw, "profiles.json": encode([]InputProfile{p}), "release-digest.txt": []byte(bundle.ReleaseDigest)} {
			if e = os.WriteFile(dir+"/"+name, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e = VerifyRun(raw, []InputProfile{p}, bundle.ReleaseDigest); e != nil {
		t.Fatal("offline verification", e)
	}
	if e = VerifyRun(raw, nil, bundle.ReleaseDigest); e == nil {
		t.Fatal("trusted keys inferred from export")
	}
	if e = VerifyRun(raw, []InputProfile{p}, digest([]byte("different release"))); e == nil {
		t.Fatal("release identity not checked")
	}
	bundle.Run.Input.Preparation.Lineage[0].Present = false
	if e = VerifyRun(encode(bundle), []InputProfile{p}, bundle.ReleaseDigest); e == nil {
		t.Fatal("altered lineage accepted")
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	again, replayed, e := s.submit(job.ID, "v2-1", input)
	if e != nil || !replayed || again.ID != run.ID {
		t.Fatal("replay after restart", e)
	}
	req := httptest.NewRequest("GET", "/v1/runs/"+run.ID+"/verification", nil)
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	s.Handler("test").ServeHTTP(w, req)
	if w.Code != 200 || VerifyRun(w.Body.Bytes(), cfg.InputProfiles, bundle.ReleaseDigest) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	brief := encode(briefInput(done.Input))
	if bytes.Contains(brief, []byte(`"salts"`)) || bytes.Contains(brief, []byte(`"response"`)) {
		t.Fatal("proof leaked into brief")
	}
}

func TestV2DriveVerifiesSelectedBytesAndRequest(t *testing.T) {
	p, key := testProfile(t)
	p.ID = "drive"
	p.Source = "drive"
	p.Shape = "http"
	p.Tools = nil
	at := time.Now().UTC().Truncate(time.Second)
	selected := fileInput(`{"approved":false}`, InputMapping{Version: 1, Provider: "local-file"})
	var snap map[string]any
	_ = json.Unmarshal(selected.Source.Snapshot, &snap)
	delete(snap, "selectedAt")
	original := snap["original"].(map[string]any)
	grant := strings.Repeat("b", 64)
	args := encode(map[string]string{"fileId": "file-A", "grant": grant})
	result := encode(map[string]any{"document": map[string]any{"id": original["sha256"], "name": original["name"], "mediaType": original["mediaType"], "size": len(`{"approved":false}`)}, "original": map[string]any{"bytes": original["bytes"]}, "provenance": map[string]any{"observedAt": at.Format(time.RFC3339), "source": map[string]string{"kind": "google-drive", "fileId": "file-A", "version": "7"}}})
	response := signResponse(t, p, key, args, result, at, 0)
	snap["proof"] = map[string]any{"session": "mapping.session", "source": "drive", "authority": p.Authority, "publicKey": p.PublicKey, "response": string(response), "registry": "", "drive": map[string]string{"fileId": "file-A", "grant": grant}}
	mapping := InputMapping{Version: 2,
		Case: &CaseMapping{Parameters: map[string]Parameter{"fileId": {Pointer: "/fileId", Type: "string"}, "grant": {Pointer: "/grant", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{{Name: "file", Kind: "selected-file", Provider: "google-drive", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300,
			Arguments: json.RawMessage(`{"fileId":{"$param":"fileId"},"grant":{"$param":"grant"}}`),
			Read:      SourceRead{Copy: &CopyMapping{[]FactMapping{{"/approved", "/approved"}}, []EvidenceMapping{}}},
		}},
	}
	input := Input{Source: &SourceInput{Mapping: mapping, Case: args, Sources: map[string]SourceValue{"file": {Snapshot: encode(snap)}}}}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil || string(got.Facts) != `{"approved":false}` {
		t.Fatal(e, string(got.Facts))
	}
	input.Source.Case = encode(map[string]string{"fileId": "file-B", "grant": grant})
	if _, e = normalizeV2(input, []InputProfile{p}, at); e == nil {
		t.Fatal("uncommitted selected file accepted")
	}
	input.Source.Case = args
	bad := cloneInput(input)
	v, _ := readInputJSON(bad.Source.Sources["file"].Snapshot)
	s := v.(map[string]any)
	s["proof"].(map[string]any)["publicKey"] = strings.Repeat("c", 64)
	bad.Source.Sources["file"] = SourceValue{Snapshot: encode(s)}
	if _, e = normalizeV2(bad, []InputProfile{p}, at); e == nil {
		t.Fatal("submitted key trusted")
	}
}
func TestV2DerivationErrorsAreNotUnknownAndUnwrapIsBounded(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.Sources[0].Read.Unwrap = []string{"/content/0/text"}
	args := []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`)
	for _, artifact := range []string{`{"id":7,"description":1.25}`, `{"id":7,"description":1e99999999}`, `{"id":7,"description":9007199254740992}`, `{"id":7,"description":"\ud800"}`, `{"id":7,"id":8,"description":"ambiguous"}`, `{"id":7`} {
		input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, args, encode(map[string]any{"content": []any{map[string]string{"text": artifact}}}), at, 0)}
		if _, e := normalizeV2(input, []InputProfile{p}, at); e == nil {
			t.Fatalf("derivation failure became policy data: %s", artifact)
		}
	}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, args, encode(map[string]any{"content": []any{map[string]string{"text": `{"id":8,"description":"wrong subject"}`}}}), at, 0)}
	if got, e := normalizeV2(input, []InputProfile{p}, at); e != nil || string(got.Evidence) != `{"registry":"unknown"}` {
		t.Fatal("legitimate unknown refused", e)
	}
}
func TestV2CoverageAndRequestParsing(t *testing.T) {
	input, _, _, _ := v2Fixture(t)
	m := input.Source.Mapping
	if _, e := mappingCoverage(m, []string{"/vendor/description"}, []string{"registry"}); e != nil {
		t.Fatal(e)
	}
	if _, e := mappingCoverage(m, []string{"/vendor/description", "/unmapped"}, []string{"registry"}); e == nil {
		t.Fatal("unacknowledged missing producer")
	}
	m.Unmapped = []string{"/unmapped"}
	if _, e := mappingCoverage(m, []string{"/vendor/description", "/unmapped"}, []string{"registry"}); e != nil {
		t.Fatal(e)
	}
	if _, e := mappingCoverage(m, []string{""}, []string{"registry", "other"}); e == nil {
		t.Fatal("unacknowledged evidence")
	}
	for _, raw := range []string{`{"facts":{},"facts":{}}`, `{"source":{"mapping":{"version":2,"extra":true}}}`, `{"facts":{"name":"\ud800"}}`} {
		req := httptest.NewRequest("POST", "/v1/inputs/preview", strings.NewReader(raw))
		w := httptest.NewRecorder()
		var i Input
		if e := decode(w, req, &i); e == nil {
			t.Fatal("ambiguous request accepted", raw)
		}
	}
}

func TestV1MappingDigestRemainsByteCompatible(t *testing.T) {
	mapping := fileMapping()
	legacy := []byte(`{"version":1,"provider":"local-file","facts":[{"target":"/request","source":"/facts/request"}],"evidence":[{"requirement":"intake-form","source":"/evidence/intake-form"},{"requirement":"sponsor-endorsement","source":"/evidence/sponsor-endorsement"}]}`)
	if mappingHash(mapping) != digest(legacy) || !bytes.Equal(encode(mapping), legacy) {
		t.Fatal("historical v1 mapping digest changed")
	}
}

func TestV2RealRuntimeWithoutExternalCitations(t *testing.T) {
	for _, kind := range []string{"case", "local-file"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			copy := CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}
			input := Input{Source: &SourceInput{Mapping: InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}}}}
			if kind == "case" {
				input.Source.Case = encode(sample())
				input.Source.Mapping.Case = &CaseMapping{Facts: copy.Facts, Evidence: copy.Evidence}
			} else {
				local := fileInput(string(encode(sample())), InputMapping{Version: 1, Provider: "local-file"})
				input.Source.Mapping.Sources = []MappingSource{{Name: "file", Kind: "selected-file", Provider: "local-file", Read: SourceRead{Copy: &copy}}}
				input.Source.Sources = map[string]SourceValue{"file": {Snapshot: local.Source.Snapshot}}
			}
			pack, err := os.ReadFile("testdata/triage.pack.json")
			if err != nil {
				t.Fatal(err)
			}
			release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.createJob("No citations", release.ID)
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := s.submit(job.ID, "no-cites", input)
			if err != nil {
				t.Fatal(err)
			}
			done := waitRun(t, s, run.ID)
			if done.State != "completed" || len(done.Audit) == 0 {
				t.Fatal(done.State, done.Problem)
			}
			bundle := VerificationBundle{2, releaseDigest(release), release, done}
			if err = VerifyRun(encode(bundle), nil, bundle.ReleaseDigest); err != nil {
				t.Fatal(err)
			}
		})
	}
	if auditCitesMatch(nil, []Citation{{SessionID: "session", CallIndex: 0, Signature: "expected"}}) {
		t.Fatal("missing external citation accepted")
	}
	if auditCitesMatch([]byte(`null`), []Citation{}) {
		t.Fatal("null is not Runtime's omitted or empty citation array")
	}
}
