package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestManualStorePrepareCLIRefusesMalformedFrames(t *testing.T) {
	for _, raw := range []string{"", "{}", "{}\n{}\n", "{\n}\n", "{}\n ", strings.Repeat("x", 16384) + "\n", "{\"version\":1,\"Version\":1}\n"} {
		var out, diagnostic bytes.Buffer
		cmd := exec.Command(os.Args[0], "-test.run=^TestManualStorePrepareCommandHelper$")
		cmd.Env = append(os.Environ(), "JPACK_MANUAL_CLI_PREPARE_CHILD=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(raw), &out, &diagnostic
		if err := cmd.Run(); err == nil {
			t.Fatal("malformed helper frame accepted")
		}
		var refusal map[string]any
		if json.Unmarshal(out.Bytes(), &refusal) != nil || len(refusal) != 3 || refusal["status"] != "refused" || refusal["code"] != "invalid-request" {
			t.Fatalf("closed refusal %s", out.Bytes())
		}
	}
}

func TestManualAdmissionClosedV3Codec(t *testing.T) {
	var out bytes.Buffer
	if err := printManualCapabilities(&out); err != nil {
		t.Fatal(err)
	}
	want := `{"version":3,"capabilities":["jobs/1","required-boot-capabilities/1","executor-ownership/1","policy-manual-pack-run/1"],"governanceAdmission":true,"governanceProfiles":["local-admin-manual-pack/1"]}` + "\n"
	if out.String() != want {
		t.Fatalf("closed v3 %s", out.String())
	}
}

// The real helper entry terminates its dedicated process, including refusals.
func TestManualStorePrepareCommandHelper(t *testing.T) {
	if os.Getenv("JPACK_MANUAL_CLI_PREPARE_CHILD") == "1" {
		os.Exit(runnerCommand([]string{"prepare-manual-store"}, os.Stdin, os.Stdout, os.Stderr))
	}
}
