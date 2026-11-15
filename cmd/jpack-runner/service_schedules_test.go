//go:build linux || darwin || freebsd

package main

import (
	"slices"
	"strings"
	"testing"
)

// The operator's schedule settings in a service configuration
// (docs/design/schedules.md, section 3): the schedules member names the
// shared intervals and the cron bounds; anything else is refused before
// anything is opened, and without the member no interval is shared.
func TestAServiceConfigurationNamesItsSharedIntervalsAndCronBounds(t *testing.T) {
	f := newServiceFixture(t)
	schedules := func(name string, v any) string {
		return f.config(name, f.members("127.0.0.1:8470", map[string]any{"schedules": v}))
	}
	c, err := readServiceConfig(schedules("service.json", map[string]any{"sharedIntervals": []string{"5m", "15m", "1h", "1d"}, "minimumSeconds": 300, "maximumSeconds": 86400}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.runner.Schedules; !slices.Equal(got.SharedIntervals, []string{"5m", "15m", "1h", "1d"}) || got.MinimumSeconds != 300 || got.MaximumSeconds != 86400 {
		t.Fatalf("the schedules member was not read: %+v", got)
	}
	if c, err = readServiceConfig(f.config("plain.json", f.members("127.0.0.1:8470", nil))); err != nil || c.runner.Schedules.SharedIntervals != nil || c.runner.Schedules.MinimumSeconds != 0 {
		t.Fatal("a configuration without schedules shares an interval or sets a bound:", c.runner.Schedules, err)
	}
	for i, r := range []struct {
		name string
		v    any
		says string
	}{
		{"not an object", "15m", "schedules, when present"},
		{"a member more", map[string]any{"sharedIntervals": []string{"15m"}, "jitter": 1}, "schedules, when present"},
		{"a member spelled another way", map[string]any{"SharedIntervals": []string{"15m"}}, "schedules, when present"},
		{"a null member", map[string]any{"sharedIntervals": nil}, "schedules, when present"},
		{"intervals that are not names", map[string]any{"sharedIntervals": []int{15}}, "schedules, when present"},
		{"a fractional bound", map[string]any{"minimumSeconds": 90.5}, "schedules, when present"},
		{"an interval that does not divide a day", map[string]any{"sharedIntervals": []string{"7m"}}, "a shared interval is named"},
		{"an interval under a minute", map[string]any{"sharedIntervals": []string{"0m"}}, "a shared interval is named"},
		{"an interval in seconds", map[string]any{"sharedIntervals": []string{"60s"}}, "a shared interval is named"},
		{"an interval with a leading zero", map[string]any{"sharedIntervals": []string{"05m"}}, "a shared interval is named"},
		{"two days", map[string]any{"sharedIntervals": []string{"2d"}}, "a shared interval is named"},
		{"one interval twice", map[string]any{"sharedIntervals": []string{"60m", "1h"}}, "one shared interval twice"},
		{"a minimum under a minute", map[string]any{"minimumSeconds": 59}, "schedules.minimumSeconds is from 60"},
		{"a maximum over 366 days", map[string]any{"maximumSeconds": 366*86400 + 1}, "schedules.maximumSeconds is from 60"},
		{"a minimum above the maximum", map[string]any{"minimumSeconds": 3600, "maximumSeconds": 600}, "minimumSeconds is above maximumSeconds"},
	} {
		_, err := readServiceConfig(schedules("refused-"+string(rune('a'+i))+".json", r.v))
		if err == nil || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v, want a refusal saying %q", r.name, err, r.says)
		}
	}
}
