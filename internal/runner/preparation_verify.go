package runner

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// PreparationFixture is an offline verification input. Profiles describe this
// fixture; they are never installed as authority for a live job.
type PreparationFixture struct {
	Dialect, Case, Acquisitions, Profiles, Preparation, Binding []byte
	ExpectedDigest                                              string
}

// PreparationReport follows the Spec §11 report. With no audit record supplied,
// binding is explicitly not-checked, even when the local import binding is valid.
type PreparationReport struct {
	Result             string               `json:"result"`
	Refusal            string               `json:"refusal,omitempty"`
	Finding            string               `json:"finding,omitempty"`
	Digest             string               `json:"digest,omitempty"`
	Binding            string               `json:"binding,omitempty"`
	Facts              json.RawMessage      `json:"facts,omitempty"`
	Evidence           json.RawMessage      `json:"evidence,omitempty"`
	TargetsByClass     *InputClasses        `json:"targetsByClass,omitempty"`
	UnsignedParameters []UnsignedParameters `json:"unsignedParameters,omitempty"`
	UnsignedFreshness  *UnsignedFreshness   `json:"unsignedFreshness,omitempty"`
}

func (r PreparationReport) ExitCode() int {
	if r.Result == "reading" {
		return 0
	}
	if r.Result == "finding" {
		return 2
	}
	return 1
}
func preparationRefusal(code string) PreparationReport {
	return PreparationReport{Result: "refusal", Refusal: code}
}
func preparationFinding(code string) PreparationReport {
	return PreparationReport{Result: "finding", Finding: code}
}

// Preserve HTTP error identity and typed preparation findings together.
type preparationCause struct{ primary, cause error }

func (e preparationCause) Error() string   { return e.primary.Error() }
func (e preparationCause) Unwrap() []error { return []error{e.primary, e.cause} }
func verifyPreparationAcquisition(raw, args []byte, p InputProfile, key ed25519.PublicKey, at time.Time, age int) (json.RawMessage, *Citation, error) {
	result, cite, err := verifyAcquisition(raw, args, p, key, at, age)
	if err != nil {
		var typed researchError
		if errors.As(err, &typed) {
			return result, cite, err
		}
		code := codeInvalid
		if errors.Is(err, errAcquisitionStale) {
			code = codeStale
		}
		err = preparationCause{err, researchFail(code, "acquisition check failed")}
	}
	return result, cite, err
}
func preparationFailure(err error, fallback string) PreparationReport {
	var e researchError
	code := fallback
	if errors.As(err, &e) {
		code = e.code
	}
	switch code {
	case codeMalformed, codeOverBound, "version-unread", "kind-unread":
		return preparationRefusal(code)
	}
	return preparationFinding(code)
}

// VerifyPreparation recomputes with the same preparer used by live execution and
// verify-run. It opens no store and makes no network, model or Runtime call.
func VerifyPreparation(f PreparationFixture) PreparationReport {
	if len(f.Acquisitions) == 0 {
		f.Acquisitions = []byte(`{}`)
	}
	if len(f.Dialect) > 128<<10 || len(f.Profiles) > 64<<10 || len(f.Binding) > 64<<10 || len(f.Preparation) > 8<<20 || len(f.Dialect)+len(f.Case)+len(f.Acquisitions) > MaxBody {
		return preparationRefusal(codeOverBound)
	}
	for _, doc := range []struct {
		raw   []byte
		depth int
	}{{f.Dialect, 64}, {f.Case, 32}, {f.Acquisitions, 64}, {f.Profiles, 64}, {f.Preparation, 64}, {f.Binding, 64}} {
		if verificationBounds(doc.raw, doc.depth) {
			return preparationRefusal(codeOverBound)
		}
	}
	if !validDigest(f.ExpectedDigest) {
		return preparationRefusal(codeMalformed)
	}
	documents := make([]any, 0, 6)
	for _, raw := range [][]byte{f.Dialect, f.Case, f.Acquisitions, f.Profiles, f.Preparation} {
		value, err := readInputJSONDepth(raw, 64)
		if err != nil {
			return preparationRefusal(codeMalformed)
		}
		documents = append(documents, value)
	}
	var bindingDocument any
	if len(f.Binding) > 0 {
		var err error
		bindingDocument, err = readInputJSONDepth(f.Binding, 64)
		if err != nil {
			return preparationRefusal(codeMalformed)
		}
	}
	if preparationCountBounds(documents[0], documents[2], documents[3], bindingDocument) {
		return preparationRefusal(codeOverBound)
	}
	if digest(f.Preparation) != f.ExpectedDigest {
		return preparationFinding("preparation-differs")
	}
	var expected struct {
		VerifiedAt string `json:"verifiedAt"`
	}
	givenObject, isObject := documents[4].(map[string]any)
	if json.Unmarshal(f.Preparation, &expected) != nil || !isObject || givenObject["verifiedAt"] == nil {
		return preparationRefusal(codeMalformed)
	}
	at, ok := receiptInstant(expected.VerifiedAt)
	if !ok || at.Format("2006-01-02T15:04:05Z") != expected.VerifiedAt {
		return preparationRefusal(codeMalformed)
	}
	var m InputMapping
	if strictJSON(f.Dialect, &m) != nil {
		return preparationRefusal(codeMalformed)
	}
	if !m.dialect() {
		return preparationRefusal(codeMalformed)
	}
	if err := exactDialectForm(f.Dialect, m); err != nil {
		return preparationRefusal(codeMalformed)
	}
	var profileMembers []map[string]json.RawMessage
	if json.Unmarshal(f.Profiles, &profileMembers) != nil || profileMembers == nil {
		return preparationRefusal(codeMalformed)
	}
	for _, profile := range profileMembers {
		for _, name := range []string{"id", "publicKey", "class", "source", "authority", "shape", "adapter", "endpoint"} {
			if _, present := profile[name]; !present {
				return preparationRefusal(codeMalformed)
			}
		}
	}
	profiles, err := ParseInputProfiles(f.Profiles)
	if err != nil || profiles == nil || exactPreparationKeys(f.Profiles, reflect.TypeOf(profiles)) != nil {
		return preparationRefusal(codeMalformed)
	}
	var acquisitions map[string]SourceValue
	if strictJSON(f.Acquisitions, &acquisitions) != nil || acquisitions == nil || !exactAcquisitionMembers(f.Acquisitions) {
		return preparationRefusal(codeMalformed)
	}
	var caseValue map[string]json.RawMessage
	if strictJSON(f.Case, &caseValue) != nil || caseValue == nil {
		return preparationRefusal(codeMalformed)
	}
	if err := preparationAcquisitionForms(m, acquisitions); err != nil {
		return preparationFailure(err, codeMalformed)
	}
	for _, source := range m.Sources {
		if source.Kind != "operation" && source.Kind != "research" && !(source.Kind == "selected-file" && source.Provider == "local-file") {
			return preparationRefusal("kind-unread")
		}
	}
	if m.Version == 2 && m.FormatVersion == "" {
		m.originalDocument = append([]byte(nil), f.Dialect...)
	}
	source := &SourceInput{Mapping: m, Case: f.Case, Sources: acquisitions}
	if len(f.Binding) > 0 {
		if err := validateDialect(m, true); err != nil {
			return preparationRefusal(codeMalformed)
		}
		bound, err := bindDialect(DialectImport{Dialect: f.Dialect, Binding: f.Binding}, profiles)
		if err != nil {
			return preparationFailure(err, codeMismatch)
		}
		source.Mapping = bound
	} else if err := m.validate(); err != nil {
		return preparationFailure(err, codeMalformed)
	}
	if err := preflightPreparation(source.Mapping, profiles, false); err != nil {
		return preparationFailure(err, codeMismatch)
	}
	var judged []judgedFreshness
	input, err := prepareV2(Input{Source: source}, profiles, at, false, &judged)
	if err != nil {
		return preparationFailure(err, codeReadFailed)
	}
	given, err := canonical(f.Preparation)
	if err != nil {
		return preparationRefusal(codeMalformed)
	}
	actual, err := canonical(encode(input.Preparation))
	if err != nil {
		return preparationRefusal(codeMalformed)
	}
	if !bytes.Equal(given, actual) {
		return preparationFinding("preparation-differs")
	}
	classes := classesOf(input.Preparation.Lineage)
	if len(input.Facts) == 0 {
		input.Facts = json.RawMessage(`{}`)
	}
	if len(input.Evidence) == 0 {
		input.Evidence = json.RawMessage(`{}`)
	}
	return PreparationReport{Result: "reading", Digest: digest(actual), Binding: "not-checked", Facts: input.Facts, Evidence: input.Evidence, TargetsByClass: &classes, UnsignedParameters: unsignedParameters(source.Mapping, input.Preparation), UnsignedFreshness: unsignedFreshness(input.Preparation, judged)}
}

// Canonical report encoding is shared by CLI and host callers.
func (r PreparationReport) CanonicalBytes() ([]byte, error) { return canonical(encode(r)) }
