package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Caller scopes (docs/adr/0002-caller-scopes.md): a caller is held to the
// scope identifiers its token's scope_ids and its subject's entry in
// identity.callerScopes both allow.

// heldToScopeA is a configuration that grants ops-service scope-a, and
// build-bot the installation.
func heldToScopeA() callerScopes {
	return callerScopes{entries: map[string]callerEntry{
		"ops-service": {scopes: map[string]bool{"scope-a": true}},
		"build-bot":   {installation: true},
	}}
}

// callerWorld is a store with a job under scope-a, one under scope-b and one
// under none, each with an enabled event trigger, an occurrence and a run
// (scopeWorld's), served in service mode with an identity under scopes, and
// the tokens of three callers: ofA, ops-service claiming scope-a; ofNone,
// ops-service with no claim; installation, build-bot with no claim.
type callerWorld struct {
	s                         *Service
	cfg                       Config
	runtime                   string
	signer                    tokenSigner
	id                        *Identity
	h                         http.Handler
	a, b, none                scopedRecords
	ofA, ofNone, installation string
}

func newCallerWorld(t *testing.T, scopes callerScopes) *callerWorld {
	t.Helper()
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	t.Cleanup(func() { s.Close() })
	w := &callerWorld{s: s, cfg: cfg, runtime: completingRuntime(t, cfg.Dir), signer: edSigner(t, "k1")}
	create := func(scope string) scopedRecords {
		c := eventTriggerConfig()
		j, e := s.createJobBy(requester{}, "Job "+scope, journalRelease(t, s, w.runtime).ID, scope, &c)
		must(t, e)
		tr, token, e := s.setTriggerState(j.InitialTriggerID, 1, false, true)
		must(t, e)
		o, _, e := s.event(tr.ID, token, delivery("first"))
		must(t, e)
		r, _, e := s.submit(j.ID, "first", journalInput())
		must(t, e)
		return scopedRecords{j.ID, r.ID, tr.ID, o.ID, token}
	}
	w.a, w.b, w.none = create("scope-a"), create("scope-b"), create("")
	w.id = testIdentity(t, w.signer)
	w.id.scopes = scopes
	w.h = s.ServiceHandler("", w.id)
	w.ofA = w.token("ops-service", []string{"scope-a"})
	w.ofNone = w.token("ops-service", nil)
	w.installation = w.token("build-bot", nil)
	return w
}

// token is a token for subject whose scope_ids is scopes, or that has none
// for nil.
func (w *callerWorld) token(subject string, scopes any) string {
	claims := claimsFor(subject, time.Now())
	if scopes != nil {
		claims[scopeClaim] = scopes
	}
	return tokenOf(w.signer, claims)
}

func (w *callerWorld) serve(t *testing.T, token, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAs(t, w.h, method, path, body, token, header)
}

// identity.callerScopes is "unchecked", or an entry per listed subject: 1 to
// 256 scope identifiers, each once, "unscoped" or "installation". A refusal
// names the member, never a value.
func TestCallerScopesAreConfiguredPerListedSubject(t *testing.T) {
	member := func(v any) string {
		c := map[string]any{"issuer": testIssuer, "audience": testAudience, "subjects": []string{"ops-service", "build-bot"}, "keys": "/etc/runner/keys.json"}
		if v != nil {
			c["callerScopes"] = v
		}
		return string(encode(c))
	}
	many := make([]string, maxCallerScopes+1)
	for i := range many {
		many[i] = fmt.Sprint("scope-", i)
	}
	for _, c := range []struct {
		name string
		raw  string
		want callerScopes
	}{
		{"absent: checked, every subject unscoped", member(nil), callerScopes{}},
		{"unchecked", member("unchecked"), callerScopes{unchecked: true}},
		{"an entry of each form", member(map[string]any{"ops-service": []string{"scope-a", "scope-b"}, "build-bot": "installation"}), callerScopes{entries: map[string]callerEntry{"ops-service": {scopes: map[string]bool{"scope-a": true, "scope-b": true}}, "build-bot": {installation: true}}}},
		{"unscoped", member(map[string]any{"ops-service": "unscoped"}), callerScopes{entries: map[string]callerEntry{"ops-service": {}}}},
		{"no entry", member(map[string]any{}), callerScopes{entries: map[string]callerEntry{}}},
		{"256 scope identifiers", member(map[string]any{"ops-service": many[:maxCallerScopes]}), callerScopes{entries: map[string]callerEntry{"ops-service": {scopes: func() map[string]bool {
			m := map[string]bool{}
			for _, s := range many[:maxCallerScopes] {
				m[s] = true
			}
			return m
		}()}}}},
	} {
		got, err := ParseIdentityConfig([]byte(c.raw))
		if err != nil || !reflect.DeepEqual(got.scopes, c.want) {
			t.Errorf("%s: %+v %v", c.name, got.scopes, err)
		}
	}
	for _, c := range []struct {
		name string
		raw  string
		want error
	}{
		{"another word", member("checked"), errCallerScopesForm},
		{"null", `{"issuer":"https://issuer.example","audience":"runner-control","subjects":["ops-service"],"keys":"/k","callerScopes":null}`, errCallerScopesForm},
		{"a list", member([]string{"scope-a"}), errCallerScopesForm},
		{"a subject the identity does not list", member(map[string]any{"report-bot": []string{"scope-a"}}), errCallerScopesSubject},
		{"a subject in another case", member(map[string]any{"Ops-Service": []string{"scope-a"}}), errCallerScopesSubject},
		{"no scope identifier", member(map[string]any{"ops-service": []string{}}), errCallerScopesForm},
		{"257 scope identifiers", member(map[string]any{"ops-service": many}), errCallerScopesForm},
		{"a scope identifier twice", member(map[string]any{"ops-service": []string{"scope-a", "scope-a"}}), errCallerScopesForm},
		{"an empty scope identifier", member(map[string]any{"ops-service": []string{""}}), errCallerScopesForm},
		{"a scope identifier out of form", member(map[string]any{"ops-service": []string{"scope\ta"}}), errCallerScopesForm},
		{"a member that is not a string", member(map[string]any{"ops-service": []any{1}}), errCallerScopesForm},
		{"a bare scope identifier", member(map[string]any{"ops-service": "scope-a"}), errCallerScopesForm},
		{"the installation in another case", member(map[string]any{"ops-service": "Installation"}), errCallerScopesForm},
		{"an entry of null", member(map[string]any{"ops-service": nil}), errCallerScopesForm},
		{"an entry twice", strings.Replace(member(map[string]any{"ops-service": "unscoped"}), `"ops-service":"unscoped"`, `"ops-service":"unscoped","ops-service":"installation"`, 1), nil},
	} {
		got, err := ParseIdentityConfig([]byte(c.raw))
		if err == nil || c.want != nil && err != c.want {
			t.Errorf("%s: %+v %v, want %v", c.name, got.scopes, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), "report-bot") || strings.Contains(err.Error(), "Ops-Service") || strings.Contains(err.Error(), "scope-a") {
			t.Errorf("%s: the refusal repeats a value: %v", c.name, err)
		}
	}
	// The start holds an entry to a subject the identity lists, however the
	// configuration was made.
	c := IdentityConfig{Issuer: testIssuer, Audience: testAudience, Subjects: []string{"ops-service"}, Keys: "/nonexistent-key-set-for-a-test.json", scopes: callerScopes{entries: map[string]callerEntry{"report-bot": {}}}}
	if _, err := LoadIdentity(c, "journal-owner", t.TempDir()); err != errCallerScopesSubject {
		t.Fatal("an entry for a subject not listed was loaded:", err)
	}
}

// A token reaches the scope identifiers its claim and its subject's entry
// both allow; a claim wider than the entry is refused, not narrowed; with no
// claim, the none scope, or the installation for an "installation" entry.
// Unchecked, the claim is not read.
func TestATokensScopeClaimIsHeldToItsSubjectsEntry(t *testing.T) {
	checked := heldToScopeA()
	checked.entries["ops-service"] = callerEntry{scopes: map[string]bool{"scope-a": true, "scope-b": true}}
	reach := func(scopes ...string) callerReach {
		m := map[string]bool{}
		for _, s := range scopes {
			m[s] = true
		}
		return callerReach{scopes: m}
	}
	many := make([]any, maxCallerScopes+1)
	for i := range many {
		many[i] = fmt.Sprint("scope-", i)
	}
	none := map[string]any{}
	claim := func(v any) map[string]any { return map[string]any{scopeClaim: v} }
	for _, c := range []struct {
		name    string
		scopes  callerScopes
		subject string
		claims  map[string]any
		want    callerReach
		err     error
	}{
		{"no claim, an entry of scopes", checked, "ops-service", none, callerReach{none: true}, nil},
		{"no claim, no entry", checked, "report-bot", none, callerReach{none: true}, nil},
		{"no claim, no member", callerScopes{}, "build-bot", none, callerReach{none: true}, nil},
		{"no claim, the installation's entry", checked, "build-bot", none, installationReach, nil},
		{"a claim within the entry", checked, "ops-service", claim([]any{"scope-a"}), reach("scope-a"), nil},
		{"a claim of every scope the entry grants", checked, "ops-service", claim([]any{"scope-b", "scope-a"}), reach("scope-a", "scope-b"), nil},
		{"a claim of an installation subject", checked, "build-bot", claim([]any{"scope-z"}), reach("scope-z"), nil},
		{"a claim wider than the entry", checked, "ops-service", claim([]any{"scope-a", "scope-c"}), callerReach{}, errScopeClaimWider},
		{"a claim the entry grants nothing of", checked, "ops-service", claim([]any{"scope-c"}), callerReach{}, errScopeClaimWider},
		{"a claim with no entry", checked, "report-bot", claim([]any{"scope-a"}), callerReach{}, errScopeClaimWider},
		{"a claim with no member", callerScopes{}, "ops-service", claim([]any{"scope-a"}), callerReach{}, errScopeClaimWider},
		{"a claim of null", checked, "ops-service", claim(nil), callerReach{}, errScopeClaimForm},
		{"a claim that is a string", checked, "ops-service", claim("scope-a"), callerReach{}, errScopeClaimForm},
		{"a claim of no scope", checked, "ops-service", claim([]any{}), callerReach{}, errScopeClaimForm},
		{"a claim past 256", checked, "build-bot", claim(many), callerReach{}, errScopeClaimForm},
		{"a claim naming one twice", checked, "ops-service", claim([]any{"scope-a", "scope-a"}), callerReach{}, errScopeClaimForm},
		{"a claim naming one out of form", checked, "ops-service", claim([]any{"scope\x07a"}), callerReach{}, errScopeClaimForm},
		{"a claim naming an empty one", checked, "ops-service", claim([]any{""}), callerReach{}, errScopeClaimForm},
		{"a claim naming a number", checked, "ops-service", claim([]any{1}), callerReach{}, errScopeClaimForm},
		{"unchecked, a claim wider than any entry", callerScopes{unchecked: true}, "ops-service", claim([]any{"scope-c"}), installationReach, nil},
		{"unchecked, a claim out of form", callerScopes{unchecked: true}, "ops-service", claim("scope-a"), installationReach, nil},
		{"unchecked, no claim", callerScopes{unchecked: true}, "ops-service", none, installationReach, nil},
	} {
		got, err := c.scopes.reachOf(c.subject, c.claims)
		if err != c.err || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, err, c.want, c.err)
		}
	}
	for _, c := range []struct {
		reach   callerReach
		scope   string
		reaches bool
	}{
		{callerReach{}, "", false},
		{callerReach{}, "scope-a", false},
		{callerReach{none: true}, "", true},
		{callerReach{none: true}, "scope-a", false},
		{reach("scope-a"), "scope-a", true},
		{reach("scope-a"), "", false},
		{reach("scope-a"), "Scope-a", false},
		{installationReach, "", true},
		{installationReach, "scope-z", true},
	} {
		if got := c.reach.reaches(c.scope); got != c.reaches {
			t.Errorf("%+v reaches %q: %v", c.reach, c.scope, got)
		}
	}
}

// A token whose claim is refused is answered as every identity refusal is,
// 401 with its reason, before any route, and the reason repeats nothing of
// the token; nothing is journaled.
func TestATokenWhoseScopeClaimIsRefusedIsNotAdmitted(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	before := journalLength(t, w.s)
	for _, c := range []struct {
		name  string
		claim any
		want  string
	}{
		{"wider than the entry", []string{"scope-a", "scope-secret"}, "the token's scope_ids names a scope identifier the configuration does not grant its subject"},
		{"out of form", []string{"scope-secret", "scope-secret"}, "the token's scope_ids is not 1 to 256 scope identifiers, each once"},
	} {
		for _, path := range []string{"/v1/status", "/v1/jobs/" + w.a.job + "?scope=scope-a"} {
			got := w.serve(t, w.token("ops-service", c.claim), "GET", path, "", nil)
			if got.Code != 401 || got.Header().Get("WWW-Authenticate") != "Bearer" || got.Body.String() != `{"message":"Runner authentication required: `+c.want+`."}`+"\n" {
				t.Errorf("%s, %s: %d %v %s", c.name, path, got.Code, got.Header(), got.Body)
			}
		}
	}
	if after := journalLength(t, w.s); after != before {
		t.Fatalf("the refusals wrote %d entries", after-before)
	}
}

// A caller held to scopes, asking about a record of a scope it does not
// reach (another scope identifier, or the none scope), on every route that
// names a record, is answered byte for byte as the same caller about a
// record the store does not hold, and nothing is changed or journaled; about
// a record of a scope it reaches, the route serves it.
func TestACallerIsHeldToItsScopesOnEveryRouteThatNamesARecord(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	routes := scopeRoutes()
	before := journalLength(t, w.s)
	for _, r := range routes {
		for _, c := range []struct {
			name, token string
			records     scopedRecords
			scope       string
		}{
			{"a caller of scope-a about scope-b's records", w.ofA, w.b, "scope-b"},
			{"a caller of scope-a about the none scope's records", w.ofA, w.none, ""},
			{"a caller of the none scope about scope-a's records", w.ofNone, w.a, "scope-a"},
			{"a caller of the none scope about scope-b's records", w.ofNone, w.b, "scope-b"},
			{"build-bot claiming scope-a about the none scope's records", w.token("build-bot", []string{"scope-a"}), w.none, ""},
		} {
			missing := scopedRecords{"job_missing", "run_missing", "trg_missing", "occ_missing", c.records.token}
			method, path, body, header := r.request(c.records)
			asked := withScope(path, c.scope)
			got := w.serve(t, c.token, method, asked, body, header)
			method, path, body, header = r.request(missing)
			want := w.serve(t, c.token, method, withScope(path, c.scope), body, header)
			if wholeAnswer(got) != wholeAnswer(want) {
				t.Errorf("%s %s (%s): answered\n%s\nand for a record the store does not hold\n%s", r.pattern, c.name, asked, wholeAnswer(got), wholeAnswer(want))
			}
			if got.Code < 400 {
				t.Errorf("%s %s (%s): served %d", r.pattern, c.name, asked, got.Code)
			}
		}
	}
	if after := journalLength(t, w.s); after != before {
		t.Fatalf("the refused requests wrote %d entries", after-before)
	}
	for _, r := range routes {
		if !r.reaches {
			continue
		}
		for _, c := range []struct {
			token   string
			records scopedRecords
			scope   string
		}{{w.ofA, w.a, "scope-a"}, {w.ofNone, w.none, ""}} {
			method, path, body, header := r.request(c.records)
			got := w.serve(t, c.token, method, withScope(path, c.scope), body, header)
			missing := scopedRecords{"job_missing", "run_missing", "trg_missing", "occ_missing", c.records.token}
			method, path, body, header = r.request(missing)
			want := w.serve(t, c.token, method, withScope(path, c.scope), body, header)
			if got.Code == want.Code || got.Code >= 400 {
				t.Errorf("%s under %q: %d %s, and %d for a record the store does not hold", r.pattern, c.scope, got.Code, got.Body, want.Code)
			}
		}
	}
	// The installation's caller reaches every scope's records.
	for _, c := range []struct {
		records scopedRecords
		scope   string
	}{{w.a, "scope-a"}, {w.b, "scope-b"}, {w.none, ""}} {
		if got := w.serve(t, w.installation, "GET", withScope("/v1/jobs/"+c.records.job, c.scope), "", nil); got.Code != 200 {
			t.Errorf("the installation's caller about %q's job: %d %s", c.scope, got.Code, got.Body)
		}
	}
}

// The store-wide listings keep only the records of the scope a caller held
// to scopes is held to: the scope named, if it reaches it; naming none, the
// none scope's, if it reaches that; otherwise none, answered as a listing of
// a scope that holds no record. The installation's caller and the owner
// bearer list every scope's naming none, as before.
func TestACallersListingsHoldOnlyTheScopesItReaches(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	for _, listing := range []string{"/v1/jobs", "/v1/runs", "/v1/events"} {
		empty := serve(t, w.s, "GET", listing+"?scope=scope-held-by-no-record", "", nil)
		if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"items":[]`) {
			t.Fatalf("%s of a scope no record carries: %d %s", listing, empty.Code, empty.Body)
		}
		for _, c := range []struct {
			name, token, scope string
		}{
			{"a caller of scope-a naming none", w.ofA, ""},
			{"a caller of scope-a naming scope-b", w.ofA, "scope-b"},
			{"a caller of the none scope naming scope-a", w.ofNone, "scope-a"},
		} {
			got := w.serve(t, c.token, "GET", withScope(listing, c.scope), "", nil)
			if got.Code != 200 || got.Body.String() != empty.Body.String() {
				t.Errorf("%s, %s: %d %s, want %s", listing, c.name, got.Code, got.Body, empty.Body)
			}
		}
	}
	lists := func(token, path string, want, not []string) {
		t.Helper()
		var got *httptest.ResponseRecorder
		if token == "" {
			got = serve(t, w.s, "GET", path, "", nil)
		} else {
			got = w.serve(t, token, "GET", path, "", nil)
		}
		if got.Code != 200 {
			t.Fatalf("%s: %d %s", path, got.Code, got.Body)
		}
		for _, s := range want {
			if !strings.Contains(got.Body.String(), s) {
				t.Errorf("%s does not list %s: %s", path, s, got.Body)
			}
		}
		for _, s := range not {
			if strings.Contains(got.Body.String(), s) {
				t.Errorf("%s lists %s: %s", path, s, got.Body)
			}
		}
	}
	scopeA, scopeB := `"scope":"scope-a"`, `"scope":"scope-b"`
	lists(w.ofA, "/v1/jobs?scope=scope-a", []string{w.a.job}, []string{w.b.job, w.none.job})
	lists(w.ofA, "/v1/runs?scope=scope-a", []string{w.a.run}, []string{w.b.run, w.none.run})
	lists(w.ofA, "/v1/events?scope=scope-a", []string{w.a.job}, []string{w.b.job, w.none.job, scopeB})
	lists(w.ofNone, "/v1/jobs", []string{w.none.job}, []string{w.a.job, w.b.job})
	lists(w.ofNone, "/v1/runs", []string{w.none.run}, []string{w.a.run, w.b.run})
	lists(w.ofNone, "/v1/events", []string{w.none.job}, []string{w.a.job, w.b.job, scopeA, scopeB})
	for _, token := range []string{w.installation, ""} {
		lists(token, "/v1/jobs", []string{w.a.job, w.b.job, w.none.job}, nil)
		lists(token, "/v1/runs", []string{w.a.run, w.b.run, w.none.run}, nil)
		lists(token, "/v1/events", []string{w.a.job, w.b.job, w.none.job}, nil)
	}
}

// A job is created only under a scope identifier, or under none, that its
// caller reaches: otherwise 403 scope_not_allowed, naming neither scope,
// whatever the store holds, and not journaled.
func TestACallerCreatesAJobOnlyUnderAScopeItReaches(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	creation := func(scope string) string {
		member := ""
		if scope != "" {
			member = fmt.Sprintf(`,"scope":%q`, scope)
		}
		return fmt.Sprintf(`{"name":"Held","releaseId":%q,"reviewed":true%s}`, journalRelease(t, w.s, w.runtime).ID, member)
	}
	// Each body names a release of its own, previewed before the journal is
	// counted.
	underB, underNone, underA := creation("scope-b"), creation(""), creation("scope-a")
	before := journalLength(t, w.s)
	want := `{"error":{"code":"scope_not_allowed","message":"This caller's identity does not allow a job under the scope identifier named, or under none when none is named.","retryable":false}}` + "\n"
	for _, c := range []struct {
		name, token, body string
	}{
		{"a caller of scope-a under scope-b", w.ofA, underB},
		{"a caller of scope-a under none", w.ofA, underNone},
		{"a caller of the none scope under scope-a", w.ofNone, underA},
		// A release the store does not hold: the refusal depends on nothing
		// stored.
		{"a caller of scope-a under scope-b, of a release not held", w.ofA, `{"name":"Held","releaseId":"release_missing","reviewed":true,"scope":"scope-b"}`},
	} {
		got := w.serve(t, c.token, "POST", "/v1/jobs", c.body, nil)
		if got.Code != 403 || got.Body.String() != want {
			t.Errorf("%s: %d %s", c.name, got.Code, got.Body)
		}
	}
	if after := journalLength(t, w.s); after != before {
		t.Fatalf("the refused creations wrote %d entries", after-before)
	}
	for _, c := range []struct {
		name, token, body, scope string
	}{
		{"a caller of scope-a under scope-a", w.ofA, creation("scope-a"), `"scope":"scope-a"`},
		{"a caller of the none scope under none", w.ofNone, creation(""), ""},
		{"the installation's caller under scope-b", w.installation, creation("scope-b"), `"scope":"scope-b"`},
	} {
		got := w.serve(t, c.token, "POST", "/v1/jobs", c.body, nil)
		if got.Code != 201 || c.scope != "" && !strings.Contains(got.Body.String(), c.scope) || c.scope == "" && strings.Contains(got.Body.String(), `"scope"`) {
			t.Errorf("%s: %d %s", c.name, got.Code, got.Body)
		}
	}
}

// POST /v1/ticks and the status's triggersOutsideSettings span every scope:
// a caller held to scopes is refused the ticks, 403
// installation_caller_required, before its scope or body is read, and is not
// given the member. The installation's caller and the owner bearer are, as
// before. The status says whether callers are checked.
func TestTheInstallationsRoutesAreServedOnlyToItsCaller(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	w.s.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	w.s.heldSchedules = &heldSchedules{at: now()}
	want := `{"error":{"code":"installation_caller_required","message":"This route acts for every scope identifier: it is served only to the installation's caller.","retryable":false}}` + "\n"
	before := journalLength(t, w.s)
	for _, c := range []struct {
		name, token, path, body string
	}{
		{"a caller of scope-a", w.ofA, "/v1/ticks", `{"interval":"15m"}`},
		{"a caller of the none scope", w.ofNone, "/v1/ticks", `{"interval":"15m"}`},
		{"build-bot claiming scope-a", w.token("build-bot", []string{"scope-a"}), "/v1/ticks", `{"interval":"15m"}`},
		{"a caller of scope-a naming its scope", w.ofA, "/v1/ticks?scope=scope-a", `{"interval":"15m"}`},
		{"a caller of scope-a naming a scope out of form", w.ofA, "/v1/ticks?scope=", `{"interval":"15m"}`},
		{"a caller of scope-a with a body out of form", w.ofA, "/v1/ticks", `{"interval":`},
	} {
		got := w.serve(t, c.token, "POST", c.path, c.body, nil)
		if got.Code != 403 || got.Body.String() != want {
			t.Errorf("%s: %d %s", c.name, got.Code, got.Body)
		}
	}
	if after := journalLength(t, w.s); after != before {
		t.Fatalf("the refused ticks wrote %d entries", after-before)
	}
	if got := w.serve(t, w.installation, "POST", "/v1/ticks", `{"interval":"15m"}`, nil); got.Code != 200 {
		t.Errorf("the installation's caller's tick: %d %s", got.Code, got.Body)
	}
	if got := serve(t, w.s, "POST", "/v1/ticks", `{"interval":"15m"}`, nil); got.Code != 200 {
		t.Errorf("the owner bearer's tick: %d %s", got.Code, got.Body)
	}
	for _, c := range []struct {
		name, token string
		held        bool
	}{
		{"a caller of scope-a", w.ofA, false},
		{"a caller of the none scope", w.ofNone, false},
		{"the installation's caller", w.installation, true},
	} {
		got := w.serve(t, c.token, "GET", "/v1/status", "", nil)
		if got.Code != 200 || strings.Contains(got.Body.String(), `"triggersOutsideSettings"`) != c.held || !strings.Contains(got.Body.String(), `"callerScopes":"checked"`) {
			t.Errorf("%s: %d %s", c.name, got.Code, got.Body)
		}
	}
	if got := serve(t, w.s, "GET", "/v1/status", "", nil); !strings.Contains(got.Body.String(), `"triggersOutsideSettings"`) || strings.Contains(got.Body.String(), "callerScopes") {
		t.Errorf("the owner bearer's status: %d %s", got.Code, got.Body)
	}
	if note := w.id.CallerScopesNote(); note != "" {
		t.Errorf("checked caller scopes are noted at start: %q", note)
	}
}

// "unchecked" keeps the behavior before caller scopes: every caller reaches
// what the installation reaches, its claim unread, and the start and the
// status say so.
func TestUncheckedCallerScopesServeEveryCallerAsBefore(t *testing.T) {
	w := newCallerWorld(t, callerScopes{unchecked: true})
	w.s.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}}
	w.s.heldSchedules = &heldSchedules{at: now()}
	for _, token := range []string{w.ofNone, w.token("ops-service", []string{"scope-c"}), w.token("ops-service", "scope-a")} {
		if got := w.serve(t, token, "GET", "/v1/jobs/"+w.b.job+"?scope=scope-b", "", nil); got.Code != 200 {
			t.Errorf("scope-b's job: %d %s", got.Code, got.Body)
		}
		if got := w.serve(t, token, "GET", "/v1/jobs", "", nil); got.Code != 200 || !strings.Contains(got.Body.String(), w.a.job) || !strings.Contains(got.Body.String(), w.b.job) || !strings.Contains(got.Body.String(), w.none.job) {
			t.Errorf("the jobs: %d %s", got.Code, got.Body)
		}
		if got := w.serve(t, token, "POST", "/v1/jobs", fmt.Sprintf(`{"name":"Any","releaseId":%q,"reviewed":true,"scope":"scope-b"}`, journalRelease(t, w.s, w.runtime).ID), nil); got.Code != 201 {
			t.Errorf("a creation under scope-b: %d %s", got.Code, got.Body)
		}
		if got := w.serve(t, token, "POST", "/v1/ticks", `{"interval":"15m"}`, nil); got.Code != 200 {
			t.Errorf("a tick: %d %s", got.Code, got.Body)
		}
		if got := w.serve(t, token, "GET", "/v1/status", "", nil); !strings.Contains(got.Body.String(), `"callerScopes":"unchecked"`) || !strings.Contains(got.Body.String(), `"triggersOutsideSettings"`) {
			t.Errorf("the status: %d %s", got.Code, got.Body)
		}
	}
	if note := w.id.CallerScopesNote(); !strings.Contains(note, "caller scopes are unchecked") {
		t.Errorf("unchecked caller scopes are not noted at start: %q", note)
	}
	if note := (*Identity)(nil).CallerScopesNote(); note != "" {
		t.Errorf("no identity is noted at start: %q", note)
	}
}

// On a kind that holds scopes, a caller held to scopes reaches rows only
// through a view held to the scope its identity allows: the scope named when
// it reaches it, and otherwise one no record carries; never the store for
// every scope, and never the scope the request named alone. A refused
// creation or tick opens no view held to the scope named.
func TestACallerHeldToScopesReachesOnlyTheViewItsIdentityAllows(t *testing.T) {
	w := newCallerWorld(t, heldToScopeA())
	base := w.s.store
	o := &onlyThroughHeld{base: base}
	w.s.store = o
	t.Cleanup(func() { w.s.store = base })
	unheldView := scopeHeld(unheldScope)
	for _, c := range []struct {
		name, token, method, path, body string
		code                            int
		held                            []storeScope
	}{
		{"scope-a's listing naming none", w.ofA, "GET", "/v1/jobs", "", 200, []storeScope{unheldView}},
		{"scope-a's listing naming scope-b", w.ofA, "GET", "/v1/jobs?scope=scope-b", "", 200, []storeScope{unheldView}},
		{"scope-a's listing naming scope-a", w.ofA, "GET", "/v1/runs?scope=scope-a", "", 200, []storeScope{scopeHeld("scope-a")}},
		{"scope-a's journal naming none", w.ofA, "GET", "/v1/events", "", 200, []storeScope{unheldView}},
		{"the none scope's listing naming none", w.ofNone, "GET", "/v1/jobs", "", 200, []storeScope{scopeHeld("")}},
		{"the none scope's journal naming none", w.ofNone, "GET", "/v1/events", "", 200, []storeScope{scopeHeld("")}},
		{"the none scope's listing naming scope-a", w.ofNone, "GET", "/v1/jobs?scope=scope-a", "", 200, []storeScope{unheldView}},
		{"scope-b's job by scope-a's caller", w.ofA, "GET", "/v1/jobs/" + w.b.job + "?scope=scope-b", "", 404, []storeScope{unheldView}},
		{"scope-b's run's retry by scope-a's caller", w.ofA, "POST", "/v1/runs/" + w.b.run + "/retry?scope=scope-b", `{"attempt":0}`, 404, []storeScope{unheldView}},
		{"scope-b's journal by scope-a's caller", w.ofA, "GET", "/v1/events?job=" + w.b.job + "&scope=scope-b", "", 404, []storeScope{unheldView}},
		{"scope-a's job by its caller", w.ofA, "GET", "/v1/jobs/" + w.a.job + "?scope=scope-a", "", 200, []storeScope{scopeHeld("scope-a")}},
		{"a creation under scope-b by scope-a's caller", w.ofA, "POST", "/v1/jobs", `{"name":"Held","releaseId":"release_missing","reviewed":true,"scope":"scope-b"}`, 403, []storeScope{unheldView}},
		{"a creation under scope-a by its caller", w.ofA, "POST", "/v1/jobs", fmt.Sprintf(`{"name":"Held","releaseId":%q,"reviewed":true,"scope":"scope-a"}`, journalRelease(t, w.s, w.runtime).ID), 201, []storeScope{unheldView, scopeHeld("scope-a")}},
		{"a tick by scope-a's caller", w.ofA, "POST", "/v1/ticks", `{"interval":"15m"}`, 403, nil},
	} {
		o.held = nil
		got, every := func() (got *httptest.ResponseRecorder, every bool) {
			defer func() {
				if p := recover(); p != nil {
					every = true
				}
			}()
			return w.serve(t, c.token, c.method, c.path, c.body, nil), false
		}()
		if every {
			t.Errorf("%s read or wrote the store for every scope", c.name)
			continue
		}
		if got.Code != c.code || !reflect.DeepEqual(o.held, c.held) {
			t.Errorf("%s: %d %s, held to %+v; want %d, held to %+v", c.name, got.Code, got.Body, o.held, c.code, c.held)
		}
	}
	// The installation's caller lists every scope's records, through the
	// store for every scope, as the owner bearer does.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the installation's caller's listing naming none was held to a scope")
			}
		}()
		w.serve(t, w.installation, "GET", "/v1/jobs", "", nil)
	}()
}

// On the postgresql kind, a caller held to scopes is held by row security to
// the scope its identity allows: its listings and journal keep only that
// scope's rows, another scope's job reads absent, its creation's transaction
// is held to a scope it reaches, and a request naming no scope it reaches
// reads no scoped row, the installation's shared rows still served.
func TestPostgreSQLHoldsACallerToTheScopesItsIdentityAllows(t *testing.T) {
	_, _, sv, jobs := pgScopedRunner(t)
	a, b := jobs["team-a"], jobs["team-b"]
	runtime := completingRuntime(t, sv.cfg.Dir)
	unscoped := serve(t, sv, "POST", "/v1/jobs", `{"name":"Unscoped","releaseId":"`+journalRelease(t, sv, runtime).ID+`","reviewed":true}`, nil)
	if unscoped.Code != 201 {
		t.Fatal(unscoped.Code, unscoped.Body.String())
	}
	var none Job
	must(t, json.Unmarshal(unscoped.Body.Bytes(), &none))
	signer := edSigner(t, "k1")
	id := testIdentity(t, signer)
	id.scopes = callerScopes{entries: map[string]callerEntry{"ops-service": {scopes: map[string]bool{"team-a": true}}, "build-bot": {installation: true}}}
	h := sv.ServiceHandler("", id)
	token := func(subject string, scopes any) string {
		claims := claimsFor(subject, time.Now())
		if scopes != nil {
			claims[scopeClaim] = scopes
		}
		return tokenOf(signer, claims)
	}
	ofA, ofNone := token("ops-service", []string{"team-a"}), token("ops-service", nil)
	call := func(tok, method, path, body string, want int) string {
		t.Helper()
		got := serveAs(t, h, method, path, body, tok, nil)
		if got.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, got.Code, got.Body, want)
		}
		return got.Body.String()
	}
	holds := func(body string, want []string, not ...string) {
		t.Helper()
		for _, s := range want {
			if !strings.Contains(body, s) {
				t.Errorf("does not hold %s: %s", s, body)
			}
		}
		for _, s := range not {
			if strings.Contains(body, s) {
				t.Errorf("holds %s: %s", s, body)
			}
		}
	}
	empty := call(ofA, "GET", "/v1/jobs?scope=team-c", "", 200)
	holds(empty, []string{`"items":[]`})
	for _, path := range []string{"/v1/jobs", "/v1/jobs?scope=team-b"} {
		if got := call(ofA, "GET", path, "", 200); got != empty {
			t.Errorf("%s by team-a's caller: %s, want %s", path, got, empty)
		}
	}
	holds(call(ofA, "GET", "/v1/jobs?scope=team-a", "", 200), []string{a.ID}, b.ID, none.ID)
	holds(call(ofNone, "GET", "/v1/jobs", "", 200), []string{none.ID}, a.ID, b.ID)
	holds(call(ofA, "GET", "/v1/events?scope=team-a", "", 200), []string{a.ID}, b.ID, none.ID)
	holds(call(ofNone, "GET", "/v1/events", "", 200), []string{none.ID}, a.ID, b.ID, `"scope":"team-`)
	holds(call(token("build-bot", nil), "GET", "/v1/jobs", "", 200), []string{a.ID, b.ID, none.ID})
	missing := call(ofA, "GET", "/v1/jobs/job_missing?scope=team-b", "", 404)
	if got := call(ofA, "GET", "/v1/jobs/"+b.ID+"?scope=team-b", "", 404); got != missing {
		t.Errorf("team-b's job by team-a's caller: %s, want %s", got, missing)
	}
	call(ofA, "GET", "/v1/jobs/"+none.ID, "", 404)
	call(ofA, "GET", "/v1/jobs/"+a.ID+"?scope=team-a", "", 200)
	call(ofA, "POST", "/v1/jobs", `{"name":"Elsewhere","releaseId":"`+journalRelease(t, sv, runtime).ID+`","reviewed":true,"scope":"team-b"}`, 403)
	holds(call(ofA, "POST", "/v1/jobs", `{"name":"Mine","releaseId":"`+journalRelease(t, sv, runtime).ID+`","reviewed":true,"scope":"team-a"}`, 201), []string{`"scope":"team-a"`})
	call(ofA, "POST", "/v1/ticks", `{"interval":"15m"}`, 403)
	// The installation's shared rows, read on a view that reaches no scoped
	// row.
	call(ofA, "GET", "/v1/run-chain", "", 200)
	call(ofA, "GET", "/v1/input-profiles", "", 200)
	holds(call(ofA, "GET", "/v1/status", "", 200), []string{`"callerScopes":"checked"`})
}
