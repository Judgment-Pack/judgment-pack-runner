package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func workflowReaderArtifactForm(path string, raw []byte, parsed map[string]any, files map[string][]byte) error {
	if !workflowJSONArtifact(path) {
		return nil
	}
	malformed := func() error { return workflowRefusal(codeMalformed) }
	parts := strings.Split(path, "/")
	var pack []byte
	var published []byte
	checkAcquisitions := false
	if len(parts) > 2 && parts[0] == "instances" {
		for _, value := range workflowReadList(workflowReadObject(parsed["execution.json"])["instances"]) {
			i := workflowReadObject(value)
			if workflowReadString(i["id"]) == parts[1] {
				base := "stages/" + workflowReadString(i["stage"]) + "/"
				pack, published = files[base+"pack.json"], files[base+"dialect.json"]
				if len(parts) >= 5 && parts[2] == "attempts" {
					for _, value := range workflowReadList(i["attempts"]) {
						attempt := workflowReadObject(value)
						if number, ok := attempt["number"].(json.Number); ok && number.String() == parts[3] && attempt["preparationDigest"] != nil {
							checkAcquisitions = true
						}
					}
				}
			}
		}
	}
	switch {
	case strings.HasSuffix(path, "/dialect.json"), strings.HasSuffix(path, "/bound-dialect.json"):
		var dialect InputMapping
		if strictJSON(raw, &dialect) != nil || exactDialectForm(raw, dialect) != nil {
			return malformed()
		}
		if err := validateDialectMode(dialect, true, true); err != nil {
			return malformed()
		}
	case strings.HasSuffix(path, "/binding.json"):
		published := files[strings.TrimSuffix(path, "binding.json")+"dialect.json"]
		if published != nil {
			var dialect InputMapping
			if strictJSON(published, &dialect) != nil || bindingDocumentForm(parsed[path], published, dialect) != nil {
				return malformed()
			}
		}
	case strings.HasSuffix(path, "/profiles.json"):
		if _, err := verifierProfiles(parsed[path], raw); err != nil {
			return malformed()
		}
	case strings.HasSuffix(path, "/release.json"):
		var release Release
		if strictJSON(raw, &release) != nil || !sameJSON(raw, encode(release)) {
			return malformed()
		}
	case strings.HasPrefix(path, "selections/"):
		if _, err := workflowSelectionForm(raw, 4096); err != nil {
			return workflowReferenceError(err)
		}
	case strings.HasPrefix(path, "sets/"):
		var set workflowSet
		if strictJSON(raw, &set) != nil || !sameJSON(raw, encode(set)) || !workflowDigest.MatchString(set.WorkflowDigest) || !workflowID.MatchString(set.Execution) || !workflowID.MatchString(set.Stage) || set.Records == nil {
			return malformed()
		}
		for _, pin := range set.Records {
			if !workflowDigest.MatchString(pin) {
				return malformed()
			}
		}
	case strings.HasPrefix(path, "outputs/") && strings.HasSuffix(path, "/record.json"):
		if _, _, err := readWorkflowOutput(raw); err != nil {
			return workflowReferenceError(err)
		}
	case path == "root-case.json", strings.HasSuffix(path, "/case.json"), strings.HasSuffix(path, "/payload.json"):
		if _, _, err := workflowCase(raw); err != nil {
			return workflowReferenceError(err)
		}
		written, err := canonical(raw)
		if err != nil || !bytes.Equal(raw, written) {
			return malformed()
		}
	case strings.HasSuffix(path, "/preparation.json"):
		written, err := canonical(raw)
		if err != nil || !bytes.Equal(raw, written) {
			return malformed()
		}
		if _, ok := receiptInstant(workflowReadString(workflowReadObject(parsed[path])["verifiedAt"])); !ok {
			return malformed()
		}
	case strings.HasSuffix(path, "/run.json"):
		if _, err := workflowRunForm(raw, pack); err != nil {
			return err
		}
	case strings.HasSuffix(path, "/sources.json"):
		var sources map[string]SourceValue
		if strictJSON(raw, &sources) != nil || sources == nil || !exactAcquisitionMembers(raw) {
			return malformed()
		}
		var dialect InputMapping
		if checkAcquisitions && (published == nil || strictJSON(published, &dialect) == nil) {
			if err := preparationAcquisitionForms(dialect, sources); err != nil {
				return malformed()
			}
		}
	case strings.HasSuffix(path, "/audit.json"):
		if !workflowDispositionForm(member(raw, "disposition"), pack) {
			return malformed()
		}
	case strings.HasSuffix(path, "/evaluation-error.json"):
		if !workflowEvaluationError(raw) {
			return malformed()
		}
	}
	return nil
}

func workflowRunForm(raw, pack []byte) (VerificationBundle, error) {
	var bundle VerificationBundle
	malformed := func() (VerificationBundle, error) { return bundle, workflowRefusal(codeMalformed) }
	if strictJSON(raw, &bundle) != nil || !sameJSON(raw, encode(bundle)) || (bundle.Version != 2 && bundle.Version != 3) || bundle.Run.Input.Source == nil {
		return malformed()
	}
	if bundle.Inputs != "" || bundle.Chain != nil || len(bundle.Run.AuditSignatures) > 0 || checkAuditBytes(bundle) != nil {
		return malformed()
	}
	if len(bundle.Run.Audit) > 0 {
		audit := workflowRunAudit(bundle)
		if _, err := readInputJSONDepth(audit, 64); err != nil || !workflowDispositionForm(member(audit, "disposition"), pack) {
			return malformed()
		}
	}
	if len(bundle.Run.Result) > 0 && (bundle.Run.State == "completed" || len(bundle.Run.Audit) > 0) {
		if !workflowDispositionForm(member(bundle.Run.Result, "disposition"), pack) {
			return malformed()
		}
	}
	return bundle, nil
}

func (r *workflowReader) stageForm(index int, parsed map[string]any) error {
	stage := r.w.Document.Stages[index]
	held := &r.stages[index]
	for _, raw := range [][]byte{held.published, held.bound} {
		if raw == nil {
			continue
		}
		var dialect InputMapping
		if strictJSON(raw, &dialect) != nil || exactDialectForm(raw, dialect) != nil {
			return workflowRefusal(codeMalformed)
		}
		if err := validateDialectMode(dialect, true, true); err != nil {
			result, code := verificationOutcome(err)
			if result == ResultRefusal {
				return workflowRefusal(code)
			}
			return workflowRefusal(codeMalformed)
		}
	}
	if held.published != nil && held.binding != nil {
		var dialect InputMapping
		_ = strictJSON(held.published, &dialect)
		binding, err := readInputJSONDepth(held.binding, 64)
		if err != nil {
			return workflowRefusal(codeMalformed)
		}
		if err := bindingDocumentForm(binding, held.published, dialect); err != nil {
			return workflowRefusal(codeMalformed)
		}
	}
	if held.release != nil {
		if strictJSON(held.release, &held.releaseValue) != nil || !sameJSON(held.release, encode(held.releaseValue)) {
			return workflowRefusal(codeMalformed)
		}
	}
	profiles := r.in.Profiles[stage.ID]
	if profiles == nil {
		profiles = []byte(`[]`)
	}
	value, err := readInputJSONDepth(profiles, 64)
	if err != nil {
		return workflowRefusal(codeMalformed)
	}
	held.profiles, err = verifierProfiles(value, profiles)
	if err != nil {
		return workflowRefusal(codeMalformed)
	}
	if held.recordedProfiles != nil {
		value, err := readInputJSONDepth(held.recordedProfiles, 64)
		if err != nil {
			return workflowRefusal(codeMalformed)
		}
		if _, err := verifierProfiles(value, held.recordedProfiles); err != nil {
			return workflowRefusal(codeMalformed)
		}
	}
	return nil
}

func (r *workflowReader) attemptForm(index, j int, attempt workflowAttempt) error {
	instance := r.w.Execution.Instances[index]
	a := &r.attempts[index][j]
	base := fmt.Sprintf("instances/%s/attempts/%d/", instance.ID, attempt.Number)
	if attempt.RunDigest != nil {
		a.run = r.file(base+"run.json", true)
		if a.run != nil {
			var err error
			a.bundle, err = workflowRunForm(a.run, r.stages[r.w.stageIndex(instance.Stage)].pack)
			if err != nil {
				return err
			}
		}
		r.w.Artifacts["run/"+*attempt.RunDigest] = a.run
		a.sources = r.file(base+"sources.json", true)
		if a.sources != nil {
			var sources map[string]SourceValue
			if strictJSON(a.sources, &sources) != nil || sources == nil || !exactAcquisitionMembers(a.sources) {
				return workflowRefusal(codeMalformed)
			}
		}
	}
	if attempt.PreparationDigest != nil {
		a.preparation = r.file(base+"preparation.json", true)
		r.w.Artifacts["preparation/"+*attempt.PreparationDigest] = a.preparation
		r.file(base+"preparation.sha256", true)
		r.file(base+"verified-at.txt", true)
	}
	if attempt.AuditDigest != nil {
		a.audit = r.file(base+"audit.json", true)
		r.w.Artifacts["audit/"+*attempt.AuditDigest] = a.audit
		a.signatures = r.file(base+"audit-signatures.json", false)
		if a.audit != nil && !workflowDispositionForm(member(a.audit, "disposition"), r.stages[r.w.stageIndex(instance.Stage)].pack) {
			return workflowRefusal(codeMalformed)
		}
	}
	if attempt.EvaluationErrorDigest != nil {
		a.evaluationError = r.file(base+"evaluation-error.json", true)
		r.w.Artifacts["evaluation-error/"+*attempt.EvaluationErrorDigest] = a.evaluationError
		if a.evaluationError != nil && !workflowEvaluationError(a.evaluationError) {
			return workflowRefusal(codeMalformed)
		}
	}
	return nil
}

func (r *workflowReader) preparations() error {
	mismatch := false
	for i, instance := range r.w.Execution.Instances {
		stage := &r.stages[r.w.stageIndex(instance.Stage)]
		for j, attempt := range instance.Attempts {
			if attempt.PreparationDigest == nil {
				continue
			}
			a := &r.attempts[i][j]
			fixture := PreparationFixture{Dialect: stage.published, Binding: stage.binding, Case: r.in.Files["instances/"+instance.ID+"/case.json"], Sources: a.sources, Profiles: r.in.Profiles[instance.Stage], Preparation: a.preparation}
			if fixture.Profiles == nil {
				fixture.Profiles = []byte(`[]`)
			}
			report := VerifyPreparation(fixture)
			if report.Result == ResultRefusal {
				return workflowRefusal(report.Code)
			}
			if report.Result != ResultReading {
				mismatch = true
				continue
			}
			// Recompute with the same offline ordering to retain lineage and whether
			// evidence was absent, which the public report deliberately normalizes.
			var source SourceInput
			source.Case = fixture.Case
			if strictJSON(fixture.Sources, &source.Sources) != nil {
				return workflowRefusal(codeMalformed)
			}
			if fixture.Binding != nil {
				source.Import = &DialectImport{Dialect: fixture.Dialect, Binding: fixture.Binding}
			} else {
				if strictJSON(fixture.Dialect, &source.Mapping) != nil {
					return workflowRefusal(codeMalformed)
				}
				if source.Mapping.Version == 2 && source.Mapping.FormatVersion == "" {
					source.Mapping.originalDocument = append(json.RawMessage(nil), fixture.Dialect...)
				}
			}
			var p Preparation
			if strictJSON(a.preparation, &p) != nil {
				return workflowRefusal(codeMalformed)
			}
			at, ok := receiptInstant(p.VerifiedAt)
			if !ok {
				return workflowRefusal(codeMalformed)
			}
			prepared, err := prepareV2Mode(Input{Source: &source}, stage.profiles, at, false, nil, true)
			if err != nil {
				result, code := verificationOutcome(err)
				if result == ResultRefusal {
					return workflowRefusal(code)
				}
				mismatch = true
				continue
			}
			a.prepared = &prepared
			if !sameJSON(a.bundle.Run.Input.Facts, prepared.Facts) || (len(a.bundle.Run.Input.Evidence) == 0) != (len(prepared.Evidence) == 0) || len(prepared.Evidence) > 0 && !sameJSON(a.bundle.Run.Input.Evidence, prepared.Evidence) {
				mismatch = true
			}
			if len(a.audit) > 0 {
				// Bind the separately retained audit to the checked preparation. It must
				// match even if output extraction later failed or exceeded a bound.
				checked := a.bundle
				checked.Version = 2
				checked.Run.Audit = a.audit
				checked.Run.AuditBytes = nil
				checked.Run.Input = prepared
				binding, err := checkBinding(checked)
				// Pack identity is checked in semantic phase 1. This phase only
				// compares preparation binding, facts, evidence and citations.
				auditRelease := stage.releaseValue
				auditRelease.PackDigest = workflowReadString(workflowReadObject(workflowReadObject(workflowDiscover(a.audit, 8<<20, 64))["pack"])["digest"])
				if err != nil || binding.Status != BindingMatches || !auditRecordsInputs(a.audit, auditRelease, prepared, prepared.Preparation.Cites) {
					mismatch = true
				}
			} else if attempt.State == "completed" || attempt.State == "completed-zero" {
				mismatch = true
			}
		}
	}
	if mismatch {
		return workflowFinding("preparation-mismatch")
	}
	return nil
}
func stringPin(pin *string) string {
	if pin == nil {
		return ""
	}
	return *pin
}
