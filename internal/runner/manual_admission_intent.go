package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"
)

// The private header is only an intent selector. Its syntax or presence is not
// authentication, registration or permission; later reserve/start must match
// Desk's anchored trusted intent against these exact bytes.
type manualIntentInput struct {
	intentID, key, request, facts, evidence string
	supplied                                bool
	resource                                *ManualResource
	registration                            *ManualRegistration
}
type manualIntentStored struct {
	Version          int                 `json:"version"`
	IntentID         string              `json:"intentId"`
	IdempotencyKey   string              `json:"idempotencyKey"`
	RequestDigest    string              `json:"requestDigest"`
	Request          []byte              `json:"requestBytes"`
	Facts            []byte              `json:"factsBytes"`
	EvidencePresence string              `json:"evidencePresence"`
	Evidence         []byte              `json:"evidenceBytes,omitempty"`
	Resource         *ManualResource     `json:"resource,omitempty"`
	Registration     *ManualRegistration `json:"registration,omitempty"`
}

func captureManualIntent(id, key string, raw []byte) (*manualIntentInput, error) {
	if !manualID(id) || len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c < 33 || c > 126 }) >= 0 || len(raw) > MaxBody || !utf8.Valid(raw) {
		return nil, errManualProtocol
	}
	if _, err := readInputJSONDepth(raw, 64); err != nil {
		return nil, errManualProtocol
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errManualProtocol
	}
	facts, ok := fields["facts"]
	if !ok {
		return nil, errManualProtocol
	}
	evidence, supplied := fields["evidence"]
	expectedFields := 1
	if supplied {
		expectedFields++
	}
	if len(fields) != expectedFields {
		return nil, manualUnsupported()
	}
	input := Input{Facts: bytes.Clone(facts), Evidence: bytes.Clone(evidence)}
	if input.validate() != nil {
		return nil, errManualProtocol
	}
	return &manualIntentInput{intentID: id, key: key, request: string(raw), facts: string(facts), evidence: string(evidence), supplied: supplied}, nil
}
func (m *manualIntentInput) input() Input {
	return Input{Facts: []byte(m.facts), Evidence: []byte(m.evidence)}
}
func (m *manualIntentInput) stored() *manualIntentStored {
	p := "omitted"
	if m.supplied {
		p = "supplied"
	}
	return &manualIntentStored{Version: 1, IntentID: m.intentID, IdempotencyKey: m.key, RequestDigest: digest([]byte(m.request)), Request: []byte(m.request), Facts: []byte(m.facts), EvidencePresence: p, Evidence: []byte(m.evidence), Resource: m.resource, Registration: m.registration}
}
func (s manualIntentStored) restore(held Input) (*manualIntentInput, error) {
	if s.Version != 1 || s.RequestDigest != digest(s.Request) || held.Source != nil || held.Preparation != nil {
		return nil, errManualProtocol
	}
	restored, err := captureManualIntent(s.IntentID, s.IdempotencyKey, s.Request)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(s.Facts, []byte(restored.facts)) || !bytes.Equal(s.Evidence, []byte(restored.evidence)) || s.EvidencePresence != restored.stored().EvidencePresence || !sameJSON(held.Facts, s.Facts) || (len(held.Evidence) != 0) != (s.EvidencePresence == "supplied") || s.EvidencePresence == "supplied" && !sameJSON(held.Evidence, s.Evidence) {
		return nil, errManualProtocol
	}
	if (s.Resource == nil) != (s.Registration == nil) || s.Resource != nil && (!manualResource(*s.Resource) || !manualRegistration(*s.Registration)) {
		return nil, errManualProtocol
	}
	if s.Resource != nil {
		resource, registration := *s.Resource, *s.Registration
		restored.resource = &resource
		restored.registration = &registration
	}
	return restored, nil
}

var reflectManualStored = reflect.TypeOf(manualIntentStored{})

func manualStoredDecode(raw []byte, dst *manualIntentStored) error {
	if len(raw) > 4*MaxBody+4096 || !utf8.Valid(raw) {
		return errManualProtocol
	}
	if _, err := readInputJSONDepth(raw, 64); err != nil {
		return errManualProtocol
	}
	if manualShape(raw, reflectManualStored) != nil || json.Unmarshal(raw, dst) != nil {
		return errManualProtocol
	}
	return nil
}
func (s *Service) manualOwnerInput(w http.ResponseWriter, r *http.Request, key string) (Input, *manualIntentInput, error) {
	values := r.Header.Values(ManualIntentHeader)
	if s.cfg.ExecutorAdmission == nil {
		if len(values) != 0 {
			return Input{}, nil, &apiError{400, "unexpected_manual_intent", "This Runner has no activated manual admission profile."}
		}
		var input Input
		err := decodeInput(w, r, &input)
		return input, nil, err
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || keys[0] != key {
		return Input{}, nil, &apiError{400, "idempotency_key_required", "Supply exactly one original Idempotency-Key."}
	}
	if len(values) != 1 {
		return Input{}, nil, &apiError{400, "manual_intent_required", "Supply the original host-prepared manual intent selector."}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		return Input{}, nil, errManualProtocol
	}
	intent, err := captureManualIntent(values[0], key, raw)
	if err != nil {
		return Input{}, nil, err
	}
	return intent.input(), intent, nil
}

// reservation binds the original host-selector ingress to the frozen descriptor
// without inventing caller identity. Desk still validates the actual intent and
// all current registration/epoch/operation state on the dedicated channel.
func (m *manualIntentInput) reservation(resource ManualResource, registration ManualRegistration, payload ManualPayload) (ManualReserveRequest, error) {
	if m == nil {
		return ManualReserveRequest{}, errManualProtocol
	}
	request := ManualReserveRequest{IntentID: m.intentID, RawRequestDigest: digest([]byte(m.request)), Resource: resource, Registration: registration, Payload: payload}
	request.Payload.Bytes = bytes.Clone(payload.Bytes)
	if !manualReserve(request) {
		return ManualReserveRequest{}, errManualProtocol
	}
	var descriptor executionPayloadDescriptor
	_ = json.Unmarshal(payload.Bytes, &descriptor)
	if descriptor.Facts.Digest != digest([]byte(m.facts)) || descriptor.Facts.Bytes != len(m.facts) {
		return ManualReserveRequest{}, errManualProtocol
	}
	if m.supplied {
		e := descriptor.Evidence.Value
		if descriptor.Evidence.Presence != "supplied" || e == nil || e.Digest != digest([]byte(m.evidence)) || e.Bytes != len(m.evidence) {
			return ManualReserveRequest{}, errManualProtocol
		}
	} else if descriptor.Evidence.Presence != "omitted" || descriptor.Evidence.Value != nil {
		return ManualReserveRequest{}, errManualProtocol
	}
	return request, nil
}
