package runner

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, err error) {
	status, code, message := 500, "store_error", "The local runner could not complete this request."
	var e *apiError
	if errors.As(err, &e) {
		status, code, message = e.Status, e.Code, e.Message
	} else if errors.Is(err, sql.ErrNoRows) {
		status, code, message = 404, "not_found", "This job, release or run was not found."
	}
	write(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "retryable": status == 429 || status == 503}})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil || strictJSON(raw, dst) != nil {
		return &apiError{400, "invalid_request", "Supply one bounded JSON request with supported fields and no duplicate members."}
	}
	return nil
}
func (s *Service) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	s.triggerRoutes(mux)
	mux.HandleFunc("GET /v1/background-connections", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"cloudConnections": s.cloudStatus(), "gatewayProfiles": s.gatewayProfiles(), "durableGatewayProfiles": s.durableGatewayProfiles()})
	})
	mux.HandleFunc("GET /v1/input-profiles", func(w http.ResponseWriter, r *http.Request) {
		profiles := []any{}
		for _, p := range s.cfg.InputProfiles {
			profiles = append(profiles, map[string]any{"profile": p, "digest": profileHash(p)})
		}
		write(w, 200, profiles)
	})
	mux.HandleFunc("GET /v1/runs/{run}/verification", func(w http.ResponseWriter, r *http.Request) {
		version, err := exportVersionAsked(r.URL.RawQuery)
		if err != nil {
			failure(w, err)
			return
		}
		run, err := s.run(r.PathValue("run"))
		if errors.Is(err, sql.ErrNoRows) {
			// A run the store no longer holds keeps its entry in the chain,
			// which still names it: say so, rather than that it never existed.
			if sequence, chained, e := s.runEntrySequence(r.PathValue("run")); e != nil {
				err = e
			} else if chained {
				err = &apiError{410, "run_not_held", fmt.Sprintf("This run was recorded at sequence %d of the installation's chain of runs, and the store no longer holds it.", sequence)}
			}
		}
		if err != nil {
			failure(w, err)
			return
		}
		release, err := s.release(run.ReleaseID)
		if err != nil {
			failure(w, err)
			return
		}
		var bundle VerificationBundle
		if version < 4 {
			bundle = verificationExport(release, run, version)
		} else {
			entry, err := s.runEntry(run.ID)
			if err == nil {
				bundle, err = chainedExport(release, run, entry)
			}
			if err != nil {
				failure(w, err)
				return
			}
			if version == 5 {
				bundle = signedExport(bundle, run)
			}
		}
		// A run of a job with no input mapping, or a v1 file mapping, has no
		// lineage, and is exported without it, saying so.
		if run.Input.Preparation == nil {
			if bundle, err = notMappedExport(bundle); err != nil {
				failure(w, err)
				return
			}
		}
		write(w, 200, bundle)
	})
	mux.HandleFunc("GET /v1/run-chain", s.runChainHandler)
	mux.HandleFunc("GET /v1/jobs/{job}/events", s.jobEventsHandler)
	mux.HandleFunc("GET /v1/events", s.eventsHandler)
	mux.HandleFunc("GET /v1/jobs/{job}/briefs", s.briefHandler("job"))
	mux.HandleFunc("POST /v1/jobs/{job}/briefs", s.briefHandler("job"))
	mux.HandleFunc("GET /v1/runs/{run}/briefs", s.briefHandler("run"))
	mux.HandleFunc("POST /v1/runs/{run}/briefs", s.briefHandler("run"))
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"healthy": !s.unhealthy.Load(), "schemaVersion": "1", "workspace": s.cfg.Workspace, "owner": s.cfg.Owner, "runtimeDigest": s.runtimeDigest, "stateDirectory": s.cfg.Dir, "capabilities": []string{"single-pack", "manual", "api", "durable-history", "release-tests", "connected-inputs", "mapping-v2-mcp", "verified-input-lineage", "local-schedules", "event-delivery", "local-file-triggers", "google-cloud-triggers", "gateway-automatic-inputs", "activity-journal"}})
	})
	mux.HandleFunc("POST /v1/inputs/next", func(w http.ResponseWriter, r *http.Request) {
		var input Input
		if err := decode(w, r, &input); err != nil {
			failure(w, err)
			return
		}
		plan, err := nextInput(input, s.cfg.InputProfiles, time.Now().UTC().Truncate(time.Second))
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, plan)
	})
	mux.HandleFunc("POST /v1/inputs/preview", func(w http.ResponseWriter, r *http.Request) {
		var input Input
		if err := decode(w, r, &input); err != nil {
			failure(w, err)
			return
		}
		mapped, err := s.normalizeInput(input)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, map[string]any{"input": mapped, "factsText": inputJSONText(mapped.Facts), "evidenceText": inputJSONText(mapped.Evidence)})
	})
	mux.HandleFunc("POST /v1/previews", func(w http.ResponseWriter, r *http.Request) {
		var req PreviewRequest
		if err := decode(w, r, &req); err != nil {
			failure(w, err)
			return
		}
		release, err := s.preview(r.Context(), req)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 201, release)
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name      string         `json:"name"`
			ReleaseID string         `json:"releaseId"`
			Reviewed  bool           `json:"reviewed"`
			Trigger   *TriggerConfig `json:"trigger,omitempty"`
		}
		if err := decode(w, r, &req); err != nil {
			failure(w, err)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || len(req.Name) > 160 || !req.Reviewed {
			err := bad("review_required", "Enter a job name and review its fixed release and sample result.")
			s.refusedJob(req.ReleaseID, err)
			failure(w, err)
			return
		}
		j, err := s.createJobConfigured(req.Name, req.ReleaseID, req.Trigger)
		if err != nil {
			s.refusedJob(req.ReleaseID, err)
			failure(w, err)
			return
		}
		write(w, 201, j)
	})
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) { s.list(w, r, "runs", "") })
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) { s.list(w, r, "jobs", "") })
	mux.HandleFunc("GET /v1/jobs/{job}", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.job(r.PathValue("job"))
		if err != nil {
			failure(w, err)
			return
		}
		release, err := s.release(j.ReleaseID)
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, map[string]any{"job": j, "release": release})
	})
	mux.HandleFunc("GET /v1/jobs/{job}/runs", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.job(r.PathValue("job")); err != nil {
			failure(w, err)
			return
		}
		s.list(w, r, "runs", r.PathValue("job"))
	})
	mux.HandleFunc("POST /v1/jobs/{job}/runs", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
			err := &apiError{400, "idempotency_key_required", "Supply an Idempotency-Key of 1 to 128 visible ASCII characters."}
			s.refusedRun(r.PathValue("job"), err)
			failure(w, err)
			return
		}
		var input Input
		if err := decode(w, r, &input); err != nil {
			s.refusedRun(r.PathValue("job"), err)
			failure(w, err)
			return
		}
		run, replayed, err := s.submit(r.PathValue("job"), key, input)
		if err != nil {
			s.refusedRun(r.PathValue("job"), err)
			failure(w, err)
			return
		}
		status := 202
		if replayed {
			status = 200
		}
		write(w, status, storedRun{run, run.InterruptedAt})
	})
	mux.HandleFunc("GET /v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		run, err := s.run(r.PathValue("run"))
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, storedRun{run, run.InterruptedAt})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(token)) != 1 {
			write(w, 401, map[string]string{"message": "Runner authentication required."})
			return
		}
		// Browser requests go through Desk's same-origin/session guard. This private
		// control listener does not expose cookies, CORS or browser authority.
		if r.Header.Get("Origin") != "" {
			write(w, 403, map[string]string{"message": "Use the authenticated Desk API."})
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Service) list(w http.ResponseWriter, r *http.Request, kind, job string) {
	after := int64(0)
	var err error
	if q := r.URL.Query().Get("after"); q != "" {
		after, err = strconv.ParseInt(q, 10, 64)
	}
	if err != nil || after < 0 {
		failure(w, &apiError{400, "invalid_cursor", "Invalid page cursor."})
		return
	}
	filter := recordFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), State: r.URL.Query().Get("state"), Review: r.URL.Query().Get("review") == "true"}
	if len(filter.Search) > 200 || (filter.State != "" && !validRunState(filter.State)) || (r.URL.Query().Get("review") != "" && r.URL.Query().Get("review") != "true" && r.URL.Query().Get("review") != "false") || (kind == "jobs" && (filter.State != "" || filter.Review)) {
		failure(w, &apiError{400, "invalid_filter", "Use a search up to 200 bytes and supported run filters."})
		return
	}
	items, next, err := s.filteredRecords(kind, job, after, filter)
	if err != nil {
		failure(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "next": next})
}

// runChainHandler serves the installation's chain of runs: every entry's line,
// exactly as stored, each ended by a newline, in sequence order. That is a
// trail file as the Runtime reads one, which verify-run --chain reads too. It
// is read in pages, so a slow reader never holds the store's one connection.
// Rows are only ever appended, so the answer is the chain as it stood at some
// moment during the reading. A page that cannot be read after the answer has
// begun aborts it, so a reader sees a failed transfer, never a shorter chain.
func (s *Service) runChainHandler(w http.ResponseWriter, r *http.Request) {
	const page = 1000
	lines, last, err := s.chainPage(0, page)
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/jsonl")
	w.WriteHeader(200)
	for {
		var out bytes.Buffer
		for _, line := range lines {
			out.Write(line)
			out.WriteByte('\n')
		}
		if _, err = w.Write(out.Bytes()); err != nil {
			return
		}
		if len(lines) < page {
			return
		}
		if lines, last, err = s.chainPage(last, page); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
