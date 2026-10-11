package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// seed2 is RFC 8032's second seed, whose public half is key2.
const seed2 = "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb"

// keyedRunner is a Runner started on the Runtime at bin with a signing key,
// the guide's first seed, and a job of the fixture's pack.
type keyedRunner struct {
	t     *testing.T
	key   string
	job   string
	input runner.Input
	call  func(method, path string, body any, idem string, want int) json.RawMessage
}

func startKeyed(t *testing.T, bin string) *keyedRunner {
	t.Helper()
	keyDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keyDir, "decisions.seed")
	if err = os.WriteFile(key, []byte(seed1+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := runner.Open(runner.Config{Dir: dir, Runtime: bin, Workspace: "test-workspace", Owner: "local-owner", SigningKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := s.Handler("test")
	k := &keyedRunner{t: t, key: key, input: caseInput(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)}
	k.call = func(method, path string, body any, idem string, want int) json.RawMessage {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatal(method, path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	pack, err := os.ReadFile("../../internal/runner/testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	var id struct {
		ID string `json:"id"`
	}
	json.Unmarshal(k.call("POST", "/v1/previews", map[string]any{"pack": string(pack), "input": k.input}, "", 201), &id)
	json.Unmarshal(k.call("POST", "/v1/jobs", map[string]any{"name": "Keyed", "releaseId": id.ID, "reviewed": true}, "", 201), &id)
	k.job = id.ID
	return k
}

// run completes one run of the fixture's case, and answers the run as the
// run route serves it, its export asked for as version 5, and its release
// digest.
func (k *keyedRunner) run(idem string) (map[string]json.RawMessage, map[string]json.RawMessage, string) {
	t := k.t
	t.Helper()
	var id struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	json.Unmarshal(k.call("POST", "/v1/jobs/"+k.job+"/runs", k.input, idem, 202), &id)
	var served map[string]json.RawMessage
	for deadline := time.Now().Add(10 * time.Second); id.State != "completed"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) || id.State == "failed" || id.State == "interrupted" {
			t.Fatal("run did not complete:", id.State)
		}
		raw := k.call("GET", "/v1/runs/"+id.ID, nil, "", 200)
		json.Unmarshal(raw, &id)
		json.Unmarshal(raw, &served)
	}
	var bundle map[string]json.RawMessage
	var digest string
	if err := json.Unmarshal(k.call("GET", "/v1/runs/"+id.ID+"/verification?version=5", nil, "", 200), &bundle); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bundle["releaseDigest"], &digest); err != nil {
		t.Fatal(err)
	}
	return served, bundle, digest
}

// replace renames a new private file holding seed2 over the key's path, as a
// same-user process that replaces the key after Runner's start does.
func (k *keyedRunner) replace() {
	k.t.Helper()
	next := k.key + ".next"
	if err := os.WriteFile(next, []byte(seed2+"\n"), 0600); err != nil {
		k.t.Fatal(err)
	}
	if err := os.Rename(next, k.key); err != nil {
		k.t.Fatal(err)
	}
}

// decision is a served run's decision: its result's disposition.
func decision(t *testing.T, served map[string]json.RawMessage) string {
	t.Helper()
	var result struct {
		Disposition json.RawMessage `json:"disposition"`
	}
	if err := json.Unmarshal(served["result"], &result); err != nil || len(result.Disposition) == 0 {
		t.Fatalf("the run has no decision: %v %s", err, served["result"])
	}
	return string(result.Disposition)
}

// noKeyNamed fails the test if what a run route served names the key's path,
// either seed, or the replacement's public key.
func (k *keyedRunner) noKeyNamed(served map[string]json.RawMessage) {
	k.t.Helper()
	raw, _ := json.Marshal(served)
	for _, secret := range []string{k.key, seed1, seed2, key2} {
		if strings.Contains(string(raw), secret) {
			k.t.Fatalf("the run names the key's path or a key: %s", raw)
		}
	}
}

// signedUnderKey1 is the signing of a run signed under the expected key.
const signedUnderKey1 = `{"status":"signed","publicKey":"` + key1 + `"}`

// With a Runtime that knows the expected key (0.30.0 and later; CI pins one
// as JPACK_TEST_BIN), a seed replaced at its path after Runner's start signs
// nothing: the Runtime refuses it. The run completes with its decision, and
// says its signing failed, in words that name neither the path nor a key;
// verify-run --public-key <expected> --require-signed refuses it as unsigned.
func TestASeedReplacedAfterStartSignsNothing(t *testing.T) {
	bin := os.Getenv("JPACK_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_TEST_BIN to a Runtime of 0.30.0 or later")
	}
	k := startKeyed(t, bin)
	before, bundle, digest := k.run("before")
	if string(before["signing"]) != signedUnderKey1 || string(bundle["version"]) != "5" {
		t.Fatalf("the run before the replacement is not signed under the expected key: %s, version %s", before["signing"], bundle["version"])
	}
	if _, _, err := verify(t, bundle, digest, "--public-key", key1, "--require-signed"); err != nil {
		t.Fatal(err)
	}
	k.replace()
	after, bundle, digest := k.run("after")
	want := `{"status":"failed","reason":"The Runtime signed nothing: the key at the signing key's path is not the expected key."}`
	if string(after["signing"]) != want {
		t.Fatalf("the run after the replacement says %s; a Runtime of 0.30.0 or later refuses a seed that is not the expected key", after["signing"])
	}
	if decision(t, after) != decision(t, before) || after["auditSignatures"] != nil || string(bundle["version"]) != "4" {
		t.Fatalf("the decision is not kept, or a sidecar is: version %s", bundle["version"])
	}
	k.noKeyNamed(after)
	_, _, err := verify(t, bundle, digest, "--public-key", key1, "--require-signed")
	if !errors.Is(err, errUnsigned) {
		t.Fatalf("verify-run --require-signed does not refuse the run as unsigned: %v", err)
	}
}

// With a Runtime that signs and does not know the expected key (before
// 0.30.0; CI pins 0.27.1 as JPACK_EARLIER_TEST_BIN), a seed replaced at its
// path after Runner's start signs the run under the replacement. The run
// completes with its decision, and says its signing failed: its sidecar names
// a key that is not the expected one. verify-run --public-key <expected>
// --require-signed refuses it, and under the replacement's key it is signed.
func TestAnEarlierRuntimeSigningWithAReplacedSeedIsNamed(t *testing.T) {
	bin := os.Getenv("JPACK_EARLIER_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_EARLIER_TEST_BIN to a Runtime that signs and does not know the expected key, from 0.26.0 to 0.29.x")
	}
	k := startKeyed(t, bin)
	before, bundle, digest := k.run("before")
	if string(before["signing"]) != signedUnderKey1 {
		t.Fatalf("the run before the replacement is not signed under the expected key: %s", before["signing"])
	}
	if _, _, err := verify(t, bundle, digest, "--public-key", key1, "--require-signed"); err != nil {
		t.Fatal(err)
	}
	k.replace()
	after, bundle, digest := k.run("after")
	want := `{"status":"failed","reason":"The record's signature sidecar names a key that is not the expected one."}`
	if string(after["signing"]) != want {
		t.Fatalf("the run after the replacement says %s; a Runtime before 0.30.0 signs with the replacement", after["signing"])
	}
	if decision(t, after) != decision(t, before) || after["auditSignatures"] == nil || string(bundle["version"]) != "5" {
		t.Fatalf("the decision or the sidecar is not kept: version %s", bundle["version"])
	}
	k.noKeyNamed(after)
	if _, _, err := verify(t, bundle, digest, "--public-key", key1, "--require-signed"); !errors.Is(err, errSignatureInvalid) {
		t.Fatalf("verify-run does not refuse the run under the expected key: %v", err)
	}
	if out, _, err := verify(t, bundle, digest, "--public-key", key2, "--require-signed"); err != nil || !strings.Contains(out, `"status":"signed"`) {
		t.Fatalf("the run is not signed under the replacement's key: %v %s", err, out)
	}
}

// The boot line's signingPublicKey reaches the service as the expected key it
// names, beside signingKey; without it, none is named.
func TestTheBootLineNamesTheExpectedSigningKey(t *testing.T) {
	token := strings.Repeat("t", 32)
	cfg, bearer, err := bootConfig([]byte(`{"dir":"/state","runtime":"/jpack","workspace":"w","owner":"o","token":"` + token + `","signingKey":"/keys/decisions.seed","signingPublicKey":"` + key1 + `"}` + "\n"))
	if err != nil || bearer != token || cfg.SigningKey != "/keys/decisions.seed" || cfg.SigningPublicKey != key1 {
		t.Fatalf("%v %+v", err, cfg)
	}
	cfg, _, err = bootConfig([]byte(`{"dir":"/state","runtime":"/jpack","workspace":"w","owner":"o","token":"` + token + `","signingKey":"/keys/decisions.seed"}` + "\n"))
	if err != nil || cfg.SigningKey != "/keys/decisions.seed" || cfg.SigningPublicKey != "" {
		t.Fatalf("%v %+v", err, cfg)
	}
	if _, _, err = bootConfig([]byte(`{"token":"short"}`)); err == nil || err.Error() != "invalid boot configuration" {
		t.Fatal(err)
	}
}
