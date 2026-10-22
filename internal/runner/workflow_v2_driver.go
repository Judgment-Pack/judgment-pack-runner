package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// The host explicitly commits starts before calling run. This driver does not
// begin/retry/select, choose models, grant permission, or run on startup.
type workflowV2Driver struct {
	Trace    func(workflowV2Comparison)
	Store    *workflowV2Store
	Profiles []InputProfile
	Now      func() time.Time
	Acquire  func(context.Context, workflowAcquisition) (SourceValue, error)
	Evaluate func(context.Context, Release, Input, string) (workflowEvaluation, error)
}
type workflowV2SourceCustody struct {
	Name               string
	Response, Snapshot []byte
}
type workflowV2EvaluationCustody struct {
	Run        []byte
	Evaluation workflowEvaluation
}

// On a storage failure these owned bytes remain available to the caller. This
// cannot promise crash-safe custody when the installation's storage itself fails.
type workflowV2CustodyError struct {
	Err  error
	Kind string
	Body []byte
}

func (e *workflowV2CustodyError) Error() string {
	return "workflow custody write failed: " + e.Err.Error()
}
func (e *workflowV2CustodyError) Unwrap() error { return e.Err }
func (d workflowV2Driver) stash(execution, branch, instance string, number int, kind string, body []byte) error {
	if err := d.Store.appendCustody(execution, branch, instance, number, kind, body); err != nil {
		return &workflowV2CustodyError{err, kind, bytes.Clone(body)}
	}
	return nil
}

// receive accepts out-of-order custody. drain admits only the next canonical
// result, never charging later material to an earlier candidate.
func (d workflowV2Driver) receive(execution, branch string, result workflowV2Result) (workflowV2Inventory, error) {
	if err := d.stash(execution, branch, result.Instance, result.Number, "result", encode(result)); err != nil {
		held, _, _ := d.Store.load(execution, branch)
		return held, err
	}
	return d.drain(execution, branch)
}
func (d workflowV2Driver) drain(execution, branch string) (workflowV2Inventory, error) {
	for {
		w, revision, err := d.Store.load(execution, branch)
		if err != nil {
			return w, err
		}
		w.Trace = d.Trace
		if w.Proof.closed() {
			return w, nil
		}
		e, _ := w.Proof.last()
		next := e.begins()[len(e.Results)]
		held, err := d.Store.custody(execution, branch)
		if err != nil {
			return w, err
		}
		var result *workflowV2Result
		for _, c := range held {
			if c.Instance == next.Instance && c.Number == next.Number && c.Kind == "result" {
				var r workflowV2Result
				if json.Unmarshal(c.Body, &r) != nil || r.Instance != c.Instance || r.Number != c.Number {
					return w, errWorkflowTransition
				}
				result = &r
				break
			}
		}
		if result == nil {
			return w, nil
		}
		updated, err := observeWorkflowV2(w, *result, d.Profiles)
		if err != nil {
			return w, err
		}
		if err = d.Store.save(branch, updated, revision); err != nil {
			return w, err
		}
	}
}
func (d workflowV2Driver) run(ctx context.Context, execution, branch string) (workflowV2Inventory, error) {
	host := workflowDriver{Profiles: d.Profiles, Now: d.Now, Acquire: d.Acquire, Evaluate: d.Evaluate}
	for {
		w, err := d.drain(execution, branch)
		if err != nil || w.Proof.closed() {
			return w, err
		}
		epoch, _ := w.Proof.last()
		next := epoch.begins()[len(epoch.Results)]
		held, err := d.Store.custody(execution, branch)
		if err != nil {
			return w, err
		}
		for _, c := range held {
			if c.Instance == next.Instance && c.Number == next.Number {
				return w, errWorkflowTransition
			}
		} // recovery, never reacquisition
		if w.Cutoffs[w.History[next.Instance].Stage] {
			if _, err = d.receive(execution, branch, workflowV2Result{next.Instance, next.Number, workflowAttemptResult{State: "suppressed"}}); err != nil {
				return w, err
			}
			continue
		}
		if err = d.Store.claimDrive(execution, branch, next.Instance, next.Number); err != nil {
			return w, err
		}
		var checkpoint *Run
		err = host.driveAdmitted(ctx, w.Core, next.Instance,
			func(raw []byte) error {
				if err := d.stash(execution, branch, next.Instance, next.Number, "checkpoint", raw); err != nil {
					return err
				}
				var b VerificationBundle
				if strictJSON(raw, &b) != nil || b.Run.State != "interrupted" || !workflowCheckpointIdentity(w.Core, next.Instance, b) {
					return errWorkflowTransition
				}
				if checkpoint != nil && !workflowRunExtends(*checkpoint, b.Run) {
					return errWorkflowTransition
				}
				r := b.Run
				checkpoint = &r
				return nil
			}, func(result workflowAttemptResult) error {
				_, err := d.receive(execution, branch, workflowV2Result{next.Instance, next.Number, result})
				return err
			}, func(request workflowAcquisition, value SourceValue) error {
				raw := encode(workflowV2SourceCustody{request.Next.Name, bytes.Clone(value.Response), bytes.Clone(value.Snapshot)})
				return d.stash(execution, branch, next.Instance, next.Number, "source", raw)
			}, func(run Run, evaluated workflowEvaluation, _ error) error {
				run.FinishedAt = host.now().Format("2006-01-02T15:04:05Z")
				var release Release
				frozen := w.Core.Execution.Frozen[w.Core.stageIndex(w.History[next.Instance].Stage)]
				if strictJSON(w.Core.Artifacts["release/"+frozen.ReleaseDigest], &release) != nil {
					return errWorkflowTransition
				}
				body := encode(workflowV2EvaluationCustody{encode(verificationExport(release, run, 3)), evaluated})
				return d.stash(execution, branch, next.Instance, next.Number, "evaluation", body)
			}, true)
		if err != nil {
			// A final-result drain may commit a prefix before a later held result fails.
			held, _, loadErr := d.Store.load(execution, branch)
			if loadErr == nil {
				return held, err
			}
			return w, err
		}
	}
}

// recover is an explicit exclusive-host operation. It processes received final
// results, otherwise retains available inputs as interrupted. No callbacks run.
func (d workflowV2Driver) recover(execution, branch string) (workflowV2Inventory, error) {
	for {
		w, err := d.drain(execution, branch)
		if err != nil || w.Proof.closed() {
			return w, err
		}
		epoch, _ := w.Proof.last()
		next := epoch.begins()[len(epoch.Results)]
		held, err := d.Store.custody(execution, branch)
		if err != nil {
			return w, err
		}
		var latest []byte
		var additions []workflowV2SourceCustody
		var evaluated *workflowV2EvaluationCustody
		for _, c := range held {
			if c.Instance == next.Instance && c.Number == next.Number {
				switch c.Kind {
				case "evaluation":
					var value workflowV2EvaluationCustody
					if json.Unmarshal(c.Body, &value) != nil {
						return w, errWorkflowTransition
					}
					evaluated = &value
				case "checkpoint":
					latest = bytes.Clone(c.Body)
					additions = nil
				case "source":
					var source workflowV2SourceCustody
					if json.Unmarshal(c.Body, &source) != nil {
						return w, errWorkflowTransition
					}
					additions = append(additions, source)
				}
			}
		}
		if evaluated != nil {
			value := evaluated.Evaluation
			if len(value.Signatures) > 0 && len(value.Audit) == 0 {
				return w, errWorkflowTransition
			}
			if len(value.ErrorEnvelope) > 0 && (len(value.Result) > 0 || len(value.Audit) > 0) {
				return w, errWorkflowTransition
			}
			if len(value.Audit) > 0 || workflowEvaluationError(value.ErrorEnvelope) {
				var bundle VerificationBundle
				if strictJSON(evaluated.Run, &bundle) != nil {
					return w, errWorkflowTransition
				}
				state, failure := "completed", ""
				if len(value.ErrorEnvelope) > 0 {
					state, failure = "failed", "evaluation-error"
				} else {
					bundle.Run.Result = value.Result
					bundle.Run.retainAudit(value.Audit, value.Signatures)
				}
				bundle.Run.State = state
				result := workflowV2Result{next.Instance, next.Number, workflowAttemptResult{Run: encode(verificationExport(bundle.Release, bundle.Run, 3)), Signatures: value.Signatures, EvaluationError: value.ErrorEnvelope, State: state, Failure: failure}}
				if updated, receiveErr := d.receive(execution, branch, result); receiveErr != nil {
					return updated, receiveErr
				}
				continue
			}
		}
		if len(additions) > 0 {
			var b VerificationBundle
			if len(latest) == 0 || strictJSON(latest, &b) != nil || b.Run.Input.Source == nil {
				return w, errWorkflowV2Recording
			}
			for _, source := range additions {
				if len(source.Response) > 0 && !json.Valid(source.Response) || len(source.Snapshot) > 0 && !json.Valid(source.Snapshot) {
					return w, errWorkflowV2Recording
				}
				if old, exists := b.Run.Input.Source.Sources[source.Name]; exists && !bytes.Equal(encode(old), encode(SourceValue{Response: source.Response, Snapshot: source.Snapshot})) {
					return w, errWorkflowTransition
				}
				b.Run.Input.Source.Sources[source.Name] = SourceValue{Response: source.Response, Snapshot: source.Snapshot}
			}
			latest = encode(b)
		}
		result := workflowV2Result{next.Instance, next.Number, workflowAttemptResult{Run: latest, State: "interrupted"}}
		if updated, receiveErr := d.receive(execution, branch, result); receiveErr != nil {
			return updated, receiveErr
		}
	}
}

// A fork saves a fresh private branch head and never overwrites the parent.
func (s *workflowV2Store) fork(execution, parent, branch string, revision int, raw []byte, childIDs []string, traces ...func(workflowV2Comparison)) (workflowV2Inventory, error) {
	w, heldRevision, err := s.load(execution, parent)
	if err != nil {
		return w, err
	}
	if heldRevision != revision {
		return w, errWorkflowRevision
	}
	if len(traces) > 1 {
		return w, errWorkflowForm
	}
	if len(traces) == 1 {
		w.Trace = traces[0]
	}
	next, err := selectWorkflowV2(w, raw, childIDs, true)
	if err != nil {
		return w, err
	}
	if !workflowID.MatchString(branch) || branch == parent {
		return w, errWorkflowForm
	}
	tx, err := s.db.Begin()
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	var current int
	if err = tx.QueryRow(`SELECT revision FROM workflow_v2_branches WHERE execution=? AND branch=?`, execution, parent).Scan(&current); err != nil {
		return w, err
	}
	if current != revision {
		return w, errWorkflowRevision
	}
	if _, err = tx.Exec(`INSERT INTO workflow_v2_branches(execution,branch,revision,body) VALUES(?,?,1,?)`, execution, branch, workflowV2StoreBody(next)); err != nil {
		return w, err
	}
	if err = workflowV2StoreArtifacts(tx, branch, next); err != nil {
		return w, err
	}
	if err = tx.Commit(); err != nil {
		return w, err
	}
	return next, nil
}

func (s *Service) workflowV2Driver(store *workflowV2Store) workflowV2Driver {
	host := s.workflowDriver(nil)
	return workflowV2Driver{Store: store, Profiles: host.Profiles, Now: host.Now, Acquire: host.Acquire, Evaluate: host.Evaluate}
}
