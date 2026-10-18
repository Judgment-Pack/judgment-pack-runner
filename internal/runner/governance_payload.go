package runner

// This private codec describes frozen operational input. It is not a permit,
// a Policy format, or an execution path.

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	executionPayloadProfile = "judgment-pack-runner/execution-payload/1"
	// runnerCanonicalProfile names the existing canonical(encode(typed))
	// encoding, including InputMapping's existing MarshalJSON behavior.
	runnerCanonicalProfile = "judgment-pack-runner/canonical/1"
	executionExactBytes    = "exact-bytes"
	maxExecutionRelease    = 8 << 20
	maxExecutionDescriptor = 4 << 10
)

// executionPayload is immutable: the descriptor's only retained state is a
// string, and Bytes returns a new allocation. Component preimages stay with
// their owning release and run; their digests commit to them here.
type executionPayload struct{ descriptor string }

func (p executionPayload) Bytes() []byte { return []byte(p.descriptor) }
func (p executionPayload) Digest() string {
	if p.descriptor == "" {
		return ""
	}
	return digest([]byte(p.descriptor))
}

type executionPayloadReference struct {
	Encoding string `json:"encoding"`
	Digest   string `json:"digest"`
	Bytes    int    `json:"bytes"`
}

type executionPayloadEvidence struct {
	Presence string                     `json:"presence"`
	Value    *executionPayloadReference `json:"value,omitempty"`
}

type executionPayloadSigning struct {
	Mode      string `json:"mode"`
	PublicKey string `json:"publicKey,omitempty"`
}

// All descriptor fields are closed host-owned strings, bounded integers or
// references. Facts and evidence never enter canonical's JSON value domain.
type executionPayloadDescriptor struct {
	Profile       string                     `json:"profile"`
	ReleaseDigest string                     `json:"releaseDigest"`
	PackDigest    string                     `json:"packDigest"`
	RuntimeDigest string                     `json:"runtimeDigest"`
	Facts         executionPayloadReference  `json:"facts"`
	Evidence      executionPayloadEvidence   `json:"evidence"`
	Preparation   *executionPayloadReference `json:"preparation,omitempty"`
	Source        *executionPayloadReference `json:"source,omitempty"`
	Signing       executionPayloadSigning    `json:"signing"`
}

// freezeExecutionPayload commits to the material evaluateWith consumes and the
// retained preparation/source material Service.evaluate checks. Callers must
// supply the operational signingInputs, not a request-selected public key.
// This function neither validates eligibility nor verifies a Runtime binary.
// It must be called on already prepared input; it does not normalize input.
func freezeExecutionPayload(release Release, input Input, signing signingInputs) (executionPayload, error) {
	ref := func(raw []byte, encoding string) *executionPayloadReference {
		return &executionPayloadReference{Encoding: encoding, Digest: digest(raw), Bytes: len(raw)}
	}
	fail := func(err error) (executionPayload, error) {
		return executionPayload{}, fmt.Errorf("execution payload: %w", err)
	}
	if !validDigest(release.PackDigest) || !validDigest(release.RuntimeDigest) {
		return fail(errors.New("invalid Pack or Runtime digest"))
	}
	for _, material := range []struct {
		name string
		raw  []byte
		max  int
	}{
		{"Pack", []byte(release.Pack), 1 << 20},
		{"configuration", []byte(release.Config), MaxBody},
		{"lock", []byte(release.Lock), MaxBody},
		{"facts", input.Facts, MaxBody},
	} {
		if err := executionPayloadRaw(material.raw, material.max); err != nil {
			return fail(fmt.Errorf("%s: %w", material.name, err))
		}
	}
	if digest([]byte(release.Pack)) != release.PackDigest {
		return fail(errors.New("Pack bytes differ from the release digest"))
	}
	releaseBytes, err := executionPayloadCanonical(release, maxExecutionRelease)
	if err != nil {
		return fail(fmt.Errorf("release: %w", err))
	}
	descriptor := executionPayloadDescriptor{
		Profile: executionPayloadProfile, ReleaseDigest: digest(releaseBytes),
		PackDigest: release.PackDigest, RuntimeDigest: release.RuntimeDigest,
		Facts:    *ref(input.Facts, executionExactBytes),
		Evidence: executionPayloadEvidence{Presence: "omitted"},
		Signing:  executionPayloadSigning{Mode: "off"},
	}
	// evaluateWith writes evidence.json and adds --evidence for every nonempty
	// slice, including the bytes "null". A nil or empty slice omits both.
	if len(input.Evidence) != 0 {
		if err := executionPayloadRaw(input.Evidence, MaxBody); err != nil {
			return fail(fmt.Errorf("evidence: %w", err))
		}
		descriptor.Evidence = executionPayloadEvidence{Presence: "supplied", Value: ref(input.Evidence, executionExactBytes)}
	}
	if input.Preparation != nil {
		raw, err := executionPayloadCanonical(input.Preparation, MaxBody)
		if err != nil {
			return fail(fmt.Errorf("preparation: %w", err))
		}
		descriptor.Preparation = ref(raw, runnerCanonicalProfile)
	}
	if input.Source != nil {
		raw, err := executionPayloadCanonical(input.Source, MaxBody)
		if err != nil {
			return fail(fmt.Errorf("source: %w", err))
		}
		descriptor.Source = ref(raw, runnerCanonicalProfile)
	}
	if signing.path == "" {
		if signing.publicKey != "" {
			return fail(errors.New("signing public key supplied without operational signing"))
		}
	} else {
		if _, err := ParsePublicKey(signing.publicKey); err != nil {
			return fail(errors.New("invalid operational signing public key"))
		}
		descriptor.Signing = executionPayloadSigning{Mode: "ed25519", PublicKey: signing.publicKey}
	}
	raw, err := executionPayloadCanonical(descriptor, maxExecutionDescriptor)
	if err != nil {
		return fail(err)
	}
	return executionPayload{descriptor: string(raw)}, nil
}

// executionPayloadRaw checks syntax and encoding without decoding, converting
// numbers, compacting, or re-encoding the bytes that Runtime will receive.
// Duplicate member semantics are left to existing input admission and Runtime;
// this exact-byte commitment does not collapse those members.
func executionPayloadRaw(raw []byte, limit int) error {
	if len(raw) == 0 || len(raw) > limit {
		return errors.New("JSON bytes are empty or exceed the limit")
	}
	if !utf8.Valid(raw) || !validInputEscapes(raw) || !json.Valid(raw) {
		return errors.New("invalid JSON bytes")
	}
	return nil
}

// executionPayloadCanonical checks the typed encoding before using Runner's
// existing canonical helper. UseNumber preserves arbitrary JSON number lexemes;
// this is not RFC 8785 or the candidate Policy safe-integer canonical profile.
func executionPayloadCanonical(value any, limit int) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("invalid typed JSON material")
	}
	if len(raw) > limit {
		return nil, errors.New("typed JSON material exceeds the limit")
	}
	if _, err = readInputJSONDepth(raw, 64); err != nil {
		return nil, errors.New("invalid typed JSON material")
	}
	raw, err = canonical(raw)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("canonical JSON material exceeds the limit")
	}
	return raw, nil
}
