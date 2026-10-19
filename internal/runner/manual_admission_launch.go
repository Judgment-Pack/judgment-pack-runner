package runner

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"sync"
)

// These private types are dormant. Phase C must construct preparation only
// after verifying all actual Runtime files against the frozen payload, then
// wire this starter into the common invocation boundary. No current call can
// enable the profile or obtain a permit by presenting a receipt over HTTP.
type manualPreparedLaunch struct {
	mu            sync.Mutex
	used          bool
	request       ManualStartRequest
	invocation    string
	runtimeDigest string
	lease         *ExecutorLease
	client        *manualAuthorityClient
	material      *manualLaunchMaterial
}
type manualLaunch struct {
	mu                        sync.Mutex
	consumed                  bool
	invocation, runtimeDigest string
	lease                     *ExecutorLease
	client                    *manualAuthorityClient
	material                  *manualLaunchMaterial
}

// Only the original channel client stores this pointer after checking a fresh
// reply. A receipt DTO, persisted journal or recovered observation is not it.
type manualFreshCommit struct {
	client  *manualAuthorityClient
	request ManualStartRequest
	receipt ManualStartResult
}

func prepareManualLaunch(request ManualStartRequest, cmd *exec.Cmd, lease *ExecutorLease) (*manualPreparedLaunch, error) {
	request.Payload.Bytes = bytes.Clone(request.Payload.Bytes)
	if lease == nil || cmd == nil || cmd.Process != nil || !manualReserve(ManualReserveRequest{IntentID: request.IntentID, RawRequestDigest: request.RawRequestDigest, Resource: request.Resource, Registration: request.Registration, Payload: request.Payload}) || !manualID(request.OperationID) || !manualID(request.AttemptID) || !manualPositive(request.Epoch) {
		return nil, errManualProtocol
	}
	var descriptor executionPayloadDescriptor
	if json.Unmarshal(request.Payload.Bytes, &descriptor) != nil {
		return nil, errManualProtocol
	}
	invocation, err := manualInvocation(cmd, descriptor.RuntimeDigest)
	if err != nil {
		return nil, err
	}
	return &manualPreparedLaunch{request: request, invocation: invocation, runtimeDigest: descriptor.RuntimeDigest, lease: lease}, nil
}
func (p *manualPreparedLaunch) retainFreshCommit(proof *manualFreshCommit, persist func(ManualStartResult) error) (*manualLaunch, error) {
	if p == nil {
		return nil, errManualProtocol
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return nil, errManualProtocol
	}
	p.used = true
	if proof == nil || proof.client == nil || p.client != proof.client {
		return nil, errManualProtocol
	}
	c := proof.client
	c.mu.Lock()
	owned := c.commitProof == proof && c.epoch == p.request.Epoch && bytes.Equal(encode(proof.request), encode(p.request))
	if c.commitProof == proof {
		c.commitProof = nil
	}
	c.mu.Unlock()
	if !owned {
		return nil, errManualProtocol
	}
	receipt := proof.receipt
	expected, err := manualRequestDigest("commitStart", p.request)
	if err != nil || receipt.Delivery != "committed" || receipt.Decision != "start-committed" || receipt.Reason != "start-committed" || receipt.OperationID != p.request.OperationID || receipt.AttemptID != p.request.AttemptID || receipt.Epoch != p.request.Epoch || receipt.RequestDigest != expected || !manualAt(receipt.At) || !manualHex(receipt.AuthorityRevision) || persist == nil {
		return nil, errManualProtocol
	}
	if err = persist(receipt); err != nil {
		return nil, err
	}
	return &manualLaunch{invocation: p.invocation, runtimeDigest: p.runtimeDigest, lease: p.lease, client: p.client, material: p.material}, nil
}
func (l *manualLaunch) start(cmd *exec.Cmd, lease *ExecutorLease) error {
	if l == nil {
		return errManualProtocol
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.consumed {
		return errManualProtocol
	}
	l.consumed = true
	if lease == nil || lease != l.lease {
		return errManualProtocol
	}
	if l.material != nil && l.material.verify(cmd) != nil {
		return errManualProtocol
	}
	invocation, err := manualInvocation(cmd, l.runtimeDigest)
	if err != nil || invocation != l.invocation {
		return errManualProtocol
	}
	_, err = lease.Start(cmd)
	return err
}
func manualInvocation(cmd *exec.Cmd, runtimeDigest string) (string, error) {
	if cmd == nil || cmd.Process != nil || len(cmd.ExtraFiles) != 0 {
		return "", errManualProtocol
	}
	image, err := readLimit(cmd.Path, 128<<20)
	if err != nil || digest(image) != runtimeDigest {
		return "", errManualProtocol
	}
	raw, err := canonical(encode(struct {
		Path, Dir string
		Args, Env []string
	}{cmd.Path, cmd.Dir, cmd.Args, cmd.Env}))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
