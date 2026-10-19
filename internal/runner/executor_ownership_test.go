//go:build linux || darwin || freebsd

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func executorOwnedConfig(f executorLeaseFixture, runtime string) Config {
	return Config{Dir: f.state, Runtime: runtime, Workspace: "runner-distinct-workspace", Owner: "local-owner", disableAutomation: true, disableDispatcher: true,
		ExecutorOwnership: &ExecutorOwnership{Root: f.root, MachineWorkspace: f.identity.Workspace, RunnerWorkspace: "runner-distinct-workspace", RootDevice: f.identity.RootDevice, RootInode: f.identity.RootInode, FileDevice: f.identity.FileDevice, FileInode: f.identity.FileInode}}
}

func TestExecutorOwnedServiceRefusesBeforePortableSideEffects(t *testing.T) {
	for _, name := range []string{"missing state", "wrong pin", "wrong runner workspace", "contended"} {
		t.Run(name, func(t *testing.T) {
			f := executorFixture(t)
			cfg := executorOwnedConfig(f, os.Args[0])
			switch name {
			case "missing state":
				executorMust(t, os.Remove(f.state))
			case "wrong pin":
				cfg.ExecutorOwnership.FileInode++
			case "wrong runner workspace":
				cfg.ExecutorOwnership.RunnerWorkspace = "other"
			case "contended":
				held, err := f.acquire()
				executorMust(t, err)
				defer held.Close()
			}
			s, err := Open(cfg)
			if err == nil {
				s.Close()
				t.Fatal("invalid ownership opened service")
			}
			if name == "missing state" {
				if _, err := os.Lstat(f.state); !os.IsNotExist(err) {
					t.Fatalf("created absent state: %v", err)
				}
			} else {
				entries, err := os.ReadDir(f.state)
				executorMust(t, err)
				if len(entries) != 0 {
					t.Fatalf("ownership refusal wrote portable state: %v", entries)
				}
			}
		})
	}
}

func TestExecutorOwnedServiceSnapshotsAndNeverFallsBack(t *testing.T) {
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, os.Args[0])
	original := *cfg.ExecutorOwnership
	s, err := Open(cfg)
	executorMust(t, err)
	defer s.Close()
	// Mutating the caller's object cannot change the service's trusted binding.
	cfg.ExecutorOwnership.Root = "/missing"
	cfg.ExecutorOwnership.MachineWorkspace = "different"
	cfg.ExecutorOwnership.FileInode++
	if *s.cfg.ExecutorOwnership != original {
		t.Fatal("trusted ownership configuration remained mutable")
	}
	if held, err := f.acquire(); err == nil {
		held.Close()
		t.Fatal("service did not retain lease")
	}
	directory := t.TempDir()
	if _, err := s.invoke(context.Background(), s.runtime, directory, "probe", "-test.run=^NoSuchTest$"); err != nil {
		t.Fatal("caller mutation affected actual start", err)
	}
	s.Close()
	// A valid executable makes refusal meaningful. The closed, immutable lease
	// must never be replaced by the standalone invocation fallback.
	if _, err := s.invoke(context.Background(), s.runtime, t.TempDir(), "closed", "-test.run=^NoSuchTest$"); err == nil {
		t.Fatal("closed owned service launched Runtime")
	}
	held, err := f.acquire()
	executorMust(t, err)
	held.Close()
}

func TestExecutorOwnedFailedOpenReleasesLease(t *testing.T) {
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, filepath.Join(f.state, "missing-runtime"))
	if s, err := Open(cfg); err == nil {
		s.Close()
		t.Fatal("missing Runtime accepted")
	}
	held, err := f.acquire()
	executorMust(t, err)
	held.Close()
}

func TestExecutorOwnedServiceRealRuntimeJobsAndChecks(t *testing.T) {
	real := testConfig(t)
	f := executorFixture(t)
	cfg := executorOwnedConfig(f, real.Runtime)
	s, err := Open(cfg)
	executorMust(t, err)
	defer s.Close()
	pack, err := os.ReadFile("testdata/triage.pack.json")
	executorMust(t, err)
	release, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample(), Matrix: releaseMatrix("proceed")})
	executorMust(t, err)
	if release.Tests != "passed" {
		t.Fatalf("saved checks failed: %+v", release.TestEvidence)
	}
	job, err := s.createJob("Owned intake", release.ID)
	executorMust(t, err)
	run, _, err := s.submit(job.ID, "owned", sample())
	executorMust(t, err)
	worked, err := s.dispatchNext(context.Background())
	executorMust(t, err)
	completed, err := s.run(run.ID)
	executorMust(t, err)
	if !worked || completed.State != "completed" || len(completed.Audit) == 0 {
		t.Fatalf("owned evaluation failed: %+v", completed)
	}
	if err := s.checkUpgradeVersion(context.Background(), "1.0.0", "1.0.1"); err != nil {
		t.Fatal(err)
	}
	// The private workflow adapter uses the same actual Service evaluator.
	answer, err := s.workflowDriver(nil).Evaluate(context.Background(), release, sample(), "owned-workflow")
	executorMust(t, err)
	if len(answer.Audit) == 0 {
		t.Fatal("workflow omitted real audit")
	}
	s.Close()
	if _, err := s.workflowDriver(nil).Evaluate(context.Background(), release, sample(), "closed-workflow"); err == nil {
		t.Fatal("closed private workflow launched Runtime")
	}
	check := s.checkReleaseTests(context.Background(), release, releaseMatrix("proceed"), nil, filepath.Join(f.state, "closed-check"))
	if check.Status != "error" {
		t.Fatal("closed service ran saved tests")
	}
	if _, err := s.preview(context.Background(), PreviewRequest{Pack: string(pack), Input: sample()}); err == nil {
		t.Fatal("closed service preview launched Runtime")
	}
}
