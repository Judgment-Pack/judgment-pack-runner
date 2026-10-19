package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type manualAuthorityClient struct {
	binding     ManualAdmissionBinding
	conn        net.Conn
	reader      *bufio.Reader
	mu          sync.Mutex
	closed      atomic.Bool
	sequence    uint64
	epoch       string
	signing     ManualSigning
	fresh       *ManualStartRequest
	commitProof *manualFreshCommit
}

// NewManualAuthority owns conn and snapshots binding. It is an inactive adapter;
// Open does not connect or activate it until the complete gate is integrated.
func NewManualAuthority(conn net.Conn, binding ManualAdmissionBinding) (ManualAuthority, error) {
	if conn == nil || binding.Version != 1 || binding.FD != 3 || binding.Profile != ManualAdmissionProfile || !manualHex(binding.InstallationID) || !manualHex(binding.MachineWorkspace) || !manualWorkspace(binding.RunnerWorkspace) || !manualHex(binding.ExecutorID) || !manualHex(binding.Challenge) {
		if conn != nil {
			conn.Close()
		}
		return nil, errManualProtocol
	}
	return &manualAuthorityClient{binding: binding, conn: conn, reader: bufio.NewReaderSize(conn, manualFrameLimit)}, nil
}
func (c *manualAuthorityClient) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		return c.conn.Close()
	}
	return nil
}
func (c *manualAuthorityClient) fault() error { c.Close(); return errManualProtocol }

type manualRequestEnvelope struct {
	Version  int             `json:"version"`
	Sequence string          `json:"sequence"`
	Method   string          `json:"method"`
	Body     json.RawMessage `json:"body"`
}
type manualReplyEnvelope struct {
	Version  int             `json:"version"`
	Sequence string          `json:"sequence"`
	Method   string          `json:"method"`
	Status   string          `json:"status"`
	Body     json.RawMessage `json:"body"`
}

func (c *manualAuthorityClient) originalCommit(result ManualStartResult) *manualFreshCommit {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commitProof == nil || c.commitProof.receipt != result {
		return nil
	}
	return c.commitProof
}

// exchange holds one request through validation of the exact reply. A partial
// write or received refusal is never retried. Any uncertain framing/cancellation
// permanently closes the descriptor, including cancellation while waiting.
func (c *manualAuthorityClient) exchange(ctx context.Context, method string, body any, result any, validate func(string) bool) error {
	if ctx == nil {
		return c.fault()
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || ctx.Err() != nil || c.sequence == ^uint64(0) {
		return c.fault()
	}
	if method == "activate" {
		if c.epoch != "" || c.sequence != 0 {
			return c.fault()
		}
	} else if c.epoch == "" {
		return c.fault()
	}
	if method == "commitStart" {
		request, ok := body.(ManualStartRequest)
		if !ok || c.fresh == nil || request.Epoch != c.epoch || !bytes.Equal(encode(*c.fresh), encode(request)) {
			return c.fault()
		}
		c.fresh = nil // consume before any bytes leave; every uncertain exchange holds
	}
	if method == "reserve" && c.fresh != nil {
		return c.fault()
	}
	if method == "reserve" {
		request := body.(ManualReserveRequest)
		var descriptor executionPayloadDescriptor
		if json.Unmarshal(request.Payload.Bytes, &descriptor) != nil || ManualSigning(descriptor.Signing) != c.signing {
			return c.fault()
		}
	}
	raw, err := json.Marshal(body)
	if err != nil || manualDecode(raw, bodyClone(body)) != nil {
		return c.fault()
	}
	requestDigest, err := manualRequestDigest(method, body)
	if err != nil {
		return c.fault()
	}
	c.sequence++
	sequence := strconv.FormatUint(c.sequence, 10)
	frame, err := json.Marshal(manualRequestEnvelope{1, sequence, method, raw})
	if err != nil || len(frame)+1 > manualFrameLimit {
		return c.fault()
	}
	frame = append(frame, '\n')
	until := time.Now().Add(5 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	if c.conn.SetDeadline(until) != nil {
		return c.fault()
	}
	for len(frame) > 0 {
		n, e := c.conn.Write(frame)
		if e != nil || n <= 0 {
			return c.fault()
		}
		frame = frame[n:]
	}
	line, err := c.reader.ReadSlice('\n')
	if err != nil || len(line) > manualFrameLimit || c.reader.Buffered() != 0 {
		return c.fault()
	}
	var reply manualReplyEnvelope
	if manualDecode(line, &reply) != nil || reply.Version != 1 || reply.Sequence != sequence || reply.Method != method {
		return c.fault()
	}
	if reply.Status == "refused" {
		var refusal ManualAuthorityRefusal
		if manualDecode(reply.Body, &refusal) != nil || !manualRefusal(refusal.Code) {
			return c.fault()
		}
		if ctx.Err() != nil {
			return c.fault()
		}
		return &refusal
	}
	if reply.Status != "ok" || manualDecode(reply.Body, result) != nil || !validate(requestDigest) || ctx.Err() != nil || c.closed.Load() {
		return c.fault()
	}
	if method == "commitStart" {
		r := body.(ManualStartRequest)
		out := *result.(*ManualStartResult)
		if out.Delivery == "committed" && out.Decision == "start-committed" {
			c.commitProof = &manualFreshCommit{client: c, request: r, receipt: out}
		}
	}
	return nil
}
func bodyClone(body any) any {
	switch body.(type) {
	case ManualActivateRequest:
		return new(ManualActivateRequest)
	case ManualInspectRequest:
		return new(ManualInspectRequest)
	case ManualReserveRequest:
		return new(ManualReserveRequest)
	case ManualStartRequest:
		return new(ManualStartRequest)
	case ManualHistoryRequest:
		return new(ManualHistoryRequest)
	}
	return new(struct{})
}
func (c *manualAuthorityClient) Activate(ctx context.Context, r ManualActivateRequest) (out ManualActivateResult, err error) {
	if r.Challenge != c.binding.Challenge || !manualSigning(r.Signing) {
		return out, c.fault()
	}
	err = c.exchange(ctx, "activate", r, &out, func(d string) bool {
		if out.Delivery != "committed" || !manualPositive(out.Epoch) || out.RequestDigest != d {
			return false
		}
		c.epoch = out.Epoch
		c.signing = r.Signing
		return true
	})
	return
}
func (c *manualAuthorityClient) Inspect(ctx context.Context, r ManualInspectRequest) (out ManualInspectResult, err error) {
	if !manualResource(r.Resource) {
		return out, c.fault()
	}
	err = c.exchange(ctx, "inspect", r, &out, func(string) bool {
		return out.Delivery == "observed" && manualRegistration(out.Registration) && manualAt(out.At) && (out.Eligible && out.Reason == "eligible" || !out.Eligible && manualBlocked(out.Reason))
	})
	return
}
func (c *manualAuthorityClient) Reserve(ctx context.Context, r ManualReserveRequest) (out ManualReserveResult, err error) {
	r.Payload.Bytes = bytes.Clone(r.Payload.Bytes)
	if !manualReserve(r) {
		return out, c.fault()
	}
	err = c.exchange(ctx, "reserve", r, &out, func(d string) bool {
		if !manualDelivery(out.Delivery) || !manualID(out.OperationID) || !manualID(out.AttemptID) || out.Epoch != c.epoch || out.RequestDigest != d || c.fresh != nil {
			return false
		}
		if out.Delivery == "committed" {
			c.fresh = &ManualStartRequest{IntentID: r.IntentID, RawRequestDigest: r.RawRequestDigest, Resource: r.Resource, Registration: r.Registration, Payload: r.Payload, OperationID: out.OperationID, AttemptID: out.AttemptID, Epoch: out.Epoch}
		}
		return true
	})
	return
}
func (c *manualAuthorityClient) CommitStart(ctx context.Context, r ManualStartRequest) (out ManualStartResult, err error) {
	r.Payload.Bytes = bytes.Clone(r.Payload.Bytes)
	if !manualReserve(ManualReserveRequest{IntentID: r.IntentID, RawRequestDigest: r.RawRequestDigest, Resource: r.Resource, Registration: r.Registration, Payload: r.Payload}) || !manualID(r.OperationID) || !manualID(r.AttemptID) || !manualPositive(r.Epoch) {
		return out, c.fault()
	}
	// Reserve freshness and consumption are checked within the same channel lock
	// as the exchange. A recovered reservation cannot continue to commitStart.
	err = c.exchange(ctx, "commitStart", r, &out, func(d string) bool {
		valid := manualDelivery(out.Delivery) && out.OperationID == r.OperationID && out.AttemptID == r.AttemptID && out.Epoch == r.Epoch && out.RequestDigest == d && manualAt(out.At) && manualHex(out.AuthorityRevision) && (out.Decision == "start-committed" && out.Reason == "start-committed" || out.Decision == "blocked" && manualBlocked(out.Reason))
		return valid
	})
	return
}
func (c *manualAuthorityClient) History(ctx context.Context, r ManualHistoryRequest) (out ManualHistoryResult, err error) {
	if !manualID(r.IntentID) {
		return out, c.fault()
	}
	err = c.exchange(ctx, "history", r, &out, func(string) bool {
		return out.Delivery == "observed" && out.IntentID == r.IntentID && manualID(out.OperationID) && manualID(out.AttemptID) && manualPositive(out.Epoch) && validDigest(out.RequestDigest) && manualAt(out.At) && (out.State == "reserved" && out.Reason == "reserved" || out.State == "start-committed" && out.Reason == "start-committed" || out.State == "blocked" && manualBlocked(out.Reason))
	})
	return
}
