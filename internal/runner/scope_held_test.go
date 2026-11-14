package runner

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// onlyThroughHeld is a store that holds scopes and answers nothing but
// through the views heldTo makes, as a request on the postgresql kind must
// reach rows: any other read or write of it panics (its runStore is nil),
// which every route's request below recovers as a read or write for every
// scope. heldTo records the scopes it was held to.
type onlyThroughHeld struct {
	runStore
	base runStore
	held []storeScope
}

func (o *onlyThroughHeld) holdsScopes() bool { return true }

func (o *onlyThroughHeld) heldTo(sc storeScope) runStore {
	o.held = append(o.held, sc)
	return o.base.heldTo(sc)
}

// throughHeld serves a request on w's Service, its store o, and says whether
// the route read or wrote the store other than through a held view.
func throughHeld(t *testing.T, w *coverWorld, method, path, body string, header map[string]string) (got *responseOf, everyScope bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			everyScope = true
		}
	}()
	res := serve(t, w.s, method, path, body, header)
	return &responseOf{res.Code, res.Body.String()}, false
}

type responseOf struct {
	code int
	body string
}

// Section 6: on a kind that holds scopes, each route the Runner serves
// reaches rows only through the view of the store held to the scope the
// request names (none, "", when it names none), the scope check's read
// included; a store-wide listing naming no scope reads every scope's; every
// other route is refused, 501 store_route_unsupported, before it reads
// anything.
func TestAKindThatHoldsScopesServesItsRoutesOnlyThroughTheirHeldView(t *testing.T) {
	w := newCoverWorld(t)
	queued, _, err := w.s.submit(w.job.ID, "first", journalInput())
	must(t, err)
	fresh := journalRelease(t, w.s, completingRuntime(t, w.cfg.Dir))
	base := w.s.store
	o := &onlyThroughHeld{base: base}
	w.s.store = o
	t.Cleanup(func() { w.s.store = base })

	key := map[string]string{"Idempotency-Key": "second"}
	for _, c := range []struct {
		method, path, body string
		header             map[string]string
		code               int
	}{
		{"GET", "/v1/status", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID, "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "?scope=other", "", nil, 404},
		{"GET", "/v1/jobs/" + w.job.ID + "/runs", "", nil, 200},
		{"GET", "/v1/runs/" + queued.ID, "", nil, 200},
		{"GET", "/v1/jobs?scope=other", "", nil, 200},
		{"GET", "/v1/runs?scope=other", "", nil, 200},
		{"POST", "/v1/jobs/" + w.job.ID + "/runs", string(encode(journalInput())), key, 202},
		{"POST", "/v1/jobs/" + w.job.ID + "/runs", `{"facts":`, key, 400},
		{"POST", "/v1/jobs", fmt.Sprintf(`{"name":"Held","releaseId":%q,"reviewed":true}`, fresh.ID), nil, 201},
		{"POST", "/v1/jobs", fmt.Sprintf(`{"name":"Held again","releaseId":%q,"reviewed":true}`, fresh.ID), nil, 409},
		{"POST", "/v1/jobs/" + w.job.ID + "/upgrade", fmt.Sprintf(`{"baseRevision":1,"baseReleaseId":%q,"releaseId":%q,"reviewed":true}`, w.release.ID, w.release.ID), nil, 422},
		{"GET", fmt.Sprintf("/v1/jobs/%s/upgrade-receipt?baseRevision=1&baseReleaseId=%s&releaseId=%s", w.job.ID, w.release.ID, fresh.ID), "", nil, 404},
	} {
		got, every := throughHeld(t, w, c.method, c.path, c.body, c.header)
		if every {
			t.Errorf("%s %s read or wrote the store for every scope", c.method, c.path)
			continue
		}
		if got.code != c.code {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, got.code, got.body, c.code)
		}
	}
	// A release's save, the end of POST /v1/previews, is held to none.
	o.held = nil
	if err := w.s.saveRelease(Release{SchemaVersion: "1", ID: id("release_"), Pack: journalPack, PackDigest: digest([]byte(journalPack)), PackID: "journal", PackVersion: "1.0.0", Title: "Journal", RuntimeDigest: fresh.RuntimeDigest, CreatedAt: now(), Sample: journalInput(), Tests: "not-run"}); err != nil {
		t.Fatal(err)
	}
	if len(o.held) != 1 || o.held[0] != scopeHeld("") {
		t.Fatal("a release's save is not held to no scope:", o.held)
	}
	// A listing naming no scope lists every scope's records: it reads the
	// store for every scope.
	if _, every := throughHeld(t, w, "GET", "/v1/jobs", "", nil); !every {
		t.Fatal("a listing naming no scope was held to one")
	}
	// Every other route is refused before it reads anything.
	for _, route := range []string{"GET /v1/events", "GET /v1/runs/" + queued.ID + "/diagnostics", "POST /v1/runs/" + queued.ID + "/retry", "GET /v1/jobs/" + w.job.ID + "/triggers", "GET /v1/run-chain", "GET /v1/input-profiles"} {
		method, path, _ := strings.Cut(route, " ")
		got, every := throughHeld(t, w, method, path, "", nil)
		if every || got.code != http.StatusNotImplemented || !strings.Contains(got.body, "store_route_unsupported") {
			t.Errorf("%s: %v %+v, want 501 store_route_unsupported", route, every, got)
		}
	}
}
