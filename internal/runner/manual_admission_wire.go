package runner

// Private host-only protocol. These types confer no authority by themselves.
// Phase A leaves activation and operational dispatch disabled in Open.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const ManualAdmissionProfile = "local-admin-manual-pack/1"
const ManualIntentHeader = "X-Jpack-Manual-Intent"
const manualFrameLimit = 64 << 10

var errManualProtocol = errors.New("manual admission channel is unavailable or invalid")
var errManualInactive = errors.New("manual admission profile is not activated in this Runner")

type ManualAdmissionBinding struct {
	Version          int    `json:"version"`
	FD               int    `json:"fd"`
	InstallationID   string `json:"installationId"`
	MachineWorkspace string `json:"machineWorkspace"`
	RunnerWorkspace  string `json:"runnerWorkspace"`
	ExecutorID       string `json:"executorId"`
	Challenge        string `json:"challenge"`
	Profile          string `json:"profile"`
}

func (b ManualAdmissionBinding) Validate(o *ExecutorOwnership, workspace string) error {
	if b.Version != 1 || b.FD != 3 || !manualHex(b.InstallationID) || !manualHex(b.MachineWorkspace) || !manualWorkspace(b.RunnerWorkspace) || !manualHex(b.ExecutorID) || !manualHex(b.Challenge) || b.Profile != ManualAdmissionProfile || o == nil || b.MachineWorkspace != o.MachineWorkspace || b.RunnerWorkspace != o.RunnerWorkspace || b.RunnerWorkspace != workspace || o.Validate(workspace) != nil {
		return errManualProtocol
	}
	return nil
}

// ParseManualAdmissionBinding performs closed, case-sensitive parsing only. It
// never opens the descriptor, activates an epoch or grants execution authority.
func ParseManualAdmissionBinding(raw []byte, o *ExecutorOwnership, workspace string) (*ManualAdmissionBinding, error) {
	var b ManualAdmissionBinding
	if manualDecode(raw, &b) != nil || b.Validate(o, workspace) != nil {
		return nil, errManualProtocol
	}
	return &b, nil
}

type ManualSigning struct {
	Mode      string `json:"mode"`
	PublicKey string `json:"publicKey,omitempty"`
}
type ManualResource struct {
	JobID          string `json:"jobId"`
	JobRevision    string `json:"jobRevision"`
	ReleaseID      string `json:"releaseId"`
	ReleaseProfile string `json:"releaseProfile"`
	ReleaseDigest  string `json:"releaseDigest"`
}
type ManualRegistration struct {
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}
type ManualPayload struct {
	Profile string `json:"profile"`
	Digest  string `json:"digest"`
	Bytes   []byte `json:"bytes"`
}
type ManualActivateRequest struct {
	Challenge string        `json:"challenge"`
	Signing   ManualSigning `json:"signing"`
}
type ManualActivateResult struct {
	Delivery      string `json:"delivery"`
	Epoch         string `json:"epoch"`
	RequestDigest string `json:"requestDigest"`
}
type ManualInspectRequest struct {
	Resource ManualResource `json:"resource"`
}
type ManualInspectResult struct {
	Delivery     string             `json:"delivery"`
	Registration ManualRegistration `json:"registration"`
	Eligible     bool               `json:"eligible"`
	Reason       string             `json:"reason"`
	At           string             `json:"at"`
}
type ManualReserveRequest struct {
	RawRequestDigest string             `json:"rawRequestDigest"`
	IntentID         string             `json:"intentId"`
	Resource         ManualResource     `json:"resource"`
	Registration     ManualRegistration `json:"registration"`
	Payload          ManualPayload      `json:"payload"`
}
type ManualReserveResult struct {
	Delivery      string `json:"delivery"`
	OperationID   string `json:"operationId"`
	AttemptID     string `json:"attemptId"`
	Epoch         string `json:"epoch"`
	RequestDigest string `json:"requestDigest"`
}
type ManualStartRequest struct {
	RawRequestDigest string             `json:"rawRequestDigest"`
	IntentID         string             `json:"intentId"`
	Resource         ManualResource     `json:"resource"`
	Registration     ManualRegistration `json:"registration"`
	Payload          ManualPayload      `json:"payload"`
	OperationID      string             `json:"operationId"`
	AttemptID        string             `json:"attemptId"`
	Epoch            string             `json:"epoch"`
}
type ManualStartResult struct {
	Delivery          string `json:"delivery"`
	OperationID       string `json:"operationId"`
	AttemptID         string `json:"attemptId"`
	Epoch             string `json:"epoch"`
	RequestDigest     string `json:"requestDigest"`
	Decision          string `json:"decision"`
	Reason            string `json:"reason"`
	At                string `json:"at"`
	AuthorityRevision string `json:"authorityRevision"`
}
type ManualHistoryRequest struct {
	IntentID string `json:"intentId"`
}
type ManualHistoryResult struct {
	Delivery      string `json:"delivery"`
	IntentID      string `json:"intentId"`
	OperationID   string `json:"operationId"`
	AttemptID     string `json:"attemptId"`
	Epoch         string `json:"epoch"`
	State         string `json:"state"`
	RequestDigest string `json:"requestDigest"`
	Reason        string `json:"reason"`
	At            string `json:"at"`
}

type ManualAuthority interface {
	Activate(context.Context, ManualActivateRequest) (ManualActivateResult, error)
	Inspect(context.Context, ManualInspectRequest) (ManualInspectResult, error)
	Reserve(context.Context, ManualReserveRequest) (ManualReserveResult, error)
	CommitStart(context.Context, ManualStartRequest) (ManualStartResult, error)
	History(context.Context, ManualHistoryRequest) (ManualHistoryResult, error)
	Close() error
}
type ManualAuthorityRefusal struct {
	Code string `json:"code"`
}

func (e *ManualAuthorityRefusal) Error() string { return "manual admission refused: " + e.Code }

func manualHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func manualID(s string) bool { return executorWorkspaceID(s) }
func manualWorkspace(s string) bool {
	if len(s) < 1 || len(s) > 256 {
		return false
	}
	for _, c := range []byte(s) {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func manualPositive(s string) bool {
	n, e := strconv.ParseUint(s, 10, 64)
	return e == nil && n > 0 && strconv.FormatUint(n, 10) == s
}
func manualAt(s string) bool {
	t, e := time.Parse("2006-01-02T15:04:05Z", s)
	return e == nil && t.Format("2006-01-02T15:04:05Z") == s
}
func manualSigning(s ManualSigning) bool {
	if s.Mode == "off" {
		return s.PublicKey == ""
	}
	if s.Mode != "ed25519" || !manualHex(s.PublicKey) {
		return false
	}
	_, err := ParsePublicKey(s.PublicKey)
	return err == nil
}
func manualResource(r ManualResource) bool {
	return manualID(r.JobID) && manualPositive(r.JobRevision) && manualID(r.ReleaseID) && r.ReleaseProfile == runnerCanonicalProfile && validDigest(r.ReleaseDigest)
}
func manualRegistration(r ManualRegistration) bool {
	return manualPositive(r.Revision) && validDigest(r.Digest)
}
func manualDelivery(s string) bool { return s == "committed" || s == "recovered" }
func manualBlocked(s string) bool {
	switch s {
	case "resource-unregistered", "resource-changed", "unsupported-profile", "intent-unavailable", "binding-conflict", "operation-held", "withdrawn", "not-effective", "expired", "superseded", "adoption-changes-requested", "dependency-unavailable", "authority-unavailable", "clock-unavailable", "capacity":
		return true
	}
	return false
}
func manualRefusal(s string) bool {
	switch s {
	case "authority-unavailable", "invalid-request", "unauthorized", "stale-epoch", "resource-unregistered", "resource-changed", "unsupported-profile", "intent-unavailable", "binding-conflict", "operation-held", "eligibility-blocked", "capacity", "clock-unavailable", "history-unavailable":
		return true
	}
	return false
}
func manualPayload(p ManualPayload) bool {
	if p.Profile != executionPayloadProfile || !validDigest(p.Digest) || len(p.Bytes) == 0 || len(p.Bytes) > maxExecutionDescriptor || digest(p.Bytes) != p.Digest {
		return false
	}
	var d executionPayloadDescriptor
	if manualDecode(p.Bytes, &d) != nil || d.Profile != executionPayloadProfile || !validDigest(d.ReleaseDigest) || !validDigest(d.PackDigest) || !validDigest(d.RuntimeDigest) || d.Preparation != nil || d.Source != nil || d.Facts.Encoding != executionExactBytes || !validDigest(d.Facts.Digest) || d.Facts.Bytes < 1 || d.Facts.Bytes > MaxBody {
		return false
	}
	if d.Evidence.Presence == "omitted" {
		if d.Evidence.Value != nil {
			return false
		}
	} else if d.Evidence.Presence == "supplied" {
		v := d.Evidence.Value
		if v == nil || v.Encoding != executionExactBytes || !validDigest(v.Digest) || v.Bytes < 1 || v.Bytes > MaxBody {
			return false
		}
	} else {
		return false
	}
	if !manualSigning(ManualSigning(d.Signing)) {
		return false
	}
	raw, e := canonical(p.Bytes)
	return e == nil && bytes.Equal(raw, p.Bytes)
}
func manualReserve(r ManualReserveRequest) bool {
	if !validDigest(r.RawRequestDigest) || !manualID(r.IntentID) || !manualResource(r.Resource) || !manualRegistration(r.Registration) || !manualPayload(r.Payload) {
		return false
	}
	var d executionPayloadDescriptor
	_ = json.Unmarshal(r.Payload.Bytes, &d)
	return d.ReleaseDigest == r.Resource.ReleaseDigest
}
func manualRequestDigest(method string, body any) (string, error) {
	raw, e := json.Marshal(struct {
		Method string `json:"method"`
		Body   any    `json:"body"`
	}{method, body})
	if e != nil {
		return "", e
	}
	raw, e = canonical(raw)
	if e != nil {
		return "", e
	}
	return digest(raw), nil
}

// manualDecode adds exact field spelling, required-member/null checks and
// canonical base64 to the existing duplicate-safe number-preserving decoder.
func manualDecode(raw []byte, dst any) error {
	if !utf8.Valid(raw) || len(raw) == 0 || len(raw) > manualFrameLimit {
		return errManualProtocol
	}
	if _, err := readInputJSONDepth(raw, 64); err != nil {
		return errManualProtocol
	}
	if err := manualShape(raw, reflect.TypeOf(dst).Elem()); err != nil {
		return err
	}
	if json.Unmarshal(raw, dst) != nil {
		return errManualProtocol
	}
	return nil
}

var manualRawType = reflect.TypeOf(json.RawMessage{})

func manualShape(raw []byte, t reflect.Type) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) || len(raw) == 0 {
		return errManualProtocol
	}
	if t == manualRawType {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		return manualShape(raw, t.Elem())
	}
	if t.Kind() == reflect.Struct {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || m == nil {
			return errManualProtocol
		}
		if t == reflect.TypeOf(ManualSigning{}) || t == reflect.TypeOf(executionPayloadSigning{}) {
			var mode string
			_ = json.Unmarshal(m["mode"], &mode)
			if mode == "off" && len(m) != 1 || mode == "ed25519" && len(m) != 2 {
				return errManualProtocol
			}
		}
		fields := map[string]reflect.StructField{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" || tag[0] == "" {
				continue
			}
			fields[tag[0]] = f
			if _, ok := m[tag[0]]; !ok && !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				return errManualProtocol
			}
		}
		for name, value := range m {
			f, ok := fields[name]
			if !ok || manualShape(value, f.Type) != nil {
				return errManualProtocol
			}
		}
		return nil
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return errManualProtocol
		}
		decoded, e := base64.StdEncoding.Strict().DecodeString(s)
		if e != nil || base64.StdEncoding.EncodeToString(decoded) != s {
			return errManualProtocol
		}
		return nil
	}
	v := reflect.New(t).Interface()
	if json.Unmarshal(raw, v) != nil {
		return errManualProtocol
	}
	return nil
}
