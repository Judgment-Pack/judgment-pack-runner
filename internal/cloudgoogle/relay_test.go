package cloudgoogle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelayStableSchedulerIdentity(t *testing.T) {
	job := "projects/example-project/locations/us-central1/jobs/daily"
	var got []Signal
	h := Relay(job, func(_ context.Context, s Signal) error { got = append(got, s); return nil })
	for _, at := range []string{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05+00:00"} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		r.Header.Set("X-CloudScheduler-JobName", job)
		r.Header.Set("X-CloudScheduler-ScheduleTime", at)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(got) != 2 || got[0] != got[1] {
		t.Fatal("retry identity changed", got)
	}
	for _, test := range []string{"wrong-job", "missing-time", "future", "too-large", "get", "query"} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		r.Header.Set("X-CloudScheduler-JobName", job)
		r.Header.Set("X-CloudScheduler-ScheduleTime", "2026-01-02T03:04:05Z")
		switch test {
		case "wrong-job":
			r.Header.Set("X-CloudScheduler-JobName", job+"x")
		case "missing-time":
			r.Header.Del("X-CloudScheduler-ScheduleTime")
		case "future":
			r.Header.Set("X-CloudScheduler-ScheduleTime", time.Now().Add(time.Hour).Format(time.RFC3339))
		case "too-large":
			r = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 1025)))
		case "get":
			r.Method = "GET"
		case "query":
			r.URL.RawQuery = "x=1"
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal(test, "accepted")
		}
	}
	if len(got) != 2 {
		t.Fatal("refusal published")
	}
}
func TestTransportBoundsAndAcknowledgement(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.URL.Path)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("content type")
		}
		if strings.HasSuffix(r.URL.Path, ":pull") {
			json.NewEncoder(w).Encode(map[string]any{"receivedMessages": []any{map[string]any{"ackId": "ack", "message": map[string]string{"data": "e30=", "messageId": "1"}}}})
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), endpoint: srv.URL + "/"}
	sub := "projects/example-project/subscriptions/desk-inbox"
	d, e := c.Pull(context.Background(), sub)
	if e != nil || len(d) != 1 {
		t.Fatal(d, e)
	}
	if len(methods) != 1 {
		t.Fatal("automatically acknowledged before storage")
	}
	if e = c.Ack(context.Background(), sub, d[0].AckID); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Pull(context.Background(), "../elsewhere"); e == nil {
		t.Fatal("accepted malformed resource")
	}
	if len(methods) != 2 {
		t.Fatal(methods)
	}
}
