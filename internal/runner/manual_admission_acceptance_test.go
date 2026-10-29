//go:build linux || darwin || freebsd

package runner

// The acceptance cases of runner #147 for governed manual runs: each a test
// named for its case, against the real Runtime, with the transport peer of
// manual_admission_test_authority_test.go standing in for the anchored
// authority (it is not Desk Policy authority; combined tests use the real
// Desk core). The vocabulary of a fence is the agent's ADR-0025 lease
// contract, which the Desk's store locks share: an owner, an authoritative
// expiry kept by the store clock, an epoch (a canonical base-10 string, never
// a JSON number), and the fence {resource, owner, epoch}. Here the resource is
// the registered job/release tuple, the owner the executor the authority
// activated, and the epoch the one its activation returned.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// admissionFence is a fence as ADR-0025 names its members.
type admissionFence struct {
	Resource ManualResource
	Owner    string
	Epoch    string
}

// governedHarness is one governed Service with an owned executor lease, a
// prepared journal, matching demand and an activated authority channel.
type governedHarness struct {
	t       *testing.T
	s       *Service
	cfg     Config
	job     Job
	methods []string
	runs    map[string]string // intent id -> run id
}

type authorityAnswer func(h *governedHarness, method string, body json.RawMessage) (string, any, bool)

func newGovernedHarness(t *testing.T, answer authorityAnswer) *governedHarness {
	t.Helper()
	cfg := testConfig(t)
	cfg.disableAutomation = true
	cfg.disableDispatcher = true
	s, err := Open(cfg)
	executorMust(t, err)
	t.Cleanup(func() { s.Close() })
	release := testRelease(t, s)
	job, err := s.createJob("governed", release.ID)
	executorMust(t, err)
	f := executorFixture(t)
	old := f.leaf()
	f.identity.Workspace = strings.Repeat("c", 64)
	executorMust(t, os.Rename(old, f.leaf()))
	f.state = cfg.Dir
	lease, err := f.acquire()
	executorMust(t, err)
	s.executor = lease
	o := ExecutorOwnership{Root: f.root, MachineWorkspace: f.identity.Workspace, RunnerWorkspace: cfg.Workspace, RootDevice: f.identity.RootDevice, RootInode: f.identity.RootInode, FileDevice: f.identity.FileDevice, FileInode: f.identity.FileInode}
	s.cfg.ExecutorOwnership = &o
	_, err = s.db.Exec(manualJournalSchema)
	executorMust(t, err)
	h := &governedHarness{t: t, s: s, cfg: cfg, job: job, runs: map[string]string{}}
	h.connect(answer)
	executorMust(t, os.Mkdir(manualDemandDirectory(o), 0700))
	executorMust(t, os.WriteFile(filepath.Join(manualDemandDirectory(o), o.MachineWorkspace), encode(s.cfg.ExecutorAdmission.demand()), 0600))
	return h
}

// connect opens and activates a fresh authority channel, as a restarted
// Runner does: every method the peer receives is recorded.
func (h *governedHarness) connect(answer authorityAnswer) {
	h.t.Helper()
	a, b := net.Pipe()
	manualGateAuthorityConnections(h.t, h.s, a, b, func(method string, body json.RawMessage) (string, any, bool) {
		h.methods = append(h.methods, method)
		if answer == nil {
			return "", nil, false
		}
		return answer(h, method, body)
	})
}

// submit queues a governed run for a fresh intent, or returns the refusal.
func (h *governedHarness) submit(intentID string) (Run, error) {
	h.t.Helper()
	raw := []byte(`{"facts":` + string(sample().Facts) + `,"evidence":` + string(sample().Evidence) + `}`)
	intent, err := captureManualIntent(intentID, "key-"+intentID, raw)
	executorMust(h.t, err)
	run, _, err := h.s.submitInternalIntent(h.job.ID, "key-"+intentID, intent.input(), nil, intent)
	if err == nil {
		h.runs[intentID] = run.ID
	}
	return run, err
}

// dispatch runs the oldest queued run and returns it as it ended.
func (h *governedHarness) dispatch(id string) Run {
	h.t.Helper()
	worked, err := h.s.dispatchNext(context.Background())
	executorMust(h.t, err)
	if !worked {
		h.t.Fatal("no queued governed run was handled")
	}
	run, err := h.s.run(id)
	executorMust(h.t, err)
	return run
}

// launched says whether the run reached the Runtime: an evaluation that
// started leaves its operational trail.
func (h *governedHarness) launched(run Run) bool {
	_, err := os.Stat(filepath.Join(h.cfg.Dir, "attempts", run.ID, "audit", "evaluations.jsonl"))
	return err == nil || len(run.Audit) > 0
}

func (h *governedHarness) requireNoLaunch(run Run) {
	h.t.Helper()
	if run.State != "failed" || h.launched(run) {
		h.t.Fatalf("an unadmitted run reached the Runtime: %s %s", run.State, run.Problem)
	}
}

func (h *governedHarness) requireCompleted(run Run) {
	h.t.Helper()
	if run.State != "completed" || !h.launched(run) {
		h.t.Fatalf("an admitted run did not complete: %s %s", run.State, run.Problem)
	}
}

func (h *governedHarness) count(method string) int {
	n := 0
	for _, m := range h.methods {
		if m == method {
			n++
		}
	}
	return n
}

// journal is the retained local attempt record of a run, or "".
func (h *governedHarness) journal(runID string) string {
	var record string
	if err := h.s.db.QueryRow("SELECT record FROM manual_attempts WHERE run_id=?", runID).Scan(&record); err != nil {
		return ""
	}
	return record
}

func refusal(code string) (string, any, bool) {
	return "refused", ManualAuthorityRefusal{Code: code}, false
}

func startDecision(body json.RawMessage, decision, reason string) (string, any, bool) {
	var q ManualStartRequest
	_ = json.Unmarshal(body, &q)
	d, _ := manualRequestDigest("commitStart", body)
	return "ok", ManualStartResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: q.Epoch, RequestDigest: d, Decision: decision, Reason: reason, At: "2026-10-19T12:00:00Z", AuthorityRevision: strings.Repeat("b", 64)}, false
}

// Omitted-dependency substitution. The closure a governed run executes is the
// registered release's: its Pack, config, lock and pinned Runtime, resolved
// from the Runner's registered state, never from a list the caller selects.
// A caller cannot add a dependency to a governed submission; a dependency
// omitted from, or substituted into, the registered release or the prepared
// attempt is refused before the authority is asked or before the Runtime
// starts, and nothing falls back to another copy.
func TestGovernedAdmissionOmittedDependencySubstitution(t *testing.T) {
	t.Run("caller-supplied-dependency", func(t *testing.T) {
		h := newGovernedHarness(t, nil)
		raw := []byte(`{"facts":` + string(sample().Facts) + `,"evidence":` + string(sample().Evidence) + `}`)
		intent, err := captureManualIntent("intent_supplied", "key-supplied", raw)
		executorMust(t, err)
		in := intent.input()
		in.Source = &SourceInput{Case: json.RawMessage(`{}`)}
		if _, _, err = h.s.submitInternalIntent(h.job.ID, "key-supplied", in, nil, intent); err == nil {
			t.Fatal("a governed submission carried its own dependency")
		}
		if h.count("inspect") != 0 {
			t.Fatal("the authority was asked about a caller-selected closure")
		}
	})
	t.Run("release-dependency-substituted", func(t *testing.T) {
		h := newGovernedHarness(t, nil)
		run, err := h.submit("intent_release")
		executorMust(t, err)
		release, err := h.s.release(run.ReleaseID)
		executorMust(t, err)
		release.Lock = strings.Replace(release.Lock, "{", `{"substituted":true,`, 1)
		_, err = h.s.db.Exec("UPDATE releases SET record=? WHERE id=?", string(encode(release)), release.ID)
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
		if h.count("reserve") != 0 || h.count("commitStart") != 0 {
			t.Fatal("a substituted release dependency reached the authority")
		}
	})
	for _, c := range []struct{ name, file string }{{"lock-omitted", "jpack.lock.json"}, {"config-omitted", "jpack.json"}, {"pack-omitted", "pack.json"}} {
		t.Run(c.name, func(t *testing.T) {
			var h *governedHarness
			h = newGovernedHarness(t, func(h *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
				if method == "commitStart" {
					if os.Remove(filepath.Join(h.cfg.Dir, "attempts", h.runs["intent_omitted"], c.file)) != nil {
						return "", nil, true
					}
				}
				return "", nil, false
			})
			run, err := h.submit("intent_omitted")
			executorMust(t, err)
			h.requireNoLaunch(h.dispatch(run.ID))
		})
	}
	t.Run("runtime-substituted", func(t *testing.T) {
		var h *governedHarness
		h = newGovernedHarness(t, func(h *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "commitStart" {
				release, err := h.s.release(h.job.ReleaseID)
				if err != nil {
					return "", nil, true
				}
				path := filepath.Join(h.cfg.Dir, "runtimes", strings.TrimPrefix(release.RuntimeDigest, "sha256:"))
				os.Chmod(path, 0700)
				if os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700) != nil {
					return "", nil, true
				}
			}
			return "", nil, false
		})
		run, err := h.submit("intent_runtime")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
	})
}

// Withdrawal, enqueue and dispatch races. Withdrawal committed before a run is
// enqueued refuses the submission; withdrawal between enqueue and dispatch, or
// between reservation and the start commitment, blocks it; a run whose start
// was committed before withdrawal finishes, and a run enqueued beside it but
// not yet started is blocked.
func TestGovernedAdmissionWithdrawalEnqueueDispatchRaces(t *testing.T) {
	t.Run("withdrawn-before-enqueue", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "inspect" {
				return "ok", ManualInspectResult{Delivery: "observed", Registration: ManualRegistration{Revision: "1", Digest: "sha256:" + strings.Repeat("a", 64)}, Eligible: false, Reason: "withdrawn", At: "2026-10-19T12:00:00Z"}, false
			}
			return "", nil, false
		})
		if _, err := h.submit("intent_before"); err == nil {
			t.Fatal("a withdrawn resource accepted a governed submission")
		}
		if h.count("reserve") != 0 {
			t.Fatal("a withdrawn resource was reserved")
		}
		if worked, err := h.s.dispatchNext(context.Background()); err != nil || worked {
			t.Fatal("a refused submission was queued", worked, err)
		}
	})
	t.Run("withdrawn-between-enqueue-and-dispatch", func(t *testing.T) {
		withdrawn := false
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "reserve" && withdrawn {
				return refusal("eligibility-blocked")
			}
			return "", nil, false
		})
		run, err := h.submit("intent_between")
		executorMust(t, err)
		withdrawn = true
		h.requireNoLaunch(h.dispatch(run.ID))
		if h.count("commitStart") != 0 {
			t.Fatal("a blocked reservation continued to the start commitment")
		}
	})
	t.Run("withdrawn-before-start-commitment", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
			if method == "commitStart" {
				return startDecision(body, "blocked", "withdrawn")
			}
			return "", nil, false
		})
		run, err := h.submit("intent_commit")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
		var record manualAttemptJournal
		if json.Unmarshal([]byte(h.journal(run.ID)), &record) != nil || record.State != "blocked" || record.Receipt == nil || record.Receipt.Reason != "withdrawn" {
			t.Fatalf("the blocked decision is not retained: %s", h.journal(run.ID))
		}
	})
	t.Run("withdrawn-after-start-commitment", func(t *testing.T) {
		withdrawn := false
		h := newGovernedHarness(t, func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
			switch {
			case method == "commitStart" && !withdrawn:
				withdrawn = true // the withdrawal commits right after this start
				return startDecision(body, "start-committed", "start-committed")
			case method == "reserve" && withdrawn:
				return refusal("eligibility-blocked")
			case method == "commitStart" && withdrawn:
				return startDecision(body, "blocked", "withdrawn")
			}
			return "", nil, false
		})
		first, err := h.submit("intent_admitted")
		executorMust(t, err)
		second, err := h.submit("intent_queued")
		executorMust(t, err)
		h.requireCompleted(h.dispatch(first.ID))
		h.requireNoLaunch(h.dispatch(second.ID))
	})
}

// A lost admission response. A reservation or start commitment whose reply
// never arrives may have committed at the authority, so it is never retried:
// the channel closes, the run does not start, later governed work holds until
// a new channel is activated, and the retained reservation is a replay
// barrier, never a permit.
func TestGovernedAdmissionLostAdmissionResponse(t *testing.T) {
	t.Run("reserve-reply-lost", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			return "", nil, method == "reserve"
		})
		first, err := h.submit("intent_lost_reserve")
		executorMust(t, err)
		second, err := h.submit("intent_after_loss")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(first.ID))
		h.requireNoLaunch(h.dispatch(second.ID))
		if h.count("reserve") != 1 || h.count("commitStart") != 0 {
			t.Fatalf("an uncertain reservation was retried or continued: %v", h.methods)
		}
	})
	t.Run("start-reply-lost", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			return "", nil, method == "commitStart"
		})
		run, err := h.submit("intent_lost_start")
		executorMust(t, err)
		ended := h.dispatch(run.ID)
		h.requireNoLaunch(ended)
		var record manualAttemptJournal
		if json.Unmarshal([]byte(h.journal(run.ID)), &record) != nil || record.State != "reserved" || record.Receipt != nil {
			t.Fatalf("the uncertain start is not held as reserved: %s", h.journal(run.ID))
		}
		release, err := h.s.release(run.ReleaseID)
		executorMust(t, err)
		h.connect(nil)
		before := len(h.methods)
		if _, _, _, _, err = h.s.governedEvaluation(context.Background(), ended, release); err == nil {
			t.Fatal("a run with an uncertain start was evaluated again")
		}
		if len(h.methods) != before || h.count("commitStart") != 1 {
			t.Fatalf("the retained reservation reached the authority again: %v", h.methods[before:])
		}
	})
}

// Worker restart and fencing. A Runner restarted on the same store activates a
// new epoch. A reservation retained under the old epoch, whose start reply was
// lost in the crash, is never evaluated again and its start request is never
// sent on the new channel; a reply carrying another epoch than the channel's
// is refused; the authority's stale-epoch refusal blocks the start; and an
// epoch is read only as a canonical decimal string, never as a JSON number.
func TestGovernedAdmissionWorkerRestartAndFencing(t *testing.T) {
	t.Run("restart-fences-the-old-epoch", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			return "", nil, method == "commitStart" // the crash takes the reply
		})
		run, err := h.submit("intent_restart")
		executorMust(t, err)
		ended := h.dispatch(run.ID)
		h.requireNoLaunch(ended)
		var record manualAttemptJournal
		executorMust(t, json.Unmarshal([]byte(h.journal(run.ID)), &record))
		old := admissionFence{Resource: record.Request.Resource, Owner: h.s.cfg.ExecutorAdmission.ExecutorID, Epoch: record.Request.Epoch}

		h.connect(func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
			if method == "activate" {
				d, _ := manualRequestDigest(method, body)
				return "ok", ManualActivateResult{Delivery: "committed", Epoch: "2", RequestDigest: d}, false
			}
			return "", nil, false
		})
		current := admissionFence{Resource: old.Resource, Owner: h.s.cfg.ExecutorAdmission.ExecutorID, Epoch: h.s.authority.epoch}
		if current.Epoch != "2" || current == old || current.Owner != old.Owner {
			t.Fatalf("the restart did not activate a new epoch for the same owner: %+v %+v", old, current)
		}
		before := len(h.methods)
		if _, err := h.s.authority.CommitStart(context.Background(), record.Request); err == nil {
			t.Fatal("a start request fenced by the old epoch was sent on the new channel")
		}
		if len(h.methods) != before {
			t.Fatalf("the old epoch's start request left the Runner: %v", h.methods[before:])
		}
		h.connect(nil)
		release, err := h.s.release(run.ReleaseID)
		executorMust(t, err)
		before = len(h.methods)
		if _, _, _, _, err = h.s.governedEvaluation(context.Background(), ended, release); err == nil || len(h.methods) != before {
			t.Fatal("the run of the old epoch was evaluated again after the restart")
		}
	})
	t.Run("reply-of-another-epoch", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
			if method == "reserve" {
				d, _ := manualRequestDigest(method, body)
				return "ok", ManualReserveResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "2", RequestDigest: d}, false
			}
			return "", nil, false
		})
		run, err := h.submit("intent_other_epoch")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
		if h.count("commitStart") != 0 {
			t.Fatal("a reservation under another epoch continued")
		}
	})
	t.Run("stale-epoch-refused", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "commitStart" {
				return refusal("stale-epoch")
			}
			return "", nil, false
		})
		run, err := h.submit("intent_stale")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
	})
	t.Run("epoch-as-number-refused", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		binding := manualFixture(t).Synthetic.Boot
		authority, err := NewManualAuthority(a, binding)
		executorMust(t, err)
		go func() {
			line := make([]byte, manualFrameLimit)
			n, _ := b.Read(line)
			var frame manualRequestEnvelope
			if manualDecode(line[:n], &frame) != nil {
				return
			}
			d, _ := manualRequestDigest("activate", frame.Body)
			b.Write([]byte(`{"version":1,"sequence":"` + frame.Sequence + `","method":"activate","status":"ok","body":{"delivery":"committed","epoch":1,"requestDigest":"` + d + `"}}` + "\n"))
		}()
		_, err = authority.Activate(context.Background(), ManualActivateRequest{Challenge: binding.Challenge, Signing: ManualSigning{Mode: "off"}})
		if err == nil || !errors.Is(err, errManualProtocol) {
			t.Fatalf("an epoch written as a JSON number was accepted: %v", err)
		}
	})
}

// Identity revocation. An executor identity the authority revokes after
// activation gets no reservation and no start commitment: each governed run
// fails without reaching the Runtime, once, and no refused request is retried.
func TestGovernedAdmissionIdentityRevocation(t *testing.T) {
	t.Run("revoked-before-reservation", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "reserve" {
				return refusal("unauthorized")
			}
			return "", nil, false
		})
		first, err := h.submit("intent_revoked_one")
		executorMust(t, err)
		second, err := h.submit("intent_revoked_two")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(first.ID))
		h.requireNoLaunch(h.dispatch(second.ID))
		if h.count("reserve") != 2 || h.count("commitStart") != 0 {
			t.Fatalf("a refused reservation was retried or continued: %v", h.methods)
		}
	})
	t.Run("revoked-before-start-commitment", func(t *testing.T) {
		h := newGovernedHarness(t, func(_ *governedHarness, method string, _ json.RawMessage) (string, any, bool) {
			if method == "commitStart" {
				return refusal("unauthorized")
			}
			return "", nil, false
		})
		run, err := h.submit("intent_revoked_start")
		executorMust(t, err)
		h.requireNoLaunch(h.dispatch(run.ID))
		if h.count("commitStart") != 1 || h.journal(run.ID) == "" {
			t.Fatalf("the refused start was retried or its reservation not held: %v", h.methods)
		}
	})
}

// Pre-withdrawal restore. A store restored from a backup taken before a
// withdrawal, or before a run started, holds the run queued and none of its
// attempt history or directory. It cannot resurrect authority: the authority's one-attempt
// barrier answers the restored intent as recovered, or refuses it, and the
// Runner starts nothing on either; a restored queue after withdrawal is
// blocked as any queue is.
func TestGovernedAdmissionPreWithdrawalRestore(t *testing.T) {
	restore := func(h *governedHarness, runID, state, record string) {
		h.t.Helper()
		_, err := h.s.db.Exec("UPDATE runs SET state=?, record=? WHERE id=?", state, record, runID)
		executorMust(h.t, err)
		_, err = h.s.db.Exec("DELETE FROM manual_attempts WHERE run_id=?", runID)
		executorMust(h.t, err)
		// The backup predates the attempt's directory too.
		executorMust(h.t, os.RemoveAll(filepath.Join(h.cfg.Dir, "attempts", runID)))
	}
	backup := func(h *governedHarness, runID string) (string, string) {
		h.t.Helper()
		var state, record string
		executorMust(h.t, h.s.db.QueryRow("SELECT state, record FROM runs WHERE id=?", runID).Scan(&state, &record))
		return state, record
	}
	for _, answer := range []string{"recovered", "intent-unavailable"} {
		t.Run("restored-after-start-"+answer, func(t *testing.T) {
			reserved := map[string]bool{}
			h := newGovernedHarness(t, func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
				if method != "reserve" {
					return "", nil, false
				}
				var q ManualReserveRequest
				_ = json.Unmarshal(body, &q)
				if !reserved[q.IntentID] {
					reserved[q.IntentID] = true
					return "", nil, false
				}
				if answer == "recovered" {
					d, _ := manualRequestDigest(method, body)
					return "ok", ManualReserveResult{Delivery: "recovered", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "1", RequestDigest: d}, false
				}
				return refusal(answer)
			})
			run, err := h.submit("intent_restored")
			executorMust(t, err)
			state, record := backup(h, run.ID)
			h.requireCompleted(h.dispatch(run.ID))
			restore(h, run.ID, state, record)
			again := h.dispatch(run.ID)
			if again.State != "failed" || h.count("commitStart") != 1 {
				t.Fatalf("a restored run resurrected its admission: %s %v", again.State, h.methods)
			}
		})
	}
	t.Run("restored-queue-after-withdrawal", func(t *testing.T) {
		withdrawn := false
		h := newGovernedHarness(t, func(_ *governedHarness, method string, body json.RawMessage) (string, any, bool) {
			switch {
			case method == "reserve" && withdrawn:
				return refusal("eligibility-blocked")
			case method == "commitStart" && withdrawn:
				return startDecision(body, "blocked", "withdrawn")
			}
			return "", nil, false
		})
		run, err := h.submit("intent_backup")
		executorMust(t, err)
		state, record := backup(h, run.ID)
		withdrawn = true
		restore(h, run.ID, state, record)
		h.requireNoLaunch(h.dispatch(run.ID))
		if h.count("commitStart") != 0 {
			t.Fatal("a restored queue reached the start commitment after withdrawal")
		}
	})
}
