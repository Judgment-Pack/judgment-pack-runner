package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Thresholds come from the complete handwritten producer fixture, independently
// of Runner's preparation/verification result. No expected audit is recorded.
func TestColumnNegotiationTrustedProducerCompleteBounds(t *testing.T) {
	for _, key := range []string{"responseBytes", "carrierBytes", "originalSnapshotBytes", "profilesBytes"} {
		t.Run(key, func(t *testing.T) {
			for _, delta := range []int{100, 0, -1} {
				f := selectedProducer(t)
				size := map[string]int{"responseBytes": len(f.input.Source.Sources["desk"].Response), "carrierBytes": len(f.carrier()), "originalSnapshotBytes": len(f.snapshot)}[key]
				if key == "profilesBytes" {
					// Decimal-width fixed point counts the complete profile including its
					// own declared maximum, rather than omitting that member from the budget.
					size = len(encode([]InputProfile{f.profile}))
					for range 4 {
						f.profile.ConversionContracts[0].Limits[key] = size + delta
						size = len(encode([]InputProfile{f.profile}))
					}
				}
				f.profile.ConversionContracts[0].Limits[key] = size + delta
				f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
				f.sign(t)
				t.Logf("complete %s threshold=%d delta=%d", key, size, delta)
				code, plan, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil)
				if delta >= 0 {
					if code != 200 || plan.Input == nil {
						t.Fatalf("lower positive complete %s bound rejected delta=%d: %d %s", key, delta, code, body)
					}
					fixture := producerPreparationFixture(*plan.Input, f.profile)
					fixture.ConversionContract = selectedContract()
					if r := VerifyPreparation(fixture); r.Result != ResultReading {
						t.Fatalf("offline exact lower bound: %+v", r)
					}
					var round Input
					if strictJSON(encode(*plan.Input), &round) != nil {
						t.Fatal("persisted input")
					}
					if _, e := normalizeV2(round, []InputProfile{f.profile}, f.at); e != nil {
						t.Fatal("persisted lower capability", e)
					}
				} else {
					if code < 400 || !strings.Contains(body, "exceeds negotiated limit") {
						t.Fatalf("actual one-over %s accepted or wrong refusal: %d %s", key, code, body)
					}
					// Shared offline owner must independently measure this same artifact.
					if _, e := prepareV2Mode(f.input, []InputProfile{f.profile}, f.at, false, nil, true); e == nil {
						t.Fatal("offline one-over complete artifact accepted")
					} else if _, c := verificationOutcome(e); c != codeOverBound {
						t.Fatal("offline wrong bound finding", c, e)
					}
				}
			}
		})
	}
	f := selectedProducer(t)
	f.profile.ConversionContracts[0].Limits["responseBytes"] = 2621440
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	if code, plan, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil); code != 200 || plan.Input == nil {
		t.Fatal("valid small artifact under lower report refused", code, body)
	}
	// Enlarged reports cannot enlarge the fixed local ceiling.
	c := f.profile.ConversionContracts[0]
	c.Limits["responseBytes"] = columnResponseLimit + 1
	got, e := conversionCapabilityLimits([]ConversionCapability{c}, "producer-carrier", true)
	if e != nil || got["responseBytes"] != columnResponseLimit {
		t.Fatal("report enlarged local ceiling", got, e)
	}
	// Whitespace outside the envelope still counts, despite unchanged signature.
	f.profile.ConversionContracts[0].Limits["responseBytes"] = len(f.input.Source.Sources["desk"].Response)
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	v := f.input.Source.Sources["desk"]
	v = SourceValue{RetainedResponseJSON: string(v.Response) + " "}
	f.input.Source.Sources["desk"] = v
	if code, _, body := scopedEntry(t, f.input, []InputProfile{f.profile}, nil); code < 400 || !strings.Contains(body, "exceeds negotiated limit") {
		t.Fatal("response whitespace escaped budget", code, body)
	}
}

func TestColumnNegotiationPreflightAndPlan(t *testing.T) {
	f := selectedProducer(t)
	f.input.Source.Sources = nil
	f.profile.ConversionContracts[0].Limits["responseBytes"] = 6000
	f.profile.ConversionContracts[0].Limits["grantBytes"] = 5000
	f.profile.ConversionContracts[0].Limits["adapterStdoutBytes"] = 5500
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	plan, e := nextInput(f.input, []InputProfile{f.profile}, f.at)
	if e != nil || plan.Next == nil || !plan.Next.RetainedColumn || plan.Next.ConversionLimits["responseBytes"] != 6000 || plan.Next.ConversionLimits["grantBytes"] != 5000 || plan.Next.ConversionLimits["adapterStdoutBytes"] != 5500 {
		t.Fatal("plan lost negotiated limits", plan, e)
	}
	// This known complete profiles overage must refuse before planning a call.
	f.profile.ConversionContracts[0].Limits["profilesBytes"] = 1
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	if p, e := nextInput(f.input, []InputProfile{f.profile}, f.at); e == nil || p.Next != nil {
		t.Fatal("known profile overage planned acquisition", p, e)
	}
	for _, bad := range []func(*ConversionCapability){func(c *ConversionCapability) { c.Role = "preview" }, func(c *ConversionCapability) { c.ProducerGrammar = "other" }, func(c *ConversionCapability) { c.Spec.Revision = strings.Repeat("0", 40) }, func(c *ConversionCapability) { delete(c.Limits, "responseBytes") }, func(c *ConversionCapability) { c.Limits["responseBytes"] = 0 }} {
		p := selectedProducer(t)
		p.input.Source.Sources = nil
		bad(&p.profile.ConversionContracts[0])
		p.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(p.profile)
		if plan, e := nextInput(p.input, []InputProfile{p.profile}, p.at); e == nil || plan.Next != nil {
			t.Fatal("invalid owner planned effects", plan, e)
		}
	}
}

// This is real SourceWorker HTTP/SQLite/restart custody. The Gateway's signed
// producer is a fixture; it cannot prove actual grant creation/adapter stdout.
func TestColumnNegotiationWorkerActualResponseCustody(t *testing.T) {
	for _, key := range []string{"responseBytes", "carrierBytes", "originalSnapshotBytes"} {
		for _, delta := range []int{0, -1} {
			t.Run(key+map[int]string{0: "/exact", -1: "/one-over"}[delta], func(t *testing.T) {
				f := selectedProducer(t)
				operation := strings.Repeat("a", 32)
				raw := []byte(`{"result":` + string(f.carrier()) + `,"receipt":{"sessionId":"async.` + operation + `","callIndex":0}}`)
				limits := conversionRoleLimits("producer-carrier")
				limits[key] = map[string]int{"responseBytes": len(raw), "carrierBytes": len(f.carrier()), "originalSnapshotBytes": len(f.snapshot)}[key] + delta
				var calls atomic.Int32
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/seal" {
						write(w, 200, map[string]bool{"sealed": true})
						return
					}
					calls.Add(1)
					w.Write(raw)
				}))
				defer gateway.Close()
				cfg := SourceWorkerConfig{Dir: filepath.Join(sourceWorkerTempDir(t), "worker"), Gateway: gateway.URL}
				worker, e := OpenSourceWorker(cfg)
				if e != nil {
					t.Fatal(e)
				}
				defer func() {
					if worker != nil {
						worker.Close()
					}
				}()
				req := sourceRequest{RetainedColumn: true, ConversionContract: selectedContract(), ConversionLimits: limits, ID: operation, Gateway: gateway.URL, Source: "desk-inputs", Arguments: json.RawMessage(producerArgs), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
				if _, _, e = worker.submit(req); e != nil {
					t.Fatal(e)
				}
				worker.workers.Wait()
				worker.Close()
				worker = nil
				worker, e = OpenSourceWorker(cfg)
				if e != nil {
					t.Fatal(e)
				}
				status, e := worker.status(operation)
				if e != nil {
					t.Fatal(e)
				}
				if calls.Load() != 1 {
					t.Fatal("acquisition repeated", calls.Load())
				}
				if delta == 0 {
					if status.State != "completed" || status.RetainedResponseJSON != string(raw) {
						t.Fatal("exact response failed durable custody", status.State, status.Reason)
					}
				} else if status.State != "needs-attention" || status.RetainedResponseJSON != "" || len(status.Response) > 0 {
					t.Fatal("oversized response retained", status.State)
				}
				changed := req
				changed.ConversionLimits = conversionRoleLimits("producer-carrier")
				if _, code, e := worker.submit(changed); e == nil || code != 409 {
					t.Fatal("same operation rebound reported maxima", code, e)
				}
				changed = req
				changed.RetainedColumn = false
				changed.ConversionLimits = nil
				if _, code, e := worker.submit(changed); e == nil || code != 409 {
					t.Fatal("producer operation downgraded into ordinary request", code, e)
				}
			})
		}
	}
	worker, req, barrier, calls := workerFixture(t)
	req.ConversionContract = selectedContract()
	req.RetainedColumn = true
	if _, _, e := worker.submit(req); e == nil || calls.Load() != 0 {
		t.Fatal("missing selected ceilings dispatched")
	}
	close(barrier)
}

func TestColumnNegotiationRuntimeMeasuredBeforeEffects(t *testing.T) {
	input := columnCase(t, "text", `"A"`)
	input.Source.ConversionContract = selectedContract()
	// A nonstandard frame pointer must be measured, not just a member named retained.
	input.Source.Case = bytes.Replace(input.Source.Case, []byte(`"retained"`), []byte(`"other"`), 1)
	input.Source.Mapping.Case.Facts[0].Source = "/other"
	frame, ok := rawPointer(input.Source.Case, "/other")
	if !ok {
		t.Fatal("handwritten frame pointer")
	}
	doc := encode(input.Source.Mapping)
	pack := strings.Repeat("x", len(doc))
	for _, key := range []string{"documentBytes", "frameBytes"} {
		for _, delta := range []int{100, 0, -1} {
			limits := conversionRoleLimits("document-validation")
			limits[key] = map[string]int{"documentBytes": len(doc), "frameBytes": len(frame)}[key] + delta
			e := runtimeConversionInput(limits, input, pack)
			if (e == nil) != (delta >= 0) {
				t.Fatal("Runtime min actual artifact admission", key, delta, e)
			}
		}
	}
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	bin := filepath.Join(dir, "runtime")
	owner := ConversionCapabilities("document-validation")[0]
	owner.Limits["documentBytes"] = 1
	if e := os.WriteFile(report, encode(map[string]any{"capabilities": map[string]any{"conversionContracts": []ConversionCapability{owner}}}), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(bin, []byte("#!/bin/sh\nexec /bin/cat '"+report+"'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	evaluation := filepath.Join(dir, "evaluation")
	calls := 0
	invoker := func(context.Context, string, string, string, []string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	if _, _, _, _, e := evaluateWithInvoker(context.Background(), bin, Release{Pack: pack}, input, evaluation, true, signingInputs{}, invoker); e == nil {
		t.Fatal("oversized Runtime document invoked")
	}
	if calls != 0 {
		t.Fatal("evaluation effect before negotiated admission", calls)
	}
	if _, e := os.Stat(evaluation); !os.IsNotExist(e) {
		t.Fatal("evaluation directory created before negotiated admission", e)
	}
}

func TestColumnNegotiationAutomaticOwnerBounds(t *testing.T) {
	for _, key := range []string{"responseBytes", "carrierBytes", "originalSnapshotBytes"} {
		for _, delta := range []int{0, -1} {
			t.Run(key+map[int]string{0: "/exact", -1: "/one-over"}[delta], func(t *testing.T) {
				f := selectedProducer(t)
				rawFor := func(session string) []byte {
					return []byte(`{"result":` + string(f.carrier()) + `,"receipt":{"sessionId":"` + session + `","callIndex":0}}`)
				}
				limits := conversionRoleLimits("producer-carrier")
				limits[key] = map[string]int{"responseBytes": len(rawFor("job." + strings.Repeat("a", 32))), "carrierBytes": len(f.carrier()), "originalSnapshotBytes": len(f.snapshot)}[key] + delta
				var calls atomic.Int32
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/seal" {
						write(w, 200, map[string]bool{"sealed": true})
						return
					}
					var q struct {
						Session string `json:"session"`
					}
					if json.NewDecoder(r.Body).Decode(&q) != nil {
						w.WriteHeader(400)
						return
					}
					calls.Add(1)
					w.Write(rawFor(q.Session))
				}))
				defer gateway.Close()
				service := &Service{cfg: Config{GatewayConnections: []GatewayConnection{{Profile: f.profile.ID, URL: gateway.URL}}}}
				value, e := service.acquirePlannedOperation(context.Background(), PlannedSource{RetainedColumn: true, retainResponse: true, ConversionContract: selectedContract(), ConversionLimits: limits, Name: "desk", Kind: "operation", Profile: f.profile.ID, Source: "desk-inputs", Arguments: json.RawMessage(producerArgs)})
				if calls.Load() != 1 {
					t.Fatal("automatic acquisition repeated or absent", calls.Load())
				}
				if delta == 0 {
					if e != nil || value.RetainedResponseJSON == "" {
						t.Fatal("automatic exact artifact refused", e)
					}
				} else if e == nil || value.RetainedResponseJSON != "" || len(value.Response) > 0 {
					t.Fatal("automatic oversized custody accepted", e)
				}
			})
		}
	}
}

func TestColumnNegotiationWorkerStatusRechecksPersistedBound(t *testing.T) {
	worker, req, barrier, calls := workerFixture(t)
	req.ConversionContract = selectedContract()
	req.RetainedColumn = true
	req.ConversionLimits = conversionRoleLimits("producer-carrier")
	req.ConversionLimits["responseBytes"] = 1
	if _, _, e := worker.submit(req); e != nil {
		t.Fatal(e)
	}
	close(barrier)
	worker.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("expected one actual response")
	}
	// A corrupted persisted completion cannot enlarge its frozen request ceiling.
	status := sourceStatus{ID: req.ID, State: "completed", Deadline: req.Deadline, RetainedResponseJSON: `{}`}
	if _, e := worker.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=?", status.State, string(encode(status)), req.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := worker.status(req.ID); e == nil {
		t.Fatal("persisted response over negotiated limit accepted")
	}
}

func TestColumnNegotiationRuntimeEachArtifact(t *testing.T) {
	f := selectedProducer(t)
	limits := conversionRoleLimits("document-validation")
	limits["frameBytes"] = len(f.frame) - 1
	if e := runtimeConversionInput(limits, f.input, "{}"); e == nil {
		t.Fatal("generated frame over report admitted")
	}
	limits["frameBytes"] = len(f.frame)
	if e := runtimeConversionInput(limits, f.input, "{}"); e != nil {
		t.Fatal("generated frame exact report refused", e)
	}
	limits = conversionRoleLimits("document-validation")
	mappingSize := len(encode(f.input.Source.Mapping))
	limits["documentBytes"] = mappingSize
	f.input.Source.Import = &DialectImport{Dialect: json.RawMessage(`{}` + strings.Repeat(" ", mappingSize-1)), Binding: json.RawMessage(`{}`)}
	if e := runtimeConversionInput(limits, f.input, "{}"); e == nil {
		t.Fatal("published document over report admitted")
	}
	f.input.Source.Import.Dialect = []byte(`{}` + strings.Repeat(" ", mappingSize-2))
	if e := runtimeConversionInput(limits, f.input, "{}"); e != nil {
		t.Fatal("published and bound exact documents refused", e)
	}
	if e := runtimeConversionInput(limits, f.input, strings.Repeat("x", mappingSize+1)); e == nil {
		t.Fatal("pack document over report admitted")
	}
}

func TestColumnNegotiationResumeRefusesChangedLimitsBeforeCall(t *testing.T) {
	s, cfg, o, remote := preparationFixture(t)
	f := selectedProducer(t)
	f.input.Source.Sources = nil
	s.cfg.InputProfiles = []InputProfile{f.profile}
	s.cfg.GatewayConnections[0].Profile = f.profile.ID
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	old := conversionRoleLimits("producer-carrier")
	old["responseBytes"] = 6000
	g := cfg.GatewayConnections[0]
	request := sourceRequest{RetainedColumn: true, ConversionContract: selectedContract(), ConversionLimits: old, ID: strings.Repeat("b", 32), Gateway: g.URL, Source: f.profile.Source, Arguments: json.RawMessage(producerArgs), Deadline: deadline}

	for _, kind := range []string{"limits", "missing-discriminator", "downgrade"} {
		t.Run(kind, func(t *testing.T) {
			heldRequest := request
			if kind == "missing-discriminator" {
				heldRequest.RetainedColumn = false
			}
			if kind == "downgrade" {
				heldRequest.RetainedColumn = false
				heldRequest.ConversionLimits = nil
			}
			o.State = "waiting"
			o.Preparation = &SourcePreparation{StartedAt: now(), Deadline: deadline, Input: &f.input, Tasks: []SourceTask{{Name: "desk", ID: request.ID, State: "running", Gateway: g.URL, OperationsURL: g.OperationsURL, Profile: f.profile.ID, Request: encode(heldRequest)}}}
			if e := s.saveOccurrence(o); e != nil {
				t.Fatal(e)
			}
			if e := s.advancePreparation(context.Background(), o); e != nil {
				t.Fatal(e)
			}
			held, e := s.occurrence(o.ID)
			if e != nil {
				t.Fatal(e)
			}
			remote.mu.Lock()
			calls := len(remote.requests)
			remote.mu.Unlock()
			if calls != 0 || held.State != "needs-attention" || !strings.Contains(held.Reason, "not rebound") {
				t.Fatal("changed limits reached source owner", calls, held.State, held.Reason)
			}
		})
	}

}

func TestColumnNegotiationOfflineCompleteProfilesWhitespace(t *testing.T) {
	f := selectedProducer(t)
	size := len(encode([]InputProfile{f.profile}))
	for range 4 {
		f.profile.ConversionContracts[0].Limits["profilesBytes"] = size
		size = len(encode([]InputProfile{f.profile}))
	}
	f.profile.ConversionContracts[0].Limits["profilesBytes"] = size
	f.input.Source.Mapping.Sources[0].ProfileDigest = profileHash(f.profile)
	prepared, e := normalizeV2(f.input, []InputProfile{f.profile}, f.at)
	if e != nil {
		t.Fatal(e)
	}
	fixture := producerPreparationFixture(prepared, f.profile)
	fixture.ConversionContract = selectedContract()
	if len(fixture.Profiles) != size {
		t.Fatal("handwritten profile fixed point", len(fixture.Profiles), size)
	}
	if report := VerifyPreparation(fixture); report.Result != ResultReading {
		t.Fatal("offline exact profiles refused", report)
	}
	fixture.Profiles = append(fixture.Profiles, ' ')
	if report := VerifyPreparation(fixture); report.Code != codeOverBound {
		t.Fatal("complete offline profiles whitespace escaped lower budget", report)
	}
	// verify-run's actual ParseInputProfiles owner retains the same file count.
	profiles, e := ParseInputProfiles(fixture.Profiles)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = normalizeV2(f.input, profiles, f.at); e == nil {
		t.Fatal("run reader discarded complete profiles file count")
	}
}

func TestColumnNegotiationMixedOrdinaryAndProducer(t *testing.T) {
	f := selectedProducer(t)
	ordinary, key := testProfile(t)
	args := json.RawMessage(`{"tool":"lookup","arguments":{}}`)
	src := MappingSource{Name: "ordinary", Kind: "operation", Profile: ordinary.ID, ProfileDigest: profileHash(ordinary), MaxAge: 300, Arguments: args, Read: SourceRead{Copy: &CopyMapping{Facts: []FactMapping{{Target: "/ordinary", Source: "/ok"}}, Evidence: []EvidenceMapping{}}}}
	f.input.Source.Mapping.Sources = append([]MappingSource{src}, f.input.Source.Mapping.Sources...)
	f.input.Source.Mapping.Admits.Facts["/ordinary"] = []string{"record"}
	profiles := []InputProfile{ordinary, f.profile}
	full := cloneInput(f.input)
	full.Source.Sources["ordinary"] = SourceValue{Response: signResponse(t, ordinary, key, args, json.RawMessage(`{"ok":"yes"}`), f.at, 1)}
	prepared, e := normalizeV2(full, profiles, f.at)
	if e != nil {
		t.Fatal("mixed ordinary/producer preparation", e)
	}
	if !bytes.Contains(prepared.Facts, []byte(`"ordinary":"yes"`)) {
		t.Fatal("ordinary mixed fact lost")
	}
	pending := cloneInput(f.input)
	pending.Source.Sources = nil
	plan, e := nextInput(pending, profiles, f.at)
	if e != nil || plan.Next == nil || plan.Next.Name != "ordinary" || plan.Next.RetainedColumn || plan.Next.ConversionLimits != nil {
		t.Fatal("ordinary selected source required producer authority", plan, e)
	}
	pending.Source.Sources = map[string]SourceValue{"ordinary": full.Source.Sources["ordinary"]}
	plan, e = nextInput(pending, profiles, f.at)
	if e != nil || plan.Next == nil || plan.Next.Name != "desk" || !plan.Next.RetainedColumn || len(plan.Next.ConversionLimits) != 6 {
		t.Fatal("producer source lost trusted discriminator/ceilings", plan, e)
	}
	// Worker supports the ordinary acquisition under the same selection without
	// inventing producer support, and still retains exact response custody.
	worker, req, barrier, calls := workerFixture(t)
	req.ConversionContract = selectedContract()
	if _, _, e := worker.submit(req); e != nil {
		t.Fatal("ordinary selected worker refused", e)
	}
	close(barrier)
	worker.workers.Wait()
	status, e := worker.status(req.ID)
	if e != nil || status.State != "completed" || status.RetainedResponseJSON == "" || calls.Load() != 1 {
		t.Fatal("ordinary selected custody changed", status, e)
	}
	producerReq := req
	producerReq.ID = strings.Repeat("d", 32)
	producerReq.ConversionLimits = conversionRoleLimits("producer-carrier")
	if _, _, e := worker.submit(producerReq); e == nil {
		t.Fatal("missing producer discriminator accepted ceilings")
	}
}

func TestColumnNegotiationRequestDiscriminatorClosure(t *testing.T) {
	base := sourceRequest{ID: strings.Repeat("a", 32), Gateway: "http://127.0.0.1:1", Source: "ordinary", Arguments: json.RawMessage(`{}`), Deadline: "2026-10-27T00:00:00Z"}
	raw := string(encode(base))
	for _, member := range []string{`"retainedColumn":null,`, `"retainedColumn":"false",`, `"RetainedColumn":false,`} {
		var req sourceRequest
		if strictJSON([]byte(strings.Replace(raw, "{", "{"+member, 1)), &req) == nil {
			t.Fatal("malformed discriminator accepted", member)
		}
	}
}
