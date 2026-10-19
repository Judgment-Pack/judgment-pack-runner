package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	jobsCapability              = "jobs/1"
	requiredBootCapability      = "required-boot-capabilities/1"
	executorOwnershipCapability = "executor-ownership/1"
)

// This is a private, closed versioned contract; see docs/design/runner-capabilities.md.
// Executor ownership is process exclusion and does not implement admission.
func printCapabilities(args []string, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "--format" || args[1] != "json" {
		return fmt.Errorf("usage: jpack-runner capabilities --format json")
	}
	return json.NewEncoder(stdout).Encode(struct {
		Version             int      `json:"version"`
		Capabilities        []string `json:"capabilities"`
		GovernanceAdmission bool     `json:"governanceAdmission"`
	}{2, []string{jobsCapability, requiredBootCapability, executorOwnershipCapability}, false})
}

// checkBootCapabilities runs before runner.Open: even a rejected requirement
// must not create state, pin a Runtime, recover a queue, or start workers.
func checkBootCapabilities(line []byte) error {
	invalidBoot := fmt.Errorf("invalid boot configuration")
	if len(line) > 128<<10 {
		return invalidBoot
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := uniqueJSONValue(decoder, 0); err != nil {
		return invalidBoot
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalidBoot
	}
	var boot map[string]json.RawMessage
	if err := json.Unmarshal(line, &boot); err != nil || boot == nil {
		return invalidBoot
	}
	for name := range boot {
		// encoding/json accepts case-insensitive struct field names. Do not let
		// a misspelled requirement disappear or replace a stronger requirement.
		if name != "executorOwnership" && strings.EqualFold(name, "executorOwnership") {
			return fmt.Errorf("invalid executor ownership binding")
		}
		if name != "requiredCapabilities" && strings.EqualFold(name, "requiredCapabilities") {
			return fmt.Errorf("invalid required boot capabilities")
		}
	}
	raw, required := boot["requiredCapabilities"]
	if !required {
		if _, present := boot["executorOwnership"]; present {
			return fmt.Errorf("executor ownership requires its boot capability")
		}
		return nil
	}
	invalidRequirements := fmt.Errorf("invalid required boot capabilities")
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || len(members) != 2 {
		return invalidRequirements
	}
	var version int
	if err := json.Unmarshal(members["version"], &version); err != nil || version != 1 {
		return invalidRequirements
	}
	var capabilities []string
	if err := json.Unmarshal(members["capabilities"], &capabilities); err != nil || capabilities == nil || len(capabilities) > 16 {
		return invalidRequirements
	}
	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || len(capability) > 64 || seen[capability] {
			return invalidRequirements
		}
		seen[capability] = true
		if capability != jobsCapability && capability != requiredBootCapability && capability != executorOwnershipCapability {
			return fmt.Errorf("unsupported required boot capability")
		}
	}
	_, owned := boot["executorOwnership"]
	if owned != seen[executorOwnershipCapability] {
		return fmt.Errorf("executor ownership and its boot capability must agree")
	}
	return nil
}

// uniqueJSONValue rejects duplicate object members at every level, including
// escaped spellings of the same name. encoding/json otherwise keeps the last
// value, allowing a requirement to be silently weakened. The boot byte bound
// and depth bound keep pre-start validation small.
func uniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("boot JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid boot JSON member")
			}
			seen[name] = true
			if err := uniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid boot JSON value")
	}
	_, err = decoder.Token()
	return err
}
