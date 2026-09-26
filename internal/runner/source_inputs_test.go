package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func fileInput(raw string, mapping InputMapping) Input {
	return Input{Source: &SourceInput{Mapping: mapping, Snapshot: encode(map[string]any{"version": 1, "selectedAt": "2026-09-25T12:00:00Z", "original": map[string]any{"name": "input.json", "mediaType": "application/json", "bytes": base64.StdEncoding.EncodeToString([]byte(raw)), "sha256": digest([]byte(raw))}})}}
}
func fileMapping() InputMapping {
	return InputMapping{Version: 1, Provider: "local-file", Facts: []FactMapping{{Target: "/request", Source: "/facts/request"}}, Evidence: []EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}}}
}
func trialFile() Input { return fileInput(string(encode(sample())), fileMapping()) }

func TestSourceMappingPreservesValuesAndOmission(t *testing.T) {
	mapping := InputMapping{Version: 1, Provider: "local-file", Facts: []FactMapping{{"/data/no", "/no"}, {"/data/zero", "/zero"}, {"/data/nil", "/nil"}, {"/data/precise", "/precise"}, {"/data/decimal", "/decimal"}, {"/data/missing", "/missing"}, {"/data/slash", "/a~1b/0"}}, Evidence: []EvidenceMapping{{"receipt", "/evidence/receipt"}, {"missing", "/evidence/missing"}}}
	got, err := normalizeInput(fileInput(`{"no":false,"zero":0,"nil":null,"precise":9007199254740993,"decimal":1.234567890123456789,"a/b":["first"],"evidence":{"receipt":"unknown"}}`, mapping))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"data":{"decimal":1.234567890123456789,"nil":null,"no":false,"precise":9007199254740993,"slash":"first","zero":0}}`
	if string(got.Facts) != want || string(got.Evidence) != `{"receipt":"unknown"}` || got.Source.MappingDigest == "" {
		t.Fatal(string(got.Facts), string(got.Evidence))
	}
	if !strings.Contains(inputJSONText(got.Facts), "9007199254740993") || !strings.Contains(inputJSONText(got.Facts), "1.234567890123456789") {
		t.Fatal("display rounded source values")
	}
	again, err := normalizeInput(got)
	if err != nil || !bytes.Equal(again.Facts, got.Facts) {
		t.Fatal("not repeatable", err)
	}
	got.Facts = json.RawMessage(`{}`)
	if _, err = normalizeInput(got); err == nil {
		t.Fatal("conflicting facts accepted")
	}
}
func TestSourceInputRejectsAmbiguityAndUnsafeMappings(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\u0020"}`, `[]`, `{} {}`, strings.Repeat(`{"x":`, 34) + `0` + strings.Repeat(`}`, 34), "{\"x\":\"\xff\"}"} {
		t.Run(raw[:min(len(raw), 24)], func(t *testing.T) {
			if _, err := normalizeInput(fileInput(raw, fileMapping())); err == nil {
				t.Fatal("accepted invalid source")
			}
		})
	}
	if _, err := normalizeInput(fileInput(`{"x":"\ud83d\ude00","escaped":"\\ud800"}`, fileMapping())); err != nil {
		t.Fatal("valid surrogate/escaped backslash refused", err)
	}
	for _, facts := range [][]FactMapping{{{"/a", "/x"}, {"/a/b", "/y"}}, {{"/a", "/x"}, {"/a", "/y"}}, {{"/__proto__/x", "/x"}}, {{"/a", "/bad~2"}}, {{"", "/a"}}} {
		m := fileMapping()
		m.Facts = facts
		if _, err := normalizeInput(fileInput(`{}`, m)); err == nil {
			t.Fatal("accepted unsafe mapping", facts)
		}
	}
	m := fileMapping()
	m.Evidence = []EvidenceMapping{{"receipt", "/receipt"}}
	for _, value := range []string{`true`, `null`, `"yes"`, `1`} {
		if _, err := normalizeInput(fileInput(`{"receipt":`+value+`}`, m)); err == nil {
			t.Fatal("coerced evidence", value)
		}
	}
	input := trialFile()
	input.Source.MappingDigest = "changed"
	if _, err := normalizeInput(input); err == nil {
		t.Fatal("mapping digest ignored")
	}
	input = trialFile()
	input.Source.Snapshot = bytes.Replace(input.Source.Snapshot, []byte(`"input.json"`), []byte(`"input.pdf"`), 1)
	// application/json is authoritative, independent of filename.
	if _, err := normalizeInput(input); err != nil {
		t.Fatal(err)
	}
	input.Source.Snapshot = bytes.Replace(input.Source.Snapshot, []byte(`"application/json"`), []byte(`"application/pdf"`), 1)
	if _, err := normalizeInput(input); err == nil {
		t.Fatal("accepted PDF")
	}
	input = trialFile()
	input.Source.Snapshot = bytes.Replace(input.Source.Snapshot, []byte(`"sha256:`), []byte(`"bad256:`), 1)
	if _, err := normalizeInput(input); err == nil {
		t.Fatal("accepted altered digest")
	}
	input = fileInput(strings.Repeat(" ", maxInputDocument)+"{}", m)
	if _, err := normalizeInput(input); err == nil {
		t.Fatal("accepted oversized file")
	}
}
func TestFileJobReleaseRunRetentionAndFrozenMapping(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	input := trialFile()
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if release.InputMapping == nil || release.Sample.Source == nil {
		t.Fatal("source not frozen")
	}
	job, err := s.createJob("File intake", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.submit(job.ID, "manual", sample()); err == nil {
		t.Fatal("accepted manual input for file job")
	}
	changed := trialFile()
	changed.Source.Mapping.Facts[0].Source = "/different"
	if _, _, err = s.submit(job.ID, "changed", changed); err == nil {
		t.Fatal("changed release mapping")
	}
	run, _, err := s.submit(job.ID, "file-one", input)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" || !bytes.Contains(done.Result, []byte(`"proceed"`)) {
		t.Fatal(done.State, done.Problem, string(done.Result))
	}
	s.Close()
	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.run(run.ID)
	if err != nil || held.Input.Source == nil || !bytes.Equal(held.Input.Source.Snapshot, input.Source.Snapshot) {
		t.Fatal("lost retained original", err)
	}
	for _, subject := range []struct{ kind, id string }{{"job", job.ID}, {"run", run.ID}} {
		body, e := s.briefSnapshot(subject.kind, subject.id)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(body, []byte(`"bytes"`)) || bytes.Contains(body, []byte(`"snapshot"`)) {
			t.Fatal("raw source included in brief")
		}
	}
	brief := string(encode(briefInput(held.Input)))
	if strings.Contains(brief, `"bytes"`) || strings.Contains(brief, `"proof"`) || !strings.Contains(brief, `"sha256"`) {
		t.Fatal("brief leaks bytes or omits provenance", brief)
	}
	req := httptest.NewRequest("POST", "/v1/inputs/preview", bytes.NewReader(encode(input)))
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	s.Handler("test").ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"factsText"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	again, replay, err := s.submit(job.ID, "file-one", input)
	if err != nil || !replay || again.ID != run.ID {
		t.Fatal("file replay changed after restart", err)
	}
}
func TestDriveSourceBindsRetainedBytesWithoutClaimingReceiptVerification(t *testing.T) {
	input := trialFile()
	input.Source.Mapping.Provider = "google-drive"
	var snapshot map[string]any
	_ = json.Unmarshal(input.Source.Snapshot, &snapshot)
	delete(snapshot, "selectedAt")
	original := snapshot["original"].(map[string]any)
	raw, _ := base64.StdEncoding.DecodeString(original["bytes"].(string))
	response := map[string]any{"result": map[string]any{"document": map[string]any{"id": original["sha256"], "name": "input.json", "mediaType": "application/json", "size": len(raw)}, "original": map[string]any{"bytes": original["bytes"]}, "provenance": map[string]any{"observedAt": now(), "source": map[string]any{"kind": "google-drive", "fileId": "selected-file", "version": "3"}}}, "receipt": map[string]any{"fixture": "not a verified signature"}}
	snapshot["proof"] = map[string]any{"session": "session", "source": "drive", "authority": "local", "publicKey": strings.Repeat("a", 64), "response": string(encode(response)), "registry": "retained seal", "drive": map[string]any{"fileId": "selected-file", "grant": strings.Repeat("b", 64)}}
	input.Source.Snapshot = encode(snapshot)
	got, err := normalizeInput(input)
	if err != nil {
		t.Fatal(err)
	}
	// Authenticity is independently verified by Desk using the current pin. Runner
	// accepts a retained source contract, never grants network access or trusts a badge.
	brief := string(encode(briefInput(got)))
	if strings.Contains(brief, "retained seal") || strings.Contains(brief, strings.Repeat("b", 64)) {
		t.Fatal("receipt/grant in brief")
	}
	snapshot["original"].(map[string]any)["bytes"] = base64.StdEncoding.EncodeToString([]byte(`{}`))
	snapshot["original"].(map[string]any)["sha256"] = digest([]byte(`{}`))
	input.Source.MappingDigest = ""
	input.Source.Snapshot = encode(snapshot)
	if _, err = normalizeInput(input); err == nil {
		t.Fatal("accepted swapped source bytes")
	}
}
