package runner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// researchModel is the model the stand-in's AI profiles declare and every
// research source in these tests asks: a name, not a model.
const researchModel = "synthetic-research-model-1"

// researchSession is the session every stand-in receipt is in, one gateway's
// calls at call indexes that differ (spec/dialect.md §5.2).
const researchSession = "research-session"

// researchStandIn stands in, in the test's process, for a gateway that
// serves a research run's three channels, each through a source its
// operator declared of shape http (gateway SPEC.md §1.2a and §6): an AI
// connection's answer, a search connection's links and a public page. An
// endpoint the test serves over TLS answers each; the stand-in reads the
// answer as adapter-http reads an endpoint, into the result and the
// acquisition its envelope carries, and mints the receipt a gateway mints
// for it, at the next call index of one session, under its key. No model is
// asked and no page outside the test's process is read: the answers are
// the endpoint's, synthetic, and the stand-in is not the adapters, as each
// profile's adapter version says.
type researchStandIn struct {
	server *httptest.Server
	public string
	key    ed25519.PrivateKey
	// session is the session the stand-in's receipts are in; next is the
	// call index of the next receipt, and previous the last receipt's
	// signature, which the next one chains to.
	session  string
	next     int
	previous any
	// answers is the endpoint's answer to each question asked.
	answers map[string]any
	// redirects is where a page of a host of example.com, named by its host
	// and path, redirects to.
	redirects map[string]string
}

func newResearchStandIn(t *testing.T) *researchStandIn {
	t.Helper()
	h := &researchStandIn{answers: map[string]any{}, redirects: map[string]string{}, session: researchSession}
	h.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && (r.URL.Path == "/answers" || r.URL.Path == "/answers-2"):
			var asked struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&asked); err != nil || h.answers[asked.Query] == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(encode(h.answers[asked.Query]))
		case r.Method == "GET" && (r.URL.Path == "/search" || r.URL.Path == "/answers" || r.URL.Path == "/answers-2"):
			// A search connection's links; an AI connection that searches
			// answers a search at its own endpoint.
			q := r.URL.Query().Get("q")
			w.Header().Set("Content-Type", "application/json")
			w.Write(encode(map[string]any{"query": q, "hits": []map[string]string{{"url": "https://" + r.Host + "/page?item=search-result-1", "title": "A synthetic page"}}}))
		case r.Method == "GET" && h.redirects[r.Host+r.URL.Path] != "":
			// A page that moved: the redirect keeps the request's query.
			http.Redirect(w, r, h.redirects[r.Host+r.URL.Path]+"?"+r.URL.RawQuery, http.StatusFound)
		case r.Method == "GET" && (r.URL.Path == "/page" || r.URL.Path == "/reader" || strings.HasSuffix(r.Host, ".example.com")):
			// A public page: at the stand-in's own address, or at a host of
			// example.com, which the test's client reaches at the stand-in.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("ETag", `"`+r.URL.Query().Get("item")+`-1"`)
			w.Write([]byte("A synthetic page about " + r.URL.Query().Get("item") + ". Every figure on it is invented for a test."))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.server.Close)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.public, h.key = hex.EncodeToString(public), key
	return h
}

// The stand-in's profiles: an AI connection's, which answers and searches
// and names its model; a search connection's; and a public page source's.
// Each is of class generated, as a profile bound to a research source's
// capability is (spec/dialect.md §5.1), and each names its endpoint path.
func (h *researchStandIn) aiProfile(id, path string) InputProfile {
	model := researchModel
	return h.profile(id, "web-search", "adapter-sources", path, &ResearchDeclaration{Capabilities: []string{"answer", "search"}, Model: &model})
}
func (h *researchStandIn) searchProfile(id string) InputProfile {
	return h.profile(id, "web-search", "adapter-sources", "/search", &ResearchDeclaration{Capabilities: []string{"search"}})
}
func (h *researchStandIn) pageProfile(id, path string) InputProfile {
	return h.profile(id, "web", "adapter-web", path, &ResearchDeclaration{Capabilities: []string{"page"}})
}
func (h *researchStandIn) profile(id, source, adapter, path string, research *ResearchDeclaration) InputProfile {
	endpoint := h.server.URL + path
	return InputProfile{ID: id, PublicKey: h.public, Class: "generated", Source: source, Authority: "stand-in", Shape: "http",
		Adapter: AdapterPin{Name: adapter, Version: "stand-in", Digest: digest([]byte(adapter + " stand-in"))}, Endpoint: &endpoint, Research: research}
}

// researchRevision is the revision of the stand-in's connections, as a
// gateway's web-search request names one.
var researchRevision = strings.Repeat("5e", 32)

// retain asks the channel p stands for, for capability, as its adapter would
// ask the endpoint p pins, and returns the retained response the research
// record holds: the profile it came through, the request the gateway
// commits to, the result and the receipt the gateway signed, and the salts.
// An answer and a search are asked with the question as the query; a page
// by its URL: under a profile of one endpoint, that endpoint with item
// naming the page, and under a profile of an endpoint scope, which pins no
// one endpoint, item itself, the URL as requested. The test's client reaches
// a host of example.com at the stand-in, and follows a redirect, as the
// gateway's web adapter does: the receipt's endpoint is the final URL, its
// query removed (spec/dialect.md §5.2, "Which URL is held").
func (h *researchStandIn) retain(t *testing.T, p InputProfile, capability, question, item string, at time.Time) map[string]any {
	t.Helper()
	var request map[string]any
	var sent *http.Request
	var err error
	switch capability {
	case "answer":
		request = map[string]any{"connection": "answers", "revision": researchRevision, "query": question, "maxResults": 5}
		sent, err = http.NewRequest("POST", *p.Endpoint, bytes.NewReader(encode(map[string]string{"query": question})))
	case "search":
		request = map[string]any{"connection": "search", "revision": researchRevision, "query": question, "maxResults": 5}
		sent, err = http.NewRequest("GET", *p.Endpoint+"?q="+url.QueryEscape(question), nil)
	case "page":
		page := item
		if p.Endpoint != nil {
			page = *p.Endpoint + "?item=" + url.QueryEscape(item)
		}
		request = map[string]any{"url": page}
		sent, err = http.NewRequest("GET", page, nil)
	default:
		t.Fatal("no channel serves", capability)
	}
	if err != nil {
		t.Fatal(err)
	}
	answer, err := h.server.Client().Do(sent)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil || answer.StatusCode != 200 || answer.TLS == nil || len(answer.TLS.PeerCertificates) == 0 {
		t.Fatalf("the endpoint did not answer %s: %d %v", capability, answer.StatusCode, err)
	}
	// The result: the status, the headers adapter-http carries, by their
	// lowercase names, and the body, as a JSON value or as text.
	headers := map[string]string{}
	for _, name := range []string{"content-type", "etag"} {
		if values := answer.Header.Values(name); len(values) > 0 {
			headers[name] = strings.Join(values, ", ")
		}
	}
	result := map[string]any{"status": answer.StatusCode, "headers": headers, "bodyEncoding": "text", "body": string(body)}
	if strings.HasPrefix(headers["content-type"], "application/json") {
		result["bodyEncoding"], result["body"] = "json", json.RawMessage(canonTest(t, body))
	}
	endpoint := *answer.Request.URL
	endpoint.RawQuery = ""
	statement := map[string]any{"method": sent.Method, "endpoint": endpoint.String(), "request": request}
	snapshot := any(nil)
	if tag := answer.Header.Get("ETag"); tag != "" {
		snapshot = tag
	}
	peer := sha256.Sum256(answer.TLS.PeerCertificates[0].Raw)
	receipt, salts := h.mint(t, p, encode(request), encode(result), endpoint.String(), statement, snapshot, "tls:sha256:"+hex.EncodeToString(peer[:]), at)
	return map[string]any{"profile": map[string]string{"id": p.ID, "digest": profileHash(p)}, "request": request, "result": json.RawMessage(canonTest(t, encode(result))), "receipt": receipt, "salts": salts}
}

// own mints the receipt of a research record that came back through the
// AI connection itself (spec/dialect.md §5.2): over the source's request,
// its recipe, and the whole record, under the AI profile.
func (h *researchStandIn) own(t *testing.T, p InputProfile, request, record []byte, at time.Time) (map[string]any, map[string]string) {
	t.Helper()
	return h.mint(t, p, request, record, *p.Endpoint, map[string]any{"method": "POST", "endpoint": *p.Endpoint, "request": json.RawMessage(request)}, nil, nil, at)
}

// mint signs the receipt a gateway signs for one acquisition through p, at
// the session's next call index, chained to the receipt before it.
func (h *researchStandIn) mint(t *testing.T, p InputProfile, request, result []byte, endpoint string, statement map[string]any, snapshot, peer any, at time.Time) (map[string]any, map[string]string) {
	t.Helper()
	argsSalt, statementSalt := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(argsSalt); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(statementSalt); err != nil {
		t.Fatal(err)
	}
	commit := func(salt []byte, label string, value []byte) string {
		return digest(append(append(append([]byte{}, salt...), label...), value...))
	}
	receipt := map[string]any{"receiptVersion": "3", "kind": "acquisition", "sessionId": h.session, "callIndex": h.next, "prevSignature": h.previous, "source": p.Source, "resultDigest": digest(canonTest(t, result)), "servedAt": at.Format(time.RFC3339), "authority": p.Authority, "keyId": keyID(p.PublicKey), "argumentsCommitment": commit(argsSalt, "args:", canonTest(t, request)), "caller": nil,
		"acquisition": map[string]any{"shape": p.Shape, "adapter": p.Adapter, "endpoint": endpoint, "statement": commit(statementSalt, "statement:", canonTest(t, encode(statement))), "snapshot": snapshot, "peerIdentity": peer, "schema": nil, "upstreamToken": nil, "observedAt": at.Format(time.RFC3339)}}
	signReceipt(t, receipt, h.key)
	h.next++
	h.previous = receipt["signature"]
	return receipt, map[string]string{"args": hex.EncodeToString(argsSalt), "statement": hex.EncodeToString(statementSalt)}
}
