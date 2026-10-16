package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// One gateway source under two classes (runner #100, spec/dialect.md §5.1):
// "A profile bound to a research source's capability serves the model's
// answer and is classed generated for that binding. The same gateway web
// source bound to a web source is record; its profile for that binding
// carries that class." An installation holds the stand-in's web source, its
// key, source and authority, under two profiles: page, of class generated,
// which a research source's page capability is bound to, and web-record, of
// class record, which an operation is bound to. Runner refuses the kind web
// by name and reads the gateway's web source as an operation of shape http
// (refusedKind). Each binding takes its profile's class.

// webPageItem is the public page both bindings read.
const webPageItem = "person-0012"

// recordPageProfile is the stand-in's public page source under a profile of
// class record: the gateway source pageProfile pins, its key, source and
// authority, its shape, adapter and endpoint, with no research declaration.
func (h *researchStandIn) recordPageProfile(id, path string) InputProfile {
	p := h.profile(id, "web", "adapter-web", path, nil)
	p.Class = "record"
	return p
}

// twoClassesPlan is a run of one research source, person, asking for answer
// and page, its page bound to the profile page, of class generated, and its
// read copying two found values. The installation holds the profile
// web-record too, of class record, for the gateway source page pins, which
// withRecordPage binds an operation to.
func twoClassesPlan(h *researchStandIn) researchPlan {
	return researchPlan{
		example:    "two-classes",
		parameters: map[string]Parameter{"personNumber": {Pointer: "/personNumber", Type: "integer"}},
		caseDoc:    `{"personNumber":12,` + researchRequestCase + `}`,
		sources: []researchSourcePlan{{
			name:         "person",
			capabilities: []string{"answer", "page"},
			question:     `{"$text":"Describe the synthetic person numbered {{personNumber}}: their identifier and whether they may be contacted."}`,
			constraints:  `{"language":"en","synthetic":true}`,
			evidence:     []string{"the page that states whether the person may be contacted"},
			fields: []map[string]any{
				declare("person-id", "text", true, "The person's synthetic identifier."),
				declare("contactable", "boolean", true, "Whether the person may be contacted."),
			},
			read: `{"copy":{"facts":[{"target":"/person/id","source":"/fields/person-id/value"},{"target":"/qualification/contactable","source":"/fields/contactable/value"}],"evidence":[]}}`,
			ai:   "assistant", web: map[string]string{"page": "page"},
			asks: []researchAsk{{"answer", "assistant", ""}, {"page", "page", webPageItem}},
			answers: map[string]any{
				"person-id":   found("synthetic-person-0012", 0, 1),
				"contactable": found(true, 1),
			},
		}},
		admits:   []string{"/person/id", "/qualification/contactable"},
		profiles: []InputProfile{h.aiProfile("assistant", "/answers"), h.pageProfile("page", "/page"), h.recordPageProfile("web-record", "/page")},
	}
}

// profileNamed is the docs' profile of id.
func profileNamed(t *testing.T, profiles []InputProfile, id string) InputProfile {
	t.Helper()
	for _, p := range profiles {
		if p.ID == id {
			return p
		}
	}
	t.Fatal("no profile", id)
	return InputProfile{}
}

// withRecordPage adds to the docs an unbound operation, page-record, after
// the research source: a read of the public page the research source's page
// capability read, through the gateway's web source (shape http, record
// adapter-web), its request the one that page response committed, {url},
// and its read copying the page's body to /webPage, which admits record by
// default and not generated. The binding document binds it to the profile
// named; its acquisition is the stand-in's, through that profile, in the
// session the research responses are in.
func withRecordPage(t *testing.T, d *researchDocs, profile string) {
	t.Helper()
	p := profileNamed(t, d.profiles, profile)
	page := *p.Endpoint + "?item=" + webPageItem
	d.dialect["sources"] = append(d.dialect["sources"].([]any), map[string]any{
		"name": "page-record", "kind": "operation", "maxAge": json.Number("3600"),
		"arguments": map[string]any{"url": page},
		"read":      map[string]any{"copy": map[string]any{"facts": []any{map[string]any{"target": "/webPage", "source": "/body"}}, "evidence": []any{}}},
		"platform":  "web", "operation": "page.read", "shape": "http", "record": "adapter-web",
	})
	d.binding["sources"].(map[string]any)["page-record"] = map[string]any{"profile": p.ID, "profileDigest": profileHash(p)}
	retained := d.h.retain(t, p, "page", "", webPageItem, d.at)
	d.sources["page-record"] = decoded(t, encode(map[string]any{"result": retained["result"], "receipt": retained["receipt"], "salts": retained["salts"]}))
}

// twoClassesDocs is twoClassesPlan's docs with the operation bound to
// web-record. The fixture is what it says: page and web-record pin one
// gateway source and differ in class.
func twoClassesDocs(t *testing.T) *researchDocs {
	t.Helper()
	d := researchDocsOf(t, twoClassesPlan)
	withRecordPage(t, d, "web-record")
	generated, record := profileNamed(t, d.profiles, "page"), profileNamed(t, d.profiles, "web-record")
	if generated.PublicKey != record.PublicKey || generated.Source != record.Source || generated.Authority != record.Authority || generated.Class != "generated" || record.Class != "record" {
		t.Fatalf("the fixture's two profiles are not one gateway source under two classes: %+v %+v", generated, record)
	}
	return d
}

// Profile admission: one gateway source, its key, source and authority, is
// held under profiles of different classes, as it is under profiles of one
// class, each with its own id. A repeated id is refused, as it was.
func TestOneGatewaySourceMayBeHeldUnderProfilesOfEachClass(t *testing.T) {
	h := newResearchStandIn(t)
	generated, record := h.pageProfile("page", "/page"), h.recordPageProfile("web-record", "/page")
	secondRecord, secondGenerated := h.recordPageProfile("web-record-2", "/reader"), h.pageProfile("reader", "/reader")
	for name, profiles := range map[string][]InputProfile{
		"generated and record":                 {generated, record},
		"record and generated":                 {record, generated},
		"two of class record":                  {record, secondRecord},
		"two of class generated":               {generated, secondGenerated},
		"each class twice":                     {generated, record, secondRecord, secondGenerated},
		"beside another source of the gateway": {h.aiProfile("assistant", "/answers"), generated, record},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateProfileKeys(profiles); err != nil {
				t.Fatal("start-up and preparation refuse the profiles:", err)
			}
			if _, err := ParseInputProfiles(encode(profiles)); err != nil {
				t.Fatal("verify-run refuses the profiles:", err)
			}
		})
	}
	again := record
	again.ID = generated.ID
	if err := validateProfiles([]InputProfile{generated, again}); err == nil || err.Error() != "invalid installation input profile" {
		t.Fatalf("two profiles of one id were admitted: %v", err)
	}
}

// One gateway source under two classes, prepared through both: the import
// binds the research source's page to the generated profile and the
// operation to the record one; preflight and the loop's planning hold each
// source to its own profile; the preparation's lineage gives each binding its
// profile's class, and targetsByClass counts each target under it; a release
// freezes both profiles. The class is the binding's: the same operation
// bound to the generated profile has generated targets, which /webPage does
// not admit, and a research response through the record profile is a
// profile-mismatch, since the research source is not bound to it.
func TestOneGatewaySourceUnderTwoClassesIsPreparedThroughBoth(t *testing.T) {
	d := twoClassesDocs(t)
	input := d.input(t)

	// Start-up: the installation's profiles are admitted.
	placeholder := filepath.Join(t.TempDir(), "never-run")
	if err := os.WriteFile(placeholder, []byte("not a Runtime: no test runs it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Dir: dir, Runtime: placeholder, Workspace: "two-classes", Owner: "owner", InputProfiles: d.profiles, GatewayConnections: []GatewayConnection{{Profile: "web-record", URL: "https://127.0.0.1:1"}}, disableAutomation: true})
	if err != nil {
		t.Fatal("start-up refuses one gateway source under two classes:", err)
	}
	t.Cleanup(s.Close)

	// Import binding: each source to its own profile.
	bound, err := bindDialect(*input.Source.Import, d.profiles)
	if err != nil {
		t.Fatal(err)
	}
	var web map[string]map[string]string
	if err = json.Unmarshal(bound.Sources[0].WebProfiles, &web); err != nil {
		t.Fatal(err)
	}
	if bound.Sources[0].Profile != "assistant" || web["page"]["profile"] != "page" || bound.Sources[1].Profile != "web-record" || bound.Sources[1].ProfileDigest != profileHash(profileNamed(t, d.profiles, "web-record")) {
		t.Fatalf("the import did not bind the research source's page to page and the operation to web-record: %s", encode(bound.Sources))
	}

	// Preflight, and the loop's admission of the operation alone.
	if err = preflightV2(bound, d.profiles); err != nil {
		t.Fatal(err)
	}
	operation := bound
	operation.Sources = bound.Sources[1:]
	if err = s.validateGatewayMapping(Release{InputMapping: &operation}); err != nil {
		t.Fatal("the loop refuses the operation through the record profile:", err)
	}

	// The loop's planning: the research source through its AI profile
	// first, then the operation through the record profile.
	planned := cloneInput(input)
	planned.Source.Sources = map[string]SourceValue{}
	plan, err := nextInput(planned, d.profiles, d.at)
	if err != nil || plan.Next == nil || plan.Next.Name != "person" || plan.Next.Profile != "assistant" {
		t.Fatalf("the first planned source is not the research source: %+v %v", plan.Next, err)
	}
	planned.Source.Sources["person"] = input.Source.Sources["person"]
	plan, err = nextInput(planned, d.profiles, d.at)
	if err != nil || plan.Next == nil || plan.Next.Name != "page-record" || plan.Next.Profile != "web-record" || plan.Next.Source != "web" || string(plan.Next.Arguments) != string(encode(map[string]string{"url": *profileNamed(t, d.profiles, "web-record").Endpoint + "?item=" + webPageItem})) {
		t.Fatalf("the operation is not planned through the record profile: %+v %v", plan.Next, err)
	}

	// The preparation: each lineage entry takes its binding's class.
	prepared, err := prepareV2(input, d.profiles, d.at, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/request": "asserted", "intake-form": "asserted", "sponsor-endorsement": "asserted", "/person/id": "generated", "/qualification/contactable": "generated", "/webPage": "record"}
	if len(prepared.Preparation.Lineage) != len(want) {
		t.Fatalf("%d lineage entries, not %d: %s", len(prepared.Preparation.Lineage), len(want), encode(prepared.Preparation.Lineage))
	}
	for _, l := range prepared.Preparation.Lineage {
		if want[l.Target] != l.Class || !l.Present {
			t.Fatalf("the lineage of %s from %s is of class %s, present %v; not %s", l.Target, l.Source, l.Class, l.Present, want[l.Target])
		}
	}
	if got := classesOf(prepared.Preparation.Lineage); got != (InputClasses{Asserted: 3, Record: 1, Generated: 2}) {
		t.Fatalf("targetsByClass is %+v", got)
	}
	if string(prepared.Facts) != `{"person":{"id":"synthetic-person-0012"},"qualification":{"contactable":true},"request":{"appropriateness":"pass","completeness":"complete","embargoedInformationToUnauthorizedRecipients":false,"type":"data-access"},"webPage":"A synthetic page about person-0012. Every figure on it is invented for a test."}` {
		t.Fatalf("facts: %s", prepared.Facts)
	}
	// Three receipts of one session: the answer and the page through
	// generated profiles, and the page again through the record one.
	if len(prepared.Preparation.Cites) != 3 {
		t.Fatalf("%d citations, not 3", len(prepared.Preparation.Cites))
	}

	// A release freezes both profiles, and holds a run to each.
	used := profilesUsed(bound, d.profiles)
	if len(used) != 3 || used[1].ID != "page" || used[2].ID != "web-record" {
		t.Fatalf("the release freezes %s", encode(used))
	}
	if err = s.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: used}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"page", "web-record"} {
		changed := append([]InputProfile(nil), d.profiles...)
		for i := range changed {
			if changed[i].ID == id {
				changed[i].Adapter.Version = "changed"
			}
		}
		other := &Service{cfg: Config{InputProfiles: changed}}
		if err = other.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: used}); err == nil {
			t.Fatalf("a changed %s was taken as the profile the release froze", id)
		}
	}
	for _, kept := range [][]InputProfile{used[:2], {used[0], used[2]}} {
		if err = s.checkReleaseProfiles(Release{InputMapping: &bound, InputProfiles: kept}); err == nil {
			t.Fatalf("a release that froze only %s was taken as freezing both", encode(kept))
		}
	}

	t.Run("the operation bound to the generated profile", func(t *testing.T) {
		d := researchDocsOf(t, twoClassesPlan)
		withRecordPage(t, d, "page")
		_, err := d.prepare(t)
		var problem *apiError
		if !errors.As(err, &problem) || problem.Message != "source page-record: fact target does not admit source class or generated dependency" {
			t.Fatalf("the operation's targets were not of its profile's class, generated: %v", err)
		}
	})
	t.Run("a research response through the record profile", func(t *testing.T) {
		d := twoClassesDocs(t)
		record := profileNamed(t, d.profiles, "web-record")
		d.retained("person", 1)["profile"] = map[string]any{"id": record.ID, "digest": profileHash(record)}
		_, err := d.prepare(t)
		refusedFor(t, err, codeMismatch)
	})
	t.Run("the research source's page bound to the record profile", func(t *testing.T) {
		d := twoClassesDocs(t)
		record := profileNamed(t, d.profiles, "web-record")
		d.assignment("person")["webProfiles"].(map[string]any)["page"] = map[string]any{"profile": record.ID, "profileDigest": profileHash(record)}
		d.retained("person", 1)["profile"] = map[string]any{"id": record.ID, "digest": profileHash(record)}
		_, err := d.prepare(t)
		refusedFor(t, err, codeMismatch)
	})
}

// A run of one gateway source under two classes is evaluated, signed,
// witnessed and verified offline: its release freezes both profiles,
// verify-run is given both, out of band, and counts each target under the
// class its binding's profile gives it. Given the generated profile alone,
// it refuses the release's record profile as not independently trusted.
func TestARunOfOneGatewaySourceUnderTwoClassesIsVerified(t *testing.T) {
	run := testConfig(t)
	d := twoClassesDocs(t)
	keyPath := signingKey(t)
	run.SigningKey = keyPath
	run.Runtime = signingRuntimeWithKey(t, run.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
	run.InputProfiles = d.profiles
	bundle := signedAndWitnessedExportOf(t, run, d.input(t), d.profiles, "Two classes", "two-classes")
	frozen := map[string]string{}
	for _, p := range bundle.Release.InputProfiles {
		frozen[p.ID] = p.Class
	}
	if len(frozen) != 3 || frozen["page"] != "generated" || frozen["web-record"] != "record" {
		t.Fatalf("the release froze %s", encode(bundle.Release.InputProfiles))
	}
	v, err := VerifyInputs(encode(bundle), d.profiles, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if v.Classes != (InputClasses{Asserted: 3, Record: 1, Generated: 2}) {
		t.Fatalf("targetsByClass is %+v", v.Classes)
	}
	for _, l := range bundle.Run.Input.Preparation.Lineage {
		if l.Source == "page-record" && l.Class != "record" || l.Source == "person" && l.Class != "generated" {
			t.Fatalf("the lineage of %s from %s is of class %s", l.Target, l.Source, l.Class)
		}
	}
	generatedOnly := []InputProfile{profileNamed(t, d.profiles, "assistant"), profileNamed(t, d.profiles, "page")}
	if _, err = VerifyInputs(encode(bundle), generatedOnly, bundle.ReleaseDigest); err == nil || err.Error() != "release profile is not independently trusted" {
		t.Fatalf("verify-run took the record profile from the export: %v", err)
	}
}
