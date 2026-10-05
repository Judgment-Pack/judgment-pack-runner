package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Stored in the occurrence's SQLite record, atomically with its state. The
// process lock plus automationMu fences updates; cancelled/terminal records
// never accept a late completion. No source claim is reassigned to a new ID.
var errPreparationStopped = errors.New("preparation is no longer active")

type SourcePreparation struct {
	StartedAt string       `json:"startedAt"`
	Deadline  string       `json:"deadline"`
	NextCheck string       `json:"nextCheck,omitempty"`
	Tasks     []SourceTask `json:"tasks"`
	Input     *Input       `json:"input,omitempty"` // private checkpoint, removed from API
}
type SourceTask struct {
	Name          string          `json:"name"`
	ID            string          `json:"id"`
	State         string          `json:"state"`
	StartedAt     string          `json:"startedAt"`
	CompletedAt   string          `json:"completedAt,omitempty"`
	Checks        int             `json:"checks"`
	OperationsURL string          `json:"operationsUrl,omitempty"` // caller-worker binding, private
	Profile       string          `json:"profile,omitempty"`       // installation binding, private
	Gateway       string          `json:"gateway,omitempty"`       // installation binding, private
	Request       json.RawMessage `json:"request,omitempty"`       // frozen, private
}

func publicOccurrence(o Occurrence) Occurrence {
	o.Input = nil
	o.PendingInput = nil
	if o.Preparation != nil {
		p := *o.Preparation
		p.Input = nil
		p.Tasks = append([]SourceTask(nil), p.Tasks...)
		for i := range p.Tasks {
			p.Tasks[i].Gateway = ""
			p.Tasks[i].OperationsURL = ""
			p.Tasks[i].Profile = ""
			p.Tasks[i].Request = nil
		}
		o.Preparation = &p
	}
	return o
}
func (s *Service) occurrence(id string) (Occurrence, error) {
	var raw string
	var o Occurrence
	e := s.db.QueryRow("SELECT record FROM occurrences WHERE id=?", id).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &o)
	}
	return o, e
}
func (s *Service) durableMapping(r Release) bool {
	if r.InputMapping == nil || r.InputMapping.Version != 2 {
		return false
	}
	operations := 0
	for _, src := range r.InputMapping.Sources {
		if src.Kind != "operation" {
			continue
		}
		operations++
		found := false
		for _, g := range s.cfg.GatewayConnections {
			if g.Profile == src.Profile && g.Durable {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return operations > 0
}
func (s *Service) prepareOccurrence(ctx context.Context) error {
	s.automationMu.Lock()
	if s.unhealthy.Load() {
		s.automationMu.Unlock()
		return nil
	}
	rows, e := s.db.Query(`SELECT record FROM occurrences WHERE (state='accepted' AND json_extract(record,'$.pendingInput') IS NOT NULL) OR state='waiting' ORDER BY seq`)
	if e != nil {
		s.automationMu.Unlock()
		return e
	}
	var chosen *Occurrence
	if s.preparing == nil {
		s.preparing = map[string]bool{}
	}
	for rows.Next() {
		var raw string
		var o Occurrence
		if e = rows.Scan(&raw); e != nil {
			break
		}
		if e = json.Unmarshal([]byte(raw), &o); e != nil {
			break
		}
		if s.preparing[o.ID] {
			continue
		}
		if o.Preparation != nil && o.Preparation.NextCheck != "" {
			next, err := time.Parse(time.RFC3339Nano, o.Preparation.NextCheck)
			if err != nil {
				e = err
				break
			}
			if time.Now().Before(next) {
				continue
			}
		}
		chosen = &o
		break
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil || chosen == nil {
		s.automationMu.Unlock()
		return e
	}
	o := *chosen
	s.preparing[o.ID] = true
	s.automationMu.Unlock()
	defer func() { s.automationMu.Lock(); delete(s.preparing, o.ID); s.automationMu.Unlock() }()
	release, e := s.release(o.ReleaseID)
	if e != nil {
		return e
	}
	if o.Preparation == nil && !s.durableMapping(release) {
		return s.prepareLegacyOccurrence(ctx, o.ID)
	}
	s.automationMu.Lock()
	o, e = s.occurrence(o.ID)
	if e != nil {
		s.automationMu.Unlock()
		return e
	}
	if o.State != "accepted" && o.State != "waiting" {
		s.automationMu.Unlock()
		return nil
	}
	if o.Preparation == nil {
		expires, err := time.Parse(time.RFC3339Nano, o.ExpiresAt)
		if err != nil {
			s.automationMu.Unlock()
			return err
		}
		if !time.Now().Before(expires) {
			o.State = "expired"
			o.Reason = "queue-expired"
			o.PendingInput = nil
			e = s.saveOccurrence(o)
			s.automationMu.Unlock()
			return e
		}
		input, err := s.automaticInputSeed(o.PendingInput, release, nil)
		if err != nil {
			o.State = "failed"
			o.Reason = err.Error()
			o.PendingInput = nil
			e = s.saveOccurrence(o)
			s.automationMu.Unlock()
			return e
		}
		seconds := o.PreparationSeconds
		if seconds == 0 {
			seconds = 3600
		}
		o.Preparation = &SourcePreparation{StartedAt: now(), Deadline: time.Now().Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339Nano), Tasks: []SourceTask{}, Input: &input}
		o.State = "waiting"
	}
	o.Preparation.NextCheck = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	e = s.saveOccurrence(o)
	s.automationMu.Unlock()
	if e != nil {
		return e
	}
	e = s.advancePreparation(ctx, o)
	if errors.Is(e, errPreparationStopped) {
		return nil
	}
	return e
}

// savePreparation records a waiting occurrence's progress, and the entry of
// its change of state, if any, in one transaction that first reads the stored
// state: a cancellation or other final state wins.
func (s *Service) savePreparation(o Occurrence) error {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	return s.changeOccurrence(o, "waiting", runnerActor)
}
func (s *Service) stopPreparation(o Occurrence, state, reason string) error {
	o.State = state
	o.Reason = reason
	o.PendingInput = nil
	return s.savePreparation(o)
}
func (s *Service) advancePreparation(ctx context.Context, o Occurrence) error {
	p := o.Preparation
	deadline, e := time.Parse(time.RFC3339Nano, p.Deadline)
	if e != nil {
		return e
	}
	if !time.Now().Before(deadline) {
		if e = s.stopPreparation(o, "expired", "Source wait expired. No decision was evaluated."); e != nil {
			return e
		}
		s.cancelSource(ctx, o)
		return nil
	}
	if p.Input == nil {
		return errors.New("waiting occurrence has no input checkpoint")
	}
	plan, e := nextInput(*p.Input, s.cfg.InputProfiles, time.Now())
	if e != nil {
		return s.stopPreparation(o, "needs-attention", e.Error())
	}
	if plan.Input != nil {
		o.State = "accepted"
		o.PendingInput = nil
		o.Input = p.Input
		o.InputDigest = digest(encode(*p.Input))
		p.Input = nil
		o.Reason = ""
		seconds := o.QueueSeconds
		if seconds == 0 {
			seconds = 3600
		}
		o.ReadyUntil = time.Now().Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339Nano)
		return s.savePreparation(o)
	}
	next := plan.Next
	if next == nil || next.Kind != "operation" {
		return s.stopPreparation(o, "needs-attention", "A required source is unavailable.")
	}
	var task *SourceTask
	for i := range p.Tasks {
		if p.Tasks[i].Name == next.Name {
			task = &p.Tasks[i]
			break
		}
	}
	if task == nil {
		gateway, operationsURL := "", ""
		for _, g := range s.cfg.GatewayConnections {
			if g.Profile == next.Profile && g.Durable {
				gateway = g.URL
				operationsURL = g.OperationsURL
			}
		}
		if gateway == "" {
			return s.stopPreparation(o, "needs-attention", "The durable Gateway connection is no longer installed.")
		}
		operation := id("")
		p.Tasks = append(p.Tasks, SourceTask{Name: next.Name, ID: operation, State: "waiting", StartedAt: now(), Gateway: gateway, OperationsURL: operationsURL, Profile: next.Profile, Request: encode(map[string]any{"gateway": gateway, "id": operation, "source": next.Source, "arguments": next.Arguments, "deadline": p.Deadline})})
		task = &p.Tasks[len(p.Tasks)-1]
		if e = s.savePreparation(o); e != nil {
			return e
		} // commit intent BEFORE network
	}
	// An installation rebind cannot silently move an in-flight acquisition.
	installed := false
	tokenFile := ""
	for _, g := range s.cfg.GatewayConnections {
		if g.Durable && g.Profile == next.Profile && g.URL == task.Gateway && g.OperationsURL == task.OperationsURL && g.OperationsURL != "" {
			installed = true
			tokenFile = g.OperationsTokenFile
		}
	}
	if !installed {
		return s.stopPreparation(o, "needs-attention", "The acquisition connection changed. Restore it to reconcile this operation.")
	}
	var result struct {
		ID       string          `json:"id"`
		State    string          `json:"state"`
		Deadline string          `json:"deadline"`
		Response json.RawMessage `json:"response,omitempty"`
		Reason   string          `json:"reason,omitempty"`
	}
	raw, code, e := operationCall(ctx, task.OperationsURL+"/operations", task.Request, tokenFile)
	task.Checks++
	if e != nil || code == 429 || code >= 500 {
		if ctx.Err() != nil {
			return nil
		} // shutdown preserves the checkpoint
		o.Reason = "Waiting for the source worker connection. The same operation will be checked again."
		delay := time.Duration(3+task.Checks*2) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		p.NextCheck = time.Now().Add(delay).UTC().Format(time.RFC3339Nano)
		return s.savePreparation(o)
	}
	if code != 200 && code != 202 {
		return s.stopPreparation(o, "needs-attention", "The source worker refused the durable operation. It was not repeated with a new ID.")
	}
	if strictJSON(raw, &result) != nil || result.ID != task.ID || result.Deadline != p.Deadline {
		return s.stopPreparation(o, "needs-attention", "The source worker response does not match this request.")
	}
	o.Reason = ""
	if result.State == "queued" || result.State == "running" {
		task.State = result.State
		return s.savePreparation(o)
	}
	if result.State != "completed" {
		task.State = result.State
		return s.stopPreparation(o, "needs-attention", "The source needs attention. No decision was evaluated and the operation was not repeated.")
	}
	// The normal verifier checks source/profile pins, signature, freshness,
	// result digest and the commitment to the original frozen arguments.
	var binding struct {
		Receipt struct {
			Session string `json:"sessionId"`
			Index   int    `json:"callIndex"`
		} `json:"receipt"`
	}
	if json.Unmarshal(result.Response, &binding) != nil || binding.Receipt.Session != "async."+task.ID || binding.Receipt.Index != 0 {
		return s.stopPreparation(o, "needs-attention", "The source receipt does not belong to this operation.")
	}
	if p.Input.Source.Sources == nil {
		p.Input.Source.Sources = map[string]SourceValue{}
	}
	p.Input.Source.Sources[next.Name] = SourceValue{Response: result.Response}
	if len(encode(*p.Input)) > MaxBody {
		return s.stopPreparation(o, "needs-attention", "Acquired inputs exceed the combined input limit.")
	}
	if _, e = nextInput(*p.Input, s.cfg.InputProfiles, time.Now()); e != nil {
		return s.stopPreparation(o, "needs-attention", e.Error())
	}
	if !time.Now().Before(deadline) {
		return s.stopPreparation(o, "expired", "Source wait expired. A late result was not evaluated.")
	}
	task.State = "completed"
	task.CompletedAt = now()
	p.NextCheck = ""
	return s.savePreparation(o)
}

// Requests only exchange control-plane state. They never hold a worker for
// the duration of an acquisition. Retries always carry identical intent.
func operationCall(parent context.Context, target string, body []byte, tokenFile string) ([]byte, int, error) {
	ctx, stop := context.WithTimeout(parent, 3*time.Second)
	defer stop()
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if e != nil {
		return nil, 0, e
	}
	token, e := readPrivateToken(tokenFile)
	if e != nil {
		return nil, 0, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, e := automaticHTTP.Do(req)
	if e != nil {
		return nil, 0, e
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	if e == nil && len(raw) > MaxBody {
		e = errors.New("operation response too large")
	}
	return raw, res.StatusCode, e
}
func (s *Service) cancelSource(ctx context.Context, o Occurrence) {
	if o.Preparation == nil {
		return
	}
	for _, task := range o.Preparation.Tasks {
		if task.State != "completed" {
			for _, g := range s.cfg.GatewayConnections {
				if g.Durable && g.URL == task.Gateway && g.OperationsURL == task.OperationsURL && g.Profile == task.Profile {
					_, _, _ = operationCall(ctx, task.OperationsURL+"/operations/"+task.ID+"/cancel", []byte("{}"), g.OperationsTokenFile)
				}
			}
		}
	}
}
func (s *Service) cancelOccurrence(ctx context.Context, id string) (Occurrence, error) {
	s.automationMu.Lock()
	o, e := s.occurrence(id)
	if e == nil {
		if o.State != "waiting" && o.State != "needs-attention" {
			e = &apiError{409, "occurrence_not_waiting", "Only source preparation can be cancelled here."}
		} else {
			o.State = "cancelled"
			if o.Preparation != nil {
				for i := range o.Preparation.Tasks {
					if o.Preparation.Tasks[i].State != "completed" {
						o.Preparation.Tasks[i].State = "cancel-requested"
					}
				}
			}
			o.Reason = "Cancelled locally. Provider cancellation is best effort; late results will not be evaluated."
			o.PendingInput = nil
			e = s.changeOccurrence(o, "", s.installation())
		}
	}
	s.automationMu.Unlock()
	if e == nil {
		s.cancelSource(ctx, o)
	}
	return o, e
}

// Reconciliation reuses the original operation IDs. It never starts a new
// attempt or extends the deadline, and is explicitly requested by the owner.
func (s *Service) reconcileOccurrence(id string) (Occurrence, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	o, e := s.occurrence(id)
	if e != nil {
		return o, e
	}
	if o.State != "needs-attention" || o.Preparation == nil || o.Preparation.Input == nil {
		return o, &apiError{409, "not_reconcilable", "Only an interrupted source preparation can be checked again."}
	}
	deadline, e := time.Parse(time.RFC3339Nano, o.Preparation.Deadline)
	if e != nil {
		return o, e
	}
	if !time.Now().Before(deadline) {
		return o, &apiError{409, "preparation_expired", "The original source deadline has elapsed."}
	}
	o.State = "waiting"
	o.Reason = ""
	o.Preparation.NextCheck = ""
	return o, s.changeOccurrence(o, "", s.installation())
}
