package main

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// Ownership has a closed, case-sensitive wire shape. uint64 identity strings
// avoid JSON number rounding in trusted hosts written in JavaScript.
func bootExecutorOwnership(line []byte, workspace string) (*runner.ExecutorOwnership, error) {
	invalid := errors.New("invalid executor ownership binding")
	var boot map[string]json.RawMessage
	if json.Unmarshal(line, &boot) != nil {
		return nil, invalid
	}
	raw, present := boot["executorOwnership"]
	if !present {
		return nil, nil
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 8 {
		return nil, invalid
	}
	var version int
	if json.Unmarshal(members["version"], &version) != nil || version != 1 {
		return nil, invalid
	}
	var o runner.ExecutorOwnership
	for name, target := range map[string]*string{"root": &o.Root, "machineWorkspace": &o.MachineWorkspace, "runnerWorkspace": &o.RunnerWorkspace} {
		if json.Unmarshal(members[name], target) != nil {
			return nil, invalid
		}
	}
	for name, target := range map[string]*uint64{"rootDevice": &o.RootDevice, "rootInode": &o.RootInode, "fileDevice": &o.FileDevice, "fileInode": &o.FileInode} {
		var text string
		if json.Unmarshal(members[name], &text) != nil || text == "" {
			return nil, invalid
		}
		number, err := strconv.ParseUint(text, 10, 64)
		if err != nil || strconv.FormatUint(number, 10) != text {
			return nil, invalid
		}
		*target = number
	}
	if o.Validate(workspace) != nil {
		return nil, invalid
	}
	return &o, nil
}
