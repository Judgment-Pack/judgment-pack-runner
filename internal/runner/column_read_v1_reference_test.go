package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/retainedcolumns"
)

func mustColumnCaseBytes(c *SourceInput) json.RawMessage {
	raw, e := columnCaseBytes(c)
	if e != nil {
		panic(e)
	}
	return raw
}

// Resolve in the already selected occurrence. No lookup by frame digest is used.
func columnInputJSON(t *testing.T, i Input, index int) *string {
	t.Helper()
	line := i.Preparation.Lineage[index]
	var read CopyMapping
	var raw []byte
	if line.Source == "case" {
		read = CopyMapping{Facts: i.Source.Mapping.Case.Facts, Evidence: i.Source.Mapping.Case.Evidence}
	} else {
		for _, source := range i.Source.Mapping.Sources {
			if source.Name == line.Source {
				read = *source.Read.Copy
				response, e := columnRetainedResponse(i.Source.Sources[source.Name])
				if e != nil {
					t.Fatal(e)
				}
				envelope, e := exactMembers(response)
				if e != nil {
					t.Fatal(e)
				}
				carrier, e := readColumnCarrier(envelope["result"])
				if e != nil {
					t.Fatal(e)
				}
				raw = carrier.RawFrame
			}
		}
	}
	var leaf json.RawMessage
	var pointer string
	for _, f := range read.Facts {
		if line.Kind == "fact" && line.Target == f.Target {
			leaf = f.Conversion
			pointer = f.Source
		}
	}
	for _, f := range read.Evidence {
		if line.Kind == "evidence" && line.Target == f.Requirement {
			leaf = f.Availability
			pointer = f.Source
		}
	}
	if line.Source == "case" {
		var ok bool
		raw, ok = rawPointer(mustColumnCaseBytes(i.Source), pointer)
		if !ok {
			t.Fatal("selected case frame missing")
		}
	}
	p, e := retainedcolumns.ParsePlan(leaf)
	if e != nil {
		t.Fatal(e)
	}
	f, e := retainedcolumns.ParseFrame(raw, retainedcolumns.Base)
	if e != nil {
		t.Fatal(e)
	}
	result, e := retainedcolumns.ResolveInput(p, f, line.ColumnTransformation)
	if e != nil {
		t.Fatal(e)
	}
	return result
}

func columnHTTPExact(t *testing.T, input Input) (int, InputPlan, string) {
	t.Helper()
	raw := encode(input)
	if len(input.Source.Case) > 0 {
		raw = bytes.Replace(raw, encode(input.Source.Case), input.Source.Case, 1)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/inputs/next", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer refs-test")
	recorder := httptest.NewRecorder()
	(&Service{}).Handler("refs-test").ServeHTTP(recorder, req)
	var plan InputPlan
	json.Unmarshal(recorder.Body.Bytes(), &plan)
	return recorder.Code, plan, recorder.Body.String()
}
func TestColumnReferenceCaseExactCustodyAndOccurrence(t *testing.T) {
	original := columnCase(t, "number", `"-0"`)
	raw := []byte("{\n \"provenance\": {\"originalValues\":{\"N\":1.2300,\"Z\":-0}},\n \"retained\": { \"contractVersion\":\"1\", \"origin\":\"asserted\", \"values\":{\"X\":\"-0\",\"Evidence\":\"present\"}, \"research\":null }, \"other\": { \"contractVersion\":\"1\", \"origin\":\"asserted\", \"values\":{\"X\":\"-0\",\"Evidence\":\"present\"}, \"research\":null }\n}")
	original.Source.Case = raw
	original.Source.Mapping.Case.Facts = append(original.Source.Mapping.Case.Facts, FactMapping{Target: "/other", Source: "/other", Conversion: original.Source.Mapping.Case.Facts[0].Conversion})
	status, plan, body := columnHTTPExact(t, original)
	if status != 200 || plan.Input == nil {
		t.Fatalf("exact case entry refused: %d %s", status, body)
	}
	prepared := *plan.Input
	if len(prepared.Source.Case) != 0 || prepared.Source.RetainedCaseJSON != string(raw) {
		t.Fatal("sole exact case custody lost")
	}
	var restored Input
	if e := strictJSON(encode(prepared), &restored); e != nil || !bytes.Equal(mustColumnCaseBytes(restored.Source), raw) {
		t.Fatalf("case whitespace/numeric tokens lost on serialized roundtrip: %v", e)
	}
	frameRaw, _ := rawPointer(raw, "/retained")
	if prepared.Preparation.Lineage[0].ColumnTransformation.Input.Ref.FrameSHA256 != digest(frameRaw) {
		t.Fatal("exact raw frame identity changed")
	}
	if tr := prepared.Preparation.Lineage[0].ColumnTransformation; tr.Input.Ref == nil || tr.Input.Ref.Location != "values" || *columnInputJSON(t, restored, 0) != `"-0"` {
		t.Fatal("raw input reference missing")
	}
	if prepared.Preparation.Lineage[0].ColumnTransformation.Input.Ref.FrameSHA256 != prepared.Preparation.Lineage[1].ColumnTransformation.Input.Ref.FrameSHA256 {
		t.Fatal("handwritten equal frames differ")
	}
	if r := VerifyPreparation(columnFixture(restored)); r.Result != ResultReading {
		t.Fatalf("exact case offline: %+v", r)
	}
	b := columnBundle(restored)
	if v, e := VerifyInputs(encode(b), nil, b.ReleaseDigest); e != nil || !v.Checked() {
		t.Fatalf("exact case export: %v", e)
	}
	b.Release.RuntimeDigest = digest([]byte("reference-execution-runtime"))
	b.Release.Config = "{}"
	b.Release.Lock = "{}"
	payload, e := freezeExecutionPayload(b.Release, restored, signingInputs{})
	if e != nil {
		t.Fatalf("execution/signing source commitment: %v", e)
	}
	replay, e := freezeExecutionPayload(b.Release, prepared, signingInputs{})
	if e != nil || payload.Digest() != replay.Digest() {
		t.Fatal("signing commitment lost exact custody on roundtrip")
	}
	other := cloneInput(restored)
	other.Source.RetainedCaseJSON = " " + other.Source.RetainedCaseJSON
	different, e := freezeExecutionPayload(b.Release, other, signingInputs{})
	if e != nil || payload.Digest() == different.Digest() {
		t.Fatal("signing commitment discarded exact case bytes")
	}

	for _, change := range []string{"wrong-occurrence", "wrong-source", "wrong-column", "wrong-location", "wrong-hash", "missing-reference", "changed-frame-spelling"} {
		t.Run(change, func(t *testing.T) {
			changed := cloneInput(restored)
			line := &changed.Preparation.Lineage[0]
			switch change {
			case "wrong-occurrence":
				line.Candidates = append([]string(nil), changed.Preparation.Lineage[1].Candidates...)
			case "wrong-source":
				line.Source = "other"
			case "wrong-column":
				line.ColumnTransformation.Column = "Evidence"
			case "wrong-location":
				line.ColumnTransformation.Input.Ref.Location = "research-value"
			case "wrong-hash":
				line.ColumnTransformation.Input.Ref.FrameSHA256 = "sha256:" + strings.Repeat("0", 64)
			case "missing-reference":
				line.ColumnTransformation.Input = retainedcolumns.InputState{State: "missing"}
			case "changed-frame-spelling":
				changed.Source.RetainedCaseJSON = strings.Replace(changed.Source.RetainedCaseJSON, `{ "contractVersion"`, `{"contractVersion"`, 1)
			}
			if r := VerifyPreparation(columnFixture(changed)); r.Code != codeDiffers {
				t.Fatalf("reference occurrence mutation trusted: %+v", r)
			}
			b := columnBundle(changed)
			if _, e := VerifyInputs(encode(b), nil, b.ReleaseDigest); e == nil {
				t.Fatal("export trusted substituted reference occurrence")
			}
		})
	}
}
func TestColumnReferenceCaseUnionRefusals(t *testing.T) {
	original := columnCase(t, "text", `"A"`)
	raw := string(original.Source.Case)
	for _, fragment := range []string{`"retainedCaseJson":null`, `"retainedCaseJson":""`, `"RetainedCaseJson":` + string(encode(raw)), `"retainedCaseJson":` + string(encode(raw)) + `,"case":null`, `"retainedCaseJson":` + string(encode(raw)) + `,"case":{}`, `"retainedCaseJson":"[]"`, `"retainedCaseJson":"{\"a\":1,\"\\u0061\":2}"`} {
		var source SourceInput
		if e := strictJSON([]byte(`{"mapping":`+string(encode(original.Source.Mapping))+`,`+fragment+`}`), &source); e == nil {
			t.Fatalf("invalid custody union accepted: %s", fragment)
		}
	}
	for _, change := range []string{"ordinary", "unselected-profile"} {
		t.Run(change, func(t *testing.T) {
			input := columnCase(t, "text", `"A"`)
			retainColumnCase(input.Source, input.Source.Case)
			if change == "ordinary" {
				input.Source.Mapping.Profile = ""
				input.Source.Mapping.Case.Facts[0].Conversion = nil
				input.Source.Mapping.Case.Evidence = []EvidenceMapping{}
				input.Source.Mapping.Case.Facts[0].Source = "/origin/originalValues/X"
				control := cloneInput(input)
				setColumnCase(control.Source, []byte(control.Source.RetainedCaseJSON))
				if _, e := nextInput(control, nil, time.Now()); e != nil {
					t.Fatalf("ordinary absent-profile control invalid: %v", e)
				}
			} else {
				input.Source.Mapping.Profile = ""
			}
			if _, e := nextInput(input, nil, time.Now()); e == nil {
				t.Fatal("unsolicited custody form accepted outside explicit Draft route")
			}
		})
	}
	// Actual offline form checking rejects the removed inline representation.
	status, plan, body := columnHTTPExact(t, original)
	if status != 200 || plan.Input == nil {
		t.Fatalf("prepare: %s", body)
	}
	fixture := columnFixture(*plan.Input)
	var p map[string]any
	json.Unmarshal(fixture.Preparation, &p)
	p["lineage"].([]any)[0].(map[string]any)["columnTransformation"].(map[string]any)["input"] = map[string]any{"state": "present", "json": `"A"`}
	fixture.Preparation = encode(p)
	if r := VerifyPreparation(fixture); r.Result != ResultRefusal || r.Code != codeMalformed {
		t.Fatalf("legacy inline input accepted in new Draft lineage: %+v", r)
	}
}

func TestColumnReferenceTABActualEntries(t *testing.T) {
	// Faithfully constructed pinned Desk-shaped provenance. The prior accounting
	// proof independently exercised the exact pinned pure helpers and old entry.
	tabs := strings.Repeat("\t", 16000)
	input := columnCase(t, "text", `""`)
	input.Source.Mapping.Case = &CaseMapping{Facts: []FactMapping{}, Evidence: []EvidenceMapping{}}
	for n := 0; n < 256; n++ {
		input.Source.Mapping.Case.Facts = append(input.Source.Mapping.Case.Facts, FactMapping{Target: fmt.Sprintf("/f%03d", n), Source: "/retained", Conversion: json.RawMessage(`{"contractVersion":"1","column":"A","kind":"text"}`)})
	}
	frame := []byte(`{"contractVersion":"1","origin":"asserted","values":{"A":` + string(encode(tabs)) + `},"research":null}`)
	input.Source.Case = []byte(`{"provenance":{"originalValues":{"A":` + string(encode(tabs)) + `},"prepared":{},"conversions":"text on each original target"},"retained":` + string(frame) + `}`)
	status, plan, body := columnHTTPExact(t, input)
	if status != 200 || plan.Input == nil || len(plan.Input.Preparation.Lineage) != 256 || string(plan.Input.Facts) != "{}" {
		t.Fatalf("TAB actual preparation refused: %d %s", status, body)
	}
	prepared := *plan.Input
	refs := 0
	for _, line := range prepared.Preparation.Lineage {
		tr := line.ColumnTransformation
		if tr == nil || tr.Input.Ref == nil || tr.Input.Ref.FrameSHA256 != digest(frame) || tr.Input.Ref.Location != "values" || tr.Result.State != "omitted" || tr.Result.Reason != "blank" {
			t.Fatal("TAB ref/blank semantics lost")
		}
		refs += len(encode(tr.Input))
	}
	if raw := columnInputJSON(t, prepared, 0); raw == nil || *raw != string(encode(tabs)) {
		t.Fatal("TAB exact diagnostic input lost")
	}
	if !bytes.Equal(mustColumnCaseBytes(prepared.Source), input.Source.Case) {
		t.Fatal("TAB original provenance changed")
	}
	fixture := columnFixture(prepared)
	if r := VerifyPreparation(fixture); r.Result != ResultReading {
		t.Fatalf("TAB offline refused: %+v", r)
	}
	bundle := columnBundle(prepared)
	export := encode(bundle)
	if v, e := VerifyInputs(export, nil, bundle.ReleaseDigest); e != nil || !v.Checked() || v.Classes.Asserted != 256 {
		t.Fatalf("TAB export refused: %v %+v", e, v)
	}
	if len(fixture.Preparation) >= maxPreparationDocument || len(export) >= 8<<20 {
		t.Fatal("TAB unchanged envelope bound exceeded")
	}
	t.Logf("TAB actual HTTP200/offline/export checked: request=%d exactCase=%d rawFrame=%d preparation=%d export=%d references=%d targets=256 preparationLimit=%d exportLimit=%d", len(encode(input)), len(mustColumnCaseBytes(prepared.Source)), len(frame), len(fixture.Preparation), len(export), refs, maxPreparationDocument, 8<<20)
}

func TestColumnReferenceAutomaticFirstSaveRestart(t *testing.T) {
	s, cfg, o, producer := preparationFixture(t)
	release, e := s.release(o.ReleaseID)
	if e != nil {
		t.Fatal(e)
	}
	mapping := *release.InputMapping
	mapping.Profile = "base"
	mapping.Case.Facts = append(mapping.Case.Facts, FactMapping{Target: "/ref", Source: "/retained", Conversion: json.RawMessage(`{"contractVersion":"1","column":"A","kind":"text"}`)})
	release.InputMapping = &mapping
	if _, e = s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(release)), release.ID); e != nil {
		t.Fatal(e)
	}
	raw := []byte("{\n \"id\":7, \"retained\": { \"contractVersion\":\"1\", \"origin\":\"asserted\", \"values\":{\"A\":\" \\t\"}, \"research\":null }\n}")
	if e = os.WriteFile(filepath.Join(cfg.InputRoot, "case.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	o.PendingInput = &AutomaticInput{Kind: "mapped-sources", Path: "case.json"}
	if e = s.saveOccurrence(o); e != nil {
		t.Fatal(e)
	}
	// Exercise the first real seed/store phase, then model a crash before
	// advancePreparation. A later save in the same invocation can otherwise
	// hide compaction of the first durable checkpoint.
	seed, e := s.automaticInputSeed(o.PendingInput, release, nil)
	if e != nil {
		t.Fatal(e)
	}
	o.State = "waiting"
	o.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: &seed}
	if e = s.saveOccurrence(o); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o, e = s.occurrence(o.ID)
	if e != nil {
		t.Fatal(e)
	}
	if o.State != "waiting" || o.Preparation == nil || o.Preparation.Input == nil || !bytes.Equal(mustColumnCaseBytes(o.Preparation.Input.Source), raw) || len(o.Preparation.Input.Source.Case) != 0 {
		t.Fatal("first automatic durable save changed exact case custody")
	}
	o = stepPreparation(t, s, o.ID)
	operation := o.Preparation.Tasks[0].ID
	o = stepPreparation(t, s, o.ID)
	if o.Preparation.Tasks[0].ID != operation || !bytes.Equal(mustColumnCaseBytes(o.Preparation.Input.Source), raw) {
		t.Fatal("restart changed custody or acquisition identity")
	}
	producer.mu.Lock()
	producer.ready = true
	producer.mu.Unlock()
	o = stepPreparation(t, s, o.ID)
	o = stepPreparation(t, s, o.ID)
	if o.State != "accepted" || o.Input == nil || !bytes.Equal(mustColumnCaseBytes(o.Input.Source), raw) {
		t.Fatalf("continued preparation lost exact case: %+v", o)
	}
	plan, e := s.nextInput(*o.Input, time.Now())
	if e != nil || plan.Input == nil {
		t.Fatalf("continued source replay: %v", e)
	}
	fixture := columnFixture(*plan.Input)
	fixture.Sources = encode(plan.Input.Source.Sources)
	fixture.Profiles = encode(cfg.InputProfiles)
	if r := VerifyPreparation(fixture); r.Result != ResultReading {
		t.Fatalf("continued exact custody offline: %+v", r)
	}
}
func TestColumnReferenceWorkflowCustodySites(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	doc, freeze, runtime := workflowFreezeFixture(t, encode(input.Source.Mapping), nil, nil)
	w, e := newWorkflowInventory(doc, input.Source.Case, "reference-custody", []workflowFreezeInput{freeze}, nil, runtime)
	if e != nil {
		t.Fatal(e)
	}
	w, e = beginWorkflowAttempt(w, "instance-1", nil)
	if e != nil {
		t.Fatal(e)
	}
	original, e := workflowAttemptInput(w, "instance-1")
	if e != nil {
		t.Fatal(e)
	}
	before := cloneInput(original)
	plan, e := nextInput(original, nil, time.Now())
	if e != nil || plan.Input == nil {
		t.Fatalf("workflow prepare: %v", e)
	}
	prepared := *plan.Input
	previous := Run{SchemaVersion: "1", ID: "run_ref", ReleaseID: freeze.Release.ID, State: "interrupted", Input: before}
	current := previous
	current.Input = prepared
	if !workflowRunExtends(previous, current) {
		t.Fatal("workflow checkpoint refused equivalent sole case custody")
	}
	// Prepared references remain immutable even for an equal case object.
	changed := current
	changed.Input = cloneInput(prepared)
	changed.Input.Source.RetainedCaseJSON = strings.Replace(changed.Input.Source.RetainedCaseJSON, `"origin"`, ` "origin"`, 1)
	if workflowRunExtends(current, changed) {
		t.Fatal("workflow checkpoint rewrote retained preparation")
	}
	bundle := verificationExport(freeze.Release, current, 2)
	done, e := finishWorkflowAttempt(w, "instance-1", workflowAttemptResult{Run: encode(bundle), State: "interrupted"}, nil)
	if e != nil {
		t.Fatalf("workflow attempt sole custody getter: %v", e)
	}
	report := workflowReaderWant(t, workflowReaderInput(t, done, nil), ResultReading, "")
	if report.Result != ResultReading {
		t.Fatal("workflow offline custody readers refused")
	}
	// A transient case replacement must clear the alternate slot before replay.
	source := *prepared.Source
	setColumnCase(&source, mustColumnCaseBytes(prepared.Source))
	if source.RetainedCaseJSON != "" {
		t.Fatal("transient case replacement retained dual custody")
	}
	if _, e = nextInput(Input{Source: &source}, nil, time.Now()); e != nil {
		t.Fatalf("transient materialized case replay: %v", e)
	}
}

func TestColumnReferenceCaseEscapingAccountedBeforePreparation(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	input.Source.Case = append([]byte(`{"opaque":`+string(encode(strings.Repeat("\t", 750000)))+`,`), input.Source.Case[1:]...)
	rawCombined := len(encode(input.Source.Mapping)) + len(input.Source.Case) + len(encode(input.Source.Sources))
	retainedCombined := len(encode(input.Source.Mapping)) + columnCaseSlotBytes(input.Source) + len(encode(input.Source.Sources))
	if rawCombined >= MaxBody || retainedCombined <= MaxBody {
		t.Fatal("handwritten escaping witness does not cross the actual bound")
	}
	if e := columnInputBounds(input.Source); e == nil {
		t.Fatal("exact case custody escaping was not counted")
	}
	status, plan, body := columnHTTPExact(t, input)
	if status == 200 || plan.Input != nil || plan.Next != nil || !strings.Contains(body, "complete dialect, case and acquisitions") {
		t.Fatalf("complete escaping was not refused before preparation/acquisition: %d %s", status, body)
	}
	t.Logf("case escaping refusal: rawCombined=%d retainedCombined=%d limit=%d", rawCombined, retainedCombined, MaxBody)
}

func TestColumnReferenceGeneratedSignedOccurrence(t *testing.T) {
	f := newProducerFixture(t, "research-value", "Range")
	status, plan, body := producerHTTP(t, f.input, f.profile)
	if status != 200 || plan.Input == nil {
		t.Fatalf("signed reference preparation: %d %s", status, body)
	}
	prepared := *plan.Input
	tr := prepared.Preparation.Lineage[0].ColumnTransformation
	if tr.Input.Ref == nil || tr.Input.Ref.FrameSHA256 != digest([]byte(f.frame)) || tr.Input.Ref.Location != "research-value" || *columnInputJSON(t, prepared, 0) != `{ "min":-0, "max":2 }` {
		t.Fatal("generated original raw reference lost")
	}
	checkProducerOffline(t, prepared, f.profile)
	for _, change := range []string{"location", "column", "source", "hash", "raw-frame-spelling"} {
		t.Run(change, func(t *testing.T) {
			changed := cloneInput(prepared)
			line := &changed.Preparation.Lineage[0]
			switch change {
			case "location":
				line.ColumnTransformation.Input.Ref.Location = "values"
			case "column":
				line.ColumnTransformation.Column = "List"
			case "source":
				line.Source = "case"
			case "hash":
				line.ColumnTransformation.Input.Ref.FrameSHA256 = "sha256:" + strings.Repeat("0", 64)
			case "raw-frame-spelling":
				value := changed.Source.Sources["desk"]
				value.RetainedResponseJSON = strings.Replace(value.RetainedResponseJSON, `"retained":{ "contractVersion"`, `"retained":{"contractVersion"`, 1)
				changed.Source.Sources["desk"] = value
			}
			if r := VerifyPreparation(producerPreparationFixture(changed, f.profile)); r.Code != codeDiffers {
				t.Fatalf("signed occurrence reference substituted: %+v", r)
			}
			b := columnBundle(changed)
			b.Release.InputProfiles = []InputProfile{f.profile}
			b.ReleaseDigest = releaseDigest(b.Release)
			if _, e := VerifyInputs(encode(b), []InputProfile{f.profile}, b.ReleaseDigest); e == nil {
				t.Fatal("export trusted changed signed occurrence reference")
			}
		})
	}
	// The receipt authenticates canonical content, not arbitrary outer spelling.
	same := cloneInput(prepared)
	value := same.Source.Sources["desk"]
	value.RetainedResponseJSON = "\n" + value.RetainedResponseJSON
	same.Source.Sources["desk"] = value
	checkProducerOffline(t, same, f.profile)
}

func TestColumnReferenceImportedSignedAndAssertedCustody(t *testing.T) {
	f := newProducerFixture(t, "number", "Number")
	mapping := f.input.Source.Mapping
	mapping.Version = 3
	mapping.Case = &CaseMapping{Facts: []FactMapping{{Target: "/asserted", Source: "/asserted", Conversion: json.RawMessage(`{"contractVersion":"1","column":"A","kind":"text"}`)}}, Evidence: []EvidenceMapping{}}
	f.input.Source.Mapping = mapping
	raw := []byte("{\n \"asserted\": { \"contractVersion\":\"1\", \"origin\":\"asserted\", \"values\":{\"A\":\"  A  \"}, \"research\":null }\n}")
	// Sole custody preserves exact bytes even through the producer HTTP helper's encoding.
	retainColumnCase(f.input.Source, raw)
	published := encode(unboundOf(mapping, "desk", "desk-local", "read_reviewed_input", "mcp", f.profile.Adapter.Name))
	binding := bindingOf(t, published, map[string]InputProfile{"desk": f.profile})
	f.input.Source.Import = &DialectImport{Dialect: published, Binding: binding}
	status, plan, body := producerHTTP(t, f.input, f.profile)
	if status != 200 || plan.Input == nil {
		t.Fatalf("imported reference entry: %d %s", status, body)
	}
	prepared := *plan.Input
	if !bytes.Equal(mustColumnCaseBytes(prepared.Source), raw) || prepared.Source.Import == nil || !bytes.Equal(prepared.Source.Import.Dialect, published) || !bytes.Equal(prepared.Source.Import.Binding, binding) {
		t.Fatal("imported original documents or exact case custody changed")
	}
	fixture := producerPreparationFixture(prepared, f.profile)
	fixture.Dialect = published
	fixture.Binding = binding
	if r := VerifyPreparation(fixture); r.Result != ResultReading {
		t.Fatalf("imported offline exact references: %+v", r)
	}
	bundle := columnBundle(prepared)
	bundle.Release.InputProfiles = []InputProfile{f.profile}
	bundle.ReleaseDigest = releaseDigest(bundle.Release)
	v, e := VerifyInputs(encode(bundle), []InputProfile{f.profile}, bundle.ReleaseDigest)
	if e != nil || !v.Checked() || v.Classes.Asserted != 1 || v.Classes.Generated != 2 {
		t.Fatalf("imported export reference replay: %v %+v", e, v)
	}
}

func TestColumnReferenceInitialCaseRequiresExplicitDraft(t *testing.T) {
	initial := columnCase(t, "text", `"A"`)
	initial.Source.Mapping.Profile = ""
	status, plan, body := columnHTTPExact(t, initial)
	if status == 200 || plan.Input != nil || plan.Next != nil || !strings.Contains(body, "explicit Draft base profile") {
		t.Fatalf("initial raw case executed without explicit Draft profile: %d %s", status, body)
	}
	initial.Source.Mapping.Profile = "base"
	status, plan, body = columnHTTPExact(t, initial)
	if status != 200 || plan.Input == nil {
		t.Fatalf("explicit Draft control refused: %d %s", status, body)
	}
	if r := VerifyPreparation(columnFixture(*plan.Input)); r.Result != ResultReading {
		t.Fatalf("explicit Draft output does not replay: %+v", r)
	}
	fixture := columnFixture(*plan.Input)
	mapping := plan.Input.Source.Mapping
	mapping.Profile = ""
	fixture.Dialect = encode(mapping)
	if r := VerifyPreparation(fixture); r.Result != ResultRefusal || r.Code != codeMalformed {
		t.Fatalf("initial unselected profile is not a closed offline refusal: %+v", r)
	}
}

func TestColumnReferenceDraftInitialCaseSpelling(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	for _, imported := range []bool{false, true} {
		source := map[string]any{"mapping": input.Source.Mapping, "Case": json.RawMessage(input.Source.Case)}
		if imported {
			published := input.Source.Mapping
			published.Version = 3
			source["mapping"] = InputMapping{}
			source["import"] = &DialectImport{Dialect: encode(published), Binding: json.RawMessage(`{}`)}
		}
		var parsed SourceInput
		if e := strictJSON(encode(source), &parsed); e == nil {
			t.Fatal("Draft initial Case spelling accepted")
		}
	}
	// The actual Draft input consumer rejects the misspelled initial custody slot.
	source := map[string]any{"mapping": input.Source.Mapping, "Case": json.RawMessage(input.Source.Case)}
	req := httptest.NewRequest(http.MethodPost, "/v1/inputs/next", bytes.NewReader(encode(map[string]any{"source": source})))
	req.Header.Set("Authorization", "Bearer refs-test")
	response := httptest.NewRecorder()
	(&Service{}).Handler("refs-test").ServeHTTP(response, req)
	if response.Code != 400 {
		t.Fatalf("Draft initial Case spelling reached preparation: %d %s", response.Code, response.Body.String())
	}
	// Ordinary case-fold decoding remains the existing legacy behavior.
	input.Source.Mapping.Profile = ""
	input.Source.Mapping.Case.Facts = []FactMapping{{Target: "/value", Source: "/origin/originalValues/X"}}
	input.Source.Mapping.Case.Evidence = []EvidenceMapping{}
	var ordinary SourceInput
	if e := strictJSON(encode(map[string]any{"mapping": input.Source.Mapping, "Case": json.RawMessage(input.Source.Case)}), &ordinary); e != nil {
		t.Fatalf("ordinary absent-member decoding changed: %v", e)
	}
	if _, e := nextInput(Input{Source: &ordinary}, nil, time.Now()); e != nil {
		t.Fatalf("ordinary case-fold control refused: %v", e)
	}
}
