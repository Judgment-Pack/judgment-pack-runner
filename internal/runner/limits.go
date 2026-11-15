package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// The installation's limits (docs/design/schedules.md, section 9).
//
// An installation holds at most so many triggers, at most so many
// occurrences pending (accepted, preparing or waiting) at once, and at most
// so many runs queued or running at once, the run queue. A companion keeps
// the fixed defaults. In service mode the configuration's limits member may
// set any of them, up to its maximum: a service that runs one job for each
// of several hundred scope identifiers, each with a trigger on one shared
// interval, needs more than the defaults hold.

// The defaults and the maximums of the limits. The maximums were chosen
// from one tick of that many due triggers, and the dispatch of several
// hundred occurrences into runs, measured on one machine on both storage
// kinds (docs/design/schedules.md, section 8).
const (
	defaultTriggerLimit       = 128
	defaultPendingOccurrences = 100
	defaultRunQueue           = 100
	maxTriggerLimit           = 1000
	maxPendingOccurrences     = 2000
	maxRunQueue               = 2000
)

// LimitSettings are the operator's: the most triggers the installation
// holds, the most occurrences pending at once, and the most runs queued or
// running at once. The zero value of each is its default, 128, 100 and 100.
type LimitSettings struct {
	Triggers           int `json:"triggers,omitempty"`
	PendingOccurrences int `json:"pendingOccurrences,omitempty"`
	RunQueue           int `json:"runQueue,omitempty"`
}

// limitMembers are the limits member's members, in the order they are
// checked.
var limitMembers = []string{"triggers", "pendingOccurrences", "runQueue"}

// ParseLimitSettings reads a service configuration's limits member: one
// object of triggers, pendingOccurrences and runQueue, each a whole number
// from 1 to its maximum, each at most once and spelled exactly, none null,
// and nothing else. A member more is refused first; then the three are
// checked in this order, triggers, pendingOccurrences, runQueue, so that of
// several refusals the same one is answered every time.
func ParseLimitSettings(raw json.RawMessage) (LimitSettings, error) {
	refused := errors.New("limits, when present, is an object of triggers, pendingOccurrences and runQueue (whole numbers), each at most once, spelled so, and none null")
	members, err := exactMembers(raw)
	if err != nil {
		return LimitSettings{}, refused
	}
	for name := range members {
		if !slices.Contains(limitMembers, name) {
			return LimitSettings{}, refused
		}
	}
	var c LimitSettings
	for _, m := range []struct {
		name string
		to   *int
	}{{"triggers", &c.Triggers}, {"pendingOccurrences", &c.PendingOccurrences}, {"runQueue", &c.RunQueue}} {
		v, ok := members[m.name]
		if !ok {
			continue
		}
		if string(v) == "null" {
			return LimitSettings{}, refused
		}
		var n int
		if json.Unmarshal(v, &n) != nil {
			return LimitSettings{}, refused
		}
		if n < 1 {
			return LimitSettings{}, limitBounds(m.name)
		}
		*m.to = n
		if e := c.validate(); e != nil {
			return LimitSettings{}, e
		}
	}
	return c, nil
}

// limitBounds is the refusal of a limit out of its bounds.
func limitBounds(name string) error {
	most := maxTriggerLimit
	switch name {
	case "pendingOccurrences":
		most = maxPendingOccurrences
	case "runQueue":
		most = maxRunQueue
	}
	return fmt.Errorf("limits.%s is from 1 to %d", name, most)
}

// validate holds the settings to their bounds: zero is the default.
func (c LimitSettings) validate() error {
	if c.Triggers < 0 || c.Triggers > maxTriggerLimit {
		return limitBounds("triggers")
	}
	if c.PendingOccurrences < 0 || c.PendingOccurrences > maxPendingOccurrences {
		return limitBounds("pendingOccurrences")
	}
	if c.RunQueue < 0 || c.RunQueue > maxRunQueue {
		return limitBounds("runQueue")
	}
	return nil
}

// triggers is the most triggers the installation holds.
func (c LimitSettings) triggers() int {
	if c.Triggers == 0 {
		return defaultTriggerLimit
	}
	return c.Triggers
}

// pendingOccurrences is the most occurrences pending at once: the automatic
// queue.
func (c LimitSettings) pendingOccurrences() int {
	if c.PendingOccurrences == 0 {
		return defaultPendingOccurrences
	}
	return c.PendingOccurrences
}

// runQueue is the most runs queued or running at once: the run queue, which
// a run's submission, an occurrence's dispatch and a run's retry are held to.
func (c LimitSettings) runQueue() int {
	if c.RunQueue == 0 {
		return defaultRunQueue
	}
	return c.RunQueue
}

// status is the settings as GET /v1/status states them: the limits in
// force, the defaults included.
func (c LimitSettings) status() map[string]any {
	return map[string]any{"triggers": c.triggers(), "pendingOccurrences": c.pendingOccurrences(), "runQueue": c.runQueue()}
}

// errTriggerLimit is the refusal of a trigger past the installation's
// limit, which it states.
func errTriggerLimit(limit int) error {
	return bad("trigger_limit", fmt.Sprintf("This local runner supports up to %d triggers.", limit))
}

// errTriggerLimitHeld is the cause of the faultRefused a kind that holds the
// trigger count in a row reports when the row refuses a trigger past the
// limit (store_postgresql_writes.go): a creation that read the count below
// the limit, before another Runner's creation at the limit committed.
var errTriggerLimitHeld = errors.New("the installation holds its limit of triggers")

// triggerLimitHeld answers err, a trigger creation's failure, as the
// request is answered when its own read of the count finds the limit
// reached: the row's refusal is errTriggerLimit, stating limit, and any
// other failure is err itself.
func triggerLimitHeld(err error, limit int) error {
	if faultOf(err) == faultRefused && errors.Is(err, errTriggerLimitHeld) {
		return errTriggerLimit(limit)
	}
	return err
}
