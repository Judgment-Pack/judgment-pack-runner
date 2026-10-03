package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// syntheticTrail is the trail an attempt's record is given when the Runtime
// under test did not chain it.
const syntheticTrail = "00112233445566778899aabbccddeeff"

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
// sidecar's one line, ended by its newline. The signed bytes are spelled out
// here, as the guide spells them, and not taken from the code under test.
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
	link, chained := readRecordLink(line)
	if !chained {
		from := []byte(`{"recordVersion":"1",`)
		if !bytes.HasPrefix(line, from) {
			return errors.New("the record does not start as the helper expects")
		}
		line = append([]byte(`{"recordVersion":"1","trail":"`+syntheticTrail+`","sequence":1,"previous":"`+emptyDigest+`",`), line[len(from):]...)
		if err = os.WriteFile(path, append(bytes.Clone(line), '\n'), 0600); err != nil {
			return err
		}
		link.trail = syntheticTrail
	}
	seed, _ := hex.DecodeString(vectorSeed1)
	record := digest(line)
	signature := ed25519.Sign(ed25519.NewKeyFromSeed(seed), fmt.Appendf(nil, `judgment-pack-runtime/record-signature/1:{"record":"%s","sequence":1,"trail":"%s"}`, record, link.trail))
	sidecar := fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":1,"sidecarVersion":"1","signature":%q,"trail":%q}`, vectorKeyID1, record, hex.EncodeToString(signature), link.trail) + "\n"
	return os.WriteFile(filepath.Join(dir, "audit", "signatures.jsonl"), []byte(sidecar), 0600)
}

// signingRuntime is the Runtime under test, after whose operational
// evaluation this test binary signs the attempt, as signAttempt does. Its
// output goes to log, never to the Runtime's answer Runner reads.
func signingRuntime(t *testing.T, real, log string) string {
	t.Helper()
	real, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
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
