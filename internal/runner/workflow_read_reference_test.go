package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func workflowReferenceFixture(t *testing.T) workflowReadInput {
	t.Helper()
	w := workflowUnstartedFixture(t)
	var release Release
	if err := strictJSON(w.Artifacts["release/"+w.Execution.Frozen[0].ReleaseDigest], &release); err != nil {
		t.Fatal(err)
	}
	return workflowReaderInput(t, w, release.InputProfiles)
}

func TestWorkflowReaderReferenceGlobalPhases(t *testing.T) {
	in := workflowReferenceFixture(t)
	workflowReaderWant(t, in, ResultReading, "")
	for _, tc := range []struct {
		name, code string
		edit       func(*testing.T, *workflowReadInput)
	}{
		{"branch bytes precede malformed execution", codeOverBound, func(t *testing.T, in *workflowReadInput) {
			doc := decodedJSON(t, in.Files["workflow.json"])
			doc["limits"].(map[string]any)["maxBytes"] = 1
			in.Files["workflow.json"] = encode(doc)
			in.Files["execution.json"] = []byte(`{"executionVersion":`)
		}},
		{"later independent profiles bytes precede earlier dialect form", codeOverBound, func(t *testing.T, in *workflowReadInput) {
			in.Files["stages/companies/dialect.json"] = []byte(`{"version":3,"unknown":true}`)
			in.Profiles["later-stage"] = bytes.Repeat([]byte(" "), maxProfilesDocument+1)
		}},
		{"independent profiles nesting precedes strict syntax", codeOverBound, func(t *testing.T, in *workflowReadInput) {
			in.Files["execution.json"] = []byte(`{`)
			in.Profiles["later-stage"] = []byte(strings.Repeat("[", 65))
		}},
		{"strict syntax precedes independent profiles count", codeMalformed, func(t *testing.T, in *workflowReadInput) {
			in.Files["execution.json"] = []byte(`{`)
			in.Profiles["later-stage"] = encode([]any{map[string]any{"research": map[string]any{"model": strings.Repeat("m", 257)}}})
		}},
		{"integer domain precedes stage count", codeMalformed, func(t *testing.T, in *workflowReadInput) {
			in.Files["root-case.json"] = []byte(`{"number":1.25}`)
			doc := decodedJSON(t, in.Files["workflow.json"])
			stages := doc["stages"].([]any)
			for len(stages) < 33 {
				stages = append(stages, stages[0])
			}
			doc["stages"] = stages
			in.Files["workflow.json"] = encode(doc)
		}},
		{"stage count precedes unknown workflow selector", codeOverBound, func(t *testing.T, in *workflowReadInput) {
			doc := decodedJSON(t, in.Files["workflow.json"])
			stages := doc["stages"].([]any)
			for len(stages) < 33 {
				stages = append(stages, stages[0])
			}
			doc["stages"], doc["workflowVersion"] = stages, 7
			in.Files["workflow.json"] = encode(doc)
		}},
		{"later embedded mapping selector precedes earlier dialect form", "version-unread", func(t *testing.T, in *workflowReadInput) {
			in.Files["stages/companies/dialect.json"] = []byte(`{"version":3,"unknown":true}`)
			for path, raw := range in.Files {
				if strings.HasSuffix(path, "/release.json") {
					v := decodedJSON(t, raw)
					v["inputMapping"].(map[string]any)["version"] = 91
					in.Files[path] = encode(v)
				}
			}
		}},
		{"object selector refused without panic", "version-unread", func(t *testing.T, in *workflowReadInput) {
			doc := decodedJSON(t, in.Files["workflow.json"])
			doc["workflowVersion"] = map[string]any{}
			in.Files["workflow.json"] = encode(doc)
		}},
		{"available workflow form precedes missing execution", codeMalformed, func(t *testing.T, in *workflowReadInput) {
			delete(in.Files, "execution.json")
			doc := decodedJSON(t, in.Files["workflow.json"])
			doc["unknown"] = true
			in.Files["workflow.json"] = encode(doc)
		}},
		{"repinned whitespace root case remains malformed", codeMalformed, func(t *testing.T, in *workflowReadInput) {
			raw := append([]byte(" "), in.Files["root-case.json"]...)
			in.Files["root-case.json"] = raw
			workflowReaderExecution(t, in, func(e *workflowExecution) { e.RootCaseDigest = digest(raw) })
		}},
		{"signature sidecar bytes precede malformed execution", codeOverBound, func(t *testing.T, in *workflowReadInput) {
			in.Files["instances/instance-1/attempts/1/audit-signatures.json"] = bytes.Repeat([]byte("x"), maxSidecar+1)
			in.Files["execution.json"] = []byte(`{`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := workflowReaderCopy(in)
			tc.edit(t, &changed)
			workflowReaderWant(t, changed, ResultRefusal, tc.code)
		})
	}
	// Separate retained branches are not current artifacts, even when their
	// bytes have unsupported selectors or do not parse as JSON.
	changed := workflowReaderCopy(in)
	changed.Files["instances/other-branch/case.json"] = []byte(`{`)
	changed.Files["outputs/companies/other-branch/0/record.json"] = []byte(`{"outputRecordVersion":9}`)
	changed.Files["selections/companies/1.json"] = []byte(`{`)
	workflowReaderWant(t, changed, ResultReading, "")
}

func TestWorkflowReaderReferenceCompletedPhases(t *testing.T) {
	w, profiles := workflowCompanyPeopleRecording(t)
	in := workflowReaderInput(t, w, profiles)
	for _, tc := range []struct {
		name, result, code string
		edit               func(*testing.T, *workflowReadInput)
	}{
		{"missing pack preserves inventory refusal", ResultRefusal, "inventory-mismatch", func(t *testing.T, in *workflowReadInput) {
			delete(in.Files, "stages/companies/pack.json")
		}},
		{"combined preparation bytes precede stage form", ResultRefusal, codeOverBound, func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-1/attempts/1/sources.json"
			in.Files[path] = append(in.Files[path], bytes.Repeat([]byte(" "), MaxBody-len(in.Files[path])-1)...)
			in.Files["stages/people/dialect.json"] = []byte(`{"version":3,"unknown":true}`)
		}},
		{"acquisition value depth precedes missing artifact", ResultRefusal, codeOverBound, func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-1/attempts/1/sources.json"
			v := decodedJSON(t, in.Files[path])
			var nested any = map[string]any{}
			for i := 0; i < 32; i++ {
				nested = []any{nested}
			}
			v["organisations"].(map[string]any)["response"].(map[string]any)["result"] = nested
			in.Files[path] = encode(v)
			delete(in.Files, "stages/people/pack.json")
		}},
		{"review bound precedes selector", ResultRefusal, codeOverBound, func(t *testing.T, in *workflowReadInput) {
			path := "selections/companies/1.json"
			v := decodedJSON(t, in.Files[path])
			v["selectionVersion"] = 4
			v["review"].(map[string]any)["actor"] = strings.Repeat("a", 257)
			in.Files[path] = encode(v)
		}},
		{"review form precedes membership finding", ResultRefusal, codeMalformed, func(t *testing.T, in *workflowReadInput) {
			path := "selections/companies/1.json"
			v := decodedJSON(t, in.Files[path])
			v["review"].(map[string]any)["actor"] = 12
			v["selected"] = []any{digest([]byte("foreign"))}
			in.Files[path] = encode(v)
		}},
		{"split parsed and exact audit refused", ResultRefusal, codeMalformed, func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-1/attempts/1/run.json"
			v := decodedJSON(t, in.Files[path])
			v["run"].(map[string]any)["audit"].(map[string]any)["at"] = "2026-01-01T00:00:00Z"
			in.Files[path] = encode(v)
		}},
		{"malformed result disposition refused", ResultRefusal, codeMalformed, func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-1/attempts/1/run.json"
			v := decodedJSON(t, in.Files[path])
			v["run"].(map[string]any)["result"].(map[string]any)["disposition"] = false
			in.Files[path] = encode(v)
		}},
		{"preparation selector precedes earlier stage form", ResultRefusal, "version-unread", func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-3/attempts/1/preparation.json"
			v := decodedJSON(t, in.Files[path])
			v["version"] = 8
			in.Files[path] = encode(v)
			in.Files["stages/companies/dialect.json"] = []byte(`{"version":3,"unknown":true}`)
		}},
		{"earlier revisions are not parsed", ResultReading, "", func(t *testing.T, in *workflowReadInput) {
			in.Files["selections/companies/2.json"] = []byte(`{"selectionVersion":99}`)
			in.Files["selections/companies/3.json"] = []byte(strings.Repeat("[", 65))
		}},
		{"sole changed selection gives digest finding", ResultFinding, "digest-mismatch", func(t *testing.T, in *workflowReadInput) {
			path := "selections/companies/1.json"
			v := decodedJSON(t, in.Files[path])
			v["review"].(map[string]any)["assertion"] = "Changed current review."
			in.Files[path] = encode(v)
		}},
		{"locator selects changed current among history", ResultFinding, "digest-mismatch", func(t *testing.T, in *workflowReadInput) {
			path := "selections/companies/1.json"
			v := decodedJSON(t, in.Files[path])
			v["review"].(map[string]any)["assertion"] = "Changed current review."
			in.Files[path] = encode(v)
			in.Files["selections/companies/2.json"] = []byte(`{`)
			in.Selections = map[string]string{"companies": path}
		}},
		{"selection discovery uses workflow integer canon", ResultRefusal, codeMalformed, func(t *testing.T, in *workflowReadInput) {
			path := "selections/companies/1.json"
			v := decodedJSON(t, in.Files[path])
			v["revision"] = json.Number("-0")
			in.Files[path] = encode(v)
			v["revision"] = 0
			canonical, err := workflowCanonical(v, 65536)
			if err != nil {
				t.Fatal(err)
			}
			workflowReaderExecution(t, in, func(e *workflowExecution) { e.Selections[0] = workflowPin(digest(canonical)) })
			in.Files["selections/companies/2.json"] = []byte(`{"selectionVersion":99}`)
		}},
		{"repinned whitespace preparation remains malformed", ResultRefusal, codeMalformed, func(t *testing.T, in *workflowReadInput) {
			path := "instances/instance-1/attempts/1/preparation.json"
			raw := append([]byte(" "), in.Files[path]...)
			in.Files[path] = raw
			in.Files["instances/instance-1/attempts/1/preparation.sha256"] = []byte(digest(raw) + "\n")
			workflowReaderExecution(t, in, func(e *workflowExecution) { e.Instances[0].Attempts[0].PreparationDigest = workflowPin(digest(raw)) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := workflowReaderCopy(in)
			tc.edit(t, &changed)
			workflowReaderWant(t, changed, tc.result, tc.code)
		})
	}
	t.Run("multiple signature lines stay opaque and unauthenticated", func(t *testing.T) {
		changed := workflowReaderCopy(in)
		sidecar := []byte(vectorLine1 + "\n" + vectorLine2 + "\n" + vectorLine3 + "\n")
		path := "instances/instance-1/attempts/1/audit-signatures.json"
		changed.Files[path] = sidecar
		r := workflowReader{in: changed, used: map[string]bool{}, ignored: map[string]bool{}}
		if err := r.reference(); err != nil || !bytes.Equal(r.attempts[0][0].signatures, sidecar) {
			t.Fatal("signature sidecar bytes were not retained exactly", err)
		}
		report := workflowReaderWant(t, changed, ResultReading, "")
		if !bytes.Contains(report.line(), []byte(`"authentication":"not-established"`)) {
			t.Fatal("opaque signature bytes must not imply authentication", string(report.line()))
		}
	})
}

func TestWorkflowDispositionFormCanonicalArrays(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"kind":"unresolved","reasons":["conflict","unknown"],"handoff":{"state":"requested","triggeredBy":["conflict","unknown"]}}`, true},
		{`{"kind":"unresolved","reasons":["unknown","conflict"],"handoff":{"state":"none"}}`, false},
		{`{"kind":"unresolved","reasons":["conflict","unknown"],"handoff":{"state":"requested","triggeredBy":["unknown","conflict"]}}`, false},
		{`{"kind":"outcome","outcomeId":"yes","reasons":[],"handoff":{"state":"none"}}`, true},
		{`{"kind":"outcome","outcomeId":"foreign","reasons":[],"handoff":{"state":"none"}}`, false},
	} {
		if got := workflowDispositionForm([]byte(tc.raw), []byte(`{"outcomes":[{"id":"yes"}]}`)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}
