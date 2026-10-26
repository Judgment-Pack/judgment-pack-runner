package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func paddedJSON(raw []byte, n int) []byte {
	return append(bytes.Repeat([]byte(" "), n-len(raw)), raw...)
}
func TestColumnRoleOfflineArtifactBoundaries(t *testing.T) {
	original := columnCase(t, "number", `"-0"`)
	original.Source.ConversionContract = selectedContract()
	i, e := normalizeV2(original, nil, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name  string
		max   int
		field func(*PreparationFixture) *[]byte
	}{{"document", 1048576, func(f *PreparationFixture) *[]byte { return &f.Dialect }}, {"preparation", 16777216, func(f *PreparationFixture) *[]byte { return &f.Preparation }}, {"profiles", 81920, func(f *PreparationFixture) *[]byte { return &f.Profiles }}} {
		t.Run(tc.name, func(t *testing.T) {
			f := columnFixture(i)
			f.ConversionContract = selectedContract()
			held := tc.field(&f)
			*held = paddedJSON(*held, tc.max)
			if report := VerifyPreparation(f); report.Result != ResultReading {
				t.Fatalf("exact %s artifact refused: %+v", tc.name, report)
			}
			*held = append([]byte(" "), (*held)...)
			if report := VerifyPreparation(f); report.Code != codeOverBound {
				t.Fatalf("one-over %s did not refuse at byte guard: %+v", tc.name, report)
			}
		})
	}
	f := columnFixture(i)
	f.ConversionContract = selectedContract()
	f.Preparation = paddedJSON(f.Preparation, 16777216)
	if PreparationPin(f.Preparation, digest(f.Preparation), selectedContract()) != nil || PreparationPin(f.Preparation, digest([]byte("other")), selectedContract()) == nil {
		t.Fatal("selected preparation pin skipped digest within selected bound")
	}
	ordinary := columnFixture(i)
	ordinary.ConversionContract = nil
	ordinary.Dialect = paddedJSON(ordinary.Dialect, 131073)
	if report := VerifyPreparation(ordinary); report.Code != codeOverBound {
		t.Fatal("ordinary document cap grew")
	}
}
func TestColumnRoleCarrierResponseCombinedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int
	}{{"carrier", 3145728}, {"response", 5242880}} {
		t.Run(tc.name, func(t *testing.T) {
			f := selectedProducer(t)
			raw := f.input.Source.Sources["desk"].Response
			if tc.name == "carrier" {
				carrier := f.carrier()
				grown := append([]byte("{"), append(bytes.Repeat([]byte(" "), tc.max-len(carrier)), carrier[1:]...)...)
				raw = bytes.Replace(raw, carrier, grown, 1)
			} else {
				raw = append([]byte("{"), append(bytes.Repeat([]byte(" "), tc.max-len(raw)), raw[1:]...)...)
			}
			f.input.Source.Sources["desk"] = SourceValue{RetainedResponseJSON: string(raw)}
			if code, _, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil); code != 200 {
				t.Fatalf("exact %s actual entry refused: %d %.500s", tc.name, code, body)
			}
			if tc.name == "carrier" {
				raw = bytes.Replace(raw, []byte(`"result":{`), []byte(`"result":{ `), 1)
			} else {
				raw = append([]byte("{"), raw...)
				raw[1] = ' '
			}
			f.input.Source.Sources["desk"] = SourceValue{RetainedResponseJSON: string(raw)}
			if _, e := normalizeV2(f.input, []InputProfile{f.profile}, f.at); e == nil {
				t.Fatalf("one-over %s accepted", tc.name)
			}
		})
	}
	f := selectedProducer(t)
	f.input.Source.Case = []byte(`{"opaque":"` + strings.Repeat("a", 4*1048576-len(`{"opaque":""}`)) + `"}`)
	raw := f.input.Source.Sources["desk"].Response
	f.input.Source.Sources["desk"] = SourceValue{RetainedResponseJSON: string(raw)}
	growth := 12582912 - len(encode(f.input.Source))
	tabs := growth / 2
	grown := append([]byte("{"), append(bytes.Repeat([]byte("\t"), tabs), raw[1:]...)...)
	if growth%2 != 0 {
		grown = append(grown, ' ')
	}
	f.input.Source.Sources["desk"] = SourceValue{RetainedResponseJSON: string(grown)}
	if len(encode(f.input.Source)) != 12582912 {
		t.Fatal("combined boundary arithmetic wrong")
	}
	if len(grown) > 5242880 {
		t.Fatal("combined fixture exceeds independent response bound")
	}
	if code, _, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil); code != 200 {
		t.Fatalf("exact combined source actual entry: %d %.500s", code, body)
	}
	f.input.Source.Sources["desk"] = SourceValue{RetainedResponseJSON: string(grown) + " "}
	if _, e := normalizeV2(f.input, []InputProfile{f.profile}, f.at); e == nil {
		t.Fatal("one-over combined source accepted")
	}
}
func TestColumnRoleStoredAndExportBoundaries(t *testing.T) {
	i := columnCase(t, "text", `"A"`)
	i.Source.ConversionContract = selectedContract()
	i, e := normalizeV2(i, nil, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	release := Release{ID: "limits-release", Pack: `{}`, PackDigest: digest([]byte(`{}`)), InputMapping: &i.Source.Mapping, Sample: i}
	run := Run{ID: "limits-run", JobID: "limits-job", ReleaseID: release.ID, State: "queued", Input: i}
	t.Run("release", func(t *testing.T) {
		r := release
		r.Preview = json.RawMessage(`""`)
		n := 41943040 - len(encode(r))
		r.Preview = json.RawMessage(`"` + strings.Repeat("a", n) + `"`)
		if e := newMemoryStore().update(func(tx runTx) error { return tx.createRelease(r) }); e != nil {
			t.Fatal("exact stored release", e)
		}
		r.Preview = append(r.Preview[:len(r.Preview)-1], 'a', '"')
		if e := newMemoryStore().update(func(tx runTx) error { return tx.createRelease(r) }); e == nil {
			t.Fatal("one-over stored release accepted")
		}
	})
	t.Run("run", func(t *testing.T) {
		r := run
		r.Result = json.RawMessage(`""`)
		n := 41943040 - len(runText(r))
		r.Result = json.RawMessage(`"` + strings.Repeat("a", n) + `"`)
		if e := newMemoryStore().update(func(tx runTx) error { return tx.createRun(r, "owner", "key", "digest") }); e != nil {
			t.Fatal("exact stored run", e)
		}
		r.Result = append(r.Result[:len(r.Result)-1], 'a', '"')
		if e := newMemoryStore().update(func(tx runTx) error { return tx.createRun(r, "owner", "key", "digest") }); e == nil {
			t.Fatal("over stored run accepted")
		}
	})
	bundle := VerificationBundle{Version: 2, Release: release, ReleaseDigest: releaseDigest(release), Run: run}
	t.Run("export-base", func(t *testing.T) {
		raw := paddedJSON(encode(bundle), 75497472)
		if v, e := VerifyInputs(raw, nil, bundle.ReleaseDigest, selectedContract()); e != nil || !v.Checked() {
			t.Fatal("exact export base actual reader", e)
		}
		if _, e := VerifyInputs(append([]byte(" "), raw...), nil, bundle.ReleaseDigest, selectedContract()); e == nil {
			t.Fatal("one-over export base accepted")
		}
	})

	t.Run("record", func(t *testing.T) {
		// A handwritten unsigned audit exercises the actual offline record reader.
		// It is not a claim that Runtime emitted a maximum-sized signed record.
		b := bundle
		b.Version = 3
		b.Run.State = "completed"
		disposition := json.RawMessage(`{"kind":"unresolved","reasons":["unknown"]}`)
		b.Run.Result = encode(map[string]any{"disposition": disposition})
		audit := map[string]any{"kind": "evaluation", "pack": map[string]any{"digest": release.PackDigest}, "inputs": map[string]any{"facts": json.RawMessage(`{"value":"A"}`), "evidenceSupplied": true, "evidence": json.RawMessage(`{"receipt":"present"}`)}, "disposition": disposition, "cites": []Citation{}, "binding": json.RawMessage(bindingDocument(preparationDigest(i.Preparation))), "padding": ""}
		size := 9437184 - len(encode(audit))
		audit["padding"] = strings.Repeat("a", size)
		b.Run.AuditBytes = encode(audit)
		b.Run.Audit = append(json.RawMessage(nil), b.Run.AuditBytes...)
		if len(b.Run.AuditBytes) != 9437184 {
			t.Fatal("record boundary arithmetic wrong")
		}
		if v, e := VerifyInputs(encode(b), nil, b.ReleaseDigest, selectedContract()); e != nil || !v.Checked() {
			t.Fatal("exact complete record actual offline reader", e)
		}
		audit["padding"] = strings.Repeat("a", size+1)
		b.Run.AuditBytes = encode(audit)
		b.Run.Audit = append(json.RawMessage(nil), b.Run.AuditBytes...)
		if _, e := VerifyInputs(encode(b), nil, b.ReleaseDigest, selectedContract()); e == nil {
			t.Fatal("one-over record artifact accepted")
		}
	})
	t.Run("wire", func(t *testing.T) {
		if _, e := VerifyInputs(bytes.Repeat([]byte(" "), 90177537), nil, bundle.ReleaseDigest, selectedContract()); e == nil || !strings.Contains(e.Error(), "selected wire limit") {
			t.Fatal("one-over wire did not refuse before decoding", e)
		}
		b := bundle
		b.Version = 5
		b.Run.AuditBytes = bytes.Repeat([]byte("a"), 9437184)
		b.Run.AuditSignatures = bytes.Repeat([]byte("a"), maxSidecar)
		raw := encode(b)
		sidecars := len(auditBytesMember) + (9437184+2)/3*4 + len(auditSignaturesMember) + (maxSidecar+2)/3*4
		raw = paddedJSON(raw, 75497472+sidecars)
		if len(raw) >= 90177536 || exportWithinLimits(raw, b) != nil {
			t.Fatal("coupled maximum base/record/sidecar wire does not fit declared ceiling")
		}
	})
	t.Run("reply", func(t *testing.T) {
		raw := json.RawMessage(`"` + strings.Repeat("a", 37748733) + `"`)
		w := httptest.NewRecorder()
		write(conversionWriter{w, 37748736}, 200, raw)
		if w.Code != 200 || w.Body.Len() != 37748736 {
			t.Fatal("exact selected reply refused or wire length changed")
		}
		w = httptest.NewRecorder()
		raw = append(raw[:len(raw)-1], 'a', '"')
		write(conversionWriter{w, 37748736}, 200, raw)
		if w.Code < 400 {
			t.Fatal("over selected reply accepted")
		}
	})
}

func TestColumnRoleOrdinaryAndCaseLimits(t *testing.T) {
	ordinary := columnCase(t, "text", `"A"`)
	raw := paddedJSON(encode(ordinary), 2097152)
	for _, n := range []int{0, 1} {
		req := httptest.NewRequest("POST", "/v1/inputs/next", bytes.NewReader(append(bytes.Repeat([]byte(" "), n), raw...)))
		req.Header.Set("Authorization", "Bearer ordinary")
		w := httptest.NewRecorder()
		(&Service{}).Handler("ordinary").ServeHTTP(w, req)
		if n == 0 && w.Code != 200 || n == 1 && w.Code != 400 {
			t.Fatalf("ordinary body limit changed: over=%d status=%d", n, w.Code)
		}
	}
	i := columnCase(t, "text", `"A"`)
	i.Source.ConversionContract = selectedContract()
	base := i.Source.Case
	i.Source.Case = append([]byte("{"), append(bytes.Repeat([]byte(" "), 4194560-len(base)), base[1:]...)...)
	if code, _, body := scopedEntry(t, i, nil, nil); code != 200 {
		t.Fatalf("exact retained case bound refused: %d %.500s", code, body)
	}
	i.Source.Case = append([]byte("{"), append([]byte(" "), i.Source.Case[1:]...)...)
	if _, e := normalizeV2(i, nil, time.Now()); e == nil {
		t.Fatal("one-over retained case string accepted")
	}
}

// Build received UTF-8 directly: encoding/json would first expand U+2028/2029
// and therefore test a smaller input. The generated producer is a signed fixture.
func TestColumnRoleLiteralSeparatorWireCustody(t *testing.T) {
	separator := "\u2028\u2029"
	t.Run("maximum-asserted-frame", func(t *testing.T) {
		values := `"X":"A","Evidence":"present"`
		for n := 0; n < 130; n++ {
			values += fmt.Sprintf(`,"f%d":"`, n) + strings.Repeat(separator, 2600) + `"`
		}
		frame := `{"contractVersion":"1","origin":"asserted","values":{` + values + `},"research":null}`
		frame = "{" + strings.Repeat(" ", 2097152-len(frame)) + frame[1:]
		rawCase := []byte(`{"retained":` + frame + `}`)
		i := columnCase(t, "text", `"A"`)
		i.Source.ConversionContract = selectedContract()
		i.Source.Case = rawCase
		code, plan, body := scopedEntry(t, i, nil, nil)
		if code != 200 || plan.Input == nil {
			t.Fatalf("literal maximum frame HTTP preparation: %d %.500s", code, body)
		}
		if !bytes.Equal(mustColumnCaseBytes(plan.Input.Source), rawCase) {
			t.Fatal("literal separator case custody changed")
		}
		if len(body) > 37748736 || !strings.Contains(body, `\u2028\u2029`) {
			t.Fatal("literal separator HTTP wire was not measured after escaping")
		}
		r := Release{ID: "separator-release", Pack: `{}`, PackDigest: digest([]byte(`{}`)), InputMapping: &i.Source.Mapping, Sample: *plan.Input}
		b := VerificationBundle{Version: 2, Release: r, ReleaseDigest: releaseDigest(r), Run: Run{ID: "separator-run", ReleaseID: r.ID, State: "queued", Input: *plan.Input}}
		w := httptest.NewRecorder()
		write(conversionWriter{w, 90177536}, 200, b)
		if w.Code != 200 || exportWithinLimits(w.Body.Bytes(), b) != nil {
			t.Fatal("literal separator complete export wire refused")
		}
		v, e := VerifyInputs(w.Body.Bytes(), nil, b.ReleaseDigest, selectedContract())
		if e != nil || !v.Checked() {
			t.Fatal("literal separator export actual reader", e)
		}
		t.Logf("receivedFrameBytes=%d inputBodyBytes=%d inputReplyBytes=%d exportWireBytes=%d", len(frame), len(encode(i)), len(body), w.Body.Len())
	})
	t.Run("maximum-original-snapshot-valueJson", func(t *testing.T) {
		f := selectedProducer(t)
		literal := strings.Repeat(separator, 80000)
		rawValue := `"` + literal + `"`
		f.snapshot = strings.Replace(f.snapshot, `"value":"  B  "`, `"value":`+rawValue, 1)
		f.frame = strings.Replace(f.frame, `"valueJson":"\"  B  \""`, `"valueJson":"\"`+literal+`\""`, 1)
		f.snapshot += strings.Repeat(" ", 524288-len(f.snapshot))
		f.frame = "{" + strings.Repeat(" ", 2097152-len(f.frame)) + f.frame[1:]
		// The legacy fixture signer has an ordinary 2 MiB guard. Sign this larger
		// selected carrier explicitly using the same canonical receipt algorithm.
		var parts map[string]json.RawMessage
		json.Unmarshal(f.input.Source.Sources["desk"].Response, &parts)
		var receipt map[string]any
		json.Unmarshal(parts["receipt"], &receipt)
		canon, err := derivation.Canon(f.carrier())
		if err != nil {
			t.Fatal(err)
		}
		receipt["resultDigest"] = digest(canon)
		signReceipt(t, receipt, f.key)
		rawResponse := []byte(`{ "result":` + string(f.carrier()) + `, "receipt":` + string(encode(receipt)) + `, "salts":` + string(parts["salts"]) + ` }`)
		f.input.Source.Sources["desk"] = SourceValue{Response: rawResponse}
		code, plan, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil)
		if code != 200 || plan.Input == nil {
			t.Fatalf("literal generated valueJson HTTP preparation: %d %.500s", code, body)
		}
		held := plan.Input.Source.Sources["desk"].RetainedResponseJSON
		carrier, e := readColumnCarrier(f.carrier(), selectedContract())
		if e != nil || string(carrier.RawFrame) != f.frame || string(carrier.OriginalSnapshot) != f.snapshot || !strings.Contains(held, literal) {
			t.Fatal("literal valueJson exact producer custody changed", e)
		}
		fixture := producerPreparationFixture(*plan.Input, f.profile)
		fixture.ConversionContract = selectedContract()
		if report := VerifyPreparation(fixture); report.Result != ResultReading {
			t.Fatalf("literal valueJson offline preparation: %+v", report)
		}
		t.Logf("originalSnapshotBytes=%d receivedFrameBytes=%d carrierBytes=%d inputReplyBytes=%d", len(f.snapshot), len(f.frame), len(f.carrier()), len(body))
	})
}
