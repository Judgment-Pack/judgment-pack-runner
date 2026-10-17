package runner

// The private selection writer measures the actual draft §11 recording layout,
// not the deduplicated SQLite blob store. These are in-memory file bytes; this
// helper neither writes paths nor verifies an execution. Attempt files are derived from their retained
// exports and checked against each separately retained pin.
import (
	"bytes"
	"fmt"
)

func (w workflowInventory) recordingFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	put := func(path string, raw []byte) error {
		if len(raw) == 0 {
			return errWorkflowTransition
		}
		if _, exists := files[path]; exists {
			return errWorkflowTransition
		}
		files[path] = raw
		return nil
	}
	retained := func(path, kind, pin string, canonical bool) error {
		raw, exists := w.Artifacts[kind+"/"+pin]
		if !exists {
			return errWorkflowTransition
		}
		var err error
		preimage := raw
		if kind == "release" {
			preimage, err = canonicalRelease(raw)
		} else if canonical {
			preimage, err = proofCanon(raw)
		}
		if err != nil || digest(preimage) != pin {
			return errWorkflowTransition
		}
		if canonical {
			raw = preimage
		}
		return put(path, raw)
	}
	if err := retained("workflow.json", "workflow", w.Execution.WorkflowDigest, false); err != nil {
		return nil, err
	}
	if err := retained("root-case.json", "case", w.Execution.RootCaseDigest, false); err != nil {
		return nil, err
	}
	record, err := workflowCanonical(w.Execution, 8<<20)
	if err != nil {
		return nil, err
	}
	files["execution.json"] = record
	files["execution.sha256"] = []byte(digest(record) + "\n")
	files["workflow.sha256"] = []byte(w.Execution.WorkflowDigest + "\n")
	if len(w.Execution.Frozen) != len(w.Document.Stages) || len(w.Execution.OutputSets) != len(w.Document.Stages) || len(w.Execution.Selections) != len(w.Document.Stages) {
		return nil, errWorkflowTransition
	}
	for i, stage := range w.Execution.Frozen {
		if !workflowID.MatchString(stage.Stage) || stage.Stage != w.Document.Stages[i].ID {
			return nil, errWorkflowTransition
		}
		base := "stages/" + stage.Stage + "/"
		for _, artifact := range []struct {
			name, kind, pin string
			canonical       bool
		}{
			{"dialect.json", "published", stage.PublishedDialectDigest, true},
			{"bound-dialect.json", "bound", stage.BoundDialectDigest, true},
			{"pack.json", "pack", stage.PackDigest, false},
			{"release.json", "release", stage.ReleaseDigest, false},
		} {
			if err := retained(base+artifact.name, artifact.kind, artifact.pin, artifact.canonical); err != nil {
				return nil, err
			}
		}
		if stage.BindingDocumentDigest != nil {
			if err := retained(base+"binding.json", "binding", *stage.BindingDocumentDigest, true); err != nil {
				return nil, err
			}
		}
		var release Release
		if strictJSON(files[base+"release.json"], &release) != nil {
			return nil, errWorkflowTransition
		}
		profileList := release.InputProfiles
		if profileList == nil {
			profileList = []InputProfile{}
		}
		profiles, err := proofCanon(encode(profileList))
		if err != nil {
			return nil, err
		}
		files[base+"profiles.json"] = profiles
		set, err := workflowCanonical(w.outputSet(i), 8<<20)
		if err != nil {
			return nil, err
		}
		files["sets/"+stage.Stage+".json"] = set
		if pin := w.Execution.Selections[i]; pin != nil {
			var selection workflowSelection
			if strictJSON(w.Artifacts["selection/"+*pin], &selection) != nil {
				return nil, errWorkflowTransition
			}
			if err := retained(fmt.Sprintf("selections/%s/%d.json", stage.Stage, selection.Revision), "selection", *pin, false); err != nil {
				return nil, err
			}
		}
		for _, pin := range w.Execution.OutputSets[i].Records {
			output, payload, err := w.record(pin)
			if err != nil || output.Lineage.Stage != stage.Stage || !workflowID.MatchString(output.Lineage.Instance) || output.Lineage.Index < 0 {
				return nil, errWorkflowTransition
			}
			base := fmt.Sprintf("outputs/%s/%s/%d/", stage.Stage, output.Lineage.Instance, output.Lineage.Index)
			if err := retained(base+"record.json", "output", pin, false); err != nil {
				return nil, err
			}
			if err := put(base+"payload.json", payload); err != nil {
				return nil, err
			}
		}
	}
	for _, instance := range w.Execution.Instances {
		if !workflowID.MatchString(instance.ID) {
			return nil, errWorkflowTransition
		}
		if instance.CaseDigest != nil {
			if err := retained("instances/"+instance.ID+"/case.json", "case", *instance.CaseDigest, false); err != nil {
				return nil, err
			}
		}
		for _, attempt := range instance.Attempts {
			base := fmt.Sprintf("instances/%s/attempts/%d/", instance.ID, attempt.Number)
			var run VerificationBundle
			if attempt.RunDigest != nil {
				if err := retained(base+"run.json", "run", *attempt.RunDigest, false); err != nil {
					return nil, err
				}
				if strictJSON(files[base+"run.json"], &run) != nil || run.Run.Input.Source == nil {
					return nil, errWorkflowTransition
				}
				sources := member(files[base+"run.json"], "run", "input", "source", "sources")
				if len(sources) == 0 {
					sources = []byte(`{}`)
				}
				files[base+"sources.json"] = bytes.Clone(sources)
				if len(run.Run.AuditSignatures) > 0 {
					files[base+"audit-signatures.json"] = bytes.Clone(run.Run.AuditSignatures)
				}
			}
			if attempt.AuditDigest != nil {
				if signatures, ok := w.Artifacts[workflowSignatureKey(instance.ID, attempt.Number, *attempt.RunDigest)]; ok {
					if len(signatures) == 0 || len(signatures) > maxSidecar {
						return nil, errWorkflowTransition
					}
					files[base+"audit-signatures.json"] = bytes.Clone(signatures)
				}
			}
			if attempt.PreparationDigest != nil {
				if err := retained(base+"preparation.json", "preparation", *attempt.PreparationDigest, false); err != nil {
					return nil, err
				}
				var preparation Preparation
				if strictJSON(files[base+"preparation.json"], &preparation) != nil || run.Run.Input.Preparation == nil || !sameJSON(encode(run.Run.Input.Preparation), files[base+"preparation.json"]) {
					return nil, errWorkflowTransition
				}
				files[base+"preparation.sha256"] = []byte(*attempt.PreparationDigest + "\n")
				files[base+"verified-at.txt"] = []byte(preparation.VerifiedAt + "\n")
			}
			if attempt.AuditDigest != nil {
				if err := retained(base+"audit.json", "audit", *attempt.AuditDigest, false); err != nil {
					return nil, err
				}
				if !bytes.Equal(files[base+"audit.json"], workflowRunAudit(run)) {
					return nil, errWorkflowTransition
				}
			}
			if attempt.EvaluationErrorDigest != nil {
				if err := retained(base+"evaluation-error.json", "evaluation-error", *attempt.EvaluationErrorDigest, false); err != nil {
					return nil, err
				}
			}
		}
	}
	// Never lend mutable custody bytes to a caller of the eventual exporter.
	for path, raw := range files {
		files[path] = bytes.Clone(raw)
	}
	return files, nil
}
