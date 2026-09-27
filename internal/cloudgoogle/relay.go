package cloudgoogle

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Relay runs behind Cloud Run IAM. Only the scheduler service account may invoke
// it. The headers provide deduplication identity; Cloud Run supplies authentication.
func Relay(job string, publish func(context.Context, Signal) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/" || r.URL.RawQuery != "" {
			http.Error(w, "not found", 404)
			return
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 1025))
		if e != nil || len(body) > 1024 {
			http.Error(w, "invalid request", 400)
			return
		}
		names, times := r.Header.Values("X-CloudScheduler-JobName"), r.Header.Values("X-CloudScheduler-ScheduleTime")
		if !Job.MatchString(job) || len(names) != 1 || names[0] != job || len(times) != 1 {
			http.Error(w, "invalid scheduler identity", 403)
			return
		}
		at, e := time.Parse(time.RFC3339Nano, times[0])
		if e != nil || time.Until(at) > 30*time.Second {
			http.Error(w, "invalid scheduled time", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if publish(ctx, Signal{1, job, at.UTC().Format(time.RFC3339Nano)}) != nil {
			http.Error(w, "delivery unavailable", 503)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
