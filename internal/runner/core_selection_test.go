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
	"path/filepath"
	"strings"
	"testing"
)

// The Core version a pack declares selects its evaluator on ordinary runs as
// on selected ones (#188): 0.2.0-draft and 0.3.0 are admitted, anything else
// is refused by name, and nothing upgrades a 0.2.0-draft pack.
func TestOrdinaryCoreVersionIsTheDeclaredOne(t *testing.T) {
	for _, version := range []string{"0.2.0-draft", "0.3.0"} {
		if !supportedEvaluatorSpec(version, nil) {
			t.Fatal("ordinary evaluator refused", version)
		}
		r := Release{Pack: `{"specVersion":"` + version + `"}`}
		if expectedEvaluatorSpec(r) != version {
			t.Fatal("ordinary release is not evaluated under its declared Core", version, expectedEvaluatorSpec(r))
		}
	}
	for _, version := range []string{"", "0.2.0", "0.3.0-rc.2", "0.3.1", "0.4.0", "1.0.0", "0.3.0 "} {
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
	for declared, named := range map[string]string{"0.4.0": `"0.4.0"`, "": "declares none", strings.Repeat("9", 65): "another version"} {
		message := unsupportedEvaluatorSpec(declared)
		if !strings.Contains(message, named) || !strings.Contains(message, "0.2.0-draft or 0.3.0") {
			t.Fatalf("refusal does not name the version: %q", message)
		}
	}
}

// The pack the Runtime is given is the release's, byte for byte, and its
// answer must name the declared Core: a 0.2.0-draft pack is never evaluated as
// 0.3.0, nor a 0.3.0 pack as 0.2.0-draft, and no answer is accepted for a pack
// that declares a Core Runner does not evaluate.
func TestOrdinaryRunIsNeverEvaluatedUnderAnotherCore(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "runtime-is-never-started")
	for _, c := range []struct{ declared, answered string }{
		{"0.2.0-draft", "0.2.0-draft"}, {"0.3.0", "0.3.0"},
		{"0.2.0-draft", "0.3.0"}, {"0.3.0", "0.2.0-draft"}, {"0.4.0", "0.4.0"},
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
		case c.declared == "0.4.0":
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
	cfg.SigningKey = signingKey(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	input, p, key, at := v2Fixture(t)
	cfg.InputProfiles = []InputProfile{p}
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/request", Source: "/facts/request"}}, Evidence: []EvidenceMapping{{Requirement: "intake-form", Source: "/evidence/intake-form"}, {Requirement: "sponsor-endorsement", Source: "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	draftPack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	declares := []byte(`"specVersion": "0.2.0-draft"`)
	if bytes.Count(draftPack, declares) != 1 {
		t.Fatal("handwritten pack no longer declares 0.2.0-draft once")
	}
	// Each Core's pack is its own artifact version: one identity and version
	// names one pack's content.
	packOf := func(version string) string {
		pack := bytes.Replace(draftPack, declares, []byte(`"specVersion": "`+version+`"`), 1)
		if version != "0.2.0-draft" {
			pack = bytes.Replace(pack, []byte(`"version": "0.1.0"`), []byte(`"version": "0.1.1"`), 1)
		}
		return string(pack)
	}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	if !runtimeTakesBinding(t, cfg.Runtime) {
		t.Fatal("the Core 0.3.0 Runtime must take a preparation binding")
	}
	for _, version := range []string{"0.4.0", "0.3.0-rc.2", "0.2.0"} {
		_, e := s.preview(context.Background(), PreviewRequest{Pack: packOf(version), Input: input})
		var refusal *apiError
		if !errors.As(e, &refusal) || refusal.Code != "invalid_pack" || !strings.Contains(refusal.Message, fmt.Sprintf("%q", version)) {
			t.Fatalf("unsupported Core %s was not refused by name: %v", version, e)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.Dir, "releases")); len(entries) != 0 {
		t.Fatal("a refused Core left a release")
	}
	versions := []string{"0.2.0-draft", "0.3.0"}
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
