package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

type manualWireVector struct {
	Name          string          `json:"name"`
	Request       json.RawMessage `json:"request"`
	Reply         json.RawMessage `json:"reply"`
	RequestDigest string          `json:"requestDigest"`
	RequestLine   string          `json:"requestLine"`
	ReplyLine     string          `json:"replyLine"`
}
type manualWireFixture struct {
	Synthetic struct {
		Boot ManualAdmissionBinding `json:"boot"`
	} `json:"synthetic"`
	Vectors []manualWireVector `json:"vectors"`
	Invalid []struct {
		Name string `json:"name"`
		Line string `json:"line"`
	} `json:"invalidVectors"`
}

func manualFixture(t *testing.T) manualWireFixture {
	t.Helper()
	raw, err := os.ReadFile("../../docs/design/manual-admission-wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var f manualWireFixture
	if json.Unmarshal(raw, &f) != nil {
		t.Fatal("fixture")
	}
	return f
}
func manualVectorBody[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var envelope manualRequestEnvelope
	if manualDecode(raw, &envelope) != nil {
		t.Fatal("fixture envelope")
	}
	var out T
	if manualDecode(envelope.Body, &out) != nil {
		t.Fatal("fixture body")
	}
	return out
}
func manualScenario(t *testing.T, vectors []manualWireVector, mutate func(int, []byte) []byte) (ManualAuthority, <-chan error) {
	t.Helper()
	a, b := net.Pipe()
	client, err := NewManualAuthority(a, manualFixture(t).Synthetic.Boot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); b.Close() })
	done := make(chan error, 1)
	go func() {
		defer b.Close()
		reader := bufio.NewReaderSize(b, manualFrameLimit)
		for i, v := range vectors {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				done <- err
				return
			}
			actual, e := canonical(line)
			expected, _ := canonical(v.Request)
			if e != nil || !bytes.Equal(actual, expected) {
				done <- fmt.Errorf("request %d differs: %s", i, line)
				return
			}
			reply, _ := canonical(v.Reply)
			if mutate != nil {
				reply = mutate(i, reply)
			}
			if _, err = b.Write(append(reply, '\n')); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return client, done
}
func TestManualAdmissionWireVectors(t *testing.T) {
	f := manualFixture(t)
	for _, v := range f.Vectors {
		if len(v.Request) == 0 {
			continue
		}
		var envelope manualRequestEnvelope
		if manualDecode(v.Request, &envelope) != nil {
			t.Fatal(v.Name)
		}
		var body any
		switch envelope.Method {
		case "activate":
			body = manualVectorBody[ManualActivateRequest](t, v.Request)
		case "inspect":
			body = manualVectorBody[ManualInspectRequest](t, v.Request)
		case "reserve":
			body = manualVectorBody[ManualReserveRequest](t, v.Request)
		case "commitStart":
			body = manualVectorBody[ManualStartRequest](t, v.Request)
		case "history":
			body = manualVectorBody[ManualHistoryRequest](t, v.Request)
		}
		got, err := manualRequestDigest(envelope.Method, body)
		if err != nil || got != v.RequestDigest {
			t.Fatalf("%s digest %s != %s", v.Name, got, v.RequestDigest)
		}
	}
	for _, v := range f.Invalid {
		if v.Line == "" {
			continue
		}
		valid := validManualRequestVector([]byte(v.Line))
		var err error
		if !valid {
			err = errManualProtocol
		}
		if err == nil {
			t.Fatalf("invalid request accepted: %s", v.Name)
		}
	}
}
func TestManualAdmissionClientExactSequenceAndBindings(t *testing.T) {
	f := manualFixture(t)
	client, done := manualScenario(t, f.Vectors[:5], nil)
	if _, err := client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), manualVectorBody[ManualInspectRequest](t, f.Vectors[1].Request)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Reserve(context.Background(), manualVectorBody[ManualReserveRequest](t, f.Vectors[2].Request)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CommitStart(context.Background(), manualVectorBody[ManualStartRequest](t, f.Vectors[3].Request)); err != nil {
		t.Fatal(err)
	}
	history, err := client.History(context.Background(), manualVectorBody[ManualHistoryRequest](t, f.Vectors[4].Request))
	if err != nil || history.Delivery != "observed" || history.RequestDigest == f.Vectors[4].RequestDigest {
		t.Fatal("history became permission", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestManualAdmissionClientPoisonsBadReplies(t *testing.T) {
	f := manualFixture(t)
	cases := map[string]func([]byte) []byte{
		"oversized frame": func(b []byte) []byte { return bytes.Repeat([]byte("x"), manualFrameLimit+1) },
		"incomplete JSON": func(b []byte) []byte { return b[:len(b)-1] },
		"duplicate": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
		},
		"case alias":      func(b []byte) []byte { return bytes.Replace(b, []byte(`"sequence"`), []byte(`"Sequence"`), 1) },
		"sequence replay": func(b []byte) []byte { return bytes.Replace(b, []byte(`"sequence":"1"`), []byte(`"sequence":"2"`), 1) },
		"unknown":         func(b []byte) []byte { return append([]byte(`{"extra":true,`), b[1:]...) },
		"null": func(b []byte) []byte {
			var v map[string]json.RawMessage
			_ = json.Unmarshal(b, &v)
			v["body"] = json.RawMessage(`null`)
			return encode(v)
		},
		"epoch overflow": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"epoch":"1"`), []byte(`"epoch":"18446744073709551616"`), 1)
		},
		"request digest": func(b []byte) []byte {
			return bytes.Replace(b, []byte(f.Vectors[0].RequestDigest), []byte("sha256:"+string(bytes.Repeat([]byte("0"), 64))), 1)
		},
		"activation recovered": func(b []byte) []byte { return bytes.Replace(b, []byte(`"committed"`), []byte(`"recovered"`), 1) },
		"trailing frame":       func(b []byte) []byte { return append(append(b, '\n'), b...) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			client, done := manualScenario(t, f.Vectors[:1], func(_ int, b []byte) []byte { return mutate(b) })
			if _, err := client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)); err == nil {
				t.Fatal("bad reply accepted")
			}
			if _, err := client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)); err == nil {
				t.Fatal("poisoned channel reused")
			}
			<-done
		})
	}
}
func TestManualAdmissionClientSemanticRefusals(t *testing.T) {
	f := manualFixture(t)
	t.Run("eligible reason contradiction", func(t *testing.T) {
		client, done := manualScenario(t, f.Vectors[:2], func(i int, b []byte) []byte {
			if i == 1 {
				return bytes.Replace(b, []byte(`"eligible":true`), []byte(`"eligible":false`), 1)
			}
			return b
		})
		_, err := client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.Inspect(context.Background(), manualVectorBody[ManualInspectRequest](t, f.Vectors[1].Request)); err == nil {
			t.Fatal("contradiction")
		}
		<-done
	})
	for _, mode := range []string{"recovered reserve", "changed commit binding", "changed original body digest", "start reason contradiction"} {
		t.Run(mode, func(t *testing.T) {
			n := 4
			if mode != "start reason contradiction" {
				n = 3
			}
			client, done := manualScenario(t, f.Vectors[:n], func(i int, b []byte) []byte {
				if i == 2 && mode == "recovered reserve" {
					return bytes.Replace(b, []byte(`"committed"`), []byte(`"recovered"`), 1)
				}
				if i == 3 {
					return bytes.Replace(b, []byte(`"reason":"start-committed"`), []byte(`"reason":"withdrawn"`), 1)
				}
				return b
			})
			_, err := client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Inspect(context.Background(), manualVectorBody[ManualInspectRequest](t, f.Vectors[1].Request))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Reserve(context.Background(), manualVectorBody[ManualReserveRequest](t, f.Vectors[2].Request))
			if err != nil {
				t.Fatal(err)
			}
			request := manualVectorBody[ManualStartRequest](t, f.Vectors[3].Request)
			if mode == "changed commit binding" {
				request.IntentID = "different"
			}
			if mode == "changed original body digest" {
				request.RawRequestDigest = "sha256:" + string(bytes.Repeat([]byte("0"), 64))
			}
			if _, err = client.CommitStart(context.Background(), request); err == nil {
				t.Fatal("unsafe continuation")
			}
			<-done
		})
	}
}
func TestManualAdmissionClientCancellationAndPhaseHold(t *testing.T) {
	f := manualFixture(t)
	for _, phase := range []string{"timeout", "already cancelled", "inspect before activate"} {
		t.Run(phase, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			client, err := NewManualAuthority(a, f.Synthetic.Boot)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if phase == "already cancelled" {
				cancel()
			}
			go func() { ioReader := bufio.NewReader(b); _, _ = ioReader.ReadBytes('\n') }()
			if phase == "inspect before activate" {
				_, err = client.Inspect(ctx, manualVectorBody[ManualInspectRequest](t, f.Vectors[1].Request))
			} else {
				_, err = client.Activate(ctx, manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request))
			}
			if err == nil {
				t.Fatal("unsafe phase continued")
			}
			if _, err = client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)); err == nil {
				t.Fatal("channel revived")
			}
		})
	}
}
func TestManualAdmissionClientSerializesConcurrentRequests(t *testing.T) {
	f := manualFixture(t)
	a, b := net.Pipe()
	defer b.Close()
	client, err := NewManualAuthority(a, f.Synthetic.Boot)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(b)
		for i := 1; i <= 17; i++ {
			line, e := reader.ReadBytes('\n')
			if e != nil {
				done <- e
				return
			}
			var r manualRequestEnvelope
			if manualDecode(line, &r) != nil || r.Sequence != strconv.Itoa(i) {
				done <- fmt.Errorf("sequence %d", i)
				return
			}
			var result any
			if i == 1 {
				var request ManualActivateRequest
				_ = json.Unmarshal(r.Body, &request)
				d, _ := manualRequestDigest(r.Method, request)
				result = ManualActivateResult{"committed", "1", d}
			} else {
				result = ManualInspectResult{"observed", ManualRegistration{"1", "sha256:" + string(bytes.Repeat([]byte("2"), 64))}, true, "eligible", "2026-10-19T12:00:00Z"}
			}
			reply := encode(manualReplyEnvelope{1, r.Sequence, r.Method, "ok", encode(result)})
			if _, e = b.Write(append(reply, '\n')); e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	if _, err = client.Activate(context.Background(), manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, e := client.Inspect(context.Background(), manualVectorBody[ManualInspectRequest](t, f.Vectors[1].Request))
			errs <- e
		}()
	}
	group.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func validManualRequestVector(raw []byte) bool {
	var r manualRequestEnvelope
	if manualDecode(raw, &r) != nil || r.Version != 1 || !manualPositive(r.Sequence) {
		return false
	}
	switch r.Method {
	case "activate":
		var body ManualActivateRequest
		return manualDecode(r.Body, &body) == nil && manualHex(body.Challenge) && manualSigning(body.Signing)
	case "inspect":
		var body ManualInspectRequest
		return manualDecode(r.Body, &body) == nil && manualResource(body.Resource)
	case "reserve":
		var body ManualReserveRequest
		return manualDecode(r.Body, &body) == nil && manualReserve(body)
	case "commitStart":
		var body ManualStartRequest
		return manualDecode(r.Body, &body) == nil && manualReserve(ManualReserveRequest{IntentID: body.IntentID, RawRequestDigest: body.RawRequestDigest, Resource: body.Resource, Registration: body.Registration, Payload: body.Payload}) && manualID(body.OperationID) && manualID(body.AttemptID) && manualPositive(body.Epoch)
	case "history":
		var body ManualHistoryRequest
		return manualDecode(r.Body, &body) == nil && manualID(body.IntentID)
	}
	return false
}

func TestManualAdmissionActivationRefusalCannotRetry(t *testing.T) {
	f := manualFixture(t)
	a, b := net.Pipe()
	client, err := NewManualAuthority(a, f.Synthetic.Boot)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(b)
		_, err := reader.ReadBytes('\n')
		if err != nil {
			done <- err
			return
		}
		_, err = b.Write(append(encode(manualReplyEnvelope{1, "1", "activate", "refused", encode(ManualAuthorityRefusal{"authority-unavailable"})}), '\n'))
		if err != nil {
			done <- err
			return
		}
		line, err := reader.ReadBytes('\n')
		if len(line) != 0 || err == nil {
			done <- fmt.Errorf("activation retried: %s", line)
			return
		}
		done <- nil
	}()
	request := manualVectorBody[ManualActivateRequest](t, f.Vectors[0].Request)
	if _, err = client.Activate(context.Background(), request); err == nil {
		t.Fatal("refusal accepted")
	}
	if _, err = client.Activate(context.Background(), request); err == nil {
		t.Fatal("activation retried")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
