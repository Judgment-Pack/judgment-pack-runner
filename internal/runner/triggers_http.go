package runner

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

func (s *Service) triggerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/occurrences/{occurrence}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		o, e := s.reconcileOccurrence(r.PathValue("occurrence"))
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, publicOccurrence(o))
	})
	mux.HandleFunc("POST /v1/occurrences/{occurrence}/cancel", func(w http.ResponseWriter, r *http.Request) {
		o, e := s.cancelOccurrence(r.Context(), r.PathValue("occurrence"))
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, publicOccurrence(o))
	})
	mux.HandleFunc("GET /v1/jobs/{job}/triggers", func(w http.ResponseWriter, r *http.Request) {
		if _, e := s.job(r.PathValue("job")); e != nil {
			failure(w, e)
			return
		}
		rows, e := s.db.Query("SELECT record,key_hash FROM triggers WHERE job_id=? ORDER BY seq", r.PathValue("job"))
		if e != nil {
			failure(w, e)
			return
		}
		defer rows.Close()
		items := []Trigger{}
		for rows.Next() {
			var raw, hash string
			var t Trigger
			if e = rows.Scan(&raw, &hash); e == nil {
				e = json.Unmarshal([]byte(raw), &t)
			}
			if e != nil {
				failure(w, e)
				return
			}
			t.HasKey = hash != ""
			if t.Config.Cloud != nil {
				if err := s.validateCloud(t.Config.Cloud); err != nil {
					t.Problem = err.Error()
				} else {
					s.cloudMu.Lock()
					t.Problem = s.cloudProblems[t.Config.Cloud.Connection]
					s.cloudMu.Unlock()
				}
			}
			items = append(items, t)
		}
		if e = rows.Err(); e != nil {
			failure(w, e)
			return
		}
		write(w, 200, map[string]any{"items": items, "localFiles": s.cfg.InputRoot != "", "cloudConnections": s.cloudStatus(), "gatewayProfiles": s.gatewayProfiles()})
	})
	mux.HandleFunc("POST /v1/jobs/{job}/triggers", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string        `json:"id,omitempty"`
			Revision int           `json:"revision,omitempty"`
			Config   TriggerConfig `json:"config"`
		}
		if e := decode(w, r, &req); e != nil {
			failure(w, e)
			return
		}
		t, e := s.configureTrigger(r.PathValue("job"), req.ID, req.Revision, req.Config)
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, t)
	})
	mux.HandleFunc("POST /v1/jobs/{job}/triggers/preview", func(w http.ResponseWriter, r *http.Request) {
		var c TriggerConfig
		if e := decode(w, r, &c); e != nil {
			failure(w, e)
			return
		}
		job, e := s.job(r.PathValue("job"))
		if e != nil {
			failure(w, e)
			return
		}
		release, e := s.release(job.ReleaseID)
		if e != nil {
			failure(w, e)
			return
		}
		if e = s.validateTrigger(&c, release, time.Now()); e != nil {
			failure(w, e)
			return
		}
		result := map[string]any{"config": c, "next": []string{}, "inputReady": false}
		if c.Schedule != nil {
			result["next"] = c.Schedule.preview(time.Now())
		}
		if c.Input != nil && c.Input.Kind == "mapped-sources" && s.durableMapping(release) {
			// Enabling must not wait for (or start) a multi-hour provider call.
			seed, e := s.automaticInputSeed(c.Input, release, nil)
			if e != nil {
				failure(w, e)
				return
			}
			if _, e = nextInput(seed, s.cfg.InputProfiles, time.Now()); e != nil {
				failure(w, e)
				return
			}
			result["configurationReady"] = true
			result["deferred"] = true
		} else if c.Input != nil {
			input, e := s.automaticInputContext(r.Context(), c.Input, release, nil)
			if e != nil {
				failure(w, e)
				return
			}
			normalized, e := s.normalizeInput(input)
			if e != nil {
				failure(w, e)
				return
			}
			result["inputReady"] = true
			result["factsText"] = string(normalized.Facts)
			result["evidenceText"] = string(normalized.Evidence)
		}
		write(w, 200, result)
	})
	mux.HandleFunc("POST /v1/triggers/{trigger}/state", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Revision int  `json:"revision"`
			Paused   bool `json:"paused"`
			Reviewed bool `json:"reviewed"`
		}
		if e := decode(w, r, &req); e != nil {
			failure(w, e)
			return
		}
		t, key, e := s.setTriggerState(r.PathValue("trigger"), req.Revision, req.Paused, req.Reviewed)
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, map[string]any{"trigger": t, "token": key})
	})
	mux.HandleFunc("POST /v1/triggers/{trigger}/rotate-key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Revision int `json:"revision"`
		}
		if e := decode(w, r, &req); e != nil {
			failure(w, e)
			return
		}
		t, key, e := s.rotateTriggerKey(r.PathValue("trigger"), req.Revision)
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, map[string]any{"trigger": t, "token": key})
	})
	mux.HandleFunc("POST /v1/triggers/{trigger}/events", func(w http.ResponseWriter, r *http.Request) {
		var delivery EventDelivery
		if e := decode(w, r, &delivery); e != nil {
			s.refusedEvent(r.PathValue("trigger"), r.Header.Get("X-Trigger-Token"), e)
			failure(w, e)
			return
		}
		o, replay, e := s.event(r.PathValue("trigger"), r.Header.Get("X-Trigger-Token"), delivery)
		if e != nil {
			s.refusedEvent(r.PathValue("trigger"), r.Header.Get("X-Trigger-Token"), e)
			failure(w, e)
			return
		}
		status := 202
		if replay {
			status = 200
		}
		write(w, status, map[string]any{"occurrence": o, "replayed": replay})
	})
	mux.HandleFunc("GET /v1/triggers/{trigger}/occurrences/{occurrence}", func(w http.ResponseWriter, r *http.Request) {
		answer, e := s.eventResult(r.PathValue("trigger"), r.PathValue("occurrence"), r.Header.Get("X-Trigger-Token"))
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, answer)
	})
	mux.HandleFunc("GET /v1/jobs/{job}/occurrences", func(w http.ResponseWriter, r *http.Request) {
		if _, e := s.job(r.PathValue("job")); e != nil {
			failure(w, e)
			return
		}
		after := int64(math.MaxInt64)
		if v := r.URL.Query().Get("after"); v != "" && v != "0" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 1 {
				failure(w, &apiError{400, "invalid_cursor", "Use a positive pagination cursor."})
				return
			}
			after = n
		}
		rows, e := s.db.Query("SELECT seq,record FROM occurrences WHERE job_id=? AND seq<? AND (?=0 OR (json_extract(record,'$.preparation') IS NOT NULL AND state<>'submitted')) ORDER BY seq DESC LIMIT 51", r.PathValue("job"), after, r.URL.Query().Get("preparations") == "1")
		if e != nil {
			failure(w, e)
			return
		}
		defer rows.Close()
		items := []Occurrence{}
		var last, next int64
		for rows.Next() {
			var seq int64
			var raw string
			if e = rows.Scan(&seq, &raw); e != nil {
				failure(w, e)
				return
			}
			if len(items) == 50 {
				next = last
				break
			}
			var o Occurrence
			if e = json.Unmarshal([]byte(raw), &o); e != nil {
				failure(w, e)
				return
			}
			o = publicOccurrence(o)
			items = append(items, o)
			last = seq
		}
		if e = rows.Err(); e != nil {
			failure(w, e)
			return
		}
		write(w, 200, map[string]any{"items": items, "next": next})
	})
}
