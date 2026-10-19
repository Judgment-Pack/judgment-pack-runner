package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

// This is a transport peer for Runner gate tests, not Desk Policy authority.
// Combined tests use the real anchored Desk core separately.
func manualGateAuthority(t *testing.T, s *Service, change func(string, json.RawMessage) (string, any, bool)) {
	t.Helper()
	a, b := net.Pipe()
	manualGateAuthorityConnections(t, s, a, b, change)
}

func manualGateAuthorityConnections(t *testing.T, s *Service, a, b net.Conn, change func(string, json.RawMessage) (string, any, bool)) {
	t.Helper()
	binding := manualFixture(t).Synthetic.Boot
	binding.RunnerWorkspace = s.cfg.Workspace
	if s.cfg.ExecutorOwnership != nil {
		binding.MachineWorkspace = s.cfg.ExecutorOwnership.MachineWorkspace
	}
	authority, err := NewManualAuthority(a, binding)
	if err != nil {
		t.Fatal(err)
	}
	s.authority = authority.(*manualAuthorityClient)
	s.cfg.ExecutorAdmission = &binding
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		defer b.Close()
		reader := bufio.NewReader(b)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var frame manualRequestEnvelope
			if manualDecode(line, &frame) != nil {
				return
			}
			d, err := manualRequestDigest(frame.Method, frame.Body)
			if err != nil {
				return
			}
			var body any
			switch frame.Method {
			case "activate":
				body = ManualActivateResult{Delivery: "committed", Epoch: "1", RequestDigest: d}
			case "inspect":
				body = ManualInspectResult{Delivery: "observed", Registration: ManualRegistration{Revision: "1", Digest: "sha256:" + strings.Repeat("a", 64)}, Eligible: true, Reason: "eligible", At: "2026-10-19T12:00:00Z"}
			case "reserve":
				body = ManualReserveResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "1", RequestDigest: d}
			case "commitStart":
				body = ManualStartResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "1", RequestDigest: d, Decision: "start-committed", Reason: "start-committed", At: "2026-10-19T12:00:00Z", AuthorityRevision: strings.Repeat("b", 64)}
			default:
				return
			}
			status := "ok"
			if change != nil {
				if next, replacement, drop := change(frame.Method, frame.Body); drop {
					return
				} else if replacement != nil {
					status = next
					body = replacement
				}
			}
			if _, err = b.Write(append(encode(manualReplyEnvelope{1, frame.Sequence, frame.Method, status, encode(body)}), '\n')); err != nil {
				return
			}
		}
	}()
	signing := ManualSigning{Mode: "off"}
	if s.expectedKey != nil {
		signing = ManualSigning{Mode: "ed25519", PublicKey: hexPublic(s.expectedKey)}
	}
	_, err = s.authority.Activate(context.Background(), ManualActivateRequest{Challenge: binding.Challenge, Signing: signing})
	if err != nil {
		t.Fatal(err)
	}
}
