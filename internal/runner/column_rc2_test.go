package runner

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const rc2FixtureDirectory = "../retainedcolumns/testdata/rc2"

// This fixed installation exists only in a _test.go file. No request, flag,
// environment variable or host configuration can change production expected pins.
func rc2SyntheticInstallation() ConversionSpec {
	return ConversionSpec{Version: "0.3.0-rc.2", Revision: strings.Repeat("0", 40), SchemaSHA256: "sha256:" + strings.Repeat("1", 64), CorpusSHA256: "sha256:" + strings.Repeat("2", 64)}
}
func rc2Read(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(rc2FixtureDirectory, name))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRC2InstalledIdentityAndSyntheticRefusal(t *testing.T) {
	var identity struct{ ReleaseVersion, SourceCommit string }
	if e := json.Unmarshal(rc2Read(t, "release-identity.json"), &identity); e != nil {
		t.Fatal(e)
	}
	var inventory struct {
		Schema, Corpus, SchemaSHA256, CorpusSHA256 string
		Files                                      map[string]string
	}
	if e := json.Unmarshal(rc2Read(t, "release-inventory.json"), &inventory); e != nil {
		t.Fatal(e)
	}
	want := ConversionSpec{Version: identity.ReleaseVersion, Revision: identity.SourceCommit, SchemaSHA256: inventory.SchemaSHA256, CorpusSHA256: inventory.CorpusSHA256}
	if installedConversionSpec != want {
		t.Fatalf("compiled support differs from published installation: %+v", installedConversionSpec)
	}
	for _, p := range []string{"contract.schema.json", "cases.json", "examples.json", "capabilities.json", "preparations.json", "manifest.json"} {
		source := "conformance/retained-column/1/" + p
		if p == "contract.schema.json" {
			source = inventory.Schema
		}
		if digest(rc2Read(t, p)) != inventory.Files[source] {
			t.Fatalf("published fixture changed: %s", p)
		}
	}
	for _, pin := range []ConversionSpec{rc2SyntheticInstallation(), {Version: "Draft RFC0029", Revision: "ade3914b0b81ef299e5b9a8d6147f37ff3f0976c", SchemaSHA256: "sha256:996cec17473987eebf1aa553536df0ce5a740971532567f411282ee70036879f", CorpusSHA256: "sha256:e2ac53f56cd5ab779e032d2847b49fa04438ffb9df57d456f75f2ba058dc12d2"}} {
		c := selectedContract()
		c.Spec = pin
		body := &unreadBody{}
		req := httptest.NewRequest("POST", "/v1/retained-column/1/inputs/next", body)
		req.Header.Set("Authorization", "Bearer rc2")
		req.Header.Set("JPack-Conversion-Contract", selectedHeader(c))
		w := httptest.NewRecorder()
		(&Service{}).Handler("rc2").ServeHTTP(w, req)
		if w.Code != 422 || body.reads != 0 || !strings.Contains(w.Body.String(), "capability-unsupported") {
			t.Fatalf("noninstalled tuple reached public body owner: %d reads%d %s", w.Code, body.reads, w.Body.String())
		}
	}
}

type rc2PreparationRow struct{ ID, DialectJSON, CaseJSON, AcquisitionsJSON, ProfilesJSON, PreparationJSON, SourceInputJSON, Expected string }
type rc2PreparationCorpus struct {
	TestInstallation ConversionSpec
	Selector         *ConversionContract
	Cases            []rc2PreparationRow
}

func rc2Fixture(t *testing.T, row rc2PreparationRow, c *ConversionContract) (PreparationFixture, error) {
	t.Helper()
	f := PreparationFixture{ConversionContract: c, Dialect: []byte(row.DialectJSON), Case: []byte(row.CaseJSON), Sources: []byte(row.AcquisitionsJSON), Profiles: []byte(row.ProfilesJSON), Preparation: []byte(row.PreparationJSON)}
	var members map[string]json.RawMessage
	if e := json.Unmarshal(f.Case, &members); e == nil && members["retainedCaseJson"] != nil {
		// Adapt the published case-custody fragment through the actual enclosing
		// SourceInput decoder, preserving its exact inner text and union guards.
		raw := `{"mapping":` + row.DialectJSON + `,"conversionContract":` + string(encode(c)) + `,` + strings.TrimSpace(row.CaseJSON)[1:]
		var source SourceInput
		if e := strictJSON([]byte(raw), &source); e != nil {
			return f, e
		}
		b, e := columnCaseBytes(&source)
		if e != nil {
			return f, e
		}
		f.Case = b
	}
	return f, nil
}
func TestRC2PublishedPreparationCorpus(t *testing.T) {
	var corpus rc2PreparationCorpus
	if e := json.Unmarshal(rc2Read(t, "preparations.json"), &corpus); e != nil {
		t.Fatal(e)
	}
	if corpus.TestInstallation != rc2SyntheticInstallation() || len(corpus.Cases) != 27 {
		t.Fatal("fixed synthetic installation or case inventory changed")
	}
	// Sequential test-only substitution, restored before any other top-level
	// test runs. It grants no caller-controlled or production installation path.
	saved := installedConversionSpec
	installedConversionSpec = rc2SyntheticInstallation()
	t.Cleanup(func() { installedConversionSpec = saved })
	for _, row := range corpus.Cases {
		t.Run(row.ID, func(t *testing.T) {
			f, e := rc2Fixture(t, row, corpus.Selector)
			var got string
			if e != nil {
				result, code := verificationOutcome(e)
				got = result + ":" + code
			} else {
				report := verifyPreparationUsing(f, rc2PreparationAcquisition)
				got = report.Result
				if report.Code != "" {
					got += ":" + report.Code
				}
				if got != row.Expected {
					t.Fatalf("published preparation owner: got%s want%s reason%s", got, row.Expected, report.Reason)
				}
			}
			if got != row.Expected {
				t.Fatalf("published custody owner: got%s want%s", got, row.Expected)
			}
		})
	}
}

// These immutable fixtures target Dialect §5.2 preparation consistency, whose
// chain/caller/witness fields belong to the Gateway verifier (§5.2 last paragraph).
// Independently check its five receipt conditions on the ORIGINAL bytes. The
// production VerifyPreparation entry keeps verifyAcquisition, including the full
// Gateway envelope checks; this reader exists only in test code.
func rc2PreparationAcquisition(raw, args []byte, p InputProfile, public ed25519.PublicKey, at time.Time, maxAge int, selection ...*ConversionContract) (json.RawMessage, *Citation, error) {
	fail := func() (json.RawMessage, *Citation, error) {
		return nil, nil, withCode(codeInvalid, errors.New("published preparation-only receipt failed"))
	}
	var envelope struct {
		Result, Receipt json.RawMessage
		Salts           struct{ Args string }
	}
	if strictJSON(raw, &envelope) != nil {
		return fail()
	}
	result, e := scopedJSON(envelope.Result, columnCarrierLimit)
	if e != nil {
		return fail()
	}
	value, e := readInputJSON(envelope.Receipt)
	if e != nil {
		return fail()
	}
	receipt, ok := value.(map[string]any)
	if !ok {
		return fail()
	}
	str := func(k string) string { s, _ := receipt[k].(string); return s }
	if str("receiptVersion") != "3" || str("kind") != "acquisition" || str("keyId") != keyID(p.PublicKey) || str("source") != p.Source || str("authority") != p.Authority || !validHex(str("signature"), 64) {
		return fail()
	}
	session, ok := receipt["sessionId"].(string)
	if !ok {
		return fail()
	}
	idx, ok := receipt["callIndex"].(json.Number)
	if !ok {
		return fail()
	}
	index, e := idx.Int64()
	if e != nil || index < 0 {
		return fail()
	}
	acquisition, ok := receipt["acquisition"].(map[string]any)
	if !ok {
		return fail()
	}
	if _, present := acquisition["pageItems"]; present {
		return fail()
	}
	endpoint := any(nil)
	if p.Endpoint != nil {
		endpoint = *p.Endpoint
	}
	if acquisition["endpoint"] != endpoint || acquisition["shape"] != p.Shape || !sameJSON(encode(acquisition["adapter"]), encode(p.Adapter)) {
		return fail()
	}
	signature, _ := hex.DecodeString(str("signature"))
	resultDigest, argsCommitment, served := str("resultDigest"), str("argumentsCommitment"), str("servedAt")
	delete(receipt, "signature")
	unsigned, e := proofCanon(encode(receipt))
	if e != nil || !ed25519.Verify(public, append([]byte("judgment-pack-gateway/receipt/3:"), unsigned...), signature) || digest(result) != resultDigest || !validHex(envelope.Salts.Args, 32) {
		return fail()
	}
	salt, _ := hex.DecodeString(envelope.Salts.Args)
	canonicalArgs, e := proofCanon(args)
	if e != nil || digest(append(append(salt, []byte("args:")...), canonicalArgs...)) != argsCommitment {
		return fail()
	}
	observed, _ := acquisition["observedAt"].(string)
	if e := acquisitionTimes(observed, served, at, maxAge); e != nil {
		return nil, nil, e
	}
	return result, &Citation{SessionID: session, CallIndex: int(index), Signature: hex.EncodeToString(signature)}, nil
}

func TestRC2ConvertedBasisIsDistinct(t *testing.T) {
	input := columnCase(t, "text", `"yes"`)
	fact := input.Source.Mapping.Case.Facts[0]
	fact.Target = "/second"
	input.Source.Mapping.Case.Facts = append(input.Source.Mapping.Case.Facts, fact)
	// The existing fixture also maps evidence from this same frame.
	prepared, e := normalizeV2(input, nil, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	for _, line := range prepared.Preparation.Lineage {
		if len(line.Basis) != 1 || line.Basis[0] != "/retained" {
			t.Fatalf("shared converted basis is not distinct: %v", line.Basis)
		}
	}
	if report := VerifyPreparation(columnFixture(prepared)); report.Result != ResultReading {
		t.Fatal(report)
	}
}

func TestRC2SelectedCaseContextPrecedesFrameReader(t *testing.T) {
	input := columnCase(t, "text", `"yes"`)
	input.Source.ConversionContract = selectedContract()
	raw := []byte(`{"contractVersion":"1","origin":"generated","values":{"X":"yes"},"research":null}`)
	if _, e := retainedcolumns.ParseFrame(raw, "base"); e == nil {
		t.Fatal("standalone malformed generated frame admitted")
	}
	input.Source.Case = json.RawMessage(`{"retained":` + string(raw) + `}`)
	if _, e := normalizeV2(input, nil, time.Now()); e == nil {
		t.Fatal("generated case admitted")
	} else if _, code := verificationOutcome(e); code != codeReadFailed {
		t.Fatalf("selected generated case refusal order: %s %v", code, e)
	}
	if status, plan, body := scopedEntry(t, input, nil, nil); status < 400 || plan.Input != nil || !strings.Contains(body, "case retained frame must be asserted") {
		t.Fatalf("public selected generated case admitted/wrong guard: %d %s", status, body)
	}
	// Outer duplicate admission remains earlier than source-context refusal.
	input.Source.Case = json.RawMessage(`{"retained":{"contractVersion":"1","origin":"generated","origin":"asserted","values":{"X":"yes"},"research":null}}`)
	if _, e := normalizeV2(input, nil, time.Now()); e == nil {
		t.Fatal("duplicate outer case admitted")
	} else if _, code := verificationOutcome(e); code != codeMalformed {
		t.Fatal("duplicate guard order changed", code, e)
	}
}

func TestRC2ProductionAcquisitionRemainsFull(t *testing.T) {
	var corpus rc2PreparationCorpus
	if e := json.Unmarshal(rc2Read(t, "preparations.json"), &corpus); e != nil {
		t.Fatal(e)
	}
	saved := installedConversionSpec
	installedConversionSpec = rc2SyntheticInstallation()
	t.Cleanup(func() { installedConversionSpec = saved })
	row := corpus.Cases[2]
	if row.ID != "generated-eight-types-response" {
		t.Fatal("fixed fixture changed")
	}
	f, e := rc2Fixture(t, row, corpus.Selector)
	if e != nil {
		t.Fatal(e)
	}
	if report := VerifyPreparation(f); report.Code != codeInvalid {
		t.Fatalf("public owner lost full Gateway authenticity checks: %+v", report)
	}
	if report := verifyPreparationUsing(f, rc2PreparationAcquisition); report.Result != ResultReading {
		t.Fatalf("role-specific consistency fixture failed: %+v", report)
	}
}

// Only the published preparation reader entry is adapted here. SourceInput and
// complete-export rows have distinct enclosing owners; this is not full-chain
// verification of the publisher's preparation-only synthetic exports.
func TestRC2PublishedPreparationReaderBoundaries(t *testing.T) {
	var corpus struct {
		Selector            *ConversionContract
		Cases               []rc2PreparationRow
		ReaderBoundaryCases []map[string]json.RawMessage
	}
	if e := json.Unmarshal(rc2Read(t, "preparations.json"), &corpus); e != nil {
		t.Fatal(e)
	}
	saved := installedConversionSpec
	installedConversionSpec = rc2SyntheticInstallation()
	t.Cleanup(func() { installedConversionSpec = saved })
	checked, excluded := 0, 0
	for _, b := range corpus.ReaderBoundaryCases {
		str := func(k string) string { var s string; json.Unmarshal(b[k], &s); return s }
		if str("entrypoint") != "verify" {
			excluded++
			continue
		}
		checked++
		t.Run(str("id"), func(t *testing.T) {
			var row rc2PreparationRow
			for _, base := range corpus.Cases {
				if base.ID == str("baseCaseId") {
					row = base
				}
			}
			// Patch the literal named input only, preserving every fixture byte string.
			for _, k := range []string{"dialectJson", "caseJson", "acquisitionsJson", "profilesJson", "preparationJson"} {
				if v := b[k]; v != nil {
					switch k {
					case "dialectJson":
						json.Unmarshal(v, &row.DialectJSON)
					case "caseJson":
						json.Unmarshal(v, &row.CaseJSON)
					case "acquisitionsJson":
						json.Unmarshal(v, &row.AcquisitionsJSON)
					case "profilesJson":
						json.Unmarshal(v, &row.ProfilesJSON)
					case "preparationJson":
						json.Unmarshal(v, &row.PreparationJSON)
					}
				}
			}
			c := corpus.Selector
			var e error
			if h := str("selectorHex"); h != "" {
				var bytes []byte
				bytes, e = hex.DecodeString(h)
				if e == nil {
					c, e = ParseConversionSelector(base64.RawURLEncoding.EncodeToString(bytes), "preparation-verification")
				}
			}
			var f PreparationFixture
			if e == nil {
				f, e = rc2Fixture(t, row, c)
			}
			got := ""
			if e != nil {
				result, code := verificationOutcome(e)
				got = result + ":" + code
			} else {
				r := verifyPreparationUsing(f, rc2PreparationAcquisition)
				got = r.Result
				if r.Code != "" {
					got += ":" + r.Code
				}
			}
			if got != str("expected") {
				t.Fatalf("published preparation boundary got%s want%s (%v)", got, str("expected"), e)
			}
		})
	}
	t.Logf("preparation reader boundaries=%d; distinct SourceInput/full-export owners excluded=%d", checked, excluded)
}
