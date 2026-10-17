package runner

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The profiles a preparation verifier is given are held to spec/dialect.md
// §5.1 alone, and each is digested as given, its canon bytes (runner #124).
// Each case here agrees with the specification's reader, which the change's
// record re-runs on every one.

// profileFixture is a vector as verify-preparation is given it: bound
// (mapping.json), or, imported, published (unbound.json) with its binding
// document.
func profileFixture(t *testing.T, vector string, imported bool) PreparationFixture {
	t.Helper()
	f := parityVectorFixture(t, vector)
	if imported {
		for name, into := range map[string]*[]byte{"unbound.json": &f.Dialect, "binding.json": &f.Binding} {
			b, err := os.ReadFile(preparationVectors + vector + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			*into = b
		}
	}
	return f
}

// profilesOf is a fixture's profiles, read as the verifier reads them.
func profilesOf(t *testing.T, f PreparationFixture) []any {
	t.Helper()
	v, err := readInputJSONDepth(f.Profiles, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v.([]any)
}

// canonDigestOf is the digest of a value's canon bytes (spec/dialect.md §1).
func canonDigestOf(t *testing.T, v any) string {
	t.Helper()
	return digest(canonTest(t, encode(v)))
}

// changedProfile is f with its profile at index changed by change. Pinned,
// every document names the digest it named before. Otherwise each document
// that names the profile's digest names the digest of its canon bytes as
// changed, and the preparation's mappingDigest is the digest of the bound
// dialect so changed: f's dialect, or, for a published dialect, the vector's
// mapping.json, which its import makes.
func changedProfile(t *testing.T, f PreparationFixture, vector string, index int, change func(map[string]any), pinned bool) PreparationFixture {
	t.Helper()
	profiles := profilesOf(t, f)
	profile := profiles[index].(map[string]any)
	was := canonDigestOf(t, profile)
	change(profile)
	f.Profiles = encode(profiles)
	if pinned {
		return f
	}
	now := canonDigestOf(t, profile)
	renamed := func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), was, now)) }
	f.Dialect, f.Sources = renamed(f.Dialect), renamed(f.Sources)
	bound := f.Dialect
	if f.Binding != nil {
		f.Binding = renamed(f.Binding)
		b, err := os.ReadFile(preparationVectors + vector + "/mapping.json")
		if err != nil {
			t.Fatal(err)
		}
		bound = renamed(b)
	}
	f.Preparation = editJSON(t, f.Preparation, func(v any) any {
		v.(map[string]any)["mappingDigest"] = digest(canonTest(t, bound))
		return v
	})
	return f
}

// requireReading holds a report to a reading of the given preparation.
func requireReading(t *testing.T, f PreparationFixture) {
	t.Helper()
	r := VerifyPreparation(f)
	requireReport(t, r, ResultReading, "")
	given, err := canonical(f.Preparation)
	if err != nil || r.Digest != digest(given) {
		t.Fatalf("the reading's digest is %s, not the given preparation's", r.Digest)
	}
}

// A profile's digest is the digest of its canon bytes as given (§5.1): an
// empty tools array, which Runner's own encoding of a profile leaves out, is
// in it, wherever a profile is named, by an operation (the first probe of
// runner #124), by a research source and its retained responses, and by a
// binding document. A source that names the digest the profile had without
// it names another profile's, profile-mismatch; and a profile written with
// its members in another order and with whitespace has the digest it had.
func TestOfflineProfilesAreDigestedAsGiven(t *testing.T) {
	const research, imported = "gtm-company-person-valid-types-all-six", "unbound-bound-at-import"
	emptyTools := func(p map[string]any) { p["tools"] = []any{} }
	for _, tt := range []struct {
		name, vector string
		published    bool
		index        int
		pinned       bool
		result, code string
	}{
		{"tools [] on an http profile, the source naming its canon digest", "http-shaped-source", false, 0, false, ResultReading, ""},
		{"tools [] on an http profile, the source naming the digest without it", "http-shaped-source", false, 0, true, ResultFinding, codeMismatch},
		{"tools [] on a research source's AI profile, named by its canon digest", research, false, 0, false, ResultReading, ""},
		{"tools [] on a research source's AI profile, published and bound", research, true, 0, false, ResultReading, ""},
		{"tools [] on a research source's web profile, named by its canon digest", research, false, 1, false, ResultReading, ""},
		{"tools [] on a research source's AI profile, named by the digest without it", research, false, 0, true, ResultFinding, codeMismatch},
		{"tools [] on a profile a binding document names by its canon digest", imported, true, 0, false, ResultReading, ""},
		{"tools [] on a profile a binding document names by the digest without it", imported, true, 0, true, ResultFinding, codeMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := changedProfile(t, profileFixture(t, tt.vector, tt.published), tt.vector, tt.index, emptyTools, tt.pinned)
			if tt.result == ResultReading {
				requireReading(t, f)
				return
			}
			requireReport(t, VerifyPreparation(f), tt.result, tt.code)
		})
	}
	t.Run("members in another order, with whitespace", func(t *testing.T) {
		f := profileFixture(t, "http-shaped-source", false)
		profile := profilesOf(t, f)[0].(map[string]any)
		var members []string
		for _, name := range sortedKeys(profile) {
			members = append([]string{`"` + name + `" : ` + string(encode(profile[name]))}, members...)
		}
		f.Profiles = []byte("[ {\n " + strings.Join(members, ",\n ") + "\n} ]\n")
		requireReading(t, f)
	})
}

// unusedProfile is a profile of §5.1's form that no source names, changed by
// change.
func unusedProfile(id string, change func(map[string]any)) map[string]any {
	p := map[string]any{
		"id": id, "publicKey": "77697fc03348d5492d7d5f8fc239765c5d20da5f889724c3973c7726578f09ba", "class": "record", "source": "s", "authority": "a", "shape": "http",
		"adapter": map[string]any{"name": "adapter-http", "version": "v", "digest": "sha256:" + strings.Repeat("ab", 32)}, "endpoint": nil,
	}
	if change != nil {
		change(p)
	}
	return p
}

// sixteenUnused is sixteen profiles that no source names, of distinct ids:
// beside a vector's one, more than Runner admits of an installation.
func sixteenUnused() []any {
	profiles := []any{}
	for i := range 16 {
		profiles = append(profiles, unusedProfile("u"+string(rune('a'+i)), nil))
	}
	return profiles
}

// The verifier's profiles are held to §5.1's table and §12's bounds alone,
// as the specification's reader holds them, and not to the limits Runner
// sets on an installation's own (validateProfileKeys): a profile over any of
// those, an id with a space among them (the second probe of runner #124),
// is read. Every check §5.1 makes is kept: a member it does not list
// (runner #124's reverse), a member missing or of another form, an id given
// twice, a key the key rule refuses, an mcp profile without tools, a
// calculator's profile of another class or shape, and a research
// declaration of another form are input-malformed, and a declaration over
// §12's maxima input-over-bound.
func TestOfflineProfilesAreHeldToSection51Alone(t *testing.T) {
	with := func(profiles ...any) func(*PreparationFixture) {
		return func(f *PreparationFixture) { f.Profiles = encode(append(profilesOf(t, *f), profiles...)) }
	}
	unused := func(change func(map[string]any)) func(*PreparationFixture) { return with(unusedProfile("u", change)) }
	set := func(member string, value any) func(map[string]any) {
		return func(p map[string]any) { p[member] = value }
	}
	adapter := func(member string, value any) func(map[string]any) {
		return func(p map[string]any) { p["adapter"].(map[string]any)[member] = value }
	}
	mcp := func(p map[string]any) { p["shape"], p["tools"] = "mcp", []any{"c"} }
	tools := []any{}
	for i := range 65 {
		tools = append(tools, "t"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
	}
	for _, tt := range []struct {
		name   string
		mutate func(*PreparationFixture)
		result string
		code   string
	}{
		// Runner's own limits, which §5.1 does not set.
		{"an unused profile whose id has a space", with(unusedProfile("x y", nil)), ResultReading, ""},
		{"an unused profile whose id is of 70 characters", with(unusedProfile(strings.Repeat("p", 70), nil)), ResultReading, ""},
		{"an unused profile whose id starts with a digit", with(unusedProfile("1p", nil)), ResultReading, ""},
		{"17 profiles", with(sixteenUnused()...), ResultReading, ""},
		{"an unused profile whose source and authority are empty", unused(func(p map[string]any) { p["source"], p["authority"] = "", "" }), ResultReading, ""},
		{"an unused profile whose source is of 300 bytes", unused(set("source", strings.Repeat("s", 300))), ResultReading, ""},
		{"an unused profile whose adapter's name and version are empty", unused(func(p map[string]any) { adapter("name", "")(p); adapter("version", "")(p) }), ResultReading, ""},
		{"an unused profile whose endpoint is of 40000 bytes", unused(set("endpoint", strings.Repeat("e", 40000))), ResultReading, ""},
		{"an unused http profile with 65 tools", unused(set("tools", tools)), ResultReading, ""},
		{"an unused http profile with a tool of 300 bytes", unused(set("tools", []any{strings.Repeat("t", 300)})), ResultReading, ""},
		{"an unused calculator's profile whose name and version are empty", unused(func(p map[string]any) { mcp(p); p["calculator"] = map[string]any{"name": "", "version": ""} }), ResultReading, ""},
		// §5.1's checks.
		{"an unused profile with a member §5.1 does not list", unused(set("note", "x")), ResultRefusal, codeMalformed},
		{"an unused profile without its endpoint", unused(func(p map[string]any) { delete(p, "endpoint") }), ResultRefusal, codeMalformed},
		{"two profiles of one id", with(unusedProfile("u", nil), unusedProfile("u", nil)), ResultRefusal, codeMalformed},
		{"an empty id", with(unusedProfile("", nil)), ResultRefusal, codeMalformed},
		{"an id that is a number", unused(set("id", 7)), ResultRefusal, codeMalformed},
		{"a key of uppercase characters", unused(set("publicKey", "77697FC03348D5492D7D5F8FC239765C5D20DA5F889724C3973C7726578F09BA")), ResultRefusal, codeMalformed},
		{"a key of small order", unused(set("publicKey", "01"+strings.Repeat("00", 31))), ResultRefusal, codeMalformed},
		{"a class of another value", unused(set("class", "asserted")), ResultRefusal, codeMalformed},
		{"a source that is a number", unused(set("source", 5)), ResultRefusal, codeMalformed},
		{"a shape of another value", unused(set("shape", "grpc")), ResultRefusal, codeMalformed},
		{"an adapter with a fourth member", unused(adapter("x", "y")), ResultRefusal, codeMalformed},
		{"an adapter whose version is null", unused(adapter("version", nil)), ResultRefusal, codeMalformed},
		{"an adapter whose digest is not a digest", unused(adapter("digest", "sha256:AB")), ResultRefusal, codeMalformed},
		{"an endpoint that is false", unused(set("endpoint", false)), ResultRefusal, codeMalformed},
		{"tools given twice", unused(set("tools", []any{"a", "a"})), ResultRefusal, codeMalformed},
		{"a tool that is empty", unused(set("tools", []any{""})), ResultRefusal, codeMalformed},
		{"tools that are null", unused(set("tools", nil)), ResultRefusal, codeMalformed},
		{"an mcp profile whose tools are empty", unused(func(p map[string]any) { mcp(p); p["tools"] = []any{} }), ResultRefusal, codeMalformed},
		{"an mcp profile without tools", unused(set("shape", "mcp")), ResultRefusal, codeMalformed},
		{"a calculator on an http profile", unused(set("calculator", map[string]any{"name": "c", "version": "1"})), ResultRefusal, codeMalformed},
		{"a calculator on a generated profile", unused(func(p map[string]any) {
			mcp(p)
			p["class"], p["calculator"] = "generated", map[string]any{"name": "c", "version": "1"}
		}), ResultRefusal, codeMalformed},
		{"a calculator with a third member", unused(func(p map[string]any) {
			mcp(p)
			p["calculator"] = map[string]any{"name": "c", "version": "1", "x": "y"}
		}), ResultRefusal, codeMalformed},
		{"a research model without answer", unused(set("research", map[string]any{"capabilities": []any{"search"}, "model": "m"})), ResultRefusal, codeMalformed},
		{"a research answer without a model", unused(set("research", map[string]any{"capabilities": []any{"answer"}})), ResultRefusal, codeMalformed},
		{"research capabilities out of order", unused(set("research", map[string]any{"capabilities": []any{"search", "answer"}, "model": "m"})), ResultRefusal, codeMalformed},
		{"a research model that is empty", unused(set("research", map[string]any{"capabilities": []any{"answer"}, "model": ""})), ResultRefusal, codeMalformed},
		{"a research declaration with a third member", unused(set("research", map[string]any{"capabilities": []any{"search"}, "x": 1})), ResultRefusal, codeMalformed},
		{"four research capabilities", unused(set("research", map[string]any{"capabilities": []any{"answer", "search", "page", "page"}, "model": "m"})), ResultRefusal, codeOverBound},
		{"a research model of 257 bytes", unused(set("research", map[string]any{"capabilities": []any{"answer"}, "model": strings.Repeat("m", 257)})), ResultRefusal, codeOverBound},
		{"a profile that is a string", with("u"), ResultRefusal, codeMalformed},
		{"profiles that are an object", func(f *PreparationFixture) { f.Profiles = []byte(`{"id":"fx"}`) }, ResultRefusal, codeMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := parityFixture(t)
			tt.mutate(&f)
			if tt.result == ResultReading {
				requireReading(t, f)
				return
			}
			requireReport(t, VerifyPreparation(f), tt.result, tt.code)
		})
	}
	// A profile a source names, whose id has a space, named by its canon
	// digest: a dialect names a profile by any non-empty string.
	t.Run("a profile a source names whose id has a space", func(t *testing.T) {
		f := parityFixture(t)
		f.Dialect = []byte(strings.Replace(string(f.Dialect), `"profile":"fx"`, `"profile":"f x"`, 1))
		requireReading(t, changedProfile(t, f, "valid", 0, set("id", "f x"), false))
	})
}

// An installation's profiles keep Runner's admission and its digest on every
// live path: the limits §5.1 does not set still refuse them, and a profile
// written with an empty tools array is digested as Runner admitted it,
// without it, so a run prepares the vector under the digest its dialect
// pins and refuses the digest of the canon bytes as written,
// profile-mismatch.
func TestAnInstallationsProfilesKeepRunnersAdmissionAndDigest(t *testing.T) {
	valid := parityFixture(t)
	for name, profiles := range map[string][]any{
		"an id with a space": append(profilesOf(t, valid), unusedProfile("x y", nil)),
		"17 profiles":        append(profilesOf(t, valid), sixteenUnused()...),
	} {
		if _, err := ParseInputProfiles(encode(profiles)); err == nil {
			t.Fatalf("%s: an installation's profiles are admitted", name)
		}
	}

	f := profileFixture(t, "http-shaped-source", false)
	empty := changedProfile(t, f, "http-shaped-source", 0, func(p map[string]any) { p["tools"] = []any{} }, true)
	installed, err := ParseInputProfiles(empty.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	given, err := verifierProfiles(profilesOf(t, empty), empty.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	pinned := profileHash(installed[0])
	if canon := canonDigestOf(t, profilesOf(t, empty)[0]); profileHash(given[0]) != canon || pinned == canon || !strings.Contains(string(f.Dialect), `"profileDigest":"`+pinned+`"`) {
		t.Fatalf("an installation's profile is digested %s, and the verifier's %s: the canon bytes' digest is %s", pinned, profileHash(given[0]), canon)
	}
	prepare := func(dialect []byte) (Input, error) {
		source := &SourceInput{Case: empty.Case}
		if err := strictJSON(dialect, &source.Mapping); err != nil {
			t.Fatal(err)
		}
		if err := strictJSON(empty.Sources, &source.Sources); err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse("2006-01-02T15:04:05Z", object(t, mustRead(t, empty.Preparation))["verifiedAt"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return prepareV2(Input{Source: source}, installed, at, false, nil)
	}
	prepared, err := prepare(empty.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if computed, err := canonical(encode(prepared.Preparation)); err != nil || string(computed) != string(empty.Preparation) {
		t.Fatalf("a run does not prepare the vector under its pinned digest: %v", err)
	}
	renamed := changedProfile(t, f, "http-shaped-source", 0, func(p map[string]any) { p["tools"] = []any{} }, false)
	if _, err := prepare(renamed.Dialect); err == nil {
		t.Fatal("a run takes the digest of the canon bytes as written for an installation's profile")
	} else if _, code := verificationOutcome(err); code != codeMismatch {
		t.Fatalf("a run refuses the digest of the canon bytes as written as %s: %v", code, err)
	}
}

// mustRead is raw read as the verifier reads a document.
func mustRead(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := readInputJSONDepth(raw, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
