package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Section 9 (#286): a start holds every enabled trigger to the schedule
// settings in force, reports each one they refuse, in the journal once and
// in GET /v1/status, and changes none. On both kinds: the postgresql kind's
// when a server is named (JPACK_TEST_POSTGRESQL).
func TestAStartReportsTheTriggersOutsideItsScheduleSettings(t *testing.T) {
	for _, kind := range []string{"sqlite", "postgresql"} {
		t.Run(kind, func(t *testing.T) {
			cfg := journalConfig(t)
			if kind == "postgresql" {
				p := pgRunners(t)
				cfg.PostgreSQL = &p.conn
			}
			startReportsTheTriggersOutsideItsSettings(t, cfg)
		})
	}
}

func startReportsTheTriggersOutsideItsSettings(t *testing.T, cfg Config) {
	cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m", "1h"}}
	w := &tickWorld{t: t, cfg: cfg, s: openJournal(t, cfg)}
	t.Cleanup(func() { w.s.Close() })
	w.runtime = completingRuntime(t, cfg.Dir)
	due := time.Now().UTC().Truncate(15 * time.Minute).Add(time.Hour)
	removed := routedTrigger(w, "scope-a", sharedSchedule("1h", ""), true)
	kept := routedTrigger(w, "scope-b", sharedSchedule("15m", ""), true)
	short := routedTrigger(w, "", cronSchedule("*/5 * * * *", "UTC"), true)
	daily := routedTrigger(w, "", cronSchedule("0 3 * * *", "UTC"), true)
	paused := routedTrigger(w, "scope-a", sharedSchedule("1h", ""), false)
	before := map[string]Trigger{}
	for _, tr := range []Trigger{removed, kept, short, daily, paused} {
		before[tr.ID], _ = storedTrigger(t, w.s, tr.ID)
	}
	// Under the settings the triggers were enabled under, nothing is
	// reported.
	w.s.Close()
	w.s = openJournal(t, w.cfg)
	if got := outsideSettings(t, w); len(got) != 0 {
		t.Fatalf("a start under the settings in force reported %+v", got)
	}
	w.statusStates(`"unknownSharedInterval":{"count":0,"triggers":[]}`, `"outsideBounds":{"count":0,"triggers":[]}`)

	// The operator removes the interval 1h and raises the minimum to 10
	// minutes.
	w.s.Close()
	w.cfg.Schedules = ScheduleSettings{SharedIntervals: []string{"15m"}, MinimumSeconds: 600}
	w.s = openJournal(t, w.cfg)
	got := outsideSettings(t, w)
	if len(got) != 2 {
		t.Fatalf("the start journaled %d entries, not one for each of the two triggers: %+v", len(got), got)
	}
	for i, want := range []struct {
		trigger Trigger
		scope   string
		code    string
		means   string
	}{{removed, "scope-a", "unknown_shared_interval", "This Runner does not tick the trigger."}, {short, "", "cron_interval_too_short", "goes on ticking the trigger"}} {
		e := got[i]
		if e.By.Kind != "runner" || e.Concerns.Trigger != want.trigger.ID || e.Concerns.Job != want.trigger.JobID || e.Revision == nil || *e.Revision != before[want.trigger.ID].Revision || e.Code != want.code || e.Scope != want.scope || !strings.Contains(e.Reason, want.means) {
			t.Errorf("entry %d: %s", i, encode(e))
		}
		if strings.Contains(e.Reason, "scope-") {
			t.Errorf("the reason names the scope identifier: %s", e.Reason)
		}
	}
	w.statusStates(`"unknownSharedInterval":{"count":1,"triggers":["`+removed.ID+`"]}`, `"outsideBounds":{"count":1,"triggers":["`+short.ID+`"]}`)

	// No trigger is changed, and nothing but the start writes the entry: a
	// tick of the interval kept and a pass of the Runner's clock add none.
	for id, was := range before {
		if now, _ := storedTrigger(t, w.s, id); now.Revision != was.Revision || now.Paused != was.Paused || now.NextAt != was.NextAt || string(encode(now.Config)) != string(encode(was.Config)) {
			t.Errorf("the start changed trigger %s: %+v, was %+v", id, now, was)
		}
	}
	w.at(due.Add(time.Second))
	if r := w.tick("15m", ticked(due)); r.Code != 200 {
		t.Fatalf("the tick of the interval kept: %d %s", r.Code, r.Body.String())
	}
	must(t, w.s.automationTick(due.Add(time.Second)))
	if again := outsideSettings(t, w); len(again) != 2 {
		t.Fatalf("after a tick and a pass of the clock, %d entries", len(again))
	}
}

// routedTrigger configures c through the routes on a new job under scope
// ("" for none), and enables it when enabled.
func routedTrigger(w *tickWorld, scope string, c TriggerConfig, enabled bool) Trigger {
	w.t.Helper()
	held := ""
	if scope != "" {
		held = "?scope=" + scope
	}
	got := serve(w.t, w.s, "POST", "/v1/jobs/"+w.job(scope).ID+"/triggers"+held, string(encode(map[string]any{"config": c})), nil)
	if got.Code != 200 {
		w.t.Fatal(got.Code, got.Body.String())
	}
	var tr Trigger
	must(w.t, json.Unmarshal(got.Body.Bytes(), &tr))
	if enabled {
		if got = serve(w.t, w.s, "POST", "/v1/triggers/"+tr.ID+"/state"+held, string(encode(map[string]any{"revision": tr.Revision, "paused": false, "reviewed": true})), nil); got.Code != 200 {
			w.t.Fatal(got.Code, got.Body.String())
		}
		must(w.t, json.Unmarshal(got.Body.Bytes(), &tr))
	}
	return tr
}

// outsideSettings are the journal's trigger.outside-settings entries, oldest
// first.
func outsideSettings(t *testing.T, w *tickWorld) []Event {
	t.Helper()
	var out []Event
	for after := int64(0); ; {
		page, e := eventPageFrom(w.s.store, after, journalFilter{})
		must(t, e)
		for _, e := range page.Items {
			if e.Kind == outsideSettingsKind {
				out = append(out, e)
			}
		}
		if !page.More {
			return out
		}
		after = page.Next
	}
}

// statusStates holds GET /v1/status to stating each of want under
// triggersOutsideSettings.
func (w *tickWorld) statusStates(want ...string) {
	w.t.Helper()
	got := serve(w.t, w.s, "GET", "/v1/status", "", nil)
	var status struct {
		Held json.RawMessage `json:"triggersOutsideSettings"`
	}
	must(w.t, json.Unmarshal(got.Body.Bytes(), &status))
	held := string(status.Held)
	if held == "" {
		w.t.Fatalf("the status states no triggersOutsideSettings: %s", got.Body.String())
	}
	for _, s := range want {
		if !strings.Contains(held, s) {
			w.t.Errorf("the status does not state %s: %s", s, held)
		}
	}
}
