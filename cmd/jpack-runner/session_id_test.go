package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// inSession is the input with its one receipt's sessionId replaced by
// session and the receipt signed again, as a gateway that wrote that
// session would have signed it.
func inSession(t *testing.T, s signer, input runner.Input, session string) runner.Input {
	t.Helper()
	var response struct {
		Receipt map[string]any  `json:"receipt"`
		Result  json.RawMessage `json:"result"`
		Salts   json.RawMessage `json:"salts"`
	}
	if err := json.Unmarshal(input.Source.Sources["fx"].Response, &response); err != nil {
		t.Fatal(err)
	}
	response.Receipt["sessionId"] = session
	delete(response.Receipt, "signature")
	response.Receipt["signature"] = hex.EncodeToString(ed25519.Sign(s.key, append([]byte("judgment-pack-gateway/receipt/3:"), canonical(t, response.Receipt)...)))
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]runner.SourceValue{"fx": {Response: raw}}
	input.Source.Sources = sources
	return input
}

// sessionsIn is every sessionId member of a JSON document, in order.
func sessionsIn(t *testing.T, raw []byte) []string {
	t.Helper()
	var found []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, m := range v {
				if s, ok := m.(string); ok && k == "sessionId" {
					found = append(found, s)
				}
				walk(m)
			}
		case []any:
			for _, m := range v {
				walk(m)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc)
	return found
}

// A receipt whose session is not of the gateway's token form, and one whose
// session is the empty string, is admitted at preparation (runner #99), and
// the Runtime, which reads a citation's sessionId as any string (runtime
// #291), evaluates the run: the record is produced, the run is exported, and
// verify-run reads it, in-process and by re-execution. The string is never
// trimmed, replaced or omitted at any step: not in the preparation's
// citation, not in the Runtime's record, not in the export, not in the
// report verify-run recomputes from it.
func TestARunWhoseReceiptSessionIsNoTokenIsEvaluatedExportedAndVerified(t *testing.T) {
	bindingRuntime(t)
	for name, session := range map[string]string{"a string of every kind of character": "a b:c/d\"e\\fé", "the empty string": ""} {
		t.Run(name, func(t *testing.T) {
			input, s := calculatorOnACopy(t)
			input = inSession(t, s, input, session)
			profiles := []runner.InputProfile{s.profile}
			e := exportedRunKeyed(t, input, profiles, "", "?version=5", "", "")

			var run struct {
				State string `json:"state"`
				Input struct {
					Preparation struct {
						Cites []runner.Citation `json:"cites"`
					} `json:"preparation"`
				} `json:"input"`
			}
			if err := json.Unmarshal(e.bundle["run"], &run); err != nil {
				t.Fatal(err)
			}
			if run.State != "completed" || len(run.Input.Preparation.Cites) != 1 || run.Input.Preparation.Cites[0].SessionID != session {
				t.Fatalf("the preparation's citation: %s %+v", run.State, run.Input.Preparation.Cites)
			}
			record := recordOf(t, e.bundle)
			if !bytes.Equal(record, e.line) || !bytes.Contains(record, []byte(`"kind":"evaluation"`)) {
				t.Fatalf("no evaluation record: %s", record)
			}
			if got := sessionsIn(t, record); len(got) != 1 || got[0] != session {
				t.Fatalf("the Runtime's record carries %q, not %q: %s", got, session, record)
			}
			if got := sessionsIn(t, e.bundle["run"]); len(got) < 2 {
				t.Fatalf("the export carries %q: %s", got, e.bundle["run"])
			} else {
				for _, g := range got {
					if g != session {
						t.Fatalf("the export carries %q, not %q", g, session)
					}
				}
			}
			t.Setenv("TMPDIR", t.TempDir())
			out, human, err := verifyWith(t, e.bundle, e.digest, profiles)
			if err != nil || !strings.Contains(out, `"status":"verified-inputs"`) {
				t.Fatal(err, out, human)
			}
			out, human, err = verifyWith(t, e.bundle, e.digest, profiles, "--runtime="+os.Getenv("JPACK_TEST_BIN"))
			if err != nil || !strings.Contains(out, `"status":"verified-disposition"`) || !strings.Contains(out, `"binding":{"status":"matches"}`) {
				t.Fatal(err, out, human)
			}
		})
	}
}
