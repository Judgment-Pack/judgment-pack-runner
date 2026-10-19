package runner

import "path/filepath"

// ManualExecutionDemand is independently retained host custody, not permission.
type ManualExecutionDemand struct {
	Version          int    `json:"version"`
	InstallationID   string `json:"installationId"`
	MachineWorkspace string `json:"machineWorkspace"`
	RunnerWorkspace  string `json:"runnerWorkspace"`
	ExecutorID       string `json:"executorId"`
	Profile          string `json:"profile"`
}

func (d ManualExecutionDemand) valid() bool {
	return d.Version == 1 && manualHex(d.InstallationID) && manualHex(d.MachineWorkspace) && manualWorkspace(d.RunnerWorkspace) && manualHex(d.ExecutorID) && d.Profile == ManualAdmissionProfile
}
func (b ManualAdmissionBinding) demand() ManualExecutionDemand {
	return ManualExecutionDemand{1, b.InstallationID, b.MachineWorkspace, b.RunnerWorkspace, b.ExecutorID, b.Profile}
}
func manualDemandDirectory(o ExecutorOwnership) string {
	return filepath.Join(filepath.Dir(o.Root), "policy-execution-v1")
}

func checkManualDemand(o *ExecutorOwnership, binding *ManualAdmissionBinding) error {
	if o == nil {
		if binding != nil {
			return errManualProtocol
		}
		return nil
	}
	demand, err := readManualDemand(*o)
	if err != nil {
		return err
	}
	if demand == nil {
		if binding != nil {
			return errManualProtocol
		}
		return nil
	}
	if binding == nil || *demand != binding.demand() {
		return errManualProtocol
	}
	return nil
}

func verifyManualLease(lease *ExecutorLease) error {
	if lease == nil {
		return errManualProtocol
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.file == nil {
		return errManualProtocol
	}
	return verifyExecutorLease(lease)
}
