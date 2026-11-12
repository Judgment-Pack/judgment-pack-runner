package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Core version a pack declares selects its evaluator on ordinary runs as
// on selected ones (#188): 0.2.0-draft, 0.3.0 and 0.5.0 (#232) are admitted,
// anything else is refused by name, the unreleased working selector
// 0.4.0-draft among them, and nothing upgrades a pack to another Core.
func TestOrdinaryCoreVersionIsTheDeclaredOne(t *testing.T) {
	for _, version := range []string{"0.2.0-draft", "0.3.0", "0.5.0"} {
		if !supportedEvaluatorSpec(version, nil) {
			t.Fatal("ordinary evaluator refused", version)
		}
		r := Release{Pack: `{"specVersion":"` + version + `"}`}
		if expectedEvaluatorSpec(r) != version {
			t.Fatal("ordinary release is not evaluated under its declared Core", version, expectedEvaluatorSpec(r))
		}
	}
	for _, version := range []string{"", "0.2.0", "0.3.0-rc.2", "0.3.1", "0.4.0", "0.4.0-draft", "0.5.0-draft", "0.5.1", "0.5", "v0.5.0", "1.0.0", "0.3.0 ", "0.5.0 ", " 0.5.0"} {
		if supportedEvaluatorSpec(version, nil) {
			t.Fatal("unsupported Core admitted on an ordinary run", version)
		}
		if expectedEvaluatorSpec(Release{Pack: `{"specVersion":"` + version + `"}`}) != "" {
			t.Fatal("unsupported Core selected an evaluator", version)
		}
	}
	if expectedEvaluatorSpec(Release{Pack: `{}`}) != "" || expectedEvaluatorSpec(Release{Pack: `not json`}) != "" {
		t.Fatal("a pack that declares no Core selected an evaluator")
	}
	draft := Release{Pack: `{"specVersion":"0.2.0-draft"}`}
	if evaluatorSpecMatches(draft, "0.3.0") || !evaluatorSpecMatches(draft, "0.2.0-draft") {
		t.Fatal("a 0.2.0-draft release accepted a 0.3.0 evaluation")
	}
	core := Release{Pack: `{"specVersion":"0.3.0"}`}
	if evaluatorSpecMatches(core, "0.2.0-draft") || !evaluatorSpecMatches(core, "0.3.0") {
		t.Fatal("a 0.3.0 release accepted a 0.2.0-draft evaluation")
	}
	if evaluatorSpecMatches(draft, "0.5.0") || evaluatorSpecMatches(core, "0.5.0") {
		t.Fatal("a 0.2.0-draft or 0.3.0 release accepted a 0.5.0 evaluation")
	}
	released := Release{Pack: `{"specVersion":"0.5.0"}`}
	if !evaluatorSpecMatches(released, "0.5.0") {
		t.Fatal("a 0.5.0 release refused its own Core's evaluation")
	}
	for _, other := range []string{"0.2.0-draft", "0.3.0", "0.4.0-draft", "0.4.0", "", "0.5.0 "} {
		if evaluatorSpecMatches(released, other) {
			t.Fatalf("a 0.5.0 release accepted a %q evaluation", other)
		}
	}
	for declared, named := range map[string]string{"0.4.0": `"0.4.0"`, "": "declares none", strings.Repeat("9", 65): "another version"} {
		message := unsupportedEvaluatorSpec(declared, nil)
		if !strings.Contains(message, named) || !strings.Contains(message, "0.2.0-draft, 0.3.0 or 0.5.0") {
			t.Fatalf("refusal does not name the version: %q", message)
		}
	}
	// The working selector Spec v0.5.0 was released from, and the version it
	// would have been, are refused by name on an ordinary run.
	for declared, want := range map[string]string{
		"0.4.0-draft": `This runner evaluates packs that declare specVersion 0.2.0-draft, 0.3.0 or 0.5.0; this pack declares "0.4.0-draft".`,
		"0.4.0":       `This runner evaluates packs that declare specVersion 0.2.0-draft, 0.3.0 or 0.5.0; this pack declares "0.4.0".`,
	} {
		if got := unsupportedEvaluatorSpec(declared, nil); got != want {
			t.Fatalf("ordinary refusal of %s: %q", declared, got)
		}
	}
}

// The pack the Runtime is given is the release's, byte for byte, and its
// answer must name the declared Core: a 0.2.0-draft pack is never evaluated as
// 0.3.0, nor a 0.3.0 pack as 0.2.0-draft, a 0.5.0 pack is evaluated as 0.5.0
// and as nothing else, nor is anything else evaluated as 0.5.0, and no answer
// is accepted for a pack that declares a Core Runner does not evaluate.
func TestOrdinaryRunIsNeverEvaluatedUnderAnotherCore(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "runtime-is-never-started")
	for _, c := range []struct{ declared, answered string }{
		{"0.2.0-draft", "0.2.0-draft"}, {"0.3.0", "0.3.0"},
		{"0.2.0-draft", "0.3.0"}, {"0.3.0", "0.2.0-draft"}, {"0.4.0", "0.4.0"},
		{"0.5.0", "0.5.0"}, {"0.5.0", "0.3.0"}, {"0.5.0", "0.2.0-draft"}, {"0.5.0", "0.4.0-draft"},
		{"0.3.0", "0.5.0"}, {"0.2.0-draft", "0.5.0"}, {"0.4.0-draft", "0.4.0-draft"}, {"0.4.0-draft", "0.5.0"},
	} {
		pack := `{"id":"p","version":"1","specVersion":"` + c.declared + `"}`
		r := Release{Pack: pack, PackDigest: digest([]byte(pack)), PackID: "p", PackVersion: "1"}
		calls := 0
		invoker := func(_ context.Context, _, dir, _ string, _ []string, _ ...string) ([]byte, error) {
			calls++
			given, e := os.ReadFile(filepath.Join(dir, "pack.json"))
			if e != nil || string(given) != pack {
				t.Fatalf("the Runtime was not given the release's own pack: %s %v", given, e)
			}
			return []byte(`{"outputVersion":"2","status":"evaluated","packId":"p","packVersion":"1","evaluatorSpecVersion":"` + c.answered + `","rehearsal":true,"disposition":{"kind":"unresolved","reasons":["unknown"]}}`), nil
		}
		dir := filepath.Join(t.TempDir(), "evaluation")
		_, _, _, _, e := evaluateWithInvoker(context.Background(), bin, r, sample(), dir, true, signingInputs{}, invoker)
		switch {
		case c.declared == "0.4.0" || c.declared == "0.4.0-draft":
			if e == nil {
				t.Fatal("an evaluation of an unsupported Core was accepted")
			}
		case c.declared == c.answered:
			if e != nil || calls != 1 {
				t.Fatal("the declared Core was not evaluated", c.declared, e)
			}
		default:
			if e == nil {
				t.Fatalf("a %s pack was accepted as evaluated under %s", c.declared, c.answered)
			}
		}
	}
}

// verify-run holds the Core version a run's record names, in its result and
// in its audit record, to the version its release's pack declares. A record
// that names none keeps its meaning.
func TestVerifyRunHoldsTheRecordsCoreVersion(t *testing.T) {
	bundle := func(declared, result, audit string) VerificationBundle {
		pack := `{"specVersion":"` + declared + `"}`
		named := func(version string) json.RawMessage {
			if version == "" {
				return json.RawMessage(`{"disposition":{"kind":"unresolved"}}`)
			}
			return json.RawMessage(`{"evaluatorSpecVersion":"` + version + `","disposition":{"kind":"unresolved"}}`)
		}
		return VerificationBundle{Release: Release{Pack: pack}, Run: Run{State: "completed", Result: named(result), Audit: named(audit)}}
	}
	for _, c := range []struct {
		declared, result, audit string
		ok                      bool
	}{
		{"0.2.0-draft", "0.2.0-draft", "0.2.0-draft", true},
		{"0.3.0", "0.3.0", "0.3.0", true},
		{"0.2.0-draft", "", "", true},
		{"0.2.0-draft", "0.2.0-draft", "", true},
		{"0.2.0-draft", "0.3.0", "0.2.0-draft", false},
		{"0.2.0-draft", "0.2.0-draft", "0.3.0", false},
		{"0.3.0", "0.2.0-draft", "0.3.0", false},
		{"0.3.0", "0.3.0", "0.2.0-draft", false},
		{"0.4.0", "0.4.0", "0.4.0", false},
		{"0.5.0", "0.5.0", "0.5.0", true},
		{"0.5.0", "", "", true},
		{"0.5.0", "0.5.0", "", true},
		{"0.5.0", "0.3.0", "0.5.0", false},
		{"0.5.0", "0.5.0", "0.3.0", false},
		{"0.5.0", "0.4.0-draft", "0.5.0", false},
		{"0.5.0", "0.5.0", "0.2.0-draft", false},
		{"0.3.0", "0.5.0", "0.3.0", false},
		{"0.3.0", "0.3.0", "0.5.0", false},
		{"0.2.0-draft", "0.5.0", "0.2.0-draft", false},
		{"0.4.0-draft", "0.4.0-draft", "0.4.0-draft", false},
		{"0.4.0-draft", "0.5.0", "0.5.0", false},
	} {
		e := checkEvaluatorSpec(bundle(c.declared, c.result, c.audit))
		if (e == nil) != c.ok || (e != nil && !errors.Is(e, ErrEvaluatorSpec)) {
			t.Fatalf("declared %s, result %q, audit %q: %v", c.declared, c.result, c.audit, e)
		}
	}
	b := bundle("0.2.0-draft", "0.2.0-draft", "0.2.0-draft")
	b.Run.Audit = json.RawMessage(`{"evaluatorSpecVersion":"0.2.0-draft","EvaluatorSpecVersion":"0.3.0"}`)
	if e := checkEvaluatorSpec(b); e == nil || !strings.Contains(e.Error(), "member-name") {
		t.Fatal("a case sibling of evaluatorSpecVersion was not refused", e)
	}
	b.Run.Audit = json.RawMessage(`{"evaluatorSpecVersion":null}`)
	if checkEvaluatorSpec(b) == nil {
		t.Fatal("a null Core version was taken for the declared one")
	}
}

// Both Core versions run side by side in one service on ordinary mapped-source
// runs, through the pinned Runtime that evaluates Core 0.3.0: each run's
// record names the version its pack declares, verify-run verifies it, and a
// restart and a backup restored from a copy of the store keep it, for runs
// completed before and runs dispatched after. An unsupported version is
// refused by name, before any release exists.
func TestOrdinaryCoreVersionsRealRuntimeSideBySide(t *testing.T) {
	cfg := testConfig(t)
	bin := os.Getenv("JPACK_RETAINED_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_RETAINED_TEST_BIN to the pinned Runtime that evaluates Core 0.3.0 (scripts/checks/build-pinned.sh)")
	}
	cfg.Runtime = bin
	coreVersionsSideBySide(t, cfg, []string{"0.2.0-draft", "0.3.0"}, []string{"0.4.0", "0.3.0-rc.2", "0.2.0"}, triagePackOf(t))
}

// triagePackOf returns the handwritten triage pack declaring a given Core.
// Each Core's pack is its own artifact version: one identity and version
// names one pack's content. The 0.5.0 one is its own handwritten pack, which
// states its request types and its not-yet-judged assessments as value sets
// and carries annotations.
func triagePackOf(t *testing.T) func(string) string {
	t.Helper()
	draftPack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	declares := []byte(`"specVersion": "0.2.0-draft"`)
	if bytes.Count(draftPack, declares) != 1 {
		t.Fatal("handwritten pack no longer declares 0.2.0-draft once")
	}
	released, e := os.ReadFile("testdata/triage-0.5.0.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, member := range []string{`"specVersion": "0.5.0"`, `"version": "0.1.2"`, `"valueSets": [`, `"valueSet": "request-types"`, `"valueSet": "appropriateness-not-yet-judged"`, `"org.judgmentpack.annotations"`} {
		if !bytes.Contains(released, []byte(member)) {
			t.Fatal("handwritten Core 0.5.0 pack no longer has", member)
		}
	}
	return func(version string) string {
		if version == "0.5.0" {
			return string(released)
		}
		pack := bytes.Replace(draftPack, declares, []byte(`"specVersion": "`+version+`"`), 1)
		if version != "0.2.0-draft" {
			pack = bytes.Replace(pack, []byte(`"version": "0.1.0"`), []byte(`"version": "0.1.1"`), 1)
		}
		return string(pack)
	}
}

// sideBySideInput is an ordinary mapped-source input whose one source, signed
// by its profile, reads the triage sample's facts with facts and its evidence.
func sideBySideInput(t *testing.T, facts []FactMapping) (Input, InputProfile) {
	t.Helper()
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: facts, Evidence: []EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	return input, p
}

// coreVersionsSideBySide runs versions side by side in one service, on cfg's
// Runtime, and refuses each of refused by name before any release exists. It
// returns the release of each version.
func coreVersionsSideBySide(t *testing.T, cfg Config, versions, refused []string, packOf func(string) string) map[string]Release {
	t.Helper()
	cfg.SigningKey = signingKey(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	input, p := sideBySideInput(t, []FactMapping{{Target: "/request", Source: "/facts/request"}})
	cfg.InputProfiles = []InputProfile{p}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	if !runtimeTakesBinding(t, cfg.Runtime) {
		t.Fatal("the pinned Runtime must take a preparation binding")
	}
	for _, version := range refused {
		_, e := s.preview(context.Background(), PreviewRequest{Pack: packOf(version), Input: input})
		var refusal *apiError
		if !errors.As(e, &refusal) || refusal.Code != "invalid_pack" || !strings.Contains(refusal.Message, fmt.Sprintf("%q", version)) {
			t.Fatalf("unsupported Core %s was not refused by name: %v", version, e)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.Dir, "releases")); len(entries) != 0 {
		t.Fatal("a refused Core left a release")
	}
	releases := map[string]Release{}
	jobs := map[string]string{}
	runs := map[string][]string{}
	for _, version := range versions {
		release, e := s.preview(context.Background(), PreviewRequest{Pack: packOf(version), Input: input})
		if e != nil {
			t.Fatal(version, e)
		}
		if got := string(member(release.Preview, "evaluatorSpecVersion")); got != `"`+version+`"` || expectedEvaluatorSpec(release) != version {
			t.Fatalf("the %s release was previewed under %s", version, got)
		}
		releases[version] = release
		job, e := s.createJob("Core "+version, release.ID)
		if e != nil {
			t.Fatal(e)
		}
		jobs[version] = job.ID
	}
	// Both releases' runs are queued in one service before either is
	// dispatched, and dispatched in turn by the same dispatcher.
	submit := func(s *Service, key string) {
		for _, version := range versions {
			run, replayed, e := s.submit(jobs[version], key+"-"+version, input)
			if e != nil || replayed {
				t.Fatal(version, e)
			}
			runs[version] = append(runs[version], run.ID)
		}
	}
	dispatch := func(s *Service) {
		for range versions {
			if worked, e := s.dispatchNext(context.Background()); e != nil || !worked {
				t.Fatal("dispatch", worked, e)
			}
		}
	}
	// check holds every run of each release to its declared Core, in the
	// stored run and its record, and verifies each one's export offline.
	check := func(s *Service, stage string) {
		t.Helper()
		for _, version := range versions {
			release, e := s.release(releases[version].ID)
			if e != nil || release.Pack != releases[version].Pack || expectedEvaluatorSpec(release) != version {
				t.Fatalf("%s: the %s release lost its pack: %v", stage, version, e)
			}
			for _, id := range runs[version] {
				run := waitRun(t, s, id)
				if run.State != "completed" || run.Signing == nil || run.Signing.Status != SigningSigned {
					t.Fatalf("%s: %s run %s did not complete signed: %s %s", stage, version, id, run.State, run.Problem)
				}
				named := `"` + version + `"`
				if string(member(run.Result, "evaluatorSpecVersion")) != named || string(member(run.Audit, "evaluatorSpecVersion")) != named || string(member(run.Audit, "pack", "specVersion")) != named {
					t.Fatalf("%s: a %s run's record names %s, %s", stage, version, member(run.Result, "evaluatorSpecVersion"), member(run.Audit, "evaluatorSpecVersion"))
				}
				req := httptest.NewRequest("GET", "/v1/runs/"+id+"/verification?version=5", nil)
				req.Header.Set("Authorization", "Bearer test")
				w := httptest.NewRecorder()
				s.Handler("test").ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatalf("%s: export %d %.500s", stage, w.Code, w.Body.String())
				}
				verified, e := VerifyInputs(w.Body.Bytes(), cfg.InputProfiles, releaseDigest(release))
				if e != nil || verified.ExportVersion() != 5 || verified.CheckSignature(s.expectedKey).Status != SignatureSigned {
					t.Fatalf("%s: verify-run of a %s run: %v", stage, version, e)
				}
				// A record that names the other Core is refused offline.
				other := versions[0]
				if version == other {
					other = versions[1]
				}
				var b VerificationBundle
				if e = json.Unmarshal(w.Body.Bytes(), &b); e != nil {
					t.Fatal(e)
				}
				changed := bytes.Replace(b.Run.Result, []byte(`"evaluatorSpecVersion":`+named), []byte(`"evaluatorSpecVersion":"`+other+`"`), 1)
				if bytes.Equal(changed, b.Run.Result) {
					t.Fatalf("%s: the exported result does not name %s compactly", stage, version)
				}
				b.Run.Result = changed
				if _, e = VerifyInputs(encode(b), cfg.InputProfiles, releaseDigest(release)); !errors.Is(e, ErrEvaluatorSpec) {
					t.Fatalf("%s: a %s run whose result names %s verified: %v", stage, version, other, e)
				}
			}
		}
	}
	submit(s, "first")
	dispatch(s)
	check(s, "completed")
	// Restart: runs queued before the stop are dispatched after it under the
	// Core their release declares.
	submit(s, "before-restart")
	s.Close()
	if s, e = Open(cfg); e != nil {
		t.Fatal(e)
	}
	dispatch(s)
	check(s, "restart")
	// Backup and restore: a copy of the stopped store, with runs queued in
	// it, restored in place of the store.
	submit(s, "before-backup")
	s.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if e = copyStore(cfg.Dir, backup); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(cfg.Dir); e != nil {
		t.Fatal(e)
	}
	if e = copyStore(backup, cfg.Dir); e != nil {
		t.Fatal(e)
	}
	if s, e = Open(cfg); e != nil {
		t.Fatal(e)
	}
	dispatch(s)
	check(s, "restore")
	if worked, e := s.dispatchNext(context.Background()); e != nil || worked {
		t.Fatal("a run was left or repeated", worked, e)
	}
	return releases
}

// copyStore copies a stopped store's directory, as an operator's backup does:
// every file with its private permissions.
func copyStore(from, to string) error {
	return filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected store entry %s", rel)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

// The version answers of three Runtimes, written by hand as each writes it:
// 0.44.0, the first that reads and evaluates Core 0.5.0; 0.43.0, which listed
// its working selector 0.4.0-draft as a staged evaluator capability; and
// 0.41.0, which reads and evaluates Core 0.3.0 at most.
const (
	versionReadsCore050  = `{"evaluatorCapabilities":[{"specVersion":"0.2.0-draft"},{"specVersion":"0.3.0","supportedProfiles":["base"]},{"specVersion":"0.5.0","supportedProfiles":["base"]}],"outputVersion":"2","tool":{"name":"jpack","version":"0.44.0"},"command":"version","status":"valid","supportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0","0.5.0"],"artifactProvenance":"immutable-git-ref"}`
	versionStagesDraft   = `{"evaluatorCapabilities":[{"specVersion":"0.2.0-draft"},{"specVersion":"0.3.0","supportedProfiles":["base"]},{"specVersion":"0.4.0-draft","supportedProfiles":["base"],"stage":"staged-unreleased"}],"outputVersion":"2","tool":{"name":"jpack","version":"0.43.0"},"command":"version","status":"valid","supportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0","0.4.0-draft"],"artifactProvenance":"immutable-git-ref"}`
	versionReadsCore030  = `{"evaluatorCapabilities":[{"specVersion":"0.2.0-draft"},{"specVersion":"0.3.0","supportedProfiles":["base"]}],"outputVersion":"2","tool":{"name":"jpack","version":"0.41.0"},"command":"version","status":"valid","supportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0"],"artifactProvenance":"immutable-git-ref"}`
	core050JournalPack   = `{"id":"journal","version":"1.0.0","title":"Journal","specVersion":"0.5.0"}`
	core030JournalPack   = `{"id":"journal","version":"1.0.0","title":"Journal","specVersion":"0.3.0"}`
	core050Unsupported   = `{"error":{"code":"runtime_core_unsupported","message":"This pack declares specVersion \"0.5.0\", and the pinned Runtime does not say that it reads and evaluates it: its version answer must list 0.5.0 among its supportedSpecVersions and as an evaluator capability that is not staged and supports the base profile. Runtime 0.44.0 and later do. Pin such a Runtime, or check a pack that declares 0.2.0-draft or 0.3.0.","retryable":false}}` + "\n"
	core050AskedNoPack   = "version --format json | none\n"
	core050ReleaseAnswer = `{"outputVersion":"2","status":"evaluated","packId":"journal","packVersion":"1.0.0","evaluatorSpecVersion":"%s","rehearsal":true,"disposition":` + journalDisposition + `}`
)

// versionRuntime is a Runtime that answers version with answer and exits
// exit, checks a release as the Runtime does, answering its rehearsal under
// the Core the pack it is given declares, and logs each invocation with the
// pack it was given, or none.
func versionRuntime(t *testing.T, answer string, exit int, log string) string {
	t.Helper()
	script := `#!/bin/sh
PATH=/usr/bin:/bin
export PATH
umask 077
pack=none
if [ -f pack.json ]; then pack=$(cat pack.json); fi
printf '%s | %s\n' "$*" "$pack" >> '` + log + `'
case "$1 $2" in
'version --format')
	printf '%s\n' '` + answer + `'
	exit ` + strconv.Itoa(exit) + ` ;;
'spec validate')
	printf '%s\n' '{"outputVersion":"2","status":"valid"}' ;;
'packs lock')
	printf '%s' '` + checkLock + `' > jpack.lock.json
	printf '%s\n' '{"outputVersion":"2","status":"locked"}' ;;
'experimental evaluate')
	printf '` + core050ReleaseAnswer + `\n' "$(sed -n 's/.*"specVersion":"\([^"]*\)".*/\1/p' pack.json)" ;;
*)
	exit 9 ;;
esac
`
	path := filepath.Join(t.TempDir(), "version-runtime")
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}

func versionService(t *testing.T, answer string, exit int) (*Service, string) {
	t.Helper()
	cfg := journalConfig(t)
	log := filepath.Join(t.TempDir(), "invocations.log")
	cfg.Runtime = versionRuntime(t, answer, exit, log)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	return s, log
}

func previewOf(pack string) string {
	return string(encode(PreviewRequest{Pack: pack, Input: journalInput()}))
}

// A pack that declares Core 0.5.0 is checked only by a Runtime whose own
// version answer says that it reads and evaluates 0.5.0, asked before the
// Runtime is given the pack (#232). Any other answer, or one that cannot be
// read, refuses the check with these bytes: no release is saved, and the
// Runtime is never given the pack, so a Runtime that does not read 0.5.0
// never reports it as a defect of the pack. The same Runtime checks a pack
// that declares 0.2.0-draft or 0.3.0 as before, without the question.
func TestCore050CheckedOnlyByARuntimeThatSaysItEvaluatesIt(t *testing.T) {
	entry := `{"specVersion":"0.5.0","supportedProfiles":["base"]}`
	documents := `"supportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0","0.5.0"]`
	for _, c := range []struct {
		name   string
		answer string
		exit   int
	}{
		{"a Runtime that staged the working selector", versionStagesDraft, 0},
		{"a Runtime that evaluates Core 0.3.0 at most", versionReadsCore030, 0},
		{"a staged 0.5.0 evaluator", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["base"],"stage":"staged-unreleased"}`, 1), 0},
		{"a 0.5.0 evaluator whose stage is null", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["base"],"stage":null}`, 1), 0},
		{"0.5.0 documents but no 0.5.0 evaluator", strings.Replace(versionReadsCore050, ","+entry, ``, 1), 0},
		{"an evaluator that only resembles 0.5.0", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0-draft","supportedProfiles":["base"]}`, 1), 0},
		{"an evaluator of 0.5.0 written with a space", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0 ","supportedProfiles":["base"]}`, 1), 0},
		{"no evaluator capabilities", strings.Replace(versionReadsCore050, `"evaluatorCapabilities":[{"specVersion":"0.2.0-draft"},{"specVersion":"0.3.0","supportedProfiles":["base"]},`+entry+`],`, ``, 1), 0},
		{"evaluator capabilities that are not an array", strings.Replace(versionReadsCore050, `[{"specVersion":"0.2.0-draft"},{"specVersion":"0.3.0","supportedProfiles":["base"]},`+entry+`]`, entry, 1), 0},
		{"a 0.5.0 evaluator whose documents are not listed", strings.Replace(versionReadsCore050, documents, `"supportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0"]`, 1), 0},
		{"no document versions", strings.Replace(versionReadsCore050, ","+documents, ``, 1), 0},
		{"document versions that are not strings", strings.Replace(versionReadsCore050, documents, `"supportedSpecVersions":"0.5.0"`, 1), 0},
		{"an answer whose Runtime exited with a failure", versionReadsCore050, 1},
		{"another outputVersion", strings.Replace(versionReadsCore050, `"outputVersion":"2"`, `"outputVersion":"1"`, 1), 0},
		{"another command's answer", strings.Replace(versionReadsCore050, `"command":"version"`, `"command":"packs schema"`, 1), 0},
		{"an answer that is not valid", strings.Replace(versionReadsCore050, `"status":"valid"`, `"status":"invalid"`, 1), 0},
		{"an answer that is not JSON", `usage: jpack version`, 0},
		{"an empty answer", ``, 2},
		// Read exactly (#233 review): a duplicated member is refused in
		// either order, and a member named as a read one but for case is not
		// read as it.
		{"document versions duplicated, the listing last", strings.Replace(versionReadsCore050, documents, `"supportedSpecVersions":["0.3.0"],`+documents, 1), 0},
		{"document versions duplicated, the listing first", strings.Replace(versionReadsCore050, documents, documents+`,"supportedSpecVersions":["0.3.0"]`, 1), 0},
		{"evaluator capabilities duplicated, the 0.5.0 one last", strings.Replace(versionReadsCore050, `{"evaluatorCapabilities":[`, `{"evaluatorCapabilities":[],"evaluatorCapabilities":[`, 1), 0},
		{"evaluator capabilities duplicated, the 0.5.0 one first", strings.Replace(versionReadsCore050, `"outputVersion":"2"`, `"outputVersion":"2","evaluatorCapabilities":[]`, 1), 0},
		{"a duplicated status, valid last", strings.Replace(versionReadsCore050, `"status":"valid"`, `"status":"invalid","status":"valid"`, 1), 0},
		{"a duplicated status, valid first", strings.Replace(versionReadsCore050, `"status":"valid"`, `"status":"valid","status":"invalid"`, 1), 0},
		{"an evaluator whose specVersion is duplicated, 0.5.0 last", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.3.0","specVersion":"0.5.0","supportedProfiles":["base"]}`, 1), 0},
		{"an evaluator whose specVersion is duplicated, 0.5.0 first", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","specVersion":"0.3.0","supportedProfiles":["base"]}`, 1), 0},
		{"an evaluator whose profiles are duplicated, base last", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":[],"supportedProfiles":["base"]}`, 1), 0},
		{"an evaluator whose specVersion is named SpecVersion", strings.Replace(versionReadsCore050, entry, `{"SpecVersion":"0.5.0","supportedProfiles":["base"]}`, 1), 0},
		{"an evaluator whose stage is named Stage", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["base"],"Stage":"staged-unreleased"}`, 1), 0},
		{"an evaluator whose profiles are named SupportedProfiles", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","SupportedProfiles":["base"]}`, 1), 0},
		{"document versions named SupportedSpecVersions", strings.Replace(versionReadsCore050, documents, `"SupportedSpecVersions":["0.1.0-draft","0.2.0-draft","0.3.0","0.5.0"]`, 1), 0},
		{"evaluator capabilities named EvaluatorCapabilities", strings.Replace(versionReadsCore050, `"evaluatorCapabilities":`, `"EvaluatorCapabilities":`, 1), 0},
		{"a status named Status beside an invalid one", strings.Replace(versionReadsCore050, `"status":"valid"`, `"status":"invalid","Status":"valid"`, 1), 0},
		// The 0.5.0 evaluator must list the base profile (#233 review).
		{"a 0.5.0 evaluator with no profiles", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0"}`, 1), 0},
		{"a 0.5.0 evaluator with an empty list of profiles", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":[]}`, 1), 0},
		{"a 0.5.0 evaluator whose profiles do not list base", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["extended"]}`, 1), 0},
		{"a 0.5.0 evaluator whose profiles list Base", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["Base"]}`, 1), 0},
		{"a 0.5.0 evaluator whose profiles are not a list", strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":"base"}`, 1), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.answer == versionReadsCore050 && c.exit == 0 {
				t.Fatal("the case is the answer that says it")
			}
			s, log := versionService(t, c.answer, c.exit)
			res := serve(t, s, "POST", "/v1/previews", previewOf(core050JournalPack), nil)
			if res.Code != 422 || res.Body.String() != core050Unsupported {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a refused check saved a release:", n)
			}
			if got := takeLog(t, log); got != core050AskedNoPack {
				t.Fatalf("the Runtime was invoked\n%s", got)
			}
			for _, pack := range []string{journalPack, core030JournalPack} {
				res = serve(t, s, "POST", "/v1/previews", previewOf(pack), nil)
				if res.Code != 201 {
					t.Fatalf("%s check: %d %s", pack, res.Code, res.Body.String())
				}
				if got := takeLog(t, log); strings.Contains(got, "version --format json") || !strings.HasPrefix(got, "spec validate pack.json --format json | "+pack+"\n") {
					t.Fatalf("the check of %s asked\n%s", pack, got)
				}
			}
		})
	}
	s, log := versionService(t, versionReadsCore050, 0)
	res := serve(t, s, "POST", "/v1/previews", previewOf(core050JournalPack), nil)
	if res.Code != 201 {
		t.Fatalf("a Runtime that says it evaluates 0.5.0 refused: %d %s", res.Code, res.Body.String())
	}
	var r Release
	must(t, json.Unmarshal(res.Body.Bytes(), &r))
	if r.Pack != core050JournalPack || expectedEvaluatorSpec(r) != "0.5.0" || string(member(r.Preview, "evaluatorSpecVersion")) != `"0.5.0"` || releasesHeld(t, s) != 1 {
		t.Fatalf("the 0.5.0 release was not checked under 0.5.0: %+v", r)
	}
	if got := takeLog(t, log); !strings.HasPrefix(got, core050AskedNoPack+"spec validate pack.json --format json | "+core050JournalPack+"\n") || strings.Count(got, "version --format json") != 1 {
		t.Fatalf("the Runtime was not asked first, and once\n%s", got)
	}
	// Members a later Runtime adds beside the ones read, at the top and in
	// an evaluator, are not read, and the answer still says it.
	for name, answer := range map[string]string{
		"a member added at the top":             strings.Replace(versionReadsCore050, `"outputVersion":"2"`, `"outputVersion":"2","futureMember":{"nested":[1,"0.4.0-draft"]}`, 1),
		"a member added in the 0.5.0 evaluator": strings.Replace(versionReadsCore050, entry, `{"specVersion":"0.5.0","supportedProfiles":["base","extended"],"futureMember":true}`, 1),
		"a member added in another evaluator":   strings.Replace(versionReadsCore050, `{"specVersion":"0.2.0-draft"}`, `{"specVersion":"0.2.0-draft","futureMember":null}`, 1),
		"the answer's members in another order": `{"status":"valid","command":"version","supportedSpecVersions":["0.5.0"],"outputVersion":"2","evaluatorCapabilities":[{"supportedProfiles":["base"],"specVersion":"0.5.0"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, log := versionService(t, answer, 0)
			res := serve(t, s, "POST", "/v1/previews", previewOf(core050JournalPack), nil)
			if res.Code != 201 || releasesHeld(t, s) != 1 {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if got := takeLog(t, log); !strings.HasPrefix(got, core050AskedNoPack+"spec validate ") {
				t.Fatalf("the Runtime was not asked first\n%s", got)
			}
		})
	}
}

// saysItEvaluatesCore050 reads a Runtime's own version answer, as this test
// reads it and not as Runner does: whether it lists Core 0.5.0 among its
// documents and as an evaluator capability with no stage that lists the base
// profile.
func saysItEvaluatesCore050(t *testing.T, bin string) bool {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(bin, "version", "--format", "json")
	cmd.Dir = dir
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	out, e := cmd.Output()
	if e != nil {
		t.Fatal("the Runtime's version answer:", e)
	}
	var answer struct {
		Documents    []string         `json:"supportedSpecVersions"`
		Capabilities []map[string]any `json:"evaluatorCapabilities"`
	}
	if e = json.Unmarshal(out, &answer); e != nil {
		t.Fatal("the Runtime's version answer:", e)
	}
	documents := false
	for _, v := range answer.Documents {
		documents = documents || v == "0.5.0"
	}
	for _, c := range answer.Capabilities {
		profiles, _ := c["supportedProfiles"].([]any)
		if _, staged := c["stage"]; c["specVersion"] == "0.5.0" && !staged && slices.Contains(profiles, any("base")) {
			return documents
		}
	}
	return false
}

// triageMatrix is the release tests of every triage pack, written by hand: a
// clean intake proceeds, and one whose appropriateness is pending, which the
// 0.5.0 pack states as a member of its value set of assessments not yet
// judged, is returned for clarification.
const triageMatrix = `{"matrixVersion":"3","cases":[` +
	`{"id":"clean","facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidenceAvailability":{"intake-form":"present","sponsor-endorsement":"present"},"expectedDisposition":{"kind":"outcome","outcomeId":"proceed","reasons":[],"handoff":{"state":"none"}}},` +
	`{"id":"pending","facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pending","embargoedInformationToUnauthorizedRecipients":false}},"evidenceAvailability":{"intake-form":"present","sponsor-endorsement":"present"},"expectedDisposition":{"kind":"outcome","outcomeId":"clarify-return","reasons":[],"handoff":{"state":"none"}}}` +
	`]}`

// Core 0.5.0 runs side by side with 0.2.0-draft and 0.3.0 in one service on
// ordinary mapped-source runs, through the pinned Runtime that says it reads
// and evaluates it (#232), as the two did before: each run's record names the
// version its pack declares, verify-run verifies it and refuses a record that
// names another, and a restart and a restored backup keep it. The working
// selector 0.4.0-draft, the version it would have been, and versions that
// resemble 0.5.0 are refused by name, before any release exists. The pack's
// value sets are read where Runner relies on the Runtime's reading of a pack:
// its release tests pass under 0.5.0, the mapping's coverage is the same as
// for the other versions, and a mapping that leaves out the one fact the pack
// consults only through a value set is refused for it, as for the others.
func TestOrdinaryCore050RealRuntimeSideBySide(t *testing.T) {
	cfg := testConfig(t)
	bin := os.Getenv("JPACK_CORE050_TEST_BIN")
	if bin == "" {
		t.Skip("set JPACK_CORE050_TEST_BIN to the pinned Runtime that evaluates Core 0.5.0 (scripts/checks/build-pinned.sh)")
	}
	if !saysItEvaluatesCore050(t, bin) {
		t.Fatal("JPACK_CORE050_TEST_BIN does not say that it reads and evaluates Core 0.5.0")
	}
	cfg.Runtime = bin
	versions := []string{"0.2.0-draft", "0.3.0", "0.5.0"}
	packOf := triagePackOf(t)
	releases := coreVersionsSideBySide(t, cfg, versions, []string{"0.4.0-draft", "0.4.0", "0.5.0-draft", "0.5.1", "0.3.0-rc.2"}, packOf)
	for _, version := range versions {
		if len(releases[version].MappingWarnings) == 0 || !slices.Equal(releases[version].MappingWarnings, releases["0.2.0-draft"].MappingWarnings) {
			t.Fatalf("the %s release's mapping warnings differ: %q", version, releases[version].MappingWarnings)
		}
	}
	cfg = testConfig(t)
	cfg.Runtime = bin
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	all := []FactMapping{{Target: "/request/appropriateness", Source: "/facts/request/appropriateness"}, {Target: "/request/completeness", Source: "/facts/request/completeness"}, {Target: "/request/embargoedInformationToUnauthorizedRecipients", Source: "/facts/request/embargoedInformationToUnauthorizedRecipients"}}
	uncovered, p := sideBySideInput(t, all)
	cfg.InputProfiles = []InputProfile{p}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for _, version := range versions {
		r, e := s.preview(context.Background(), PreviewRequest{Pack: packOf(version), Input: sample(), Matrix: triageMatrix})
		if e != nil || r.Tests != "passed" || !releaseReady(r) || string(member(r.TestEvidence.Report, "evaluatorSpecVersion")) != `"`+version+`"` || string(member(r.Preview, "evaluatorSpecVersion")) != `"`+version+`"` {
			t.Fatalf("the %s release tests: %v %s %.800s", version, e, r.Tests, r.TestEvidence.Report)
		}
		_, e = s.preview(context.Background(), PreviewRequest{Pack: packOf(version), Input: uncovered})
		var refusal *apiError
		if !errors.As(e, &refusal) || refusal.Code != "invalid_source_input" || refusal.Message != `Map or explicitly acknowledge fact pointer "/request/type" before release.` {
			t.Fatalf("the %s release of a mapping without /request/type: %v", version, e)
		}
	}
}

// A pack that declares Core 0.5.0 is refused, with the bytes the check's
// refusal has and no release saved, by each pinned Runtime that does not say
// it evaluates 0.5.0: the ordinary Runtime, whose version answer lists no
// evaluator capabilities, and the retained-column companion, which lists
// Core 0.3.0 at most. Asked to validate the pack, each says it is not valid:
// without the question its refusal would have been reported as the pack's.
func TestCore050RefusedByARealRuntimeThatDoesNotSayItEvaluatesIt(t *testing.T) {
	testConfig(t)
	pack, e := os.ReadFile("testdata/triage-0.5.0.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"JPACK_TEST_BIN", "JPACK_RETAINED_TEST_BIN"} {
		t.Run(name, func(t *testing.T) {
			bin := os.Getenv(name)
			if bin == "" {
				t.Skip("set " + name + " to its pinned Runtime (scripts/checks/build-pinned.sh)")
			}
			if saysItEvaluatesCore050(t, bin) {
				t.Fatal(name + " says that it evaluates Core 0.5.0: this test needs a pinned Runtime that does not")
			}
			dir := t.TempDir()
			must(t, os.WriteFile(filepath.Join(dir, "pack.json"), pack, 0600))
			cmd := exec.Command(bin, "spec", "validate", "pack.json", "--format", "json")
			cmd.Dir = dir
			cmd.Env = []string{"LANG=C", "LC_ALL=C"}
			out, _ := cmd.Output()
			if string(member(out, "status")) == `"valid"` {
				t.Fatalf("%s validates the Core 0.5.0 pack: %s", name, out)
			}
			cfg := testConfig(t)
			cfg.Runtime = bin
			cfg.disableAutomation = true
			cfg.disableDispatcher = true
			s, e := Open(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			res := serve(t, s, "POST", "/v1/previews", string(encode(PreviewRequest{Pack: string(pack), Input: sample()})), nil)
			if res.Code != 422 || res.Body.String() != core050Unsupported {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a refused check saved a release:", n)
			}
		})
	}
}
