package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
)

type manualAttemptJournal struct {
	Version int                `json:"version"`
	State   string             `json:"state"`
	Request ManualStartRequest `json:"request"`
	Receipt *ManualStartResult `json:"receipt,omitempty"`
}

func (s *Service) inspectManualSubmission(jobID string, m *manualIntentInput) (*manualIntentInput, error) {
	if s.authority == nil || m == nil {
		return nil, errManualProtocol
	}
	binding, err := s.manualExecutionBinding(jobID)
	if err != nil {
		return nil, err
	}
	resource := manualResourceOf(binding)
	observation, err := s.authority.Inspect(context.Background(), ManualInspectRequest{Resource: resource})
	if err != nil {
		return nil, err
	}
	// inspect does not authorize dispatch. Reserve checks the trusted intent and
	// current eligibility independently, even after a positive observation.
	if !observation.Eligible {
		return nil, manualUnsupported()
	}
	copy := *m
	copy.resource = &resource
	registration := observation.Registration
	copy.registration = &registration
	return &copy, nil
}
func (s *Service) retainManualAttempt(run Run, q ManualStartRequest) error {
	if run.manual == nil || run.manual.resource == nil || run.manual.registration == nil || q.Resource != *run.manual.resource || q.Registration != *run.manual.registration {
		return errManualProtocol
	}
	record := manualAttemptJournal{Version: 1, State: "reserved", Request: q}
	_, err := s.db.Exec("INSERT INTO manual_attempts(run_id,intent_id,record) VALUES(?,?,?)", run.ID, q.IntentID, string(encode(record)))
	return err
}
func (s *Service) retainManualStart(runID string, q ManualStartRequest, receipt ManualStartResult) error {
	before := manualAttemptJournal{Version: 1, State: "reserved", Request: q}
	after := manualAttemptJournal{Version: 1, State: receipt.Decision, Request: q, Receipt: &receipt}
	result, err := s.db.Exec("UPDATE manual_attempts SET record=? WHERE run_id=? AND record=?", string(encode(after)), runID, string(encode(before)))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errManualProtocol
	}
	return nil
}
func (s *Service) governedEvaluation(ctx context.Context, run Run, r Release) (result, trail []byte, signatures []byte, binding *RunBinding, err error) {
	if s.authority == nil || s.cfg.ExecutorAdmission == nil || run.manual == nil || run.manual.resource == nil || run.manual.registration == nil || !manualRelease(r) || run.Trigger != nil {
		return nil, nil, nil, nil, errManualProtocol
	}
	resource := *run.manual.resource
	if resource.JobID != run.JobID || resource.ReleaseID != run.ReleaseID || resource.JobRevision != decimalRevision(run.Revision) {
		return nil, nil, nil, nil, errManualProtocol
	}
	// Existing local attempt history is a replay barrier, never permission.
	var held int
	if e := s.db.QueryRow("SELECT count(*) FROM manual_attempts WHERE run_id=? OR intent_id=?", run.ID, run.manual.intentID).Scan(&held); e != nil || held != 0 {
		return nil, nil, nil, nil, errManualProtocol
	}
	signing := signingInputs{}
	if s.expectedKey != nil {
		signing = signingInputs{s.cfg.SigningKey, hexPublic(s.expectedKey)}
	}
	payload, e := freezeExecutionPayload(r, run.manual.input(), signing)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	reserve, e := run.manual.reservation(resource, *run.manual.registration, ManualPayload{Profile: executionPayloadProfile, Digest: payload.Digest(), Bytes: payload.Bytes()})
	if e != nil {
		return nil, nil, nil, nil, e
	}
	reserved, e := s.authority.Reserve(ctx, reserve)
	if e != nil || reserved.Delivery != "committed" {
		return nil, nil, nil, nil, errManualProtocol
	}
	q := ManualStartRequest{IntentID: reserve.IntentID, RawRequestDigest: reserve.RawRequestDigest, Resource: reserve.Resource, Registration: reserve.Registration, Payload: reserve.Payload, OperationID: reserved.OperationID, AttemptID: reserved.AttemptID, Epoch: reserved.Epoch}
	if e = s.retainManualAttempt(run, q); e != nil {
		return nil, nil, nil, nil, e
	}
	bin := filepath.Join(s.cfg.Dir, "runtimes", r.RuntimeDigest[len("sha256:"):])
	invoker := func(ctx context.Context, bin, dir, label string, env []string, args ...string) ([]byte, error) {
		return invokeStarting(ctx, bin, dir, label, env, func(cmd *exec.Cmd) error {
			prepared, e := s.prepareGovernedLaunch(q, cmd, r, run.manual.input())
			if e != nil {
				return e
			}
			receipt, e := s.authority.CommitStart(ctx, q)
			if e != nil {
				return e
			}
			if receipt.Decision != "start-committed" || receipt.Delivery != "committed" {
				s.retainManualStart(run.ID, q, receipt)
				return errManualProtocol
			}
			launch, e := prepared.retainFreshCommit(s.authority.originalCommit(receipt), func(r ManualStartResult) error { return s.retainManualStart(run.ID, q, r) })
			if e != nil {
				return e
			}
			return s.startRuntimeAs(cmd, manualOperationalInvocation, launch)
		}, args...)
	}
	return evaluateWithInvoker(ctx, bin, r, run.manual.input(), filepath.Join(s.cfg.Dir, "attempts", run.ID), false, signing, invoker)
}

func decimalRevision(revision int) string { return strconv.Itoa(revision) }

func manualResourceOf(binding ManualExecutionBinding) ManualResource {
	return ManualResource{JobID: binding.JobID, JobRevision: binding.JobRevision, ReleaseID: binding.ReleaseID, ReleaseProfile: binding.ReleaseProfile, ReleaseDigest: binding.ReleaseDigest}
}
