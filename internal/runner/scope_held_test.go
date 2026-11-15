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

// missingAttemptHoldsNoAudit is the kind's answer, which reads no row.
func (o *onlyThroughHeld) missingAttemptHoldsNoAudit() bool {
	return o.base.missingAttemptHoldsNoAudit()
}

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
	event := TriggerConfig{Name: "Reviewed records", Kind: "event", Missed: "skip", Overlap: "queue", QueueSeconds: 3600}
	trigger, err := w.s.configureTrigger(w.job.ID, "", 0, event)
	must(t, err)
	// An enabled event trigger, its key, and an occurrence it admitted.
	deliveries, token := w.eventTrigger("queue")
	occurrence, _, err := w.s.event(deliveries.ID, token, delivery("first"))
	must(t, err)
	tokenOf := map[string]string{"X-Trigger-Token": token}
	// An installation input profile, and a selected one, each served
	// through its conversion's path.
	_, ordinary, _, _ := v2Fixture(t)
	selected := selectedProducer(t).profile
	conversion := map[string]string{"JPack-Conversion-Contract": selectedHeader(selectedContract())}
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
		// Triggers.
		{"GET", "/v1/jobs/" + w.job.ID + "/triggers", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "/triggers?scope=other", "", nil, 404},
		{"POST", "/v1/jobs/" + w.job.ID + "/triggers", string(encode(map[string]any{"config": event})), nil, 200},
		{"POST", "/v1/jobs/" + w.job.ID + "/triggers", string(encode(map[string]any{"id": trigger.ID, "revision": 1, "config": event})), nil, 200},
		{"POST", "/v1/jobs/" + w.job.ID + "/triggers", string(encode(map[string]any{"id": trigger.ID, "revision": 1, "config": event})), nil, 409},
		{"POST", "/v1/jobs/" + w.job.ID + "/triggers?scope=other", string(encode(map[string]any{"config": event})), nil, 404},
		{"POST", "/v1/jobs/" + w.job.ID + "/triggers/preview", string(encode(event)), nil, 200},
		{"POST", "/v1/triggers/" + trigger.ID + "/state", `{"revision":2,"paused":false,"reviewed":true}`, nil, 200},
		{"POST", "/v1/triggers/" + trigger.ID + "/state?scope=other", `{"revision":3,"paused":true}`, nil, 404},
		{"POST", "/v1/triggers/" + trigger.ID + "/rotate-key", `{"revision":3}`, nil, 200},
		{"POST", "/v1/triggers/" + trigger.ID + "/rotate-key?scope=other", `{"revision":4}`, nil, 404},
		// Event deliveries, occurrences and a run's retry.
		{"POST", "/v1/triggers/" + deliveries.ID + "/events", string(encode(delivery("second"))), tokenOf, 202},
		{"POST", "/v1/triggers/" + deliveries.ID + "/events", string(encode(delivery("second"))), tokenOf, 200},
		{"POST", "/v1/triggers/" + deliveries.ID + "/events", string(encode(delivery("third"))), nil, 401},
		{"POST", "/v1/triggers/" + deliveries.ID + "/events", `{"id":`, tokenOf, 400},
		// A trigger another scope holds answers as an unknown one: the
		// credential is invalid.
		{"POST", "/v1/triggers/" + deliveries.ID + "/events?scope=other", string(encode(delivery("third"))), tokenOf, 401},
		{"GET", "/v1/triggers/" + deliveries.ID + "/occurrences/" + occurrence.ID, "", tokenOf, 200},
		{"GET", "/v1/triggers/" + deliveries.ID + "/occurrences/" + occurrence.ID + "?scope=other", "", tokenOf, 401},
		{"POST", "/v1/triggers/" + deliveries.ID + "/deliveries", string(encode(RecordDelivery{ReleaseID: w.release.ID, Event: delivery("record")})), nil, 202},
		{"POST", "/v1/triggers/" + deliveries.ID + "/deliveries", string(encode(RecordDelivery{Event: delivery("unbound")})), nil, 422},
		{"GET", "/v1/triggers/" + deliveries.ID + "/deliveries/record", "", nil, 200},
		{"GET", "/v1/triggers/" + deliveries.ID + "/deliveries/record?scope=other", "", nil, 404},
		{"GET", "/v1/jobs/" + w.job.ID + "/occurrences", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "/occurrences?preparations=1", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "/occurrences?scope=other", "", nil, 404},
		{"POST", "/v1/occurrences/" + occurrence.ID + "/cancel", "", nil, 409},
		{"POST", "/v1/occurrences/" + occurrence.ID + "/reconcile", "", nil, 409},
		{"POST", "/v1/occurrences/" + occurrence.ID + "/cancel?scope=other", "", nil, 404},
		{"POST", "/v1/runs/" + queued.ID + "/retry", `{"attempt":0}`, nil, 409},
		{"POST", "/v1/runs/" + queued.ID + "/retry", `{}`, nil, 400},
		{"POST", "/v1/runs/" + queued.ID + "/retry?scope=other", `{"attempt":0}`, nil, 404},
		// The installation's profiles, held to none.
		{"POST", "/v1/input-profiles", string(encode(ordinary)), nil, 201},
		{"POST", "/v1/input-profiles", string(encode(ordinary)), nil, 200},
		{"POST", "/v1/input-profiles?scope=other", string(encode(ordinary)), nil, 400},
		{"GET", "/v1/input-profiles", "", nil, 200},
		{"POST", "/v1/retained-column/1/selected-profiles", string(encode(selected)), conversion, 201},
		{"POST", "/v1/retained-column/1/selected-profiles", string(encode(selected)), conversion, 200},
		{"GET", "/v1/retained-column/1/selected-profiles", "", conversion, 200},
		// The journal and the chain.
		{"GET", "/v1/jobs/" + w.job.ID + "/events", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "/events?tail=true", "", nil, 200},
		{"GET", "/v1/jobs/" + w.job.ID + "/events?scope=other", "", nil, 404},
		{"GET", "/v1/events?job=" + w.job.ID, "", nil, 200},
		{"GET", "/v1/events?job=" + w.job.ID + "&scope=other", "", nil, 404},
		{"GET", "/v1/events?scope=other", "", nil, 200},
		{"GET", "/v1/events?scope=other&tail=true", "", nil, 200},
		{"GET", "/v1/run-chain", "", nil, 200},
		{"GET", "/v1/run-chain?scope=other", "", nil, 400},
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
	// store for every scope. The journal naming a job and no scope is that
	// job's route, held to none.
	for _, path := range []string{"/v1/jobs", "/v1/runs", "/v1/events"} {
		if _, every := throughHeld(t, w, "GET", path, "", nil); !every {
			t.Errorf("GET %s naming no scope was held to one", path)
		}
	}
	o.held = nil
	if _, every := throughHeld(t, w, "GET", "/v1/events?job="+w.job.ID, "", nil); every || len(o.held) != 1 || o.held[0] != scopeHeld("") {
		t.Error("the journal naming a job and no scope is not held to none:", every, o.held)
	}
	// Every other route is refused before it reads anything.
	for _, route := range []string{"GET /v1/runs/" + queued.ID + "/diagnostics", "GET /v1/runs/" + queued.ID + "/verification", "GET /v1/jobs/" + w.job.ID + "/briefs", "GET /v1/background-connections", "POST /v1/inputs/next"} {
		method, path, _ := strings.Cut(route, " ")
		got, every := throughHeld(t, w, method, path, "", nil)
		if every || got.code != http.StatusNotImplemented || !strings.Contains(got.body, "store_route_unsupported") {
			t.Errorf("%s: %v %+v, want 501 store_route_unsupported", route, every, got)
		}
	}
}
