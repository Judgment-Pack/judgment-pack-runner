package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// The form is the gateway's for its own scope identifier: these are its
// corpus cases, and the classes the standard library's tables are read for.
func TestTheScopeFormIsTheGateways(t *testing.T) {
	if unicode.Version != scopeUnicodeVersion {
		t.Fatalf("the standard library's tables are Unicode %s; the form is held to %s", unicode.Version, scopeUnicodeVersion)
	}
	bound := strings.Repeat("\U0001F600", 4) + strings.Repeat("é", 56)
	if len(bound) != maxScopeBytes {
		t.Fatalf("the case at the bound is %d bytes", len(bound))
	}
	for _, c := range []struct {
		name, value string
		valid       bool
	}{
		{"valid", "scope-a", true},
		{"at the bound", bound, true},
		{"past the bound", strings.Repeat("a", maxScopeBytes+1), false},
		{"empty", "", false},
		{"control character", "scope\x07a", false},
		{"tab", "scope\ta", false},
		{"line feed", "scope\na", false},
		{"zero-width space", "scope​a", false},
		{"bidi override", "scope‮a", false},
		{"private use", "scopea", false},
		{"noncharacter", "scope﷐a", false},
		{"unassigned in 15.0.0", "scope⿼a", false},
		{"not UTF-8", "scope\xffa", false},
		// Default_Ignorable_Code_Point, as DerivedCoreProperties.txt derives
		// it: ignorable by property, by format category and as a selector.
		{"soft hyphen", "scope­a", false},
		{"combining grapheme joiner", "scope͏a", false},
		{"hangul filler", "scopeㅤa", false},
		{"variation selector", "scope️a", false},
		{"mongolian vowel separator", "scope᠎a", false},
		{"tag", "scope\U000e0041a", false},
		// ...and the format characters it leaves out.
		{"interlinear annotation anchor", "scope￹a", true},
		{"prepended concatenation mark", "scope؀a", true},
		{"egyptian hieroglyph format control", "scope\U00013430a", true},
		{"space", "scope a", true},
		{"case and composition, as written", "Scope-É", true},
	} {
		if got := isScopeIdentifier(c.value); got != c.valid {
			t.Errorf("%s: %q taken %v, want %v", c.name, c.value, got, c.valid)
		}
	}
}

// A request names a scope identifier once, in form, in its query; a query that
// does not parse is refused when one of its pieces names a scope, so that a
// malformed scope is never read as none.
func TestAScopeIsNamedOnceAndInForm(t *testing.T) {
	for _, c := range []struct {
		raw, scope     string
		named, refused bool
	}{
		{"", "", false, false},
		{"q=x", "", false, false},
		{"q=%zz", "", false, false},
		{"scope=scope-a", "scope-a", true, false},
		{"sc%6Fpe=scope%2Da", "scope-a", true, false},
		{"scope=scope+a", "scope a", true, false},
		{"scope=scope-a&scope=scope-a", "", false, true},
		{"scope=", "", false, true},
		{"scope", "", false, true},
		{"scope=scope%09a", "", false, true},
		{"scope=%zz", "", false, true},
		{"q=%zz&scope=scope-a", "", false, true},
		{"scope=scope-a;q=x", "", false, true},
		{"q=x;sc%6Fpe=scope-a", "", false, true},
	} {
		scope, named, err := scopeParameter(c.raw)
		if scope != c.scope || named != c.named || (err != nil) != c.refused || err != nil && err != error(errScopeForm) {
			t.Errorf("%q: %q %v %v", c.raw, scope, named, err)
		}
	}
}

// scopedRecords are a job's records, and its trigger's key.
type scopedRecords struct{ job, run, trigger, occurrence, token string }

// scopeWorld is a store with a job under scope-a and a job under none, each
// with an enabled event trigger, an occurrence and a run.
func scopeWorld(t *testing.T) (*Service, Config, scopedRecords, scopedRecords) {
	t.Helper()
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	runtime := completingRuntime(t, cfg.Dir)
	create := func(scope string) scopedRecords {
		c := eventTriggerConfig()
		j, e := s.createJobBy(requester{}, "Job "+scope, journalRelease(t, s, runtime).ID, scope, &c)
		must(t, e)
		tr, token, e := s.setTriggerState(j.InitialTriggerID, 1, false, true)
		must(t, e)
		o, _, e := s.event(tr.ID, token, delivery("first"))
		must(t, e)
		r, _, e := s.submit(j.ID, "first", journalInput())
		must(t, e)
		return scopedRecords{j.ID, r.ID, tr.ID, o.ID, token}
	}
	return s, cfg, create("scope-a"), create("")
}

// scopeRoute is a request a route answers about the records it names.
type scopeRoute struct {
	pattern string
	// reaches says the route serves the records it names under their scope
	// otherwise than it answers for records the store does not hold.
	reaches bool
	request func(r scopedRecords) (method, path, body string, header map[string]string)
}

func scopeRoutes() []scopeRoute {
	get := func(path func(r scopedRecords) string) func(r scopedRecords) (string, string, string, map[string]string) {
		return func(r scopedRecords) (string, string, string, map[string]string) { return "GET", path(r), "", nil }
	}
	post := func(path func(r scopedRecords) string, body string) func(r scopedRecords) (string, string, string, map[string]string) {
		return func(r scopedRecords) (string, string, string, map[string]string) { return "POST", path(r), body, nil }
	}
	trigger := eventTriggerConfig()
	return []scopeRoute{
		{"GET /v1/jobs/{job}", true, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job })},
		{"GET /v1/jobs/{job}/briefs", false, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/briefs" })},
		{"POST /v1/jobs/{job}/briefs", false, post(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/briefs" }, `{}`)},
		{"GET /v1/jobs/{job}/events", true, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/events" })},
		{"GET /v1/jobs/{job}/execution-binding", false, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/execution-binding" })},
		{"GET /v1/jobs/{job}/occurrences", true, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/occurrences" })},
		{"GET /v1/jobs/{job}/runs", true, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/runs" })},
		{"POST /v1/jobs/{job}/runs", true, func(r scopedRecords) (string, string, string, map[string]string) {
			return "POST", "/v1/jobs/" + r.job + "/runs", string(encode(journalInput())), map[string]string{"Idempotency-Key": "second"}
		}},
		{"GET /v1/jobs/{job}/triggers", true, get(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/triggers" })},
		{"POST /v1/jobs/{job}/triggers", true, post(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/triggers" }, string(encode(map[string]any{"config": trigger})))},
		{"POST /v1/jobs/{job}/triggers/preview", true, post(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/triggers/preview" }, string(encode(trigger)))},
		{"GET /v1/jobs/{job}/upgrade-receipt", false, get(func(r scopedRecords) string {
			return "/v1/jobs/" + r.job + "/upgrade-receipt?baseRevision=1&baseReleaseId=release_x&releaseId=release_y"
		})},
		{"POST /v1/jobs/{job}/upgrade", false, post(func(r scopedRecords) string { return "/v1/jobs/" + r.job + "/upgrade" }, string(encode(UpgradeJobRequest{ReleaseID: "release_x", BaseReleaseID: "release_y", BaseRevision: 1, Reviewed: true})))},
		{"GET /v1/events", true, get(func(r scopedRecords) string { return "/v1/events?job=" + r.job })},
		// A job named more than once, even as itself or empty, names no
		// record: the route refuses it, whatever the ids.
		{"GET /v1/events", false, get(func(r scopedRecords) string { return "/v1/events?job=" + r.job + "&job=x" })},
		{"GET /v1/events", false, get(func(r scopedRecords) string { return "/v1/events?job=" + r.job + "&job=" + r.job })},
		{"GET /v1/events", false, get(func(r scopedRecords) string { return "/v1/events?job=" + r.job + "&job=" })},
		{"GET /v1/runs/{run}", true, get(func(r scopedRecords) string { return "/v1/runs/" + r.run })},
		{"GET /v1/runs/{run}/briefs", false, get(func(r scopedRecords) string { return "/v1/runs/" + r.run + "/briefs" })},
		{"POST /v1/runs/{run}/briefs", false, post(func(r scopedRecords) string { return "/v1/runs/" + r.run + "/briefs" }, `{}`)},
		{"GET /v1/runs/{run}/diagnostics", false, get(func(r scopedRecords) string { return "/v1/runs/" + r.run + "/diagnostics" })},
		{"POST /v1/runs/{run}/retry", false, post(func(r scopedRecords) string { return "/v1/runs/" + r.run + "/retry" }, `{"attempt":0}`)},
		{"GET /v1/runs/{run}/verification", false, get(func(r scopedRecords) string { return "/v1/runs/" + r.run + "/verification" })},
		{"GET /v1/triggers/{trigger}/deliveries/{event}", true, get(func(r scopedRecords) string { return "/v1/triggers/" + r.trigger + "/deliveries/first" })},
		{"POST /v1/triggers/{trigger}/deliveries", false, post(func(r scopedRecords) string { return "/v1/triggers/" + r.trigger + "/deliveries" }, string(encode(RecordDelivery{ReleaseID: "release_x", Event: delivery("record")})))},
		{"POST /v1/triggers/{trigger}/state", true, post(func(r scopedRecords) string { return "/v1/triggers/" + r.trigger + "/state" }, `{"revision":2,"paused":false,"reviewed":true}`)},
		{"POST /v1/triggers/{trigger}/rotate-key", false, post(func(r scopedRecords) string { return "/v1/triggers/" + r.trigger + "/rotate-key" }, `{"revision":2}`)},
		{"POST /v1/triggers/{trigger}/events", true, func(r scopedRecords) (string, string, string, map[string]string) {
			return "POST", "/v1/triggers/" + r.trigger + "/events", string(encode(delivery("second"))), map[string]string{"X-Trigger-Token": r.token}
		}},
		{"GET /v1/triggers/{trigger}/occurrences/{occurrence}", true, func(r scopedRecords) (string, string, string, map[string]string) {
			return "GET", "/v1/triggers/" + r.trigger + "/occurrences/" + r.occurrence, "", map[string]string{"X-Trigger-Token": r.token}
		}},
		{"POST /v1/occurrences/{occurrence}/reconcile", false, post(func(r scopedRecords) string { return "/v1/occurrences/" + r.occurrence + "/reconcile" }, ``)},
		{"POST /v1/occurrences/{occurrence}/cancel", false, post(func(r scopedRecords) string { return "/v1/occurrences/" + r.occurrence + "/cancel" }, ``)},
	}
}

// registeredRoutes is every route the package registers, read from its source.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	files, e := filepath.Glob("*.go")
	if e != nil {
		t.Fatal(e)
	}
	pattern := regexp.MustCompile(`HandleFunc\("([A-Z]+ /[^"]*)"`)
	seen := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		for _, m := range pattern.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
	}
	routes := []string{}
	for r := range seen {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	return routes
}

// withScope is path naming scope, or none for "".
func withScope(path, scope string) string {
	if scope == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "scope=" + url.QueryEscape(scope)
}

func wholeAnswer(w *httptest.ResponseRecorder) string {
	return fmt.Sprintf("%d %v %s", w.Code, w.Header(), w.Body.Bytes())
}

func journalLength(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if e := sqliteDB(s).QueryRow("SELECT count(*) FROM events").Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// A request that names a job, a run, a trigger or an occurrence under another
// scope than the record's, under none for a record that carries one, or under
// one for a record that carries none, is answered byte for byte as the same
// request about a record the store does not hold, and changes and journals
// nothing. Under the record's own scope the route serves it.
func TestARequestNamesARecordUnderItsScope(t *testing.T) {
	s, _, scoped, unscoped := scopeWorld(t)
	missing := scopedRecords{"job_missing", "run_missing", "trg_missing", "occ_missing", scoped.token}
	routes := scopeRoutes()
	covered := map[string]bool{}
	for _, r := range routes {
		covered[r.pattern] = true
	}
	for _, pattern := range registeredRoutes(t) {
		path := strings.SplitN(pattern, " ", 2)[1]
		if strings.Contains(path, "{job}") || strings.Contains(path, "{run}") || strings.Contains(path, "{trigger}") || strings.Contains(path, "{occurrence}") || pattern == "GET /v1/events" {
			if !covered[pattern] {
				t.Errorf("%s names a record and is not held to its scope here", pattern)
			}
			delete(covered, pattern)
		}
	}
	for pattern := range covered {
		t.Errorf("%s is not a route that names a record", pattern)
	}
	before := journalLength(t, s)
	for _, r := range routes {
		for _, c := range []struct {
			name    string
			records scopedRecords
			scope   string
		}{
			{"under another scope", scoped, "scope-b"},
			{"under none, for a record under one", scoped, ""},
			{"under one, for a record under none", unscoped, "scope-a"},
		} {
			method, path, body, header := r.request(c.records)
			asked := withScope(path, c.scope)
			got := serve(t, s, method, asked, body, header)
			method, path, body, header = r.request(missing)
			want := serve(t, s, method, withScope(path, c.scope), body, header)
			if wholeAnswer(got) != wholeAnswer(want) {
				t.Errorf("%s %s (%s): answered\n%s\nand for a record the store does not hold\n%s", r.pattern, c.name, asked, wholeAnswer(got), wholeAnswer(want))
			}
			if got.Code < 400 {
				t.Errorf("%s %s (%s): served %d", r.pattern, c.name, asked, got.Code)
			}
		}
	}
	if after := journalLength(t, s); after != before {
		t.Fatalf("the refused requests wrote %d entries", after-before)
	}
	for _, r := range routes {
		if !r.reaches {
			continue
		}
		for _, c := range []struct {
			records scopedRecords
			scope   string
		}{{scoped, "scope-a"}, {unscoped, ""}} {
			method, path, body, header := r.request(c.records)
			got := serve(t, s, method, withScope(path, c.scope), body, header)
			method, path, body, header = r.request(missing)
			want := serve(t, s, method, withScope(path, c.scope), body, header)
			if got.Code == want.Code || got.Code >= 400 {
				t.Errorf("%s under %q: %d %s, and %d for a record the store does not hold", r.pattern, c.scope, got.Code, got.Body, want.Code)
			}
		}
	}
	// Each record is served with its scope, and one under none without.
	for _, c := range []struct{ path, want string }{
		{"/v1/jobs/" + scoped.job + "?scope=scope-a", `"scope":"scope-a"`},
		{"/v1/runs/" + scoped.run + "?scope=scope-a", `"scope":"scope-a"`},
		{"/v1/jobs/" + scoped.job + "/triggers?scope=scope-a", `"scope":"scope-a"`},
		{"/v1/jobs/" + scoped.job + "/occurrences?scope=scope-a", `"scope":"scope-a"`},
		{"/v1/jobs/" + unscoped.job, ""},
		{"/v1/runs/" + unscoped.run, ""},
		{"/v1/jobs/" + unscoped.job + "/triggers", ""},
		{"/v1/jobs/" + unscoped.job + "/occurrences", ""},
	} {
		got := serve(t, s, "GET", c.path, "", nil)
		if got.Code != 200 || c.want != "" && !strings.Contains(got.Body.String(), c.want) || c.want == "" && strings.Contains(got.Body.String(), `"scope"`) {
			t.Errorf("%s: %d %s", c.path, got.Code, got.Body)
		}
	}
}

// Any other route refuses a scope rather than answer as if none were named;
// every route refuses one out of form.
func TestARouteThatNamesNoRecordRefusesAScope(t *testing.T) {
	s, _, scoped, _ := scopeWorld(t)
	addressing := map[string]bool{}
	for _, r := range scopeRoutes() {
		addressing[r.pattern] = true
	}
	refused := func(w *httptest.ResponseRecorder) bool {
		var p struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		return w.Code == 400 && json.Unmarshal(w.Body.Bytes(), &p) == nil && p.Error.Code == "invalid_scope"
	}
	for _, pattern := range registeredRoutes(t) {
		method, path, _ := strings.Cut(pattern, " ")
		if !addressing[pattern] && !scopeListings[pattern] {
			if got := serve(t, s, method, path+"?scope=scope-a", `{}`, nil); !refused(got) {
				t.Errorf("%s took a scope: %d %s", pattern, got.Code, got.Body)
			}
		}
	}
	for _, path := range []string{"/v1/jobs/" + scoped.job, "/v1/jobs", "/v1/status"} {
		for _, query := range []string{"scope=", "scope=scope-a&scope=scope-a", "scope=scope%09a", "scope=%zz"} {
			if got := serve(t, s, "GET", path+"?"+query, "", nil); !refused(got) {
				t.Errorf("%s?%s: %d %s", path, query, got.Code, got.Body)
			}
		}
	}
}

// listedIDs is the ids of the items a listing serves.
func listedIDs(t *testing.T, s *Service, path string) []string {
	t.Helper()
	got := serve(t, s, "GET", path, "", nil)
	var page struct {
		Items []struct {
			ID       string `json:"id"`
			Sequence int64  `json:"sequence"`
		} `json:"items"`
	}
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil {
		t.Fatalf("%s: %d %s", path, got.Code, got.Body)
	}
	ids := []string{}
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

// A store-wide listing filtered by a scope serves exactly the records that
// carry it, byte for byte: not a scope it is a prefix of, nor one that differs
// in case. Without the filter it serves everything.
func TestAScopeFiltersTheStoreWideListingsExactly(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	if got := serve(t, s, "GET", "/v1/status", "", nil); !strings.Contains(got.Body.String(), `"scope-identifiers"`) {
		t.Fatalf("the capability is not listed: %s", got.Body)
	}
	runtime := completingRuntime(t, cfg.Dir)
	jobs, runs := map[string]string{}, map[string]string{}
	for _, scope := range []string{"scope-a", "scope-ab", "Scope-A", ""} {
		j, e := s.createJobBy(requester{}, "Job", journalRelease(t, s, runtime).ID, scope, nil)
		must(t, e)
		r, _, e := s.submit(j.ID, "k", journalInput())
		must(t, e)
		jobs[scope], runs[scope] = j.ID, r.ID
	}
	if got := listedIDs(t, s, "/v1/jobs?scope=scope-a"); !slices.Equal(got, []string{jobs["scope-a"]}) {
		t.Errorf("jobs under scope-a: %v", got)
	}
	if got := listedIDs(t, s, "/v1/runs?scope=scope-a"); !slices.Equal(got, []string{runs["scope-a"]}) {
		t.Errorf("runs under scope-a: %v", got)
	}
	if got := listedIDs(t, s, "/v1/runs?scope=scope-a&state=queued"); !slices.Equal(got, []string{runs["scope-a"]}) {
		t.Errorf("queued runs under scope-a: %v", got)
	}
	if got := listedIDs(t, s, "/v1/jobs"); len(got) != 4 {
		t.Errorf("every job: %v", got)
	}
	if got := listedIDs(t, s, "/v1/runs"); len(got) != 4 {
		t.Errorf("every run: %v", got)
	}
	for _, path := range []string{"/v1/events?scope=scope-a", "/v1/events?job=" + jobs["scope-a"] + "&scope=scope-a"} {
		got := serve(t, s, "GET", path, "", nil)
		var page EventPage
		if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, got.Code, got.Body)
		}
		kinds := []string{}
		for _, e := range page.Items {
			if e.Scope != "scope-a" || e.Concerns.Job != jobs["scope-a"] {
				t.Errorf("%s served %+v", path, e)
			}
			kinds = append(kinds, e.Kind)
		}
		if !slices.Equal(kinds, []string{"job.created", "run.queued"}) {
			t.Errorf("%s: %v", path, kinds)
		}
	}
	var all EventPage
	if got := serve(t, s, "GET", "/v1/events", "", nil); json.Unmarshal(got.Body.Bytes(), &all) != nil || len(all.Items) != len(journalEntries(t, cfg)) {
		t.Errorf("every entry: %d %s", got.Code, got.Body)
	}
}

// A job's scope is fixed when it is created: a repeated creation must name
// the same one, an upgrade names none and keeps the job's, and the job's
// creation and revision records carry it.
func TestAJobsScopeCannotBeChanged(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	first := journalRelease(t, s, s.runtimeDigest)
	create := func(scope any) *httptest.ResponseRecorder {
		body := map[string]any{"name": "Scoped", "releaseId": first.ID, "reviewed": true}
		if scope != nil {
			body["scope"] = scope
		}
		return serve(t, s, "POST", "/v1/jobs", string(encode(body)), nil)
	}
	got := create("scope-a")
	var j Job
	if got.Code != 201 || json.Unmarshal(got.Body.Bytes(), &j) != nil || j.Scope != "scope-a" {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if again := create("scope-a"); again.Code >= 300 || !strings.Contains(again.Body.String(), j.ID) {
		t.Fatalf("the same creation again: %d %s", again.Code, again.Body)
	}
	for _, scope := range []any{"scope-b", nil} {
		if again := create(scope); again.Code != 409 || !strings.Contains(again.Body.String(), "release_already_used") {
			t.Errorf("the creation again under %v: %d %s", scope, again.Code, again.Body)
		}
	}
	next := journalRelease(t, s, s.runtimeDigest)
	upgrade := UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true}
	var renamed map[string]any
	must(t, json.Unmarshal(encode(upgrade), &renamed))
	renamed["scope"] = "scope-b"
	if got := serve(t, s, "POST", "/v1/jobs/"+j.ID+"/upgrade?scope=scope-a", string(encode(renamed)), nil); got.Code != 400 {
		t.Errorf("an upgrade naming a scope: %d %s", got.Code, got.Body)
	}
	for _, scope := range []string{"", "scope-b"} {
		if got := serve(t, s, "POST", withScope("/v1/jobs/"+j.ID+"/upgrade", scope), string(encode(upgrade)), nil); got.Code != 404 {
			t.Errorf("an upgrade under %q: %d %s", scope, got.Code, got.Body)
		}
	}
	if got := serve(t, s, "POST", "/v1/jobs/"+j.ID+"/upgrade?scope=scope-a", string(encode(upgrade)), nil); got.Code != 200 {
		t.Fatalf("the upgrade: %d %s", got.Code, got.Body)
	}
	upgraded, e := s.job(j.ID)
	must(t, e)
	if upgraded.Revision != 2 || upgraded.Scope != "scope-a" {
		t.Fatalf("%+v", upgraded)
	}
	// The upgrade's receipt is served under the job's scope, which is no
	// part of the upgrade it is for, and under none as for a job the store
	// does not hold.
	receipt := "/v1/jobs/" + j.ID + "/upgrade-receipt?baseRevision=1&baseReleaseId=" + first.ID + "&releaseId=" + next.ID
	if got := serve(t, s, "GET", receipt+"&scope=scope-a", "", nil); got.Code != 200 || !strings.Contains(got.Body.String(), `"scope":"scope-a"`) || !strings.Contains(got.Body.String(), next.ID) {
		t.Errorf("the upgrade's receipt: %d %s", got.Code, got.Body)
	}
	if got := serve(t, s, "GET", receipt, "", nil); got.Code != 404 || !strings.Contains(got.Body.String(), "upgrade_not_completed") {
		t.Errorf("the upgrade's receipt under none: %d %s", got.Code, got.Body)
	}
	for _, table := range []string{"jobs", "job_creations", "job_revisions"} {
		var n, carrying int
		must(t, sqliteDB(s).QueryRow("SELECT count(*), count(CASE WHEN json_extract(record,'$.scope')='scope-a' THEN 1 END) FROM "+table).Scan(&n, &carrying))
		if n == 0 || carrying != n {
			t.Errorf("%s: %d of %d records carry the scope", table, carrying, n)
		}
	}
}

// A creation's scope member is a scope identifier in form, or absent: one
// written null, empty, of another type or out of form creates nothing.
func TestAJobIsCreatedUnderAScopeInForm(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	r := journalRelease(t, s, s.runtimeDigest)
	for scope, code := range map[string]string{`null`: "invalid_scope", `""`: "invalid_scope", `"scope\ta"`: "invalid_scope", `"` + strings.Repeat("a", 129) + `"`: "invalid_scope", `7`: "invalid_request", `["scope-a"]`: "invalid_request"} {
		body := `{"name":"Scoped","releaseId":"` + r.ID + `","reviewed":true,"scope":` + scope + `}`
		if got := serve(t, s, "POST", "/v1/jobs", body, nil); got.Code != 400 || !strings.Contains(got.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("scope %s: %d %s", scope, got.Code, got.Body)
		}
	}
	// A member whose name only folds to scope, which the decoder would take
	// as it, is refused, alone or beside scope, so that a reader of scope as
	// written never reads another value than Runner; scope named twice is
	// refused as every duplicate member is.
	for members, code := range map[string]string{
		`"Scope":"scope-a"`:                   "invalid_scope",
		`"SCOPE":"scope-a"`:                   "invalid_scope",
		`"ſcope":"scope-a"`:                   "invalid_scope",
		`"Scope":null`:                        "invalid_scope",
		`"scope":"scope-a","Scope":"scope-b"`: "invalid_scope",
		`"Scope":"scope-b","scope":"scope-a"`: "invalid_scope",
		`"scope":"scope-a","scope":"scope-b"`: "invalid_request",
	} {
		body := `{"name":"Scoped","releaseId":"` + r.ID + `","reviewed":true,` + members + `}`
		if got := serve(t, s, "POST", "/v1/jobs", body, nil); got.Code != 400 || !strings.Contains(got.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("members %s: %d %s", members, got.Code, got.Body)
		}
	}
	if ids := listedIDs(t, s, "/v1/jobs"); len(ids) != 0 {
		t.Fatalf("a refused creation made %v", ids)
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("schema %s", schema)
	}
	// The manual-admission gate holds no scope, and refuses one as it
	// refuses a trigger.
	s.cfg.ExecutorAdmission = &ManualAdmissionBinding{}
	_, e := s.createJobBy(requester{}, "Scoped", r.ID, "scope-a", nil)
	s.cfg.ExecutorAdmission = nil
	if e == nil || e.Error() != manualUnsupported().Error() {
		t.Fatalf("a manual-admission Runner answered %v", e)
	}
}

func storeSchemaOf(t *testing.T, db *sql.DB) string {
	t.Helper()
	var schema string
	must(t, db.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema))
	return schema
}

// Every record and every journal entry about a job created under a scope
// carries it, through every kind of change Runner makes; an entry about no
// job carries none. Each served entry is the documented one.
func TestEveryRecordAndEntryOfAScopedJobCarriesItsScope(t *testing.T) {
	cfg := journalConfig(t)
	w := runScopedJournalSteps(t, cfg, "scope-a", true)
	s := w.s
	// An upgrade rewrites a job's triggers, and a cloud signal bound to a
	// trigger can be refused: the scenario makes neither.
	first := journalRelease(t, s, s.runtimeDigest)
	c := eventTriggerConfig()
	j, e := w.createJob("Upgraded with a trigger", first.ID, &c)
	must(t, e)
	next := journalRelease(t, s, s.runtimeDigest)
	_, e = s.upgradeJob(context.Background(), j.ID, UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: first.ID, BaseRevision: 1, Reviewed: true})
	must(t, e)
	tr, _ := storedTrigger(t, s, w.event.ID)
	s.refusedCloudSignal("scheduler", &tr, "A cloud signal named a paused trigger.")
	entries := journalEntries(t, cfg)
	if got := kindsOf(entries[len(entries)-7:]); !slices.Equal(got, []string{"release.previewed", "job.created", "trigger.configured", "release.previewed", "job.upgraded", "trigger.configured", "admission.refused"}) || entries[len(entries)-1].Request != "cloud-signal" {
		t.Fatalf("the upgrade and the refusal wrote %v", got)
	}
	for _, e := range entries {
		if e.Concerns.Job != "" && (e.Scope != "scope-a" || e.EntryVersion != wantEntryVersion(e)) {
			t.Errorf("an entry about a scoped job: %+v", e)
		}
		if e.Concerns.Job == "" && (e.Scope != "" || e.EntryVersion == scopedEntryVersion) {
			t.Errorf("an entry about no job: %+v", e)
		}
	}
	for _, table := range []string{"jobs", "job_creations", "job_revisions", "triggers", "trigger_revisions", "occurrences", "runs"} {
		var n, carrying int
		must(t, sqliteDB(s).QueryRow("SELECT count(*), count(CASE WHEN json_extract(record,'$.scope')='scope-a' THEN 1 END) FROM "+table).Scan(&n, &carrying))
		if n == 0 || carrying != n {
			t.Errorf("%s: %d of %d records carry the scope", table, carrying, n)
		}
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != scopedStoreSchema {
		t.Errorf("schema %s", schema)
	}
	desk := standInDesk{t, readOpenAPI(t), s}
	read, _, _ := desk.follow("/v1/events?scope=scope-a", 0)
	scoped := 0
	for _, e := range entries {
		if e.Scope != "" {
			scoped++
		}
	}
	if len(read) != scoped {
		t.Errorf("the filter served %d of %d entries", len(read), scoped)
	}
	desk.follow("/v1/events", 0)
	doc := readOpenAPI(t)
	// Each record is served with the documented member.
	for _, c := range []struct{ path, schema string }{
		{"/v1/jobs/" + w.job.ID + "?scope=scope-a", "Job"},
		{"/v1/runs/" + w.completed.ID + "?scope=scope-a", "Run"},
		{"/v1/jobs/" + w.job.ID + "/triggers?scope=scope-a", "Trigger"},
		{"/v1/jobs/" + w.job.ID + "/occurrences?scope=scope-a", "Occurrence"},
	} {
		got := serve(t, s, "GET", c.path, "", nil)
		body, _ := decodeNumbers(t, got.Body.Bytes()).(map[string]any)
		values := []any{body}
		if items, listed := body["items"].([]any); listed {
			values = items
		} else if job, detailed := body["job"]; detailed {
			values = []any{job}
		}
		member, documented := doc.schema(c.schema)["properties"].(map[string]any)["scope"].(map[string]any)
		if got.Code != 200 || len(values) == 0 || !documented {
			t.Fatalf("%s: %d %s", c.path, got.Code, got.Body)
		}
		for _, v := range values {
			if scope := v.(map[string]any)["scope"]; scope != "scope-a" || len(doc.check(c.schema+"/scope", member, scope)) != 0 {
				t.Errorf("%s: %v is not the documented scope", c.path, scope)
			}
		}
	}
}

// A store at the scoped schema is relocated as a store at the schema before
// it is, and keeps its scoped job and its schema.
func TestAScopedStoreIsRelocated(t *testing.T) {
	cfg := journalConfig(t)
	cfg.InputRoot = relocationTempDir(t)
	s, e := Open(cfg)
	must(t, e)
	j, e := s.createJobBy(requester{}, "Scoped", journalRelease(t, s, s.runtimeDigest).ID, "scope-a", nil)
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != scopedStoreSchema {
		t.Fatalf("schema %s", schema)
	}
	s.Close()
	next := filepath.Join(relocationTempDir(t), "moved-project")
	must(t, os.Rename(cfg.InputRoot, next))
	request := RelocateRequest{Version: 1, ID: strings.Repeat("b", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: next}
	if result, e := RelocateWorkspace(request); e != nil || result.AlreadyApplied {
		t.Fatal(result, e)
	}
	cfg.InputRoot = next
	s, e = Open(cfg)
	must(t, e)
	defer s.Close()
	held, e := s.job(j.ID)
	must(t, e)
	if held.Scope != "scope-a" || storeSchemaOf(t, sqliteDB(s)) != scopedStoreSchema {
		t.Fatalf("%+v under schema %s", held, storeSchemaOf(t, sqliteDB(s)))
	}
}

// A store's first job under a scope raises its schema, with the job, to one
// no earlier Runner opens; a store without one stays where it was, and a
// store prepared for the manual-admission gate stays refused.
func TestAScopedJobRaisesTheStoresSchema(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	runtime := completingRuntime(t, cfg.Dir)
	_, e := s.createJob("Unscoped", journalRelease(t, s, runtime).ID)
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("an unscoped job: schema %s", schema)
	}
	refuse(t, s, `CREATE TRIGGER scope_test_fault BEFORE INSERT ON events WHEN NEW.kind='job.created' BEGIN SELECT RAISE(ABORT,'the test refuses this write'); END`)
	if _, e = s.createJobBy(requester{}, "Scoped", journalRelease(t, s, runtime).ID, "scope-a", nil); e == nil {
		t.Fatal("the refused creation was made")
	}
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != storeSchema {
		t.Fatalf("a refused scoped job: schema %s", schema)
	}
	refuse(t, s, "DROP TRIGGER scope_test_fault")
	_, e = s.createJobBy(requester{}, "Scoped", journalRelease(t, s, runtime).ID, "scope-a", nil)
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != scopedStoreSchema || schema == storeSchema || schema == manualStoreSchema {
		t.Fatalf("a scoped job: schema %s", schema)
	}
	_, e = s.createJobBy(requester{}, "Scoped too", journalRelease(t, s, runtime).ID, "scope-b", nil)
	must(t, e)
	s.Close()
	s = openJournal(t, cfg)
	if schema := storeSchemaOf(t, sqliteDB(s)); schema != scopedStoreSchema {
		t.Fatalf("reopened: schema %s", schema)
	}
	refuse(t, s, "UPDATE metadata SET value='"+manualStoreSchema+"' WHERE key='schema'")
	s.Close()
	if _, e = Open(cfg); e == nil {
		t.Fatal("a store at the manual-admission gate's schema was opened")
	}
}
