package runner

import (
	"fmt"
	"time"
	_ "time/tzdata"
)

// Calendar schedules choose the first occurrence of a repeated local time and
// skip nonexistent times. Intervals advance by elapsed UTC seconds.
type Schedule struct {
	Kind         string `json:"kind"`
	EverySeconds int    `json:"everySeconds,omitempty"`
	Timezone     string `json:"timezone"`
	Time         string `json:"time,omitempty"`
	Weekday      *int   `json:"weekday,omitempty"`
	StartAt      string `json:"startAt"`
	EndAt        string `json:"endAt,omitempty"`
}

func (c Schedule) validate() error {
	start, e := time.Parse(time.RFC3339, c.StartAt)
	if e != nil || start.Year() < 2000 || start.Year() > 2200 {
		return bad("invalid_schedule", "Choose a valid start timestamp with a UTC offset.")
	}
	if c.EndAt != "" {
		end, e := time.Parse(time.RFC3339, c.EndAt)
		if e != nil || !end.After(start) {
			return bad("invalid_schedule", "The end must be after the start.")
		}
	}
	if _, e = time.LoadLocation(c.Timezone); e != nil || c.Timezone == "Local" || c.Timezone == "" {
		return bad("invalid_schedule", "Choose an IANA time zone or UTC.")
	}
	switch c.Kind {
	case "interval":
		if c.EverySeconds < 60 || c.EverySeconds > 30*86400 || c.Time != "" || c.Weekday != nil {
			return bad("invalid_schedule", "Choose an interval between one minute and 30 days.")
		}
	case "daily", "weekly":
		if _, e = time.Parse("15:04", c.Time); e != nil || c.EverySeconds != 0 {
			return bad("invalid_schedule", "Choose a local time in HH:MM format.")
		}
		if c.Kind == "weekly" && (c.Weekday == nil || *c.Weekday < 0 || *c.Weekday > 6) || c.Kind == "daily" && c.Weekday != nil {
			return bad("invalid_schedule", "Choose one weekday for a weekly schedule.")
		}
	case "once":
		if c.EverySeconds != 0 || c.Time != "" || c.Weekday != nil {
			return bad("invalid_schedule", "One-time schedules use the start timestamp only.")
		}
	default:
		return bad("invalid_schedule", "Unsupported schedule type.")
	}
	return nil
}
func calendarTime(day time.Time, hour, minute int, loc *time.Location) time.Time {
	y, m, d := day.Date()
	wall := time.Date(y, m, d, hour, minute, 0, 0, time.UTC)
	offsets := map[int]bool{}
	for h := -36; h <= 36; h += 6 {
		_, offset := wall.Add(time.Duration(h) * time.Hour).In(loc).Zone()
		offsets[offset] = true
	}
	var first time.Time
	for offset := range offsets {
		v := wall.Add(-time.Duration(offset) * time.Second)
		local := v.In(loc)
		if local.Year() == y && local.Month() == m && local.Day() == d && local.Hour() == hour && local.Minute() == minute && (first.IsZero() || v.Before(first)) {
			first = v
		}
	}
	return first
}
func (c Schedule) next(after time.Time) time.Time {
	start, _ := time.Parse(time.RFC3339, c.StartAt)
	loc, _ := time.LoadLocation(c.Timezone)
	var next time.Time
	switch c.Kind {
	case "once":
		if start.After(after) {
			next = start
		}
	case "interval":
		span := time.Duration(c.EverySeconds) * time.Second
		if after.Before(start) {
			next = start
		} else {
			next = start.Add((after.Sub(start)/span + 1) * span)
		}
	case "daily", "weekly":
		base := after
		if start.After(base) {
			base = start
		}
		y, m, d := base.In(loc).Date()
		day := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
		hour, minute := 0, 0
		fmt.Sscanf(c.Time, "%d:%d", &hour, &minute)
		for n := 0; n < 16; n++ {
			date := day.AddDate(0, 0, n)
			if c.Kind == "weekly" && int(date.Weekday()) != *c.Weekday {
				continue
			}
			v := calendarTime(date, hour, minute, loc)
			if !v.IsZero() && v.After(after) && !v.Before(start) {
				next = v
				break
			}
		}
	}
	if c.EndAt != "" && !next.IsZero() {
		end, _ := time.Parse(time.RFC3339, c.EndAt)
		if !next.Before(end) {
			return time.Time{}
		}
	}
	return next.UTC()
}
func (c Schedule) latest(at time.Time) time.Time {
	start, _ := time.Parse(time.RFC3339, c.StartAt)
	if c.EndAt != "" {
		end, _ := time.Parse(time.RFC3339, c.EndAt)
		if !at.Before(end) {
			at = end.Add(-time.Nanosecond)
		}
	}
	if at.Before(start) {
		return time.Time{}
	}
	if c.Kind == "once" {
		return start
	}
	if c.Kind == "interval" {
		span := time.Duration(c.EverySeconds) * time.Second
		return start.Add(at.Sub(start) / span * span)
	}
	// At most sixteen local days include a previous weekly occurrence even when
	// an entire local date is skipped by a time-zone transition.
	loc, _ := time.LoadLocation(c.Timezone)
	y, m, d := at.In(loc).Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	hour, minute := 0, 0
	fmt.Sscanf(c.Time, "%d:%d", &hour, &minute)
	for n := 0; n < 16; n++ {
		date := day.AddDate(0, 0, -n)
		if c.Kind == "weekly" && int(date.Weekday()) != *c.Weekday {
			continue
		}
		v := calendarTime(date, hour, minute, loc)
		if !v.IsZero() && !v.After(at) && !v.Before(start) {
			return v
		}
	}
	return time.Time{}
}
func (c Schedule) preview(at time.Time) []string {
	out := []string{}
	loc, _ := time.LoadLocation(c.Timezone)
	for range 3 {
		at = c.next(at)
		if at.IsZero() {
			break
		}
		out = append(out, at.In(loc).Format(time.RFC3339Nano))
	}
	return out
}
