package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No network account is needed: these are the real Gateway and MCP adapter
// executables, with a controllable local MCP service standing in for a provider.
func TestDurablePreparationRealGatewayMCPAndRunnerRestart(t *testing.T) {
	gateway, adapter := os.Getenv("JPACK_GATEWAY_TEST_BIN"), os.Getenv("JPACK_MCP_ADAPTER_TEST_BIN")
	if gateway == "" || adapter == "" {
		t.Skip("set JPACK_GATEWAY_TEST_BIN and JPACK_MCP_ADAPTER_TEST_BIN for cross-repository integration")
	}
	cfg := testConfig(t)
	cfg.disableAutomation = true
	dir := sourceWorkerTempDir(t)
	source := filepath.Join(dir, "mcp-source")
	build := exec.Command("go", "build", "-o", source, "./testdata/async-mcp")
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	seed := bytes.Repeat([]byte{7}, 32)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	seedfile := filepath.Join(dir, "seed")
	os.WriteFile(seedfile, []byte(hex.EncodeToString(seed)), 0600)
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := strings.Split(listener.Addr().String(), ":")[1]
	listener.Close()
	endpoint := "fixture.local"
	origin := "http://127.0.0.1:" + port
	command := exec.Command(gateway, "serve", filepath.Join(dir, "gateway"), seedfile, "gateway:durable-test", filepath.Join(dir, "registry.jsonl"), "--source", "live="+adapter+" --timeout 2m --endpoint "+endpoint+" --tools lookup -- "+source+" "+dir, "--source-shape", "live=mcp", "--source-timeout", "live=180", "--port", port)
	var logs bytes.Buffer
	command.Stdout = &logs
	command.Stderr = &logs
	if e = command.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { command.Process.Signal(os.Interrupt); command.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	up := false
	client := http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		res, err := client.Get(origin + "/publickey")
		if err == nil {
			res.Body.Close()
			up = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		t.Fatal("Gateway did not start")
	}
	image, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	profile := InputProfile{ID: "live", PublicKey: hex.EncodeToString(pub), Class: "record", Source: "live", Authority: "gateway:durable-test", Shape: "mcp", Adapter: AdapterPin{Name: source, Version: "0.1", Digest: digest(image)}, Endpoint: &endpoint, Tools: []string{"lookup"}}
	mapping := InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Sources: []MappingSource{{Name: "input", Kind: "operation", Profile: profile.ID, ProfileDigest: profileHash(profile), MaxAge: 300, Arguments: []byte(`{"tool":"lookup","arguments":{}}`), Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{"/request", "/structuredContent/facts/request"}}, Evidence: []EvidenceMapping{{"intake-form", "/structuredContent/evidence/intake-form"}, {"sponsor-endorsement", "/structuredContent/evidence/sponsor-endorsement"}}}}}}}
	raw, e := automaticCall(context.Background(), origin+"/acquire", encode(map[string]any{"session": "release-preview", "source": "live", "arguments": json.RawMessage(`{"tool":"lookup","arguments":{}}`)}), MaxBody)
	if e != nil {
		t.Fatal(e)
	}
	cfg.InputProfiles = []InputProfile{profile}
	worker, e := OpenSourceWorker(SourceWorkerConfig{Dir: filepath.Join(dir, "caller-state"), Gateway: origin})
	if e != nil {
		t.Fatal(e)
	}
	defer worker.Close()
	server := httptest.NewServer(worker.Handler())
	defer server.Close()
	cfg.GatewayConnections = []GatewayConnection{{Profile: profile.ID, URL: origin, Durable: true, OperationsURL: server.URL, OperationsTokenFile: worker.TokenFile()}}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	pack, _ := os.ReadFile("testdata/triage.pack.json")
	release, e := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: Input{Source: &SourceInput{Mapping: mapping, Sources: map[string]SourceValue{"input": {Response: raw}}}}})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Durable real MCP", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	config := constantTrigger(time.Now())
	config.Input = &AutomaticInput{Kind: "mapped-sources"}
	config.PreparationSeconds = 300
	trigger, e := s.configureTrigger(job.ID, "", 0, config)
	if e != nil {
		t.Fatal(e)
	}
	o := newOccurrence(trigger, job, time.Now())
	o.PendingInput = config.Input
	tx, _ := s.db.Begin()
	if e = s.admitOccurrence(tx, trigger, &o, "real-mcp", ""); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(dir, "hold"), []byte("hold"), 0600)
	o = stepPreparation(t, s, o.ID)
	operation := o.Preparation.Tasks[0].ID
	deadline = time.Now().Add(10 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if _, e = os.Stat(filepath.Join(dir, "started")); e == nil {
			started = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !started {
		t.Fatal("MCP source never entered wait")
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	o = stepPreparation(t, s, o.ID)
	if o.State != "waiting" || o.Preparation.Tasks[0].ID != operation {
		t.Fatal("lost waiting operation", o)
	}
	os.WriteFile(filepath.Join(dir, "release"), []byte("finish"), 0600)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		o = stepPreparation(t, s, o.ID)
		if o.State != "waiting" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o.State != "accepted" {
		t.Fatal("real proof was not admitted", string(encode(publicOccurrence(o))))
	}
	if e = s.dispatchOccurrence(o, time.Now()); e != nil {
		t.Fatal(e)
	}
	o, _ = s.occurrence(o.ID)
	run := waitRun(t, s, o.RunID)
	if run.State != "completed" {
		t.Fatal(run.State, run.Problem)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "call\ncall\n" {
		t.Fatal("expected one preview and one operational acquisition", string(calls))
	}
	// The caller may retain salts; the signer store must not retain any.
	response := run.Input.Source.Sources["input"].Response
	var proof struct {
		Salts map[string]string `json:"salts"`
	}
	json.Unmarshal(response, &proof)
	if len(proof.Salts) == 0 {
		t.Fatal("caller did not retain proof secrets")
	}
	if err := filepath.WalkDir(filepath.Join(dir, "gateway"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		for _, salt := range proof.Salts {
			if bytes.Contains(data, []byte(salt)) {
				t.Errorf("signer retained caller salt in %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if run.Input.Preparation == nil || len(run.Input.Preparation.Cites) != 1 {
		t.Fatal("verified lineage was lost")
	}
}
