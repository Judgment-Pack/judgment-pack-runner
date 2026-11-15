package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Shared intervals and cron bounds (docs/design/schedules.md).
//
// An operator names a closed set of shared intervals, and the bounds every
// cron expression is held to, in a service configuration's schedules member.
// A schedule trigger of kind "tick" names one of the intervals; an external
// scheduler calls POST /v1/ticks once per interval, and that one call ticks
// every enabled trigger of every scope bound to the interval. The Runner's
// own clock never ticks such a trigger.

// scheduleStoreSchema is the schema of a store that holds a trigger with a
// cron or a shared-interval schedule, raised in the transaction that writes
// the first (raiseSchema), from storeSchema, scopedStoreSchema or
// retryStoreSchema. An earlier Runner would read such a schedule as one of
// a kind it does not know: its own clock would tick a shared-interval
// trigger, and it would drop the expression and the interval from every
// trigger it rewrites; every earlier Runner refuses a store at a schema not
// its own. A store that holds neither keeps its schema, byte for byte.
const scheduleStoreSchema = "7"

// The bounds of the settings.
const (
	// maxSharedIntervals is the most shared intervals an operator names.
	maxSharedIntervals = 16
	// cronFloorSeconds is the shortest gap a cron expression may have, its
	// own granularity, and the default minimum.
	cronFloorSeconds = 60
	// cronCeilingSeconds is the longest gap a cron expression may have, 366
	// days, and the default maximum.
	cronCeilingSeconds = 366 * 86400
)

// ScheduleSettings are the operator's: the shared intervals the Runner
// ticks on a call (each named as 15m, 1h or 1d), and the shortest and the
// longest gap a cron expression may have between two matches. The zero
// value names no shared interval and holds cron expressions to one minute
// and 366 days.
type ScheduleSettings struct {
	SharedIntervals []string `json:"sharedIntervals,omitempty"`
	MinimumSeconds  int      `json:"minimumSeconds,omitempty"`
	MaximumSeconds  int      `json:"maximumSeconds,omitempty"`
}

// ParseScheduleSettings reads a service configuration's schedules member:
// one object of sharedIntervals, a list of interval names, and
// minimumSeconds and maximumSeconds, each at most once and spelled exactly,
// none null, and nothing else.
func ParseScheduleSettings(raw json.RawMessage) (ScheduleSettings, error) {
	refused := errors.New(`schedules, when present, is an object of sharedIntervals (a list of names such as "15m", "1h" or "1d"), minimumSeconds and maximumSeconds (whole numbers of seconds), each at most once, spelled so, and none null`)
	members, err := exactMembers(raw)
	if err != nil {
		return ScheduleSettings{}, refused
	}
	var c ScheduleSettings
	for name, v := range members {
		if string(v) == "null" {
			return ScheduleSettings{}, refused
		}
		switch name {
		case "sharedIntervals":
			err = json.Unmarshal(v, &c.SharedIntervals)
		case "minimumSeconds", "maximumSeconds":
			var n int
			if err = json.Unmarshal(v, &n); err == nil && (n < cronFloorSeconds || n > cronCeilingSeconds) {
				return ScheduleSettings{}, fmt.Errorf("schedules.%s is from %d to %d", name, cronFloorSeconds, cronCeilingSeconds)
			}
			if name == "minimumSeconds" {
				c.MinimumSeconds = n
			} else {
				c.MaximumSeconds = n
			}
		default:
			return ScheduleSettings{}, refused
		}
		if err != nil {
			return ScheduleSettings{}, refused
		}
	}
	return c, c.validate()
}

// validate holds the settings to their bounds.
func (c ScheduleSettings) validate() error {
	if len(c.SharedIntervals) > maxSharedIntervals {
		return fmt.Errorf("schedules names more than %d shared intervals", maxSharedIntervals)
	}
	spans := map[time.Duration]bool{}
	for _, name := range c.SharedIntervals {
		span, ok := sharedIntervalSpan(name)
		if !ok {
			return errors.New("a shared interval is named as a number of minutes (m) or hours (h), or 1d, from one minute to one day, that divides a day evenly, as 5m, 15m, 1h or 1d")
		}
		if spans[span] {
			return errors.New("schedules names one shared interval twice")
		}
		spans[span] = true
	}
	if c.MinimumSeconds != 0 && (c.MinimumSeconds < cronFloorSeconds || c.MinimumSeconds > cronCeilingSeconds) || c.MaximumSeconds != 0 && (c.MaximumSeconds < cronFloorSeconds || c.MaximumSeconds > cronCeilingSeconds) {
		return fmt.Errorf("minimumSeconds and maximumSeconds are from %d to %d", cronFloorSeconds, cronCeilingSeconds)
	}
	if c.minimum() > c.maximum() {
		return errors.New("minimumSeconds is above maximumSeconds")
	}
	return nil
}

func (c ScheduleSettings) minimum() time.Duration {
	if c.MinimumSeconds == 0 {
		return cronFloorSeconds * time.Second
	}
	return time.Duration(c.MinimumSeconds) * time.Second
}

func (c ScheduleSettings) maximum() time.Duration {
	if c.MaximumSeconds == 0 {
		return cronCeilingSeconds * time.Second
	}
	return time.Duration(c.MaximumSeconds) * time.Second
}

// status is the settings as GET /v1/status states them: the shared
// intervals, and the bounds in force, the defaults included.
func (c ScheduleSettings) status() map[string]any {
	return map[string]any{"sharedIntervals": append([]string{}, c.SharedIntervals...), "minimumSeconds": int(c.minimum() / time.Second), "maximumSeconds": int(c.maximum() / time.Second)}
}

// shares says whether the operator named the shared interval name.
func (c ScheduleSettings) shares(name string) bool {
	for _, n := range c.SharedIntervals {
		if n == name {
			return true
		}
	}
	return false
}

// sharedIntervalSpan is the length of the shared interval name: a number of
// minutes or hours with no sign and no leading zero, or 1d, from one minute
// to one day, that divides a day evenly, so that its ticks fall on the same
// times every day, counted from midnight UTC.
func sharedIntervalSpan(name string) (time.Duration, bool) {
	if len(name) < 2 || len(name) > 5 || name[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(name[:len(name)-1])
	if err != nil || n < 1 || strconv.Itoa(n) != name[:len(name)-1] {
		return 0, false
	}
	var span time.Duration
	switch name[len(name)-1] {
	case 'm':
		span = time.Duration(n) * time.Minute
	case 'h':
		span = time.Duration(n) * time.Hour
	case 'd':
		if n != 1 {
			return 0, false
		}
		span = 24 * time.Hour
	default:
		return 0, false
	}
	if span < time.Minute || span > 24*time.Hour || (24*time.Hour)%span != 0 {
		return 0, false
	}
	return span, true
}

// checkSchedule holds a cron or a shared-interval schedule to the
// operator's settings: a shared interval the operator names, and a cron
// expression whose matches are no closer than the minimum (and, on a shared
// interval, than the interval) and no further apart than the maximum. Each
// refusal is its own code.
func (c ScheduleSettings) checkSchedule(sc *Schedule) error {
	floor := c.minimum()
	switch sc.Kind {
	case "tick":
		if !c.shares(sc.Tick) {
			return bad("unknown_shared_interval", "This Runner's operator names no such shared interval.")
		}
		if sc.Cron == "" {
			return nil
		}
		// A match closer to the last than one interval would share its
		// tick: the interval is the shortest schedule it honours.
		if span, _ := sharedIntervalSpan(sc.Tick); span > floor {
			floor = span
		}
	case "cron":
	default:
		return nil
	}
	expr, e := parseCron(sc.Cron)
	if e != nil {
		return e
	}
	shortest, longest, matches := expr.gaps()
	if !matches {
		return bad("cron_never_matches", "This cron expression matches no date.")
	}
	if shortest < floor {
		return bad("cron_interval_too_short", fmt.Sprintf("Two matches of this cron expression are closer than %d seconds, the shortest gap this schedule takes.", int(floor/time.Second)))
	}
	if longest > c.maximum() {
		return bad("cron_interval_too_long", fmt.Sprintf("Two matches of this cron expression are further apart than %d seconds, the longest gap this Runner's operator allows.", int(c.maximum()/time.Second)))
	}
	return nil
}

// needsScheduleSchema says whether a trigger configured so needs
// scheduleStoreSchema.
func (c *TriggerConfig) needsScheduleSchema() bool {
	return c != nil && c.Kind == "schedule" && c.Schedule != nil && (c.Schedule.Kind == "cron" || c.Schedule.Kind == "tick")
}

// sharedTicked says whether t is a schedule trigger bound to the shared
// interval name: only that interval's calls tick it.
func sharedTicked(t Trigger, name string) bool {
	return t.Config.Kind == "schedule" && t.Config.Schedule != nil && t.Config.Schedule.Kind == "tick" && (name == "" || t.Config.Schedule.Tick == name)
}

// sharedTickLead is how far ahead of the Runner's clock a tick may be named,
// so that a scheduler whose clock runs a little ahead names the tick it
// means. A call that names none ticks the last tick at or before the
// Runner's clock plus the lead.
const sharedTickLead = 30 * time.Second

// TickRequest is the body of POST /v1/ticks: the shared interval, and
// optionally the tick, a multiple of the interval since midnight UTC, no
// more than sharedTickLead ahead of the Runner's clock.
type TickRequest struct {
	Interval string `json:"interval"`
	At       string `json:"at,omitempty"`
}

// TickResult answers a tick: the tick, and the occurrences this call
// recorded, accepted or skipped. A call that repeats a tick records none.
type TickResult struct {
	Interval    string `json:"interval"`
	At          string `json:"at"`
	Occurrences int    `json:"occurrences"`
}

// sharedTick ticks, at the tick req names, every enabled trigger of every
// scope bound to req's shared interval. Each trigger due at the tick is
// ticked as the Runner's own clock ticks a schedule (scheduleDue): it
// admits its due time's occurrence and moves its next time on in one
// transaction, which expects the trigger's row version (D17), so that a
// repeated or late call, here or on another Runner of the store, admits
// nothing twice.
func (s *Service) sharedTick(req TickRequest) (TickResult, error) {
	if s.cfg.ExecutorAdmission != nil {
		return TickResult{}, manualUnsupported()
	}
	span, ok := sharedIntervalSpan(req.Interval)
	if !ok || !s.cfg.Schedules.shares(req.Interval) {
		return TickResult{}, bad("unknown_shared_interval", "This Runner's operator names no such shared interval.")
	}
	now := time.Now()
	tick := now.Add(sharedTickLead).Truncate(span)
	if req.At != "" {
		at, e := time.Parse(time.RFC3339, req.At)
		if e != nil || at.Year() < 2000 || at.Year() > 2200 || !at.Truncate(span).Equal(at) || at.Sub(now) > sharedTickLead {
			return TickResult{}, bad("invalid_tick", "Name a tick of the interval, a multiple of it since midnight UTC, no more than 30 seconds ahead of the Runner's clock.")
		}
		tick = at
	}
	tick = tick.UTC()
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if s.unhealthy.Load() {
		return TickResult{}, &apiError{503, "runner_unavailable", "The runner requires a restart before it ticks."}
	}
	ids, e := s.store.enabledTriggers()
	if e != nil {
		return TickResult{}, e
	}
	result := TickResult{Interval: req.Interval, At: tick.Format(time.RFC3339)}
	for _, id := range ids {
		t, hash, e := s.trigger(id)
		if absent(e) {
			continue
		}
		if e != nil {
			return result, e
		}
		if t.Paused || !sharedTicked(t, req.Interval) {
			continue
		}
		made, e := s.scheduleDue(t, hash, tick, now, 0)
		if e != nil {
			return result, e
		}
		if made {
			result.Occurrences++
		}
	}
	return result, nil
}

// sharedTickRoutes serves POST /v1/ticks. It is the installation's: it
// takes no scope (scoped refuses one), and ticks the triggers of every
// scope on the store for every scope, as the Runner's own clock does
// (docs/design/store-postgresql.md, section 6). The control API's
// credential alone admits it.
func (s *Service) sharedTickRoutes(mux routeMux) {
	mux.HandleFunc("POST /v1/ticks", func(w http.ResponseWriter, r *http.Request) {
		var req TickRequest
		if e := decode(w, r, &req); e != nil {
			failure(w, e)
			return
		}
		result, e := s.sharedTick(req)
		if e != nil {
			failure(w, e)
			return
		}
		write(w, 200, result)
	})
}
