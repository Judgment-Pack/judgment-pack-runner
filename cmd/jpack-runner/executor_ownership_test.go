package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ownedRequirement = `,"requiredCapabilities":{"version":1,"capabilities":["jobs/1","required-boot-capabilities/1","executor-ownership/1"]}`
const ownedBinding = `,"executorOwnership":{"version":1,"root":"/trusted/machine/executors/ws_machine","machineWorkspace":"ws_machine","runnerWorkspace":"workspace","rootDevice":"123","rootInode":"9007199254740993","fileDevice":"123","fileInode":"18446744073709551615"}`

func TestExecutorOwnershipBootClosedBinding(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent-state")
	cfg, _, err := bootConfig(capabilityBoot(t, state, ownedRequirement+ownedBinding))
	if err != nil || cfg.ExecutorOwnership == nil {
		t.Fatalf("binding refused: %v", err)
	}
	o := cfg.ExecutorOwnership
	if o.MachineWorkspace != "ws_machine" || o.RunnerWorkspace != cfg.Workspace || o.RootInode != 9007199254740993 || o.FileInode != ^uint64(0) {
		t.Fatalf("binding changed: %+v", o)
	}
	// Both hosts retain the same closed wire fixture, with distinct identities.
	raw, err := os.ReadFile("../../docs/design/executor-ownership-boot.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid shared fixture")
	}
	fixture["workspace"] = json.RawMessage(`"runner-existing-identity"`)
	fixture["token"] = json.RawMessage(`"` + strings.Repeat("t", 32) + `"`)
	line, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = bootConfig(line); err != nil {
		t.Fatalf("shared host fixture: %v", err)
	}
}

func TestExecutorOwnershipBootRefusesBeforeOpen(t *testing.T) {
	cases := map[string]string{
		"ownership without requirement":      ownedBinding,
		"ownership without explicit feature": `,"requiredCapabilities":{"version":1,"capabilities":["jobs/1"]}` + ownedBinding,
		"requirement without ownership":      ownedRequirement,
		"null ownership":                     ownedRequirement + `,"executorOwnership":null`,
		"array ownership":                    ownedRequirement + `,"executorOwnership":[]`,
		"unknown member":                     ownedRequirement + strings.Replace(ownedBinding, `"version":1`, `"unknown":true,"version":1`, 1),
		"case alias":                         ownedRequirement + strings.Replace(ownedBinding, `"executorOwnership"`, `"ExecutorOwnership"`, 1),
		"case aliased root":                  ownedRequirement + strings.Replace(ownedBinding, `"root"`, `"Root"`, 1),
		"duplicate member":                   ownedRequirement + strings.Replace(ownedBinding, `"version":1`, `"version":1,"version":1`, 1),
		"escaped duplicate":                  ownedRequirement + strings.Replace(ownedBinding, `"version":1`, `"version":1,"vers\u0069on":1`, 1),
		"fractional version":                 ownedRequirement + strings.Replace(ownedBinding, `"version":1`, `"version":1.0`, 1),
		"wrong version":                      ownedRequirement + strings.Replace(ownedBinding, `"version":1`, `"version":2`, 1),
		"null root":                          ownedRequirement + strings.Replace(ownedBinding, `"/trusted/machine/executors/ws_machine"`, `null`, 1),
		"relative root":                      ownedRequirement + strings.Replace(ownedBinding, `"/trusted/machine/executors/ws_machine"`, `"relative"`, 1),
		"unclean root":                       ownedRequirement + strings.Replace(ownedBinding, `"/trusted/machine/executors/ws_machine"`, `"/trusted/../root"`, 1),
		"root is filesystem root":            ownedRequirement + strings.Replace(ownedBinding, `"/trusted/machine/executors/ws_machine"`, `"/"`, 1),
		"root contains NUL":                  ownedRequirement + strings.Replace(ownedBinding, `"/trusted/machine/executors/ws_machine"`, `"/trusted/\u0000root"`, 1),
		"machine workspace traversal":        ownedRequirement + strings.Replace(ownedBinding, `"ws_machine"`, `"../workspace"`, 1),
		"runner workspace mismatch":          ownedRequirement + strings.Replace(ownedBinding, `"runnerWorkspace":"workspace"`, `"runnerWorkspace":"different"`, 1),
	}
	for _, field := range []string{"rootDevice", "rootInode", "fileDevice", "fileInode"} {
		for _, value := range []string{`null`, `123`, `""`, `"+1"`, `"-1"`, `"01"`, `" 1"`, `"1.0"`, `"18446744073709551616"`} {
			var binding map[string]json.RawMessage
			if json.Unmarshal([]byte(strings.TrimPrefix(ownedBinding, `,"executorOwnership":`)), &binding) != nil {
				t.Fatal("test binding")
			}
			binding[field] = json.RawMessage(value)
			raw, _ := json.Marshal(binding)
			cases[field+" "+value] = ownedRequirement + `,"executorOwnership":` + string(raw)
		}
	}
	for _, field := range []string{"rootInode", "fileInode"} {
		raw := strings.Replace(ownedBinding, `"`+field+`":"`+map[string]string{"rootInode": "9007199254740993", "fileInode": "18446744073709551615"}[field]+`"`, `"`+field+`":"0"`, 1)
		cases[field+" zero"] = ownedRequirement + raw
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "absent-state")
			line := capabilityBoot(t, state, members)
			if _, _, err := bootConfig(line); err == nil {
				t.Fatal("malformed ownership passed the pre-Open parser")
			}
			var stdout, stderr bytes.Buffer
			status := runnerCommand(nil, bytes.NewReader(line), &stdout, &stderr)
			if status == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("status %d stdout %q stderr %q", status, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), state) || strings.Contains(stderr.String(), "trusted/machine") {
				t.Fatal("binding authority leaked in diagnostics")
			}
			if _, err := os.Lstat(state); !os.IsNotExist(err) {
				t.Fatalf("refusal initialized state: %v", err)
			}
		})
	}
}
