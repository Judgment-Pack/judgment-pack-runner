package runner

import (
	"encoding/json"
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
		// A brief reads the parsed record. Its bytes add nothing to read, and
		// would count against the snapshot's limit a second time; nor does the
		// record's signature sidecar.
		delete(held, "auditBytes")
		delete(held, "auditSignatures")
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
		// The command is read before the transaction, so that the
		// transaction reads nothing but the store and can be run again.
		var c BriefCommand
		if r.Method == http.MethodPost {
			if err = decode(w, r, &c); err != nil {
				failure(w, err)
				return
			}
		}
		var answer BriefStore
		err = s.store.update(func(tx runTx) error {
			d := BriefStore{Version: 1, Subjects: map[string]BriefHistory{}}
			saved, err := tx.brief(subject)
			if err != nil && !absent(err) {
				return err
			}
			if len(saved) > 0 {
				if err = json.Unmarshal(saved, &d); err != nil {
					return err
				}
			}
			if r.Method != http.MethodPost {
				answer = d
				return nil
			}
			if c.Subject != subject {
				return bad("invalid_subject", "This brief belongs to another subject.")
			}
			command := c
			if command.Action == "begin" {
				if !sameBriefJSON(command.Snapshot, snapshot) {
					return &apiError{409, "stale_source", "The retained source changed. Reload before generating."}
				}
				command.Snapshot = snapshot
			}
			if err = applyBrief(&d, command, time.Now()); err != nil {
				return &apiError{409, "brief_conflict", err.Error()}
			}
			data, _ := json.Marshal(d)
			if len(data) > 16<<20 {
				return bad("brief_limit", "Brief history reached its storage limit.")
			}
			if err = tx.putBrief(subject, data); err != nil {
				return err
			}
			answer = d
			return nil
		})
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, map[string]any{"content": answer, "snapshot": snapshot})
	}
}
