//go:build linux || darwin || freebsd

package main

import (
	"strings"
	"testing"
)

// The installation's limits in a service configuration
// (docs/design/schedules.md, section 9): the limits member sets the trigger
// limit and the pending-occurrence limit, each a whole number from 1 to its
// maximum; anything else is refused before anything is opened, and without
// the member, or without one of the two, the defaults hold.
func TestAServiceConfigurationSetsTheInstallationsLimits(t *testing.T) {
	f := newServiceFixture(t)
	limits := func(name string, v any) string {
		return f.config(name, f.members("127.0.0.1:8470", map[string]any{"limits": v}))
	}
	c, err := readServiceConfig(limits("service.json", map[string]any{"triggers": 1000, "pendingOccurrences": 2000}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.runner.Limits; got.Triggers != 1000 || got.PendingOccurrences != 2000 {
		t.Fatalf("the limits member was not read: %+v", got)
	}
	if c, err = readServiceConfig(limits("one.json", map[string]any{"pendingOccurrences": 400})); err != nil || c.runner.Limits.Triggers != 0 || c.runner.Limits.PendingOccurrences != 400 {
		t.Fatal("a configuration that sets one limit:", c.runner.Limits, err)
	}
	if c, err = readServiceConfig(f.config("plain.json", f.members("127.0.0.1:8470", nil))); err != nil || c.runner.Limits.Triggers != 0 || c.runner.Limits.PendingOccurrences != 0 {
		t.Fatal("a configuration without limits sets one:", c.runner.Limits, err)
	}
	for i, r := range []struct {
		name string
		v    any
		says string
	}{
		{"not an object", 128, "limits, when present"},
		{"a member more", map[string]any{"triggers": 200, "activeRuns": 200}, "limits, when present"},
		{"a member spelled another way", map[string]any{"Triggers": 200}, "limits, when present"},
		{"a null member", map[string]any{"triggers": nil}, "limits, when present"},
		{"a fractional limit", map[string]any{"pendingOccurrences": 150.5}, "limits, when present"},
		{"a limit in a string", map[string]any{"triggers": "200"}, "limits, when present"},
		{"no trigger", map[string]any{"triggers": 0}, "limits.triggers is from 1 to 1000"},
		{"triggers past the maximum", map[string]any{"triggers": 1001}, "limits.triggers is from 1 to 1000"},
		{"a negative queue", map[string]any{"pendingOccurrences": -1}, "limits.pendingOccurrences is from 1 to 2000"},
		{"a queue past the maximum", map[string]any{"pendingOccurrences": 2001}, "limits.pendingOccurrences is from 1 to 2000"},
	} {
		_, err := readServiceConfig(limits("refused-"+string(rune('a'+i))+".json", r.v))
		if err == nil || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v, want a refusal saying %q", r.name, err, r.says)
		}
	}
}
