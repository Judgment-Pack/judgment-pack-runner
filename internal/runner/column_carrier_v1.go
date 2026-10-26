package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

// This parser establishes structure and separate byte identities only. It does
// not authorize preparation, human review, or an original declaration inventory.
// In particular, even a verified signature over this wrapper alone supplies no
// unspecified relation between its frame and its original snapshot.
type retainedColumnCarrier struct {
	OriginalSnapshot                      []byte
	RawFrame                              json.RawMessage
	Frame                                 retainedcolumns.Frame
	OriginalSnapshotSHA256, CarrierSHA256 string
}

func readColumnCarrier(raw []byte, selection ...*ConversionContract) (retainedColumnCarrier, error) {
	return readColumnCarrierLimits(raw, nil, selection...)
}
func readColumnCarrierLimits(raw []byte, limits map[string]int, selection ...*ConversionContract) (retainedColumnCarrier, error) {
	var c retainedColumnCarrier
	badCarrier := func() (retainedColumnCarrier, error) {
		return c, withCode(codeMalformed, errors.New("invalid retained-column generated carrier"))
	}
	if e := selectedConversion(selection).validate("producer-carrier"); e != nil {
		return c, e
	}
	if e := conversionArtifactBound(limits, "carrierBytes", len(raw)); e != nil {
		return c, e
	}
	if len(raw) > artifactLimit(selectedConversion(selection), MaxBody, columnCarrierLimit) {
		return c, withCode(codeOverBound, errors.New("retained-column carrier exceeds base limit"))
	}
	if _, e := readInputJSONDepth(raw, maxDocumentDepth); e != nil {
		return badCarrier()
	}
	m, e := exactMembers(raw)
	if e != nil || len(m) != 4 {
		return badCarrier()
	}
	for _, k := range []string{"contractVersion", "originalSnapshotJson", "originalSnapshotSha256", "retained"} {
		if _, ok := m[k]; !ok {
			return badCarrier()
		}
	}
	var version, snapshot, pin string
	if json.Unmarshal(m["contractVersion"], &version) != nil || version != "1" || json.Unmarshal(m["originalSnapshotJson"], &snapshot) != nil || json.Unmarshal(m["originalSnapshotSha256"], &pin) != nil || !validDigest(pin) || pin != digest([]byte(snapshot)) {
		return badCarrier()
	}
	if e := conversionArtifactBound(limits, "originalSnapshotBytes", len(snapshot)); e != nil {
		return c, e
	}
	if len(snapshot) > 524288 {
		return c, withCode(codeOverBound, errors.New("original reviewed snapshot exceeds producer bound"))
	}
	if !snapshotObject([]byte(snapshot)) {
		return badCarrier()
	}
	frame, e := retainedcolumns.ParseFrame(m["retained"], retainedcolumns.Base)
	if e != nil {
		return c, columnError(e)
	}
	if frame.Origin() != "generated" {
		return badCarrier()
	}
	c = retainedColumnCarrier{OriginalSnapshot: []byte(snapshot), RawFrame: append(json.RawMessage(nil), m["retained"]...), Frame: frame, OriginalSnapshotSHA256: pin, CarrierSHA256: digest(raw)}
	return c, nil
}

// Snapshot text is opaque for restoration, including historical lone-surrogate
// escapes. Compare decoded member names as UTF-16 code units so distinct legacy
// surrogates are not collapsed into replacement characters by encoding/json.
func snapshotName(raw []byte) string {
	raw = bytes.TrimSpace(raw)
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte(",")))
	var units []uint16
	for i := 1; i < len(raw)-1; {
		if raw[i] != '\\' {
			r, n := utf8.DecodeRune(raw[i:])
			units = append(units, utf16.Encode([]rune{r})...)
			i += n
			continue
		}
		i++
		if raw[i] == 'u' {
			var n uint16
			for j := 1; j <= 4; j++ {
				n *= 16
				x := raw[i+j]
				switch {
				case x >= '0' && x <= '9':
					n += uint16(x - '0')
				case x >= 'a' && x <= 'f':
					n += uint16(x - 'a' + 10)
				default:
					n += uint16(x - 'A' + 10)
				}
			}
			units = append(units, n)
			i += 5
			continue
		}
		escaped := map[byte]uint16{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}
		units = append(units, escaped[raw[i]])
		i++
	}
	var identity strings.Builder
	for _, u := range units {
		identity.WriteByte(byte(u >> 8))
		identity.WriteByte(byte(u))
	}
	return identity.String()
}
func snapshotObject(raw []byte) bool {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) bool
	read = func(depth int) bool {
		if depth > maxDocumentDepth {
			return false
		}
		t, e := d.Token()
		if e != nil {
			return false
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				before := d.InputOffset()
				if _, e = d.Token(); e != nil {
					return false
				}
				key := snapshotName(raw[before:d.InputOffset()])
				if seen[key] {
					return false
				}
				seen[key] = true
				if !read(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim('}')
		case json.Delim('['):
			for d.More() {
				if !read(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim(']')
		}
		return true
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !read(0) {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}

type retainedAcquisitionResult struct{ Canonical, Raw json.RawMessage }

// The signature domain remains the canonical result. Raw preserves the received
// carrier/frame spelling separately, never pretending that a signature pins raw
// serialization. Existing verifyAcquisition callers keep their old contract.
func verifyRetainedAcquisition(raw, args []byte, p InputProfile, public ed25519.PublicKey, at time.Time, maxAge int, selection ...*ConversionContract) (retainedAcquisitionResult, *Citation, error) {
	if e := selectedConversion(selection).validate("producer-carrier"); e != nil {
		return retainedAcquisitionResult{}, nil, e
	}
	if selectedConversion(selection) != nil {
		limits, err := conversionCapabilityLimits(p.ConversionContracts, "producer-carrier", true)
		if err != nil {
			return retainedAcquisitionResult{}, nil, err
		}
		if err = conversionArtifactBound(limits, "responseBytes", len(raw)); err != nil {
			return retainedAcquisitionResult{}, nil, err
		}
		envelope, err := exactMembers(raw)
		if err == nil {
			if _, err = readColumnCarrierLimits(envelope["result"], limits, selection...); err != nil {
				return retainedAcquisitionResult{}, nil, err
			}
		}
	}
	if len(raw) > responseLimit(selectedConversion(selection)) {
		return retainedAcquisitionResult{}, nil, withCode(codeOverBound, errors.New("complete retained-column response exceeds base limit"))
	}
	envelope, e := exactMembers(raw)
	if e == nil && len(envelope["result"]) > artifactLimit(selectedConversion(selection), MaxBody, columnCarrierLimit) {
		return retainedAcquisitionResult{}, nil, withCode(codeOverBound, errors.New("retained-column carrier exceeds selected limit"))
	}
	canonical, cite, e := verifyAcquisition(raw, args, p, public, at, maxAge, selectedConversion(selection))
	if e != nil {
		return retainedAcquisitionResult{}, nil, e
	}
	m, e := exactMembers(raw)
	if e != nil {
		return retainedAcquisitionResult{}, nil, e
	}
	return retainedAcquisitionResult{Canonical: canonical, Raw: append(json.RawMessage(nil), m["result"]...)}, cite, nil
}
