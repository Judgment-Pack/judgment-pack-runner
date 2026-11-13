package runner

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The control API in service mode (ServiceHandler, docs/design/
// service-mode.md): every request holds an identity token when an identity
// is configured, the caller it names is recorded on what the request creates
// and in the journal, and GET /v1/health answers without a credential.

// serveAs serves one request on h, carrying token as its bearer when token is
// not "".
func serveAs(t *testing.T, h http.Handler, method, path, body, token string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A caller is the requester of every run it submits, and the actor of every
// entry its requests write: the journal names it by issuer and subject in an
// entry of version 2, the run records it as requestedBy, and its idempotency
// keys are its own. An event delivery's credential is still the delivery's
// actor. No record, entry or answer holds a token.
func TestACallerIsRecordedOnWhatItsRequestsCreate(t *testing.T) {
	s, _ := checkService(t)
	signer := edSigner(t, "k1")
	h := s.ServiceHandler("", testIdentity(t, signer))
	var issued []string
	var answers bytes.Buffer
	as := func(subject, method, path, body string, header map[string]string, want int) []byte {
		t.Helper()
		token := tokenOf(signer, claimsFor(subject, time.Now()))
		issued = append(issued, token)
		got := serveAs(t, h, method, path, body, token, header)
		answers.Write(got.Body.Bytes())
		if got.Code != want {
			t.Fatalf("%s %s as %s: %d %s", method, path, subject, got.Code, got.Body)
		}
		return got.Body.Bytes()
	}
	var release, next Release
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/previews", checkRequest(false), nil, 201), &release))
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/previews", checkRequest(false), nil, 201), &next))
	var job Job
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/jobs", string(encode(map[string]any{"name": "Caller job", "releaseId": release.ID, "reviewed": true, "trigger": eventTriggerConfig()})), nil, 201), &job))
	if job.Owner != "journal-owner" {
		t.Fatalf("the job's record changed: %+v", job)
	}
	// A refusal is the caller's, and coalesced with the same caller's alone.
	for _, subject := range []string{"ops-service", "build-bot", "ops-service"} {
		as(subject, "POST", "/v1/jobs", `{"name":"Again","releaseId":"`+release.ID+`","reviewed":true}`, nil, 409)
	}

	input := string(encode(journalInput()))
	keyed := map[string]string{"Idempotency-Key": "k1"}
	var first, replay, other Run
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/jobs/"+job.ID+"/runs", input, keyed, 202), &first))
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/jobs/"+job.ID+"/runs", input, keyed, 200), &replay))
	must(t, json.Unmarshal(as("build-bot", "POST", "/v1/jobs/"+job.ID+"/runs", input, keyed, 202), &other))
	if first.RequestedBy != "ops-service" || replay.ID != first.ID || other.ID == first.ID || other.RequestedBy != "build-bot" {
		t.Fatalf("first %s by %s, replay %s, other %s by %s", first.ID, first.RequestedBy, replay.ID, other.ID, other.RequestedBy)
	}
	for id, subject := range map[string]string{first.ID: "ops-service", other.ID: "build-bot"} {
		if held, e := s.run(id); e != nil || held.RequestedBy != subject {
			t.Fatalf("run %s is held as requested by %q: %v", id, held.RequestedBy, e)
		}
	}
	as("build-bot", "POST", "/v1/jobs/"+job.ID+"/runs", input, nil, 400)
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/jobs/"+job.ID+"/upgrade", string(encode(UpgradeJobRequest{ReleaseID: next.ID, BaseReleaseID: release.ID, BaseRevision: 1, Reviewed: true})), nil, 200), &job))
	if job.ReleaseID != next.ID {
		t.Fatalf("the job was not upgraded: %+v", job)
	}

	tr, _ := storedTrigger(t, s, job.InitialTriggerID)
	var changed struct {
		Trigger Trigger `json:"trigger"`
		Token   string  `json:"token"`
	}
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/triggers/"+tr.ID+"/state", string(encode(map[string]any{"revision": tr.Revision, "paused": false, "reviewed": true})), nil, 200), &changed))
	must(t, json.Unmarshal(as("build-bot", "POST", "/v1/triggers/"+tr.ID+"/rotate-key", string(encode(map[string]any{"revision": changed.Trigger.Revision})), nil, 200), &changed))
	if changed.Token == "" {
		t.Fatal("no trigger key was issued")
	}
	// The trigger key is checked as it always was, beside the caller's token.
	as("ops-service", "POST", "/v1/triggers/"+tr.ID+"/events", string(encode(delivery("by-credential"))), map[string]string{"X-Trigger-Token": changed.Token}, 202)
	as("ops-service", "POST", "/v1/triggers/"+tr.ID+"/events", string(encode(delivery("no-credential"))), nil, 401)
	as("build-bot", "POST", "/v1/triggers/"+tr.ID+"/deliveries", string(encode(RecordDelivery{ReleaseID: job.ReleaseID, Event: delivery("by-record")})), nil, 202)

	// An occurrence that needs attention is reconciled by one caller and
	// cancelled by another.
	tr, _ = storedTrigger(t, s, tr.ID)
	o := newOccurrence(tr, job, time.Now())
	o.PendingInput = &AutomaticInput{Kind: "constant", Value: string(encode(journalInput()))}
	admit(t, s, tr, &o, "attention")
	must(t, s.prepareLegacyOccurrence(t.Context(), o.ID))
	held := storedOccurrence(t, s, o.ID)
	held.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: held.Input}
	held.State = "waiting"
	must(t, s.saveOccurrence(held))
	must(t, s.stopPreparation(storedOccurrence(t, s, o.ID), "needs-attention", "A required source is unavailable."))
	as("ops-service", "POST", "/v1/occurrences/"+o.ID+"/reconcile", "", nil, 200)
	as("build-bot", "POST", "/v1/occurrences/"+o.ID+"/cancel", "", nil, 200)

	tr, _ = storedTrigger(t, s, tr.ID)
	must(t, json.Unmarshal(as("ops-service", "POST", "/v1/triggers/"+tr.ID+"/state", string(encode(map[string]any{"revision": tr.Revision, "paused": true})), nil, 200), &changed))
	as("build-bot", "POST", "/v1/jobs/"+job.ID+"/triggers", string(encode(map[string]any{"id": tr.ID, "revision": changed.Trigger.Revision, "config": eventTriggerConfig()})), nil, 200)

	want := []string{
		"admission.refused/build-bot", "admission.refused/build-bot", "admission.refused/ops-service", "admission.refused/ops-service",
		"job.created/ops-service", "job.upgraded/ops-service",
		"occurrence.cancelled/build-bot", "occurrence.received/build-bot", "occurrence.reconciled/ops-service",
		"release.previewed/ops-service", "release.previewed/ops-service",
		"run.queued/build-bot", "run.queued/ops-service",
		"trigger.configured/build-bot", "trigger.configured/ops-service", "trigger.configured/ops-service",
		"trigger.key-rotated/build-bot", "trigger.paused/ops-service", "trigger.resumed/ops-service",
	}
	doc := readOpenAPI(t)
	var got []string
	credential := 0
	entries := journalEntries(t, s.cfg)
	for _, e := range entries {
		if problems := doc.check(e.Kind, doc.schema("JournalEntry"), decodeNumbers(t, encode(e))); len(problems) > 0 {
			t.Errorf("a %s entry is not the documented entry: %v", e.Kind, problems)
		}
		switch e.By.Kind {
		case "caller":
			if e.EntryVersion != "2" || e.By.Issuer != testIssuer || e.By.Owner != "" || e.By.Trigger != "" {
				t.Errorf("a caller's entry: %s", encode(e))
			}
			got = append(got, e.Kind+"/"+e.By.Subject)
		case "installation":
			t.Errorf("an entry names the installation, though every request held a token: %s", encode(e))
		case "trigger-credential":
			credential++
			fallthrough
		default:
			if e.EntryVersion != "1" {
				t.Errorf("an entry with no caller: %s", encode(e))
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the callers' entries are\n%v\nwant\n%v", got, want)
	}
	// The delivery that carried the trigger's key, and the occurrence the
	// test admitted on the event trigger itself.
	if credential != 2 {
		t.Fatalf("%d entries name the trigger's credential", credential)
	}

	// The journal routes serve the entries as documented, to a caller.
	for after := int64(0); ; {
		page := as("ops-service", "GET", "/v1/events?after="+strconv.FormatInt(after, 10), "", nil, 200)
		if problems := doc.check("JournalPage", doc.schema("JournalPage"), decodeNumbers(t, page)); len(problems) > 0 {
			t.Fatalf("a page is not the documented page: %v", problems)
		}
		var p EventPage
		must(t, json.Unmarshal(page, &p))
		if !p.More {
			break
		}
		after = p.Next
	}

	stored := storeBytes(t, s.cfg.Dir)
	for _, token := range issued {
		parts := strings.Split(token, ".")
		for what, text := range map[string]string{"a token": token, "a token's signature": parts[2], "a token's signed parts": parts[0] + "." + parts[1]} {
			if bytes.Contains(stored, []byte(text)) {
				t.Fatalf("the store holds %s", what)
			}
			if bytes.Contains(answers.Bytes(), []byte(text)) {
				t.Fatalf("an answer holds %s", what)
			}
			if bytes.Contains(encode(entries), []byte(text)) {
				t.Fatalf("the journal holds %s", what)
			}
		}
	}
}

// storeBytes is every file directly in the state directory, as stored: the
// database, its write-ahead log and anything beside them.
func storeBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var all bytes.Buffer
	items, err := os.ReadDir(dir)
	must(t, err)
	for _, item := range items {
		if item.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, item.Name()))
			must(t, err)
			all.Write(b)
		}
	}
	return all.Bytes()
}

// With an identity, every route but GET /v1/health holds an identity token,
// and the owner bearer is not taken in its place. A refused request writes no
// entry and says why, without repeating the token; one from a browser is
// refused as on every route.
func TestWithAnIdentityEveryRequestHoldsAToken(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	signer := edSigner(t, "k1")
	h := s.ServiceHandler(journalBearer, testIdentity(t, signer))
	before := len(journalEntries(t, cfg))
	expired := tokenOf(signer, withClaims(claimsFor("ops-service", time.Now()), "exp", time.Now().Add(-time.Hour).Unix()))
	for _, c := range []struct {
		name, token, want string
	}{
		{"no token", "", "Runner authentication required: the request carries no bearer token."},
		{"the owner bearer", journalBearer, "Runner authentication required: the token is not three dot-separated parts."},
		{"an expired token", expired, "Runner authentication required: the token has expired."},
		{"a subject not listed", tokenOf(signer, claimsFor("someone-else", time.Now())), "Runner authentication required: the token's subject is not one the configuration lists."},
	} {
		for _, path := range []string{"/v1/status", "/v1/events", "/v1/jobs", "/v1/health/"} {
			got := serveAs(t, h, "GET", path, "", c.token, nil)
			var body map[string]string
			if got.Code != 401 || got.Header().Get("WWW-Authenticate") != "Bearer" || json.Unmarshal(got.Body.Bytes(), &body) != nil || body["message"] != c.want || len(body) != 1 {
				t.Fatalf("%s on %s: %d %v %s", c.name, path, got.Code, got.Header(), got.Body)
			}
		}
	}
	if got := serveAs(t, h, "POST", "/v1/jobs", `{"name":"x","releaseId":"release_x","reviewed":true}`, expired, nil); got.Code != 401 {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if after := len(journalEntries(t, cfg)); after != before {
		t.Fatalf("refused requests wrote %d entries", after-before)
	}
	valid := tokenOf(signer, claimsFor("ops-service", time.Now()))
	if got := serveAs(t, h, "GET", "/v1/status", "", valid, nil); got.Code != 200 || !strings.Contains(got.Body.String(), `"owner":"journal-owner"`) {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if got := serveAs(t, h, "GET", "/v1/status", "", valid, map[string]string{"Origin": "http://127.0.0.1:1"}); got.Code != 403 {
		t.Fatalf("a browser request: %d %s", got.Code, got.Body)
	}
	two := httptest.NewRequest("GET", "/v1/status", nil)
	two.Header.Add("Authorization", "Bearer "+valid)
	two.Header.Add("Authorization", "Bearer "+valid)
	answer := httptest.NewRecorder()
	h.ServeHTTP(answer, two)
	if answer.Code != 401 || !strings.Contains(answer.Body.String(), "more than one Authorization header") {
		t.Fatalf("two headers: %d %s", answer.Code, answer.Body)
	}
}

// Without an identity, service mode holds every request to the owner bearer,
// as a companion does, and refuses with a companion's words; a companion's
// control API is as it was, and has no health route.
func TestWithoutAnIdentityTheOwnerBearerIsHeldAsACompanionsIs(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	signer := edSigner(t, "k1")
	token := tokenOf(signer, claimsFor("ops-service", time.Now()))
	for name, h := range map[string]http.Handler{"service mode": s.ServiceHandler(journalBearer, nil), "a companion": s.Handler(journalBearer)} {
		for _, bearer := range []string{"", token, journalBearer + "x"} {
			got := serveAs(t, h, "GET", "/v1/status", "", bearer, nil)
			if got.Code != 401 || got.Body.String() != "{\"message\":\"Runner authentication required.\"}\n" || got.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("%s: %d %v %q", name, got.Code, got.Header(), got.Body)
			}
		}
		if got := serveAs(t, h, "GET", "/v1/status", "", journalBearer, nil); got.Code != 200 {
			t.Fatalf("%s: %d %s", name, got.Code, got.Body)
		}
	}
	companion := s.Handler(journalBearer)
	if got := serveAs(t, companion, "GET", "/v1/health", "", "", nil); got.Code != 401 {
		t.Fatalf("a companion's health without the bearer: %d %s", got.Code, got.Body)
	}
	if got := serveAs(t, companion, "GET", "/v1/health", "", journalBearer, nil); got.Code != 404 {
		t.Fatalf("a companion's health: %d %s", got.Code, got.Body)
	}
}

// GET /v1/health answers without a credential, in service mode alone, and
// says whether Runner is healthy and nothing else: not the store, its
// directory, the workspace, the owner or a digest.
func TestHealthAnswersWithoutACredentialAndSaysNothingElse(t *testing.T) {
	cfg := journalConfig(t)
	s := openJournal(t, cfg)
	defer s.Close()
	signer := edSigner(t, "k1")
	for name, h := range map[string]http.Handler{"with identity": s.ServiceHandler("", testIdentity(t, signer)), "with the owner bearer": s.ServiceHandler(journalBearer, nil)} {
		s.unhealthy.Store(false)
		for _, c := range []struct{ method, path string }{{"GET", "/v1/health"}, {"GET", "/v1/health?probe=1"}, {"HEAD", "/v1/health"}} {
			got := serveAs(t, h, c.method, c.path, "", "", nil)
			if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" || c.method == "GET" && got.Body.String() != "{\"healthy\":true}\n" {
				t.Fatalf("%s: %s %s: %d %v %q", name, c.method, c.path, got.Code, got.Header(), got.Body)
			}
		}
		if got := serveAs(t, h, "POST", "/v1/health", "", "", nil); got.Code != 401 {
			t.Fatalf("%s: POST: %d %s", name, got.Code, got.Body)
		}
		if got := serveAs(t, h, "GET", "/v1/health", "", "", map[string]string{"Origin": "http://127.0.0.1:1"}); got.Code != 403 {
			t.Fatalf("%s: a browser request: %d %s", name, got.Code, got.Body)
		}
		s.unhealthy.Store(true)
		got := serveAs(t, h, "GET", "/v1/health", "", "", nil)
		if got.Code != 503 || got.Body.String() != "{\"healthy\":false}\n" {
			t.Fatalf("%s: unhealthy: %d %q", name, got.Code, got.Body)
		}
		for _, held := range []string{cfg.Dir, cfg.Owner, cfg.Workspace, s.runtimeDigest} {
			if strings.Contains(got.Body.String(), held) {
				t.Fatalf("%s: health says %q", name, held)
			}
		}
	}
	s.unhealthy.Store(false)
}
