package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// The guide's test vector keys: the seeds of RFC 8032's first two vectors.
const (
	seed1  = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	key1   = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	keyID1 = "21fe31dfa154a261626bf854046fd227"
	key2   = "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c"
	keyID2 = "39f713d0a644253f04529421b9f51b9b"
	// The trail a record is given when the Runtime under test did not chain it.
	syntheticTrail = "00112233445566778899aabbccddeeff"
)

// signatureLine is a sidecar line signing record as the given sequence of the
// given trail with seed, in the canonical form the Runtime writes.
func signatureLine(seed string, record []byte, sequence int64, trail string) string {
	s, _ := hex.DecodeString(seed)
	private := ed25519.NewKeyFromSeed(s)
	public := private.Public().(ed25519.PublicKey)
	id := sha256.Sum256(public)
	digest := sha(record)
	signature := ed25519.Sign(private, fmt.Appendf(nil, `judgment-pack-runtime/record-signature/1:{"record":"%s","sequence":%d,"trail":"%s"}`, digest, sequence, trail))
	return fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":%d,"sidecarVersion":"1","signature":%q,"trail":%q}`, hex.EncodeToString(id[:])[:32], digest, sequence, hex.EncodeToString(signature), trail) + "\n"
}

// chainedRecord is the record line the export carries, chained as the first
// line of a new trail if the Runtime under test did not chain it, and its
// trail.
func chainedRecord(t *testing.T, line []byte) ([]byte, string) {
	t.Helper()
	var members struct {
		Trail string `json:"trail"`
	}
	if err := json.Unmarshal(line, &members); err != nil {
		t.Fatal(err)
	}
	if members.Trail != "" {
		return line, members.Trail
	}
	from := []byte(`{"recordVersion":"1",`)
	if !bytes.HasPrefix(line, from) {
		t.Fatalf("the record does not start as the fixture expects: %.80s", line)
	}
	chained := append([]byte(`{"recordVersion":"1","trail":"`+syntheticTrail+`","sequence":1,"previous":"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",`), line[len(from):]...)
	return chained, syntheticTrail
}

// withRecord is a version-4 export with the record's bytes, and the record,
// replaced by line, and its entry and checkpoint made again for them: what an
// operator who rewrote the record and the chain would export.
func withRecord(t *testing.T, bundle map[string]json.RawMessage, line []byte) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for k, v := range bundle {
		out[k] = v
	}
	var run map[string]json.RawMessage
	var chain struct {
		Entry []byte `json:"entry"`
	}
	if err := json.Unmarshal(bundle["run"], &run); err != nil || json.Unmarshal(bundle["chain"], &chain) != nil {
		t.Fatal("not a version-4 export", err)
	}
	run["auditBytes"], _ = json.Marshal(line)
	run["audit"] = json.RawMessage(line)
	entry, err := runner.ParseChainEntry(chain.Entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.AuditDigest = sha(line)
	entryLine, _ := json.Marshal(entry)
	checkpoint := runner.Checkpoint{CheckpointVersion: "1", RecordDigest: sha(entryLine), Sequence: entry.Sequence, Trail: entry.Trail}
	out["run"], _ = json.Marshal(run)
	out["chain"], _ = json.Marshal(runner.RunChainExport{Entry: entryLine, Checkpoint: checkpoint})
	return out
}

// withSidecar is an export made version 5, carrying sidecar as the record's
// signature sidecar.
func withSidecar(t *testing.T, bundle map[string]json.RawMessage, sidecar string) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for k, v := range bundle {
		out[k] = v
	}
	var run map[string]json.RawMessage
	if err := json.Unmarshal(bundle["run"], &run); err != nil {
		t.Fatal(err)
	}
	run["auditSignatures"], _ = json.Marshal([]byte(sidecar))
	out["run"], _ = json.Marshal(run)
	out["version"] = json.RawMessage("5")
	return out
}

// signedExport is the chained run's export as the installation's signing key,
// the guide's first seed, would sign it: its record chained as a trail's first
// line if the Runtime under test did not chain it, and a sidecar line signing
// that record, in version 5. A Runtime that cannot sign is how CI runs today;
// a Runtime that can sign is held to the same checks in
// TestVerifyRunChecksARunTheRuntimeSigned.
func signedExport(t *testing.T) (exported, []byte, string, map[string]json.RawMessage) {
	t.Helper()
	e, _ := chainedRun(t)
	record, trail := chainedRecord(t, e.line)
	v4 := withRecord(t, e.bundle, record)
	return e, record, trail, v4
}

// The report member and the line on standard error for each case of a record
// signature, and the refusals: a signature under another key, a signature
// changed, a record rewritten with its chain but not re-signed, a signature
// for another trail's record, a torn sidecar, an export with none, and a key
// a verifier refuses, before the export is read.
func TestVerifyRunChecksTheRecordSignature(t *testing.T) {
	e, record, trail, v4 := signedExport(t)
	line := signatureLine(seed1, record, 1, trail)
	signed := withSidecar(t, v4, line)
	member := func(keyID, key, status string, findings string, total, readable, unreadable int) string {
		return fmt.Sprintf(`"signature":{"findings":%s,"findingsTotal":%d,"keyId":%q,"publicKey":%q,"readableLines":%d,"status":%q,"unreadableLines":%d}`, findings, total, keyID, key, readable, status, unreadable)
	}
	rewritten := withSidecar(t, withRecord(t, v4, bytes.Replace(record, []byte(`"kind":"evaluation"`), []byte(`"kind":"evaluation","note":"rewritten"`), 1)), line)
	at := strings.Index(line, `"signature":"`) + len(`"signature":"`)
	other := "0"
	if line[at] == '0' {
		other = "1"
	}
	flipped := line[:at] + other + line[at+1:]
	for name, c := range map[string]struct {
		bundle  map[string]json.RawMessage
		extra   []string
		status  string
		member  string
		said    string
		refusal string
	}{
		"signed": {signed, []string{"--public-key", key1}, "verified-inputs", member(keyID1, key1, "signed", "[]", 0, 1, 0),
			" The record's signature verifies under key " + keyID1 + ": whoever held that key signed these exact bytes as the attempt's record. That establishes nothing against the operator, who holds the key, and nothing once the key is copied.", ""},
		"signed, and required": {signed, []string{"--public-key", key1, "--require-signed"}, "verified-inputs", member(keyID1, key1, "signed", "[]", 0, 1, 0), "", ""},
		"not checked": {signed, nil, "verified-inputs", `"signature":{"findings":[],"findingsTotal":0,"readableLines":1,"status":"not-checked","unreadableLines":0}`,
			" The export carries the signature sidecar of the run's audit record, which was not checked: --public-key names the key to check it under.", ""},
		"under another key": {signed, []string{"--public-key", key2}, "signature-invalid",
			member(keyID2, key2, "invalid", `[{"name":"signature-invalid","line":1,"detail":"sidecar line 1: the signature does not verify under the key in force"}]`, 1, 1, 0), "",
			"signature-invalid: signature-invalid at line 1: sidecar line 1: the signature does not verify under the key in force"},
		"a signature changed": {withSidecar(t, v4, flipped), []string{"--public-key", key1}, "signature-invalid", "", "",
			"signature-invalid: signature-invalid at line 1: sidecar line 1: the signature does not verify under the key in force"},
		"the record rewritten with its chain": {rewritten, []string{"--public-key", key1}, "signature-invalid", "", "",
			"signature-invalid: signature-record-mismatch at line 1: sidecar line 1: the signature is for another record than the one at its sequence"},
		"for another trail's record": {withSidecar(t, v4, signatureLine(seed1, record, 1, strings.Repeat("a", 32))), []string{"--public-key", key1}, "signature-invalid", "", "",
			"signature-invalid: signature-no-record at line 1: sidecar line 1: the trail has no chained record of this trail at this sequence"},
		"a torn sidecar": {withSidecar(t, v4, strings.TrimSuffix(line, "\n")), []string{"--public-key", key1}, "verified-inputs", member(keyID1, key1, "unsigned", "[]", 0, 0, 1),
			" No valid signature under key " + keyID1 + " covers the record: its signature sidecar holds none. That is not a failure by itself, since a signature the Runtime could not write leaves its decision recorded; --require-signed refuses it.", ""},
		"a torn sidecar, required": {withSidecar(t, v4, strings.TrimSuffix(line, "\n")), []string{"--public-key", key1, "--require-signed"}, "unsigned", member(keyID1, key1, "unsigned", "[]", 0, 0, 1), "",
			"unsigned: no valid signature under key " + keyID1 + " covers the run's audit record, and --require-signed refuses it"},
		"no sidecar": {v4, []string{"--public-key", key1}, "verified-inputs", member(keyID1, key1, "unsigned", "[]", 0, 0, 0),
			" A version-4 export carries no record signatures, so as far as it shows no signature under key " + keyID1 + " covers the record: ask for version 5 to check one. An unsigned record is refused only with --require-signed.", ""},
		"no sidecar, required": {v4, []string{"--public-key", key1, "--require-signed"}, "unsigned", "", "",
			"unsigned: no valid signature under key " + keyID1 + " covers the run's audit record, and --require-signed refuses it"},
	} {
		t.Run(name, func(t *testing.T) {
			out, said, err := verify(t, c.bundle, e.digest, c.extra...)
			if c.refusal != "" {
				if err == nil || err.Error() != c.refusal {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, `"status":"`+c.status+`"`) || c.member != "" && !strings.Contains(out, c.member) {
				t.Fatalf("report:\n%s", out)
			}
			if c.said != "" && !strings.HasSuffix(strings.TrimSuffix(said, "\n"), c.said) {
				t.Fatalf("said:\n%s", said)
			}
		})
	}
	// A key is held to the key check before the export is read; and
	// --require-signed needs a key.
	for flags, refusal := range map[string]string{
		"--public-key 0000000000000000000000000000000000000000000000000000000000000000": "--public-key is refused: the public key is a point of small order, under which anyone can sign",
		"--public-key edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f": "--public-key is refused: the public key is not the canonical encoding of a point",
		"--public-key 0200000000000000000000000000000000000000000000000000000000000000": "--public-key is refused: the public key is no point of the curve",
		"--public-key " + strings.ToUpper(key1):                                         "--public-key is refused: a public key is 64 lowercase hexadecimal characters",
		"--require-signed":                                                              "--require-signed needs --public-key, the key a signature is checked under",
	} {
		var stdout, stderr bytes.Buffer
		err := verifyRun(append([]string{"--file", filepath.Join(t.TempDir(), "absent.json"), "--profiles", "absent.json", "--release-digest", e.digest}, strings.Fields(flags)...), &stdout, &stderr)
		if err == nil || err.Error() != refusal || stdout.Len() != 0 {
			t.Fatal(flags, err)
		}
	}
}

// probeSigning asks the Runtime at bin for the public key of the seed at key:
// "" when it prints it, the Runtime's own words when it answers, at exit status
// 3 with nothing else, that it has no `audit key public`, and an error for
// anything else, which fails the test.
func probeSigning(bin, key string) (string, error) {
	cmd := exec.Command(bin, "audit", "key", "public", key)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = filepath.Dir(key)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	ran := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ran == nil && stdout.String() == key1+"\n":
		return "", nil
	case ran != nil && !errors.As(ran, &exit):
		return "", errors.New("the Runtime could not be run: " + ran.Error())
	case ran != nil && exit.ExitCode() == 3 && stdout.Len() == 0:
		for _, answer := range []string{`unknown command "audit" for "jpack"`, `unknown command "key" for "jpack audit"`, `unknown command "public" for "jpack audit key"`} {
			if stderr.String() == "error: "+answer+"\n" {
				return answer, nil
			}
		}
	}
	return "", errors.New("the Runtime answered `audit key public` otherwise than with the key or an unknown command: " + stdout.String() + stderr.String())
}

// A run the Runtime signed with the installation's key verifies as signed,
// as exported, and --require-signed passes it; under another key it is
// refused.
func TestVerifyRunChecksARunTheRuntimeSigned(t *testing.T) {
	keyDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keyDir, "decisions.seed")
	if err = os.WriteFile(key, []byte(seed1+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e := exportedRunKeyed(t, caseInput(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`), nil, "", "?version=5", key, "")
	runtime, err := os.ReadFile(os.Getenv("JPACK_TEST_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(runtime)
	absent, err := probeSigning(filepath.Join(e.dir, "runtimes", hex.EncodeToString(sum[:])), key)
	if err != nil {
		t.Fatal(err)
	}
	if absent != "" {
		t.Skipf("the Runtime under test answers `jpack audit key public` with %q: it cannot sign, as Runtime 0.25.0 and earlier cannot. "+
			"This test needs a Runtime that signs its trail (runtime #209 and #216): Runtime 0.26.0 or later, which CI pins (runner #35)", absent)
	}
	var run struct {
		AuditSignatures []byte `json:"auditSignatures"`
	}
	if string(e.bundle["version"]) != "5" || json.Unmarshal(e.bundle["run"], &run) != nil || len(run.AuditSignatures) == 0 {
		t.Fatal("a run the Runtime signed is not exported as version 5")
	}
	out, said, err := verify(t, e.bundle, e.digest, "--public-key", key1, "--require-signed")
	if err != nil || !strings.Contains(out, `"status":"signed"`) || !strings.Contains(said, "The record's signature verifies under key "+keyID1) {
		t.Fatal(err, out, said)
	}
	if _, _, err = verify(t, e.bundle, e.digest, "--public-key", key2); err == nil || !strings.HasPrefix(err.Error(), "signature-invalid: signature-invalid at line 1") {
		t.Fatal(err)
	}
}
