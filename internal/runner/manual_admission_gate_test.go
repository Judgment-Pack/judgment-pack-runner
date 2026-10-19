//go:build linux || darwin || freebsd

package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualProfileRefusesUnsupportedBeforeActivity(t *testing.T) {
	s := manualServiceFixture(t)
	b := manualFixture(t).Synthetic.Boot
	s.cfg.ExecutorAdmission = &b
	checks := map[string]func() error{
		"trigger-create":      func() error { _, e := s.configureTrigger("missing", "", 0, TriggerConfig{}); return e },
		"trigger-enable":      func() error { _, _, e := s.setTriggerState("missing", 1, false, true); return e },
		"trigger-key":         func() error { _, _, e := s.rotateTriggerKey("missing", 1); return e },
		"event":               func() error { _, _, e := s.event("missing", "secret", EventDelivery{}); return e },
		"record":              func() error { _, _, e := s.deliverRecord("missing", "release", EventDelivery{}); return e },
		"schedule":            func() error { return s.scheduleTick(Trigger{}, "", time.Now()) },
		"file":                func() error { return s.fileTick(Trigger{}, "", time.Now()) },
		"dispatch-occurrence": func() error { return s.dispatchOccurrence(Occurrence{}, time.Now()) },
		"automatic":           func() error { _, e := s.acquireAutomatic(context.Background(), Input{}); return e },
		"planned":             func() error { _, e := s.acquirePlannedOperation(context.Background(), PlannedSource{}); return e },
		"prepare":             func() error { return s.prepareOccurrence(context.Background()) },
		"prepare-legacy":      func() error { return s.prepareLegacyOccurrence(context.Background(), "missing") },
		"input-plan":          func() error { _, e := s.nextInput(Input{}, time.Now()); return e },
		"read-source":         func() error { _, e := s.readAutomaticFile("/must-not-open"); return e },
		"profile":             func() error { _, e := s.registerInputProfile(InputProfile{}); return e },
		"upgrade":             func() error { _, e := s.upgradeJob(context.Background(), "missing", UpgradeJobRequest{}); return e },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); err == nil {
				t.Fatal("unsupported profile path proceeded")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Dir, "workflow-attempts")); !os.IsNotExist(err) {
		t.Fatal("unsupported path created workflow state")
	}
}

func TestManualGovernedRealRuntimeGate(t *testing.T) {
	for _, scenario := range []string{"success", "blocked", "lost-reply", "changed-facts", "changed-config", "changed-lock", "changed-signing", "changed-epoch", "changed-demand", "journal-failure", "start-journal-failure", "recovered-reserve"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.disableAutomation = true
			cfg.disableDispatcher = true
			if scenario == "changed-signing" {
				cfg.SigningKey = signingKey(t)
			}
			s, err := Open(cfg)
			executorMust(t, err)
			defer s.Close()
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
			var runID string
			commits := 0
			manualGateAuthority(t, s, func(method string, body json.RawMessage) (string, any, bool) {
				if method == "reserve" {
					if scenario == "journal-failure" {
						_, e := s.db.Exec("DROP TABLE manual_attempts")
						if e != nil {
							return "", nil, true
						}
					}
					if scenario == "recovered-reserve" {
						d, _ := manualRequestDigest(method, body)
						return "ok", ManualReserveResult{Delivery: "recovered", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "1", RequestDigest: d}, false
					}
				}
				if method == "commitStart" {
					commits++
					if scenario == "start-journal-failure" {
						if _, e := s.db.Exec("DROP TABLE manual_attempts"); e != nil {
							return "", nil, true
						}
					}
					if scenario == "lost-reply" {
						return "", nil, true
					}
					if scenario == "changed-facts" {
						if os.WriteFile(filepath.Join(cfg.Dir, "attempts", runID, "facts.json"), []byte(`{}`), 0600) != nil {
							return "", nil, true
						}
					}
					if scenario == "changed-config" || scenario == "changed-lock" {
						name := "jpack.json"
						if scenario == "changed-lock" {
							name = "jpack.lock.json"
						}
						if os.WriteFile(filepath.Join(cfg.Dir, "attempts", runID, name), []byte("null"), 0600) != nil {
							return "", nil, true
						}
					}
					if scenario == "changed-signing" {
						if os.WriteFile(cfg.SigningKey, []byte(strings.Repeat("0", 64)+"\n"), 0600) != nil {
							return "", nil, true
						}
					}
					if scenario == "changed-demand" {
						if os.Remove(filepath.Join(manualDemandDirectory(o), o.MachineWorkspace)) != nil {
							return "", nil, true
						}
					}
					if scenario == "changed-epoch" {
						d, _ := manualRequestDigest(method, body)
						return "ok", ManualStartResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "2", RequestDigest: d, Decision: "start-committed", Reason: "start-committed", At: "2026-10-19T12:00:00Z", AuthorityRevision: strings.Repeat("b", 64)}, false
					}
					if scenario == "blocked" {
						d, _ := manualRequestDigest(method, body)
						return "ok", ManualStartResult{Delivery: "committed", OperationID: "operation_test", AttemptID: "attempt_test", Epoch: "1", RequestDigest: d, Decision: "blocked", Reason: "withdrawn", At: "2026-10-19T12:00:00Z", AuthorityRevision: strings.Repeat("b", 64)}, false
					}
				}
				return "", nil, false
			})
			executorMust(t, os.Mkdir(manualDemandDirectory(o), 0700))
			executorMust(t, os.WriteFile(filepath.Join(manualDemandDirectory(o), o.MachineWorkspace), encode(s.cfg.ExecutorAdmission.demand()), 0600))
			facts := `{ ` + string(sample().Facts[1:len(sample().Facts)-1]) + `, "n":9007199254740993.00 }`
			raw := []byte(`{"facts":` + facts + `,"evidence":` + string(sample().Evidence) + `}`)
			intent, err := captureManualIntent("intent_gate", "original-key", raw)
			executorMust(t, err)
			run, replay, err := s.submitInternalIntent(job.ID, "original-key", intent.input(), nil, intent)
			executorMust(t, err)
			if replay {
				t.Fatal("unexpected replay")
			}
			runID = run.ID
			worked, err := s.dispatchNext(context.Background())
			executorMust(t, err)
			if !worked {
				t.Fatal("queued governed run not handled")
			}
			finished, err := s.run(run.ID)
			executorMust(t, err)
			if scenario == "success" {
				if finished.State != "completed" || len(finished.Audit) == 0 {
					t.Fatalf("actual Runtime failed: %s %s", finished.State, finished.Problem)
				}
				actual, e := os.ReadFile(filepath.Join(cfg.Dir, "attempts", run.ID, "facts.json"))
				executorMust(t, e)
				if string(actual) != facts {
					t.Fatal("actual Runtime facts preimage changed")
				}
			} else {
				if finished.State != "failed" || len(finished.Audit) != 0 {
					t.Fatalf("uncertain/blocked launch: %s %s", finished.State, finished.Problem)
				}
				if _, e := os.Stat(filepath.Join(cfg.Dir, "attempts", run.ID, "audit", "evaluations.jsonl")); !os.IsNotExist(e) {
					t.Fatalf("operational trail after refusal: %v", e)
				}
			}
			if (scenario == "recovered-reserve" || scenario == "journal-failure") && commits != 0 {
				t.Fatal("uncertain reserve/local journal continued to commit")
			}
			if err := s.startRuntimeAs(exec.Command(s.runtime, "experimental", "evaluate"), manualUtilityInvocation, nil); err == nil {
				t.Fatal("alternate operational utility path started")
			}
		})
	}
}
