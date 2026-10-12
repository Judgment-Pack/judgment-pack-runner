package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeTakesBinding says whether the Runtime at bin takes a binding, from
// the release it names, as Runner tells it; the test fails if it cannot be
// told.
func runtimeTakesBinding(t *testing.T, bin string) bool {
	t.Helper()
	status, err := bindingStatusOf(context.Background(), bin, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return status == RunBound
}

// bindingConfig is testConfig with a Runtime that takes a binding: the test is
// skipped otherwise. CI's pin does, and the test must run there.
func bindingConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	if !runtimeTakesBinding(t, cfg.Runtime) {
		t.Skip("JPACK_TEST_BIN takes no binding: set it to a Runtime of 0.31.0 or later")
	}
	return cfg
}

// loggingRuntime is the Runtime at real, run with its arguments written first
// to log, one invocation a line. Its answers are the Runtime's.
func loggingRuntime(t *testing.T, real, log string) string {
	t.Helper()
	real, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	path := filepath.Join(t.TempDir(), "logging-runtime")
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// shellRuntime is a script around the Runtime at real: body runs first in the
// attempt's directory, with real in $real, and decides what is run.
func shellRuntime(t *testing.T, real, body string) string {
	t.Helper()
	real, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nPATH=/usr/bin:/bin\nexport PATH\nreal='" + real + "'\n" + body + "\n"
	path := filepath.Join(t.TempDir(), "shell-runtime")
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// boundRun submits one run of input to a new job of the triage pack, with
// input as the release's sample, and waits for it to end.
func boundRun(t *testing.T, s *Service, input Input) (Release, Run) {
	t.Helper()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.createJob("Bound", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "one", input)
	if err != nil {
		t.Fatal(err)
	}
	return release, waitRun(t, s, run.ID)
}

// calculatedTriage is the calculated run calculatedSignedExport makes: a
// calculator that computed, whose answer a copy maps to the triage pack's
// request and two evidence requirements.
func calculatedTriage(t *testing.T) (Input, InputProfile) {
	t.Helper()
	input, profile, key, at := calculatorFixture(t)
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{
		Facts:    []FactMapping{{"/request", "/facts/request"}},
		Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}},
	}}
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	result := map[string]any{"calculation": calculation(at)}
	_ = json.Unmarshal(encode(sample()), &result)
	input.Source.Sources["fx"] = SourceValue{Response: signResponse(t, profile, key, []byte(calculatorArgs), encode(result), at, 0)}
	return input, profile
}

// asserted is the triage case every run here maps.
const asserted = `{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`

// The digest Runner binds is the SHA-256 of the preparation in the canonical
// form a frozen release is digested by, written out here by hand: members
// sorted by name, no insignificant whitespace, numbers as written, and &, <
// and > escaped as Go's encoding/json escapes them. Not the struct's own
// order, and not the bytes encode writes. The binding's document is exactly
// what the Runtime records.
func TestThePreparationsDigestIsTheSHA256OfItsCanonicalForm(t *testing.T) {
	p := &Preparation{Version: 2, VerifiedAt: "2026-10-11T12:00:00Z", MappingDigest: "sha256:" + strings.Repeat("ab", 32), Verification: "a&b <c>", Cites: []Citation{{SessionID: "s", CallIndex: 1, Signature: "f0"}}, Lineage: []TargetLineage{}, Outcomes: []SourceOutcome{{Name: "case", Status: "resolved", Reason: "r", Parameters: json.RawMessage(`{ "runAt" : "2026-10-11T12:00:00Z", "amount": 1200.50 }`), Claim: json.RawMessage(`{}`)}}}
	written := `{"cites":[{"callIndex":1,"sessionId":"s","signature":"f0"}],"lineage":[],"mappingDigest":"sha256:` + strings.Repeat("ab", 32) + `","outcomes":[{"claim":{},"name":"case","parameters":{"amount":1200.50,"runAt":"2026-10-11T12:00:00Z"},"reason":"r","status":"resolved"}],"verification":"a\u0026b \u003cc\u003e","verifiedAt":"2026-10-11T12:00:00Z","version":2}`
	sum := sha256.Sum256([]byte(written))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := preparationDigest(p); got != want {
		t.Fatalf("%s, not the SHA-256 of\n%s", got, written)
	}
	if digest(encode(p)) == want {
		t.Fatal("the fixture's encoding is already canonical: it shows nothing")
	}
	if got := string(bindingDocument(want)); got != `{"kind":"judgment-pack-runner/preparation/1","digest":"`+want+`"}` {
		t.Fatal(got)
	}
}

// The record's binding is read as recorded, by its exact name, and compared
// byte for byte with the binding Runner passes for the export's preparation:
// matches only when it is that binding. One of another kind is foreign
// whatever its digest, the preparation's included: no digest is compared. One
// of Runner's kind otherwise is a mismatch, Runner's own binding with its
// members reordered or its slashes escaped among them: the member is never
// canonicalised again. A record with none says absent, a version-2 export's
// record is read as it carries it, and a record whose members cannot be read,
// or that names a member binding but for case, is refused rather than taken
// for one with none.
func TestTheRecordsBindingIsReadAsRecorded(t *testing.T) {
	p := &Preparation{Version: 2, VerifiedAt: "2026-10-11T12:00:00Z", MappingDigest: "sha256:" + strings.Repeat("0", 64), Verification: "v", Cites: []Citation{}, Lineage: []TargetLineage{}, Outcomes: []SourceOutcome{}}
	ours := string(bindingDocument(preparationDigest(p)))
	record := func(binding string) []byte {
		if binding == "" {
			return []byte(`{"recordVersion":"1","kind":"evaluation","cites":[],"disposition":{}}`)
		}
		return []byte(`{"recordVersion":"1","kind":"evaluation","cites":[],"binding":` + binding + `,"disposition":{}}`)
	}
	check := func(version int, line []byte) (*BindingReport, error) {
		b := VerificationBundle{Version: version, Run: Run{Input: Input{Preparation: p}}}
		if version >= 3 {
			b.Run.AuditBytes = line
		} else {
			b.Run.Audit = line
		}
		return checkBinding(b)
	}
	for name, c := range map[string]struct {
		binding string
		want    string
	}{
		"Runner's binding": {ours, BindingMatches},
		"none":             {"", BindingAbsent},
		"another scheme, the preparation's digest":  {strings.Replace(ours, PreparationBindingKind, "another-companion/preparation/1", 1), BindingForeignKind},
		"another scheme, another digest":            {`{"kind":"another-companion/preparation/1","digest":"sha256:` + strings.Repeat("1", 64) + `"}`, BindingForeignKind},
		"no kind":                                   {`{"digest":"` + preparationDigest(p) + `"}`, BindingForeignKind},
		"a kind that is not a string":               {`{"kind":1,"digest":"` + preparationDigest(p) + `"}`, BindingForeignKind},
		"not an object":                             {`"` + PreparationBindingKind + `"`, BindingForeignKind},
		"null":                                      {`null`, BindingForeignKind},
		"Runner's scheme, another digest":           {`{"kind":"judgment-pack-runner/preparation/1","digest":"sha256:` + strings.Repeat("1", 64) + `"}`, BindingMismatch},
		"Runner's binding, its members reordered":   {`{"digest":"` + preparationDigest(p) + `","kind":"judgment-pack-runner/preparation/1"}`, BindingMismatch},
		"Runner's binding, its slashes escaped":     {strings.ReplaceAll(ours, "/", `\/`), BindingMismatch},
		"Runner's binding, spaced":                  {strings.Replace(ours, ",", ", ", 1), BindingMismatch},
		"Runner's binding, with a member more":      {strings.TrimSuffix(ours, "}") + `,"note":"x"}`, BindingMismatch},
		"Runner's binding, its digest in uppercase": {strings.Replace(ours, preparationDigest(p)[7:], strings.ToUpper(preparationDigest(p)[7:]), 1), BindingMismatch},
	} {
		for _, version := range []int{2, 5} {
			got, err := check(version, record(c.binding))
			if err != nil || got == nil || got.Status != c.want {
				t.Fatalf("%s, version %d: %+v %v", name, version, got, err)
			}
		}
	}
	if got, err := check(2, nil); err != nil || got.Status != BindingAbsent {
		t.Fatal("a run with no record:", got, err)
	}
	for name, line := range map[string][]byte{
		"binding named but for case": []byte(`{"recordVersion":"1","Binding":` + ours + `}`),
		"binding named twice":        []byte(`{"recordVersion":"1","binding":` + ours + `,"binding":` + ours + `}`),
		"not an object":              []byte(`[]`),
	} {
		if got, err := check(5, line); err == nil || got != nil {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if _, err := check(5, []byte(`{"BINDING":`+ours+`}`)); !errors.Is(err, ErrBindingMemberName) {
		t.Fatal(err)
	}
}

// An operational evaluation of a mapping-v2 run, by a Runtime that takes a
// binding (0.31.0 and later), is given the preparation's binding as a file in
// the attempt's private directory, and the record carries it byte for byte:
// the run says bound, as served. The release's preview, a rehearsal, is given
// none, and neither is a re-execution at verification. The Runtime is asked
// its release once, before the evaluation. The export's binding matches.
func TestAnOperationalEvaluationBindsItsPreparation(t *testing.T) {
	cfg := bindingConfig(t)
	log := filepath.Join(t.TempDir(), "invocations")
	cfg.Runtime = loggingRuntime(t, cfg.Runtime, log)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, run := boundRun(t, s, caseRun(asserted))
	if run.State != "completed" || run.Binding == nil || run.Binding.Status != RunBound {
		t.Fatal(run.State, run.Problem, run.Binding)
	}
	want := bindingDocument(preparationDigest(run.Input.Preparation))
	path := filepath.Join(cfg.Dir, "attempts", run.ID, bindingFile)
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, want) || privateFile(path) != nil {
		t.Fatalf("the binding file is not the binding, private: %s %v", written, err)
	}
	line := trailLine(t, cfg.Dir, run.ID)
	if bytes.Count(line, []byte(`"binding":`)) != 1 || !bytes.Contains(line, append([]byte(`"binding":`), want...)) {
		t.Fatalf("the record does not carry the binding as passed: %s", line)
	}
	if served := get(t, s.Handler("test"), "/v1/runs/"+run.ID, 200); !bytes.Contains(served, []byte(`"binding":{"status":"bound"}`)) {
		t.Fatalf("the served run does not say it is bound: %s", served)
	}
	raw := get(t, s.Handler("test"), "/v1/runs/"+run.ID+"/verification?version=4", 200)
	verified, err := VerifyInputs(raw, nil, releaseDigest(release))
	if err != nil || verified.Binding == nil || verified.Binding.Status != BindingMatches {
		t.Fatal(err, verified.Binding)
	}
	t.Setenv("TMPDIR", t.TempDir())
	if err = verified.Disposition(context.Background(), cfg.Runtime); err != nil {
		t.Fatal(err)
	}
	invocations, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var versions, bound, rehearsals int
	for _, args := range strings.Split(strings.TrimSuffix(string(invocations), "\n"), "\n") {
		switch {
		case args == "version --format json":
			versions++
		case strings.HasPrefix(args, "experimental evaluate ") && strings.Contains(args, "--rehearsal"):
			rehearsals++
			if strings.Contains(args, "--binding") {
				t.Fatalf("a rehearsal was given a binding: %s", args)
			}
		case strings.HasPrefix(args, "experimental evaluate "):
			bound++
			if !strings.HasSuffix(args, " --binding binding.json") {
				t.Fatalf("the operational evaluation was not given the binding: %s", args)
			}
		}
	}
	if versions != 1 || bound != 1 || rehearsals != 2 {
		t.Fatalf("%d version, %d operational and %d rehearsal invocations:\n%s", versions, bound, rehearsals, invocations)
	}
}

// A Runtime without the binding (before 0.31.0) is given none: the
// evaluation completes as before, its record carries none, and the run says
// its Runtime took none. The export's binding is absent, and its freshness
// is still named.
func TestARuntimeWithoutTheBindingIsGivenNone(t *testing.T) {
	cfg := testConfig(t)
	earlier := os.Getenv("JPACK_EARLIER_TEST_BIN")
	if earlier == "" {
		t.Skip("set JPACK_EARLIER_TEST_BIN to a Runtime before 0.31.0")
	}
	if runtimeTakesBinding(t, earlier) {
		t.Fatal("JPACK_EARLIER_TEST_BIN must be a Runtime before 0.31.0")
	}
	log := filepath.Join(t.TempDir(), "invocations")
	cfg.Runtime = loggingRuntime(t, earlier, log)
	input, p := calculatedTriage(t)
	cfg.InputProfiles = []InputProfile{p}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, run := boundRun(t, s, input)
	if run.State != "completed" || run.Binding == nil || run.Binding.Status != RunBindingUnsupported {
		t.Fatal(run.State, run.Problem, run.Binding)
	}
	if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, bindingFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a binding file was written for a Runtime that takes none:", err)
	}
	if line := trailLine(t, cfg.Dir, run.ID); bytes.Contains(line, []byte(`"binding"`)) {
		t.Fatalf("the record carries a binding: %s", line)
	}
	if served := get(t, s.Handler("test"), "/v1/runs/"+run.ID, 200); !bytes.Contains(served, []byte(`"binding":{"status":"unsupported-runtime"}`)) {
		t.Fatalf("the served run does not say its Runtime took no binding: %s", served)
	}
	if invocations, _ := os.ReadFile(log); bytes.Contains(invocations, []byte("--binding")) {
		t.Fatalf("a Runtime that takes no binding was given one:\n%s", invocations)
	}
	verified, err := VerifyInputs(get(t, s.Handler("test"), "/v1/runs/"+run.ID+"/verification?version=4", 200), []InputProfile{p}, releaseDigest(release))
	if err != nil || verified.Binding == nil || verified.Binding.Status != BindingAbsent || verified.Freshness == nil {
		t.Fatal(err, verified.Binding, verified.Freshness)
	}
}

// A binding the Runtime refuses is Runner's defect, never a silent drop: the
// run fails with that said, and its reason, binding-refused; the Runtime's
// refusal is kept as the attempt's diagnostics; nothing is recorded, and the
// run is not evaluated again without the binding.
func TestABindingTheRuntimeRefusesFailsTheRun(t *testing.T) {
	cfg := bindingConfig(t)
	log := filepath.Join(t.TempDir(), "invocations")
	// The binding file is spoiled, after Runner wrote it, before the Runtime
	// reads it.
	cfg.Runtime = shellRuntime(t, cfg.Runtime, "printf '%s\\n' \"$*\" >> '"+log+"'\n[ -f binding.json ] && printf '{\"kind\":\"Not A Kind\",\"digest\":\"sha256:0\"}' > binding.json\nexec \"$real\" \"$@\"")
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, run := boundRun(t, s, caseRun(asserted))
	if run.State != "failed" || run.Problem != errBindingRefused.Error() || run.Reason != runBindingRefused || run.Binding != nil || run.Result != nil || run.AuditBytes != nil {
		t.Fatal(run.State, run.Problem, run.Reason, run.Binding)
	}
	if served := get(t, s.Handler("test"), "/v1/runs/"+run.ID, 200); !bytes.Contains(served, []byte(`"reason":"binding-refused"`)) || bytes.Contains(served, []byte(`"binding":`)) {
		t.Fatalf("the served run does not say why it failed: %s", served)
	}
	if stdout := get(t, s.Handler("test"), "/v1/runs/"+run.ID+"/diagnostics?stream=stdout", 200); !bytes.Contains(stdout, []byte("JPS-INVOCATION-BINDING")) {
		t.Fatalf("the Runtime's refusal is not kept: %s", stdout)
	}
	if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, "audit")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refused evaluation recorded something:", err)
	}
	invocations, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if operational := strings.Count(string(invocations), "experimental evaluate ") - strings.Count(string(invocations), "--rehearsal"); operational != 1 {
		t.Fatalf("%d operational evaluations:\n%s", operational, invocations)
	}
}

// A Runtime that names a release that takes a binding, and records none, or
// another, fails the run: the record must carry the binding Runner passed,
// byte for byte, as it must carry its citations. So does one that names a
// release that takes none, and records one all the same: a record must carry
// none when none was passed.
func TestARecordWithoutTheBindingPassedFailsTheRun(t *testing.T) {
	for name, body := range map[string]string{
		// The Runtime says it is 0.30.0, and binds a digest of its own on
		// the operational evaluation.
		"added": "if [ \"$1\" = version ]; then printf '{\"outputVersion\":\"2\",\"tool\":{\"name\":\"jpack\",\"version\":\"0.30.0\"}}\\n'; exit 0; fi\n" +
			"case \" $* \" in *\" --rehearsal \"*) ;; *\" experimental evaluate \"*) printf '{\"kind\":\"judgment-pack-runner/preparation/1\",\"digest\":\"sha256:" + strings.Repeat("0", 64) + "\"}' > added.json; exec \"$real\" \"$@\" --binding added.json;; esac\nexec \"$real\" \"$@\"",
		// The flag and its file are dropped from the evaluation.
		"dropped": "for a; do shift; if [ -n \"$skip\" ]; then skip=; continue; fi; if [ \"$a\" = --binding ]; then skip=1; continue; fi; set -- \"$@\" \"$a\"; done\nexec \"$real\" \"$@\"",
		// Another preparation's digest is bound in its place.
		"another": "[ -f binding.json ] && printf '{\"kind\":\"judgment-pack-runner/preparation/1\",\"digest\":\"sha256:" + strings.Repeat("0", 64) + "\"}' > binding.json\nexec \"$real\" \"$@\"",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := bindingConfig(t)
			cfg.Runtime = shellRuntime(t, cfg.Runtime, body)
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, run := boundRun(t, s, caseRun(asserted))
			if run.State != "failed" || run.Problem != errRecordBinding.Error() || run.Binding != nil || run.Reason != "" {
				t.Fatal(run.State, run.Problem, run.Binding, run.Reason)
			}
			if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "evaluations.jsonl")); err != nil {
				t.Fatal("the Runtime recorded nothing, so the record's binding was not what failed the run:", err)
			}
		})
	}
}

// A Runtime whose release cannot be read from its `version` answer evaluates
// nothing for a mapping-v2 run: whether it takes a binding cannot be told, and
// the run is not evaluated unbound in its place. A run without a mapping v2,
// which binds nothing, does not ask.
func TestARuntimeThatDoesNotSayItsReleaseEvaluatesNothing(t *testing.T) {
	for name, version := range map[string]string{
		"refused":   "exit 3",
		"no answer": "printf 'not JSON\\n'",
		"unnamed":   "printf '{\"tool\":{\"name\":\"jpack\",\"version\":\"dev\"}}\\n'",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Runtime = shellRuntime(t, cfg.Runtime, "if [ \"$1\" = version ]; then "+version+"; exit $?; fi\nexec \"$real\" \"$@\"")
			s, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, run := boundRun(t, s, caseRun(asserted))
			if run.State != "failed" || run.Problem != errRuntimeVersionUnread.Error() || run.Binding != nil {
				t.Fatal(run.State, run.Problem, run.Binding)
			}
			if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, "evaluation.stdout")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("the run was evaluated:", err)
			}
			release := testRelease(t, s)
			job, err := s.createJob("Unmapped", release.ID)
			if err != nil {
				t.Fatal(err)
			}
			plain, _, err := s.submit(job.ID, "plain", sample())
			if err != nil {
				t.Fatal(err)
			}
			if done := waitRun(t, s, plain.ID); done.State != "completed" || done.Binding != nil {
				t.Fatal(done.State, done.Problem, done.Binding)
			}
		})
	}
}

// What the Runtime's version answer says of the binding: 0.31.0 and later take
// one; earlier releases do not; and a build that names none (0.0.0-dev) is
// given none, and said to be unversioned.
func TestTheRuntimesReleaseSaysWhetherItTakesABinding(t *testing.T) {
	for version, want := range map[string]string{"0.31.0": RunBound, "0.31.1": RunBound, "0.40.0": RunBound, "1.0.0": RunBound, "0.31.0-rc.1": RunBound, "0.30.0": RunBindingUnsupported, "0.27.1": RunBindingUnsupported, "0.1.0": RunBindingUnsupported, "0.0.0-dev": RunBindingUnversioned, "0.0.0": RunBindingUnversioned} {
		bin := filepath.Join(t.TempDir(), "version")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '{\"outputVersion\":\"2\",\"tool\":{\"name\":\"jpack\",\"version\":\""+version+"\"}}\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if got, err := bindingStatusOf(context.Background(), bin, t.TempDir()); err != nil || got != want {
			t.Fatal(version, got, err)
		}
	}
}
