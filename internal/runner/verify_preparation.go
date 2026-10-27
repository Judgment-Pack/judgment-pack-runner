package runner

// The offline preparation verifier (runner #110).
//
// A preparation verifier (spec/dialect.md §11, §14) is given a dialect, a
// case, the acquisitions, the input profiles it trusts and, for a dialect
// with an unbound source, its binding document, and the preparation to
// verify. It recomputes the preparation at the preparation's own instant,
// verifiedAt, and reports a reading, a refusal or a finding. VerifyPreparation
// is that reader, over Runner's own preparation: the checks and the
// recomputation are prepareV2's, the binding is the import step's
// (bindDialect), and the canonical form and the digest are the ones
// TestPreparationVectorsAreByteExact holds every vector to. Nothing here
// prepares otherwise than a run does.
//
// Its command holds the expected preparation to a digest supplied apart from
// the files before anything is read (PreparationPin): a precondition, as a
// file it can open is, and no step of the reading, so a preparation the pin
// does not name is not read and no report is written. The verifier is given
// no record: a reading's binding is therefore always not-checked, and nothing
// here holds a signed record to the preparation or to its instant.
//
// No clock is read: the instant is the given preparation's verifiedAt, held
// to §1's timestamp form with an instant, and nothing else. No network, model
// or Runtime is asked, and nothing is written. The profiles are the ones the
// caller gives as verification inputs, such as an Exchange fixture's, held to
// §5.1 alone and each digested as given (verifierProfiles); they are not an
// installation's configuration, and a reading establishes that the
// preparation recomputes from these inputs, not that any record is true
// (spec/dialect.md §13, §14).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The codes of spec/dialect.md §11 that only the preparation verifier names
// (research.go names the others).
const (
	codeParameter   = "parameter-invalid"
	codeCalculation = "calculation-invalid"
	codeKindUnread  = "kind-unread"
	codeDiffers     = "preparation-differs"
)

// The bounds of spec/dialect.md §12 that are measured on a document's bytes
// before it is read. The dialect's and the binding document's are the import
// step's (dialect_v3.go), and the aggregate is the input bound (MaxBody).
const (
	maxProfilesDocument    = 64 << 10
	maxPreparationDocument = 8 << 20
	maxDocumentDepth       = 64
	maxValueDepth          = 32
)

// The results of a reading (spec/dialect.md §11, the report).
const (
	ResultReading = "reading"
	ResultRefusal = "refusal"
	ResultFinding = "finding"
)

// refusalCodes includes ordinary §11 refusals and the explicitly selected
// retained-column extension's unsupported selection/profile refusals. Every
// other code is a finding's.
var refusalCodes = map[string]bool{"capability-unsupported": true, "profile-unsupported": true, codeOverBound: true, codeMalformed: true, "version-unread": true, codeKindUnread: true}

// verifiedKinds are the source kinds the verifier reads: those the vectors
// hold (spec/dialect.md §4, §14). A dialect that names another kind Runner
// prepares, selected-file, is refused by name, as are the kinds Runner does
// not prepare (refusedKind).
var verifiedKinds = map[string]bool{"operation": true, "research": true}

// codedError is a check's failure with the code §11 gives it. Its message is
// the check's own, unchanged, so that adding the code changes no sentence
// Runner says elsewhere.
type codedError struct {
	code string
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

// withCode is err with the code §11 gives it, or nil.
func withCode(code string, err error) error {
	if err == nil {
		return nil
	}
	return codedError{code, err}
}

// verificationCoded is an error that names its §11 code: a codedError, or a
// research refusal, which names it in its sentence too.
type verificationCoded interface{ verificationCode() string }

func (e codedError) verificationCode() string    { return e.code }
func (e researchError) verificationCode() string { return e.code }

// causedError is an API error that keeps the error it reports, so that a
// verifier can name the code of the check that failed. Its message, status
// and code are the API error's.
type causedError struct {
	*apiError
	cause error
}

func (e causedError) Unwrap() []error { return []error{e.apiError, e.cause} }

// inputProblemOf is inputProblem, keeping cause.
func inputProblemOf(message string, cause error) error {
	return causedError{bad("invalid_source_input", message).(*apiError), cause}
}

// codedProblem is inputProblem, with the code §11 gives it.
func codedProblem(code, message string) error {
	return inputProblemOf(message, codedError{code, errors.New(message)})
}

// verificationOutcome is the result and code §11 gives err: its code, and
// input-malformed for a refusal that names none, which is a form Runner
// refuses.
func verificationOutcome(err error) (result, code string) {
	code = codeMalformed
	var c verificationCoded
	if errors.As(err, &c) {
		code = c.verificationCode()
	}
	if refusalCodes[code] {
		return ResultRefusal, code
	}
	return ResultFinding, code
}

// PreparationFixture is what the verifier reads: each document's bytes as
// read. Binding is nil when no binding document is given, and Sources when no
// acquisitions are, which is the empty object.
type PreparationFixture struct {
	ConversionContract *ConversionContract
	Dialect            []byte
	Binding            []byte
	Case               []byte
	Sources            []byte
	Profiles           []byte
	Preparation        []byte
}

// PreparationPin holds an expected preparation's bytes to the digest supplied
// apart from them: verify-preparation's precondition, as a file it can open
// is, and no step of the reading §11 defines. A preparation the pin does not
// name is not the one its publisher pinned, and is not read. A preparation
// over its bound is not digested: the reading refuses it, input-over-bound,
// without reading it whole.
func PreparationPin(preparation []byte, pin string, selection ...*ConversionContract) error {
	if e := selectedConversion(selection).validate("preparation-verification"); e != nil {
		return e
	}
	if len(preparation) > artifactLimit(selectedConversion(selection), maxPreparationDocument, columnPreparationLimit) {
		return nil
	}
	if got := digest(preparation); got != pin {
		return fmt.Errorf("the preparation's bytes are %s, not %s, the digest supplied with them: it is not the preparation the digest pins, and it is not read", got, pin)
	}
	return nil
}

// PreparationReport is a reading, a refusal or a finding (spec/dialect.md
// §11). A refusal and a finding carry their code and a sentence saying why; a
// reading carries the members §11 lists.
type PreparationReport struct {
	Result string
	Code   string
	Reason string
	// A reading's members.
	Digest             string
	Facts              json.RawMessage
	Evidence           json.RawMessage
	TargetsByClass     InputClasses
	UnsignedParameters []UnsignedParameters
	UnsignedFreshness  *UnsignedFreshness
}

// BindingNotChecked is a reading's binding: verify-preparation is given no
// record, so no record's binding of the preparation is held to it.
const BindingNotChecked = "not-checked"

// Line is the report as §11 writes it: one JSON object in the preparation's
// canonical form, whose result is reading, refusal or finding, with exactly
// the members that result carries. It has no newline.
func (r PreparationReport) Line() []byte {
	report := map[string]any{"result": r.Result}
	switch r.Result {
	case ResultRefusal:
		report["refusal"] = r.Code
	case ResultFinding:
		report["finding"] = r.Code
	default:
		report["digest"] = r.Digest
		report["binding"] = BindingNotChecked
		report["facts"] = r.Facts
		report["evidence"] = r.Evidence
		report["targetsByClass"] = r.TargetsByClass
		if len(r.UnsignedParameters) > 0 {
			report["unsignedParameters"] = r.UnsignedParameters
		}
		if r.UnsignedFreshness != nil {
			report["unsignedFreshness"] = r.UnsignedFreshness
		}
	}
	line, err := canonical(encode(report))
	if err != nil {
		panic(err)
	}
	return line
}

// ExitStatus is the status §11 gives a program's report: 0 for a reading, 1
// for a refusal and 2 for a finding.
func (r PreparationReport) ExitStatus() int {
	switch r.Result {
	case ResultReading:
		return 0
	case ResultRefusal:
		return 1
	}
	return 2
}

// VerifyPreparation reads a fixture as spec/dialect.md §11 reads one, through
// Runner's own preparation, at the instant the given preparation names.
func VerifyPreparation(f PreparationFixture) PreparationReport {
	return verifyPreparationUsing(f, verifyAcquisition)
}

// Public verification always uses the full acquisition reader; the private
// dependency permits exact preparation-only corpus tests with prevalidated inputs.
func verifyPreparationUsing(f PreparationFixture, readAcquisition acquisitionReader) PreparationReport {
	stop := func(code, reason string) PreparationReport {
		result := ResultFinding
		if refusalCodes[code] {
			result = ResultRefusal
		}
		return PreparationReport{Result: result, Code: code, Reason: reason}
	}
	failed := func(err error) PreparationReport {
		result, code := verificationOutcome(err)
		return PreparationReport{Result: result, Code: code, Reason: err.Error()}
	}

	if e := f.ConversionContract.validate("document-validation", "preview", "preparation", "preparation-verification"); e != nil {
		return failed(e)
	}
	prepLimit := artifactLimit(f.ConversionContract, maxPreparationDocument, columnPreparationLimit)
	docLimit := artifactLimit(f.ConversionContract, maxPublishedDialect, columnDocumentLimit)
	allLimit := artifactLimit(f.ConversionContract, MaxBody, columnCombinedLimit)
	profilesLimit := artifactLimit(f.ConversionContract, maxProfilesDocument, columnProfilesLimit)
	if code := columnProfileBytes(f.Dialect); code != "" {
		return stop(code, "retained-column profile is unsupported or malformed")
	}
	sources := f.Sources
	if sources == nil {
		sources = []byte(`{}`)
	}

	// §12's bounds on bytes, on every document before any is read (§11's
	// first phase).
	switch {
	case len(f.Preparation) > prepLimit:
		return stop(codeOverBound, fmt.Sprintf("the preparation is over %d bytes", maxPreparationDocument))
	case len(f.Dialect) > docLimit:
		return stop(codeOverBound, fmt.Sprintf("the dialect is over %d bytes", maxPublishedDialect))
	case len(f.Dialect)+len(f.Case)+len(sources) > allLimit:
		return stop(codeOverBound, fmt.Sprintf("the dialect, the case and the acquisitions are together over %d bytes", MaxBody))
	case len(f.Profiles) > profilesLimit:
		return stop(codeOverBound, fmt.Sprintf("the profiles are over %d bytes", maxProfilesDocument))
	case f.Binding != nil && len(f.Binding) > maxBindingDocument:
		return stop(codeOverBound, fmt.Sprintf("the binding document is over %d bytes", maxBindingDocument))
	}
	type document struct {
		name  string
		bytes []byte
		depth int
	}
	documents := []document{{"the dialect", f.Dialect, maxDocumentDepth}, {"the case", f.Case, maxValueDepth}, {"the acquisitions", sources, maxDocumentDepth}, {"the profiles", f.Profiles, maxDocumentDepth}, {"the preparation", f.Preparation, maxDocumentDepth}}
	if f.Binding != nil {
		documents = append(documents, document{"the binding document", f.Binding, maxDocumentDepth})
	}
	for _, d := range documents {
		if err := measureBytes(d.bytes, d.depth); err != nil {
			return stop(codeOverBound, d.name+": "+err.Error())
		}
	}
	// Each document read as §1 reads one (§11's second phase).
	read := map[string]any{}
	for _, d := range documents {
		v, err := readInputJSONDepth(d.bytes, d.depth)
		if err != nil {
			return stop(codeMalformed, d.name+" is not one JSON document read strictly: "+err.Error())
		}
		read[d.name] = v
	}
	// The nesting of each acquisition's result, which a read applies to, a
	// research record among them (§11's third phase).
	if acquisitions, ok := read["the acquisitions"].(map[string]any); ok {
		for _, name := range sortedKeys(acquisitions) {
			member, _ := acquisitions[name].(map[string]any)
			response, _ := member["response"].(map[string]any)
			if result, ok := response["result"]; ok && valueDepth(result) > maxValueDepth {
				return stop(codeOverBound, fmt.Sprintf("the acquisition %s holds a result nested more than %d levels", name, maxValueDepth))
			}
		}
	}

	if preparationCountBounds(read["the dialect"], read["the acquisitions"], read["the profiles"], read["the binding document"]) {
		return stop(codeOverBound, "the inputs exceed a counted preparation limit")
	}

	// The forms (§11's fifth phase): the profiles, the acquisitions, the
	// preparation's instant and the dialect; the binding document's is the
	// import step's, below.

	// The profiles are held to §5.1 alone, and each is digested as given,
	// its canon bytes (verifierProfiles): Runner's own limits on an
	// installation's profiles are not §5.1's.
	profiles, err := verifierProfiles(read["the profiles"], f.Profiles)
	if err != nil {
		if f.ConversionContract != nil {
			if _, code := verificationOutcome(err); code == "capability-unsupported" {
				return stop(code, "the profiles: "+err.Error())
			}
		}
		return stop(codeMalformed, "the profiles: "+err.Error())
	}
	source := &SourceInput{Case: f.Case, ConversionContract: f.ConversionContract}
	if err = strictJSON(sources, &source.Sources); err != nil || source.Sources == nil || !exactAcquisitionMembers(sources) {
		return stop(codeMalformed, fmt.Sprintf("the acquisitions are not one object of acquisitions by source: %v", err))
	}
	given, _ := read["the preparation"].(map[string]any)
	// Inspect only the reserved selector here. Ordinary preparation members
	// retain their historical canonical recomputation/finding semantics.
	var retainedSelection *ConversionContract
	for name, value := range given {
		if strings.EqualFold(name, "conversionContract") {
			if name != "conversionContract" || value == nil || strictJSON(encode(value), &retainedSelection) != nil {
				return stop(codeMalformed, "preparation conversion selection differs")
			}
		}
	}
	if !sameConversion(retainedSelection, f.ConversionContract) {
		return stop(codeMalformed, "preparation conversion selection differs")
	}
	if err := columnReferenceForms(given); err != nil {
		return stop(codeMalformed, err.Error())
	}
	verifiedAt, _ := given["verifiedAt"].(string)
	at, ok := receiptInstant(verifiedAt)
	if !ok {
		return stop(codeMalformed, "the preparation's verifiedAt is not a timestamp with an instant (YYYY-MM-DDThh:mm:ssZ, a time that exists): it is the only instant the preparation is recomputed at")
	}
	var dialect InputMapping
	if err = strictJSON(f.Dialect, &dialect); err != nil {
		return stop(codeMalformed, "the dialect is not a dialect document: "+err.Error())
	}
	dialect.selectedConversion = f.ConversionContract
	if err = exactDialectForm(f.Dialect, dialect); err != nil {
		return stop(codeMalformed, "the dialect has invalid members")
	}
	if err = preparationAcquisitionForms(dialect, source.Sources); err != nil {
		return failed(err)
	}
	if dialect.Version == 2 && dialect.FormatVersion == "" && f.Binding == nil {
		dialect.originalDocument = append(json.RawMessage(nil), f.Dialect...)
	}
	// A dialect with an unbound source is read with its binding document, as
	// the import step reads one; one with none, as it is given (§4.4). Every
	// source is held to its form, a kind the verifier does not read too, and
	// the binding document to its own, before any kind is refused.
	if err = validateDialectMode(dialect, f.Binding != nil, true); err != nil {
		return failed(err)
	}
	if f.Binding != nil {
		if err = bindingDocumentForm(read["the binding document"], f.Dialect, dialect); err != nil {
			return failed(err)
		}
	}
	// A kind Runner prepares and the verifier does not read (§11's sixth
	// phase), after every form.
	for _, s := range dialect.Sources {
		if !verifiedKinds[s.Kind] {
			return stop(codeKindUnread, fmt.Sprintf("source %s is of kind %s, which verify-preparation does not read: it reads the kinds operation and research", s.Name, s.Kind))
		}
	}
	if f.Binding != nil {
		source.Import = &DialectImport{Dialect: f.Dialect, Binding: f.Binding}
	} else {
		source.Mapping = dialect
	}

	// The recomputation, at the given preparation's instant (§11's
	// findings 1 to 4), and the canonical comparison (finding 5).
	var judged []judgedFreshness
	prepared, err := prepareV2Mode(Input{Source: source}, profiles, at, false, &judged, true, readAcquisition)
	if err != nil {
		return failed(err)
	}
	computed, err := canonical(encode(prepared.Preparation))
	if err != nil {
		panic(err)
	}
	if written, err := canonical(f.Preparation); err != nil || string(written) != string(computed) {
		return stop(codeDiffers, fmt.Sprintf("the preparation recomputed at %s from these inputs is %s, and the given preparation, written in the canonical form, is not it", verifiedAt, digest(computed)))
	}
	evidence := prepared.Evidence
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	return PreparationReport{
		Result:             ResultReading,
		Digest:             digest(computed),
		Facts:              prepared.Facts,
		Evidence:           evidence,
		TargetsByClass:     classesOf(prepared.Preparation.Lineage),
		UnsignedParameters: unsignedParameters(prepared.Source.Mapping, prepared.Preparation),
		UnsignedFreshness:  unsignedFreshness(prepared.Preparation, judged),
	}
}

// PreparationVerifierVersion is the version of verify-preparation's
// interface: its flags, its report and its capabilities. It changes when any
// of them does.
const PreparationVerifierVersion = 2

// PreparationVerifierCapabilities is what verify-preparation --capabilities
// prints, so that a caller can check what the command reads before it calls
// it: a closed object, in the preparation's canonical form, naming the
// command's version and the release it was built from, the dialect versions
// and named formats, source kinds and operation shapes it reads, the instant
// it recomputes at, its report's form, results, codes and exit statuses, and
// the bounds it holds documents to (spec/dialect.md §12). Its preconditions
// are checked before a reading, and one that fails is couldNotRun, with no
// report: the expected preparation's digest (PreparationPin).
func PreparationVerifierCapabilities(release string) []byte {
	shapes := sortedKeys(adapterShapes)
	capabilities := map[string]any{
		"conversionContracts": ConversionCapabilities("preparation-verification"),
		"command":             "verify-preparation",
		"version":             PreparationVerifierVersion,
		"runner":              release,
		"dialectVersions":     []int{2, 3},
		"formatVersions":      []string{"2.0.0", "3.0.0"},
		"sourceKinds":         sortedKeys(verifiedKinds),
		"shapes":              shapes,
		"instant":             freshnessAgainst,
		"report": map[string]any{
			"form": "spec/dialect.md §11",
			// The declared refusal vocabulary extends in interface 2, even
			// though the actual §11 report members and exit statuses do not.
			"version":    2,
			"results":    []string{ResultReading, ResultRefusal, ResultFinding},
			"binding":    []string{BindingNotChecked},
			"refusals":   []string{codeOverBound, codeMalformed, codeKindUnread, "capability-unsupported", "profile-unsupported"},
			"findings":   []string{codeMismatch, codeParameter, codeReadFailed, codeMissing, codeUnexpected, codeInvalid, codeStale, codeSessions, codeCalculation, codeAdmission, codeDiffers},
			"exitStatus": map[string]int{ResultReading: 0, ResultRefusal: 1, ResultFinding: 2, "couldNotRun": 3},
			// Checked before a reading; one that fails is couldNotRun.
			"preconditions": []string{"preparation-digest"},
		},
		"bounds": map[string]int{
			"dialectBytes":      maxPublishedDialect,
			"envelopeBytes":     MaxBody,
			"profilesBytes":     maxProfilesDocument,
			"bindingBytes":      maxBindingDocument,
			"preparationBytes":  maxPreparationDocument,
			"documentDepth":     maxDocumentDepth,
			"valueDepth":        maxValueDepth,
			"numberCharacters":  128,
			"exponent":          308,
			"sources":           16,
			"parameters":        64,
			"clauses":           64,
			"conditions":        64,
			"unwrap":            4,
			"unwrappedBytes":    maxInputDocument,
			"factTargets":       256,
			"evidenceTargets":   128,
			"retainedResponses": maxRetainedResponses,
		},
	}
	line, err := canonical(encode(capabilities))
	if err != nil {
		panic(err)
	}
	return line
}
