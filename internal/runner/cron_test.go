package runner

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// refusalCode is the code of an apiError, "" for any other error or none.
func refusalCode(err error) string {
	var api *apiError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

// The cron reader's grammar, accepted and refused (docs/design/schedules.md,
// section 2): each accepted expression with the values it matches in each
// field, each refused one refused invalid_cron.
func TestCronGrammar(t *testing.T) {
	values := func(bits uint64, max int) []int {
		var out []int
		for v := 0; v <= max; v++ {
			if bits&(1<<v) != 0 {
				out = append(out, v)
			}
		}
		return out
	}
	same := slices.Equal[[]int]
	all := func(lo, hi int) []int {
		var out []int
		for v := lo; v <= hi; v++ {
			out = append(out, v)
		}
		return out
	}
	for _, c := range []struct {
		expr                          string
		minute, hour, dom, month, dow []int
		domAny, dowAny                bool
	}{
		{"* * * * *", all(0, 59), all(0, 23), all(1, 31), all(1, 12), all(0, 6), true, true},
		{"0 0 * * *", []int{0}, []int{0}, all(1, 31), all(1, 12), all(0, 6), true, true},
		{"*/15 * * * *", []int{0, 15, 30, 45}, all(0, 23), all(1, 31), all(1, 12), all(0, 6), true, true},
		{"0-10/5 9-17 * * 1-5", []int{0, 5, 10}, all(9, 17), all(1, 31), all(1, 12), all(1, 5), true, false},
		{"5,10,15 0,12 1,15 1,7 0,6", []int{5, 10, 15}, []int{0, 12}, []int{1, 15}, []int{1, 7}, []int{0, 6}, false, false},
		{"00 05 01 01 0", []int{0}, []int{5}, []int{1}, []int{1}, []int{0}, false, false},
		{"59 23 31 12 6", []int{59}, []int{23}, []int{31}, []int{12}, []int{6}, false, false},
		{"0 0 1 */3 *", []int{0}, []int{0}, []int{1}, []int{1, 4, 7, 10}, all(0, 6), false, true},
		// A field that starts with "*" ("*/2") leaves the day rule open, as
		// common readers do; "1-31/2", the same days, restricts it.
		{"0 0 */2 * 5", []int{0}, []int{0}, []int{1, 3, 5, 7, 9, 11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31}, all(1, 12), []int{5}, true, false},
		{"0 0 1-31/2 * 5", []int{0}, []int{0}, []int{1, 3, 5, 7, 9, 11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31}, all(1, 12), []int{5}, false, false},
		{"0 0 13 * */2", []int{0}, []int{0}, []int{13}, all(1, 12), []int{0, 2, 4, 6}, false, true},
		{"1-1 0 * * *", []int{1}, []int{0}, all(1, 31), all(1, 12), all(0, 6), true, true},
		{"*/59 */23 */31 */12 */6", []int{0, 59}, []int{0, 23}, []int{1}, []int{1}, []int{0, 6}, true, true},
	} {
		got, err := parseCron(c.expr)
		if err != nil {
			t.Errorf("%q refused: %v", c.expr, err)
			continue
		}
		if !same(values(got.minute, 59), c.minute) || !same(values(uint64(got.hour), 23), c.hour) || !same(values(uint64(got.dom), 31), c.dom) || !same(values(uint64(got.month), 12), c.month) || !same(values(uint64(got.dow), 6), c.dow) || got.domAny != c.domAny || got.dowAny != c.dowAny {
			t.Errorf("%q read as %+v", c.expr, got)
		}
	}
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", " * * * * *", "* * * * * ", "*  * * * *", "*\t* * * *", "* * * * *\n",
		"60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 0 *", "* * * 13 *", "* * * * 7", "* * * * 0-7",
		"* * * JAN *", "* * * * MON", "? * * * *", "* * L * *", "* * 15W * *", "* * * * 1#2", "@daily", "@every 5m",
		"5/10 * * * *", "*/0 * * * *", "*/60 * * * *", "* * * * */7", "10-5 * * * *", "1-2-3 * * * *", "*/5/2 * * * *",
		"-1 * * * *", "+1 * * * *", "1,,2 * * * *", ",1 * * * *", "1, * * * *", "001 * * * *", "1- * * * *", "-5 * * * *",
		"１ * * * *", "1.5 * * * *", "** * * * *", "*/ * * * *", "*-5 * * * *",
		strings.Repeat("0,", 200) + "0 * * * *",
	} {
		if _, err := parseCron(expr); refusalCode(err) != "invalid_cron" {
			t.Errorf("%q: %v, want invalid_cron", expr, err)
		}
	}
	// The refusal states the grammar and repeats nothing of the expression.
	if _, err := parseCron("* * * JANUARY *"); err == nil || strings.Contains(err.Error(), "JANUARY") || !strings.Contains(err.Error(), "month") {
		t.Errorf("the refusal %v does not name the field alone", err)
	}
}

func mustCron(t *testing.T, expr string) cronExpr {
	t.Helper()
	c, err := parseCron(expr)
	if err != nil {
		t.Fatal(expr, err)
	}
	return c
}

// The next and the latest time of an expression: month ends, the leap day,
// the day-of-month and day-of-week rule, a zone's transitions, and an
// expression that never matches.
func TestCronNextAndLatest(t *testing.T) {
	toronto, err := time.LoadLocation("America/Toronto")
	must(t, err)
	for _, c := range []struct {
		why, expr string
		loc       *time.Location
		after     string
		want      string // "" for none
	}{
		{"every minute", "* * * * *", time.UTC, "2026-11-15T10:00:30Z", "2026-11-15T10:01:00Z"},
		{"strictly after", "0 0 * * *", time.UTC, "2026-11-15T00:00:00Z", "2026-11-16T00:00:00Z"},
		{"the end of a day", "59 23 * * *", time.UTC, "2026-12-31T23:59:00Z", "2027-01-01T23:59:00Z"},
		{"a month without the 31st is passed", "0 0 31 * *", time.UTC, "2026-01-31T00:00:00Z", "2026-03-31T00:00:00Z"},
		{"February has no 30th", "0 12 30 * *", time.UTC, "2026-02-01T00:00:00Z", "2026-03-30T12:00:00Z"},
		{"the leap day", "0 0 29 2 *", time.UTC, "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"2100 is no leap year", "0 0 29 2 *", time.UTC, "2096-03-01T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"the 13th or a Friday: the Friday first", "0 0 13 * 5", time.UTC, "2026-11-01T00:00:00Z", "2026-11-06T00:00:00Z"},
		{"the 13th or a Friday: the 13th", "0 0 13 * 5", time.UTC, "2026-11-06T00:00:00Z", "2026-11-13T00:00:00Z"},
		{"the 13th alone", "0 0 13 * *", time.UTC, "2026-11-01T00:00:00Z", "2026-11-13T00:00:00Z"},
		{"Fridays alone", "0 0 * * 5", time.UTC, "2026-11-07T00:00:00Z", "2026-11-13T00:00:00Z"},
		// A day field that starts with "*" leaves the rule open: both
		// fields must match, as in common readers.
		{"odd days that are Fridays", "0 0 */2 * 5", time.UTC, "2026-11-01T00:00:00Z", "2026-11-13T00:00:00Z"},
		{"odd days that are Mondays", "0 0 */2 * 1", time.UTC, "2026-11-01T00:00:00Z", "2026-11-09T00:00:00Z"},
		{"odd days that are Mondays, the next", "0 0 */2 * 1", time.UTC, "2026-11-09T00:00:00Z", "2026-11-23T00:00:00Z"},
		{"the 13th that is a Sunday, Tuesday, Thursday or Saturday", "0 0 13 * */2", time.UTC, "2026-11-01T00:00:00Z", "2026-12-13T00:00:00Z"},
		{"odd days written as a range, or Fridays", "0 0 1-31/2 * 5", time.UTC, "2026-11-01T00:00:00Z", "2026-11-03T00:00:00Z"},
		{"odd days written as a range, or Fridays: the Friday", "0 0 1-31/2 * 5", time.UTC, "2026-11-05T00:00:00Z", "2026-11-06T00:00:00Z"},
		{"a weekday at nine, over a weekend", "0 9 * * 1-5", time.UTC, "2026-11-13T09:00:00Z", "2026-11-16T09:00:00Z"},
		{"the zone's wall clock", "0 9 * * *", toronto, "2026-11-15T00:00:00Z", "2026-11-15T14:00:00Z"},
		{"a nonexistent local time is skipped", "30 2 * * *", toronto, "2026-03-07T08:00:00Z", "2026-03-09T06:30:00Z"},
		{"a repeated local time at its first", "30 1 * * *", toronto, "2026-11-01T00:00:00Z", "2026-11-01T05:30:00Z"},
		{"a repeated local time once", "30 1 * * *", toronto, "2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
		{"every half hour across the repeated hour", "*/30 * * * *", toronto, "2026-11-01T05:30:00Z", "2026-11-01T07:00:00Z"},
		{"never", "0 0 30 2 *", time.UTC, "2026-01-01T00:00:00Z", ""},
		{"never, on the 31st of short months alone", "0 0 31 4,6,9,11 *", time.UTC, "2026-01-01T00:00:00Z", ""},
	} {
		got := mustCron(t, c.expr).next(instant(c.after), c.loc)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("%s: %q after %s is %s, want none", c.why, c.expr, c.after, got)
			}
			continue
		}
		if !got.Equal(instant(c.want)) {
			t.Errorf("%s: %q after %s is %s, want %s", c.why, c.expr, c.after, got.UTC(), c.want)
		}
		// The latest at the next time is that time, and just before it an
		// earlier one.
		if l := mustCron(t, c.expr).latest(got, c.loc); !l.Equal(got) {
			t.Errorf("%s: the latest at %s is %s", c.why, got, l)
		}
		if l := mustCron(t, c.expr).latest(got.Add(-time.Second), c.loc); !l.Before(got) || l.After(instant(c.after)) {
			t.Errorf("%s: the latest before %s is %s, not at or before %s", c.why, got, l, c.after)
		}
	}
	if l := mustCron(t, "0 0 31 * *").latest(instant("2026-03-15T00:00:00Z"), time.UTC); !l.Equal(instant("2026-01-31T00:00:00Z")) {
		t.Error("the latest 31st before March 15 is", l)
	}
	if l := mustCron(t, "30 1 * * *").latest(instant("2026-11-01T06:31:00Z"), toronto); !l.Equal(instant("2026-11-01T05:30:00Z")) {
		t.Error("the latest repeated local time is", l)
	}
}

// The shortest and the longest gap between matches, over the whole cycle,
// which the bounds hold an expression to.
func TestCronGaps(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		expr              string
		shortest, longest time.Duration
	}{
		{"* * * * *", time.Minute, time.Minute},
		{"*/15 * * * *", 15 * time.Minute, 15 * time.Minute},
		{"0,30 0 * * *", 30 * time.Minute, day - 30*time.Minute},
		{"0 9 * * 1-5", day, 3 * day},
		{"0 0 1 * *", 28 * day, 31 * day},
		{"0 0 1 1 *", 365 * day, 366 * day},
		// 2096-02-29 to 2104-02-29.
		{"0 0 29 2 *", (4*365 + 1) * day, (8*365 + 1) * day},
	} {
		shortest, longest, matches := mustCron(t, c.expr).gaps()
		if !matches || shortest != c.shortest || longest != c.longest {
			t.Errorf("%q: gaps %s to %s (%v), want %s to %s", c.expr, shortest, longest, matches, c.shortest, c.longest)
		}
	}
	if _, _, matches := mustCron(t, "0 0 30 2 *").gaps(); matches {
		t.Error("the 30th of February matches")
	}
}
