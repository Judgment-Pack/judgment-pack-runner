package runner

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Shared intervals and cron schedules (docs/design/schedules.md).

// tickWorld is a Service with the operator's schedule settings, its
// Runtime one that completes, and the jobs it makes each with a release of
// their own.
type tickWorld struct {
	t       *testing.T
	cfg     Config
	s       *Service
	runtime string
}

func newTickWorld(t *testing.T, settings ScheduleSettings) *tickWorld {
	t.Helper()
	cfg := journalConfig(t)
	cfg.Schedules = settings
	w := &tickWorld{t: t, cfg: cfg, s: openJournal(t, cfg)}
	t.Cleanup(func() { w.s.Close() })
	w.runtime = completingRuntime(t, cfg.Dir)
	return w
}

// sharedSchedule is a schedule trigger's configuration on the shared
// interval tick, at its every tick or, with an expression, after its
// matches.
func sharedSchedule(tick, cron string) TriggerConfig {
	c := scheduleTriggerConfig(time.Now())
	c.Name = "Shared " + tick
	c.Schedule = &Schedule{Kind: "tick", Tick: tick, Cron: cron, Timezone: "UTC", StartAt: "2026-01-01T00:00:00Z"}
	return c
}

// cronSchedule is a schedule trigger's configuration on the Runner's own
// clock, by expr in zone.
func cronSchedule(expr, zone string) TriggerConfig {
	c := scheduleTriggerConfig(time.Now())
	c.Name = "Cron"
	c.Schedule = &Schedule{Kind: "cron", Cron: expr, Timezone: zone, StartAt: "2026-01-01T00:00:00Z"}
	return c
}

// job makes a job under scope ("" for none) with a release of its own.
func (w *tickWorld) job(scope string) Job {
	w.t.Helper()
	j, e := w.s.createJobWith(requester{}, "Ticked", journalRelease(w.t, w.s, w.runtime).ID, scope, nil, nil)
	must(w.t, e)
	return j
}

// trigger configures c on a new job under scope, enabled when enabled, and
// due at due: its next time written so, as the tick before due left it.
func (w *tickWorld) trigger(scope string, c TriggerConfig, enabled bool, due time.Time) Trigger {
	w.t.Helper()
	tr, e := w.s.configureTrigger(w.job(scope).ID, "", 0, c)
	must(w.t, e)
	if enabled {
		tr, _, e = w.s.setTriggerState(tr.ID, tr.Revision, false, true)
		must(w.t, e)
	}
	tr.NextAt = due.Format(time.RFC3339Nano)
	must(w.t, w.s.writeTrigger(tr, "", false))
	return tr
}

// occurrencesOf are the occurrences trigger id admitted, oldest first.
func (w *tickWorld) occurrencesOf(id string) []Occurrence {
	w.t.Helper()
	rows, e := sqliteDB(w.s).Query("SELECT record FROM occurrences WHERE json_extract(record,'$.triggerId')=? ORDER BY seq", id)
	must(w.t, e)
	defer rows.Close()
	var out []Occurrence
	for rows.Next() {
		var raw string
		must(w.t, rows.Scan(&raw))
		var o Occurrence
		must(w.t, json.Unmarshal([]byte(raw), &o))
		out = append(out, o)
	}
	return out
}

// at sets the Runner's clock, the time a tick is judged by, to now, and
// answers it: a test passes its times rather than read the wall clock's.
func (w *tickWorld) at(now time.Time) time.Time {
	w.s.clock = func() time.Time { return now }
	return now
}

// nextAt is trigger id's next time as stored.
func (w *tickWorld) nextAt(id string) string {
	w.t.Helper()
	tr, _ := storedTrigger(w.t, w.s, id)
	return tr.NextAt
}

// tick asks POST /v1/ticks for interval at at ("" for none) and answers the
// response.
func (w *tickWorld) tick(interval, at string) *httptest.ResponseRecorder {
	w.t.Helper()
	body := map[string]string{"interval": interval}
	if at != "" {
		body["at"] = at
	}
	return serve(w.t, w.s, "POST", "/v1/ticks", string(encode(body)), nil)
}

// tickResult reads a tick's answer, held to the API document.
func tickResult(t *testing.T, doc openAPI, got *httptest.ResponseRecorder) TickResult {
	t.Helper()
	if got.Code != 200 {
		t.Fatalf("the tick was answered %d %s", got.Code, got.Body.String())
	}
	if problems := doc.check("tick", doc.schema("TickResult"), decoded(t, got.Body.Bytes())); len(problems) != 0 {
		t.Fatalf("the tick's answer is not the documented one: %v", problems)
	}
	var r TickResult
	must(t, json.Unmarshal(got.Body.Bytes(), &r))
	return r
}

func ticked(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Sections 4 and 5: one call ticks every enabled trigger of every scope
// bound to its interval, each due at the tick admitting its occurrence once
// and moving its next time to the next tick; a repeated call, or one for a
// tick already passed, admits nothing. A trigger of another interval, a
// paused one and one on the Runner's own clock are not its.
func TestASharedTickTicksEveryScopesTriggersOnce(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m", "1h"}})
	doc := readOpenAPI(t)
	tick := w.at(time.Now().UTC().Truncate(15 * time.Minute))
	var bound []Trigger
	for _, scope := range []string{"", "scope-a", "scope-b"} {
		bound = append(bound, w.trigger(scope, sharedSchedule("15m", ""), true, tick))
	}
	hourly := w.trigger("scope-a", sharedSchedule("1h", ""), true, tick)
	paused := w.trigger("scope-a", sharedSchedule("15m", ""), false, tick)
	own := w.trigger("scope-b", scheduleTriggerConfig(time.Now()), true, tick)
	entries := len(journalEntries(t, w.cfg))

	r := tickResult(t, doc, w.tick("15m", ticked(tick)))
	if r.Interval != "15m" || r.At != ticked(tick) || r.Occurrences != len(bound) {
		t.Fatalf("the tick answered %+v", r)
	}
	for _, tr := range bound {
		got := w.occurrencesOf(tr.ID)
		if len(got) != 1 || got[0].State != "accepted" || got[0].Kind != "schedule" || got[0].ScheduledAt != tick.Format(time.RFC3339Nano) || got[0].MissedFrom != "" {
			t.Fatalf("trigger %s (scope %q) admitted %+v", tr.ID, tr.Scope, got)
		}
		if next := w.nextAt(tr.ID); next != tick.Add(15*time.Minute).Format(time.RFC3339Nano) {
			t.Fatalf("trigger %s is next due %s", tr.ID, next)
		}
	}
	for _, tr := range []Trigger{hourly, paused, own} {
		if got := w.occurrencesOf(tr.ID); len(got) != 0 || w.nextAt(tr.ID) != tick.Format(time.RFC3339Nano) {
			t.Fatalf("trigger %s, not the interval's, was ticked: %+v", tr.Config.Schedule.Kind+" "+tr.Config.Schedule.Tick, got)
		}
	}
	// Each admission is journaled as the Runner's own tick's is.
	if n := len(journalEntries(t, w.cfg)); n != entries+len(bound) {
		t.Fatalf("the tick journaled %d entries", n-entries)
	}

	// The same tick again, and a tick already passed (one interval behind
	// the clock, the furthest a call may name), admit nothing.
	for _, at := range []time.Time{tick, tick.Add(-15 * time.Minute)} {
		if r = tickResult(t, doc, w.tick("15m", ticked(at))); r.Occurrences != 0 {
			t.Fatalf("the tick %s again admitted %d", at, r.Occurrences)
		}
	}
	for _, tr := range bound {
		if got := w.occurrencesOf(tr.ID); len(got) != 1 {
			t.Fatalf("a repeated tick admitted again: %+v", got)
		}
	}
	if n := len(journalEntries(t, w.cfg)); n != entries+len(bound) {
		t.Fatalf("a repeated tick journaled %d entries", n-entries-len(bound))
	}

	// The Runner's own clock never ticks a shared-interval trigger, however
	// late it is.
	held, hash := storedTrigger(t, w.s, hourly.ID)
	must(t, w.s.scheduleTick(held, hash, tick.Add(48*time.Hour)))
	if got := w.occurrencesOf(hourly.ID); len(got) != 0 {
		t.Fatal("the Runner's own clock ticked a shared-interval trigger:", got)
	}

	// A call that names no tick ticks the last at or before the Runner's
	// clock plus the lead: a tick of the interval, never more than one
	// interval behind the clock.
	r = tickResult(t, doc, w.tick("1h", ""))
	at, e := time.Parse(time.RFC3339, r.At)
	must(t, e)
	if !at.Truncate(time.Hour).Equal(at) || at.After(tick.Add(sharedTickLead)) || tick.Sub(at) > time.Hour {
		t.Fatalf("a call naming no tick ticked %s", r.At)
	}
}

// Section 5: a trigger whose due time is before the tick called, its own
// tick or ticks never called, follows its missed rule: skip records the
// missed occurrence skipped through the tick, latest admits the tick's.
func TestASharedTickAfterMissedTicksFollowsTheMissedRule(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	due := tick.Add(-30 * time.Minute)
	skip := w.trigger("", sharedSchedule("15m", ""), true, due)
	c := sharedSchedule("15m", "")
	c.Missed = "latest"
	latest := w.trigger("scope-a", c, true, due)
	if r := tickResult(t, readOpenAPI(t), w.tick("15m", ticked(tick))); r.Occurrences != 2 {
		t.Fatalf("the tick admitted %d", r.Occurrences)
	}
	got := w.occurrencesOf(skip.ID)
	if len(got) != 1 || got[0].State != "skipped" || got[0].Reason != "missed" || got[0].MissedFrom != due.Format(time.RFC3339Nano) || got[0].MissedThrough != tick.Format(time.RFC3339Nano) {
		t.Fatalf("skip admitted %+v", got)
	}
	got = w.occurrencesOf(latest.ID)
	if len(got) != 1 || got[0].State != "accepted" || got[0].Reason != "latest-after-missed" || got[0].ScheduledAt != tick.Format(time.RFC3339Nano) || got[0].MissedFrom != due.Format(time.RFC3339Nano) {
		t.Fatalf("latest admitted %+v", got)
	}
	for _, tr := range []Trigger{skip, latest} {
		if next := w.nextAt(tr.ID); next != tick.Add(15*time.Minute).Format(time.RFC3339Nano) {
			t.Fatalf("trigger %s is next due %s", tr.ID, next)
		}
	}
}

// Section 6: a scheduler down from 10:00 to 11:00 that replays at 11:00:05
// every tick it missed makes no catch-up burst. Each tick more than one
// interval behind the Runner's clock is refused, invalid_tick, and records
// nothing; the 11:00 tick finds the 10:00 due time missed: latest admits
// one occurrence, at 11:00, and skip records the missed times skipped.
func TestASharedTickReplayedAfterAnOutageMakesNoBurst(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	doc := readOpenAPI(t)
	down, back := instant("2026-11-15T10:00:00Z"), instant("2026-11-15T11:00:00Z")
	skip := w.trigger("", sharedSchedule("15m", ""), true, down)
	c := sharedSchedule("15m", "")
	c.Missed = "latest"
	latest := w.trigger("scope-a", c, true, down)
	w.at(back.Add(5 * time.Second))
	made, refused := 0, 0
	for at := down; !at.After(back); at = at.Add(15 * time.Minute) {
		got := w.tick("15m", ticked(at))
		if got.Code == 422 && strings.Contains(got.Body.String(), `"code":"invalid_tick"`) {
			refused++
			continue
		}
		made += tickResult(t, doc, got).Occurrences
	}
	if refused != 4 || made != 2 {
		t.Fatalf("the replay was refused %d times and recorded %d occurrences", refused, made)
	}
	got := w.occurrencesOf(skip.ID)
	if len(got) != 1 || got[0].State != "skipped" || got[0].Reason != "missed" || got[0].MissedFrom != down.Format(time.RFC3339Nano) || got[0].MissedThrough != back.Format(time.RFC3339Nano) {
		t.Fatalf("skip recorded %+v", got)
	}
	got = w.occurrencesOf(latest.ID)
	if len(got) != 1 || got[0].State != "accepted" || got[0].Reason != "latest-after-missed" || got[0].ScheduledAt != back.Format(time.RFC3339Nano) || got[0].MissedFrom != down.Format(time.RFC3339Nano) {
		t.Fatalf("latest admitted %+v", got)
	}
	for _, tr := range []Trigger{skip, latest} {
		if next := w.nextAt(tr.ID); next != back.Add(15*time.Minute).Format(time.RFC3339Nano) {
			t.Fatalf("trigger %s is next due %s", tr.ID, next)
		}
	}
}

// Section 4: the route is the installation's. A request without the
// control API's credential, with another credential (a trigger's key
// among them), naming a scope identifier, naming an interval the operator
// does not, or a time that is not a tick no more than the lead ahead, is
// refused, and ticks, changes and journals nothing; the triggers were due
// all along.
func TestASharedTickIsRefusedWithoutTheCredentialOrWithAScope(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	var due []Trigger
	for _, scope := range []string{"", "scope-a"} {
		due = append(due, w.trigger(scope, sharedSchedule("15m", ""), true, tick))
	}
	events, e := w.s.configureTrigger(w.job("scope-a").ID, "", 0, eventTriggerConfig())
	must(t, e)
	_, key, e := w.s.setTriggerState(events.ID, events.Revision, false, true)
	must(t, e)
	if key == "" {
		t.Fatal("the event trigger has no key")
	}
	occurrences, entries := countRows(t, w.s, "occurrences"), len(journalEntries(t, w.cfg))
	body := string(encode(map[string]string{"interval": "15m", "at": ticked(tick)}))
	anonymous := func(header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/ticks", strings.NewReader(body))
		for k, v := range header {
			req.Header.Set(k, v)
		}
		got := httptest.NewRecorder()
		w.s.Handler(journalBearer).ServeHTTP(got, req)
		return got
	}
	for _, c := range []struct {
		name string
		got  *httptest.ResponseRecorder
		code int
		says string
	}{
		{"no credential", anonymous(nil), 401, "authentication required"},
		{"another bearer", anonymous(map[string]string{"Authorization": "Bearer " + strings.Repeat("x", len(journalBearer))}), 401, "authentication required"},
		{"a trigger's key alone", anonymous(map[string]string{"X-Trigger-Token": key}), 401, "authentication required"},
		{"a scope identifier", serve(t, w.s, "POST", "/v1/ticks?scope=scope-a", body, nil), 400, "invalid_scope"},
		{"a scope identifier out of form", serve(t, w.s, "POST", "/v1/ticks?scope=", body, nil), 400, "invalid_scope"},
		{"an interval the operator names not", w.tick("5m", ticked(tick)), 422, "unknown_shared_interval"},
		{"no interval", w.tick("", ticked(tick)), 422, "unknown_shared_interval"},
		{"a time that is not a tick", w.tick("15m", ticked(tick.Add(time.Minute))), 422, "invalid_tick"},
		{"a tick ahead of the clock", w.tick("15m", ticked(tick.Add(time.Hour))), 422, "invalid_tick"},
		{"a tick more than one interval behind the clock", w.tick("15m", ticked(tick.Add(-30*time.Minute))), 422, "invalid_tick"},
		{"a time without its offset", w.tick("15m", tick.Format("2006-01-02T15:04:05")), 422, "invalid_tick"},
		{"a tick before 2000", w.tick("15m", "1999-12-31T00:00:00Z"), 422, "invalid_tick"},
		{"a member more", serve(t, w.s, "POST", "/v1/ticks", `{"interval":"15m","scope":"scope-a"}`, nil), 400, "invalid_request"},
	} {
		if c.got.Code != c.code || !strings.Contains(c.got.Body.String(), c.says) {
			t.Errorf("%s: %d %s, want %d %s", c.name, c.got.Code, c.got.Body.String(), c.code, c.says)
		}
	}
	if n := countRows(t, w.s, "occurrences"); n != occurrences {
		t.Fatalf("a refused tick admitted %d", n-occurrences)
	}
	if n := len(journalEntries(t, w.cfg)); n != entries {
		t.Fatalf("a refused tick journaled %d entries", n-entries)
	}
	for _, tr := range due {
		if w.nextAt(tr.ID) != tick.Format(time.RFC3339Nano) {
			t.Fatal("a refused tick moved a trigger on")
		}
	}
	if r := tickResult(t, readOpenAPI(t), w.tick("15m", ticked(tick))); r.Occurrences != len(due) {
		t.Fatalf("the tick with the credential admitted %d", r.Occurrences)
	}
}

// Section 3: a cron expression is held to the operator's bounds, and a
// shared interval to the operator's list, as the trigger is configured,
// enabled or changed through POST /v1/jobs/{job}/triggers: one code per
// reason, and nothing written.
func TestACronScheduleIsHeldToTheOperatorsBounds(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m", "1h"}, MinimumSeconds: 300, MaximumSeconds: 7 * 86400})
	j := w.job("scope-a")
	configure := func(c TriggerConfig, id string, revision int) *httptest.ResponseRecorder {
		return serve(t, w.s, "POST", "/v1/jobs/"+j.ID+"/triggers?scope=scope-a", string(encode(map[string]any{"id": id, "revision": revision, "config": c})), nil)
	}
	tickWith := func(tick, cron string) TriggerConfig { return sharedSchedule(tick, cron) }
	interval := scheduleTriggerConfig(time.Now())
	interval.Schedule.Cron = "*/5 * * * *"
	tickAndTime := sharedSchedule("15m", "")
	tickAndTime.Schedule.Time = "09:00"
	for _, c := range []struct {
		name   string
		config TriggerConfig
		code   string
	}{
		{"matches closer than the minimum", cronSchedule("* * * * *", "UTC"), "cron_interval_too_short"},
		{"matches further apart than the maximum", cronSchedule("0 0 1 * *", "UTC"), "cron_interval_too_long"},
		{"no date ever matches", cronSchedule("0 0 30 2 *", "UTC"), "cron_never_matches"},
		{"a name", cronSchedule("0 9 * * MON", "UTC"), "invalid_cron"},
		{"six fields", cronSchedule("0 0 9 * * *", "UTC"), "invalid_cron"},
		{"no expression", cronSchedule("", "UTC"), "invalid_schedule"},
		{"a zone not named", cronSchedule("0 9 * * *", "Local"), "invalid_schedule"},
		{"an interval the operator names not", tickWith("5m", ""), "unknown_shared_interval"},
		{"an interval that is not one", tickWith("7m", ""), "invalid_schedule"},
		{"matches closer than the interval", tickWith("1h", "*/15 * * * *"), "cron_interval_too_short"},
		{"an expression on an interval schedule", interval, "invalid_schedule"},
		{"a time beside the interval", tickAndTime, "invalid_schedule"},
	} {
		got := configure(c.config, "", 0)
		if got.Code != 422 || !strings.Contains(got.Body.String(), `"code":"`+c.code+`"`) {
			t.Errorf("%s: %d %s, want 422 %s", c.name, got.Code, got.Body.String(), c.code)
		}
	}
	if n := countRows(t, w.s, "triggers"); n != 0 {
		t.Fatalf("a refused schedule wrote %d triggers", n)
	}
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != scopedStoreSchema {
		t.Fatalf("a refused schedule left the store at %s", schema)
	}

	// Within the bounds: each is taken, and the store's schema raised.
	for _, c := range []TriggerConfig{cronSchedule("*/5 * * * *", "America/Toronto"), cronSchedule("0 9 * * 1-5", "UTC"), tickWith("1h", "0 */2 * * *"), tickWith("15m", "")} {
		if got := configure(c, "", 0); got.Code != 200 {
			t.Fatalf("%+v: %d %s", *c.Schedule, got.Code, got.Body.String())
		}
	}
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != scheduleStoreSchema {
		t.Fatalf("a cron schedule left the store at %s", schema)
	}

	// An interval schedule changed to a cron one through the same route,
	// its next run time stated once it is enabled.
	got := configure(scheduleTriggerConfig(time.Now()), "", 0)
	var tr Trigger
	must(t, json.Unmarshal(got.Body.Bytes(), &tr))
	if got = configure(cronSchedule("0 * * * *", "UTC"), tr.ID, tr.Revision); got.Code != 200 {
		t.Fatalf("the change to a cron schedule: %d %s", got.Code, got.Body.String())
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &tr))
	got = serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/state?scope=scope-a", fmt.Sprintf(`{"revision":%d,"paused":false,"reviewed":true}`, tr.Revision), nil)
	var enabled struct {
		Trigger Trigger `json:"trigger"`
	}
	must(t, json.Unmarshal(got.Body.Bytes(), &enabled))
	next, e := time.Parse(time.RFC3339Nano, enabled.Trigger.NextAt)
	if got.Code != 200 || e != nil || enabled.Trigger.Config.Schedule.Kind != "cron" || !next.Truncate(time.Hour).Equal(next) || next.Before(time.Now()) || next.After(time.Now().Add(time.Hour)) {
		t.Fatalf("the cron trigger enabled: %d %s", got.Code, got.Body.String())
	}

	// The bounds in force at each check: a Runner whose operator raised the
	// minimum refuses to enable a trigger configured under the earlier one.
	got = configure(cronSchedule("*/5 * * * *", "UTC"), "", 0)
	must(t, json.Unmarshal(got.Body.Bytes(), &tr))
	w.s.Close()
	w.cfg.Schedules.MinimumSeconds = 600
	w.s = openJournal(t, w.cfg)
	got = serve(t, w.s, "POST", "/v1/triggers/"+tr.ID+"/state?scope=scope-a", fmt.Sprintf(`{"revision":%d,"paused":false,"reviewed":true}`, tr.Revision), nil)
	if got.Code != 422 || !strings.Contains(got.Body.String(), "cron_interval_too_short") {
		t.Fatalf("a trigger under a raised minimum was enabled: %d %s", got.Code, got.Body.String())
	}
	// The status states the settings in force.
	status := serve(t, w.s, "GET", "/v1/status", "", nil).Body.String()
	for _, want := range []string{`"cron-schedules"`, `"shared-interval-ticks"`, `"schedules":{"maximumSeconds":604800,"minimumSeconds":600,"sharedIntervals":["15m","1h"]}`} {
		if !strings.Contains(status, want) {
			t.Errorf("the status does not state %s: %s", want, status)
		}
	}
}

// Section 2: a cron schedule is ticked by the Runner's own clock, at each
// match in its zone, as any schedule is.
func TestACronScheduleIsTickedByTheRunnersClock(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{})
	due := time.Now().UTC().Truncate(15 * time.Minute)
	tr := w.trigger("", cronSchedule("*/15 * * * *", "America/Toronto"), true, due)
	held, hash := storedTrigger(t, w.s, tr.ID)
	must(t, w.s.scheduleTick(held, hash, due.Add(time.Second)))
	got := w.occurrencesOf(tr.ID)
	if len(got) != 1 || got[0].State != "accepted" || got[0].ScheduledAt != due.Format(time.RFC3339Nano) {
		t.Fatalf("the cron schedule admitted %+v", got)
	}
	if next := w.nextAt(tr.ID); next != due.Add(15*time.Minute).Format(time.RFC3339Nano) {
		t.Fatal("the cron schedule is next due", next)
	}
	// The trigger's tick of the Runner's clock again admits nothing.
	held, hash = storedTrigger(t, w.s, tr.ID)
	must(t, w.s.scheduleTick(held, hash, due.Add(2*time.Second)))
	if got = w.occurrencesOf(tr.ID); len(got) != 1 {
		t.Fatal("the cron schedule admitted twice:", got)
	}
}

// Sections 2 and 5: a shared-interval schedule's times are the interval's
// ticks, from midnight UTC, or with an expression the first tick at or
// after each match; its start is inclusive and its end exclusive.
func TestASharedIntervalScheduleFallsOnItsTicks(t *testing.T) {
	every := Schedule{Kind: "tick", Tick: "15m", Timezone: "UTC", StartAt: "2026-11-15T10:20:00Z"}
	must(t, every.validate())
	for _, c := range []struct{ after, want string }{
		{"2026-11-15T09:00:00Z", "2026-11-15T10:30:00Z"},
		{"2026-11-15T10:30:00Z", "2026-11-15T10:45:00Z"},
		{"2026-11-15T10:37:12Z", "2026-11-15T10:45:00Z"},
		{"2026-11-15T23:59:59Z", "2026-11-16T00:00:00Z"},
	} {
		if got := every.next(instant(c.after)); !got.Equal(instant(c.want)) {
			t.Errorf("after %s: %s, want %s", c.after, got, c.want)
		}
	}
	if got := every.latest(instant("2026-11-15T10:44:59Z")); !got.Equal(instant("2026-11-15T10:30:00Z")) {
		t.Error("the latest tick is", got)
	}
	if got := every.latest(instant("2026-11-15T10:25:00Z")); !got.IsZero() {
		t.Error("a tick before the start is", got)
	}
	at := every
	at.StartAt = "2026-11-15T10:30:00Z"
	if got := at.next(instant("2026-11-15T10:00:00Z")); !got.Equal(instant("2026-11-15T10:30:00Z")) {
		t.Error("a start on a tick is not inclusive:", got)
	}
	at.EndAt = "2026-11-15T10:45:00Z"
	if got := at.next(instant("2026-11-15T10:30:00Z")); !got.IsZero() {
		t.Error("the end is not exclusive:", got)
	}

	matched := Schedule{Kind: "tick", Tick: "15m", Cron: "7 9 * * 1-5", Timezone: "America/Toronto", StartAt: "2026-01-01T00:00:00Z"}
	must(t, matched.validate())
	// Friday 09:07 in Toronto is 14:07 UTC, its tick 14:15; then Monday's.
	if got := matched.next(instant("2026-11-13T00:00:00Z")); !got.Equal(instant("2026-11-13T14:15:00Z")) {
		t.Error("the tick after a match is", got)
	}
	if got := matched.next(instant("2026-11-13T14:15:00Z")); !got.Equal(instant("2026-11-16T14:15:00Z")) {
		t.Error("the next match's tick is", got)
	}
	if got := matched.latest(instant("2026-11-16T14:14:59Z")); !got.Equal(instant("2026-11-13T14:15:00Z")) {
		t.Error("the latest match's tick is", got)
	}
	if got := matched.latest(instant("2026-11-16T14:15:00Z")); !got.Equal(instant("2026-11-16T14:15:00Z")) {
		t.Error("the latest match's tick at the tick is", got)
	}
	for _, bad := range []Schedule{
		{Kind: "tick", Timezone: "UTC", StartAt: every.StartAt},
		{Kind: "tick", Tick: "15m", EverySeconds: 900, Timezone: "UTC", StartAt: every.StartAt},
		{Kind: "tick", Tick: "50m", Timezone: "UTC", StartAt: every.StartAt},
		{Kind: "cron", Cron: "* * * * *", Tick: "15m", Timezone: "UTC", StartAt: every.StartAt},
		{Kind: "daily", Time: "09:00", Cron: "0 9 * * *", Timezone: "UTC", StartAt: every.StartAt},
		{Kind: "once", Tick: "15m", Timezone: "UTC", StartAt: every.StartAt},
	} {
		if err := bad.validate(); refusalCode(err) != "invalid_schedule" {
			t.Errorf("%+v: %v, want invalid_schedule", bad, err)
		}
	}
}

// Section 6: a store's first cron or shared-interval schedule raises its
// schema to 7, from 3, 5 or 6, configured or made with its job; the store
// opens again; one that holds none keeps its schema.
func TestTheFirstCronOrSharedScheduleRaisesTheStoresSchema(t *testing.T) {
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	j := w.job("")
	_, e := w.s.configureTrigger(j.ID, "", 0, scheduleTriggerConfig(time.Now()))
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != storeSchema {
		t.Fatalf("an interval schedule raised the store to %s", schema)
	}
	_, e = w.s.configureTrigger(j.ID, "", 0, cronSchedule("0 9 * * *", "UTC"))
	must(t, e)
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != scheduleStoreSchema {
		t.Fatalf("a cron schedule left the store at %s", schema)
	}
	w.s.Close()
	w.s = openJournal(t, w.cfg)
	if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != scheduleStoreSchema {
		t.Fatalf("the store opened again at %s", schema)
	}

	for _, from := range []struct {
		schema string
		raise  func(*tickWorld)
	}{
		{scopedStoreSchema, func(w *tickWorld) { w.job("scope-a") }},
		{retryStoreSchema, func(w *tickWorld) {
			_, e := w.s.createJobWith(requester{}, "Retrying", journalRelease(t, w.s, w.runtime).ID, "", nil, &RetryPolicy{2, 5, 5})
			must(t, e)
		}},
	} {
		w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
		from.raise(w)
		if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != from.schema {
			t.Fatalf("the store is at %s, not %s", schema, from.schema)
		}
		// A job made with its first trigger, a shared-interval one.
		c := sharedSchedule("15m", "")
		_, e := w.s.createJobWith(requester{}, "Made ticked", journalRelease(t, w.s, w.runtime).ID, "", &c, nil)
		must(t, e)
		if schema := storeSchemaOf(t, sqliteDB(w.s)); schema != scheduleStoreSchema {
			t.Fatalf("a job made with a shared-interval trigger left the store at %s, from %s", schema, from.schema)
		}
	}
}

// Section 7: the cost of one tick at several hundred jobs, each with one
// shared-interval trigger due, past the installation's trigger limit, which
// the triggers are written around: the test reports it, and holds what the
// tick did (each trigger ticked once, the queue's room taken and the rest
// skipped queue-full), never its time.
func TestASharedTickAtSeveralHundredJobsReportsItsCost(t *testing.T) {
	const jobs = 400
	w := newTickWorld(t, ScheduleSettings{SharedIntervals: []string{"15m"}})
	tick := w.at(time.Now()).UTC().Truncate(15 * time.Minute)
	c := sharedSchedule("15m", "")
	for i := range jobs {
		j := w.job([]string{"", "scope-a", "scope-b", "scope-c"}[i%4])
		tr := Trigger{ID: id("trg_"), JobID: j.ID, Revision: 1, Authority: "local", Config: c, CreatedAt: now(), UpdatedAt: now(), Scope: j.Scope, NextAt: tick.Format(time.RFC3339Nano)}
		must(t, w.s.store.update(func(tx runTx) error {
			if err := tx.createTrigger(tr); err != nil {
				return err
			}
			if err := tx.putTrigger(tr, "", true); err != nil {
				return err
			}
			return tx.raiseSchema(scheduleStoreSchema)
		}))
	}
	doc := readOpenAPI(t)
	started := time.Now()
	r := tickResult(t, doc, w.tick("15m", ticked(tick)))
	first := time.Since(started)
	started = time.Now()
	again := tickResult(t, doc, w.tick("15m", ticked(tick)))
	repeated := time.Since(started)
	if r.Occurrences != jobs || again.Occurrences != 0 {
		t.Fatalf("the tick admitted %d, and again %d", r.Occurrences, again.Occurrences)
	}
	var accepted, full int
	must(t, sqliteDB(w.s).QueryRow("SELECT count(CASE WHEN json_extract(record,'$.state')='accepted' THEN 1 END), count(CASE WHEN json_extract(record,'$.reason')='queue-full' THEN 1 END) FROM occurrences").Scan(&accepted, &full))
	if accepted != queueLimit || full != jobs-queueLimit {
		t.Fatalf("%d accepted and %d skipped queue-full", accepted, full)
	}
	t.Logf("one tick of %d due triggers on the sqlite kind: %s (%s each); the same tick again: %s (this machine's figure, not a bound)", jobs, first.Round(time.Millisecond), (first / jobs).Round(time.Microsecond), repeated.Round(time.Millisecond))
}
