package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRelocationPreservesRecordsAndIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	cfg.InputRoot = relocationTempDir(t)
	cfg.disableAutomation = true
	service, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	release := testRelease(t, service)
	job, err := service.createJob("Retained job", release.ID)
	if err != nil {
		t.Fatal(err)
	}
	var original string
	if err = service.db.QueryRow("SELECT record FROM jobs WHERE id=?", job.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(relocationTempDir(t), "moved-project")
	request := RelocateRequest{Version: 1, ID: strings.Repeat("a", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: next}
	if err = os.Rename(cfg.InputRoot, next); err != nil {
		t.Fatal(err)
	}
	if _, err = RelocateWorkspace(request); err == nil {
		t.Fatal("changed a live workspace")
	}
	service.Close()
	wrong := request
	wrong.Owner = "someone-else"
	if _, err = RelocateWorkspace(wrong); err == nil {
		t.Fatal("changed workspace owner")
	}
	stale := request
	stale.PreviousInputRoot = relocationTempDir(t)
	if _, err = RelocateWorkspace(stale); err == nil {
		t.Fatal("changed stale binding")
	}
	result, err := RelocateWorkspace(request)
	if err != nil || result.AlreadyApplied {
		t.Fatal(result, err)
	}
	result, err = RelocateWorkspace(request)
	if err != nil || !result.AlreadyApplied {
		t.Fatal("not idempotent", result, err)
	}
	cfg.InputRoot = next
	again, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var saved string
	if err = again.db.QueryRow("SELECT record FROM jobs WHERE id=?", job.ID).Scan(&saved); err != nil || saved != original {
		t.Fatal("retained job bytes changed", err)
	}
	var history int
	if err = again.db.QueryRow("SELECT COUNT(*) FROM metadata WHERE key=?", "inputRootRelocation:"+request.ID).Scan(&history); err != nil || history != 1 {
		t.Fatal("history lost or duplicated", err)
	}
}
func TestWorkspaceRelocationRefusesRunningAndQueuedWork(t *testing.T) {
	for _, state := range []string{"queued", "running"} {
		t.Run(state, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.InputRoot = relocationTempDir(t)
			cfg.disableAutomation = true
			cfg.disableDispatcher = true
			service, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record)VALUES('run_test','job_test','test','key','digest',?,'{}')", state)
			if err != nil {
				t.Fatal(err)
			}
			service.Close()
			request := RelocateRequest{Version: 1, ID: strings.Repeat("b", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: relocationTempDir(t)}
			if _, err = RelocateWorkspace(request); err == nil {
				t.Fatal("moved unfinished work")
			}
		})
	}
}

func TestWorkspaceRelocationRefusesPendingSourcesAndTriggers(t *testing.T) {
	for _, state := range []string{"accepted", "preparing", "waiting", "needs-attention", "unpaused-trigger"} {
		t.Run(state, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.InputRoot = relocationTempDir(t)
			cfg.disableAutomation, cfg.disableDispatcher = true, true
			service, err := Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if state == "unpaused-trigger" {
				_, err = service.db.Exec(`INSERT INTO triggers(id,job_id,record) VALUES('trigger_test','job_test','{"paused":false}')`)
			} else {
				_, err = service.db.Exec(`INSERT INTO occurrences(id,job_id,trigger_id,identity,payload_digest,state,record) VALUES('occ_test','job_test','trigger_test','identity','digest',?,'{}')`, state)
			}
			if err != nil {
				t.Fatal(err)
			}
			service.Close()
			request := RelocateRequest{Version: 1, ID: strings.Repeat("c", 32), Dir: cfg.Dir, Workspace: cfg.Workspace, Owner: cfg.Owner, PreviousInputRoot: cfg.InputRoot, InputRoot: relocationTempDir(t)}
			if _, err = RelocateWorkspace(request); err == nil {
				t.Fatal("moved pending source work")
			}
		})
	}
}

func TestDecodeRelocationRefusesAmbiguousInput(t *testing.T) {
	for _, raw := range []string{`{"version":1,"version":2}`, `{"version":1,"unknown":"field"}`, `{} {}`, strings.Repeat(" ", 16385)} {
		if _, err := DecodeRelocateRequest([]byte(raw)); err == nil {
			t.Fatal("accepted ambiguous request")
		}
	}
}

// macOS exposes its temporary directory through /var -> /private/var. Requests
// carry canonical input roots, just as Desk's reviewed move plan does.
func relocationTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
