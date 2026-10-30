package runner

// Relocation is an offline host operation. It changes only the current input
// root binding, under Runner's instance lock and a single SQLite transaction.
// No HTTP capability grants it, and no retained run, release or audit is edited.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

type RelocateRequest struct {
	Version           int    `json:"version"`
	ID                string `json:"id"`
	Dir               string `json:"dir"`
	Workspace         string `json:"workspace"`
	Owner             string `json:"owner"`
	PreviousInputRoot string `json:"previousInputRoot"`
	InputRoot         string `json:"inputRoot"`
}
type RelocateResult struct {
	Version        int    `json:"version"`
	ID             string `json:"id"`
	InputRoot      string `json:"inputRoot"`
	AlreadyApplied bool   `json:"alreadyApplied"`
}

// DecodeRelocateRequest refuses unknown and duplicate JSON members before any
// directory or database is opened.
func DecodeRelocateRequest(raw []byte) (RelocateRequest, error) {
	var request RelocateRequest
	if len(raw) > 16384 {
		return request, errors.New("bounded relocation request required")
	}
	if err := checkRequestPaths("the relocation request", raw, relocationPathMembers); err != nil {
		return request, err
	}
	if err := strictJSON(raw, &request); err != nil {
		return request, errors.New("invalid relocation request")
	}
	return request, nil
}

func RelocateWorkspace(request RelocateRequest) (RelocateResult, error) {
	result := RelocateResult{Version: 1, ID: request.ID, InputRoot: request.InputRoot}
	// A Go host's paths are in the path domain too, before anything is read.
	if err := checkPaths("dir", request.Dir, "workspace", request.Workspace, "previousInputRoot", request.PreviousInputRoot, "inputRoot", request.InputRoot); err != nil {
		return result, err
	}
	absolute := func(path string) bool {
		return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) != path
	}
	if request.Version != 1 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(request.ID) || request.Workspace == "" || request.Owner == "" || !absolute(request.Dir) || !absolute(request.PreviousInputRoot) || !absolute(request.InputRoot) || request.PreviousInputRoot == request.InputRoot {
		return result, errors.New("invalid reviewed workspace relocation")
	}
	// Never create an empty workspace for a misspelled source.
	if info, err := os.Lstat(request.Dir); err != nil || !info.IsDir() {
		return result, errors.New("existing workspace required")
	}
	if err := privateDir(request.Dir); err != nil {
		return result, err
	}
	root, err := filepath.EvalSymlinks(request.InputRoot)
	if err != nil || root != request.InputRoot {
		return result, errors.New("new input root must be an existing real directory")
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return result, errors.New("new input root must be a directory")
	}
	lock, err := instanceLock(filepath.Join(request.Dir, "runner.lock"))
	if err != nil {
		return result, err
	}
	defer lock.Close()
	database := filepath.Join(request.Dir, "runs.sqlite")
	if err = privateFile(database); err != nil {
		return result, err
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return result, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;"); err != nil {
		return result, err
	}
	tx, err := db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for key, expected := range map[string]string{"workspace": request.Workspace, "owner": request.Owner} {
		var actual string
		if err = tx.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&actual); err != nil || actual != expected {
			return result, errors.New("workspace identity does not match the relocation")
		}
	}
	var schema string
	if err = tx.QueryRow("SELECT value FROM metadata WHERE key='schema'").Scan(&schema); err != nil || schema != "1" && schema != "2" && schema != storeSchema {
		return result, errors.New("unsupported workspace schema")
	}
	historyKey := "inputRootRelocation:" + request.ID
	var binding, prior string
	if err = tx.QueryRow("SELECT value FROM metadata WHERE key='inputRoot'").Scan(&binding); err != nil {
		return result, err
	}
	if err = checkStoredBinding(binding); err != nil {
		return result, err
	}
	err = tx.QueryRow("SELECT value FROM metadata WHERE key=?", historyKey).Scan(&prior)
	record := struct {
		Previous string `json:"previous"`
		Current  string `json:"current"`
	}{request.PreviousInputRoot, request.InputRoot}
	recorded, _ := json.Marshal(record)
	if err == nil {
		if prior != string(recorded) || binding != request.InputRoot {
			return result, errors.New("relocation ID or current binding changed")
		}
		result.AlreadyApplied = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if binding != request.PreviousInputRoot {
		return result, errors.New("previous input root does not match; no relocation was made")
	}
	var active int
	if err = tx.QueryRow("SELECT COUNT(*) FROM runs WHERE state IN ('queued','running')").Scan(&active); err != nil {
		return result, err
	}
	if active != 0 {
		return result, errors.New("finish queued and running evaluations before relocating the workspace")
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM triggers WHERE json_extract(record,'$.paused') IS NOT 1").Scan(&active); err != nil {
		return result, err
	}
	if active != 0 {
		return result, errors.New("pause automatic triggers before relocating the workspace")
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM occurrences WHERE state IN ('accepted','preparing','waiting','needs-attention')").Scan(&active); err != nil {
		return result, err
	}
	if active != 0 {
		return result, errors.New("finish or cancel pending source preparations before relocating the workspace")
	}
	if _, err = tx.Exec("UPDATE metadata SET value=? WHERE key='inputRoot' AND value=?", request.InputRoot, request.PreviousInputRoot); err != nil {
		return result, err
	}
	if _, err = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?)", historyKey, string(recorded)); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
