//go:build linux || darwin || freebsd

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"
)

func TestManualPreparedLaunchFreshDurableAndSingleUse(t *testing.T) {
	fixture := manualFixture(t)
	s := manualServiceFixture(t)
	owned := executorFixture(t)
	lease, err := owned.acquire()
	executorMust(t, err)
	defer lease.Close()
	request := manualVectorBody[ManualStartRequest](t, fixture.Vectors[3].Request)
	var descriptor executionPayloadDescriptor
	executorMust(t, json.Unmarshal(request.Payload.Bytes, &descriptor))
	descriptor.RuntimeDigest = s.runtimeDigest
	request.Payload.Bytes, _ = canonical(encode(descriptor))
	request.Payload.Digest = digest(request.Payload.Bytes)
	command := func() *exec.Cmd {
		cmd := exec.CommandContext(context.Background(), s.runtime, "-test.run=^NoSuchTest$")
		cmd.Env = []string{"LANG=C"}
		cmd.Dir = s.cfg.Dir
		return cmd
	}
	receipt := func() ManualStartResult {
		d, _ := manualRequestDigest("commitStart", request)
		return ManualStartResult{"committed", request.OperationID, request.AttemptID, request.Epoch, d, "start-committed", "start-committed", "2026-10-19T12:00:00Z", fixture.Synthetic.Boot.InstallationID}
	}
	proofFor := func(p *manualPreparedLaunch, r ManualStartResult) *manualFreshCommit {
		c := &manualAuthorityClient{epoch: request.Epoch}
		p.client = c
		proof := &manualFreshCommit{client: c, request: request, receipt: r}
		c.commitProof = proof
		return proof
	}
	for _, mode := range []string{"copied-proof", "other-session", "stale-epoch"} {
		t.Run(mode, func(t *testing.T) {
			prepared, err := prepareManualLaunch(request, command(), lease)
			executorMust(t, err)
			proof := proofFor(prepared, receipt())
			switch mode {
			case "copied-proof":
				copy := *proof
				proof = &copy
			case "other-session":
				prepared.client = &manualAuthorityClient{epoch: request.Epoch}
			case "stale-epoch":
				proof.client.epoch = "999"
			}
			if _, err := prepared.retainFreshCommit(proof, func(ManualStartResult) error { t.Fatal("invalid proof reached journal"); return nil }); err == nil {
				t.Fatal("nonoriginal session proof minted")
			}
		})
	}
	t.Run("persistence failure", func(t *testing.T) {
		prepared, err := prepareManualLaunch(request, command(), lease)
		executorMust(t, err)
		if launch, err := prepared.retainFreshCommit(proofFor(prepared, receipt()), func(ManualStartResult) error { return errors.New("disk uncertainty") }); err == nil || launch != nil {
			t.Fatal("failed persistence minted launch")
		}
		if _, err = prepared.retainFreshCommit(proofFor(prepared, receipt()), func(ManualStartResult) error { return nil }); err == nil {
			t.Fatal("preparation reused after uncertainty")
		}
	})
	t.Run("recovered receipt", func(t *testing.T) {
		prepared, err := prepareManualLaunch(request, command(), lease)
		executorMust(t, err)
		r := receipt()
		r.Delivery = "recovered"
		if _, err = prepared.retainFreshCommit(proofFor(prepared, r), func(ManualStartResult) error { t.Fatal("recovered persistence"); return nil }); err == nil {
			t.Fatal("recovered receipt minted")
		}
	})
	t.Run("changed invocation consumes", func(t *testing.T) {
		prepared, err := prepareManualLaunch(request, command(), lease)
		executorMust(t, err)
		launch, err := prepared.retainFreshCommit(proofFor(prepared, receipt()), func(ManualStartResult) error { return nil })
		executorMust(t, err)
		changed := command()
		changed.Args = append(changed.Args, "-test.v")
		if err = launch.start(changed, lease); err == nil {
			changed.Wait()
			t.Fatal("changed invocation launched")
		}
		if err = launch.start(command(), lease); err == nil {
			t.Fatal("consumed launch reused")
		}
	})
	t.Run("one actual start", func(t *testing.T) {
		cmd := command()
		prepared, err := prepareManualLaunch(request, cmd, lease)
		executorMust(t, err)
		persisted := false
		launch, err := prepared.retainFreshCommit(proofFor(prepared, receipt()), func(ManualStartResult) error { persisted = true; return nil })
		executorMust(t, err)
		if !persisted {
			t.Fatal("not durable")
		}
		executorMust(t, launch.start(cmd, lease))
		executorMust(t, cmd.Wait())
		if err = launch.start(command(), lease); err == nil {
			t.Fatal("duplicate process started")
		}
	})
}
