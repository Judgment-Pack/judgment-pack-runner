package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestManualStorePrepareCLIRefusesMalformedFrames(t *testing.T) {
	for _, raw := range []string{"", "{}", "{}\n{}\n", "{\n}\n", "{}\n ", strings.Repeat("x", 16384) + "\n", "{\"version\":1,\"Version\":1}\n"} {
		var out, diagnostic bytes.Buffer
		if runnerCommand([]string{"prepare-manual-store"}, strings.NewReader(raw), &out, &diagnostic) == 0 {
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
