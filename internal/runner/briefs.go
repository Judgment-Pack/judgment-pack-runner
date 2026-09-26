package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func (s *Service) briefSnapshot(kind, key string) (json.RawMessage, error) {
	var j Job
	var run *Run
	var err error
	if kind == "run" {
		r, e := s.run(key)
		if e != nil {
			return nil, e
		}
		if r.State == "queued" || r.State == "running" {
			return nil, &apiError{409, "run_pending", "Wait for the run to finish before generating its brief."}
		}
		run = &r
		j, err = s.job(r.JobID)
	} else {
		j, err = s.job(key)
	}
	if err != nil {
		return nil, err
	}
	release, err := s.release(j.ReleaseID)
	if err != nil {
		return nil, err
	}
	record := map[string]any{"job": j, "releaseId": release.ID, "packDigest": release.PackDigest, "runtimeDigest": release.RuntimeDigest, "sample": briefInput(release.Sample), "preview": release.Preview, "tests": release.Tests}
	if release.TestEvidence != nil {
		record["testEvidence"] = release.TestEvidence
	}
	if run != nil {
		var held map[string]any
		json.Unmarshal(encode(run), &held)
		held["input"] = briefInput(run.Input)
		record["run"] = held
	}
	return json.Marshal(map[string]any{"kind": kind, "title": j.Name, "pack": release.Pack, "record": record})
}
func (s *Service) briefHandler(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue(kind)
		subject := kind + ":" + key
		snapshot, err := s.briefSnapshot(kind, key)
		if err != nil {
			failure(w, err)
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			failure(w, err)
			return
		}
		defer tx.Rollback()
		d := BriefStore{Version: 1, Subjects: map[string]BriefHistory{}}
		var saved []byte
		err = tx.QueryRow("SELECT record FROM briefs WHERE subject=?", subject).Scan(&saved)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			failure(w, err)
			return
		}
		if len(saved) > 0 {
			if err = json.Unmarshal(saved, &d); err != nil {
				failure(w, err)
				return
			}
		}
		if r.Method == http.MethodPost {
			var c BriefCommand
			if err = decode(w, r, &c); err != nil {
				failure(w, err)
				return
			}
			if c.Subject != subject {
				failure(w, bad("invalid_subject", "This brief belongs to another subject."))
				return
			}
			if c.Action == "begin" {
				if !sameBriefJSON(c.Snapshot, snapshot) {
					failure(w, &apiError{409, "stale_source", "The retained source changed. Reload before generating."})
					return
				}
				c.Snapshot = snapshot
			}
			if err = applyBrief(&d, c, time.Now()); err != nil {
				failure(w, &apiError{409, "brief_conflict", err.Error()})
				return
			}
			data, _ := json.Marshal(d)
			if len(data) > 16<<20 {
				failure(w, bad("brief_limit", "Brief history reached its storage limit."))
				return
			}
			if _, err = tx.Exec("INSERT INTO briefs(subject,record) VALUES (?,?) ON CONFLICT(subject) DO UPDATE SET record=excluded.record", subject, data); err != nil {
				failure(w, err)
				return
			}
		}
		if err = tx.Commit(); err != nil {
			failure(w, err)
			return
		}
		write(w, 200, map[string]any{"content": d, "snapshot": snapshot})
	}
}
