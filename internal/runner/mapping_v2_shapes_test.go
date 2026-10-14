package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The preparer reads a retained record of any gateway adapter shape alike
// (runner #79): airbyte, mcp and http. These tests hold it to that, and the
// vector http-shaped-source (vectors/preparation) to the record a stand-in
// of a gateway http source recorded.

// shapedFixture is v2Fixture's input, with its one source's profile of the
// given shape and its record declaring that shape. Everything else is the
// same for every shape: the key, the request, the result and the instant.
func shapedFixture(t *testing.T, shape string, public ed25519.PublicKey, key ed25519.PrivateKey, at time.Time) (Input, InputProfile) {
	t.Helper()
	input, p, _, _ := v2Fixture(t)
	p.PublicKey = hex.EncodeToString(public)
	p.Shape = shape
	if shape != "mcp" {
		p.Tools = nil
	}
	input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p)
	input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, p, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":7,"description":"a verified vendor"}`), at, 0)}
	return input, p
}

// A preparation's form does not depend on its source's shape. The same
// mapping, request and result, signed under the same key at the same
// instant, prepare to the same preparation from a record of each adapter
// shape, but for what names the profile and the receipt: the mapping's
// digest, which covers the profile's by its digest, and the receipt's
// signature, which covers the shape the record declares. Nothing in the
// preparation names the shape.
func TestAPreparationsFormDoesNotDependOnItsSourcesShape(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	var first []byte
	for _, shape := range []string{"mcp", "http", "airbyte"} {
		input, p := shapedFixture(t, shape, public, key, at)
		got, err := normalizeV2(input, []InputProfile{p}, at)
		if err != nil {
			t.Fatalf("%s: %v", shape, err)
		}
		if string(got.Facts) != `{"vendor":{"description":"a verified vendor"}}` || string(got.Evidence) != `{"registry":"present"}` {
			t.Fatalf("%s: %s %s", shape, got.Facts, got.Evidence)
		}
		prepared, err := canonical(encode(got.Preparation))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(prepared, []byte(shape)) || bytes.Contains(prepared, []byte(`"shape"`)) {
			t.Fatalf("%s: the preparation names the shape: %s", shape, prepared)
		}
		form := bytes.ReplaceAll(prepared, []byte(got.Preparation.MappingDigest), []byte("MAPPING"))
		form = bytes.ReplaceAll(form, []byte(got.Preparation.Cites[0].Signature), []byte("SIGNATURE"))
		if first == nil {
			first = form
			continue
		}
		if !bytes.Equal(form, first) {
			t.Fatalf("%s prepares to another form than mcp:\n%s\n%s", shape, form, first)
		}
		again, err := normalizeV2(got, []InputProfile{p}, at)
		if err != nil || !sameJSON(encode(again), encode(got)) {
			t.Fatalf("%s: not repeatable: %v", shape, err)
		}
	}
}

// A record is held to the shape its profile pins, read from the record's
// own acquisition.shape, as it was before the preparer read every adapter
// shape: a record of another shape than its profile's, of a shape the
// gateway does not define, or naming none, is refused for that, whatever
// the profile's shape. A profile of a shape the gateway does not define is
// refused as it was, and an operation from a bare command's profile is
// refused. A paged acquisition, which adapter-airbyte makes of every read,
// is refused whatever its shape, as it was (MAPPING-V2.md, "Deferred
// scope").
func TestARecordIsHeldToTheShapeItsProfilePins(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	refused := func(t *testing.T, input Input, p InputProfile, want string) {
		t.Helper()
		_, err := normalizeV2(input, []InputProfile{p}, at)
		var problem *apiError
		if err == nil || !errors.As(err, &problem) || problem.Message != want {
			t.Fatalf("not refused with %q: %v", want, err)
		}
	}
	differs := "Source vendor: acquisition shape differs from the trusted profile. No decision was requested."
	for _, pinned := range []string{"mcp", "http", "airbyte"} {
		for _, declared := range []string{"mcp", "http", "airbyte", "command", "ftp", "MCP", ""} {
			if declared == pinned {
				continue
			}
			t.Run(pinned+" pinned, "+declared+" declared", func(t *testing.T) {
				input, p := shapedFixture(t, pinned, public, key, at)
				other := p
				other.Shape = declared
				input.Source.Sources["vendor"] = SourceValue{Response: signResponse(t, other, key, []byte(`{"tool":"execute_sql","arguments":{"sql":"SELECT * FROM vendors WHERE id = 7"}}`), []byte(`{"id":7,"description":"a verified vendor"}`), at, 0)}
				refused(t, input, p, differs)
			})
		}
		t.Run(pinned+" pinned, none declared", func(t *testing.T) {
			input, p := shapedFixture(t, pinned, public, key, at)
			resign(t, &input, key, func(receipt map[string]any) { delete(receipt["acquisition"].(map[string]any), "shape") })
			refused(t, input, p, differs)
		})
		t.Run(pinned+" pinned, paged", func(t *testing.T) {
			input, p := shapedFixture(t, pinned, public, key, at)
			resign(t, &input, key, func(receipt map[string]any) {
				receipt["acquisition"].(map[string]any)["pageItems"] = []any{digest([]byte(`{"id":7,"description":"a verified vendor"}`))}
			})
			refused(t, input, p, "Source vendor: paged acquisitions are not supported. No decision was requested.")
		})
	}
	for _, shape := range []string{"ftp", "MCP", "", "airbyte "} {
		t.Run("a profile of shape "+shape, func(t *testing.T) {
			input, p := shapedFixture(t, "mcp", public, key, at)
			p.Shape = shape
			input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p)
			if err := validateProfiles([]InputProfile{p}); err == nil || err.Error() != "invalid installation input profile" {
				t.Fatalf("the profile was not refused as an invalid installation input profile: %v", err)
			}
			if _, err := normalizeV2(input, []InputProfile{p}, at); err == nil || err.Error() != "invalid installation input profile" {
				t.Fatalf("preparation under the profile was not refused for it: %v", err)
			}
		})
	}
	t.Run("an operation from a command's profile", func(t *testing.T) {
		input, p := shapedFixture(t, "command", public, key, at)
		if err := validateProfiles([]InputProfile{p}); err != nil {
			t.Fatal("a command's profile is no longer admitted:", err)
		}
		refused(t, input, p, "an operation is read from an adapter source: its profile's shape must be airbyte, mcp or http")
	})
}

// resign changes the receipt of the input's vendor record and signs it
// again under key.
func resign(t *testing.T, input *Input, key ed25519.PrivateKey, change func(receipt map[string]any)) {
	t.Helper()
	v, err := readInputJSON(input.Source.Sources["vendor"].Response)
	if err != nil {
		t.Fatal(err)
	}
	r := v.(map[string]any)
	receipt := r["receipt"].(map[string]any)
	change(receipt)
	signReceipt(t, receipt, key)
	input.Source.Sources["vendor"] = SourceValue{Response: encode(r)}
}

// An operation's request is a tool call only for a source of shape mcp. To
// a source of shape http or airbyte it is the request that source reads,
// formed by the mapping's author, held to the template rules every request
// is held to and committed to by the receipt, and to nothing that makes it
// a tool call: the planner hands it on as the mapping instantiates it, and
// the preparer derives from the record answering it. Under a profile of
// shape mcp the same request is refused as no tool call, and an MCP
// request naming a tool its profile does not allow, or execute_sql with
// SQL a parameter replaces, is refused as before.
func TestAnOperationsRequestIsAToolCallOnlyForAnMCPSource(t *testing.T) {
	stand := newHTTPStandIn(t)
	at := time.Now().UTC().Truncate(time.Second)
	input := stand.input(t, at)
	got, err := normalizeV2(input, []InputProfile{stand.profile}, at)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Facts) != `{"request":`+standInRequest+`}` || string(got.Evidence) != `{"intake-form":"present","sponsor-endorsement":"present"}` || got.Preparation.Outcomes[1].Reason != "matching-request" {
		t.Fatal(string(encode(got)))
	}
	if string(got.Preparation.Outcomes[1].Arguments) != standInArgs {
		t.Fatalf("the request is not the one the mapping instantiates: %s", got.Preparation.Outcomes[1].Arguments)
	}
	planned := cloneInput(input)
	planned.Source.Sources = map[string]SourceValue{}
	plan, err := nextInput(planned, []InputProfile{stand.profile}, at)
	if err != nil || plan.Next == nil || plan.Next.Name != "intake" || plan.Next.Source != "intake/api" || string(plan.Next.Arguments) != standInArgs {
		t.Fatalf("the planner does not hand on the request as the mapping instantiates it: %+v %v", plan.Next, err)
	}
	airbyte := stand.profile
	airbyte.Shape = "airbyte"
	asAirbyte := cloneInput(input)
	asAirbyte.Source.Mapping.Sources[0].ProfileDigest = profileHash(airbyte)
	asAirbyte.Source.Sources["intake"] = stand.acquireAs(t, airbyte, []byte(standInArgs), at, "mapping.session")
	if _, err := normalizeV2(asAirbyte, []InputProfile{airbyte}, at); err != nil {
		t.Fatal("an airbyte operation's request was held to a tool call:", err)
	}
	for name, change := range map[string]func(*Input){
		"an undeclared parameter": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"path":"/intake/requests","method":"GET","query":{"id":{"$param":"undeclared"}}}`)
		},
		"runAt in the request": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"path":"/intake/requests","method":"GET","query":{"at":{"$param":"runAt"}}}`)
		},
		"a string in text": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"path":"/intake/requests","method":"GET","query":{"id":{"$text":"{{requestId}}"}}}`)
		},
		"an unknown template form": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(`{"path":"/intake/requests","method":"GET","query":{"id":{"$value":"REQ-7"}}}`)
		},
		"a request that is not an object": func(i *Input) {
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(`["/intake/requests"]`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(input)
			change(&i)
			var problem *apiError
			if _, err := normalizeV2(i, []InputProfile{stand.profile}, at); err == nil || !errors.As(err, &problem) || !strings.HasPrefix(problem.Message, "Mapping v2: ") {
				t.Fatalf("the template rules do not hold an http request: %v", err)
			}
		})
	}

	mcp := stand.profile
	mcp.Shape = "mcp"
	mcp.Tools = []string{"lookup"}
	asMCP := cloneInput(input)
	asMCP.Source.Mapping.Sources[0].ProfileDigest = profileHash(mcp)
	if _, err := normalizeV2(asMCP, []InputProfile{mcp}, at); err == nil || err.Error() != "Mapping v2: MCP tool identity must be a fixed literal with an arguments object" {
		t.Fatalf("an MCP operation's request that is no tool call was not refused as one: %v", err)
	}
	vendor, p, _, _ := v2Fixture(t)
	for name, c := range map[string]struct{ arguments, want string }{
		"a tool the profile does not allow": {`{"tool":"delete_rows","arguments":{}}`, "source vendor: tool is not allowed by its trusted profile"},
		"a tool given by a parameter":       {`{"tool":{"$param":"vendorId"},"arguments":{}}`, "Mapping v2: MCP tool identity must be a fixed literal with an arguments object"},
		"SQL a parameter replaces":          {`{"tool":"execute_sql","arguments":{"sql":{"$param":"vendorId"}}}`, "Mapping v2: execute_sql requires fixed SQL text or integer-only text interpolation; use bind arguments for strings"},
		"SQL that is not text":              {`{"tool":"execute_sql","arguments":{"sql":7}}`, "Mapping v2: execute_sql requires fixed SQL text"},
	} {
		t.Run(name, func(t *testing.T) {
			i := cloneInput(vendor)
			i.Source.Mapping.Sources[0].Arguments = json.RawMessage(c.arguments)
			if _, err := normalizeV2(i, []InputProfile{p}, at); err == nil || err.Error() != c.want {
				t.Fatalf("not refused with %q: %v", c.want, err)
			}
		})
	}
}

// Runner's unattended acquisition hands a gateway an http source's request
// as the mapping instantiates it, and takes the record the gateway answers
// with, as it does an MCP source's.
func TestAutomaticGatewayAcquiresAnHTTPSource(t *testing.T) {
	stand := newHTTPStandIn(t)
	at := time.Now().UTC().Truncate(time.Second)
	input := stand.input(t, at)
	input.Source.Sources = map[string]SourceValue{}
	var asked atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Session   string          `json:"session"`
			Source    string          `json:"source"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/seal" {
			w.Write([]byte(`{}`))
			return
		}
		asked.Add(1)
		if req.Source != "intake/api" || string(req.Arguments) != standInArgs {
			t.Errorf("the gateway was asked for %s with %s", req.Source, req.Arguments)
		}
		w.Write(stand.acquireAs(t, stand.profile, req.Arguments, time.Now().UTC().Truncate(time.Second), req.Session).Response)
	}))
	defer gateway.Close()
	s := &Service{cfg: Config{InputProfiles: []InputProfile{stand.profile}, GatewayConnections: []GatewayConnection{{Profile: stand.profile.ID, URL: gateway.URL}}}}
	got, err := s.acquireAutomatic(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := normalizeV2(got, s.cfg.InputProfiles, time.Now().UTC().Truncate(time.Second))
	if err != nil || asked.Load() != 1 || string(prepared.Facts) != `{"request":`+standInRequest+`}` {
		t.Fatal(asked.Load(), string(prepared.Facts), err)
	}
}

// standInRequest is the request record the stand-in's endpoint answers
// with: the facts the test pack reads.
const standInRequest = `{"appropriateness":"pass","completeness":"complete","embargoedInformationToUnauthorizedRecipients":false,"type":"data-access"}`

// standInArgs is the request the mapping instantiates for the case, as the
// gateway's canonical arguments: the path, the method and the query
// adapter-http reads.
const standInArgs = `{"method":"GET","path":"/intake/requests","query":{"id":"REQ-7"}}`

// httpStandIn stands in, in the test's process, for a gateway source the
// operator declared of shape http (gateway SPEC.md §1.2a and §6, "Adapter
// sources"; adapters/README.md, "adapter-http"). An endpoint served by the
// test over TLS answers one request record; the stand-in reads it as
// adapter-http reads an endpoint, into the result and the acquisition its
// envelope carries, and mints the receipt a gateway mints for it, with the
// shape the operator declared, under the profile's key. No live service is
// asked, and the stand-in is not the adapter: its profile names it so.
type httpStandIn struct {
	server  *httptest.Server
	profile InputProfile
	key     ed25519.PrivateKey
}

func newHTTPStandIn(t *testing.T) *httpStandIn {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/intake/requests" || r.URL.RawQuery != "id=REQ-7" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"REQ-7-3"`)
		w.Write([]byte(`{"id":"REQ-7","request":` + standInRequest + `}`))
	}))
	t.Cleanup(server.Close)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := server.URL + "/intake/requests"
	profile := InputProfile{ID: "intake", PublicKey: hex.EncodeToString(public), Class: "record", Source: "intake/api", Authority: "test", Shape: "http",
		Adapter: AdapterPin{Name: "adapter-http", Version: "stand-in", Digest: digest([]byte("adapter-http stand-in"))}, Endpoint: &endpoint}
	return &httpStandIn{server, profile, key}
}

// input is a mapping of one http source, asked for the case's request by
// its id, whose rule binds the record to the case and reads the request
// from the answer's body, and the record the stand-in acquires for it.
func (h *httpStandIn) input(t *testing.T, at time.Time) Input {
	t.Helper()
	rule := `{"ruleVersion":"1","parameters":{"requestId":"string"},"clauses":[` +
		`{"when":{"op":"all","of":[{"op":"equals","field":"/status","to":200},{"op":"equalsParam","field":"/body/id","param":"requestId"},{"op":"exists","field":"/body/request"}]},` +
		`"claim":{"facts":[{"pointer":"/request","from":"/body/request"}],"evidence":{"intake-form":"present","sponsor-endorsement":"present"},"acquisitionStatus":"resolved"},"reason":"matching-request"},` +
		`{"when":{"op":"always"},"claim":{"facts":[],"evidence":{"intake-form":"unknown","sponsor-endorsement":"unknown"},"acquisitionStatus":"unknown"},"reason":"no-matching-request"}]}`
	m := InputMapping{Version: 2,
		Case: &CaseMapping{Parameters: map[string]Parameter{"requestId": {Pointer: "/id", Type: "string"}}, Facts: []FactMapping{}, Evidence: []EvidenceMapping{}},
		Sources: []MappingSource{{Name: "intake", Kind: "operation", Profile: h.profile.ID, ProfileDigest: profileHash(h.profile), MaxAge: 300,
			Arguments: json.RawMessage(`{"path":"/intake/requests","method":"GET","query":{"id":{"$param":"requestId"}}}`),
			Read:      SourceRead{Rule: json.RawMessage(rule)}}},
		UnmappedEvidence: []string{"sensitive-data-approvals"},
	}
	return Input{Source: &SourceInput{Mapping: m, Case: json.RawMessage(`{"id":"REQ-7"}`), Sources: map[string]SourceValue{"intake": h.acquireAs(t, h.profile, []byte(standInArgs), at, "mapping.session")}}}
}

// acquireAs asks the endpoint as adapter-http would for the request args
// (path, method, query), and returns the record a gateway retains for it,
// {result, receipt, salts}, as the first call of session, its receipt
// naming p's identity and shape and signed under the stand-in's key.
func (h *httpStandIn) acquireAs(t *testing.T, p InputProfile, args []byte, at time.Time, session string) SourceValue {
	t.Helper()
	var request struct {
		Path   string            `json:"path"`
		Method string            `json:"method"`
		Query  map[string]string `json:"query"`
	}
	if err := strictJSON(args, &request); err != nil || request.Method != "GET" {
		t.Fatalf("the stand-in reads a GET of path and query: %s %v", args, err)
	}
	query := url.Values{}
	for name, value := range request.Query {
		query.Set(name, value)
	}
	sent, err := http.NewRequest(request.Method, h.server.URL+request.Path+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sent.Header.Set("Accept", "application/json")
	answer, err := h.server.Client().Do(sent)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil || answer.StatusCode != 200 || answer.TLS == nil || len(answer.TLS.PeerCertificates) == 0 {
		t.Fatalf("the endpoint did not answer: %d %v", answer.StatusCode, err)
	}
	// The result: the status, the headers adapter-http carries, by their
	// lowercase names, and a JSON body carried as a value.
	headers := map[string]string{}
	for _, name := range []string{"cache-control", "content-length", "content-type", "date", "etag", "expires", "last-modified"} {
		if values := answer.Header.Values(name); len(values) > 0 {
			headers[name] = strings.Join(values, ", ")
		}
	}
	result := canonTest(t, encode(map[string]any{"status": answer.StatusCode, "headers": headers, "bodyEncoding": "json", "body": json.RawMessage(canonTest(t, body))}))
	// The acquisition: the stand-in as the adapter, the URL asked without
	// its query, the request as the statement the gateway commits to, the
	// answer's ETag, the TLS peer's certificate, and null for what an HTTP
	// answer does not carry.
	endpoint := *sent.URL
	endpoint.RawQuery = ""
	statement := string(canonTest(t, encode(map[string]any{"method": request.Method, "path": request.Path, "query": request.Query})))
	peer := sha256.Sum256(answer.TLS.PeerCertificates[0].Raw)
	argsSalt, statementSalt := make([]byte, 32), make([]byte, 32)
	if _, err = rand.Read(argsSalt); err != nil {
		t.Fatal(err)
	}
	if _, err = rand.Read(statementSalt); err != nil {
		t.Fatal(err)
	}
	commit := func(salt []byte, label string, value []byte) string {
		return digest(append(append(append([]byte{}, salt...), label...), value...))
	}
	receipt := map[string]any{"receiptVersion": "3", "kind": "acquisition", "sessionId": session, "callIndex": 0, "prevSignature": nil, "source": p.Source, "resultDigest": digest(result), "servedAt": at.Format(time.RFC3339), "authority": p.Authority, "keyId": keyID(p.PublicKey), "argumentsCommitment": commit(argsSalt, "args:", canonTest(t, args)), "caller": nil,
		"acquisition": map[string]any{"shape": p.Shape, "adapter": p.Adapter, "endpoint": endpoint.String(), "statement": commit(statementSalt, "statement:", canonTest(t, encode(statement))), "snapshot": answer.Header.Get("ETag"), "peerIdentity": "tls:sha256:" + hex.EncodeToString(peer[:]), "schema": nil, "upstreamToken": nil, "observedAt": at.Format(time.RFC3339)}}
	signReceipt(t, receipt, h.key)
	return SourceValue{Response: encode(map[string]any{"result": json.RawMessage(result), "receipt": receipt, "salts": map[string]string{"args": hex.EncodeToString(argsSalt), "statement": hex.EncodeToString(statementSalt)}})}
}

// A run whose one source is of shape http is evaluated, signed, witnessed
// and bound, and verified offline, as a run of an MCP source is. The export
// in testdata/mapping-v2-http-shaped was written by this test, under
// Runtime 0.31.0, with JPACK_V2_HTTP_EXPORT_DIR set, and the vector
// http-shaped-source was generated from it.
func TestAnHTTPShapedRunIsSignedWitnessedAndBound(t *testing.T) {
	cfg := testConfig(t)
	keyPath := signingKey(t)
	cfg.SigningKey = keyPath
	cfg.Runtime = signingRuntimeWithKey(t, cfg.Runtime, filepath.Join(t.TempDir(), "signing.log"), keyPath)
	stand := newHTTPStandIn(t)
	cfg.InputProfiles = []InputProfile{stand.profile}
	input := stand.input(t, time.Now().UTC().Truncate(time.Second))
	bundle := signedAndWitnessedExport(t, cfg, input, stand.profile, "An http source, signed and witnessed", "http-shaped")
	v, err := VerifyInputs(encode(bundle), cfg.InputProfiles, bundle.ReleaseDigest)
	if err != nil {
		t.Fatal(err)
	}
	if v.Classes.Record != 3 || v.Classes.Asserted != 0 || len(v.Unsigned) != 0 || v.Freshness == nil {
		t.Fatalf("the http source's targets are not counted as signed records: %+v %+v", v.Classes, v.Unsigned)
	}
	if runtimeTakesBinding(t, os.Getenv("JPACK_TEST_BIN")) && (v.Binding == nil || v.Binding.Status != BindingMatches) {
		t.Fatalf("the record does not bind the preparation: %+v", v.Binding)
	}
	writeBoundFixture(t, os.Getenv("JPACK_V2_HTTP_EXPORT_DIR"), bundle, stand.profile)
}
