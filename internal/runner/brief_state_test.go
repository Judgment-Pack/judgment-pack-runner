package runner

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBriefReservationAndImmutableHistory(t *testing.T) {
	now := time.Now().UTC()
	d := BriefStore{Version: 1, Subjects: map[string]BriefHistory{}}
	source := json.RawMessage(`{"kind":"case","record":{"facts":{"value":1}}}`)
	c := BriefCommand{Action: "begin", Subject: "case:one", Snapshot: source, Model: "fixture"}
	if e := applyBrief(&d, c, now); e != nil {
		t.Fatal(e)
	}
	first := d.Subjects[c.Subject].Pending.Token
	if e := applyBrief(&d, c, now); e == nil {
		t.Fatal("parallel generation admitted")
	}
	good := BriefText{Context: "Context.", Findings: "Findings.", Uncertainty: "Unverified.", NextAction: "Review."}
	c = BriefCommand{Action: "complete", Subject: c.Subject, Token: first, Text: BriefText{}}
	if e := applyBrief(&d, c, now); e == nil {
		t.Fatal("malformed AI reply saved")
	}
	c.Text = good
	if e := applyBrief(&d, c, now); e != nil {
		t.Fatal(e)
	}
	if e := applyBrief(&d, c, now.Add(time.Hour)); e != nil {
		t.Fatal("save retry failed", e)
	}
	h := d.Subjects[c.Subject]
	if len(h.Revisions) != 1 || h.Pending != nil || !sameBriefJSON(h.Revisions[0].Snapshot, source) {
		t.Fatal("snapshot or history changed")
	}
	c = BriefCommand{Action: "begin", Subject: c.Subject, Snapshot: json.RawMessage(`{"new":true}`), Model: "fixture"}
	if e := applyBrief(&d, c, now); e == nil {
		t.Fatal("stale revision overwrote current")
	}
	c.ExpectedRevision = 1
	if e := applyBrief(&d, c, now); e != nil {
		t.Fatal(e)
	}
	cancelled := d.Subjects[c.Subject].Pending.Token
	if e := applyBrief(&d, BriefCommand{Action: "cancel", Subject: c.Subject, Token: cancelled}, now); e != nil {
		t.Fatal(e)
	}
	if len(d.Subjects[c.Subject].Revisions) != 1 {
		t.Fatal("cancel removed prior brief")
	}
	if e := applyBrief(&d, c, now); e != nil {
		t.Fatal(e)
	}
	expired := d.Subjects[c.Subject].Pending.Token
	if e := applyBrief(&d, BriefCommand{Action: "complete", Subject: c.Subject, Token: expired, Text: good}, now.Add(time.Hour)); e == nil {
		t.Fatal("expired writer saved")
	}
	if e := applyBrief(&d, c, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if e := applyBrief(&d, BriefCommand{Action: "cancel", Subject: c.Subject, Token: expired}, now.Add(time.Hour)); e == nil {
		t.Fatal("old tab cancelled new writer")
	}
	token := d.Subjects[c.Subject].Pending.Token
	if e := applyBrief(&d, BriefCommand{Action: "complete", Subject: c.Subject, Token: token, Text: good}, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	h = d.Subjects[c.Subject]
	if len(h.Revisions) != 2 || !sameBriefJSON(h.Revisions[0].Snapshot, source) {
		t.Fatal("regeneration changed historical source")
	}
	data, _ := json.Marshal(d)
	if e := validateBriefs(data); e != nil {
		t.Fatal(e)
	}
	var restored BriefStore
	_ = json.Unmarshal(data, &restored)
	if len(restored.Subjects[c.Subject].Revisions) != 2 {
		t.Fatal("reload lost revisions")
	}
}
