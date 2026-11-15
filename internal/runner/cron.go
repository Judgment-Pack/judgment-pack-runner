package runner

import (
	"strconv"
	"strings"
	"time"
)

// A schedule's cron expression (docs/design/schedules.md, section 2): five
// fields, minute, hour, day of the month, month and day of the week, each
// one space from the next. The grammar is kept small and stated:
//
//	field = item *("," item)
//	item  = "*" | number | number "-" number | ("*" | number "-" number) "/" step
//
// A number is one or two decimal digits in the field's range: minute 0-59,
// hour 0-23, day of the month 1-31, month 1-12, day of the week 0-6, Sunday
// 0. A range runs upward; a step is from 1 to the field's largest value.
// Names (JAN, MON), "?", "L", "W", "#", seven for Sunday, a step on a
// single number, and the "@" macros are refused.
//
// A day matches by the rule common cron readers (Vixie cron, cronie) apply:
// a day-of-month or day-of-week field that starts with "*" ("*" or "*/2")
// is unrestricted for this rule, any other spelling restricts it; when both
// are restricted a day either one matches matches, and when either is
// unrestricted a day both match matches, so "0 0 */2 * 1" is odd days that
// are Mondays.
//
// The reader uses the standard library only.

// cronExpr is a cron expression as read: the values each field matches, as
// bits, and whether its day fields start with "*", which leaves them
// unrestricted for the day rule.
type cronExpr struct {
	minute         uint64 // bits 0-59
	hour           uint32 // bits 0-23
	dom            uint32 // bits 1-31
	month          uint16 // bits 1-12
	dow            uint8  // bits 0-6
	domAny, dowAny bool
}

// maxCronLength is the longest expression read, in bytes.
const maxCronLength = 256

// cronHorizonDays is how far ahead, and back, a next or a latest time is
// looked for: eight years and two days, past the longest gap between two
// leap days (2096 to 2104). An expression the bounds take matches far more
// often; the horizon is there so that a time-zone transition that skips a
// yearly match does not end the schedule.
const cronHorizonDays = 8*366 + 2

// gregorianCycleDays is the length of the Gregorian calendar's cycle, 400
// years, after which its dates fall on the same weekdays again.
const gregorianCycleDays = 146097

var cronFields = [5]struct {
	name     string
	min, max int
}{{"minute", 0, 59}, {"hour", 0, 23}, {"day of the month", 1, 31}, {"month", 1, 12}, {"day of the week", 0, 6}}

// errCronForm is the refusal of an expression out of the grammar. Its
// message states the grammar and repeats nothing of the expression.
func errCronForm(field string) error {
	where := "Write five fields separated by single spaces"
	if field != "" {
		where = "The " + field + " field is out of the grammar"
	}
	return bad("invalid_cron", where+": each a comma-separated list of *, a number, a range a-b, or */step or a-b/step, with numbers only (no names), the day of the week 0-6 with Sunday 0.")
}

// parseCron reads a cron expression, or refuses it as invalid_cron.
func parseCron(s string) (cronExpr, error) {
	var c cronExpr
	if len(s) == 0 || len(s) > maxCronLength {
		return c, errCronForm("")
	}
	parts := strings.Split(s, " ")
	if len(parts) != 5 {
		return c, errCronForm("")
	}
	var bits [5]uint64
	for i, part := range parts {
		f := cronFields[i]
		if part == "" {
			return c, errCronForm("")
		}
		for _, item := range strings.Split(part, ",") {
			lo, hi, step, ok := cronItem(item, f.min, f.max)
			if !ok {
				return c, errCronForm(f.name)
			}
			for v := lo; v <= hi; v += step {
				bits[i] |= 1 << v
			}
		}
	}
	c.minute, c.hour, c.dom, c.month, c.dow = bits[0], uint32(bits[1]), uint32(bits[2]), uint16(bits[3]), uint8(bits[4])
	c.domAny, c.dowAny = strings.HasPrefix(parts[2], "*"), strings.HasPrefix(parts[4], "*")
	return c, nil
}

// cronItem reads one item of a field whose values run from min to max: the
// values it names, from lo to hi by step.
func cronItem(item string, min, max int) (lo, hi, step int, ok bool) {
	base, stepText, stepped := strings.Cut(item, "/")
	step = 1
	if stepped {
		n, ok := cronNumber(stepText)
		if !ok || n < 1 || n > max {
			return 0, 0, 0, false
		}
		step = n
	}
	if base == "*" {
		return min, max, step, true
	}
	first, last, ranged := strings.Cut(base, "-")
	lo, ok = cronNumber(first)
	if !ok {
		return 0, 0, 0, false
	}
	hi = lo
	if ranged {
		if hi, ok = cronNumber(last); !ok {
			return 0, 0, 0, false
		}
	} else if stepped {
		// A step on a single number is refused rather than read as a
		// range to the field's end, as some readers do.
		return 0, 0, 0, false
	}
	if lo < min || hi > max || lo > hi {
		return 0, 0, 0, false
	}
	return lo, hi, step, true
}

// cronNumber reads one or two decimal digits.
func cronNumber(s string) (int, bool) {
	if len(s) == 0 || len(s) > 2 {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// day says whether the expression matches the date y-m-d, a weekday wd.
func (c cronExpr) day(m time.Month, d int, wd time.Weekday) bool {
	if c.month&(1<<m) == 0 {
		return false
	}
	dom, dow := c.dom&(1<<d) != 0, c.dow&(1<<wd) != 0
	if c.domAny || c.dowAny {
		return dom && dow
	}
	return dom || dow
}

// times are the minutes of a day the expression matches, in order, as
// minutes since midnight.
func (c cronExpr) times() []int {
	var out []int
	for h := range 24 {
		if c.hour&(1<<h) == 0 {
			continue
		}
		for m := range 60 {
			if c.minute&(1<<m) != 0 {
				out = append(out, h*60+m)
			}
		}
	}
	return out
}

// localDay maps the local date of day (a time at noon UTC whose date is the
// local date) to instants: offset is the zone's offset in seconds when the
// whole date lies within one of the zone's periods, and exact is then true;
// otherwise a transition falls on the date and calendarTime maps each time.
func localDay(day time.Time, loc *time.Location) (midnight time.Time, offset int, exact bool) {
	y, m, d := day.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if h, mi, sec := start.Clock(); h != 0 || mi != 0 || sec != 0 {
		return time.Time{}, 0, false
	}
	_, offset = start.Zone()
	// A period that begins at the date's midnight may follow a repeated
	// midnight, and one that ends within the date holds a transition.
	begin, end := start.ZoneBounds()
	if !begin.IsZero() && !begin.Before(start) || !end.IsZero() && end.Before(start.Add(24*time.Hour)) {
		return time.Time{}, 0, false
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), offset, true
}

// cronInstant is the instant of minute (since local midnight) on the local
// date of day, by localDay's offset when exact, or by calendarTime.
func cronInstant(day time.Time, minute int, loc *time.Location, midnight time.Time, offset int, exact bool) time.Time {
	if exact {
		return midnight.Add(time.Duration(minute)*time.Minute - time.Duration(offset)*time.Second)
	}
	return calendarTime(day, minute/60, minute%60, loc)
}

// next is the first time after the expression matches in loc, read on the
// local wall clock by the calendar rule (calendarTime): a repeated local
// time matches at its first occurrence, a nonexistent one not at all. Zero
// when none falls within cronHorizonDays local days.
func (c cronExpr) next(after time.Time, loc *time.Location) time.Time {
	times := c.times()
	y, m, d := after.In(loc).Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	for n := 0; n <= cronHorizonDays; n++ {
		date := day.AddDate(0, 0, n)
		if !c.day(date.Month(), date.Day(), date.Weekday()) {
			continue
		}
		midnight, offset, exact := localDay(date, loc)
		for _, minute := range times {
			v := cronInstant(date, minute, loc, midnight, offset, exact)
			if !v.IsZero() && v.After(after) {
				return v
			}
		}
	}
	return time.Time{}
}

// latest is the last time at or before at the expression matches in loc, by
// the rule of next. Zero when none falls within cronHorizonDays local days.
func (c cronExpr) latest(at time.Time, loc *time.Location) time.Time {
	times := c.times()
	y, m, d := at.In(loc).Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	for n := 0; n <= cronHorizonDays; n++ {
		date := day.AddDate(0, 0, -n)
		if !c.day(date.Month(), date.Day(), date.Weekday()) {
			continue
		}
		midnight, offset, exact := localDay(date, loc)
		for i := len(times) - 1; i >= 0; i-- {
			v := cronInstant(date, times[i], loc, midnight, offset, exact)
			if !v.IsZero() && !v.After(at) {
				return v
			}
		}
	}
	return time.Time{}
}

// gaps are the shortest and the longest time between two consecutive
// matches of the expression, read on the local wall clock over one whole
// Gregorian cycle, from its last match back round to its first; matches is
// false for an expression that matches no day of the cycle, and so none
// ever. A time-zone transition can lengthen or shorten one actual gap by
// its change of offset, and a local time it skips is not run.
func (c cronExpr) gaps() (shortest, longest time.Duration, matches bool) {
	times := c.times()
	first, last := times[0], times[len(times)-1]
	shortestMin, longestMin := -1, -1
	keep := func(gap int) {
		if shortestMin < 0 || gap < shortestMin {
			shortestMin = gap
		}
		if gap > longestMin {
			longestMin = gap
		}
	}
	for i := 1; i < len(times); i++ {
		keep(times[i] - times[i-1])
	}
	// 2000-01-01 was a Saturday; the cycle's days are walked from it.
	y, m, d, wd := 2000, time.January, 1, time.Saturday
	firstDay, previous := -1, -1
	for n := range gregorianCycleDays {
		if c.day(m, d, wd) {
			if firstDay < 0 {
				firstDay = n
			} else {
				keep((n-previous)*1440 - last + first)
			}
			previous = n
		}
		d++
		if d > daysIn(y, m) {
			d = 1
			if m++; m > time.December {
				m = time.January
				y++
			}
		}
		wd = (wd + 1) % 7
	}
	if firstDay < 0 {
		return 0, 0, false
	}
	// From the cycle's last matching day round to its first.
	keep((firstDay+gregorianCycleDays-previous)*1440 - last + first)
	return time.Duration(shortestMin) * time.Minute, time.Duration(longestMin) * time.Minute, true
}

// daysIn is the number of days in month m of year y, in the Gregorian
// calendar.
func daysIn(y int, m time.Month) int {
	switch m {
	case time.February:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	case time.April, time.June, time.September, time.November:
		return 30
	}
	return 31
}
