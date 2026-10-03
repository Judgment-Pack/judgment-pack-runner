package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// signingKey writes the guide's first seed as a signing key, 64 hexadecimal
// characters and a newline, as `jpack audit key generate` writes one, in a
// private directory of its own outside any store, named by its real path.
func signingKey(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "decisions.seed")
	if err = os.WriteFile(path, []byte(vectorSeed1+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyConfig is a store whose Runtime is never run, given a signing key.
func keyConfig(t *testing.T, key string) Config {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	placeholder := filepath.Join(t.TempDir(), "never-run")
	if err = os.WriteFile(placeholder, []byte("not a Runtime: no test runs it\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Dir: dir, Runtime: placeholder, Workspace: "signing", Owner: "owner", SigningKey: key, disableAutomation: true}
}

// A signing key is held outside the store, by the placement the Runtime's
// guide states, or Runner does not open: an absolute, clean path with no
// symbolic link in it, through no directory that is the state directory, to
// one regular file with one name that its owner alone may read or write. The
// refusal never names the path.
func TestTheSigningKeyIsHeldOutsideTheStore(t *testing.T) {
	good := signingKey(t)
	s, err := Open(keyConfig(t, good))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for name, c := range map[string]struct {
		key     func(cfg Config) string
		refusal string
	}{
		"a relative path": {func(Config) string { return "decisions.seed" }, "its path must be absolute and clean"},
		"an unclean path": {func(Config) string { return filepath.Dir(good) + "/./decisions.seed" }, "its path must be absolute and clean"},
		"through a link": {func(Config) string {
			link := filepath.Join(t.TempDir(), "keys")
			os.Symlink(filepath.Dir(good), link)
			return filepath.Join(link, "decisions.seed")
		}, "no symbolic link may be anywhere in its path: name it by its real path"},
		"a link itself": {func(Config) string {
			link := filepath.Join(filepath.Dir(signingKey(t)), "link.seed")
			os.Symlink(good, link)
			return link
		}, "no symbolic link may be anywhere in its path: name it by its real path"},
		"inside the store": {func(cfg Config) string {
			dir := filepath.Join(cfg.Dir, "keys")
			os.Mkdir(dir, 0700)
			os.WriteFile(filepath.Join(dir, "decisions.seed"), []byte(vectorSeed1+"\n"), 0600)
			return filepath.Join(dir, "decisions.seed")
		}, "it is inside Runner's state directory, which holds every attempt's directory"},
		"readable by its group": {func(Config) string {
			key := signingKey(t)
			os.Chmod(key, 0640)
			return key
		}, "neither its group nor others may read or write it"},
		"writable by others": {func(Config) string {
			key := signingKey(t)
			os.Chmod(key, 0602)
			return key
		}, "neither its group nor others may read or write it"},
		"with a second name": {func(Config) string {
			key := signingKey(t)
			os.Link(key, key+".copy")
			return key
		}, "it must have one name, with no hard link elsewhere"},
		"another user's": {func(Config) string {
			// A file of the system's, owned by another user, reached without a
			// link and of one name; there is none to name when running as root.
			for _, path := range []string{"/usr/bin/env", "/bin/sh", "/usr/bin/true", "/etc/hosts"} {
				info, err := os.Lstat(path)
				real, _ := filepath.EvalSymlinks(path)
				if st, ok := info.Sys().(*syscall.Stat_t); err == nil && ok && os.Getuid() != 0 && real == path && info.Mode().IsRegular() && st.Nlink == 1 && st.Uid != uint32(os.Getuid()) {
					return path
				}
			}
			t.Skip("no file of another user's to name")
			return ""
		}, "it must be owned by the user Runner runs as"},
		"a directory":  {func(Config) string { return filepath.Dir(signingKey(t)) }, "it must be a regular file"},
		"no file":      {func(Config) string { return filepath.Join(filepath.Dir(good), "absent.seed") }, "there is no file at its path"},
		"under a file": {func(Config) string { return filepath.Join(good, "decisions.seed") }, "its path passes through something that is not a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := keyConfig(t, "")
			cfg.SigningKey = c.key(cfg)
			s, err := Open(cfg)
			if err == nil {
				s.Close()
				t.Fatal("the key was accepted")
			}
			if err.Error() != "the signing key is refused: "+c.refusal || strings.Contains(err.Error(), filepath.Base(filepath.Dir(good))) {
				t.Fatal(err)
			}
		})
	}
}

// filesHolding names the files under dir whose bytes contain needle.
func filesHolding(t *testing.T, dir string, needle []byte) []string {
	t.Helper()
	var found []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, e := os.ReadFile(path); e == nil && bytes.Contains(b, needle) {
				found = append(found, path)
			}
		}
		return nil
	})
	return found
}

// The key's path reaches the Runtime only for an operational evaluation, as
// JPACK_SIGNING_KEY, and is kept nowhere in the store: not for validation, a
// lock, a rehearsal or a saved test. A Runtime that cannot sign ignores it,
// and the run completes as before.
func TestTheSigningKeyReachesOnlyTheOperationalEvaluation(t *testing.T) {
	cfg := testConfig(t)
	real, err := filepath.Abs(cfg.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "invocations")
	wrapper := filepath.Join(t.TempDir(), "logging-runtime")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"${JPACK_SIGNING_KEY-none}\" \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Runtime, cfg.SigningKey = wrapper, signingKey(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release := testRelease(t, s)
	job, err := s.createJob("Keyed", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "one", sample())
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	operational := 0
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		key, args, _ := strings.Cut(line, "|")
		evaluation := strings.HasPrefix(args, "experimental evaluate ") && !strings.Contains(args, "--rehearsal")
		if evaluation {
			operational++
		}
		if evaluation != (key == cfg.SigningKey) || !evaluation && key != "none" {
			t.Fatalf("the key reached the wrong invocation: %s", line)
		}
	}
	if operational != 1 {
		t.Fatalf("%d operational evaluations:\n%s", operational, b)
	}
	if found := filesHolding(t, cfg.Dir, []byte(cfg.SigningKey)); len(found) > 0 {
		t.Fatal("the store keeps the key's path:", found)
	}
}

// Signing off, no sidecar is written or kept, and the record is unsigned: a
// version-5 export is asked for and version 4 is served, byte for byte.
func TestSigningOffKeepsNoSidecar(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob("Unsigned", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "one", input)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" || done.AuditSignatures != nil {
		t.Fatal(done.State, done.Problem, done.AuditSignatures)
	}
	if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "signatures.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a sidecar was written without a key:", err)
	}
	h := s.Handler("test")
	v4 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=4", 200)
	if v5 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=5", 200); !bytes.Equal(v5, v4) || !bytes.Contains(v4, []byte(`{"version":4,`)) || bytes.Contains(v4, []byte("auditSignatures")) {
		t.Fatalf("version 5 of an unsigned run is not its version 4: %.200s", v5)
	}
}

// sidecarRuntime is the Runtime under test, after whose operational
// evaluation write runs, a shell command in the attempt's directory.
func sidecarRuntime(t *testing.T, real, write string) string {
	t.Helper()
	real, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n'" + real + "' \"$@\" || exit $?\nPATH=/usr/bin:/bin\n[ -f audit/evaluations.jsonl ] || exit 0\numask 077\n" + write + "\n"
	path := filepath.Join(t.TempDir(), "sidecar-runtime")
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Whatever the attempt's sidecar holds is kept exactly, newlines included, in
// the transaction that records the run completed: a torn line, a line of no
// shape the rule reads. What it holds is read only when a run is verified, and
// only version 5 of the export carries it. An empty sidecar keeps nothing. A
// sidecar Runner cannot read as it reads the trail, larger than it keeps, not
// private, or a link, fails the run.
func TestTheSidecarIsKeptAsWritten(t *testing.T) {
	held := vectorLine1 + "\n" + "not a sidecar line\n" + vectorLine1[:40]
	for name, c := range map[string]struct {
		write   string
		kept    string
		problem string
	}{
		"as written":       {"printf '%s' '" + held + "' > audit/signatures.jsonl", held, ""},
		"empty":            {": > audit/signatures.jsonl", "", ""},
		"larger than kept": {"head -c 16385 /dev/zero | tr '\\000' x > audit/signatures.jsonl", "", "operational audit signatures could not be read"},
		"not private":      {"printf 'x\\n' > audit/signatures.jsonl && chmod 644 audit/signatures.jsonl", "", "operational audit signatures could not be read"},
		"a link":           {"printf 'x\\n' > audit/elsewhere && ln -s elsewhere audit/signatures.jsonl", "", "operational audit signatures could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Runtime = sidecarRuntime(t, cfg.Runtime, c.write)
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
			release, err := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.createJob("Kept", release.ID)
			if err != nil {
				t.Fatal(err)
			}
			run, _, err := s.submit(job.ID, "one", input)
			if err != nil {
				t.Fatal(err)
			}
			done := waitRun(t, s, run.ID)
			if c.problem != "" {
				if done.State != "failed" || done.Problem != c.problem || done.AuditSignatures != nil {
					t.Fatal(done.State, done.Problem)
				}
				return
			}
			if done.State != "completed" || string(done.AuditSignatures) != c.kept || (c.kept == "") != (done.AuditSignatures == nil) {
				t.Fatalf("%s %s: kept %q", done.State, done.Problem, done.AuditSignatures)
			}
			if c.kept == "" {
				return
			}
			h := s.Handler("test")
			answer := get(t, h, "/v1/runs/"+run.ID, 200)
			list := get(t, h, "/v1/runs", 200)
			snapshot, err := s.briefSnapshot("run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			member := []byte(`"auditSignatures":"` + base64.StdEncoding.EncodeToString([]byte(held)) + `"`)
			if !bytes.Contains(answer, member) || bytes.Contains(list, []byte("auditSignatures")) || bytes.Contains(snapshot, []byte("auditSignatures")) {
				t.Fatal("the run does not show its sidecar, or a list or brief carries it")
			}
			v4 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=4", 200)
			v5 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=5", 200)
			if bytes.Contains(v4, []byte("auditSignatures")) || !bytes.Contains(v4, []byte(`{"version":4,`)) || !bytes.Contains(v5, member) || !bytes.Contains(v5, []byte(`{"version":5,`)) {
				t.Fatal("version 4 carries the sidecar, or version 5 does not")
			}
		})
	}
}

// signingAbsentAnswers are the answers of a Runtime with no `audit key
// public`, from its command parser, on standard error, at exit status 3:
// Runtime 0.24.0 and 0.25.0 answer the first.
var signingAbsentAnswers = []string{
	"error: unknown command \"audit\" for \"jpack\"\n",
	"error: unknown command \"key\" for \"jpack audit\"\n",
	"error: unknown command \"public\" for \"jpack audit key\"\n",
}

// probeSigning asks the Runtime at bin for the public key of the seed at key,
// with `jpack audit key public`, which a Runtime that can sign has. It answers
// "" when the Runtime prints the key's public half; the Runtime's own words
// when it answers, with nothing else, that it has no such command; and an
// error for anything else, so that a Runtime that could not be run, or that
// failed otherwise, is never taken for one that cannot sign.
func probeSigning(bin, key, want, dir string) (string, error) {
	cmd := exec.Command(bin, "audit", "key", "public", key)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	ran := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ran == nil && stdout.String() == want+"\n":
		return "", nil
	case ran != nil && !errors.As(ran, &exit):
		return "", errors.New("the Runtime could not be run: " + ran.Error())
	case ran != nil && exit.ExitCode() == 3 && stdout.Len() == 0:
		for _, answer := range signingAbsentAnswers {
			if stderr.String() == answer {
				return strings.TrimSpace(answer), nil
			}
		}
	}
	return "", errors.New("the Runtime answered `audit key public` otherwise than with the key or an unknown command: " + stdout.String() + stderr.String())
}

// A Runtime given the key signs the attempt's record, and Runner keeps the
// sidecar's line exactly, with the record's, after a restart too. The line
// signs the record's trail, sequence 1 and the SHA-256 of the record's bytes,
// with the key, and a version-5 export carries it: verified under the key's
// public half the record is signed, and under another key it is not. Version 4
// of the run is what it was. The Runtime's own audit verify agrees.
func TestASignedRunKeepsItsSignature(t *testing.T) {
	cfg := testConfig(t)
	cfg.SigningKey = signingKey(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false,"vendor":"A&B"}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	bin := pinnedRuntime(s, release)
	absent, err := probeSigning(bin, cfg.SigningKey, vectorKey1, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if absent != "" {
		t.Skipf("the Runtime under test answers `jpack audit key public` with %q: it cannot sign, as Runtime 0.25.0 and earlier cannot. "+
			"This test needs a Runtime that signs its trail (runtime #209 and #216), and runs in CI once CI pins a Runtime release that does (runner #35)", absent)
	}
	job, err := s.createJob("Signed", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "one", input)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	sidecar, err := os.ReadFile(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "signatures.jsonl"))
	if err != nil || done.State != "completed" || !bytes.Equal(done.AuditSignatures, sidecar) || bytes.Count(sidecar, []byte("\n")) != 1 || !bytes.HasSuffix(sidecar, []byte("\n")) {
		t.Fatalf("%s %s: the sidecar %q is not kept as written: %v", done.State, done.Problem, sidecar, err)
	}
	line, ok := readSidecarLine(bytes.TrimSuffix(sidecar, []byte("\n")))
	link, chained := readRecordLink(done.AuditBytes)
	if !ok || !chained || line.rotation || line.trail != link.trail || line.at != 1 || line.value != digest(done.AuditBytes) || line.keyID != vectorKeyID1 {
		t.Fatalf("the sidecar's line does not sign the record: %s", sidecar)
	}
	s.Close()
	if s, err = Open(cfg); err != nil {
		t.Fatal(err)
	}
	if again, err := s.run(run.ID); err != nil || !bytes.Equal(again.AuditSignatures, sidecar) {
		t.Fatal("the sidecar is not kept after a restart", err)
	}
	h := s.Handler("test")
	v5 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=5", 200)
	verified, err := VerifyInputs(v5, nil, releaseDigest(release))
	if err != nil || verified.ExportVersion() != 5 || !bytes.Equal(verified.Signatures(), sidecar) {
		t.Fatal(err, verified.ExportVersion())
	}
	if r := verified.CheckSignature(mustKey(t, vectorKey1)); r.Status != SignatureSigned || r.FindingsTotal != 0 || r.KeyID != vectorKeyID1 || r.ReadableLines != 1 {
		t.Fatalf("%+v", r)
	}
	if r := verified.CheckSignature(mustKey(t, vectorKey2)); r.Status != SignatureInvalid || r.Findings[0].Name != FindingSignatureInvalid {
		t.Fatalf("%+v", r)
	}
	// Version 4 is version 5 without the sidecar, as it was before.
	var b VerificationBundle
	if err = json.Unmarshal(v5, &b); err != nil {
		t.Fatal(err)
	}
	b.Version, b.Run.AuditSignatures = 4, nil
	var again bytes.Buffer
	if err = json.NewEncoder(&again).Encode(b); err != nil || !bytes.Equal(again.Bytes(), get(t, h, "/v1/runs/"+run.ID+"/verification?version=4", 200)) {
		t.Fatal("version 4 of a signed run is not version 5 without its sidecar", err)
	}
	// The Runtime's own audit verify reads the attempt's trail and sidecar as
	// signed under the key.
	public := filepath.Join(t.TempDir(), "decisions.pub")
	if err = os.WriteFile(public, []byte(vectorKey1+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "audit", "verify", "--trail", filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "evaluations.jsonl"), "--public-key", public, "--require-signed-through", "1", "--format", "json")
	cmd.Env, cmd.Dir = []string{"LANG=C", "LC_ALL=C"}, t.TempDir()
	out, err := cmd.Output()
	var report struct {
		Status   string `json:"status"`
		Coverage struct {
			SignedRecords int `json:"signedRecords"`
			Signed        struct {
				Status  string `json:"status"`
				Through int64  `json:"through"`
			} `json:"signed"`
		} `json:"coverage"`
	}
	if err != nil || json.Unmarshal(out, &report) != nil || report.Status != "valid" || report.Coverage.SignedRecords != 1 || report.Coverage.Signed.Through != 1 {
		t.Fatalf("the Runtime does not read the attempt's record as signed: %v %s", err, out)
	}
}

// A version-5 export carries the record's signature sidecar, not empty and at
// most the 16 KiB Runner keeps, and earlier versions carry none. Version 5 is
// held to the 8 MiB version 2 is held to, beside its members.
func TestVersion5CarriesTheSidecarAndNothingElseDoes(t *testing.T) {
	c := chainedFixture(t, 0, 0)
	var b VerificationBundle
	if err := json.Unmarshal(c.export(t, "?version=4"), &b); err != nil {
		t.Fatal(err)
	}
	sidecar := []byte(vectorLine1 + "\n")
	for name, want := range map[string]struct {
		version int
		sidecar []byte
		refusal string
	}{
		"version 5 with its sidecar":   {5, sidecar, ""},
		"version 5 with the most kept": {5, bytes.Repeat([]byte("x"), maxSidecar), ""},
		"version 5 with more":          {5, bytes.Repeat([]byte("x"), maxSidecar+1), "the record's signature sidecar exceeds 16384 bytes"},
		"version 5 without a sidecar":  {5, nil, "a version-5 export carries the signature sidecar of the run's audit record"},
		"version 4 with a sidecar":     {4, sidecar, "a version-4 export carries no record signatures"},
		"version 3 with a sidecar":     {3, sidecar, "a version-3 export carries no entry of the installation's chain of runs"},
		"version 4 without, as it was": {4, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			changed := b
			changed.Version, changed.Run.AuditSignatures = want.version, want.sidecar
			v, err := VerifyInputs(encode(changed), c.profiles, c.release)
			if want.refusal == "" && (err != nil || v.ExportVersion() != want.version || !bytes.Equal(v.Signatures(), want.sidecar)) || want.refusal != "" && (err == nil || err.Error() != want.refusal) {
				t.Fatal(err)
			}
		})
	}
	// Version 5 carries the run's entry, as version 4 does.
	unchained := b
	unchained.Version, unchained.Run.AuditSignatures, unchained.Chain = 5, sidecar, nil
	if _, err := VerifyInputs(encode(unchained), c.profiles, c.release); err == nil || err.Error() != "a version-5 export carries its run's entry in the installation's chain of runs" {
		t.Fatal(err)
	}
	b.Version, b.Run.AuditSignatures = 5, sidecar
	encoded := encode(b)
	beside := len(`,"auditBytes":""`) + len(encode(b.Run.AuditBytes)) - 2 + len(`,"chain":`) + len(encode(b.Chain)) + len(`,"auditSignatures":""`) + len(encode(sidecar)) - 2
	pad := func(to int) []byte {
		return append(bytes.Clone(encoded), bytes.Repeat([]byte(" "), to-len(encoded))...)
	}
	if _, err := VerifyInputs(pad(8<<20+beside), c.profiles, c.release); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyInputs(pad(8<<20+beside+1), c.profiles, c.release); err == nil || err.Error() != "verification bundle exceeds 8 MiB" {
		t.Fatal(err)
	}
	// Under no key, a version-5 sidecar is reported, not checked; under a
	// key, an export without one is unsigned, never invalid.
	v, err := VerifyInputs(encoded, c.profiles, c.release)
	if err != nil {
		t.Fatal(err)
	}
	if r := v.CheckSignature(nil); r.Status != SignatureNotChecked || r.ReadableLines != 1 || r.KeyID != "" {
		t.Fatalf("%+v", r)
	}
	if r := c.verified(t).CheckSignature(mustKey(t, vectorKey1)); r.Status != SignatureUnsigned || r.FindingsTotal != 0 || r.PublicKey != vectorKey1 {
		t.Fatalf("%+v", r)
	}
}

// A sidecar line signing the record, ended by its newline as every line the
// Runtime writes is, is kept and exported byte for byte, newline included, and
// verifies as signed. A Runtime that signs is not needed: the test signs the
// attempt itself, with the guide's first seed, as a Runtime given that key
// would, so this runs on any Runtime. Without its newline the line would be a
// torn one, which signs nothing.
func TestASignatureLineIsKeptWithItsNewline(t *testing.T) {
	cfg := testConfig(t)
	log := filepath.Join(t.TempDir(), "signing.log")
	cfg.Runtime = signingRuntime(t, cfg.Runtime, log)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob("Signed here", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "one", input)
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, s, run.ID)
	sidecar, err := os.ReadFile(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "signatures.jsonl"))
	if err != nil || done.State != "completed" {
		helper, _ := os.ReadFile(log)
		t.Fatalf("%s %s %v: %s", done.State, done.Problem, err, helper)
	}
	if !bytes.HasSuffix(sidecar, []byte("\n")) || bytes.Count(sidecar, []byte("\n")) != 1 || !bytes.Equal(done.AuditSignatures, sidecar) {
		t.Fatalf("the sidecar's line is not kept exactly, newline included: kept %q, written %q", done.AuditSignatures, sidecar)
	}
	h := s.Handler("test")
	member := []byte(`"auditSignatures":"` + base64.StdEncoding.EncodeToString(sidecar) + `"`)
	if !bytes.Contains(get(t, h, "/v1/runs/"+run.ID, 200), member) {
		t.Fatal("the run does not show its sidecar exactly")
	}
	v5 := get(t, h, "/v1/runs/"+run.ID+"/verification?version=5", 200)
	if !bytes.Contains(v5, member) {
		t.Fatal("version 5 does not carry the sidecar exactly")
	}
	verified, err := VerifyInputs(v5, nil, releaseDigest(release))
	if err != nil || verified.ExportVersion() != 5 || !bytes.Equal(verified.Signatures(), sidecar) {
		t.Fatal(err)
	}
	if r := verified.CheckSignature(mustKey(t, vectorKey1)); r.Status != SignatureSigned || r.ReadableLines != 1 || r.UnreadableLines != 0 || r.FindingsTotal != 0 {
		t.Fatalf("%+v", r)
	}
	if r := verified.CheckSignature(mustKey(t, vectorKey2)); r.Status != SignatureInvalid {
		t.Fatalf("%+v", r)
	}
}
