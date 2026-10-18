package runner

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The endpoint scope of RFC 0018 round 15 (spec/dialect.md §5.1, §5.2 check
// 3, §11 and §12; judgment-pack-spec#207; runner #141), its page path held to
// round 16's form (judgment-pack-spec#215; runner #152). The cases are the
// specification's own probes, tests/test_dialect_scope.py, held here on
// Runner's two paths: the offline verifier, given its profiles as a
// fixture's, and a run, given them as an installation's. The change's
// record runs the specification's reader beside verify-preparation on every
// one of them.

// scopePrefix is the prefix the specification's probes scope a page
// profile to.
const scopePrefix = "https://a.example/docs"

// round16Refused are the paths under scopePrefix that round 15's form admitted
// and round 16's refuses, the specification's ROUND_16_REFUSED in its order:
// an escaped slash or backslash anywhere in the path, or a dot segment before
// its segment's first ;, %3b, %3B, %3f, %3F, %23 or %00.
var round16Refused = []string{
	"/a%2fb", "/a%2Fb", "/a%5cb", "/a%5Cb", "/%2e%2e%2fadmin", "/..;x", "/..;/admin",
	"/%2e%2e;x", "/%2E%2e;/admin", "/.;", "/.;/admin", "/.%2e;y=1", "/x/..;",
	"/..%3b/admin", "/%2e%2e%3B/admin", "/.%3fx", "/..%3F", "/..%23x", "/.%00/x",
	"/..;;x", "/..;a=b;c/admin", "/..;x%3by", "/..%23x;y",
}

// round16Admitted are the neighbours round 16 still admits, endpoint and
// scope alike: a ; on an ordinary segment, dots after a segment's first ; or
// escaped delimiter, three dots before a parameter and an escaped dot inside
// a segment.
var round16Admitted = []string{"/page;v=1", "/a;b", "/x;../y", "/...;x", "/a%2eb", "/x%3b../y"}

// scopeVector is the vector the probes alter: its person source asks for
// answer, search and page, its page through the profile page.
const scopeVector = "gtm-company-person-valid-types-all-six"

// A string in the constrained page form (§5.1) is read as the text gives it,
// nothing decoded: the host folded to lower case for comparison alone, the
// path as written.
func TestThePageEndpointForm(t *testing.T) {
	longest := "https://a.example/" + strings.Repeat("x", 2048-len("https://a.example/"))
	for endpoint, want := range map[string][2]string{
		"https://a.example":                               {"a.example", ""},
		"https://a.example/":                              {"a.example", "/"},
		"https://A.EXAMPLE:443/docs":                      {"a.example", "/docs"},
		"https://a.example:/docs/x":                       {"a.example", "/docs/x"},
		"https://a.example:443":                           {"a.example", ""},
		"https://a.example:":                              {"a.example", ""},
		"https://a.example/docs/%7e/x":                    {"a.example", "/docs/%7e/x"},
		"https://a.example/@name":                         {"a.example", "/@name"},
		"https://a.example/...":                           {"a.example", "/..."},
		"https://a.example/.x/x.":                         {"a.example", "/.x/x."},
		"https://a.example//x":                            {"a.example", "//x"},
		"https://a-b.c1.example/x":                        {"a-b.c1.example", "/x"},
		"https://1a.example":                              {"1a.example", ""},
		"https://" + strings.Repeat("a", 63) + ".example": {strings.Repeat("a", 63) + ".example", ""},
		longest: {"a.example", longest[len("https://a.example"):]},
	} {
		host, path, ok := pageEndpoint(endpoint)
		if !ok || host != want[0] || path != want[1] {
			t.Errorf("%s reads as %v %q %q, not %q %q", endpoint, ok, host, path, want[0], want[1])
		}
	}
	// Round 16's neighbours: what follows a segment's first ; or escaped
	// delimiter is no part of its dot-segment test, and every other escape,
	// %2e outside a dot segment included, stays literal bytes.
	for _, path := range append(round16Admitted, "/.x;", "/x.;/..x", "/%2e%2e%2e;x", "/%3b..", "/;/..x", "/a%252fb", "/%2e%25", "/x%3B%2e%2e") {
		if host, got, ok := pageEndpoint(scopePrefix + path); !ok || host != "a.example" || got != "/docs"+path {
			t.Errorf("%s%s reads as %v %q %q", scopePrefix, path, ok, host, got)
		}
	}
	for _, endpoint := range []string{
		"", "http://a.example/docs", "HTTPS://a.example/docs", "https://", "https://a.example:444/docs",
		"https://a.example:0443/docs", "https://a.example:0/docs", "https://a.example:80/docs",
		"https://a.example:4430/docs", "https://a.example:443:443/docs", "https://user@a.example/docs",
		scopePrefix + "?x", scopePrefix + "#x", scopePrefix + "?", "https://a.example?x",
		"https://127.0.0.1/docs", "https://[::1]/docs", "https://127.1/docs", "https://0x7f.0x0.0x0.0x1/docs",
		"https://localhost/docs", "https://a.example./docs", "https://.a.example/docs", "https://a_b.example/docs",
		"https://-a.example/docs", "https://a-.example/docs", "https://a..example/docs", "https://a.123/docs",
		"https://" + strings.Repeat("a", 64) + ".example/docs", "https://é.example/docs", "https://*", "https://*/docs",
		"https://*.example/docs", scopePrefix + "/%", scopePrefix + "/%2", scopePrefix + "/%gg", scopePrefix + "/\\x",
		scopePrefix + "/x\n", scopePrefix + "/x y", scopePrefix + "/.", scopePrefix + "/..", scopePrefix + "/../x",
		scopePrefix + "/%2e/x", scopePrefix + "/%2E/x", scopePrefix + "/.%2E/x", scopePrefix + "/%2E%2e/x",
		scopePrefix + "/%2e./x", scopePrefix + "/x/..", "https://a.example/./",
		"https://a.example/" + strings.Repeat("x", 2031),
		scopePrefix + "/%2Fx", scopePrefix + "%2Fx", "https://a.example/%5C", "https://a.example/.;", "https://a.example/..%00",
	} {
		if _, _, ok := pageEndpoint(endpoint); ok {
			t.Errorf("%q is read in the constrained page form", endpoint)
		}
	}
	for _, path := range round16Refused {
		if _, _, ok := pageEndpoint(scopePrefix + path); ok {
			t.Errorf("%q is read in the constrained page form", scopePrefix+path)
		}
	}
}

// A scope admits an endpoint when an entry does (§5.1, "Matching"): hosts
// equal with ASCII letters folded, the three spellings of port 443 one port,
// a path prefix at a literal-slash boundary, nothing decoded; the wildcard
// admits every endpoint in the form, and nothing outside it.
func TestEndpointScopeMatching(t *testing.T) {
	for _, tt := range []struct {
		scope    []string
		endpoint string
		in       bool
	}{
		{[]string{scopePrefix}, scopePrefix + "/one", true},
		{[]string{scopePrefix}, scopePrefix, true},
		{[]string{scopePrefix}, scopePrefix + "x", false},
		{[]string{scopePrefix}, scopePrefix + "x/one", false},
		{[]string{scopePrefix}, "https://a.example/doc", false},
		{[]string{scopePrefix}, "https://a.example", false},
		{[]string{scopePrefix}, "https://other.example/docs/one", false},
		{[]string{scopePrefix}, "https://A.EXAMPLE:443/docs/one", true},
		{[]string{scopePrefix}, "https://a.example:/docs/x", true},
		{[]string{scopePrefix}, "https://a.example:443/docs", true},
		{[]string{"https://A.EXAMPLE:443/docs"}, scopePrefix + "/one", true},
		{[]string{"https://a.example:443/docs"}, "https://a.example:/docs/x", true},
		{[]string{"https://a.example:/docs"}, "https://a.example/docs/x", true},
		{[]string{scopePrefix + "/"}, scopePrefix + "/one", true},
		{[]string{scopePrefix + "/"}, scopePrefix + "/", true},
		{[]string{scopePrefix + "/"}, scopePrefix, false},
		{[]string{"https://a.example/"}, "https://a.example", true},
		{[]string{"https://a.example/"}, "https://a.example/anything/at/all", true},
		{[]string{"https://a.example"}, "https://a.example/", true},
		{[]string{"https://a.example"}, "https://a.example:443", true},
		{[]string{"https://other.example", scopePrefix}, scopePrefix + "/one", true},
		{[]string{"https://other.example", scopePrefix}, "https://other.example/x", true},
		{[]string{"https://other.example", scopePrefix}, "https://third.example/x", false},
		{[]string{scopePrefix}, "https://a.example/Docs/one", false},
		{[]string{scopePrefix}, "https://sub.a.example/docs", false},
		{[]string{"https://sub.a.example/docs"}, scopePrefix, false},
		{[]string{scopePrefix}, scopePrefix + "%2Fx", false},
		{[]string{scopePrefix + "/%7e"}, scopePrefix + "/%7e/x", true},
		{[]string{scopePrefix + "/%7e"}, scopePrefix + "/%7E", false},
		{[]string{scopePrefix + "/%7e"}, scopePrefix + "/~", false},
		{[]string{scopePrefix + "//x"}, scopePrefix + "/x", false},
		{[]string{scopePrefix}, scopePrefix + "//x", true},
		{[]string{scopePrefix}, scopePrefix + "/@name", true},
		{[]string{scopeWildcard}, "https://new.example/a", true},
		{[]string{scopeWildcard}, "https://next.example", true},
		{[]string{scopeWildcard}, "https://a.example:/x", true},
		{[]string{scopeWildcard}, "https://a.example:444/x", false},
		{[]string{scopeWildcard}, "http://a.example/x", false},
		{[]string{scopeWildcard}, "https://127.0.0.1/x", false},
		{[]string{scopeWildcard}, "https://a.example/x?y", false},
		{[]string{scopeWildcard}, scopePrefix + "/%2e/x", false},
		{[]string{scopeWildcard}, "", false},
		{[]string{scopePrefix}, scopePrefix + "/../x", false},
		{[]string{scopePrefix}, scopePrefix + "/..;x", false},
		{[]string{scopeWildcard}, scopePrefix + "/..%3b/admin", false},
		{[]string{scopeWildcard}, scopePrefix + "/a%5cb", false},
		{[]string{scopePrefix + "/a;b"}, scopePrefix + "/a;b/x", true},
		{[]string{scopePrefix + "/a;b"}, scopePrefix + "/a;bx", false},
		{[]string{scopePrefix}, scopePrefix + "/page;v=1", true},
		{[]string{scopePrefix}, scopePrefix + "/x%3b../y", true},
		{[]string{scopePrefix + "/x"}, scopePrefix + "/x%3b../y", false},
	} {
		if got := endpointInScope(tt.endpoint, tt.scope); got != tt.in {
			t.Errorf("%v admits %q: %v, not %v", tt.scope, tt.endpoint, got, tt.in)
		}
	}
}

// scopedPageProfile is a page profile of the specification's probes'
// form, scoped: class generated, shape http, adapter adapter-web, endpoint
// null and the research capability page; change changes it.
func scopedPageProfile(change func(map[string]any)) map[string]any {
	p := unusedProfile("scoped", func(p map[string]any) {
		p["class"], p["research"], p["endpointScope"] = "generated", map[string]any{"capabilities": []any{"page"}}, []any{scopePrefix}
		p["adapter"].(map[string]any)["name"] = "adapter-web"
	})
	if change != nil {
		change(p)
	}
	return p
}

// scopeProfileForms are profiles carrying endpointScope, each with the code
// §5.1 and §12 give it, or "" for a profile admitted: one on every path
// that takes profiles.
func scopeProfileForms() []struct {
	name    string
	profile map[string]any
	code    string
} {
	set := func(member string, value any) func(map[string]any) {
		return func(p map[string]any) { p[member] = value }
	}
	record := func(p map[string]any) { p["class"] = "record"; delete(p, "research") }
	over := []any{}
	for i := range 33 {
		over = append(over, "https://site"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".example")
	}
	thirtyTwo := over[:32]
	longest := "https://a.example/" + strings.Repeat("x", 2048-len("https://a.example/"))
	return []struct {
		name    string
		profile map[string]any
		code    string
	}{
		{"a generated page profile", scopedPageProfile(nil), ""},
		{"a generated page profile under the wildcard", scopedPageProfile(set("endpointScope", []any{scopeWildcard})), ""},
		{"a record profile without research", scopedPageProfile(record), ""},
		{"a record profile declaring page", scopedPageProfile(set("class", "record")), ""},
		{"32 entries", scopedPageProfile(set("endpointScope", thirtyTwo)), ""},
		{"an entry of 2048 bytes", scopedPageProfile(set("endpointScope", []any{longest})), ""},
		{"prefixes in every spelling of the port", scopedPageProfile(set("endpointScope", []any{"https://a.example", "https://b.example:443/x", "https://c.example:/y/"})), ""},
		{"an exact endpoint beside it", scopedPageProfile(set("endpoint", scopePrefix)), codeMalformed},
		{"the adapter adapter-sources", scopedPageProfile(func(p map[string]any) { p["adapter"].(map[string]any)["name"] = "adapter-sources" }), codeMalformed},
		{"the shape airbyte", scopedPageProfile(set("shape", "airbyte")), codeMalformed},
		{"the shape command", scopedPageProfile(set("shape", "command")), codeMalformed},
		{"a declaration of search and page", scopedPageProfile(set("research", map[string]any{"capabilities": []any{"search", "page"}})), codeMalformed},
		{"a declaration of answer and page", scopedPageProfile(set("research", map[string]any{"capabilities": []any{"answer", "page"}, "model": researchModel})), codeMalformed},
		{"a generated profile without a declaration", scopedPageProfile(func(p map[string]any) { delete(p, "research") }), codeMalformed},
		{"the wildcard on a record profile", scopedPageProfile(func(p map[string]any) { record(p); p["endpointScope"] = []any{scopeWildcard} }), codeMalformed},
		{"the wildcard beside a prefix on a record profile", scopedPageProfile(func(p map[string]any) { record(p); p["endpointScope"] = []any{scopePrefix, scopeWildcard} }), codeMalformed},
		{"the wildcard on a record profile declaring page", scopedPageProfile(func(p map[string]any) { p["class"], p["endpointScope"] = "record", []any{scopeWildcard} }), codeMalformed},
		{"an empty scope", scopedPageProfile(set("endpointScope", []any{})), codeMalformed},
		{"a null scope", scopedPageProfile(set("endpointScope", nil)), codeMalformed},
		{"a scope that is an object", scopedPageProfile(set("endpointScope", map[string]any{})), codeMalformed},
		{"a scope that is a string", scopedPageProfile(set("endpointScope", scopeWildcard)), codeMalformed},
		{"an entry that is null", scopedPageProfile(set("endpointScope", []any{nil})), codeMalformed},
		{"an entry given twice", scopedPageProfile(set("endpointScope", []any{scopePrefix, scopePrefix})), codeMalformed},
		{"the wildcard with a path", scopedPageProfile(set("endpointScope", []any{"https://*/docs"})), codeMalformed},
		{"an http prefix", scopedPageProfile(set("endpointScope", []any{"http://a.example"})), codeMalformed},
		{"a dot segment", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/.."})), codeMalformed},
		{"a percent-spelled dot segment", scopedPageProfile(set("endpointScope", []any{"https://A.EXAMPLE/%2E/"})), codeMalformed},
		{"a query", scopedPageProfile(set("endpointScope", []any{scopePrefix + "?x"})), codeMalformed},
		{"an IP literal", scopedPageProfile(set("endpointScope", []any{"https://127.0.0.1"})), codeMalformed},
		{"another port", scopedPageProfile(set("endpointScope", []any{"https://a.example:444"})), codeMalformed},
		{"a trailing dot", scopedPageProfile(set("endpointScope", []any{"https://a.example./"})), codeMalformed},
		{"an escaped slash", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/a%2fb"})), codeMalformed},
		{"an escaped backslash", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/a%5Cb"})), codeMalformed},
		{"a dot segment before a parameter", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/..;x"})), codeMalformed},
		{"a dot segment before an escaped delimiter", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/..%3b/admin"})), codeMalformed},
		{"a dot segment before a parameter beside an admitted entry", scopedPageProfile(set("endpointScope", []any{scopePrefix, scopePrefix + "/.;"})), codeMalformed},
		{"a parameter on an ordinary segment", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/a;b"})), ""},
		{"dots after an escaped delimiter", scopedPageProfile(set("endpointScope", []any{scopePrefix + "/x%3b../y"})), ""},
		{"33 entries", scopedPageProfile(set("endpointScope", over)), codeOverBound},
		{"33 entries on a command profile, before its form", scopedPageProfile(func(p map[string]any) { p["shape"], p["endpointScope"] = "command", over }), codeOverBound},
		{"an entry of 2049 bytes", scopedPageProfile(set("endpointScope", []any{longest + "x"})), codeOverBound},
		{"an entry of 2049 bytes on a command profile, before its form", scopedPageProfile(func(p map[string]any) { p["shape"], p["endpointScope"] = "command", []any{longest + "x"} }), codeOverBound},
		{"an entry of 1025 two-byte characters, its bytes before its grammar", scopedPageProfile(set("endpointScope", []any{strings.Repeat("é", 1025)})), codeOverBound},
	}
}

// Every path that takes profiles admits endpointScope and refuses exactly
// what §5.1 and §12 refuse, with the code they give: the offline verifier,
// given a profile no source names beside a vector's (verify-preparation),
// and an installation's profiles, at start and as verify-run is given them
// (ParseInputProfiles), and by registration (POST /v1/input-profiles).
func TestEveryPathHoldsAProfilesEndpointScopeToItsForm(t *testing.T) {
	for _, tt := range scopeProfileForms() {
		t.Run(tt.name, func(t *testing.T) {
			f := parityFixture(t)
			f.Profiles = encode(append(profilesOf(t, f), tt.profile))
			r := VerifyPreparation(f)
			if tt.code == "" {
				requireReading(t, f)
			} else {
				requireReport(t, r, ResultRefusal, tt.code)
			}
			_, err := ParseInputProfiles(encode([]any{tt.profile}))
			if (err == nil) != (tt.code == "") {
				t.Fatalf("an installation's profile is admitted: %v, and the verifier's: %s", err, r.Line())
			}
			if err != nil {
				// A form's refusal at decoding names no code of its own, and is
				// a form Runner refuses, input-malformed.
				if _, code := verificationOutcome(err); code != tt.code {
					t.Fatalf("an installation's profile is refused as %s, not %s: %v", code, tt.code, err)
				}
			}
		})
	}
}

// Registration takes a profile carrying endpointScope, and answers it with
// its digest, the scope's whole value in it; a null or empty scope, the
// wildcard on a record profile and a scope beside an endpoint are refused,
// and nothing is installed.
func TestRegistrationTakesAnEndpointScope(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	profile := scopedPageProfile(func(p map[string]any) { p["id"] = "pages" })
	for _, tt := range []struct {
		name   string
		change func(map[string]any)
		status int
	}{
		{"null", func(p map[string]any) { p["endpointScope"] = nil }, 400},
		{"empty", func(p map[string]any) { p["endpointScope"] = []any{} }, 422},
		{"the wildcard on a record profile", func(p map[string]any) {
			p["class"], p["endpointScope"] = "record", []any{scopeWildcard}
			delete(p, "research")
		}, 422},
		{"beside an endpoint", func(p map[string]any) { p["endpoint"] = scopePrefix }, 422},
		{"an escaped slash", func(p map[string]any) { p["endpointScope"] = []any{scopePrefix + "/a%2Fb"} }, 422},
		{"a dot segment before a parameter", func(p map[string]any) { p["endpointScope"] = []any{scopePrefix + "/%2e%2e;x"} }, 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var changed map[string]any
			if err := json.Unmarshal(encode(profile), &changed); err != nil {
				t.Fatal(err)
			}
			tt.change(changed)
			if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(changed)), nil); r.Code != tt.status || len(s.inputProfiles()) != 0 {
				t.Fatal(r.Code, r.Body.String())
			}
		})
	}
	response := serve(t, s, "POST", "/v1/input-profiles", string(encode(profile)), nil)
	if response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
	var registered map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	doc := readOpenAPI(t)
	if problems := doc.check("registration", doc.schema("InstalledInputProfile"), registered); len(problems) != 0 {
		t.Fatal(problems)
	}
	if registered["digest"] != canonDigestOf(t, profile) || len(s.inputProfiles()) != 1 || !sameJSON(encode(s.inputProfiles()[0]), encode(profile)) {
		t.Fatalf("the registered profile is %s, digested %v, not the profile given, %s", encode(s.inputProfiles()), registered["digest"], canonDigestOf(t, profile))
	}
	widened := scopedPageProfile(func(p map[string]any) { p["id"], p["endpointScope"] = "pages", []any{scopePrefix, "https://b.example"} })
	if r := serve(t, s, "POST", "/v1/input-profiles", string(encode(widened)), nil); r.Code != http.StatusConflict {
		t.Fatal("a registered profile's scope was changed in place:", r.Code, r.Body.String())
	}
}

// A profile without endpointScope encodes as before, and its digest is the
// one it had: no member is added, empty or otherwise.
func TestAProfileWithoutAScopeEncodesAsBefore(t *testing.T) {
	for _, vector := range []string{"valid", "http-shaped-source", scopeVector} {
		f := profileFixture(t, vector, false)
		installed, err := ParseInputProfiles(f.Profiles)
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range installed {
			if strings.Contains(string(encode(p)), "endpointScope") || profileHash(p) != canonDigestOf(t, profilesOf(t, f)[i]) {
				t.Fatalf("%s: the profile %s encodes as %s", vector, p.ID, encode(p))
			}
		}
	}
}

// scopeProbe is one of the specification's probes: the vector's page
// profile scoped (scope, by default [scopePrefix]) and changed by profile;
// its page response retained once for each endpoint, under a request of its
// own; documents changed by documents before every digest and signature is
// made again, and by after once they are.
type scopeProbe struct {
	scope     any
	endpoints []any
	profile   func(map[string]any)
	documents func(map[string]any)
	after     func(map[string]any)
}

// scopedFixture is the probe's inputs as the specification's
// test_dialect_scope.py makes them (scoped_inputs): the vector published and
// bound at import, every profile under a key made for the test, every
// digest and receipt made again, and the preparation recomputed when the
// inputs prepare, the vector's otherwise.
func scopedFixture(t *testing.T, pr scopeProbe) (PreparationFixture, time.Time) {
	t.Helper()
	f := profileFixture(t, scopeVector, true)
	read := func(raw []byte) any { return mustRead(t, raw) }
	profiles, binding, dialect, acquisitions := read(f.Profiles).([]any), read(f.Binding).(map[string]any), read(f.Dialect).(map[string]any), read(f.Sources).(map[string]any)
	page := profiles[len(profiles)-1].(map[string]any)
	if page["id"] != "page" {
		t.Fatal("the vector's last profile is not its page profile")
	}
	page["endpoint"], page["endpointScope"] = nil, pr.scope
	if pr.scope == nil {
		page["endpointScope"] = []any{scopePrefix}
	}
	if pr.profile != nil {
		pr.profile(page)
	}
	endpoints := pr.endpoints
	if endpoints == nil {
		endpoints = []any{scopePrefix + "/one", scopePrefix + "/two"}
	}
	responses := scopeAt(acquisitions, "person", "response", "result")["responses"].([]any)
	last := responses[len(responses)-1]
	responses = responses[:len(responses)-1]
	for i, endpoint := range endpoints {
		retained := read(encode(last)).(map[string]any)
		retained["request"] = map[string]any{"url": scopePrefix + "/request-" + string(rune('0'+i)) + "?item=synthetic"}
		receipt := retained["receipt"].(map[string]any)
		receipt["acquisition"].(map[string]any)["endpoint"] = endpoint
		receipt["callIndex"] = 3 + i
		responses = append(responses, retained)
	}
	scopeAt(acquisitions, "person", "response", "result")["responses"] = responses
	documents := map[string]any{"profiles": profiles, "binding": binding, "dialect": dialect, "acquisitions": acquisitions}
	if pr.documents != nil {
		pr.documents(documents)
	}
	seed := sha256.Sum256([]byte("round 15 reader controls"))
	key := ed25519.NewKeyFromSeed(seed[:])
	public := hex.EncodeToString(key.Public().(ed25519.PublicKey))
	digests := map[string]string{}
	for _, p := range profiles {
		p.(map[string]any)["publicKey"] = public
		digests[p.(map[string]any)["id"].(string)] = canonDigestOf(t, p)
	}
	for _, assignment := range binding["sources"].(map[string]any) {
		a := assignment.(map[string]any)
		refs := []map[string]any{a}
		for _, web := range a["webProfiles"].(map[string]any) {
			refs = append(refs, web.(map[string]any))
		}
		for _, ref := range refs {
			ref["profileDigest"] = digests[ref["profile"].(string)]
		}
	}
	binding["dialect"] = canonDigestOf(t, dialect)
	for _, member := range acquisitions {
		for _, value := range scopeAt(member, "response", "result")["responses"].([]any) {
			response := value.(map[string]any)
			response["profile"].(map[string]any)["digest"] = digests[response["profile"].(map[string]any)["id"].(string)]
			receipt := response["receipt"].(map[string]any)
			receipt["keyId"] = keyID(public)
			salt, _ := hex.DecodeString(response["salts"].(map[string]any)["args"].(string))
			receipt["argumentsCommitment"] = digest(append(append(salt, "args:"...), canonTest(t, encode(response["request"]))...))
			receipt["resultDigest"] = canonDigestOf(t, response["result"])
			signReceipt(t, receipt, key)
		}
	}
	if pr.after != nil {
		pr.after(documents)
	}
	f.Profiles, f.Binding, f.Dialect, f.Sources = encode(documents["profiles"]), encode(documents["binding"]), encode(documents["dialect"]), encode(documents["acquisitions"])
	if documents["binding"] == nil {
		f.Binding = nil
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", object(t, mustRead(t, f.Preparation))["verifiedAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if computed, err := offlinePreparation(t, f, at); err == nil {
		f.Preparation = computed
	}
	return f, at
}

// offlinePreparation is the preparation the verifier recomputes from f at,
// in the canonical form, as VerifyPreparation recomputes one.
func offlinePreparation(t *testing.T, f PreparationFixture, at time.Time) ([]byte, error) {
	t.Helper()
	profiles, err := verifierProfiles(mustRead(t, f.Profiles), f.Profiles)
	if err != nil {
		return nil, err
	}
	prepared, err := prepareV2Mode(Input{Source: scopeSource(t, f)}, profiles, at, false, &[]judgedFreshness{}, true)
	if err != nil {
		return nil, err
	}
	return canonical(encode(prepared.Preparation))
}

// scopeSource is f as a run's source input: bound at import when f has a
// binding document, and given bound otherwise.
func scopeSource(t *testing.T, f PreparationFixture) *SourceInput {
	t.Helper()
	source := &SourceInput{Case: f.Case}
	if err := strictJSON(f.Sources, &source.Sources); err != nil {
		t.Fatal(err)
	}
	if f.Binding != nil {
		source.Import = &DialectImport{Dialect: f.Dialect, Binding: f.Binding}
	} else if err := strictJSON(f.Dialect, &source.Mapping); err != nil {
		t.Fatal(err)
	}
	return source
}

// scopeProbes are the specification's probes, each with its answer: ""
// for a reading, and otherwise the code of its refusal or finding.
func scopeProbes() []struct {
	name  string
	probe scopeProbe
	code  string
} {
	endpoints := func(e ...any) []any { return e }
	scope := func(e ...any) any { return e }
	retained := func(d map[string]any) map[string]any {
		responses := scopeAt(d["acquisitions"], "person", "response", "result")["responses"].([]any)
		return responses[len(responses)-1].(map[string]any)
	}
	probes := []struct {
		name  string
		probe scopeProbe
		code  string
	}{
		{"two different page endpoints", scopeProbe{}, ""},
		{"another host", scopeProbe{endpoints: endpoints("https://other.example/docs/one")}, codeInvalid},
		{"the second response outside", scopeProbe{endpoints: endpoints(scopePrefix, "https://other.example/docs")}, codeInvalid},
		{"the segment boundary", scopeProbe{endpoints: endpoints(scopePrefix + "x")}, codeInvalid},
		{"the path equal", scopeProbe{endpoints: endpoints(scopePrefix)}, ""},
		{"host case and the written port", scopeProbe{endpoints: endpoints("https://A.EXAMPLE:443/docs/one")}, ""},
		{"the written port in the scope", scopeProbe{scope: scope("https://A.EXAMPLE:443/docs")}, ""},
		{"the empty port in the endpoint", scopeProbe{endpoints: endpoints("https://a.example:/docs/x")}, ""},
		{"the empty port in the endpoint, the written one in the scope", scopeProbe{scope: scope("https://a.example:443/docs"), endpoints: endpoints("https://a.example:/docs/x")}, ""},
		{"the empty port in the scope", scopeProbe{scope: scope("https://a.example:/docs"), endpoints: endpoints("https://a.example/docs/x")}, ""},
		{"a prefix ending in a slash", scopeProbe{scope: scope(scopePrefix + "/")}, ""},
		{"a prefix ending in a slash excludes its parent", scopeProbe{scope: scope(scopePrefix + "/"), endpoints: endpoints(scopePrefix)}, codeInvalid},
		{"the root admits the absent path", scopeProbe{scope: scope("https://a.example/"), endpoints: endpoints("https://a.example")}, ""},
		{"the absent prefix path", scopeProbe{scope: scope("https://a.example"), endpoints: endpoints("https://a.example/")}, ""},
		{"a union of prefixes", scopeProbe{scope: scope("https://other.example", scopePrefix)}, ""},
		{"the wildcard", scopeProbe{scope: scope(scopeWildcard), endpoints: endpoints("https://new.example/a", "https://next.example/b")}, ""},
		{"path case", scopeProbe{endpoints: endpoints("https://a.example/Docs/one")}, codeInvalid},
		{"a host suffix", scopeProbe{endpoints: endpoints("https://sub.a.example/docs")}, codeInvalid},
		{"an escaped slash at the prefix boundary is outside the form", scopeProbe{endpoints: endpoints(scopePrefix + "%2Fx")}, codeInvalid},
		{"the escape spelled as the prefix spells it", scopeProbe{scope: scope(scopePrefix + "/%7e"), endpoints: endpoints(scopePrefix + "/%7e/x")}, ""},
		{"the escape's case differs", scopeProbe{scope: scope(scopePrefix + "/%7e"), endpoints: endpoints(scopePrefix + "/%7E")}, codeInvalid},
		{"no escape is decoded", scopeProbe{scope: scope(scopePrefix + "/%7e"), endpoints: endpoints(scopePrefix + "/~")}, codeInvalid},
		{"a repeated slash is kept", scopeProbe{scope: scope(scopePrefix + "//x"), endpoints: endpoints(scopePrefix + "/x")}, codeInvalid},
		{"an at sign in the path is no user", scopeProbe{endpoints: endpoints(scopePrefix + "/@name")}, ""},
		{"the record profile on research", scopeProbe{profile: func(p map[string]any) { p["class"] = "record" }}, codeMismatch},
		{"the wildcard on a record profile", scopeProbe{scope: scope(scopeWildcard), profile: func(p map[string]any) { p["class"] = "record" }}, codeMalformed},
		{"33 entries on a command profile, before its form", scopeProbe{scope: scope(func() []any {
			e := []any{}
			for range 33 {
				e = append(e, scopePrefix)
			}
			return e
		}()...), profile: func(p map[string]any) { p["shape"] = "command" }}, codeOverBound},
		{"the scope changed under the old pin", scopeProbe{after: func(d map[string]any) {
			profiles := d["profiles"].([]any)
			profiles[len(profiles)-1].(map[string]any)["endpointScope"] = []any{scopeWildcard}
		}}, codeMismatch},
		{"the request changed", scopeProbe{after: func(d map[string]any) {
			retained(d)["request"].(map[string]any)["url"] = scopePrefix + "/altered"
		}}, codeInvalid},
		{"the receipt's endpoint changed without its signature", scopeProbe{after: func(d map[string]any) {
			retained(d)["receipt"].(map[string]any)["acquisition"].(map[string]any)["endpoint"] = scopePrefix + "/altered"
		}}, codeInvalid},
		{"the requested URL is not scope-checked in replay", scopeProbe{documents: func(d map[string]any) {
			retained(d)["request"].(map[string]any)["url"] = "https://outside.example/start"
		}}, ""},
		{"the shape still held exactly", scopeProbe{documents: func(d map[string]any) {
			retained(d)["receipt"].(map[string]any)["acquisition"].(map[string]any)["shape"] = "mcp"
		}}, codeInvalid},
		{"the adapter still held exactly", scopeProbe{documents: func(d map[string]any) {
			retained(d)["receipt"].(map[string]any)["acquisition"].(map[string]any)["adapter"] = map[string]any{"name": "other"}
		}}, codeInvalid},
		{"the scoped profile assigned to search", scopeProbe{documents: func(d map[string]any) {
			scopeAt(d["binding"], "sources", "person", "webProfiles")["search"].(map[string]any)["profile"] = "page"
		}}, codeMismatch},
		// Rebinding the profile to an operation is a finding, even without
		// an acquisition: made once every digest is, as the probe makes it.
		{"the scoped profile bound to an operation", scopeProbe{after: func(d map[string]any) {
			dialect := d["dialect"].(map[string]any)
			delete(dialect, "admits")
			profiles := d["profiles"].([]any)
			dialect["sources"] = []any{map[string]any{"name": "operation", "kind": "operation", "profile": "page", "profileDigest": scopeDigest(profiles[len(profiles)-1]),
				"maxAge": 600, "arguments": map[string]any{"url": scopePrefix}, "read": map[string]any{"copy": map[string]any{"facts": []any{}, "evidence": []any{}}}}}
			d["binding"], d["acquisitions"] = nil, map[string]any{}
		}}, codeMismatch},
	}
	for _, endpoint := range []any{nil, "http://a.example/docs", "https://a.example:444/docs", "https://a.example:0443/docs", "https://user@a.example/docs",
		scopePrefix + "?x", "https://127.0.0.1/docs", "https://[::1]/docs", "https://localhost/docs", "https://a.example./docs",
		scopePrefix + "/%gg", scopePrefix + "/../x", scopePrefix + "/%2e/x", "https://*/docs", "HTTPS://a.example/docs", "https://a.example/" + strings.Repeat("x", 2031)} {
		probes = append(probes, struct {
			name  string
			probe scopeProbe
			code  string
		}{"the endpoint " + string(encode(endpoint))[:min(40, len(encode(endpoint)))] + " under the wildcard", scopeProbe{scope: scope(scopeWildcard), endpoints: endpoints(endpoint)}, codeInvalid})
	}
	type probe = struct {
		name  string
		probe scopeProbe
		code  string
	}
	// Round 16: each path round 15 admitted under scopePrefix and round 16
	// refuses, as a receipt's endpoint and as a scope entry; and the
	// neighbours it still admits.
	for _, path := range round16Refused {
		probes = append(probes,
			probe{"round 16: the endpoint " + path, scopeProbe{endpoints: endpoints(scopePrefix + path)}, codeInvalid},
			probe{"round 16: the scope entry " + path, scopeProbe{scope: scope(scopePrefix + path)}, codeMalformed})
	}
	return append(probes,
		probe{"round 16: a parameter on an ordinary segment", scopeProbe{endpoints: endpoints(scopePrefix + "/page;v=1")}, ""},
		probe{"round 16: a parameter in a scope entry", scopeProbe{scope: scope(scopePrefix + "/a;b"), endpoints: endpoints(scopePrefix + "/a;b/x")}, ""},
		probe{"round 16: dots after the first parameter", scopeProbe{endpoints: endpoints(scopePrefix + "/x;../y")}, ""},
		probe{"round 16: three dots with a parameter", scopeProbe{endpoints: endpoints(scopePrefix + "/...;x")}, ""},
		probe{"round 16: an escaped dot inside a segment", scopeProbe{endpoints: endpoints(scopePrefix + "/a%2eb")}, ""},
		probe{"round 16: dots after an escaped delimiter", scopeProbe{endpoints: endpoints(scopePrefix + "/x%3b../y")}, ""},
	)
}

// Each of the specification's probes reads, is refused or is found as the
// specification's reader answers it, on the offline verifier and on a run
// alike: one matcher holds every receipt under a scoped profile, and one
// form every profile. A run that reads prepares the preparation the
// verifier reads; one that refuses names the verifier's code.
func TestEndpointScopeProbesOnEveryPath(t *testing.T) {
	for _, tt := range scopeProbes() {
		t.Run(tt.name, func(t *testing.T) {
			f, at := scopedFixture(t, tt.probe)
			r := VerifyPreparation(f)
			if tt.code == "" {
				requireReading(t, f)
			} else {
				result := ResultFinding
				if refusalCodes[tt.code] {
					result = ResultRefusal
				}
				requireReport(t, r, result, tt.code)
			}
			installed, err := ParseInputProfiles(f.Profiles)
			if err == nil {
				var prepared Input
				prepared, err = prepareV2(Input{Source: scopeSource(t, f)}, installed, at, false, nil)
				if err == nil {
					if computed, _ := canonical(encode(prepared.Preparation)); string(computed) != string(f.Preparation) {
						t.Fatal("a run prepares another preparation than the verifier reads")
					}
				}
			}
			if (err == nil) != (tt.code == "") {
				t.Fatalf("a run answers %v, and the verifier %s", err, r.Line())
			}
			if err != nil {
				if _, code := verificationOutcome(err); code != tt.code {
					t.Fatalf("a run refuses it as %s, not %s: %v", code, tt.code, err)
				}
			}
		})
	}
}

// The whole scope enters the profile's digest: two profiles that differ in
// their scope alone are two profiles, and a binding that pins one is not a
// binding of the other.
func TestTheScopeEntersTheProfilesDigest(t *testing.T) {
	first, _ := scopedFixture(t, scopeProbe{})
	second, _ := scopedFixture(t, scopeProbe{scope: []any{scopeWildcard}})
	reordered, _ := scopedFixture(t, scopeProbe{scope: []any{"https://other.example", scopePrefix}})
	again, _ := scopedFixture(t, scopeProbe{scope: []any{scopePrefix, "https://other.example"}})
	page := func(f PreparationFixture) string {
		profiles := profilesOf(t, f)
		return canonDigestOf(t, profiles[len(profiles)-1])
	}
	if page(first) == page(second) || page(reordered) == page(again) || string(first.Binding) == string(second.Binding) {
		t.Fatal("a profile's scope, its entries and their order, is not in its digest")
	}
	if object(t, mustRead(t, first.Preparation))["mappingDigest"] == object(t, mustRead(t, second.Preparation))["mappingDigest"] {
		t.Fatal("a scope changed under the binding does not change the bound dialect")
	}
	requireReport(t, VerifyPreparation(withPreparation(second, first.Preparation)), ResultFinding, codeDiffers)
}

// scopeAt is the object at path in v.
func scopeAt(v any, path ...string) map[string]any {
	for _, name := range path {
		v = v.(map[string]any)[name]
	}
	return v.(map[string]any)
}

// scopeDigest is the digest of a value's canon bytes (spec/dialect.md §1).
func scopeDigest(v any) string {
	b, err := proofCanon(encode(v))
	if err != nil {
		panic(err)
	}
	return digest(b)
}

// Runner sends no page request of its own (spec/dialect.md §5.1, "Admission
// before acquisition", is the caller's duty): a research source's pages are
// read by its caller, and an operation, the one source Runner plans and
// acquires itself, cannot be bound to a profile that carries an endpoint
// scope. Its plan refuses one, profile-mismatch, before any source is
// called; the same operation under a profile of one endpoint is planned for
// acquisition.
func TestNoAcquisitionIsPlannedUnderAnEndpointScope(t *testing.T) {
	input, p, _, at := v2Fixture(t)
	input.Source.Sources = nil
	plan, err := nextInput(cloneInput(input), []InputProfile{p}, at)
	if err != nil || plan.Next == nil || plan.Next.Name != "vendor" {
		t.Fatalf("the operation is not planned under a profile of one endpoint: %+v %v", plan, err)
	}
	scoped := p
	scoped.Shape, scoped.Adapter.Name, scoped.Tools, scoped.Endpoint, scoped.EndpointScope = "http", "adapter-web", nil, nil, EndpointScope{scopePrefix}
	if _, err := ParseInputProfiles(encode([]InputProfile{scoped})); err != nil {
		t.Fatal(err)
	}
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(scoped)
	plan, err = nextInput(cloneInput(input), []InputProfile{scoped}, at)
	if _, code := verificationOutcome(err); err == nil || plan.Next != nil || code != codeMismatch {
		t.Fatalf("an operation under an endpoint scope is planned: %+v %v", plan, err)
	}
}

// §12's count comes before every limit Runner sets on an installation's
// profile: a scope of 33 entries of 1100 bytes each, a profile over Runner's
// own 32 KiB, is input-over-bound on the live path, as it is offline.
func TestAScopeOverItsCountIsOverBoundBeforeRunnersOwnLimits(t *testing.T) {
	entries := []any{}
	for i := range 33 {
		prefix := "https://a.example/" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "/"
		entries = append(entries, prefix+strings.Repeat("x", 1100-len(prefix)))
	}
	profile := scopedPageProfile(func(p map[string]any) { p["endpointScope"] = entries })
	if len(encode(profile)) <= 32<<10 {
		t.Fatal("the profile is within Runner's own limit")
	}
	_, err := ParseInputProfiles(encode([]any{profile}))
	if _, code := verificationOutcome(err); err == nil || code != codeOverBound {
		t.Fatalf("an installation's profile is refused as %v", err)
	}
	f := parityFixture(t)
	f.Profiles = encode(append(profilesOf(t, f), profile))
	requireReport(t, VerifyPreparation(f), ResultRefusal, codeOverBound)
}
