package runner

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Runner #210: the configuration a release is given, the Runtime it asks
// before giving it requireComparableFacts, and the boot requirement.

// configWith is the configuration a release checked with
// requireComparableFacts has, written out by hand.
const configWith = `{"configVersion":"5","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"},"requireComparableFacts":true}`

// The packs schema answers of a Runtime that reads configVersion "5", and of
// one before 0.25.0, which does not.
const (
	schemaReadsFive = `{"outputVersion":"2","tool":"jpack","command":"packs schema","status":"valid","kind":"config","configVersion":"7","supportedConfigVersions":["1","2","3","4","5","6","7"],"schemaId":"urn:judgmentpack:runtime:jpack-config:7","bytes":1,"sha256":"0"}`
	schemaReadsFour = `{"outputVersion":"2","tool":"jpack","command":"packs schema","status":"valid","kind":"config","configVersion":"4","supportedConfigVersions":["1","2","3","4"],"schemaId":"urn:judgmentpack:runtime:jpack-config:4","bytes":1,"sha256":"0"}`
)

// takeLog returns what the Runtime logged since it was last taken, and
// empties it.
func takeLog(t *testing.T, log string) string {
	t.Helper()
	b, e := os.ReadFile(log)
	if os.IsNotExist(e) {
		return ""
	}
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(log); e != nil {
		t.Fatal(e)
	}
	return string(b)
}

func checkRequest(comparable bool) string {
	return string(encode(PreviewRequest{Pack: journalPack, Input: journalInput(), RequireComparableFacts: comparable}))
}

func checkRelease(t *testing.T, s *Service, comparable bool) Release {
	t.Helper()
	res := serve(t, s, "POST", "/v1/previews", checkRequest(comparable), nil)
	if res.Code != 201 {
		t.Fatalf("release check: %d %s", res.Code, res.Body.String())
	}
	var r Release
	must(t, json.Unmarshal(res.Body.Bytes(), &r))
	return r
}

func releasesHeld(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	must(t, s.db.QueryRow("SELECT count(*) FROM releases").Scan(&n))
	return n
}

// A release checked without requireComparableFacts has, byte for byte, the
// configuration every earlier release has, and its Runtime is not asked what
// it reads; one checked with it has the configVersion "5" configuration that
// sets the switch, after its Runtime said it reads it. Every invocation of the
// check, and every operational evaluation of the release, is given that
// configuration and the release's own lock, and nothing else.
func TestReleaseConfigurationWithAndWithoutComparableFacts(t *testing.T) {
	evaluate := "experimental evaluate --pack-id target --config jpack.json --facts facts.json --format json"
	doc := readOpenAPI(t)
	if problems := doc.check("PreviewRequest", doc.schema("PreviewRequest"), decodeNumbers(t, []byte(checkRequest(true)))); len(problems) > 0 {
		t.Fatal(problems)
	}
	for _, member := range []string{"config", "lock"} {
		if described, _ := doc.schema("Release")["properties"].(map[string]any)[member].(map[string]any); described == nil || described["type"] != "string" {
			t.Fatal("the API document does not describe the release's", member)
		}
	}
	for _, c := range []struct {
		name       string
		comparable bool
		config     string
		probe      string
	}{
		{"without", false, configWithout, ""},
		{"with", true, configWith, "packs schema --format json | none | none\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, log := checkService(t)
			r := checkRelease(t, s, c.comparable)
			if r.Config != c.config || r.Lock != checkLock {
				t.Fatalf("the release holds %s and %s", r.Config, r.Lock)
			}
			held, e := s.release(r.ID)
			must(t, e)
			if held.Config != c.config {
				t.Fatalf("the store holds %s", held.Config)
			}
			want := c.probe +
				"spec validate pack.json --format json | " + c.config + " | none\n" +
				"packs lock --config jpack.json --format json | " + c.config + " | none\n" +
				evaluate + " --rehearsal | " + c.config + " | " + checkLock + "\n"
			if got := takeLog(t, log); got != want {
				t.Fatalf("the check invoked\n%s\nnot\n%s", got, want)
			}
			j, e := s.createJob("Configured", r.ID)
			must(t, e)
			if run := runOnce(t, s, j.ID, "one"); run.State != "completed" {
				t.Fatal("the run ended", run.State, run.Problem)
			}
			if got, want := takeLog(t, log), evaluate+" | "+c.config+" | "+checkLock+"\n"; got != want {
				t.Fatalf("the run invoked\n%s\nnot\n%s", got, want)
			}
		})
	}
}

// A release check that asks for requireComparableFacts on a pinned Runtime
// that does not say it reads configVersion "5" is refused, with these bytes,
// before the Runtime is given any configuration, and no release is saved:
// whether the Runtime says it reads only earlier versions or its answer cannot
// be read. Without the option, the same Runtime checks a release as before.
func TestComparableFactsRefusedOnARuntimeThatDoesNotSayItReadsVersionFive(t *testing.T) {
	refusal := `{"error":{"code":"comparable_facts_unsupported","message":"The pinned Runtime does not say that it reads configVersion \"5\", which requireComparableFacts needs; Runtime 0.25.0 and later read it. Pin such a Runtime, or check the release without requireComparableFacts.","retryable":false}}` + "\n"
	for _, c := range []struct {
		name   string
		schema string
		exit   int
	}{
		{"a Runtime that reads only earlier versions", schemaReadsFour, 0},
		{"an answer whose Runtime exited with a failure", schemaReadsFive, 1},
		{"another outputVersion", strings.Replace(schemaReadsFive, `"outputVersion":"2"`, `"outputVersion":"1"`, 1), 0},
		{"another command's answer", strings.Replace(schemaReadsFive, `"command":"packs schema"`, `"command":"packs list"`, 1), 0},
		{"an answer that is not valid", strings.Replace(schemaReadsFive, `"status":"valid"`, `"status":"invalid"`, 1), 0},
		{"no list of versions", strings.Replace(schemaReadsFive, `"supportedConfigVersions":["1","2","3","4","5","6","7"],`, ``, 1), 0},
		{"versions that are not strings", strings.Replace(schemaReadsFive, `["1","2","3","4","5","6","7"]`, `[1,2,3,4,5,6,7]`, 1), 0},
		{"an answer that is not JSON", `usage: jpack packs`, 0},
		{"an empty answer", ``, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, log := checkServiceAnswering(t, c.schema, c.exit)
			res := serve(t, s, "POST", "/v1/previews", checkRequest(true), nil)
			if res.Code != 422 || res.Body.String() != refusal {
				t.Fatalf("answered %d %s", res.Code, res.Body.String())
			}
			if n := releasesHeld(t, s); n != 0 {
				t.Fatal("a refused check saved a release:", n)
			}
			if got := takeLog(t, log); got != "packs schema --format json | none | none\n" {
				t.Fatalf("the Runtime was invoked\n%s", got)
			}
			if r := checkRelease(t, s, false); r.Config != configWithout {
				t.Fatal("without the option the release holds", r.Config)
			}
			if got := takeLog(t, log); strings.Contains(got, "packs schema") || !strings.HasPrefix(got, "spec validate ") {
				t.Fatalf("without the option the Runtime was invoked\n%s", got)
			}
		})
	}
}

// A requireComparableFacts written null, in any spelling the decoder reads as
// it, is a malformed body and is refused with these bytes before any Runtime
// is invoked or anything is written: it is not read as false, which a bool
// cannot tell apart once decoded. Written true or false, or absent, the check
// is as before.
func TestRequireComparableFactsWrittenNullIsRefused(t *testing.T) {
	body := func(member string) string {
		return `{"pack":` + string(encode(journalPack)) + `,"input":` + string(encode(journalInput())) + member + `}`
	}
	refusal := `{"error":{"code":"invalid_request","message":"Supply one bounded JSON request with supported fields and no duplicate members.","retryable":false}}` + "\n"
	s, log := checkService(t)
	for name, member := range map[string]string{
		"null":                             `,"requireComparableFacts":null`,
		"null, spaced":                     `,"requireComparableFacts": null `,
		"null, in capitals":                `,"RequireComparableFacts":null`,
		"null, escaped":                    `,"requireComparable\u0046acts":null`,
		"null beside a true in capitals":   `,"requireComparableFacts":true,"REQUIRECOMPARABLEFACTS":null`,
		"null beside a false in lowercase": `,"requirecomparablefacts":null,"requireComparableFacts":false`,
	} {
		res := serve(t, s, "POST", "/v1/previews", body(member), nil)
		if res.Code != 400 || res.Body.String() != refusal {
			t.Fatalf("%s: answered %d %s", name, res.Code, res.Body.String())
		}
	}
	if got := takeLog(t, log); got != "" {
		t.Fatalf("a refused check invoked the Runtime\n%s", got)
	}
	if n := releasesHeld(t, s); n != 0 {
		t.Fatal("a refused check saved a release:", n)
	}
	for _, c := range []struct{ name, member, config, probe string }{
		{"true", `,"requireComparableFacts":true`, configWith, "packs schema --format json | none | none\n"},
		{"false", `,"requireComparableFacts":false`, configWithout, ""},
		{"absent", ``, configWithout, ""},
	} {
		res := serve(t, s, "POST", "/v1/previews", body(c.member), nil)
		if res.Code != 201 {
			t.Fatalf("%s: answered %d %s", c.name, res.Code, res.Body.String())
		}
		var r Release
		must(t, json.Unmarshal(res.Body.Bytes(), &r))
		if r.Config != c.config {
			t.Fatalf("%s: the release holds %s", c.name, r.Config)
		}
		if got := takeLog(t, log); !strings.HasPrefix(got, c.probe+"spec validate ") {
			t.Fatalf("%s: the check invoked\n%s", c.name, got)
		}
	}
}

// Under manual admission the executor admits no packs schema invocation, and a
// release check that asks for requireComparableFacts is refused as every
// feature the manual profile lacks is, before any Runtime is invoked.
func TestComparableFactsRefusedUnderManualAdmission(t *testing.T) {
	s, log := checkService(t)
	s.cfg.ExecutorAdmission = &ManualAdmissionBinding{}
	res := serve(t, s, "POST", "/v1/previews", checkRequest(true), nil)
	want := `{"error":{"code":"manual_profile_unsupported","message":"This profile requires an exact manual single-Pack job without triggers, mappings or workflow input.","retryable":false}}` + "\n"
	if res.Code != 409 || res.Body.String() != want {
		t.Fatalf("answered %d %s", res.Code, res.Body.String())
	}
	if got := takeLog(t, log); got != "" {
		t.Fatalf("the Runtime was invoked\n%s", got)
	}
}

// An installation booted with requireComparableFacts creates and upgrades jobs
// only from releases checked with it, refusing every other with these bytes,
// a configuration a byte away included. A release checked before Runner #210,
// stored as a Runner then stored it, still runs: a job made from it before is
// returned again, and keeps running.
func TestBootRequirementHoldsJobsToReleasesCheckedWithComparableFacts(t *testing.T) {
	s, log := checkService(t)
	// A release as a Runner before #210 stored it, written out by hand.
	old := "release_checkedbefore210"
	record := `{"schemaVersion":"1","id":"` + old + `","pack":` + string(encode(journalPack)) + `,"packDigest":"` + digest([]byte(journalPack)) + `","packId":"journal","packVersion":"1.0.0","title":"Journal","runtimeDigest":"` + s.runtimeDigest + `","createdAt":"2026-10-01T00:00:00Z","config":` + string(encode(configWithout)) + `,"lock":` + string(encode(checkLock)) + `,"validation":{"outputVersion":"2","status":"valid"},"sample":{"facts":{"request":{"marker":"facts-never-journaled-7f3a"}}},"preview":{"outputVersion":"2","status":"evaluated"},"tests":"not-run"}`
	if _, e := s.db.Exec("INSERT INTO releases(id,record) VALUES(?,?)", old, record); e != nil {
		t.Fatal(e)
	}
	before, e := s.createJob("Before", old)
	must(t, e)

	s.cfg.RequireComparableFacts = true
	again, e := s.createJob("Before", old)
	if e != nil || again != before {
		t.Fatal("repeating the creation of a job made before:", again, e)
	}
	if run := runOnce(t, s, before.ID, "after"); run.State != "completed" {
		t.Fatal("a job made before stopped running:", run.State, run.Problem)
	}
	evaluate := "experimental evaluate --pack-id target --config jpack.json --facts facts.json --format json"
	if got, want := takeLog(t, log), evaluate+" | "+configWithout+" | "+checkLock+"\n"; got != want {
		t.Fatalf("the run of the earlier release invoked\n%s\nnot\n%s", got, want)
	}

	createRefusal := `{"error":{"code":"release_comparable_facts_required","message":"This installation creates jobs only from releases checked with requireComparableFacts. Check a new release with requireComparableFacts.","retryable":false}}` + "\n"
	upgradeRefusal := `{"error":{"code":"release_comparable_facts_required","message":"This installation upgrades jobs only to releases checked with requireComparableFacts. Check a new release with requireComparableFacts.","retryable":false}}` + "\n"
	for name, config := range map[string]string{
		"checked without it":            configWithout,
		"configVersion 5 without it":    `{"configVersion":"5","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"}}`,
		"configVersion 5 with false":    `{"configVersion":"5","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"},"requireComparableFacts":false}`,
		"the same members, spaced":      `{"configVersion": "5","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"},"requireComparableFacts":true}`,
		"another configVersion with it": `{"configVersion":"7","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"},"requireComparableFacts":true}`,
		"no configuration":              "",
	} {
		r := heldRelease(t, s, config, s.runtimeDigest)
		res := serve(t, s, "POST", "/v1/jobs", `{"name":"Refused","releaseId":"`+r.ID+`","reviewed":true}`, nil)
		if res.Code != 409 || res.Body.String() != createRefusal {
			t.Fatalf("%s: creating answered %d %s", name, res.Code, res.Body.String())
		}
		res = serve(t, s, "POST", "/v1/jobs/"+before.ID+"/upgrade", string(encode(UpgradeJobRequest{ReleaseID: r.ID, BaseReleaseID: old, BaseRevision: 1, Reviewed: true})), nil)
		if res.Code != 409 || res.Body.String() != upgradeRefusal {
			t.Fatalf("%s: upgrading answered %d %s", name, res.Code, res.Body.String())
		}
	}
	if held, e := s.job(before.ID); e != nil || held != before {
		t.Fatal("a refused upgrade changed the job:", held, e)
	}

	checked := checkRelease(t, s, true)
	if res := serve(t, s, "POST", "/v1/jobs", `{"name":"Comparable","releaseId":"`+checked.ID+`","reviewed":true}`, nil); res.Code != 201 {
		t.Fatalf("a release checked with it: %d %s", res.Code, res.Body.String())
	}
	upgradeTo := checkRelease(t, s, true)
	res := serve(t, s, "POST", "/v1/jobs/"+before.ID+"/upgrade", string(encode(UpgradeJobRequest{ReleaseID: upgradeTo.ID, BaseReleaseID: old, BaseRevision: 1, Reviewed: true})), nil)
	if res.Code != 200 {
		t.Fatalf("upgrading to a release checked with it: %d %s", res.Code, res.Body.String())
	}
}
