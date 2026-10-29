package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func selectedContract() *ConversionContract {
	return &ConversionContract{Contract: "retained-column/1", Spec: installedConversionSpec, Profile: "base", Roles: append([]string(nil), conversionRoles[:]...)}
}
func selectedHeader(c *ConversionContract) string {
	return base64.RawURLEncoding.EncodeToString(encode(c))
}

type unreadBody struct{ reads int }

func (b *unreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*unreadBody) Close() error               { return nil }
func scopedEntry(t *testing.T, i Input, p []InputProfile, raw []byte) (int, InputPlan, string) {
	t.Helper()
	if raw == nil {
		raw = encode(i)
		if i.Source != nil {
			if len(i.Source.Case) > 0 {
				raw = bytes.Replace(raw, encode(i.Source.Case), i.Source.Case, 1)
			}
			if v, ok := i.Source.Sources["desk"]; ok && len(v.Response) > 0 {
				raw = bytes.Replace(raw, encode(v.Response), v.Response, 1)
			}
		}
	}
	req := httptest.NewRequest("POST", "/v1/retained-column/1/inputs/next", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer role-test")
	req.Header.Set("JPack-Conversion-Contract", selectedHeader(inputConversion(i)))
	w := httptest.NewRecorder()
	(&Service{cfg: Config{InputProfiles: p}}).Handler("role-test").ServeHTTP(w, req)
	var plan InputPlan
	json.Unmarshal(w.Body.Bytes(), &plan)
	return w.Code, plan, w.Body.String()
}
func selectedProducer(t *testing.T) producerFixture {
	t.Helper()
	f := newProducerFixture(t, "research-value", "Range")
	f.input.Source.ConversionContract = selectedContract()
	f.profile.ConversionContracts = ConversionCapabilities("producer-carrier")
	f.profile.ConversionContracts[0].ProducerGrammar = columnProducerGrammar
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	f.sign(t)
	return f
}
func TestColumnRoleSelectorRefusesBeforeBody(t *testing.T) {
	valid := selectedContract()
	validJSON := string(encode(valid))
	bad := []string{"", selectedHeader(valid) + "=", base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(validJSON, `"base"`, `"extended"`, 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(validJSON, `"roles":[`, `"roles":["preview",`, 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(validJSON, `"contract":`, `"unknown":1,"contract":`, 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(validJSON, valid.Spec.Revision, strings.Repeat("0", 40), 1))), base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(validJSON, `"preparation",`, "", 1)))}
	for j, h := range bad {
		b := &unreadBody{}
		req := httptest.NewRequest("POST", "/v1/retained-column/1/inputs/next", b)
		req.Header.Set("Authorization", "Bearer role-test")
		if h != "" {
			req.Header.Set("JPack-Conversion-Contract", h)
		}
		w := httptest.NewRecorder()
		(&Service{}).Handler("role-test").ServeHTTP(w, req)
		if w.Code < 400 || b.reads != 0 {
			t.Fatalf("selector %d admitted or read body: status=%d reads=%d", j, w.Code, b.reads)
		}
	}
	req := httptest.NewRequest("POST", "/v1/retained-column/1/inputs/next", &unreadBody{})
	req.Header.Set("JPack-Conversion-Contract", selectedHeader(valid))
	w := httptest.NewRecorder()
	(&Service{}).Handler("role-test").ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("selected route bypassed authentication")
	}
}
func TestColumnRoleCapabilityClosureAndIndependentProducer(t *testing.T) {
	c := ConversionCapabilities("producer-carrier")[0]
	c.ProducerGrammar = columnProducerGrammar
	for _, mutate := range []func(map[string]any){func(m map[string]any) { m["extra"] = true }, func(m map[string]any) { delete(m, "limits") }, func(m map[string]any) { m["limits"].(map[string]any)["carrierBytes"] = 3.0 }, func(m map[string]any) { m["role"] = "unknown" }, func(m map[string]any) { m["producerGrammar"] = "unknown" }} {
		var m map[string]any
		json.Unmarshal(encode(c), &m)
		mutate(m)
		raw := encode(m)
		if bytes.Contains(raw, []byte(`"carrierBytes":3`)) {
			raw = bytes.Replace(raw, []byte(`"carrierBytes":3`), []byte(`"carrierBytes":3.0`), 1)
		}
		var held ConversionCapability
		if strictJSON(raw, &held) == nil {
			t.Fatalf("malformed capability accepted: %s", raw)
		}
	}
	f := selectedProducer(t)
	noResponse := cloneInput(f.input)
	noResponse.Source.Sources = nil
	oldProfile := f.profile
	oldProfile.ConversionContracts = nil
	noResponse.Source.Mapping.Sources[0].ProfileDigest = profileHash(oldProfile)
	if code, _, body := scopedEntry(t, noResponse, []InputProfile{oldProfile}, nil); code < 400 {
		t.Fatalf("missing producer role planned acquisition: %s", body)
	}
	noResponse.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	if code, plan, body := scopedEntry(t, noResponse, []InputProfile{f.profile}, nil); code != 200 || plan.Next == nil || !sameConversion(plan.Next.ConversionContract, selectedContract()) {
		t.Fatalf("independently pinned producer plan: %d %s", code, body)
	}
	ordinary := newProducerFixture(t, "text", "Text")
	if code, _, body := producerHTTP(t, ordinary.input, ordinary.profile); code != 200 {
		t.Fatalf("ordinary producer changed: %d %s", code, body)
	}
}
func TestColumnRoleExactHTTPStoreOfflineCustody(t *testing.T) {
	f := selectedProducer(t)
	// Coupled original inventory/revision, raw range -0, opaque provenance and
	// maximum original snapshot, preserving the producer relation in one input.
	pad := 524288 - len(f.snapshot) - len(`"pad":"",`)
	f.snapshot = strings.Replace(f.snapshot, `"opaque":`, `"pad":"`+strings.Repeat(`\"`, pad/2)+`","opaque":`, 1)
	if len(f.snapshot) < 524288 {
		f.snapshot += " "
	}
	f.sign(t)
	if len(f.snapshot) != 524288 {
		t.Fatal("handwritten snapshot boundary is wrong")
	}
	code, plan, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil)
	if code != 200 || plan.Input == nil {
		t.Fatalf("selected actual HTTP preparation: %d %s", code, body)
	}
	i := *plan.Input
	if !sameConversion(i.Preparation.ConversionContract, inputConversion(i)) || i.Source.Sources["desk"].RetainedResponseJSON != string(f.input.Source.Sources["desk"].Response) || !bytes.Contains(i.Facts, []byte(`"min":-0`)) {
		t.Fatal("selected preparation lost identity/raw custody/numeric spelling")
	}
	if len(encode(i.Source)) <= MaxBody {
		t.Fatal("maximum snapshot did not exercise expanded complete source")
	}
	r := Release{ID: "role-release", Pack: `{}`, PackDigest: digest([]byte(`{}`)), InputMapping: &i.Source.Mapping, InputProfiles: []InputProfile{f.profile}, Sample: i}
	run := Run{ID: "role-run", JobID: "role-job", ReleaseID: r.ID, State: "queued", Input: i}
	store := newMemoryStore()
	if e := store.update(func(tx runTx) error {
		if e := tx.createRelease(r); e != nil {
			return e
		}
		return tx.createRun(run, "owner", "role-key", "request")
	}); e != nil {
		t.Fatal(e)
	}
	restored, e := store.run(run.ID)
	if e != nil || !sameConversion(inputConversion(restored.Input), inputConversion(i)) || restored.Input.Source.Sources["desk"].RetainedResponseJSON != i.Source.Sources["desk"].RetainedResponseJSON {
		t.Fatal("durable serialized custody changed", e)
	}
	fixture := producerPreparationFixture(restored.Input, f.profile)
	fixture.ConversionContract = selectedContract()
	if report := VerifyPreparation(fixture); report.Result != ResultReading {
		t.Fatalf("selected offline preparation: %+v", report)
	}
	b := VerificationBundle{Version: 2, Release: r, ReleaseDigest: releaseDigest(r), Run: restored}
	v, e := VerifyInputs(encode(b), []InputProfile{f.profile}, b.ReleaseDigest, selectedContract())
	if e != nil || !v.Checked() {
		t.Fatalf("selected offline export: %v", e)
	}
	if _, e := VerifyInputs(encode(b), []InputProfile{f.profile}, b.ReleaseDigest); e == nil {
		t.Fatal("offline export inferred selection from body")
	}
	changed := cloneInput(restored.Input)
	changed.Source.ConversionContract.Roles = changed.Source.ConversionContract.Roles[:3]
	if mappingMatchesRelease(r, changed) {
		t.Fatal("release run selection mismatch accepted")
	}
	f.snapshot += " "
	f.sign(t)
	if code, _, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil); code < 400 {
		t.Fatalf("one-over snapshot accepted: %s", body)
	}
}
func TestColumnRoleBodyAndFrameBoundaries(t *testing.T) {
	i := columnCase(t, "text", `"A"`)
	i.Source.ConversionContract = selectedContract()
	raw := encode(i)
	exact := append(bytes.Repeat([]byte(" "), 16777216-len(raw)), raw...)
	if code, _, body := scopedEntry(t, i, nil, exact); code != 200 {
		t.Fatalf("exact selected HTTP body: %d %s", code, body)
	}
	if code, _, _ := scopedEntry(t, i, nil, append([]byte(" "), exact...)); code != 400 {
		t.Fatal("one-over selected HTTP body accepted")
	}
	frame := `{"contractVersion":"1","origin":"asserted","values":{"X":"A","Evidence":"present"},"research":null}`
	atFrame := "{" + strings.Repeat(" ", 2097152-len(frame)) + frame[1:]
	i.Source.Case = []byte(`{"retained":` + atFrame + `}`)
	if code, _, body := scopedEntry(t, i, nil, nil); code != 200 {
		t.Fatalf("exact selected frame: %d %s", code, body)
	}
	i.Source.Case = []byte(`{"retained":{ ` + atFrame[1:] + `}`)
	if _, e := normalizeV2(i, nil, time.Now()); e == nil {
		t.Fatal("one-over frame accepted")
	}
}

func TestColumnRoleScopedCanonDoesNotRelaxClaims(t *testing.T) {
	for _, raw := range []string{`{"n":1.25}`, `{"n":9007199254740992}`, `{"n":1e309}`} {
		if _, e := scopedJSON([]byte(raw), 12582912); e == nil {
			t.Fatalf("selected canonical member accepted unsupported number: %s", raw)
		}
		if _, e := proofCanon([]byte(raw)); e == nil {
			t.Fatal("ordinary canonical domain changed")
		}
	}
	if _, e := scopedJSON([]byte(`{"n":-0,"text":"1.25"}`), 12582912); e != nil {
		t.Fatal("supported selected canonical member refused", e)
	}
}
func TestColumnRoleSelectedSourceExpiryClearsCustody(t *testing.T) {
	s, req, barrier, _ := workerFixture(t)
	req.ConversionContract = selectedContract()
	req.ConversionLimits = nil
	req.RetainedColumn = false
	req.Deadline = time.Now().Add(200 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	ready, retain := make(chan struct{}), make(chan struct{})
	s.beforeRetain = func() { close(ready); <-retain }
	if _, _, e := s.submit(req); e != nil {
		t.Fatal(e)
	}
	close(barrier)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("selected completion did not reach retain barrier")
	}
	time.Sleep(250 * time.Millisecond)
	close(retain)
	s.workers.Wait()
	status, _, e := s.submit(req)
	if e != nil || status.State != "expired" || status.RetainedResponseJSON != "" || len(status.Response) > 0 {
		t.Fatal("expired selected response custody survived", e, status.State, len(status.RetainedResponseJSON))
	}
}

func TestColumnRoleHandwrittenSelectionVectors(t *testing.T) {
	raw, e := os.ReadFile("../../vectors/retained-column-selection-v0.3.0.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Status       string                 `json:"status"`
		Selection    ConversionContract     `json:"selection"`
		Capabilities []ConversionCapability `json:"capabilities"`
		Negative     []struct {
			Name, Member, Code string
			Value              json.RawMessage
		} `json:"negativeSelections"`
		Conversions  []json.RawMessage `json:"conversions"`
		Availability json.RawMessage   `json:"availability"`
		Lineages     []json.RawMessage `json:"lineages"`
		SourceInputs []json.RawMessage `json:"sourceInputs"`
		Preparations []json.RawMessage `json:"preparations"`
		Profiles     []json.RawMessage `json:"profiles"`
		SourceValues []json.RawMessage `json:"sourceValues"`
		CopyMappings []json.RawMessage `json:"copyMappings"`
	}
	if e := strictJSON(raw, &f); e != nil {
		t.Fatal(e)
	}
	if f.Status != "handwritten-test-only" || f.Selection.Spec != installedConversionSpec {
		t.Fatal("external handwritten installed pin differs")
	}
	if len(f.Capabilities) != 5 {
		t.Fatal("missing role witness")
	}
	for _, entry := range f.Capabilities {
		if e := requireConversionCapability([]ConversionCapability{entry}, entry.Role, entry.Role == "producer-carrier"); e != nil {
			t.Fatal(entry.Role, e)
		}
	}
	for _, raw := range f.Conversions {
		if _, e := columnPlan(raw, false); e != nil {
			t.Fatal("handwritten conversion", e)
		}
	}
	if _, e := columnPlan(f.Availability, true); e != nil {
		t.Fatal("handwritten availability", e)
	}
	for _, raw := range f.Lineages {
		var transformation retainedcolumns.Transformation
		if e := strictJSON(raw, &transformation); e != nil {
			t.Fatal("handwritten lineage", e)
		}
	}
	for _, raw := range f.CopyMappings {
		var mapping CopyMapping
		if e := strictJSON(raw, &mapping); e != nil {
			t.Fatal("handwritten copy mapping", e)
		}
	}
	for _, raw := range f.SourceInputs {
		var input SourceInput
		if e := strictJSON(raw, &input); e != nil {
			t.Fatal("handwritten source envelope", e)
		}
	}
	for _, raw := range f.Preparations {
		var preparation Preparation
		if e := strictJSON(raw, &preparation); e != nil {
			t.Fatal("handwritten preparation", e)
		}
	}
	for _, raw := range f.Profiles {
		var profile InputProfile
		if e := strictJSON(raw, &profile); e != nil {
			t.Fatal("handwritten profile", e)
		}
	}
	for _, raw := range f.SourceValues {
		var value SourceValue
		if e := strictJSON(raw, &value); e != nil {
			t.Fatal("handwritten source custody", e)
		}
	}
	for _, n := range f.Negative {
		t.Run(n.Name, func(t *testing.T) {
			m, _ := exactMembers(encode(f.Selection))
			m[n.Member] = n.Value
			header := base64.RawURLEncoding.EncodeToString(encode(m))
			// The CLI's exported selector owner and authenticated HTTP owner
			// must preserve the same closed-selector refusal before artifacts.
			selected, e := ParseConversionSelector(header, "document-validation", "preview", "preparation")
			if _, code := verificationOutcome(e); e == nil || selected != nil || code != n.Code {
				t.Fatalf("CLI selector refusal differs: got=%s expected=%s error=%v", code, n.Code, e)
			}
			body := &unreadBody{}
			req := httptest.NewRequest("POST", "/v1/retained-column/1/inputs/next", body)
			req.Header.Set("Authorization", "Bearer vector")
			req.Header.Set("JPack-Conversion-Contract", header)
			w := httptest.NewRecorder()
			(&Service{}).Handler("vector").ServeHTTP(w, req)
			if w.Code < 400 || body.reads != 0 {
				t.Fatal("negative selector read body or admitted", n.Name, w.Code, body.reads)
			}
			var result struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			json.Unmarshal(w.Body.Bytes(), &result)
			if result.Error.Code != n.Code {
				t.Fatalf("wrong refusal class: got=%s expected=%s body=%s", result.Error.Code, n.Code, w.Body.String())
			}
		})
	}
}

func TestColumnRoleOwnerReportAndRequestAdmission(t *testing.T) {
	i := columnCase(t, "text", `"A"`)
	i.Source.ConversionContract = selectedContract()
	other := cloneInput(i)
	other.Source.ConversionContract.Roles = other.Source.ConversionContract.Roles[:3]
	if code, _, _ := scopedEntry(t, i, nil, encode(other)); code != 422 {
		t.Fatal("body replaced admitted header selection", code)
	}
	producer := ConversionCapabilities("producer-carrier")[0]
	producer.ProducerGrammar = columnProducerGrammar
	producer.Limits["carrierBytes"] = 3145727
	if requireConversionCapability([]ConversionCapability{producer}, "producer-carrier", true) != nil {
		t.Fatal("valid lower producer capacity refused")
	}
	producer.Limits["carrierBytes"] = 3145728
	if validateConversionCapabilities([]ConversionCapability{producer, producer}) == nil {
		t.Fatal("duplicate capability role admitted")
	}
	if validateConversionCapabilities(make([]ConversionCapability, 33)) == nil {
		t.Fatal("overbound capability list admitted")
	}
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	bin := filepath.Join(dir, "reported-runtime")
	if e := os.WriteFile(bin, []byte("#!/bin/sh\nexec /bin/cat '"+reportPath+"'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	doc := ConversionCapabilities("document-validation")[0]
	for _, tc := range []struct {
		name     string
		report   any
		accepted bool
	}{
		{"exact", map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{doc}}}, true},
		{"absent", map[string]any{"capabilities": map[string]any{}}, false},
		{"other-owner", map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{producer}}}, false},
		{"empty", map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := os.WriteFile(reportPath, encode(tc.report), 0600); e != nil {
				t.Fatal(e)
			}
			e := runtimeConversionAdmission(context.Background(), bin, dir, selectedContract())
			if (e == nil) != tc.accepted {
				t.Fatal("independent Runtime document owner report admission differed", tc.name, e)
			}
		})
	}
	doc.Limits["frameBytes"] = 2097151
	if e := os.WriteFile(reportPath, encode(map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{doc}}}), 0600); e != nil {
		t.Fatal(e)
	}
	if runtimeConversionAdmission(context.Background(), bin, dir, selectedContract()) != nil {
		t.Fatal("valid lower Runtime frame capacity refused")
	}
	if runtimeConversionAdmission(context.Background(), filepath.Join(dir, "absent"), dir, nil) != nil {
		t.Fatal("ordinary input required selected Runtime report")
	}
}

// The closed role grammar and duplicate guard independently make lists this
// large invalid. This explicit count control bounds work before entry validation.
func TestColumnRoleOwnerListLimit(t *testing.T) {
	entry := ConversionCapabilities("producer-carrier")[0]
	entry.ProducerGrammar = columnProducerGrammar
	entries := make([]ConversionCapability, 33)
	for j := range entries {
		entries[j] = entry
	}
	if validateConversionCapabilities(entries) == nil {
		t.Fatal("overbound capability list admitted")
	}
}
