package runner

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/cloudgoogle"
	"strings"
	"time"
)

type CloudConnection struct {
	ID              string `json:"id"`
	Subscription    string `json:"subscription"`
	CredentialsFile string `json:"credentialsFile"`
}
type CloudBinding struct {
	Connection   string `json:"connection"`
	Subscription string `json:"subscription"`
	Job          string `json:"job"`
}
type cloudTransport interface {
	Pull(context.Context, string) ([]cloudgoogle.Delivery, error)
	Ack(context.Context, string, string) error
}

func validSubscription(s string) bool { return cloudgoogle.Subscription.MatchString(s) }
func (s *Service) validateCloud(b *CloudBinding) error {
	if b == nil || !cloudgoogle.Job.MatchString(b.Job) {
		return bad("invalid_cloud_binding", "Choose a Google Cloud connection and the full Scheduler job name.")
	}
	for _, c := range s.cfg.CloudConnections {
		if c.ID == b.Connection && c.Subscription == b.Subscription && strings.Split(c.Subscription, "/")[1] == strings.Split(b.Job, "/")[1] {
			return nil
		}
	}
	return bad("cloud_connection_unavailable", "The installed cloud connection does not match this trigger. Review the connection before enabling.")
}
func (s *Service) cloudStatus() []map[string]string {
	s.cloudMu.Lock()
	defer s.cloudMu.Unlock()
	out := []map[string]string{}
	for _, c := range s.cfg.CloudConnections {
		out = append(out, map[string]string{"id": c.ID, "subscription": c.Subscription, "problem": s.cloudProblems[c.ID]})
	}
	return out
}
func (s *Service) cloudProblem(id string, e error) {
	s.cloudMu.Lock()
	defer s.cloudMu.Unlock()
	s.cloudProblems[id] = ""
	if e != nil {
		s.cloudProblems[id] = e.Error()
	}
}
func (s *Service) cloudWorker(ctx context.Context, c CloudConnection) {
	var transport cloudTransport
	for ctx.Err() == nil {
		var e error
		if transport == nil {
			if s.cloudFactory != nil {
				transport, e = s.cloudFactory(ctx, c)
			} else {
				transport, e = cloudgoogle.New(ctx, c.CredentialsFile)
			}
		}
		if e == nil {
			var deliveries []cloudgoogle.Delivery
			deliveries, e = transport.Pull(ctx, c.Subscription)
			for _, d := range deliveries {
				ack, problem := s.receiveCloud(c, d)
				if ack {
					if err := transport.Ack(ctx, c.Subscription, d.AckID); err != nil {
						e = err
						break
					}
				}
				if problem != nil {
					e = problem
				}
			}
		}
		s.cloudProblem(c.ID, e)
		delay := 5 * time.Second
		if e != nil {
			delay = 15 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (s *Service) receiveCloud(c CloudConnection, d cloudgoogle.Delivery) (bool, error) {
	// A signal discarded is acknowledged, reported as the connection's
	// problem, and journaled as a refused admission.
	discard := func(t *Trigger, reason string) (bool, error) {
		s.refusedCloudSignal(c.ID, t, reason)
		return true, errors.New(reason)
	}
	if len(d.Message.Data) > 4096 {
		return discard(nil, "A cloud signal exceeded its size limit.")
	}
	raw, e := base64.StdEncoding.DecodeString(d.Message.Data)
	if e != nil || len(raw) > 2048 {
		return discard(nil, "A cloud signal was discarded because its envelope was invalid.")
	}
	var signal cloudgoogle.Signal
	if strictJSON(raw, &signal) != nil || signal.Version != 1 || !cloudgoogle.Job.MatchString(signal.Job) {
		return discard(nil, "A cloud signal was discarded because its identity was invalid.")
	}
	at, e := time.Parse(time.RFC3339Nano, signal.ScheduledAt)
	if e != nil || time.Until(at) > 30*time.Second || at.Year() < 2000 || at.Year() > 2200 {
		return discard(nil, "A cloud signal was discarded because its timestamp was invalid.")
	}
	signal.ScheduledAt = at.UTC().Format(time.RFC3339Nano)
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if s.unhealthy.Load() {
		return false, errors.New("Runner is unavailable; cloud delivery remains unacknowledged.")
	}
	var triggerID string
	e = s.db.QueryRow(`SELECT id FROM triggers WHERE json_extract(record,'$.config.cloud.connection')=? AND json_extract(record,'$.config.cloud.job')=?`, c.ID, signal.Job).Scan(&triggerID)
	if errors.Is(e, sql.ErrNoRows) {
		return discard(nil, "An unbound cloud signal was discarded. Check the Scheduler job name.")
	}
	if e != nil {
		return false, e
	}
	t, _, e := s.trigger(triggerID)
	if e != nil {
		return false, e
	}
	if t.Config.Kind != "cloud" || t.Config.Cloud.Subscription != c.Subscription {
		return discard(&t, "A signal from a replaced cloud connection was discarded.")
	}
	identity := "cloud:" + digest(encode(signal))
	if _, _, e = s.findOccurrence(t.ID, identity); e == nil {
		return true, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false, e
	}
	job, e := s.job(t.JobID)
	if e != nil {
		return false, e
	}
	o := newOccurrence(t, job, time.Now())
	o.EventID = identity
	o.ScheduledAt = signal.ScheduledAt
	o.ExpiresAt = at.Add(time.Duration(t.Config.QueueSeconds) * time.Second).Format(time.RFC3339Nano)
	if t.Paused {
		o.State = "skipped"
		o.Reason = "trigger-paused"
	} else if !time.Now().Before(at.Add(time.Duration(t.Config.QueueSeconds) * time.Second)) {
		o.State = "expired"
		o.Reason = "queue-expired"
	} else if t.Config.Missed == "skip" && time.Since(at) > 30*time.Second {
		o.State = "skipped"
		o.Reason = "missed"
	} else {
		o.PendingInput = t.Config.Input
	}
	tx, e := s.db.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if e = s.admitOccurrence(tx, t, &o, identity, digest(encode(signal))); e != nil {
		return false, e
	}
	e = tx.Commit()
	return e == nil, e
}
