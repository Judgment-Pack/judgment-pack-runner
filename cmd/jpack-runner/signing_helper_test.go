package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain runs the tests, or, run by signingRuntime in an attempt's directory
// with RUNNER_TEST_SIGN_ATTEMPT set, signs the attempt as a Runtime that signs
// would, and exits.
func TestMain(m *testing.M) {
	if os.Getenv("RUNNER_TEST_SIGN_ATTEMPT") == "1" {
		dir, err := os.Getwd()
		if err == nil {
			err = signAttempt(dir)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// signAttempt signs the record of the attempt in dir with the guide's first
// seed, as a Runtime given that key would: it chains the record as a new
// trail's first line if the Runtime under test did not, and writes the
// sidecar's one line, ended by its newline.
func signAttempt(dir string) error {
	path := filepath.Join(dir, "audit", "evaluations.jsonl")
	trail, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	line, ended := bytes.CutSuffix(trail, []byte("\n"))
	if !ended || bytes.Contains(line, []byte("\n")) {
		return errors.New("the attempt's trail is not one line")
	}
	var members struct {
		Trail string `json:"trail"`
	}
	if err = json.Unmarshal(line, &members); err != nil {
		return err
	}
	if members.Trail == "" {
		from := []byte(`{"recordVersion":"1",`)
		if !bytes.HasPrefix(line, from) {
			return errors.New("the record does not start as the helper expects")
		}
		line = append([]byte(`{"recordVersion":"1","trail":"`+syntheticTrail+`","sequence":1,"previous":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",`), line[len(from):]...)
		if err = os.WriteFile(path, append(bytes.Clone(line), '\n'), 0600); err != nil {
			return err
		}
		members.Trail = syntheticTrail
	}
	return os.WriteFile(filepath.Join(dir, "audit", "signatures.jsonl"), []byte(signatureLine(seed1, line, 1, members.Trail)), 0600)
}

// signingRuntime is the Runtime under test, after whose operational
// evaluation this test binary signs the attempt, as signAttempt does. Its
// output goes to log, never to the Runtime's answer Runner reads.
func signingRuntime(t *testing.T, log string) string {
	t.Helper()
	real := os.Getenv("JPACK_TEST_BIN")
	if real == "" {
		t.Skip("set JPACK_TEST_BIN to exercise the real Runtime contract")
	}
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n'" + real + "' \"$@\" || exit $?\n[ -f audit/evaluations.jsonl ] || exit 0\nRUNNER_TEST_SIGN_ATTEMPT=1 '" + self + "' >> '" + log + "' 2>&1\n"
	path := filepath.Join(t.TempDir(), "signing-runtime")
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A run whose attempt's sidecar holds a line signing its record, ended by its
// newline as every line the Runtime writes is, is exported with that sidecar
// byte for byte, and verify-run reports it signed, and passes it with
// --require-signed. The test signs the attempt itself, so this runs on any
// Runtime.
func TestVerifyRunChecksARecordKeptWithItsNewline(t *testing.T) {
	log := filepath.Join(t.TempDir(), "signing.log")
	runtime := signingRuntime(t, log)
	e := exportedRunKeyed(t, caseInput(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`), nil, "", "?version=5", "", runtime)
	var run struct {
		ID              string `json:"id"`
		AuditSignatures []byte `json:"auditSignatures"`
	}
	if err := json.Unmarshal(e.bundle["run"], &run); err != nil || string(e.bundle["version"]) != "5" {
		helper, _ := os.ReadFile(log)
		t.Fatalf("not a version-5 export: %v %s", err, helper)
	}
	written, err := os.ReadFile(filepath.Join(e.dir, "attempts", run.ID, "audit", "signatures.jsonl"))
	if err != nil || !bytes.Equal(run.AuditSignatures, written) || !bytes.HasSuffix(written, []byte("\n")) {
		t.Fatalf("the export does not carry the sidecar exactly, newline included: %q %q %v", run.AuditSignatures, written, err)
	}
	out, said, err := verify(t, e.bundle, e.digest, "--public-key", key1, "--require-signed")
	member := fmt.Sprintf(`"signature":{"findings":[],"findingsTotal":0,"keyId":%q,"publicKey":%q,"readableLines":1,"status":"signed","unreadableLines":0}`, keyID1, key1)
	if err != nil || !strings.Contains(out, member) || !strings.Contains(out, `"status":"verified-inputs"`) || !strings.Contains(said, "The record's signature verifies under key "+keyID1) {
		t.Fatal(err, out, said)
	}
}
