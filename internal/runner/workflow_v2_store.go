package runner

// Private branch CAS and durable custody. Custody is not a portable baseline:
// recordingFiles includes only artifacts admitted by canonical proof prefixes.
import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
)

type workflowV2Store struct{ db *sql.DB }
type workflowV2Custody struct {
	Instance         string
	Number, Sequence int
	Kind             string
	Body             []byte
}

func newWorkflowV2Store(db *sql.DB) (*workflowV2Store, error) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS workflow_v2_branches (
 execution TEXT NOT NULL, branch TEXT NOT NULL, revision INTEGER NOT NULL, body BLOB NOT NULL,
 PRIMARY KEY(execution,branch));
 CREATE TABLE IF NOT EXISTS workflow_v2_artifacts (
 execution TEXT NOT NULL, key TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(execution,key));
 CREATE TABLE IF NOT EXISTS workflow_v2_membership (
 execution TEXT NOT NULL, branch TEXT NOT NULL, key TEXT NOT NULL, PRIMARY KEY(execution,branch,key));
 CREATE TABLE IF NOT EXISTS workflow_v2_custody (
 execution TEXT NOT NULL, branch TEXT NOT NULL, instance TEXT NOT NULL, attempt INTEGER NOT NULL,
 sequence INTEGER NOT NULL, kind TEXT NOT NULL, body BLOB NOT NULL, digest TEXT NOT NULL,
	PRIMARY KEY(execution,branch,instance,attempt,sequence));
	CREATE TABLE IF NOT EXISTS workflow_v2_drives (
	execution TEXT NOT NULL, branch TEXT NOT NULL, instance TEXT NOT NULL, attempt INTEGER NOT NULL,
	PRIMARY KEY(execution,branch,instance,attempt));`)
	if err != nil {
		return nil, err
	}
	return &workflowV2Store{db}, nil
}
func workflowV2StoreBody(w workflowV2Inventory) []byte {
	copy := w
	copy.Core.Artifacts = nil
	return encode(copy)
}
func workflowV2StoreArtifacts(tx *sql.Tx, branch string, w workflowV2Inventory) error {
	for _, key := range sortedKeys(w.Core.Artifacts) {
		if !workflowArtifactKey(key) {
			return errWorkflowTransition
		}
		raw := w.Core.Artifacts[key]
		if _, err := tx.Exec(`INSERT OR IGNORE INTO workflow_v2_artifacts(execution,key,body) VALUES(?,?,?)`, w.Core.Execution.Execution, key, raw); err != nil {
			return err
		}
		var held []byte
		if err := tx.QueryRow(`SELECT body FROM workflow_v2_artifacts WHERE execution=? AND key=?`, w.Core.Execution.Execution, key).Scan(&held); err != nil {
			return err
		}
		if !bytes.Equal(raw, held) {
			return errWorkflowTransition
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO workflow_v2_membership(execution,branch,key) VALUES(?,?,?)`, w.Core.Execution.Execution, branch, key); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT key FROM workflow_v2_membership WHERE execution=? AND branch=?`, w.Core.Execution.Execution, branch)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return err
		}
		if _, ok := w.Core.Artifacts[key]; !ok {
			return errWorkflowTransition
		}
	}
	return rows.Err()
}
func (s *workflowV2Store) create(branch string, w workflowV2Inventory) error {
	if !workflowID.MatchString(branch) {
		return errWorkflowForm
	}
	if err := workflowV2RequireRecording(w); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO workflow_v2_branches(execution,branch,revision,body) VALUES(?,?,1,?)`, w.Core.Execution.Execution, branch, workflowV2StoreBody(w)); err != nil {
		return err
	}
	if err = workflowV2StoreArtifacts(tx, branch, w); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *workflowV2Store) load(execution, branch string) (workflowV2Inventory, int, error) {
	var w workflowV2Inventory
	var revision int
	tx, err := s.db.Begin()
	if err != nil {
		return w, 0, err
	}
	defer tx.Rollback()
	var body []byte
	if err = tx.QueryRow(`SELECT revision,body FROM workflow_v2_branches WHERE execution=? AND branch=?`, execution, branch).Scan(&revision, &body); err != nil {
		return w, 0, err
	}
	if err = json.Unmarshal(body, &w); err != nil {
		return w, 0, err
	}
	w.Core.Artifacts = map[string][]byte{}
	rows, err := tx.Query(`SELECT a.key,a.body FROM workflow_v2_artifacts a JOIN workflow_v2_membership m ON a.execution=m.execution AND a.key=m.key WHERE m.execution=? AND m.branch=? ORDER BY a.key`, execution, branch)
	if err != nil {
		return w, 0, err
	}
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			break
		}
		w.Core.Artifacts[key] = raw
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return w, 0, err
	}
	if rowErr != nil {
		return w, 0, rowErr
	}
	proof, err := w.Proof.bytes()
	if err != nil {
		return w, 0, err
	}
	if _, err = readWorkflowV2Proof(proof); err != nil {
		return w, 0, err
	}
	if err = workflowV2RequireRecording(w); err != nil {
		return w, 0, err
	}
	if err = tx.Commit(); err != nil {
		return w, 0, err
	}
	return w, revision, nil
}
func workflowV2Extends(old, next workflowV2Proof) bool {
	a, b := []json.RawMessage{}, []json.RawMessage{}
	for _, block := range old.EpochBlocks {
		a = append(a, block...)
	}
	for _, block := range next.EpochBlocks {
		b = append(b, block...)
	}
	if len(a) > len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if i < len(a)-1 || old.closed() {
			if !bytes.Equal(a[i], b[i]) {
				return false
			}
			continue
		}
		if len(a) != len(b) {
			return false
		}
		var x, y map[string]json.RawMessage
		_ = json.Unmarshal(a[i], &x)
		_ = json.Unmarshal(b[i], &y)
		var xr, yr []workflowV2Observation
		if json.Unmarshal(x["results"], &xr) != nil || json.Unmarshal(y["results"], &yr) != nil || len(xr) > len(yr) {
			return false
		}
		if !bytes.Equal(encode(xr), encode(yr[:len(xr)])) {
			return false
		}
		delete(x, "results")
		delete(y, "results")
		if !bytes.Equal(encode(x), encode(y)) {
			return false
		}
	}
	return true
}
func (s *workflowV2Store) save(branch string, w workflowV2Inventory, revision int) error {
	if revision < 1 {
		return errWorkflowRevision
	}
	if err := workflowV2RequireRecording(w); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldBody []byte
	var old workflowV2Inventory
	if err = tx.QueryRow(`SELECT body FROM workflow_v2_branches WHERE execution=? AND branch=? AND revision=?`, w.Core.Execution.Execution, branch, revision).Scan(&oldBody); err != nil {
		if err == sql.ErrNoRows {
			return errWorkflowRevision
		}
		return err
	}
	if json.Unmarshal(oldBody, &old) != nil || !bytes.Equal(encode(old.Core.Document), encode(w.Core.Document)) || !workflowV2Extends(old.Proof, w.Proof) {
		return errWorkflowTransition
	}
	if bytes.Equal(encode(old.Proof), encode(w.Proof)) && !bytes.Equal(oldBody, workflowV2StoreBody(w)) {
		return errWorkflowTransition
	}
	result, err := tx.Exec(`UPDATE workflow_v2_branches SET revision=revision+1,body=? WHERE execution=? AND branch=? AND revision=?`, workflowV2StoreBody(w), w.Core.Execution.Execution, branch, revision)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errWorkflowRevision
	}
	if err = workflowV2StoreArtifacts(tx, branch, w); err != nil {
		return err
	}
	return tx.Commit()
}

// appendCustody intentionally does not apply portable maxBytes: an overflow
// remains held outside the last valid prefix. Existing acquisition/body limits
// still apply at their own boundaries. There is no automatic re-acquisition.
func (s *workflowV2Store) appendCustody(execution, branch, instance string, number int, kind string, body []byte) error {
	if kind != "checkpoint" && kind != "result" && kind != "source" && kind != "evaluation" {
		return errWorkflowForm
	}
	if len(body) == 0 {
		return errWorkflowForm
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var w workflowV2Inventory
	if err = tx.QueryRow(`SELECT body FROM workflow_v2_branches WHERE execution=? AND branch=?`, execution, branch).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal(raw, &w) != nil {
		return errWorkflowTransition
	}
	i, ok := w.History[instance]
	if !ok || number < 1 || number > len(i.Attempts) {
		return errWorkflowTransition
	}
	var sequence int
	if err = tx.QueryRow(`SELECT COALESCE(MAX(sequence),0)+1 FROM workflow_v2_custody WHERE execution=? AND branch=? AND instance=? AND attempt=?`, execution, branch, instance, number).Scan(&sequence); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO workflow_v2_custody(execution,branch,instance,attempt,sequence,kind,body,digest) VALUES(?,?,?,?,?,?,?,?)`, execution, branch, instance, number, sequence, kind, body, digest(body)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *workflowV2Store) custody(execution, branch string) ([]workflowV2Custody, error) {
	rows, err := s.db.Query(`SELECT instance,attempt,sequence,kind,body,digest FROM workflow_v2_custody WHERE execution=? AND branch=? ORDER BY instance,attempt,sequence`, execution, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workflowV2Custody{}
	for rows.Next() {
		var c workflowV2Custody
		var pin string
		if err = rows.Scan(&c.Instance, &c.Number, &c.Sequence, &c.Kind, &c.Body, &pin); err != nil {
			return nil, err
		}
		if digest(c.Body) != pin {
			return nil, fmt.Errorf("custody digest mismatch: %w", errWorkflowTransition)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// claimDrive is durable local ownership, not proof a remote request happened.
// Claims are never cleared for automatic replay; recovery seals uncertainty and
// an explicit retry uses a new attempt number. The unique key closes the race
// between checking empty custody and invoking the shared host driver.
func (s *workflowV2Store) claimDrive(execution, branch, instance string, number int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var body []byte
	var w workflowV2Inventory
	if err = tx.QueryRow(`SELECT body FROM workflow_v2_branches WHERE execution=? AND branch=?`, execution, branch).Scan(&body); err != nil {
		return err
	}
	if json.Unmarshal(body, &w) != nil || w.Proof.closed() {
		return errWorkflowTransition
	}
	e, err := w.Proof.last()
	if err != nil {
		return err
	}
	next := e.begins()[len(e.Results)]
	if next.Instance != instance || next.Number != number || w.Core.Pending[instance] != number {
		return errWorkflowTransition
	}
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM workflow_v2_custody WHERE execution=? AND branch=? AND instance=? AND attempt=?`, execution, branch, instance, number).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errWorkflowTransition
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO workflow_v2_drives(execution,branch,instance,attempt) VALUES(?,?,?,?)`, execution, branch, instance, number)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return errWorkflowTransition
	}
	return tx.Commit()
}
