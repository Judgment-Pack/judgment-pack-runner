package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"time"
)

type GatewayConnection struct {
	Durable bool   `json:"durable,omitempty"`
	Profile string `json:"profile"`
	URL     string `json:"url"`
}

var connectionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validateBackgroundConfig(c Config) error {
	if len(c.CloudConnections) > 8 || len(c.GatewayConnections) > 32 {
		return errors.New("too many background connections")
	}
	seen := map[string]bool{}
	for _, v := range c.CloudConnections {
		if !connectionID.MatchString(v.ID) || !validSubscription(v.Subscription) || !filepath.IsAbs(v.CredentialsFile) || seen[v.ID] || seen[v.Subscription] {
			return errors.New("invalid cloud connection")
		}
		seen[v.ID] = true
		seen[v.Subscription] = true
	}
	seen = map[string]bool{}
	for _, v := range c.GatewayConnections {
		u, e := url.Parse(v.URL)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.ForceQuery || u.Opaque != "" || !(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) || seen[v.Profile] {
			return errors.New("invalid Gateway connection")
		}
		found := false
		for _, p := range c.InputProfiles {
			found = found || p.ID == v.Profile
		}
		if !found {
			return errors.New("Gateway connection needs an installed input profile")
		}
		seen[v.Profile] = true
	}
	return nil
}
func (s *Service) gatewayProfiles() []string {
	out := []string{}
	for _, g := range s.cfg.GatewayConnections {
		out = append(out, g.Profile)
	}
	return out
}
func (s *Service) validateGatewayMapping(r Release) error {
	if r.InputMapping == nil || r.InputMapping.Version != 2 {
		return bad("automatic_mapping_required", "Choose a version 2 mapping for connected acquisition.")
	}
	if e := preflightV2(*r.InputMapping, s.cfg.InputProfiles); e != nil {
		return e
	}
	for _, src := range r.InputMapping.Sources {
		if src.Kind == "selected-file" && src.Provider == "local-file" {
			continue
		}
		if src.Kind != "operation" {
			return bad("standing_grant_required", "Interactive file selection does not grant background access. Use a local file or an installed Gateway operation.")
		}
		found := false
		for _, g := range s.cfg.GatewayConnections {
			found = found || g.Profile == src.Profile
		}
		if !found {
			return bad("gateway_connection_required", "Install a persistent Gateway connection for every operation profile before enabling.")
		}
	}
	return nil
}

var automaticHTTP = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Gateway redirect refused") }}

func (s *Service) acquireAutomatic(parent context.Context, input Input) (Input, error) {
	if input.Source == nil {
		return input, bad("automatic_mapping_required", "Source inputs are required.")
	}
	if input.Source.Sources == nil {
		input.Source.Sources = map[string]SourceValue{}
	}
	if e := s.validateGatewayMapping(Release{InputMapping: &input.Source.Mapping}); e != nil {
		return input, e
	}
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	for step := 0; step <= len(input.Source.Mapping.Sources); step++ {
		plan, e := nextInput(input, s.cfg.InputProfiles, time.Now())
		if e != nil {
			return input, e
		}
		if plan.Input != nil {
			return input, nil
		} // retain original bytes, recompute preparation at admission
		next := plan.Next
		if next == nil || next.Kind != "operation" {
			return input, bad("automatic_source_unavailable", "A fresh local source is missing.")
		}
		gateway := ""
		for _, g := range s.cfg.GatewayConnections {
			if g.Profile == next.Profile {
				gateway = g.URL
			}
		}
		if gateway == "" {
			return input, bad("gateway_connection_required", "The Gateway connection is no longer available.")
		}
		session := id("job.")
		raw, e := automaticCall(ctx, gateway+"/acquire", encode(map[string]any{"session": session, "source": next.Source, "arguments": next.Arguments}), 1<<20)
		// Housekeeping is bounded and never retries the acquisition itself.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		_, _ = automaticCall(cleanup, gateway+"/seal", encode(map[string]string{"session": session}), 64<<10)
		stop()
		if e != nil {
			return input, e
		}
		var response struct {
			Receipt struct {
				Session string `json:"sessionId"`
				Index   int    `json:"callIndex"`
			} `json:"receipt"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Receipt.Session != session || response.Receipt.Index != 0 {
			return input, bad("gateway_receipt_mismatch", "The Gateway response does not belong to this acquisition.")
		}
		input.Source.Sources[next.Name] = SourceValue{Response: raw}
		if len(encode(input)) > MaxBody {
			return input, bad("input_too_large", "Acquired inputs exceed the combined input limit.")
		}
	}
	return input, bad("automatic_plan_failed", "The acquisition plan did not finish within its source limit.")
}
func automaticCall(ctx context.Context, target string, body []byte, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := automaticHTTP.Do(req)
	if e != nil {
		return nil, bad("gateway_unavailable", "Gateway acquisition failed or timed out. It was not retried.")
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil || int64(len(raw)) > limit {
		return nil, bad("gateway_response_too_large", "The Gateway response exceeded its limit.")
	}
	if res.StatusCode != 200 {
		return nil, bad("gateway_refused", "The Gateway refused the operation. Check the installed connection and provider access.")
	}
	return raw, nil
}
func (s *Service) startBackground(ctx context.Context) {
	for worker := 0; worker < 4; worker++ {
		s.background.Add(1)
		go func() {
			defer s.background.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				if e := s.prepareOccurrence(ctx); e != nil {
					s.unhealthy.Store(true)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	for _, c := range s.cfg.CloudConnections {
		s.background.Add(1)
		go func() { defer s.background.Done(); s.cloudWorker(ctx, c) }()
	}
}
func (s *Service) prepareLegacyOccurrence(ctx context.Context, occurrenceID string) error {
	s.automationMu.Lock()
	if s.unhealthy.Load() {
		s.automationMu.Unlock()
		return nil
	}
	var raw string
	e := s.db.QueryRow(`SELECT record FROM occurrences WHERE id=? AND state='accepted'`, occurrenceID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		s.automationMu.Unlock()
		return nil
	}
	if e != nil {
		s.automationMu.Unlock()
		return e
	}
	var o Occurrence
	if e = json.Unmarshal([]byte(raw), &o); e != nil {
		s.automationMu.Unlock()
		return e
	}
	expiry, e := time.Parse(time.RFC3339Nano, o.ExpiresAt)
	if e != nil {
		s.automationMu.Unlock()
		return e
	}
	if !time.Now().Before(expiry) {
		o.State = "expired"
		o.Reason = "queue-expired"
		o.PendingInput = nil
		e = s.saveOccurrence(o)
		s.automationMu.Unlock()
		return e
	}
	release, e := s.release(o.ReleaseID)
	if e != nil {
		s.automationMu.Unlock()
		return e
	}
	o.State = "preparing"
	e = s.saveOccurrence(o)
	s.automationMu.Unlock()
	if e != nil {
		return e
	}
	bounded, stop := context.WithDeadline(ctx, expiry)
	input, acquisitionError := s.automaticInputContext(bounded, o.PendingInput, release, nil)
	stop()
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	o.PendingInput = nil
	if acquisitionError != nil {
		o.State = "failed"
		o.Reason = acquisitionError.Error()
	} else if !time.Now().Before(expiry) {
		o.State = "expired"
		o.Reason = "queue-expired"
	} else {
		o.State = "accepted"
		o.Input = &input
		o.InputDigest = digest(encode(input))
	}
	return s.saveOccurrence(o)
}

func (s *Service) durableGatewayProfiles() []string {
	out := []string{}
	for _, g := range s.cfg.GatewayConnections {
		if g.Durable {
			out = append(out, g.Profile)
		}
	}
	return out
}
