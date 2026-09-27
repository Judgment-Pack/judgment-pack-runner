package runner

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
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
		run, err := s.run(r.PathValue("run"))
		if err != nil {
			failure(w, err)
			return
		}
		release, err := s.release(run.ReleaseID)
		if err != nil {
			failure(w, err)
			return
		}
		if run.Input.Preparation == nil {
			failure(w, bad("not_verified_mapping", "This run does not use mapping v2."))
			return
		}
		write(w, 200, VerificationBundle{Version: 2, Release: release, Run: run, ReleaseDigest: releaseDigest(release)})
	})
	mux.HandleFunc("GET /v1/jobs/{job}/briefs", s.briefHandler("job"))
	mux.HandleFunc("POST /v1/jobs/{job}/briefs", s.briefHandler("job"))
	mux.HandleFunc("GET /v1/runs/{run}/briefs", s.briefHandler("run"))
	mux.HandleFunc("POST /v1/runs/{run}/briefs", s.briefHandler("run"))
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"healthy": !s.unhealthy.Load(), "schemaVersion": "1", "workspace": s.cfg.Workspace, "owner": s.cfg.Owner, "runtimeDigest": s.runtimeDigest, "stateDirectory": s.cfg.Dir, "capabilities": []string{"single-pack", "manual", "api", "durable-history", "release-tests", "connected-inputs", "mapping-v2-mcp", "verified-input-lineage", "local-schedules", "event-delivery", "local-file-triggers", "google-cloud-triggers", "gateway-automatic-inputs"}})
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
			failure(w, bad("review_required", "Enter a job name and review its fixed release and sample result."))
			return
		}
		j, err := s.createJobConfigured(req.Name, req.ReleaseID, req.Trigger)
		if err != nil {
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
			failure(w, &apiError{400, "idempotency_key_required", "Supply an Idempotency-Key of 1 to 128 visible ASCII characters."})
			return
		}
		var input Input
		if err := decode(w, r, &input); err != nil {
			failure(w, err)
			return
		}
		run, replayed, err := s.submit(r.PathValue("job"), key, input)
		if err != nil {
			failure(w, err)
			return
		}
		status := 202
		if replayed {
			status = 200
		}
		write(w, status, run)
	})
	mux.HandleFunc("GET /v1/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		run, err := s.run(r.PathValue("run"))
		if err != nil {
			failure(w, err)
			return
		}
		write(w, 200, run)
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
