package runner

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// selectedBody is the request body of i, its case and its producer response
// kept as written (their digests are of those bytes), as scopedEntry keeps them.
func selectedBody(i Input) string {
	raw := encode(i)
	if i.Source != nil {
		if len(i.Source.Case) > 0 {
			raw = bytes.Replace(raw, encode(i.Source.Case), i.Source.Case, 1)
		}
		if v, ok := i.Source.Sources["desk"]; ok && len(v.Response) > 0 {
			raw = bytes.Replace(raw, encode(v.Response), v.Response, 1)
		}
	}
	return string(raw)
}

// serveSelected serves a request under the retained-column prefix with the
// exact installed selector, as the Desk sends one.
func serveSelected(t *testing.T, s *Service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, s, method, "/v1/retained-column/1"+strings.TrimPrefix(path, "/v1"), body, map[string]string{"JPack-Conversion-Contract": selectedHeader(selectedContract())})
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error.Code
}

func wantRefusal(t *testing.T, what string, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || errorCode(t, w) != code {
		t.Fatalf("%s: want %d %s, got %d %s", what, status, code, w.Code, w.Body.String())
	}
}

// fillOrdinary registers 16 ordinary profiles, ordinary first among them.
func fillOrdinary(t *testing.T, s *Service, ordinary InputProfile) {
	t.Helper()
	for i := 0; i < 16; i++ {
		p := ordinary
		if i > 0 {
			p.ID = fmt.Sprintf("ordinary%d", i)
		}
		if _, err := s.registerInputProfile(p); err != nil {
			t.Fatal(err)
		}
	}
}

// selectedNamed is the fixture's selected producer profile under another id.
func selectedNamed(f producerFixture, id string) InputProfile {
	p := f.profile
	p.ID = id
	return p
}

func TestSelectedProfilesBesideFullOrdinaryRegistry(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	ordinaryInput, ordinary, _, _ := v2Fixture(t)
	fillOrdinary(t, s, ordinary)
	if _, err := s.registerInputProfile(selectedNamed(selectedProducer(t), "seventeenth")); err == nil || !strings.Contains(err.Error(), "At most 16") {
		t.Fatal("the ordinary ceiling moved:", err)
	}

	f := selectedProducer(t)
	w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(f.profile)))
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	doc := readOpenAPI(t)
	if problems := doc.check("admission", doc.schema("InstalledInputProfile"), decodeNumbers(t, w.Body.Bytes())); len(problems) != 0 {
		t.Fatal(problems)
	}
	if w = serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(f.profile))); w.Code != http.StatusOK {
		t.Fatal("an identical admission is not idempotent:", w.Code, w.Body.String())
	}

	// Selected preparation plans the next source, and then prepares the
	// supplied response completely.
	partial := cloneInput(f.input)
	partial.Source.Sources = nil
	w = serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(partial))
	var plan InputPlan
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan.Next == nil || !plan.Next.RetainedColumn || plan.Next.Profile != f.profile.ID {
		t.Fatal("selected planning:", w.Code, w.Body.String())
	}
	w = serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(f.input))
	plan = InputPlan{}
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan.Input == nil || plan.Input.Preparation == nil {
		t.Fatal("selected preparation:", w.Code, w.Body.String())
	}

	// The ordinary registry is whole and works as before.
	if got := s.inputProfiles(); len(got) != 16 || got[0].ID != ordinary.ID {
		t.Fatal("ordinary registry changed:", len(got))
	}
	w = serve(t, s, "POST", "/v1/inputs/next", string(encode(ordinaryInput)), nil)
	plan = InputPlan{}
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan.Input == nil {
		t.Fatal("ordinary planning:", w.Code, w.Body.String())
	}
	var listing []map[string]any
	_ = json.Unmarshal(serve(t, s, "GET", "/v1/input-profiles", "", nil).Body.Bytes(), &listing)
	if len(listing) != 16 {
		t.Fatal("the ordinary listing holds a selected profile:", len(listing))
	}

	// The capability report: the status names it, the namespace reports its
	// contract, its bound and its profiles.
	var status struct{ Capabilities []string }
	_ = json.Unmarshal(serve(t, s, "GET", "/v1/status", "", nil).Body.Bytes(), &status)
	if !strings.Contains(strings.Join(status.Capabilities, " "), "selected-profile-preparation") {
		t.Fatal("status does not report selected preparation:", status.Capabilities)
	}
	w = serveSelected(t, s, "GET", "/v1/selected-profiles", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if problems := doc.check("report", doc.schema("SelectedProfiles"), decodeNumbers(t, w.Body.Bytes())); len(problems) != 0 {
		t.Fatal(problems)
	}
	var report struct {
		Contract string
		Bound    struct{ Profiles, ProfilesBytes int }
		Profiles []json.RawMessage
	}
	if err := strictJSON(w.Body.Bytes(), &report); err != nil || report.Contract != selectedPreparationContract || report.Bound.Profiles != 16 || report.Bound.ProfilesBytes != columnProfilesLimit || len(report.Profiles) != 1 {
		t.Fatal("report:", w.Body.String())
	}
}

func TestSelectedProfilesOwnBound(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	f := selectedProducer(t)
	for i := 0; i < selectedProfileBound; i++ {
		if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, fmt.Sprintf("selected%d", i))))); w.Code != http.StatusCreated {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, "overflow"))))
	wantRefusal(t, "one past the selected bound", w, 422, codeSelectedLimit)
	if got := s.selectedInputProfiles(); len(got) != selectedProfileBound {
		t.Fatal("selected profiles were evicted or added:", len(got))
	}
	// The ordinary registry keeps all its 16 beside a full namespace.
	ordinary, _ := testProfile(t)
	fillOrdinary(t, s, ordinary)
	if len(s.inputProfiles()) != 16 {
		t.Fatal("a full namespace narrowed the ordinary registry")
	}
	// The bound of bytes holds the stored row to profilesBytes, the bound a
	// retained preparation holds its profiles to.
	half := json.RawMessage(`"` + strings.Repeat("x", selectedProfilesBytes/2) + `"`)
	if selectedRowBound([]json.RawMessage{half}) != nil {
		t.Fatal("a row within the bound of bytes refused")
	}
	if e := selectedRowBound([]json.RawMessage{half, half}); e == nil {
		t.Fatal("a row over the bound of bytes admitted")
	}
	small := make([]json.RawMessage, selectedProfileBound)
	for i := range small {
		small[i] = json.RawMessage(`{}`)
	}
	if selectedRowBound(small) != nil || selectedRowBound(append(small, json.RawMessage(`{}`))) == nil {
		t.Fatal("the bound of profiles is not 16")
	}
}

func TestSelectedProfilesRefusalsBothWays(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer func() { s.Close() }()
	f := selectedProducer(t)
	ordinaryInput, ordinary, _, _ := v2Fixture(t)
	if _, err := s.registerInputProfile(ordinary); err != nil {
		t.Fatal(err)
	}
	// An ordinary producer profile, which a selection could otherwise admit.
	if _, err := s.registerInputProfile(selectedNamed(f, "ordinaryproducer")); err != nil {
		t.Fatal(err)
	}
	if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(f.profile))); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}

	// Ordinary into selected: an ordinary id is never selected, a profile
	// that is not a retained producer is never admitted, and selected
	// planning refuses an input that names an ordinary profile.
	wantRefusal(t, "an ordinary id selected", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, "ordinaryproducer")))), 409, codeSelectedOrdinary)
	notProducer, _ := testProfile(t)
	notProducer.ID = "plain"
	wantRefusal(t, "a profile that is not a producer", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(notProducer))), 422, codeSelectedUnsupported)
	unpinned := selectedNamed(f, "unpinned")
	unpinned.RetainedColumnProducer = nil
	wantRefusal(t, "a producer-carrier profile without the producer pin", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(unpinned))), 422, codeSelectedUnsupported)
	noCapability := selectedNamed(f, "nocapability")
	noCapability.ConversionContracts = nil
	wantRefusal(t, "a producer without the capability", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(noCapability))), 422, codeSelectedUnsupported)
	namesOrdinary := cloneInput(ordinaryInput)
	namesOrdinary.Source.ConversionContract = selectedContract()
	wantRefusal(t, "selected planning of an ordinary profile", serveSelected(t, s, "POST", "/v1/selected-inputs/next", string(encode(namesOrdinary))), 422, codeSelectedOrdinary)
	viaOrdinaryProducer := cloneInput(f.input)
	viaOrdinaryProducer.Source.Mapping.Sources[0].Profile = "ordinaryproducer"
	wantRefusal(t, "selected planning of an ordinary producer", serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(viaOrdinaryProducer)), 422, codeSelectedOrdinary)

	// Selected into ordinary: no ordinary route binds a selected profile,
	// and no ordinary registration takes its id.
	wantRefusal(t, "ordinary retained planning", serveSelected(t, s, "POST", "/v1/inputs/next", selectedBody(f.input)), 422, codeInputProfileSelected)
	wantRefusal(t, "ordinary retained preview of input", serveSelected(t, s, "POST", "/v1/inputs/preview", selectedBody(f.input)), 422, codeInputProfileSelected)
	plain := cloneInput(f.input)
	plain.Source.ConversionContract = nil
	wantRefusal(t, "ordinary planning", serve(t, s, "POST", "/v1/inputs/next", selectedBody(plain), nil), 422, codeInputProfileSelected)
	wantRefusal(t, "ordinary registration of a selected id", serve(t, s, "POST", "/v1/input-profiles", string(encode(f.profile)), nil), 409, codeInputProfileSelected)
	if e := s.refuseSetAsideMapping(&f.input.Source.Mapping); e == nil {
		t.Fatal("a release or job mapping binds a selected profile")
	}

	// The routes are served only under the prefix, with the exact selector
	// and its producer-carrier role, which is checked before the body.
	for _, route := range []struct{ method, path string }{{"GET", "/v1/selected-profiles"}, {"POST", "/v1/selected-profiles"}, {"POST", "/v1/selected-inputs/next"}} {
		wantRefusal(t, route.path+" unselected", serve(t, s, route.method, route.path, string(encode(f.profile)), nil), 422, codeSelectedUnselected)
		short := selectedContract()
		short.Roles = []string{"document-validation", "preview", "preparation"}
		body := &unreadBody{}
		req := httptest.NewRequest(route.method, "/v1/retained-column/1"+strings.TrimPrefix(route.path, "/v1"), body)
		req.Header.Set("Authorization", "Bearer "+journalBearer)
		req.Header.Set("JPack-Conversion-Contract", selectedHeader(short))
		w := httptest.NewRecorder()
		s.Handler(journalBearer).ServeHTTP(w, req)
		if w.Code != 422 || errorCode(t, w) != "capability-unsupported" || body.reads != 0 {
			t.Fatal(route.path, "without producer-carrier:", w.Code, w.Body.String(), body.reads)
		}
	}

	// Immutable once admitted, across a restart too.
	changed := f.profile
	changed.Authority = "another authority"
	wantRefusal(t, "a changed selected profile", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(changed))), 409, codeSelectedConflict)
	s.Close()
	s = openJournal(t, cfg)
	if got := s.selectedInputProfiles(); len(got) != 1 || profileHash(got[0]) != profileHash(f.profile) {
		t.Fatal("selected profile changed across restart:", got)
	}
	wantRefusal(t, "a changed selected profile after restart", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(changed))), 409, codeSelectedConflict)
	wantRefusal(t, "ordinary planning after restart", serve(t, s, "POST", "/v1/inputs/next", selectedBody(plain), nil), 422, codeInputProfileSelected)
	// A configured profile cannot take a selected id either.
	s.Close()
	cfg.InputProfiles = []InputProfile{f.profile}
	if other, err := Open(cfg); err == nil {
		other.Close()
		t.Fatal("a configured profile took a selected id")
	}
	cfg.InputProfiles = nil
	s = openJournal(t, cfg)
}

func TestSelectedProfilesReachNoJobReleaseOrRun(t *testing.T) {
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	f := selectedProducer(t)
	if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(f.profile))); w.Code != http.StatusCreated {
		t.Fatal(w.Code, w.Body.String())
	}
	w := serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(f.input))
	var plan InputPlan
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan.Input == nil {
		t.Fatal(w.Code, w.Body.String())
	}
	// Preparation persisted nothing: no release, job or run.
	for _, table := range []string{"releases", "jobs", "runs"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
	// No other method or path under the namespace's names is served.
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/selected-inputs/next"}, {"PUT", "/v1/selected-profiles"}, {"DELETE", "/v1/selected-profiles"},
		{"POST", "/v1/selected-profiles/reviewed/jobs"}, {"POST", "/v1/selected-jobs"}, {"POST", "/v1/selected-inputs/preview"},
		{"POST", "/v1/selected-previews"}, {"POST", "/v1/selected-inputs/next/runs"},
	} {
		if w := serveSelected(t, s, route.method, route.path, "{}"); w.Code != 422 || errorCode(t, w) != "capability-unsupported" {
			t.Fatal(route.method, route.path, w.Code, w.Body.String())
		}
	}
	// What selected preparation prepared binds no ordinary preview, release,
	// job or run: each resolves the ordinary profiles, which refuse it.
	prepared := *plan.Input
	wantRefusal(t, "prepared input previewed", serveSelected(t, s, "POST", "/v1/inputs/preview", selectedBody(prepared)), 422, codeInputProfileSelected)
	if e := s.refuseSetAside(prepared); e == nil {
		t.Fatal("a run's submission binds a selected profile")
	}
}

// The handwritten public vector: the contract's names, routes and selector
// roles, an empty namespace's report and the named refusals.
func TestSelectedProfilesHandwrittenVector(t *testing.T) {
	raw, err := os.ReadFile("../../vectors/selected-profile-preparation.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Status, Contract, StatusCapability string
		SelectorRoles                      []string
		Routes                             []struct{ Method, Path string }
		EmptyReport                        json.RawMessage
		Refusals                           []struct {
			Code, When string
			Status     int
		}
	}
	if err = strictJSON(raw, &v); err != nil || v.Status != "handwritten-test-only" || v.Contract != selectedPreparationContract {
		t.Fatal("vector:", err)
	}
	s := openJournal(t, journalConfig(t))
	defer s.Close()
	var status struct{ Capabilities []string }
	_ = json.Unmarshal(serve(t, s, "GET", "/v1/status", "", nil).Body.Bytes(), &status)
	found := false
	for _, c := range status.Capabilities {
		found = found || c == v.StatusCapability
	}
	if !found {
		t.Fatal("status does not name", v.StatusCapability)
	}
	selector := func(roles []string) *ConversionContract {
		c := selectedContract()
		c.Roles = roles
		return c
	}
	for _, route := range v.Routes {
		for skip := -1; skip < len(v.SelectorRoles); skip++ {
			var roles []string
			for i, role := range v.SelectorRoles {
				if i != skip {
					roles = append(roles, role)
				}
			}
			// A body that names the same selection, so that what follows the
			// selector refuses on its own.
			body := string(encode(Input{Source: &SourceInput{ConversionContract: selector(roles)}}))
			w := serve(t, s, route.Method, route.Path, body, map[string]string{"JPack-Conversion-Contract": selectedHeader(selector(roles))})
			refused := w.Code == 422 && errorCode(t, w) == "capability-unsupported"
			if refused != (skip >= 0) || w.Code == 404 || w.Code == 405 {
				t.Fatal(route.Method, route.Path, roles, w.Code, w.Body.String())
			}
		}
	}
	var want, got any
	_ = json.Unmarshal(v.EmptyReport, &want)
	_ = json.Unmarshal(serveSelected(t, s, "GET", "/v1/selected-profiles", "").Body.Bytes(), &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatal("empty report:", got)
	}
	codes := map[string]bool{}
	for _, r := range v.Refusals {
		codes[r.Code] = true
		if r.When == "" || r.Status != 409 && r.Status != 422 {
			t.Fatal("refusal:", r)
		}
	}
	for _, code := range []string{codeSelectedUnselected, codeSelectedUnsupported, codeSelectedConflict, codeSelectedOrdinary, codeSelectedLimit, codeInputProfileSelected} {
		if !codes[code] {
			t.Fatal("the vector does not name", code)
		}
		delete(codes, code)
	}
	if len(codes) != 0 {
		t.Fatal("the vector names refusals Runner does not:", codes)
	}
}

// The namespace's row when the store opens: a stored profile this Runner's
// selection refuses on its own is set aside, kept and never bound; a row
// Runner did not write refuses to open the store.
func TestSelectedProfilesStoredRowAtOpen(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	f := selectedProducer(t)
	refused, _ := testProfile(t)
	refused.ID = "refused"
	_, ordinary, _, _ := v2Fixture(t)
	if _, err := s.registerInputProfile(ordinary); err != nil {
		t.Fatal(err)
	}
	put := func(row string) {
		t.Helper()
		if _, err := s.db.Exec("INSERT INTO metadata(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", selectedProfilesKey, row); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	distinct := func(n int, authority string) string {
		var stored []json.RawMessage
		for i := 0; i < n; i++ {
			p := selectedNamed(f, fmt.Sprintf("stored%d", i))
			p.Authority = authority
			stored = append(stored, encode(p))
		}
		return storedRow(stored)
	}
	for name, row := range map[string]string{
		"not a profile":           `[null]`,
		"not an array":            `{}`,
		"a member unknown":        strings.Replace(string(encode([]InputProfile{f.profile})), `"id":`, `"extra":1,"id":`, 1),
		"an id twice":             storedRow([]json.RawMessage{encode(f.profile), encode(f.profile)}),
		"an ordinary id":          storedRow([]json.RawMessage{encode(ordinary)}),
		"seventeen stored":        distinct(17, f.profile.Authority),
		"over the bound of bytes": distinct(9, strings.Repeat("a", 10<<10)),
	} {
		put(row)
		if other, err := Open(cfg); err == nil {
			other.Close()
			t.Fatal(name, "opened")
		}
		s = openJournalWithout(t, cfg)
	}
	put(storedRow([]json.RawMessage{encode(refused), encode(f.profile)}))
	s = openJournal(t, cfg)
	defer func() { s.Close() }()
	if got := s.selectedInputProfiles(); len(got) != 1 || got[0].ID != f.profile.ID {
		t.Fatal("admitted:", got)
	}
	w := serveSelected(t, s, "GET", "/v1/selected-profiles", "")
	if problems := readOpenAPI(t).check("report", readOpenAPI(t).schema("SelectedProfiles"), decodeNumbers(t, w.Body.Bytes())); len(problems) != 0 || !strings.Contains(w.Body.String(), `"state":"set-aside"`) {
		t.Fatal(problems, w.Body.String())
	}
	namesRefused := cloneInput(f.input)
	namesRefused.Source.Mapping.Sources[0].Profile = refused.ID
	wantRefusal(t, "planning a set-aside selected profile", serveSelected(t, s, "POST", "/v1/selected-inputs/next", selectedBody(namesRefused)), 422, codeSetAside)
	wantRefusal(t, "re-admitting a set-aside selected id", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, refused.ID)))), 409, codeSelectedConflict)
	wantRefusal(t, "registering a set-aside selected id", serve(t, s, "POST", "/v1/input-profiles", string(encode(refused)), nil), 409, codeInputProfileSelected)
	wantRefusal(t, "ordinary planning of a set-aside selected id", serve(t, s, "POST", "/v1/inputs/next", selectedBody(func() Input { i := cloneInput(namesRefused); i.Source.ConversionContract = nil; return i }()), nil), 422, codeInputProfileSelected)
	// It still counts toward the bound: 15 more fill the namespace.
	for i := 0; i < selectedProfileBound-2; i++ {
		if w := serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, fmt.Sprintf("more%d", i))))); w.Code != http.StatusCreated {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	wantRefusal(t, "past the bound with one set aside", serveSelected(t, s, "POST", "/v1/selected-profiles", string(encode(selectedNamed(f, "overflow")))), 422, codeSelectedLimit)
}

// openJournalWithout opens the store after clearing the namespace's row, as
// a test that has just refused a row opens it again.
func openJournalWithout(t *testing.T, cfg Config) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(cfg.Dir, "runs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM metadata WHERE key=?", selectedProfilesKey); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return openJournal(t, cfg)
}
