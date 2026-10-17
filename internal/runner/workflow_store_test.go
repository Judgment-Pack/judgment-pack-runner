package runner

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openWorkflowStoreTest(t *testing.T, path string) (*workflowStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	store, err := newWorkflowStore(db)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return store, db
}

func TestWorkflowStoreSelectionSurvivesRestartWithoutDuplicateChildren(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.sqlite")
	store, db := openWorkflowStoreTest(t, path)
	w := workflowInventoryFixture(t, 3)
	if err := store.create(w); err != nil {
		t.Fatal(err)
	}
	current, rev, err := store.load(w.Execution.Execution)
	if err != nil || rev != 1 {
		t.Fatal(rev, err)
	}
	selection := workflowSelectionFor(t, current, []string{current.Execution.OutputSets[0].Records[0], current.Execution.OutputSets[0].Records[2]}, 1)
	next, err := selectWorkflow(current, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(next, rev); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, db = openWorkflowStoreTest(t, path)
	defer db.Close()
	restored, rev, err := store.load(w.Execution.Execution)
	if err != nil || rev != 2 || !bytes.Equal(encode(restored), encode(next)) {
		t.Fatal(rev, err)
	}
	again, err := selectWorkflow(restored, selection, nil)
	if err != nil || !bytes.Equal(encode(again), encode(restored)) || len(again.Execution.Instances) != 3 {
		t.Fatal("duplicate admission after restart", err)
	}
	for _, child := range restored.Execution.Instances[1:] {
		if child.State != "interrupted" || len(child.Attempts) != 0 {
			t.Fatal("restart invented acquisition", child)
		}
	}
}

func TestWorkflowStoreStaleRevisionCannotOverwriteNewSelection(t *testing.T) {
	store, db := openWorkflowStoreTest(t, filepath.Join(t.TempDir(), "workflow.sqlite"))
	defer db.Close()
	w := workflowInventoryFixture(t, 3)
	if err := store.create(w); err != nil {
		t.Fatal(err)
	}
	first, rev, err := store.load(w.Execution.Execution)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.load(w.Execution.Execution)
	if err != nil {
		t.Fatal(err)
	}
	next, err := selectWorkflow(first, workflowSelectionFor(t, first, first.Execution.OutputSets[0].Records[:1], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(next, rev); err != nil {
		t.Fatal(err)
	}
	competing, err := selectWorkflow(second, workflowSelectionFor(t, second, second.Execution.OutputSets[0].Records[1:2], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(competing, rev); !errors.Is(err, errWorkflowRevision) {
		t.Fatal("stale selection overwrote admission", err)
	}
	held, _, err := store.load(w.Execution.Execution)
	if err != nil || !bytes.Equal(encode(held), encode(next)) {
		t.Fatal(err)
	}
}

func TestWorkflowStoreRollsBackAllChildrenAndArtifactsOnFailure(t *testing.T) {
	store, db := openWorkflowStoreTest(t, filepath.Join(t.TempDir(), "workflow.sqlite"))
	defer db.Close()
	w := workflowInventoryFixture(t, 3)
	if err := store.create(w); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER refuse_selection BEFORE INSERT ON workflow_artifacts WHEN NEW.key LIKE 'selection/%' BEGIN SELECT RAISE(ABORT, 'fixture persistence failure'); END;`); err != nil {
		t.Fatal(err)
	}
	next, err := selectWorkflow(w, workflowSelectionFor(t, w, w.Execution.OutputSets[0].Records[:2], 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(next, 1); err == nil {
		t.Fatal("fixture failed to refuse write")
	}
	held, rev, err := store.load(w.Execution.Execution)
	if err != nil || rev != 1 || !bytes.Equal(encode(held), encode(w)) {
		t.Fatal("partial child admission or artifact remained", rev, err)
	}
}

func TestWorkflowStoreCannotReplaceOrDropRetainedArtifacts(t *testing.T) {
	for _, name := range []string{"replace", "drop"} {
		t.Run(name, func(t *testing.T) {
			store, db := openWorkflowStoreTest(t, filepath.Join(t.TempDir(), "workflow.sqlite"))
			defer db.Close()
			w := workflowInventoryFixture(t, 3)
			if err := store.create(w); err != nil {
				t.Fatal(err)
			}
			changed := w.copy()
			key := "case/" + w.Execution.RootCaseDigest
			if name == "replace" {
				changed.Artifacts[key] = []byte(`{}`)
			} else {
				delete(changed.Artifacts, key)
			}
			if err := store.save(changed, 1); !errors.Is(err, errWorkflowTransition) {
				t.Fatal(err)
			}
			held, rev, err := store.load(w.Execution.Execution)
			if err != nil || rev != 1 || !bytes.Equal(encode(held), encode(w)) {
				t.Fatal("failed update changed retained state", rev, err)
			}
		})
	}
}
