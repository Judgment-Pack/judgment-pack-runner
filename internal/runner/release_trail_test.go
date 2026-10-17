package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// releaseConfiguration is the configuration every release carries, as
// docs/MAPPING-V2.md states it. Runner writes it, at the release preview, the
// only request that creates a release; nothing a caller sends changes it. It
// declares the audit directory, and that is what keeps a Runtime's refusal of
// citations where no trail is kept (Runtime ADR-0051) unreachable through
// Runner, which passes --cites for every operational run whose preparation
// cites receipts (#68).
const releaseConfiguration = `{"configVersion":"3","packs":{"target":{"path":"pack.json"}},"audit":{"dir":"audit"}}`

// citesNoTrail is what a Runtime carrying ADR-0051 says when it refuses
// citations in a project that keeps no trail.
const citesNoTrail = "--cites is not taken here: the project keeps no trail, so citations would be accepted and not recorded."

// A release's configuration is releaseConfiguration, byte for byte, as the
// release is answered, as the store holds it and as its directory holds
// jpack.json; and it is the configuration the documentation states.
func TestEveryReleaseDeclaresTheAuditDirectory(t *testing.T) {
	cfg := testConfig(t)
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release := testRelease(t, s)
	stored, err := s.release(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(cfg.Dir, "releases", release.ID, "jpack.json"))
	if err != nil {
		t.Fatal(err)
	}
	for place, config := range map[string]string{"answered": release.Config, "stored": stored.Config, "written": string(written)} {
		if config != releaseConfiguration {
			t.Fatalf("the %s release's configuration is not the one documented: %s", place, config)
		}
		if !bytes.Contains([]byte(config), []byte(`"audit":{"dir":"audit"}`)) {
			t.Fatalf("the %s release's configuration declares no audit directory: %s", place, config)
		}
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "MAPPING-V2.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc, []byte("`"+releaseConfiguration+"`")) {
		t.Fatal("docs/MAPPING-V2.md does not state the release's configuration")
	}
}

// The Runtime under test takes a cited run of a release's configuration: a
// run whose preparation cites a receipt completes, its attempt given the
// citations and its record carrying them. Given the same run with the same
// configuration less its audit member, the Runtime refuses the citations
// (JPS-INVOCATION-CITES, exit 3) and writes nothing; with the configuration
// as the release holds it, it evaluates.
func TestTheRuntimeTakesACitedRunOfTheReleasesConfiguration(t *testing.T) {
	cfg := testConfig(t)
	input, profile := calculatedTriage(t)
	cfg.InputProfiles = []InputProfile{profile}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	release, run := boundRun(t, s, input)
	if run.State != "completed" {
		t.Fatalf("the cited run did not complete: %s %s", run.State, run.Problem)
	}
	if run.Input.Preparation == nil || len(run.Input.Preparation.Cites) == 0 {
		t.Fatal("the run's preparation cites no receipt")
	}
	if _, err = os.Lstat(filepath.Join(cfg.Dir, "attempts", run.ID, "cites.json")); err != nil {
		t.Fatal("the attempt was not given the citations:", err)
	}
	var record struct {
		Cites []Citation `json:"cites"`
	}
	if err = json.Unmarshal(trailLine(t, cfg.Dir, run.ID), &record); err != nil || len(record.Cites) != len(run.Input.Preparation.Cites) {
		t.Fatalf("the record does not carry the citations: %v %v", err, record.Cites)
	}

	var config map[string]json.RawMessage
	if err = json.Unmarshal([]byte(release.Config), &config); err != nil {
		t.Fatal(err)
	}
	if _, declared := config["audit"]; !declared {
		t.Fatal("the release's configuration declares no audit directory")
	}
	delete(config, "audit")
	keepsNoTrail := release
	keepsNoTrail.Config = string(encode(config))

	ctx := context.Background()
	refused := filepath.Join(t.TempDir(), "keeps-no-trail")
	_, _, _, _, err = evaluateWith(ctx, cfg.Runtime, keepsNoTrail, run.Input, refused, false, signingInputs{})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the Runtime did not refuse the citations with exit 3: %v", err)
	}
	stdout, err := os.ReadFile(filepath.Join(refused, "evaluation.stdout"))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Status      string `json:"status"`
		Diagnostics []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	if err = json.Unmarshal(stdout, &answer); err != nil || answer.Status != "error" || len(answer.Diagnostics) != 1 || answer.Diagnostics[0].Code != "JPS-INVOCATION-CITES" || answer.Diagnostics[0].Message != citesNoTrail {
		t.Fatalf("the Runtime did not refuse the citations as ADR-0051 says: %s", stdout)
	}
	if _, err = os.Lstat(filepath.Join(refused, "audit")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the refused run wrote a trail:", err)
	}

	taken := filepath.Join(t.TempDir(), "keeps-a-trail")
	if _, trail, _, _, err := evaluateWith(ctx, cfg.Runtime, release, run.Input, taken, false, signingInputs{}); err != nil || len(trail) == 0 {
		t.Fatalf("the same cited run of the release's configuration was not taken: %v", err)
	}
}
