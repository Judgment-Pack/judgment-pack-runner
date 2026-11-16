package runner

import (
	"bytes"
	"context"
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
	Durable             bool   `json:"durable,omitempty"`
	OperationsURL       string `json:"operationsUrl,omitempty"`
	OperationsTokenFile string `json:"operationsTokenFile,omitempty"`
	Profile             string `json:"profile"`
	URL                 string `json:"url"`
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
		if v.Durable {
			if !validGatewayOrigin(v.OperationsURL) || v.OperationsURL == v.URL {
				return errors.New("durable acquisition requires a separate caller-owned source worker")
			}
			if _, err := readPrivateToken(v.OperationsTokenFile); err != nil {
				return err
			}
		} else if v.OperationsURL != "" || v.OperationsTokenFile != "" {
			return errors.New("source worker settings require durable acquisition")
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
	if r.InputMapping == nil || !r.InputMapping.dialect() {
		return bad("automatic_mapping_required", "Choose a version 2 mapping for connected acquisition.")
	}
	if e := s.refuseSetAsideMapping(r.InputMapping); e != nil {
		return e
	}
	if e := preflightV2(*r.InputMapping, s.inputProfiles()); e != nil {
		return e
	}
	for _, src := range r.InputMapping.Sources {
		if src.Kind == "selected-file" && src.Provider == "local-file" {
			continue
		}
		if isResearch(src) {
			return bad("research_not_acquired", "A research source is prepared from the responses its caller retains. Runner asks no model and reads no page, and does not acquire one in the background.")
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
	return s.acquireAutomaticPaged(parent, input, &occurrencePaging{})
}

// acquireAutomaticPaged is acquireAutomatic with the paged sources' state p
// (paging_loop.go): a paged source's pages are asked in its loop, and an
// expired reference starts its read again from the cursor's first value.
func (s *Service) acquireAutomaticPaged(parent context.Context, input Input, p *occurrencePaging) (Input, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Input{}, manualUnsupported()
	}
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
	restarted := false
	for step := 0; step <= len(input.Source.Mapping.Sources); step++ {
		plan, e := s.nextInput(input, time.Now())
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
		var value SourceValue
		var err error
		if next.paged != nil {
			value, err = s.acquirePages(ctx, *next, p, combinedLimit(input)-len(encode(input)))
		} else {
			value, err = s.acquirePlannedOperation(ctx, *next)
		}
		if err != nil {
			// An expired reference starts a change cursor's read again
			// from its first value, once (paging_loop.go).
			if next.paged != nil && expiredReference(err) && p.restart(&input, next.Name) {
				restarted = true
				step--
				continue
			}
			// A failure after a call that answered is not retried as the
			// whole acquisition: it would call that call's source again.
			// A paged source's pages held for the next attempt are its
			// own calls alone, and only when no other call answered.
			if answer, ok := err.(*gatewayAnswer); ok && (step > 0 || restarted) {
				held := *answer
				held.afterAcquisition = true
				err = &held
				s.sealHeld(ctx, p.held)
				p.held = nil
			}
			return input, err
		}
		input.Source.Sources[next.Name] = value
		if len(encode(input)) > combinedLimit(input) {
			return input, bad("input_too_large", "Acquired inputs exceed the combined input limit.")
		}
	}
	return input, bad("automatic_plan_failed", "The acquisition plan did not finish within its source limit.")
}

// acquirePlannedOperation performs one fresh call. Its caller checkpoints the
// returned response before another request, and never retries an uncertain call.
func (s *Service) acquirePlannedOperation(parent context.Context, next PlannedSource) (SourceValue, error) {
	if s.cfg.ExecutorAdmission != nil {
		return SourceValue{}, manualUnsupported()
	}
	if e := next.ConversionContract.validate("preparation", "producer-carrier"); e != nil {
		return SourceValue{}, e
	}
	if next.RetainedColumn != (next.retainResponse && next.ConversionContract != nil) {
		return SourceValue{}, conversionFailure(codeMalformed)
	}
	if e := validateProducerLimits(next.RetainedColumn, next.ConversionContract, next.ConversionLimits); e != nil {
		return SourceValue{}, e
	}
	if next.Kind != "operation" {
		return SourceValue{}, bad("automatic_source_unavailable", "A fresh operation source is required.")
	}
	gateway := ""
	for _, g := range s.cfg.GatewayConnections {
		if g.Profile == next.Profile {
			gateway = g.URL
		}
	}
	if gateway == "" {
		return SourceValue{}, bad("gateway_connection_required", "The Gateway connection is no longer available.")
	}
	session := id("job.")
	raw, e := automaticCall(parent, gateway+"/acquire", encode(map[string]any{"session": session, "source": next.Source, "arguments": next.Arguments}), int64(min(artifactLimit(next.ConversionContract, 1<<20, columnResponseLimit), producerResponseLimit(next.ConversionContract, next.ConversionLimits))))
	// Housekeeping is bounded and never retries the acquisition itself.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	_, _ = automaticCall(cleanup, gateway+"/seal", encode(map[string]string{"session": session}), 64<<10)
	stop()
	if e != nil {
		return SourceValue{}, e
	}
	var response struct {
		Receipt struct {
			Session string `json:"sessionId"`
			Index   int    `json:"callIndex"`
		} `json:"receipt"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Receipt.Session != session || response.Receipt.Index != 0 {
		value := SourceValue{}
		if json.Valid(raw) {
			value.Response = raw
		}
		return value, bad("gateway_receipt_mismatch", "The Gateway response does not belong to this acquisition.")
	}
	if next.ConversionContract != nil && next.retainResponse {
		envelope, err := exactMembers(raw)
		if err != nil {
			return SourceValue{}, err
		}
		if _, err = readColumnCarrierLimits(envelope["result"], next.ConversionLimits, next.ConversionContract); err != nil {
			return SourceValue{}, err
		}
		return SourceValue{RetainedResponseJSON: string(raw)}, nil
	}
	return SourceValue{Response: raw}, nil
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
		// The status and Retry-After are kept for a job's retry policy
		// (retries.go); the error is the one it always was.
		answer := &gatewayAnswer{err: bad("gateway_refused", "The Gateway refused the operation. Check the installed connection and provider access."), status: res.StatusCode, retryAfter: res.Header.Get("Retry-After"), own: gatewayRefusal(res.Header.Get("Content-Type"), raw)}
		if answer.own {
			var body struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &body)
			answer.code = body.Code
		}
		return nil, answer
	}
	return raw, nil
}
func (s *Service) startBackground(ctx context.Context) {
	if s.cfg.ExecutorAdmission != nil {
		return
	}
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
				// A preparation whose write waited out the store's lock
				// bound made nothing, and is taken up again at a later
				// pass: the store is busy, not failing (occurrenceLost).
				if e := s.prepareOccurrence(ctx); e != nil && !onlyLockWaited(e) {
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
	if s.cfg.ExecutorAdmission != nil {
		return manualUnsupported()
	}
	s.automationMu.Lock()
	if s.unhealthy.Load() {
		s.automationMu.Unlock()
		return nil
	}
	// The occurrence is claimed, and prepared as the store holds it (D18):
	// one another Runner of the store holds, or that is no longer accepted,
	// is passed by. claimed, by the monotonic clock, is before the claim's
	// transaction began, as a run's (holdClaim).
	claimed := time.Now()
	o, e := s.claimOccurrence(occurrenceID, "accepted")
	if e != nil {
		s.automationMu.Unlock()
		return occurrenceLost(e)
	}
	// No error is returned with the claim held, as a dispatch returns none
	// (dispatchClaimed): an occurrence whose own record cannot be prepared
	// is failed (failHeld), its job or release record unreadable included,
	// and an error of the store's or of this Runner's releases the claim
	// first (releasedWith, releasedAsStored).
	expiry, e := time.Parse(time.RFC3339Nano, o.ExpiresAt)
	if e != nil {
		e = s.failHeld(o, "The accepted occurrence holds no readable expiry time.")
		s.automationMu.Unlock()
		return e
	}
	if !s.instant().Before(expiry) {
		// Pages held for an attempt that will not come are dropped
		// from the record, and their session sealed once the lock is
		// given up.
		held := o.HeldPages
		o.State = "expired"
		o.Reason = "queue-expired"
		o.PendingInput = nil
		o.NextAttemptAt = ""
		o.HeldPages = nil
		e = s.settleOccurrence(o)
		s.automationMu.Unlock()
		if e == nil {
			s.sealHeld(ctx, held)
		}
		return e
	}
	// An occurrence waiting for its next attempt is not attempted before
	// its time (retries.go), and its claim is released.
	waiting, e := waitingForAttempt(o, s.instant())
	if e != nil {
		e = s.releasedWith(o, e)
		s.automationMu.Unlock()
		return e
	}
	if waiting && leases(s.store) {
		e = s.settleOccurrence(o)
	}
	if e != nil || waiting {
		s.automationMu.Unlock()
		return e
	}
	policy, e := s.jobRetryPolicy(o.JobID)
	if e != nil {
		e = s.heldRecordOrReleased(o, e)
		s.automationMu.Unlock()
		return e
	}
	release, e := s.release(o.ReleaseID)
	if e != nil {
		e = s.heldRecordOrReleased(o, e)
		s.automationMu.Unlock()
		return e
	}
	// The job's cursors are read when the input is seeded (paging_loop.go),
	// and the pages an earlier attempt received before a throttle resumed.
	paging, e := s.occurrencePagingOf(o, policy != nil)
	if e != nil {
		e = s.heldRecordOrReleased(o, e)
		s.automationMu.Unlock()
		return e
	}
	o.State = "preparing"
	if policy != nil {
		openAttempt(&o, s.instant())
	}
	// The claim is kept while the sources are acquired, and renewed on a
	// kind that leases it: the acquisition is stopped, and nothing more of
	// the occurrence written, when a renewal does not succeed, as a run's
	// attempt is (claims.go). The occurrence is then failed one lease later
	// by the dispatcher's tick of a Runner of the store (interruptLapsed).
	e = s.keepOccurrence(o, anyState)
	if e != nil && !errors.Is(e, errPreparationStopped) {
		e = s.releasedAsStored(o, e)
	}
	s.automationMu.Unlock()
	if errors.Is(e, errPreparationStopped) {
		return nil
	}
	if e != nil {
		return e
	}
	held, releaseClaim := s.holdLease(ctx, claimed, func(tx runTx) error { return tx.renewOccurrenceClaim(o.ID, o.claim) })
	bounded, stop := context.WithDeadline(held, expiry)
	input, acquisitionError := s.automaticInputPaged(bounded, o.PendingInput, release, nil, paging)
	cut := bounded.Err() != nil
	stop()
	// Pages held for an attempt that will not come are sealed, after the
	// lock is given up.
	defer func() {
		if paging.held != nil {
			s.sealHeld(ctx, paging.held)
		}
	}()
	if _, e = releaseClaim(); e != nil {
		return e
	}
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	at := s.instant()
	ending := ""
	if policy != nil {
		var scheduled bool
		if scheduled, ending = closeAttempt(&o, *policy, acquisitionError, expiry, at, cut); scheduled {
			// Another attempt is scheduled: the occurrence waits for it
			// with its pending input, and the pages it holds.
			o.State = "accepted"
			o.HeldPages, paging.held = paging.held, nil
			return s.settleOccurrence(o)
		}
	}
	o.PendingInput = nil
	o.HeldPages = nil
	if acquisitionError != nil {
		o.State = "failed"
		o.Reason = acquisitionError.Error()
		o.ErrorClass = acquisitionClass(acquisitionError)
		if ending != "" {
			// The policy's code names its class (run_results.go).
			o.Reason, o.ErrorClass = ending, ""
		}
	} else if !at.Before(expiry) {
		o.State = "expired"
		o.Reason = "queue-expired"
	} else {
		o.State = "accepted"
		o.Input = &input
		o.InputDigest = digest(encode(input))
		o.Paging = paging.result()
	}
	return s.settleOccurrence(o)
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
