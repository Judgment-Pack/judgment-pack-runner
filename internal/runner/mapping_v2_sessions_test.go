package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// signResponseIn is signResponse, in a session of the caller's naming.
func signResponseIn(t *testing.T, p InputProfile, key ed25519.PrivateKey, args, result []byte, at time.Time, session string, index int) json.RawMessage {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal(signResponse(t, p, key, args, result, at, index), &v); e != nil {
		t.Fatal(e)
	}
	receipt := v["receipt"].(map[string]any)
	receipt["sessionId"] = session
	signReceipt(t, receipt, key)
	return encode(v)
}

// threeSources is the fixture's mapping with two operation sources after
// its own, neither depending on another, and the arguments each is asked
// with.
func threeSources(t *testing.T) (Input, InputProfile, ed25519.PrivateKey, time.Time, map[string][]byte) {
	t.Helper()
	input, p, key, at := v2Fixture(t)
	for _, name := range []string{"detail", "standing"} {
		input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: name, Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"of":"` + name + `"}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/" + name, "/" + name}}, Evidence: []EvidenceMapping{}}}})
	}
	return input, p, key, at, map[string][]byte{
		"vendor":   []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`),
		"detail":   []byte(`{"tool":"lookup","arguments":{"of":"detail"}}`),
		"standing": []byte(`{"tool":"lookup","arguments":{"of":"standing"}}`),
	}
}

var sourceResults = map[string][]byte{
	"vendor":   []byte(`{"id":7,"description":"a verified vendor"}`),
	"detail":   []byte(`{"detail":"ready"}`),
	"standing": []byte(`{"standing":"good"}`),
}

// The acquisitions of an input share a session, or each is the first call
// of a session of its own. An input in neither form is refused, at the
// first source that leaves both.
func TestV2AcquisitionsShareASessionOrEachHasItsOwn(t *testing.T) {
	type where struct {
		session string
		index   int
	}
	for name, test := range map[string]struct {
		at      map[string]where
		refused string
	}{
		"one session, in order":                        {map[string]where{"vendor": {"one", 0}, "detail": {"one", 1}, "standing": {"one", 2}}, ""},
		"one session, not from its first call":         {map[string]where{"vendor": {"one", 4}, "detail": {"one", 9}, "standing": {"one", 5}}, ""},
		"each the first call of its own":               {map[string]where{"vendor": {"job.a", 0}, "detail": {"job.b", 0}, "standing": {"job.c", 0}}, ""},
		"its own, and the second not a first call":     {map[string]where{"vendor": {"job.a", 0}, "detail": {"job.b", 1}, "standing": {"job.c", 0}}, "detail"},
		"its own, and the first not a first call":      {map[string]where{"vendor": {"job.a", 1}, "detail": {"job.b", 0}, "standing": {"job.c", 0}}, "detail"},
		"its own, and the last not a first call":       {map[string]where{"vendor": {"job.a", 0}, "detail": {"job.b", 0}, "standing": {"job.c", 2}}, "standing"},
		"two that share, and a third apart":            {map[string]where{"vendor": {"one", 0}, "detail": {"one", 1}, "standing": {"job.c", 0}}, "standing"},
		"two apart, and a third that shares":           {map[string]where{"vendor": {"job.a", 0}, "detail": {"job.b", 0}, "standing": {"job.a", 1}}, "standing"},
		"two apart, and a third in the second's place": {map[string]where{"vendor": {"job.a", 0}, "detail": {"job.b", 0}, "standing": {"job.b", 0}}, "standing"},
		"one receipt's place for two sources":          {map[string]where{"vendor": {"one", 0}, "detail": {"one", 0}, "standing": {"one", 1}}, "detail"},
	} {
		input, p, key, at, args := threeSources(t)
		for source, w := range test.at {
			input.Source.Sources[source] = SourceValue{Response: signResponseIn(t, p, key, args[source], sourceResults[source], at, w.session, w.index)}
		}
		got, e := normalizeV2(input, []InputProfile{p}, at)
		if test.refused != "" {
			if e == nil || !strings.Contains(e.Error(), test.refused) {
				t.Errorf("%s: %v", name, e)
			}
			continue
		}
		if e != nil || string(got.Facts) != `{"detail":"ready","standing":"good","vendor":{"description":"a verified vendor"}}` || len(got.Preparation.Cites) != 3 {
			t.Errorf("%s: %v, %s", name, e, got.Facts)
			continue
		}
		// What is verified twice is the same twice, its citations in the
		// same order: a record made of it is compared with one made later.
		again, e := normalizeV2(got, []InputProfile{p}, at)
		if e != nil || !sameJSON(encode(again), encode(got)) {
			t.Errorf("%s: not repeatable: %v", name, e)
		}
	}
}

// Where each acquisition is the first call of its own session, the
// citations are in the mapping's order of sources; where they share one,
// in the order of their calls.
func TestV2CitationsHaveOneOrder(t *testing.T) {
	input, p, key, at, args := threeSources(t)
	for source, session := range map[string]string{"vendor": "job.c", "detail": "job.a", "standing": "job.b"} {
		input.Source.Sources[source] = SourceValue{Response: signResponseIn(t, p, key, args[source], sourceResults[source], at, session, 0)}
	}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil {
		t.Fatal(e)
	}
	order := []string{}
	for _, c := range got.Preparation.Cites {
		order = append(order, c.SessionID)
	}
	if strings.Join(order, " ") != "job.c job.a job.b" {
		t.Fatal(order)
	}
	for source, index := range map[string]int{"vendor": 2, "detail": 0, "standing": 1} {
		input.Source.Sources[source] = SourceValue{Response: signResponseIn(t, p, key, args[source], sourceResults[source], at, "one", index)}
	}
	if got, e = normalizeV2(input, []InputProfile{p}, at); e != nil {
		t.Fatal(e)
	}
	if c := got.Preparation.Cites; c[0].CallIndex != 0 || c[1].CallIndex != 1 || c[2].CallIndex != 2 {
		t.Fatal(c)
	}
}

// As many sources as a mapping may have, each acquired in a session of its
// own: the citations are in the mapping's order, however many there are.
func TestV2CitationsOfSixteenSourcesKeepTheMappingsOrder(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	names := []string{"vendor"}
	for _, letter := range "abcdefghijklmno" {
		name := "source-" + string(letter)
		names = append(names, name)
		input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: name, Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"of":"` + name + `"}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}}})
		input.Source.Sources[name] = SourceValue{Response: signResponseIn(t, p, key, []byte(`{"tool":"lookup","arguments":{"of":"`+name+`"}}`), []byte(`{}`), at, "job."+name, 0)}
	}
	input.Source.Sources["vendor"] = SourceValue{Response: signResponseIn(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), sourceResults["vendor"], at, "job.vendor", 0)}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil || len(got.Preparation.Cites) != 16 {
		t.Fatal(e)
	}
	for i, c := range got.Preparation.Cites {
		if c.SessionID != "job."+names[i] {
			t.Fatalf("the citation in place %d is of %s, and the source there is %s", i, c.SessionID, names[i])
		}
	}
}

// On the legacy path, three operation sources are acquired and what was
// acquired is verified: Runner asks the gateway for each in a session of
// its own, and takes what it acquired. No run is made of it here; the
// durable path's test below goes on to the evaluation. (Runner issue 5: it
// refused its own acquisitions.)
func TestAutomaticGatewayAcquiresEverySourceOfAMapping(t *testing.T) {
	input, p, key, _, _ := threeSources(t)
	input.Source.Sources = map[string]SourceValue{}
	var calls, seals atomic.Int32
	sessions := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Session   string          `json:"session"`
			Source    string          `json:"source"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path == "/seal" {
			seals.Add(1)
			w.Write([]byte(`{}`))
			return
		}
		calls.Add(1)
		var asked struct {
			Tool      string `json:"tool"`
			Arguments struct {
				Of string `json:"of"`
			} `json:"arguments"`
		}
		json.Unmarshal(req.Arguments, &asked)
		source := asked.Arguments.Of
		if asked.Tool == "execute_sql" {
			source = "vendor"
		}
		if _, again := sessions[req.Session]; again {
			t.Errorf("the session %s was used for two acquisitions", req.Session)
		}
		sessions[req.Session] = source
		w.Write(signResponseIn(t, p, key, req.Arguments, sourceResults[source], time.Now(), req.Session, 0))
	}))
	defer srv.Close()
	s := &Service{cfg: Config{InputProfiles: []InputProfile{p}, GatewayConnections: []GatewayConnection{{Profile: p.ID, URL: srv.URL}}}}
	got, e := s.acquireAutomatic(context.Background(), cloneInput(input))
	if e != nil {
		t.Fatal(e)
	}
	normalized, e := normalizeV2(got, s.cfg.InputProfiles, time.Now())
	if e != nil || string(normalized.Facts) != `{"detail":"ready","standing":"good","vendor":{"description":"a verified vendor"}}` {
		t.Fatal(string(normalized.Facts), e)
	}
	if calls.Load() != 3 || seals.Load() != 3 || len(sessions) != 3 || len(normalized.Preparation.Cites) != 3 {
		t.Fatalf("%d calls, %d seals, %d sessions, %d citations", calls.Load(), seals.Load(), len(sessions), len(normalized.Preparation.Cites))
	}
	for _, c := range normalized.Preparation.Cites {
		if c.CallIndex != 0 || sessions[c.SessionID] == "" {
			t.Fatalf("a citation that is not of this run's asking: %+v", c)
		}
	}
}

// Runner takes an answer only as the first call of the session it asked
// in. The verifier would take either of these answers: one is a first call
// of a session apart, and the other is one acquisition alone. So nothing
// but Runner's own look at the answer refuses them.
func TestAutomaticGatewayTakesOnlyTheAnswerItAskedFor(t *testing.T) {
	for name, answer := range map[string]func(asked string) (string, int){
		"in another session, its first call":       func(string) (string, int) { return "job.elsewhere", 0 },
		"in the session asked, not its first call": func(asked string) (string, int) { return asked, 1 },
	} {
		input, p, key, _ := v2Fixture(t)
		input.Source.Sources = map[string]SourceValue{}
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Session   string          `json:"session"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if r.URL.Path == "/seal" {
				w.Write([]byte(`{}`))
				return
			}
			calls.Add(1)
			session, index := answer(req.Session)
			response := signResponseIn(t, p, key, req.Arguments, sourceResults["vendor"], time.Now(), session, index)
			// The answer is one the verifier takes, so that what refuses it
			// is Runner's look and nothing after.
			held := cloneInput(input)
			held.Source.Sources = map[string]SourceValue{"vendor": {Response: response}}
			if _, e := normalizeV2(held, []InputProfile{p}, time.Now()); e != nil {
				t.Errorf("%s: the verifier refuses the answer, so the case tests nothing: %v", name, e)
			}
			w.Write(response)
		}))
		s := &Service{cfg: Config{InputProfiles: []InputProfile{p}, GatewayConnections: []GatewayConnection{{Profile: p.ID, URL: srv.URL}}}}
		_, e := s.acquireAutomatic(context.Background(), cloneInput(input))
		srv.Close()
		if e == nil || !strings.Contains(e.Error(), "does not belong to this acquisition") {
			t.Fatalf("%s: %v", name, e)
		}
		if calls.Load() != 1 {
			t.Fatalf("%s: %d calls; a refused answer was asked for again", name, calls.Load())
		}
	}
}

// The same on the durable path: an answer is taken only as the first call
// of the operation's own session, and one that is not leaves the
// occurrence for its owner, asked for once.
func TestDurablePreparationTakesOnlyTheAnswerOfItsOperation(t *testing.T) {
	for name, set := range map[string]func(*asyncFixture){
		"in another session, its first call":         func(f *asyncFixture) { f.elsewhere = true },
		"in the operation's own, not its first call": func(f *asyncFixture) { f.later = true },
	} {
		s, _, o, f := preparationFixture(t)
		f.mu.Lock()
		f.ready = true
		set(f)
		f.mu.Unlock()
		for step := 0; step < 4 && o.State != "needs-attention" && o.Input == nil; step++ {
			o = stepPreparation(t, s, o.ID)
		}
		if o.State != "needs-attention" || o.Reason != "The source receipt does not belong to this operation." || o.Input != nil {
			t.Fatalf("%s: %s, %q", name, o.State, o.Reason)
		}
		if countRows(t, s, "runs") != 0 {
			t.Fatalf("%s: a run was made of an answer that was refused", name)
		}
		f.mu.Lock()
		asked := len(f.requests)
		f.mu.Unlock()
		if asked != 1 {
			t.Fatalf("%s: %d operations", name, asked)
		}
	}
}

// On the durable path too: a preparation that acquires two operation
// sources, each by an operation of its own and so in a session of its own,
// comes to be accepted, is evaluated once, and its record cites both.
func TestDurablePreparationOfTwoSourcesIsEvaluated(t *testing.T) {
	s, _, o, f := preparationFixture(t)
	r, _ := s.release(o.ReleaseID)
	r.InputMapping.Sources = append(r.InputMapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: f.p.ID, ProfileDigest: profileHash(f.p), MaxAge: 300, Arguments: []byte(`{"tool":"lookup","arguments":{}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}}})
	if _, e := s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(r)), r.ID); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
	// The operations are read while the preparation waits: once it is
	// accepted the occurrence may keep them no longer.
	operations := []string{}
	for step := 0; step < 8 && o.Input == nil; step++ {
		o = stepPreparation(t, s, o.ID)
		if o.State == "needs-attention" {
			t.Fatalf("after step %d: %s", step, o.Reason)
		}
		if o.Preparation != nil && len(o.Preparation.Tasks) > len(operations) {
			operations = operations[:0]
			for _, task := range o.Preparation.Tasks {
				operations = append(operations, task.ID)
			}
		}
	}
	if o.State != "accepted" || o.Input == nil || len(operations) != 2 {
		t.Fatalf("%s, %d operations: %s", o.State, len(operations), o.Reason)
	}
	// What is kept of a preparation is what was acquired; what it verifies
	// to is made of it again at admission, and here.
	verified, e := normalizeV2(*o.Input, s.cfg.InputProfiles, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	cites := verified.Preparation.Cites
	if len(cites) != 2 || cites[0].CallIndex != 0 || cites[1].CallIndex != 0 ||
		cites[0].SessionID != "async."+operations[0] || cites[1].SessionID != "async."+operations[1] {
		t.Fatalf("%+v of operations %v", cites, operations)
	}
	if e = s.dispatchOccurrence(o, time.Now()); e != nil {
		t.Fatal(e)
	}
	o, _ = s.occurrence(o.ID)
	run := waitRun(t, s, o.RunID)
	if run.State != "completed" || countRows(t, s, "runs") != 1 {
		t.Fatal(run)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 {
		t.Fatalf("%d operations for two sources", len(f.requests))
	}
}

// One acquisition alone is taken at any call index, as it was before the
// rule had two forms: it shares a session with itself.
func TestV2OneAcquisitionAloneIsTakenAtAnyIndex(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	for _, index := range []int{0, 1, 9} {
		input.Source.Sources["vendor"] = SourceValue{Response: signResponseIn(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), sourceResults["vendor"], at, "any.session", index)}
		got, e := normalizeV2(input, []InputProfile{p}, at)
		if e != nil || len(got.Preparation.Cites) != 1 || got.Preparation.Cites[0].CallIndex != index {
			t.Fatalf("at index %d: %v", index, e)
		}
	}
}

// A mapping that names two operations and acquires one, the second being
// skipped for what it depends on, was taken before and is taken the same.
func TestV2ASkippedOperationLeavesOneAcquisition(t *testing.T) {
	input, p, key, at := v2Fixture(t)
	input.Source.Mapping.Sources = append(input.Source.Mapping.Sources, MappingSource{Name: "detail", Kind: "operation", Profile: p.ID, ProfileDigest: profileHash(p), MaxAge: 300, Parameters: map[string]Parameter{"description": {From: "vendor", Pointer: "/vendor/description", Type: "string"}}, Arguments: json.RawMessage(`{"tool":"lookup","arguments":{"description":{"$param":"description"}}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/detail", "/detail"}}, Evidence: []EvidenceMapping{}}}})
	input.Source.Sources["vendor"] = SourceValue{Response: signResponseIn(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":8,"description":"of another vendor"}`), at, "job.a", 3)}
	got, e := normalizeV2(input, []InputProfile{p}, at)
	if e != nil || len(got.Preparation.Cites) != 1 || got.Preparation.Outcomes[2].Reason != "dependency-unavailable" {
		t.Fatal(e, string(encode(got.Preparation)))
	}
}
