package runner

// The workflow store keeps a transition and all newly retained artifacts in one
// SQLite transaction. It is deliberately not wired to a public route until run
// execution, recovery and offline verification are present.
import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

type workflowStore struct{ db *sql.DB }

var errWorkflowRevision = errors.New("workflow revision changed; reload before applying this transition")

func newWorkflowStore(db *sql.DB) (*workflowStore, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS workflow_executions (
 id TEXT PRIMARY KEY, revision INTEGER NOT NULL, document BLOB NOT NULL, record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS workflow_artifacts (
 execution TEXT NOT NULL, key TEXT NOT NULL, body BLOB NOT NULL,
 PRIMARY KEY(execution,key), FOREIGN KEY(execution) REFERENCES workflow_executions(id));`)
	if err != nil {
		return nil, err
	}
	return &workflowStore{db}, nil
}
func (s *workflowStore) create(w workflowInventory) error {
	if err := w.checkBounds(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO workflow_executions(id,revision,document,record) VALUES(?,1,?,?)`, w.Execution.Execution, encode(w.Document), encode(w.Execution))
	if err != nil {
		return err
	}
	if err := keepWorkflowArtifacts(tx, w); err != nil {
		return err
	}
	return tx.Commit()
}
func keepWorkflowArtifacts(tx *sql.Tx, w workflowInventory) error {
	for _, key := range sortedKeys(w.Artifacts) {
		if !workflowArtifactKey(key) {
			return errWorkflowTransition
		}
		raw := w.Artifacts[key]
		_, err := tx.Exec(`INSERT OR IGNORE INTO workflow_artifacts(execution,key,body) VALUES(?,?,?)`, w.Execution.Execution, key, raw)
		if err != nil {
			return err
		}
		var held []byte
		if err = tx.QueryRow(`SELECT body FROM workflow_artifacts WHERE execution=? AND key=?`, w.Execution.Execution, key).Scan(&held); err != nil {
			return err
		}
		if !bytes.Equal(held, raw) {
			return errWorkflowTransition
		}
	}
	return nil
}
func (s *workflowStore) load(id string) (workflowInventory, int, error) {
	var w workflowInventory
	var revision int
	tx, err := s.db.Begin()
	if err != nil {
		return w, 0, err
	}
	defer tx.Rollback()
	var document, record []byte
	if err = tx.QueryRow(`SELECT revision,document,record FROM workflow_executions WHERE id=?`, id).Scan(&revision, &document, &record); err != nil {
		return w, 0, err
	}
	if err = json.Unmarshal(document, &w.Document); err != nil {
		return w, 0, err
	}
	if err = json.Unmarshal(record, &w.Execution); err != nil {
		return w, 0, err
	}
	rows, err := tx.Query(`SELECT key,body FROM workflow_artifacts WHERE execution=? ORDER BY key`, id)
	if err != nil {
		return w, 0, err
	}
	w.Artifacts = map[string][]byte{}
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return w, 0, err
		}
		w.Artifacts[key] = raw
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, 0, err
	}
	if err = tx.Commit(); err != nil {
		return w, 0, err
	}
	return w, revision, nil
}
func (s *workflowStore) save(w workflowInventory, revision int) error {
	if revision < 1 {
		return errWorkflowRevision
	}
	if err := w.checkBounds(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var document []byte
	if err = tx.QueryRow(`SELECT document FROM workflow_executions WHERE id=?`, w.Execution.Execution).Scan(&document); err != nil {
		return err
	}
	if !bytes.Equal(document, encode(w.Document)) {
		return errWorkflowTransition
	}
	res, err := tx.Exec(`UPDATE workflow_executions SET revision=revision+1,record=? WHERE id=? AND revision=?`, encode(w.Execution), w.Execution.Execution, revision)
	if err != nil {
		return err
	}
	if count, err := res.RowsAffected(); err != nil || count != 1 {
		return errWorkflowRevision
	}
	if err := keepWorkflowArtifacts(tx, w); err != nil {
		return err
	}
	// An update cannot silently drop prior retained answers or selections.
	rows, err := tx.Query(`SELECT key FROM workflow_artifacts WHERE execution=?`, w.Execution.Execution)
	if err != nil {
		return err
	}
	missing := false
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			break
		}
		if _, ok := w.Artifacts[key]; !ok {
			missing = true
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	if missing {
		return errWorkflowTransition
	}
	return tx.Commit()
}

// A retained key is metadata, never a path. Exporters must choose fixed directory
// names and digest basenames, rather than interpolating caller-controlled keys.
func workflowArtifactKey(key string) bool {
	kind, pin, ok := strings.Cut(key, "/")
	if !ok || !workflowDigest.MatchString(pin) {
		return false
	}
	switch kind {
	case "workflow", "case", "published", "binding", "bound", "pack", "release", "selection", "output", "payload", "preparation", "audit", "run", "evaluation-error":
		return true
	}
	return false
}
