package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

func manualBootFixture(t *testing.T, state string) map[string]any {
	t.Helper()
	var wire struct {
		Synthetic struct {
			Boot map[string]any `json:"boot"`
		} `json:"synthetic"`
	}
	raw, err := os.ReadFile("../../docs/design/manual-admission-wire.json")
	if err != nil || json.Unmarshal(raw, &wire) != nil {
		t.Fatal("wire fixture", err)
	}
	b := wire.Synthetic.Boot
	return map[string]any{"dir": state, "runtime": "/missing-runtime", "workspace": b["runnerWorkspace"], "owner": "owner", "token": strings.Repeat("s", 32), "executorOwnership": map[string]any{"version": 1, "root": "/never-opened", "machineWorkspace": b["machineWorkspace"], "runnerWorkspace": b["runnerWorkspace"], "rootDevice": "1", "rootInode": "1", "fileDevice": "1", "fileInode": "1"}, "requiredCapabilities": map[string]any{"version": 1, "capabilities": []string{"jobs/1", "required-boot-capabilities/1", "executor-ownership/1", "policy-manual-pack-run/1"}}, "executorAdmission": b}
}
func TestManualAdmissionBootSnapshotRefusesUnsafeOwnership(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent")
	boot := manualBootFixture(t, state)
	line, _ := json.Marshal(boot)
	cfg, _, err := bootConfig(line)
	if err != nil || cfg.ExecutorAdmission == nil {
		t.Fatal("strict snapshot", err)
	}
	if cfg.ExecutorAdmission.FD != 3 || cfg.ExecutorAdmission.Profile != "local-admin-manual-pack/1" {
		t.Fatal("binding changed")
	}
	boot["executorAdmission"].(map[string]any)["challenge"] = "mutated"
	if cfg.ExecutorAdmission.Challenge == "mutated" {
		t.Fatal("mutable binding")
	}
	var stdout, stderr bytes.Buffer
	if status := runnerCommand(nil, bytes.NewReader(append(line, '\n')), &stdout, &stderr); status == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("unsafe ownership escaped: %d %q %q", status, stdout.String(), stderr.String())
	}
	if runner.ManualAdmissionAvailable() && strings.Contains(stderr.String(), "not activated") {
		t.Fatal("enabled candidate retained inactive hold")
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatal("unsafe ownership touched state", err)
	}
}
func TestManualAdmissionBootClosedShapes(t *testing.T) {
	edits := map[string]func(map[string]any){
		"missing requirement": func(b map[string]any) { delete(b, "requiredCapabilities") },
		"missing binding":     func(b map[string]any) { delete(b, "executorAdmission") },
		"missing ownership":   func(b map[string]any) { delete(b, "executorOwnership") },
		"unknown member":      func(b map[string]any) { b["executorAdmission"].(map[string]any)["url"] = "http://never" },
		"case aliased member": func(b map[string]any) {
			a := b["executorAdmission"].(map[string]any)
			a["Challenge"] = a["challenge"]
			delete(a, "challenge")
		},
		"case aliased binding": func(b map[string]any) {
			b["ExecutorAdmission"] = b["executorAdmission"]
			delete(b, "executorAdmission")
		},
		"null":                func(b map[string]any) { b["executorAdmission"] = nil },
		"null fd":             func(b map[string]any) { b["executorAdmission"].(map[string]any)["fd"] = nil },
		"wrong fd":            func(b map[string]any) { b["executorAdmission"].(map[string]any)["fd"] = 4 },
		"string fd":           func(b map[string]any) { b["executorAdmission"].(map[string]any)["fd"] = "3" },
		"unsupported profile": func(b map[string]any) { b["executorAdmission"].(map[string]any)["profile"] = "other/1" },
		"runner mismatch":     func(b map[string]any) { b["executorAdmission"].(map[string]any)["runnerWorkspace"] = "other" },
		"machine mismatch": func(b map[string]any) {
			b["executorAdmission"].(map[string]any)["machineWorkspace"] = strings.Repeat("e", 64)
		},
		"nonhex executor": func(b map[string]any) { b["executorAdmission"].(map[string]any)["executorId"] = "executor_browser" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "absent")
			boot := manualBootFixture(t, state)
			edit(boot)
			line, _ := json.Marshal(boot)
			if _, _, err := bootConfig(line); err == nil {
				t.Fatal("malformed binding passed parser")
			}
			var stdout, stderr bytes.Buffer
			if runnerCommand(nil, bytes.NewReader(append(line, '\n')), &stdout, &stderr) == 0 || stdout.Len() != 0 {
				t.Fatal("malformed binding started")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("state created", err)
			}
		})
	}
	state := filepath.Join(t.TempDir(), "absent")
	raw, _ := json.Marshal(manualBootFixture(t, state))
	duplicate := bytes.Replace(raw, []byte(`"fd":3`), []byte(`"fd":3,"f\u0064":3`), 1)
	if _, _, err := bootConfig(duplicate); err == nil {
		t.Fatal("escaped duplicate accepted")
	}
	fractional := bytes.Replace(raw, []byte(`"fd":3`), []byte(`"fd":3.0`), 1)
	if _, _, err := bootConfig(fractional); err == nil {
		t.Fatal("fractional fd accepted")
	}
}
