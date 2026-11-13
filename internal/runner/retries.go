package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Retries (docs/design/retries.md): a retry policy fixed on a job, which
// repeats an occurrence's source acquisition after a failure on a closed list,
// and the explicit retry of an interrupted run.

// retryStoreSchema is the schema of a store that holds a job with a retry
// policy or a run retried, raised in the transaction that creates the first
// or makes the first (raiseRetrySchema), from storeSchema or
// scopedStoreSchema. An earlier Runner would drop the policy, an occurrence's
// attempts and a run's attempts from every record it rewrites, and would
// evaluate a retried run into its first attempt's directory; every earlier
// Runner refuses a store at a schema not its own. A store that holds neither
// keeps its schema, byte for byte.
const retryStoreSchema = "6"

// raiseRetrySchema raises the store's schema to retryStoreSchema inside tx.
// A store at it already is left as it is.
func raiseRetrySchema(tx *sql.Tx) error {
	_, err := tx.Exec("UPDATE metadata SET value=? WHERE key='schema' AND value IN (?,?)", retryStoreSchema, storeSchema, scopedStoreSchema)
	return err
}

// attemptEntryVersion is the shape of an entry whose detail holds attempt,
// a member an entry of an earlier version never holds. Such an entry may
// also name a caller and a scope.
const attemptEntryVersion = "4"

// The bounds of a retry policy, and of a run's attempts.
const (
	maxRetryAttempts  = 10
	maxBackoffSeconds = 3600
	// retryWaitCap is the longest wait Runner takes before an attempt. A
	// longer one, the source's or the back-off's, fails the occurrence
	// rather than be clamped to an earlier attempt than the source asked.
	retryWaitCap = 3600 * time.Second
)

// The reasons an occurrence ends with, as codes, for a job with a policy.
const (
	occurrenceAttemptLimit = "attempt-limit-reached"
	occurrenceWaitTooLong  = "retry-wait-too-long"
)

// RetryPolicy is a job's retry policy: how many attempts an occurrence's
// source acquisition may make, the first among them, and the back-off
// between them. The wait before attempt n+1 is
// min(BackoffSeconds × 2^(n−1), MaxBackoffSeconds), and no shorter than the
// source's Retry-After. It is fixed when the job is created.
type RetryPolicy struct {
	MaxAttempts       int `json:"maxAttempts"`
	BackoffSeconds    int `json:"backoffSeconds"`
	MaxBackoffSeconds int `json:"maxBackoffSeconds"`
}

// sameRetryPolicy says whether two jobs' policies are the same, none alike.
func sameRetryPolicy(a, b *RetryPolicy) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// backoff is the wait before attempt made+1, after made attempts.
func (p RetryPolicy) backoff(made int) time.Duration {
	wait := p.BackoffSeconds
	for n := 1; n < made && wait < p.MaxBackoffSeconds; n++ {
		wait *= 2
	}
	return time.Duration(min(wait, p.MaxBackoffSeconds)) * time.Second
}

// Attempt is the record of one attempt: an occurrence's source acquisition,
// or a run's evaluation before the run was retried.
type Attempt struct {
	Number int `json:"number"`
	// DelaySeconds is the wait that preceded the attempt: 0 for the first,
	// and for an attempt a caller asked for.
	DelaySeconds int    `json:"delaySeconds"`
	StartedAt    string `json:"startedAt,omitempty"`
	EndedAt      string `json:"endedAt,omitempty"`
	// Outcome is succeeded, failed (not retried), transient (a failure on
	// the closed list, retried while attempts are left) or interrupted.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Status and RetryAfterSeconds are the gateway's answer, when it
	// answered one, and the wait its Retry-After asked for.
	Status            int `json:"status,omitempty"`
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
}

var errRetryPolicy = bad("invalid_retry_policy", "A retry policy names maxAttempts from 1 to 10, backoffSeconds from 1 to 3600, and maxBackoffSeconds from backoffSeconds to 3600, and nothing else.")

// jobRetry is the retry policy a job creation's body names, read from its
// bytes as jobScope reads the scope: a member whose name only folds to retry
// or to one of the policy's members is refused, so that a reader that looks
// for the members as written reads what Runner reads.
func jobRetry(raw []byte, policy *RetryPolicy) (*RetryPolicy, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil {
		return nil, errRetryPolicy
	}
	for name := range members {
		if name != "retry" && strings.EqualFold(name, "retry") {
			return nil, errRetryPolicy
		}
	}
	held, named := members["retry"]
	if !named {
		return nil, nil
	}
	var inner map[string]json.RawMessage
	if policy == nil || json.Unmarshal(held, &inner) != nil || len(inner) != 3 {
		return nil, errRetryPolicy
	}
	for _, name := range []string{"maxAttempts", "backoffSeconds", "maxBackoffSeconds"} {
		if _, ok := inner[name]; !ok {
			return nil, errRetryPolicy
		}
	}
	p := *policy
	if p.MaxAttempts < 1 || p.MaxAttempts > maxRetryAttempts || p.BackoffSeconds < 1 || p.BackoffSeconds > maxBackoffSeconds || p.MaxBackoffSeconds < p.BackoffSeconds || p.MaxBackoffSeconds > maxBackoffSeconds {
		return nil, errRetryPolicy
	}
	// One attempt is what a job without a policy makes: such a policy is
	// the default, and the job is created as without one, its record and
	// the store's schema as they were.
	if p.MaxAttempts == 1 {
		return nil, nil
	}
	return &p, nil
}

// gatewayAnswer is a gateway call's failure when the gateway answered it
// with a status other than 200: the error, whose sentence the occurrence's
// reason is as it always was, with the status and the answer's Retry-After.
// afterAcquisition says that an earlier call of the same acquisition had
// answered, so that repeating the acquisition would call its source again.
type gatewayAnswer struct {
	err              error
	status           int
	retryAfter       string
	afterAcquisition bool
	// own says the answer is in the gateway's own refusal form
	// (gatewayRefusal): one that something in front of the gateway made
	// may follow a call the gateway forwarded.
	own bool
}

// gatewayRefusal says whether an answer is in the form the gateway refuses
// a call in: a JSON body (application/json) that is an object whose error
// member is a string. A proxy's or a platform's answer is not, and a 503 of
// theirs may come after the gateway called its source.
func gatewayRefusal(contentType string, body []byte) bool {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || media != "application/json" {
		return false
	}
	var members map[string]json.RawMessage
	var text string
	return json.Unmarshal(body, &members) == nil && members["error"] != nil && json.Unmarshal(members["error"], &text) == nil
}

func (e *gatewayAnswer) Error() string { return e.err.Error() }
func (e *gatewayAnswer) Unwrap() error { return e.err }

// transient says whether a failure is on the closed list that a policy
// retries: the gateway itself refused the call 429 or 503, in its own form,
// which say it did not serve it, and no earlier call of the attempt had
// answered. A call that had no answer, or an answer not in the gateway's own
// form, may have reached its source, and is never repeated.
func transient(err error) (*gatewayAnswer, bool) {
	var g *gatewayAnswer
	if !errors.As(err, &g) {
		return nil, false
	}
	return g, g.own && !g.afterAcquisition && (g.status == 429 || g.status == 503)
}

// retryAfter reads a Retry-After value, delta-seconds or an HTTP-date, as a
// wait from at. A value of neither form, negative, or naming a time already
// past, is ignored: it says no wait.
func retryAfter(value string, at time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if value[0] >= '0' && value[0] <= '9' {
		for _, c := range value {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		if len(value) > 9 {
			// Far past the cap, which refuses it: no need to read it.
			return retryWaitCap + time.Second, true
		}
		seconds, _ := strconv.Atoi(value)
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(at) {
		return 0, false
	}
	// A date past the cap is read as just past it, before any arithmetic:
	// a far one would overflow a duration.
	if when.After(at.Add(retryWaitCap)) {
		return retryWaitCap + time.Second, true
	}
	return when.Sub(at), true
}

// instant is the Service's clock: time.Now, unless a test set another.
func (s *Service) instant() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// jobRetryPolicy is the retry policy of the occurrence's job, or nil.
func (s *Service) jobRetryPolicy(jobID string) (*RetryPolicy, error) {
	j, err := s.job(jobID)
	if err != nil {
		return nil, err
	}
	return j.Retry, nil
}

// waitingForAttempt says whether an occurrence holds a next attempt time
// still ahead of at.
func waitingForAttempt(o Occurrence, at time.Time) (bool, error) {
	if o.NextAttemptAt == "" {
		return false, nil
	}
	next, err := time.Parse(time.RFC3339Nano, o.NextAttemptAt)
	if err != nil {
		return false, err
	}
	return at.Before(next), nil
}

// closeAttempt ends an occurrence's open attempt, for a job with a policy,
// and decides what follows: another attempt, scheduled; or the occurrence's
// end, with the policy's reason code when the policy ended it, or none, when
// the occurrence ends as it would without a policy. cut says that the call
// was cut short by the occurrence's deadline or a stop.
func closeAttempt(o *Occurrence, p RetryPolicy, failure error, expiry, at time.Time, cut bool) (bool, string) {
	a := &o.Attempts[len(o.Attempts)-1]
	a.EndedAt = stamp(at)
	o.NextAttemptAt = ""
	if failure == nil {
		a.Outcome = "succeeded"
		return false, ""
	}
	answer, retried := transient(failure)
	var api *apiError
	if errors.As(failure, &api) {
		a.Reason = api.Code
	}
	if answer != nil {
		a.Status = answer.status
	}
	if !retried || cut {
		a.Outcome = "failed"
		return false, ""
	}
	a.Outcome = "transient"
	if a.Number >= p.MaxAttempts {
		return false, occurrenceAttemptLimit
	}
	wait := p.backoff(a.Number)
	if asked, ok := retryAfter(answer.retryAfter, at); ok {
		a.RetryAfterSeconds = int((asked + time.Second - 1) / time.Second)
		wait = max(wait, asked)
	}
	next := at.Add(wait)
	if wait > retryWaitCap || !next.Before(expiry) {
		return false, occurrenceWaitTooLong
	}
	o.NextAttemptAt = stamp(next)
	return true, ""
}

// openAttempt starts an occurrence's next attempt, for a job with a policy.
func openAttempt(o *Occurrence, at time.Time) {
	delay := 0
	if n := len(o.Attempts); n > 0 && o.NextAttemptAt != "" {
		if ended, err := time.Parse(time.RFC3339Nano, o.Attempts[n-1].EndedAt); err == nil {
			if next, err := time.Parse(time.RFC3339Nano, o.NextAttemptAt); err == nil && next.After(ended) {
				delay = int((next.Sub(ended) + time.Second - 1) / time.Second)
			}
		}
	}
	o.NextAttemptAt = ""
	o.Attempts = append(o.Attempts, Attempt{Number: len(o.Attempts) + 1, DelaySeconds: delay, StartedAt: stamp(at)})
}

// The reasons a run's kept attempt names.
const (
	runAttemptStopped   = "runner-stopped"
	runAttemptRestarted = "runner-restarted"
)

var errRunNotRetryable = &apiError{409, "run_not_retryable", "Only an interrupted run can be retried."}

// retryRunBy queues an interrupted run again, for by: the same run, with its
// interrupted attempt kept in its attempts (docs/design/retries.md, section
// 6). attempt is the run's attempt as the caller read it. It returns the run
// and whether the request repeated a retry already made. The run is read,
// checked and queued again in one transaction.
func (s *Service) retryRunBy(by requester, runID string, attempt int) (Run, bool, error) {
	if s.cfg.ExecutorAdmission != nil {
		return Run{}, false, manualUnsupported()
	}
	if s.unhealthy.Load() {
		return Run{}, false, &apiError{503, "dispatcher_stopped", "The dispatcher stopped after a storage error. Restart Desk after checking the runner storage."}
	}
	var result Run
	repeated := false
	err := s.store.update(func(tx runTx) error {
		r, err := tx.run(runID)
		if err != nil {
			return err
		}
		if replayed(r, attempt) {
			result, repeated = r, true
			return errKeepNothing
		}
		if attempt != r.Attempt {
			return &apiError{409, "attempt_mismatch", "This run's attempt is not the one named. Read the run again."}
		}
		if r.State != "interrupted" {
			return errRunNotRetryable
		}
		if len(r.Audit) > 0 || len(r.AuditBytes) > 0 {
			return errAuditKept
		}
		if err = s.attemptsHoldNoAudit(r); err != nil {
			return err
		}
		if r.Attempt >= maxRetryAttempts {
			return &apiError{409, "run_not_retryable", "This run has made its 10 attempts."}
		}
		if r.Trigger != nil && r.Trigger.ExpiresAt != "" {
			deadline, e := time.Parse(time.RFC3339Nano, r.Trigger.ExpiresAt)
			if e != nil || !s.instant().Before(deadline) {
				return &apiError{409, "run_not_retryable", "This automatic run's queue time has ended. It is not retried."}
			}
		}
		kept := Attempt{Number: r.Attempt, StartedAt: r.StartedAt, Outcome: "interrupted", Reason: runAttemptRestarted}
		if r.InterruptedAt != "" {
			kept.EndedAt, kept.Reason = r.InterruptedAt, runAttemptStopped
		}
		next := r
		next.Attempts = append(append([]Attempt(nil), r.Attempts...), kept)
		next.State, next.StartedAt, next.FinishedAt, next.Problem, next.InterruptedAt = "queued", "", "", "", ""
		count, err := tx.activeRuns()
		if err != nil {
			return err
		}
		if count >= queueLimit {
			return &apiError{429, "queue_full", "The local queue is full. Try again after pending runs finish."}
		}
		if err = tx.raiseSchema(retryStoreSchema); err != nil {
			return err
		}
		result = next
		return recordRun(tx, next, "interrupted", s.actor(by), EventDetail{Attempt: &kept})
	})
	if repeated && errors.Is(err, errKeepNothing) {
		return result, true, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return result, false, nil
}

var errAuditKept = &apiError{409, "run_not_retryable", "This interrupted run's attempt left an audit record. It is not evaluated again."}

// attemptsHoldNoAudit refuses a retry of a run any of whose attempts may have
// completed its evaluation: an attempt directory holding an audit trail with
// any content, or whose state cannot be read. A run found running at a
// restart, or whose end was not recorded, has no audit record in its record
// though its Runtime may have written one. An attempt directory that does not
// exist holds none.
func (s *Service) attemptsHoldNoAudit(r Run) error {
	if s.attempts == nil {
		return errAuditUnknown
	}
	for n := 1; n <= max(r.Attempt, 1); n++ {
		trail := filepath.Join(attemptDirectory(Run{ID: r.ID, Attempt: n}), "audit", "evaluations.jsonl")
		looked, err := s.attempts.Lstat(trail)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !looked.Mode().IsRegular() {
			return errAuditUnknown
		}
		if looked.Size() > 0 {
			return errAuditKept
		}
	}
	return nil
}

var errAuditUnknown = &apiError{409, "run_not_retryable", "Whether this interrupted run's attempt left an audit record cannot be read. It is not evaluated again."}

// replayed says whether attempt names an attempt of r already retried.
func replayed(r Run, attempt int) bool {
	for _, a := range r.Attempts {
		if a.Number == attempt {
			return true
		}
	}
	return false
}

// attemptDirectory is the directory, under attempts, of a run's current
// attempt: the run's id for the first, as it always was, and the id and the
// attempt's number for a later one. A run's id holds no dot.
func attemptDirectory(r Run) string {
	if r.Attempt <= 1 {
		return r.ID
	}
	return r.ID + "." + strconv.Itoa(r.Attempt)
}

func (s *Service) retryRunHandler(w http.ResponseWriter, r *http.Request) {
	by := requesterFrom(r.Context())
	runID := r.PathValue("run")
	var req struct {
		Attempt *int `json:"attempt"`
	}
	if err := decode(w, r, &req); err != nil || req.Attempt == nil || *req.Attempt < 0 {
		err = &apiError{400, "invalid_request", "Supply one bounded JSON request with supported fields and no duplicate members."}
		s.refusedRetryBy(by, runID, err)
		failure(w, err)
		return
	}
	run, repeated, err := s.retryRunBy(by, runID, *req.Attempt)
	if err != nil {
		s.refusedRetryBy(by, runID, err)
		failure(w, err)
		return
	}
	response, err := s.servedRun(run)
	if err != nil {
		failure(w, err)
		return
	}
	status := 202
	if repeated {
		status = 200
	}
	write(w, status, response)
}

// refusedRetryBy journals a refused retry of a run the store holds.
func (s *Service) refusedRetryBy(by requester, runID string, err error) {
	if r, e := s.store.run(runID); e == nil {
		s.refused("retry-run", err, s.actor(by), EventConcerns{Job: r.JobID, Release: r.ReleaseID, Run: r.ID}, r.Scope)
	}
}
