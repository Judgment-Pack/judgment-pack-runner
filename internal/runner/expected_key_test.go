package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vectorSeed2 is RFC 8032's second seed, whose public half is vectorKey2.
const vectorSeed2 = "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb"

// replaceSeed renames a new private file holding seed over the key's path, as
// a same-user process that replaces the key does.
func replaceSeed(t *testing.T, path, seed string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(seed+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

// sidecarLineUnder is a sidecar line signing record, the first line of
// syntheticTrail, with seed, spelled out as the guide spells it.
func sidecarLineUnder(seed string, record []byte) string {
	s, _ := hex.DecodeString(seed)
	private := ed25519.NewKeyFromSeed(s)
	public := private.Public().(ed25519.PublicKey)
	signature := ed25519.Sign(private, fmt.Appendf(nil, `judgment-pack-runtime/record-signature/1:{"record":"%s","sequence":1,"trail":"%s"}`, digest(record), syntheticTrail))
	return fmt.Sprintf(`{"keyId":%q,"kind":"record-signature","record":%q,"sequence":1,"sidecarVersion":"1","signature":%q,"trail":%q}`, KeyID(public), digest(record), hex.EncodeToString(signature), syntheticTrail) + "\n"
}

// Runner reads the signing key once, at start, in the Runtime's seed form, and
// fixes its public key as the expected key. A key that cannot be read, or is
// not a seed, refuses the start as a misplaced key does, and the refusal names
// neither the path nor anything the file holds.
func TestTheExpectedKeyIsFixedAtStart(t *testing.T) {
	for name, held := range map[string]string{
		"as generated":                         vectorSeed1 + "\n",
		"in upper case, in whitespace":         " \t" + strings.ToUpper(vectorSeed1) + "\r\n\n",
		"with nothing after it":                vectorSeed1,
		"padded to the most the Runtime reads": vectorSeed1 + strings.Repeat(" ", maxSeedFile-len(vectorSeed1)),
	} {
		t.Run(name, func(t *testing.T) {
			key := signingKey(t)
			if err := os.WriteFile(key, []byte(held), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(keyConfig(t, key))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if hex.EncodeToString(s.expectedKey) != vectorKey1 {
				t.Fatalf("the expected key is %x", s.expectedKey)
			}
		})
	}
	for name, c := range map[string]struct {
		held    string
		mode    os.FileMode
		refusal string
	}{
		"not a seed":           {"not a seed, though it is 64 characters long, give or take a few\n", 0600, errNotASeed.Error()},
		"one character short":  {vectorSeed1[1:] + "\n", 0600, errNotASeed.Error()},
		"one character more":   {vectorSeed1 + "0\n", 0600, errNotASeed.Error()},
		"two seeds":            {vectorSeed1 + "\n" + vectorSeed2 + "\n", 0600, errNotASeed.Error()},
		"empty":                {"", 0600, errNotASeed.Error()},
		"longer than read":     {vectorSeed1 + strings.Repeat(" ", maxSeedFile-len(vectorSeed1)+1), 0600, errNotASeed.Error()},
		"not hexadecimal":      {strings.Repeat("g", 64) + "\n", 0600, errNotASeed.Error()},
		"unreadable by anyone": {vectorSeed2 + "\n", 0, "it could not be opened"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.mode == 0 && os.Getuid() == 0 {
				t.Skip("root reads a file of mode 0000")
			}
			key := signingKey(t)
			if err := os.WriteFile(key, []byte(c.held), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(key, c.mode); err != nil {
				t.Fatal(err)
			}
			s, err := Open(keyConfig(t, key))
			if err == nil {
				s.Close()
				t.Fatal("the key was accepted")
			}
			said := err.Error()
			if said != "the signing key is refused: "+c.refusal {
				t.Fatal(said)
			}
			for _, secret := range []string{key, strings.TrimSpace(c.held), vectorSeed2[:16], vectorKey2[:16]} {
				if secret != "" && strings.Contains(said, secret) {
					t.Fatalf("the refusal names the path or the key: %s", said)
				}
			}
		})
	}
}

// A run's signing holds the record, as kept, to the expected key by
// verify-run's check: signed only where a valid signature under that key
// covers it. A sidecar's presence signs nothing, a sidecar that could not be
// read is never taken for one that is not there, and where no line signs the
// record the key at the path says why. No reason names the path or a key.
func TestARunsSigningIsHeldToTheExpectedKey(t *testing.T) {
	record := []byte(vectorTrail1)
	signed1, signed2 := sidecarLineUnder(vectorSeed1, record), sidecarLineUnder(vectorSeed2, record)
	// The same line under the expected key's keyId, with a signature that is
	// of the form and verifies under no key.
	at := strings.Index(signed1, `"signature":"`) + len(`"signature":"`)
	broken := signed1[:at] + strings.Repeat("0", 128) + signed1[at+128:]
	for name, c := range map[string]struct {
		sidecar string
		record  []byte
		unread  bool
		key     func(t *testing.T, path string)
		want    RunSigning
	}{
		"signed under the expected key":        {sidecar: signed1, record: record, want: RunSigning{Status: SigningSigned, PublicKey: vectorKey1}},
		"the guide's line":                     {sidecar: vectorLine1 + "\n", record: record, want: RunSigning{Status: SigningSigned, PublicKey: vectorKey1}},
		"signed under another key":             {sidecar: signed2, record: record, want: RunSigning{Status: SigningFailed, Reason: signingOtherKey}},
		"another key after the expected one":   {sidecar: signed1 + signed2, record: record, want: RunSigning{Status: SigningFailed, Reason: signingOtherKey}},
		"a signature that does not verify":     {sidecar: broken, record: record, want: RunSigning{Status: SigningFailed, Reason: signingNotVerified}},
		"a signature of other bytes":           {sidecar: signed1, record: []byte(vectorTrail1 + " "), want: RunSigning{Status: SigningFailed, Reason: signingNotVerified}},
		"a torn line under the expected key":   {sidecar: strings.TrimSuffix(signed1, "\n"), record: record, want: RunSigning{Status: SigningFailed, Reason: signingNoSignature}},
		"no line the rule reads":               {sidecar: "not a sidecar line\n", record: record, want: RunSigning{Status: SigningFailed, Reason: signingNoSignature}},
		"no sidecar":                           {record: record, want: RunSigning{Status: SigningFailed, Reason: signingNoSignature}},
		"a sidecar that could not be read":     {record: record, unread: true, want: RunSigning{Status: SigningFailed, Reason: signingSidecarUnread}},
		"no sidecar, the key replaced":         {record: record, key: func(t *testing.T, path string) { replaceSeed(t, path, vectorSeed2) }, want: RunSigning{Status: SigningFailed, Reason: signingRuntimeSignedNothing}},
		"no sidecar, the key put back":         {record: record, key: func(t *testing.T, path string) { replaceSeed(t, path, vectorSeed2); replaceSeed(t, path, vectorSeed1) }, want: RunSigning{Status: SigningFailed, Reason: signingNoSignature}},
		"no sidecar, the key gone":             {record: record, key: func(t *testing.T, path string) { os.Remove(path) }, want: RunSigning{Status: SigningFailed, Reason: signingKeyUnread}},
		"no sidecar, the key opened to others": {record: record, key: func(t *testing.T, path string) { os.Chmod(path, 0644) }, want: RunSigning{Status: SigningFailed, Reason: signingKeyUnread}},
		"no sidecar, the key no seed":          {record: record, key: func(t *testing.T, path string) { replaceSeed(t, path, "not a seed") }, want: RunSigning{Status: SigningFailed, Reason: signingKeyUnread}},
		"signed, whatever is at the path now":  {sidecar: signed1, record: record, key: func(t *testing.T, path string) { os.Remove(path) }, want: RunSigning{Status: SigningSigned, PublicKey: vectorKey1}},
		"another key, whatever is at the path": {sidecar: signed2, record: record, key: func(t *testing.T, path string) { replaceSeed(t, path, vectorSeed2) }, want: RunSigning{Status: SigningFailed, Reason: signingOtherKey}},
		"unread, whatever is at the path":      {record: record, unread: true, key: func(t *testing.T, path string) { replaceSeed(t, path, vectorSeed2) }, want: RunSigning{Status: SigningFailed, Reason: signingSidecarUnread}},
	} {
		t.Run(name, func(t *testing.T) {
			key := signingKey(t)
			s, err := Open(keyConfig(t, key))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if c.key != nil {
				c.key(t, key)
			}
			var sidecar []byte
			if c.sidecar != "" {
				sidecar = []byte(c.sidecar)
			}
			got := s.signingOf(c.record, sidecar, c.unread)
			if got == nil || *got != c.want {
				t.Fatalf("%+v, want %+v", got, c.want)
			}
			said := string(encode(got))
			for _, secret := range []string{key, vectorSeed1, vectorSeed2, vectorKey2, vectorKeyID1, vectorKeyID2} {
				if strings.Contains(said, secret) {
					t.Fatalf("the signing names the path or a key: %s", said)
				}
			}
		})
	}
}

// The expected key is the one read at start, for as long as Runner runs: a
// key replaced at its path after start is not taken for it, and a Runner
// started again reads the key there anew.
func TestTheExpectedKeyOutlivesAReplacement(t *testing.T) {
	key := signingKey(t)
	cfg := keyConfig(t, key)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replaceSeed(t, key, vectorSeed2)
	record := []byte(vectorTrail1)
	if got := s.signingOf(record, []byte(sidecarLineUnder(vectorSeed2, record)), false); got.Status != SigningFailed || got.Reason != signingOtherKey {
		t.Fatalf("%+v", got)
	}
	s.Close()
	if s, err = Open(cfg); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !bytes.Equal(s.expectedKey, mustKey(t, vectorKey2)) {
		t.Fatalf("a Runner started again expects %x", s.expectedKey)
	}
}

// With a signing key, a sidecar Runner cannot read as it reads the trail
// fails the record's signing, in its own words, and not the run: the decision
// is kept, with no sidecar bytes. Without a key it fails the run, as before
// (TestTheSidecarIsKeptAsWritten).
func TestAnUnreadableSidecarFailsOnlyTheSigning(t *testing.T) {
	for name, write := range map[string]string{
		"not private":      "printf 'x\\n' > audit/signatures.jsonl && chmod 644 audit/signatures.jsonl",
		"a link":           "printf 'x\\n' > audit/elsewhere && rm -f audit/signatures.jsonl && ln -s elsewhere audit/signatures.jsonl",
		"larger than kept": "head -c 16385 /dev/zero | tr '\\000' x > audit/signatures.jsonl",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.SigningKey = signingKey(t)
			cfg.Runtime = sidecarRuntime(t, cfg.Runtime, write)
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			release := testRelease(t, s)
			job, err := s.createJob("Unread sidecar", release.ID)
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := s.submit(job.ID, "one", sample())
			if err != nil {
				t.Fatal(err)
			}
			done := waitRun(t, s, run.ID)
			if done.State != "completed" || len(done.Result) == 0 || len(done.AuditBytes) == 0 || done.AuditSignatures != nil {
				t.Fatal(done.State, done.Problem, done.AuditSignatures)
			}
			if done.Signing == nil || *done.Signing != (RunSigning{Status: SigningFailed, Reason: signingSidecarUnread}) {
				t.Fatalf("%+v", done.Signing)
			}
		})
	}
}

// withKeyHook runs hook between the key's checks on its open handle and the
// read of its seed, for the rest of the test.
func withKeyHook(t *testing.T, hook func()) {
	t.Helper()
	afterKeyCheck = hook
	t.Cleanup(func() { afterKeyCheck = nil })
}

// agedKey is signingKey, with its file's times set an hour back, as a key
// made before Runner starts has them: a change made now gives it another
// modification time, whatever the filesystem's timestamp granularity.
func agedKey(t *testing.T) string {
	t.Helper()
	key := signingKey(t)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(key, past, past); err != nil {
		t.Fatal(err)
	}
	return key
}

// A key file changed in place between its checks and the end of the read of
// its seed, through the one handle both use, refuses the start: its contents
// rewritten (the same size, the same inode), a name added, its mode changed,
// or bytes added. The refusal names neither the path nor any byte. A file
// left alone starts, under its own key.
func TestAKeyChangedWhileItIsReadRefusesTheStart(t *testing.T) {
	for name, change := range map[string]func(key string) error{
		"rewritten in place":  func(key string) error { return os.WriteFile(key, []byte(vectorSeed2+"\n"), 0600) },
		"given a second name": func(key string) error { return os.Link(key, key+".copy") },
		"its mode changed":    func(key string) error { return os.Chmod(key, 0400) },
		"added to": func(key string) error {
			f, err := os.OpenFile(key, os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, err = f.WriteString("\n")
				f.Close()
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			key := agedKey(t)
			withKeyHook(t, func() {
				if err := change(key); err != nil {
					t.Error(err)
				}
			})
			s, err := Open(keyConfig(t, key))
			if err == nil {
				defer s.Close()
				t.Fatalf("the start was not refused, and expects %x", s.expectedKey)
			}
			if said := err.Error(); said != "the signing key is refused: it changed while it was read" || strings.Contains(said, key) || strings.Contains(said, vectorSeed2) {
				t.Fatal(said)
			}
		})
	}
	t.Run("left alone", func(t *testing.T) {
		key := agedKey(t)
		ran := false
		withKeyHook(t, func() { ran = true })
		s, err := Open(keyConfig(t, key))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !ran || hex.EncodeToString(s.expectedKey) != vectorKey1 {
			t.Fatalf("the hook ran: %v; the expected key is %x", ran, s.expectedKey)
		}
	})
}

// The seed is read through the handle the checks were made on, never by the
// path again: with the key's directory swapped for one holding another seed
// after the checks, the expected key is still the checked file's. That file is
// unchanged, so the start goes on; a Runtime given the path then finds another
// key there, and refuses it (0.30.0 and later) or signs under it, and each run
// says so.
func TestTheSeedIsReadThroughTheHandleItWasCheckedOn(t *testing.T) {
	key := agedKey(t)
	dir := filepath.Dir(key)
	withKeyHook(t, func() {
		if err := os.Rename(dir, dir+".checked"); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(key, []byte(vectorSeed2+"\n"), 0600); err != nil {
			t.Error(err)
		}
	})
	s, err := Open(keyConfig(t, key))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if hex.EncodeToString(s.expectedKey) != vectorKey1 {
		t.Fatalf("the expected key is %x, not the checked file's", s.expectedKey)
	}
}

// An installation may name the expected key itself, as Desk can from its list
// of public keys. The key read at start must have it: a seed replaced before
// start, or before the read, cannot become the expectation. The named key is
// held to the key check before any seed is read, and is never echoed. Named
// without a signing key, it refuses the start.
func TestAConfiguredExpectedKeyIsHeldAtStart(t *testing.T) {
	t.Run("the key at the path has it", func(t *testing.T) {
		cfg := keyConfig(t, signingKey(t))
		cfg.SigningPublicKey = vectorKey1
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if hex.EncodeToString(s.expectedKey) != vectorKey1 {
			t.Fatalf("the expected key is %x", s.expectedKey)
		}
	})
	for name, c := range map[string]struct {
		named   string
		prepare func(t *testing.T, key string)
		refusal string
	}{
		"another key named":                  {vectorKey2, nil, errNotTheExpectedKey.Error()},
		"the seed replaced before the start": {vectorKey1, func(t *testing.T, key string) { replaceSeed(t, key, vectorSeed2) }, errNotTheExpectedKey.Error()},
		"the seed rewritten before the read": {vectorKey1, func(t *testing.T, key string) {
			withKeyHook(t, func() { os.WriteFile(key, []byte(vectorSeed2+"\n"), 0600) })
		}, "the signing key is refused: it changed while it was read"},
		"in upper case":     {strings.ToUpper(vectorKey1), nil, "the expected signing key is refused: " + errKeyForm.Error()},
		"with a newline":    {vectorKey1 + "\n", nil, "the expected signing key is refused: " + errKeyForm.Error()},
		"a character short": {vectorKey1[1:], nil, "the expected signing key is refused: " + errKeyForm.Error()},
		"of small order":    {smallOrderKeys[0], nil, "the expected signing key is refused: " + errKeySmallOrder.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			key := agedKey(t)
			if c.prepare != nil {
				c.prepare(t, key)
			}
			cfg := keyConfig(t, key)
			cfg.SigningPublicKey = c.named
			s, err := Open(cfg)
			if err == nil {
				defer s.Close()
				t.Fatalf("the start was not refused, and expects %x", s.expectedKey)
			}
			said := err.Error()
			if said != c.refusal || strings.Contains(said, key) || strings.Contains(said, vectorSeed1) || strings.Contains(said, vectorSeed2) || strings.Contains(said, strings.TrimSpace(c.named)) {
				t.Fatal(said)
			}
		})
	}
	t.Run("without a signing key", func(t *testing.T) {
		cfg := keyConfig(t, "")
		cfg.SigningPublicKey = vectorKey1
		if s, err := Open(cfg); err == nil {
			s.Close()
			t.Fatal("the start was not refused")
		} else if err.Error() != "the expected signing key is named without a signing key" {
			t.Fatal(err)
		}
	})
	t.Run("for the service's lifetime", func(t *testing.T) {
		key := signingKey(t)
		cfg := keyConfig(t, key)
		cfg.SigningPublicKey = vectorKey1
		s, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		replaceSeed(t, key, vectorSeed2)
		record := []byte(vectorTrail1)
		if got := s.signingOf(record, nil, false); *got != (RunSigning{Status: SigningFailed, Reason: signingRuntimeSignedNothing}) {
			t.Fatalf("%+v", got)
		}
		if got := s.signingOf(record, []byte(sidecarLineUnder(vectorSeed2, record)), false); *got != (RunSigning{Status: SigningFailed, Reason: signingOtherKey}) {
			t.Fatalf("%+v", got)
		}
	})
}
