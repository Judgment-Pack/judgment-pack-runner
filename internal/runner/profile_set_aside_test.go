package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stored profile the current form refuses is set aside, not a store that
// will not open (runner #154): the store opens, the profile is kept byte for
// byte with its id taken and counted, reported with its reason, and never
// bound; everything else it stores works as before. What fails as a set
// still refuses to open the store, and a configured profile the form
// refuses still refuses to start.

// roundSixteenRefused is a scope entry RFC 0018 round 15 admitted and round
// 16 refuses: an escaped slash in its path, as a GitLab project path writes
// one (runner #152).
const roundSixteenRefused = "https://a.example/api/v4/projects/group%2Fproject"

// earlierScoped is a scoped page profile an earlier Runner admitted and this
// one refuses, its one entry roundSixteenRefused, with the specification's
// probe vector, whose binding document binds a research source's page to it:
// the vector's page profile, scoped so.
func earlierScoped(t *testing.T) (InputProfile, PreparationFixture) {
	t.Helper()
	f, _ := scopedFixture(t, scopeProbe{scope: []any{roundSixteenRefused}})
	profiles := profilesOf(t, f)
	var scoped InputProfile
	if err := strictJSON(encode(profiles[len(profiles)-1]), &scoped); err != nil {
		t.Fatal(err)
	}
	if scoped.ID != "page" || validateProfiles([]InputProfile{scoped}) == nil {
		t.Fatal("the fixture is not a page profile this Runner refuses")
	}
	return scoped, f
}

// setStoredRow writes the registered profiles' row of a closed store, as an
// earlier Runner wrote it.
func setStoredRow(t *testing.T, cfg Config, row string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", registeredProfilesKey, row); err != nil {
		t.Fatal(err)
	}
}

// storedRowOf is the registered profiles' row of a closed store.
func storedRowOf(t *testing.T, cfg Config) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var row string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key=?", registeredProfilesKey).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

// heldRowOf is the registered profiles' row of an open store.
func heldRowOf(t *testing.T, s *Service) string {
	t.Helper()
	var row string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key=?", registeredProfilesKey).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

// requireSetAside holds err to the refusal of a binding to the set-aside
// profile id: its code, naming the profile.
func requireSetAside(t *testing.T, where string, err error, id string) {
	t.Helper()
	var refused *apiError
	if !errors.As(err, &refused) || refused.Status != 422 || refused.Code != codeSetAside || !strings.HasPrefix(refused.Message, "The input profile "+id+" was set aside when Runner opened its store, and is never bound: ") {
		t.Fatalf("%s: a binding to the set-aside profile %s is refused as %v, not by its name as %s", where, id, err, codeSetAside)
	}
}

// requireSetAsideAnswer is requireSetAside for a route's answer.
func requireSetAsideAnswer(t *testing.T, where string, w *httptest.ResponseRecorder, id string) {
	t.Helper()
	var body struct {
		Error struct {
			Code, Message string
		} `json:"error"`
	}
	if w.Code != 422 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Code != codeSetAside || !strings.HasPrefix(body.Error.Message, "The input profile "+id+" was set aside when Runner opened its store, and is never bound: ") {
		t.Fatalf("%s: a binding to the set-aside profile %s is answered %d %s, not by its name as %s", where, id, w.Code, w.Body.String(), codeSetAside)
	}
}

// The upgrade: a store an earlier Runner (v0.21.0) wrote, whose row holds an
// exact profile and a scoped page profile whose entry RFC 0018 round 16
// refuses, opens. The exact profile binds and a run under it starts; the
// scoped one is set aside, reported with its reason at open and in the
// listing, and a binding to it is refused by its name. A registration keeps
// the set-aside profile's bytes in the row; its id stays taken. Reopened,
// the store gives the same result.
func TestAStoreAnEarlierRunnerWroteOpensWithItsRefusedProfileSetAside(t *testing.T) {
	cfg := testConfig(t)
	input, exact, key, at := v2Fixture(t)
	input.Source.Mapping.UnmappedEvidence = []string{"sensitive-data-approvals"}
	input.Source.Mapping.Sources[0].Read = SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}}}}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, exact, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), encode(sample()), at, 0)}
	scoped, f := earlierScoped(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(exact)), nil); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	s.Close()
	// The row v0.21.0 wrote once it registered the scoped profile beside the
	// exact one: both, as registration encodes them.
	written := string(encode([]InputProfile{exact, scoped}))
	setStoredRow(t, cfg, written)
	if s, err = Open(cfg); err != nil {
		t.Fatalf("the store holding a profile this Runner refuses did not open: %v", err)
	}
	reason := validateProfiles([]InputProfile{scoped}).Error()
	if !strings.HasPrefix(reason, "the input profile page: ") || !strings.HasSuffix(reason, "(input-malformed)") {
		t.Fatal("the scoped profile is refused for another reason:", reason)
	}
	imported := importRequest(t, Input{Source: scopeSource(t, f)}, "", nil)
	opened := func(installed ...InputProfile) []byte {
		t.Helper()
		got := s.inputProfiles()
		if len(got) != len(installed) {
			t.Fatalf("installed %s, not %s", encode(got), encode(installed))
		}
		for i := range got {
			if profileHash(got[i]) != profileHash(installed[i]) {
				t.Fatalf("installed %s, not %s", encode(got), encode(installed))
			}
		}
		aside := s.SetAside()
		if len(aside) != 1 || aside[0].ID != "page" || aside[0].Reason != reason {
			t.Fatalf("set aside %+v, not the scoped profile with its reason", aside)
		}
		if line := aside[0].String(); line != `the registered input profile "page" is set aside, kept and never bound: `+reason {
			t.Fatal("the line at open is", line)
		}
		listed := serve(t, s, "GET", "/v1/input-profiles", "", nil)
		doc := readOpenAPI(t)
		listing := doc["paths"].(map[string]any)["/input-profiles"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if problems := doc.check("listing", listing, decodeNumbers(t, listed.Body.Bytes())); listed.Code != 200 || len(problems) != 0 {
			t.Fatal(listed.Code, problems, listed.Body.String())
		}
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(listed.Body.Bytes(), &entries); err != nil || len(entries) != len(installed)+1 {
			t.Fatal("the listing is", listed.Body.String())
		}
		for _, entry := range entries[:len(installed)] {
			if entry["state"] != nil || entry["reason"] != nil {
				t.Fatal("an installed profile is listed with a state:", listed.Body.String())
			}
		}
		last := entries[len(installed)]
		if string(last["state"]) != `"set-aside"` || string(last["reason"]) != string(encode(reason)) || string(last["digest"]) != string(encode(profileHash(scoped))) || !sameJSON(last["profile"], encode(scoped)) {
			t.Fatal("the set-aside profile is listed as", listed.Body.String())
		}
		// The digest listed is the one its bindings name.
		if want := scopeAt(mustRead(t, f.Binding), "sources", "person", "webProfiles", "page")["profileDigest"]; string(last["digest"]) != string(encode(want)) {
			t.Fatalf("the listing digests the profile %s, and its binding names %v", last["digest"], want)
		}
		// A research source's page bound to it, at import, is refused by
		// its name, planned or previewed.
		for _, route := range []string{"/v1/inputs/next", "/v1/inputs/preview"} {
			requireSetAsideAnswer(t, route, serve(t, s, "POST", route, string(imported), nil), "page")
		}
		return listed.Body.Bytes()
	}
	opened(exact)

	// The exact profile binds, and a run under it starts and completes.
	pack, err := os.ReadFile("testdata/triage.pack.json")
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: input})
	if err != nil || len(release.InputProfiles) != 1 || profileHash(release.InputProfiles[0]) != profileHash(exact) {
		t.Fatal("the exact profile does not bind:", err)
	}
	job, err := s.createJob("Verified vendor", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := s.submit(job.ID, "after-upgrade", input)
	if err != nil {
		t.Fatal(err)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" || done.StartedAt == "" {
		t.Fatal("a run under the exact profile did not start and complete:", done.State, done.Problem)
	}

	// A registration rewrites the row with the profiles it held as it held
	// them, the set-aside one's bytes unchanged, and the new one after them.
	another := exact
	another.ID = "another"
	if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(another)), nil); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	rewritten := written[:len(written)-1] + "," + string(encode(another)) + "]"
	if got := heldRowOf(t, s); got != rewritten {
		t.Fatalf("the row is\n%s\nnot\n%s", got, rewritten)
	}
	// The set-aside id stays taken: a profile the form admits, under it,
	// is the existing conflict, and the row is unchanged.
	retaken := scoped
	retaken.EndpointScope = EndpointScope{scopePrefix}
	if err := validateProfiles([]InputProfile{retaken}); err != nil {
		t.Fatal(err)
	}
	r := serve(t, s, "POST", "/v1/input-profiles", string(encode(retaken)), nil)
	if r.Code != 409 || !strings.Contains(r.Body.String(), `"code":"input_profile_conflict"`) {
		t.Fatal("the set-aside id was registered again:", r.Code, r.Body.String())
	}
	if got := heldRowOf(t, s); got != rewritten {
		t.Fatal("a refused registration changed the row:", got)
	}
	listing := opened(exact, another)

	// Reopened, the store gives the same result.
	s.Close()
	if got := storedRowOf(t, cfg); got != rewritten {
		t.Fatal("the row changed at close:", got)
	}
	if s, err = Open(cfg); err != nil {
		t.Fatal(err)
	}
	if again := opened(exact, another); string(again) != string(listing) {
		t.Fatalf("reopened, the listing is\n%s\nnot\n%s", again, listing)
	}
	if got := heldRowOf(t, s); got != rewritten {
		t.Fatal("opening changed the row:", got)
	}
}

// A set-aside profile is never bound: every place Runner resolves the
// installation's profiles refuses a binding, a job or a run that names it,
// by its name, never as a profile the installation does not hold. A job
// bound to it stays stored, an identical retry of what was accepted before
// is answered as it was, and a run queued under it before it was set aside
// ends without starting. A configured Gateway connection naming it
// refuses to start, naming it. Here the profile is set aside for a public key
// an earlier Runner admitted (MAPPING-V2.md, "Upgrade note"); its id is what
// every place refuses, and the job and run were made under the definition
// this Runner admitted before the row changed.
func TestASetAsideProfileIsNeverBound(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	input, p, _, _ := v2Fixture(t)
	if _, err := s.registerInputProfile(p); err != nil {
		t.Fatal(err)
	}
	runtime := completingRuntime(t, cfg.Dir)
	mapping := input.Source.Mapping
	release := func() Release {
		r := Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: runtime, CreatedAt: now(), Sample: input, Tests: "not-run", InputMapping: &mapping, InputProfiles: []InputProfile{p}}
		if err := s.saveRelease(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first, next := release(), release()
	job, err := s.createJob("Vendors", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	queued, _, err := s.submit(job.ID, "before", input)
	if err != nil || queued.State != "queued" {
		t.Fatal(queued.State, err)
	}
	s.Close()
	refused := p
	refused.PublicKey = smallOrderKeys[0]
	setStoredRow(t, cfg, string(encode([]InputProfile{refused})))
	s = openJournal(t, cfg)
	if aside := s.SetAside(); len(aside) != 1 || aside[0].ID != p.ID || len(s.inputProfiles()) != 0 {
		t.Fatalf("set aside %+v, installed %d", aside, len(s.inputProfiles()))
	}

	planned := cloneInput(input)
	planned.Source.Sources = nil
	f := newImportFixture(t)
	if f.profile.ID != p.ID {
		t.Fatal("the import binds another profile")
	}
	for _, c := range []struct {
		name, method, path, body string
		headers                  map[string]string
	}{
		{"planning", "POST", "/v1/inputs/next", string(encode(planned)), nil},
		{"planning an import", "POST", "/v1/inputs/next", string(importRequest(t, f.imported(t), "", nil)), nil},
		{"previewing an import", "POST", "/v1/inputs/preview", string(importRequest(t, f.imported(t), "", nil)), nil},
		{"previewing an input", "POST", "/v1/inputs/preview", string(encode(input)), nil},
		{"previewing a release", "POST", "/v1/previews", string(encode(PreviewRequest{Pack: journalPack, Input: input})), nil},
		{"creating a job", "POST", "/v1/jobs", string(encode(map[string]any{"name": "Again", "releaseId": next.ID, "reviewed": true})), nil},
		{"upgrading a job", "POST", "/v1/jobs/" + job.ID + "/upgrade", string(encode(UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true})), nil},
		{"submitting a run", "POST", "/v1/jobs/" + job.ID + "/runs", string(encode(input)), map[string]string{"Idempotency-Key": "after"}},
		{"submitting a run of an import", "POST", "/v1/jobs/" + job.ID + "/runs", string(importRequest(t, f.imported(t), "", nil)), map[string]string{"Idempotency-Key": "imported"}},
	} {
		requireSetAsideAnswer(t, c.name, serve(t, s, c.method, c.path, c.body, c.headers), p.ID)
	}
	// An identical retry of the run accepted before is still answered with
	// its original acceptance: nothing is bound anew.
	if r := serve(t, s, "POST", "/v1/jobs/"+job.ID+"/runs", string(encode(input)), map[string]string{"Idempotency-Key": "before"}); r.Code != 200 || !strings.Contains(r.Body.String(), `"id":"`+queued.ID+`"`) {
		t.Fatal("an identical retry is not answered with its acceptance:", r.Code, r.Body.String())
	}
	// So is a repeated creation of the job made before: it is returned as it
	// was, as a release no longer tested-enough returns one (release_untested).
	if r := serve(t, s, "POST", "/v1/jobs", string(encode(map[string]any{"name": "Vendors", "releaseId": first.ID, "reviewed": true})), nil); r.Code != 201 || !strings.Contains(r.Body.String(), `"id":"`+job.ID+`"`) {
		t.Fatal("a repeated creation of the job made before is not answered with it:", r.Code, r.Body.String())
	}
	requireSetAside(t, "checking the job's release", s.checkReleaseProfiles(first), p.ID)
	requireSetAside(t, "acquiring in the background", s.validateGatewayMapping(first), p.ID)
	_, err = s.nextInput(planned, time.Now())
	requireSetAside(t, "preparing an occurrence", err, p.ID)

	// The run queued before the profile was set aside ends without starting,
	// naming it; the job and the run stay stored and served.
	if more, err := s.dispatchNext(context.Background()); !more || err != nil {
		t.Fatal(more, err)
	}
	ended, err := s.run(queued.ID)
	if err != nil || ended.State != "failed" || ended.Reason != runProfileSetAside || ended.StartedAt != "" || !strings.HasPrefix(ended.Problem, "The input profile "+p.ID+" was set aside") || ended.Result != nil {
		t.Fatalf("the queued run ended %s (%s), %q: %v", ended.State, ended.Reason, ended.Problem, err)
	}
	served := serve(t, s, "GET", "/v1/runs/"+queued.ID, "", nil)
	var answer map[string]any
	if served.Code != 200 || json.Unmarshal(served.Body.Bytes(), &answer) != nil || answer["reason"] != runProfileSetAside {
		t.Fatal(served.Code, served.Body.String())
	}
	doc := readOpenAPI(t)
	if problems := doc.check("run/reason", doc.schema("Run")["properties"].(map[string]any)["reason"].(map[string]any), answer["reason"]); len(problems) != 0 {
		t.Fatal(problems)
	}
	if held, err := s.job(job.ID); err != nil || held.ReleaseID != first.ID {
		t.Fatal("the job bound to the set-aside profile is not kept:", err)
	}

	// A configured Gateway connection that names it refuses to start.
	s.Close()
	cfg.GatewayConnections = []GatewayConnection{{Profile: p.ID, URL: "http://127.0.0.1:65530"}}
	other, err := Open(cfg)
	if err == nil {
		other.Close()
		t.Fatal("a Gateway connection naming a set-aside profile started")
	}
	if !strings.HasPrefix(err.Error(), fmt.Sprintf("a Gateway connection names the registered input profile %q, which is set aside: ", p.ID)) {
		t.Fatal("the refusal does not name the set-aside profile:", err)
	}
	cfg.GatewayConnections = nil
	s = openJournal(t, cfg)
}

// A set-aside profile counts toward the bound of 16: with fifteen admitted
// beside it, the store opens, and a seventeenth is refused at registration.
func TestASetAsideProfileCountsTowardTheBound(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	p, _ := testProfile(t)
	refused := p
	refused.ID, refused.PublicKey = "earlier", smallOrderKeys[0]
	stored := []InputProfile{refused}
	for i := 1; i < 16; i++ {
		next := p
		next.ID = fmt.Sprintf("profile%d", i)
		stored = append(stored, next)
	}
	s.Close()
	row := string(encode(stored))
	setStoredRow(t, cfg, row)
	s = openJournal(t, cfg)
	if len(s.inputProfiles()) != 15 || len(s.SetAside()) != 1 {
		t.Fatal("the store did not open with fifteen profiles installed and one set aside")
	}
	p.ID = "overflow"
	r := serve(t, s, "POST", "/v1/input-profiles", string(encode(p)), nil)
	if r.Code != 422 || !strings.Contains(r.Body.String(), `"code":"input_profile_limit"`) {
		t.Fatal("a seventeenth profile was registered:", r.Code, r.Body.String())
	}
	if heldRowOf(t, s) != row || len(s.inputProfiles()) != 15 {
		t.Fatal("a refused registration changed the profiles")
	}
}

// What fails as a set, not as one profile, still refuses to open the store,
// and leaves its row as it was: a row that is unreadable, of another form or
// over its storage bound, an element that is not a profile in the form
// registration takes, null or {} included, among them; more than 16 stored profiles, or 16 with the
// configured ones, a set-aside one counted; an id stored twice, a set-aside
// profile's included; and a stored profile, set aside or admitted, whose id
// a configured profile of another definition has.
func TestWhatAStoredSetStillRefusesToOpen(t *testing.T) {
	p, _ := testProfile(t)
	named := func(id string, profile InputProfile) InputProfile {
		profile.ID = id
		return profile
	}
	refused := p
	refused.PublicKey = smallOrderKeys[0]
	many := func(n int, last InputProfile) []InputProfile {
		out := []InputProfile{}
		for i := 1; i < n; i++ {
			out = append(out, named(fmt.Sprintf("profile%d", i), p))
		}
		return append(out, last)
	}
	elsewhere := p
	other := "postgresql://elsewhere"
	elsewhere.Endpoint = &other
	row := func(profiles ...InputProfile) string { return string(encode(profiles)) }
	for _, c := range []struct {
		name, row  string
		configured []InputProfile
		says       string
	}{
		{"an unreadable row", `[{"id":"vendors"`, nil, "registered input profiles are invalid: "},
		{"a member no profile has", strings.Replace(row(p), `"id":`, `"unknown":1,"id":`, 1), nil, "registered input profiles are invalid: "},
		{"no array", `null`, nil, "registered input profiles are invalid"},
		{"a null element beside a profile", "[null," + row(p)[1:], nil, "registered input profiles are invalid: element 0 is not a profile in its exact form: it is not a JSON object"},
		{"an empty object beside a profile", "[{}," + row(p)[1:], nil, "registered input profiles are invalid: element 0 is not a profile in its exact form: it has no member id"},
		{"a null element alone", `[null]`, nil, "registered input profiles are invalid: element 0 is not a profile in its exact form: it is not a JSON object"},
		{"a profile without its endpoint", strings.Replace(row(refused), `"endpoint":"postgresql://vendor-records",`, "", 1), nil, "registered input profiles are invalid: element 0 is not a profile in its exact form: it has no member endpoint"},
		{"a member named otherwise but for case", strings.Replace(row(refused), `"class":`, `"Class":`, 1), nil, "registered input profiles are invalid: element 0 is not a profile in its exact form: unknown member"},
		{"a row over its storage bound", "[" + strings.Repeat(" ", registeredProfilesBound) + "]", nil, "registered input profiles exceed their storage bound"},
		{"seventeen stored, one set aside", row(many(17, named("earlier", refused))...), nil, "registered input profiles are invalid: at most 16 input profiles"},
		{"sixteen stored, one set aside, and one configured", row(many(16, named("earlier", refused))...), []InputProfile{named("configured", p)}, "At most 16 installation input profiles are supported"},
		{"an id stored twice, once set aside", row(refused, p), nil, `registered input profiles are invalid: the id "vendors" is stored twice`},
		{"an id stored twice, both set aside", row(refused, refused), nil, `registered input profiles are invalid: the id "vendors" is stored twice`},
		{"a set-aside profile's id configured", row(refused), []InputProfile{p}, `registered input profile "vendors", set aside, has the id of a configured profile of another definition`},
		{"an admitted profile's id configured otherwise", row(p), []InputProfile{elsewhere}, "This profile ID already names another definition."},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := journalConfig(t)
			openJournal(t, cfg).Close()
			setStoredRow(t, cfg, c.row)
			cfg.InputProfiles = c.configured
			s, err := Open(cfg)
			if err == nil {
				s.Close()
				t.Fatal("the store opened")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the refusal is %q, not %q", err, c.says)
			}
			if got := storedRowOf(t, cfg); got != c.row {
				t.Fatal("the row changed:", got)
			}
		})
	}
}

// A set-aside profile's bytes are kept as the store spells them, not as
// Runner would write them: an entry with escaped slashes and an escaped
// hyphen, members in another order, whitespace inside and a literal &, beside
// an exact profile, is byte for byte in the row after a registration and
// after the store reopens. The listing serves both shapes as openapi.json's
// oneOf gives them, the set-aside one compacted and escaped as every answer
// is.
func TestASetAsideProfileKeepsItsBytesAsSpelled(t *testing.T) {
	cfg := journalConfig(t)
	openJournal(t, cfg).Close()
	exact, _ := testProfile(t)
	scoped, _ := earlierScoped(t)
	spelled := `{ "endpointScope" : [ "https:\/\/a.example\/api\/v4\/projects\/group%2Fproject" ] , "research":{"capabilities":["page"]},` +
		` "id" : "page", "publicKey":"` + scoped.PublicKey + `", "class":"generated", "source":"` + scoped.Source + `", "authority":"R&D Authority",` +
		` "shape":"http", "adapter":{"version":"` + scoped.Adapter.Version + `","name":"adapter\u002dweb","digest":"` + scoped.Adapter.Digest + `"}, "endpoint" : null }`
	row := "[" + string(encode(exact)) + "," + spelled + "]"
	setStoredRow(t, cfg, row)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	if aside := s.SetAside(); len(aside) != 1 || aside[0].ID != "page" || !strings.HasSuffix(aside[0].Reason, "(input-malformed)") || len(s.inputProfiles()) != 1 {
		t.Fatalf("set aside %+v, installed %d", aside, len(s.inputProfiles()))
	}
	if got := heldRowOf(t, s); got != row {
		t.Fatal("opening changed the row:", got)
	}
	another := exact
	another.ID = "another"
	if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(another)), nil); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	rewritten := row[:len(row)-1] + "," + string(encode(another)) + "]"
	if got := heldRowOf(t, s); got != rewritten {
		t.Fatalf("the row is\n%s\nnot\n%s", got, rewritten)
	}
	s.Close()
	if got := storedRowOf(t, cfg); got != rewritten {
		t.Fatal("the row changed at close:", got)
	}
	s = openJournal(t, cfg)
	if got := heldRowOf(t, s); got != rewritten || len(s.SetAside()) != 1 || len(s.inputProfiles()) != 2 {
		t.Fatal("reopened, the row or the profiles changed:", got)
	}
	listed := serve(t, s, "GET", "/v1/input-profiles", "", nil)
	doc := readOpenAPI(t)
	listing := doc["paths"].(map[string]any)["/input-profiles"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	entries, ok := decodeNumbers(t, listed.Body.Bytes()).([]any)
	if problems := doc.check("listing", listing, entries); listed.Code != 200 || !ok || len(entries) != 3 || len(problems) != 0 {
		t.Fatal(listed.Code, problems, listed.Body.String())
	}
	installed, setAside := doc.schema("InstalledInputProfile"), doc.schema("SetAsideInputProfile")
	for i, entry := range entries {
		asInstalled, asSetAside := doc.check("installed", installed, entry), doc.check("set aside", setAside, entry)
		if (i < 2) != (len(asInstalled) == 0) || (i == 2) != (len(asSetAside) == 0) {
			t.Fatalf("entry %d matches the installed shape %v and the set-aside shape %v", i, asInstalled, asSetAside)
		}
	}
	var served []map[string]json.RawMessage
	if err := json.Unmarshal(listed.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(served[2]["profile"], []byte(spelled)) {
		t.Fatalf("the set-aside profile is served as\n%s\nnot as the value stored\n%s", served[2]["profile"], spelled)
	}
	if strings.Contains(string(served[2]["profile"]), " :") || !strings.Contains(string(served[2]["profile"]), `"authority":"R\u0026D Authority"`) {
		t.Fatal("the set-aside profile is not served compacted and escaped:", string(served[2]["profile"]))
	}
}

// A registration that would carry the row past its storage bound is refused,
// input_profile_limit, and the store still opens: a set-aside entry may be
// stored larger than Runner writes it, so the row's length is checked, not
// assumed from the bound on each profile. One that fits is taken.
func TestARegistrationPastTheRowsStorageBoundIsRefused(t *testing.T) {
	cfg := journalConfig(t)
	openJournal(t, cfg).Close()
	scoped, _ := earlierScoped(t)
	written := string(encode(scoped))
	padding := registeredProfilesBound - 12000 - len(written)
	padded := "{" + strings.Repeat(" ", padding) + written[1:]
	row := "[" + padded + "]"
	setStoredRow(t, cfg, row)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	if len(s.SetAside()) != 1 || s.SetAside()[0].ID != "page" {
		t.Fatal("the padded profile is not set aside")
	}
	large, _ := testProfile(t)
	large.ID = "large"
	large.Adapter.Name = strings.Repeat("a", 20000)
	if err := validateProfiles([]InputProfile{large}); err != nil || len(row)+1+len(encode(large)) <= registeredProfilesBound {
		t.Fatal("the fixture does not pass the row's bound:", err)
	}
	r := serve(t, s, "POST", "/v1/input-profiles", string(encode(large)), nil)
	if r.Code != 422 || !strings.Contains(r.Body.String(), `"code":"input_profile_limit"`) {
		t.Fatal("a registration past the row's bound was answered", r.Code, r.Body.String())
	}
	if heldRowOf(t, s) != row || len(s.inputProfiles()) != 0 {
		t.Fatal("a refused registration changed the profiles")
	}
	small, _ := testProfile(t)
	if r = serve(t, s, "POST", "/v1/input-profiles", string(encode(small)), nil); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	s.Close()
	s = openJournal(t, cfg)
	if len(s.SetAside()) != 1 || len(s.inputProfiles()) != 1 || heldRowOf(t, s) != row[:len(row)-1]+","+string(encode(small))+"]" {
		t.Fatal("the store did not reopen with its row")
	}
}
