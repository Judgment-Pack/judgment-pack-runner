package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Spec's format 4.1.0 vectors (RFC 0033, in Review), copied byte for
// byte at its commit 7510ea12d7079b60073aac498ca9c21ec1362cd4
// (vectors/dialect-format-4.1).
const format41Vectors = "../../vectors/dialect-format-4.1/spec"

// The SHA-256 of each top-level file at that commit, taken from the Spec's
// bytes (git show), not from anything this repository computes.
var format41Pins = map[string]string{
	"README.md":     "b3926effd1618a6edbf63b034825b5fa3eb44bb34e52e0fdf2890a776e03664e",
	"manifest.json": "645f4d4130be568a53d491c6198307dae297b1a4d95fd293c48140a0d9410ca3",
}

func format41File(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(format41Vectors, filepath.FromSlash(name)))
	must(t, err)
	return b
}

type format41Outcome struct {
	Status string          `json:"status"`
	Reason string          `json:"reason"`
	Paging json.RawMessage `json:"paging"`
}

type format41Manifest struct {
	Cases []struct {
		Name     string            `json:"name"`
		Row      string            `json:"row"`
		Files    map[string]string `json:"files"`
		Expected struct {
			Result             string                     `json:"result"`
			Refusal            string                     `json:"refusal"`
			Finding            string                     `json:"finding"`
			Facts              json.RawMessage            `json:"facts"`
			Outcomes           map[string]format41Outcome `json:"outcomes"`
			Cites              [][2]any                   `json:"cites"`
			UnsignedParameters json.RawMessage            `json:"unsignedParameters"`
		} `json:"expected"`
		PreparationDigest string `json:"preparationDigest"`
	} `json:"cases"`
}

func format41Read(t *testing.T) format41Manifest {
	t.Helper()
	var manifest format41Manifest
	must(t, json.Unmarshal(format41File(t, "manifest.json"), &manifest))
	return manifest
}

func TestFormat41SpecFilesArePinned(t *testing.T) {
	for name, hash := range format41Pins {
		if got := digest(format41File(t, name)); got != "sha256:"+hash {
			t.Fatalf("%s is not the Spec's bytes at the pinned commit: %s", name, got)
		}
	}
	manifest := format41Read(t)
	files := 0
	for _, c := range manifest.Cases {
		if !strings.HasPrefix(c.Name, "paged-") || c.Name != "paged-"+c.Row {
			t.Fatal("a vector is not named by its row", c.Name, c.Row)
		}
		for name, hash := range c.Files {
			if got := digest(format41File(t, c.Name+"/"+name)); got != "sha256:"+hash {
				t.Fatalf("%s/%s differs from the manifest: %s", c.Name, name, got)
			}
			files++
		}
	}
	entries, err := os.ReadDir(format41Vectors)
	must(t, err)
	if len(manifest.Cases) != 88 || files != 441 || len(entries) != 90 {
		t.Fatalf("expected the 88 vectors, their 441 files and nothing else, read %d, %d and %d entries", len(manifest.Cases), files, len(entries))
	}
}

// format41Unread is the vector Runner does not read, and its answer: its
// research source writes the typed results (spec/dialect.md's results
// contract), which Runner's research reading does not read, so the dialect is
// refused before its paged session is reached. The rule it checks, that no
// other source's receipt names a paged source's session, is the one
// session-shared checks for an unpaged source (admit, prepareV2Mode).
var format41Unread = map[string]string{"research-in-paged-session": "refusal input-malformed"}

// Every vector through verify-preparation's reader (VerifyPreparation): the
// result and its code, and of a reading the digest, the facts, each named
// source's outcome and paging, the cites in their order and, where given,
// the unsigned parameters, each the manifest's, written by hand. A reading's
// recomputed preparation is the Spec's preparation.json, or the reading
// would be preparation-differs.
func TestFormat41Vectors(t *testing.T) {
	agree := 0
	for _, c := range format41Read(t).Cases {
		t.Run(c.Row, func(t *testing.T) {
			read := func(name string) []byte { return format41File(t, c.Name+"/"+name) }
			f := PreparationFixture{Dialect: read("dialect.json"), Case: read("case.json"), Sources: read("sources.json"), Profiles: read("profiles.json"), Preparation: read("preparation.json")}
			if _, ok := c.Files["binding.json"]; ok {
				f.Binding = read("binding.json")
			}
			var recomputed *Preparation
			report := verifyPreparationUsing(f, verifyAcquisition, &recomputed)
			want := c.Expected
			code := want.Refusal + want.Finding
			if answer, ok := format41Unread[c.Row]; ok {
				if got := report.Result + " " + report.Code; got != answer || !strings.Contains(report.Reason, `unknown field "results"`) {
					t.Fatalf("%s (%s), want %s", got, report.Reason, answer)
				}
				return
			}
			if report.Result != want.Result || report.Code != code {
				t.Fatalf("%s %s (%s), want %s %s", report.Result, report.Code, report.Reason, want.Result, code)
			}
			if want.Result != ResultReading {
				agree++
				return
			}
			if report.Digest != c.PreparationDigest || digest(read("preparation.json")) != c.PreparationDigest {
				t.Fatal("preparation digest differs from the manifest", report.Digest)
			}
			if got, w := canonTest(t, report.Facts), canonTest(t, want.Facts); !bytes.Equal(got, w) {
				t.Fatalf("facts\n got %s\nwant %s", got, w)
			}
			for name, o := range want.Outcomes {
				found := false
				for _, got := range recomputed.Outcomes {
					if got.Name != name {
						continue
					}
					found = true
					if got.Status != o.Status || got.Reason != o.Reason {
						t.Fatalf("outcome %s is %s %s, want %s %s", name, got.Status, got.Reason, o.Status, o.Reason)
					}
					if (len(o.Paging) == 0) != (len(got.paging) == 0) || len(o.Paging) > 0 && !bytes.Equal(canonTest(t, got.paging), canonTest(t, o.Paging)) {
						t.Fatalf("outcome %s's paging is %s, want %s", name, got.paging, o.Paging)
					}
				}
				if !found {
					t.Fatal("no outcome", name)
				}
			}
			if len(recomputed.Cites) != len(want.Cites) {
				t.Fatalf("%d cites, want %d", len(recomputed.Cites), len(want.Cites))
			}
			for i, w := range want.Cites {
				if got := recomputed.Cites[i]; got.SessionID != w[0] || float64(got.CallIndex) != w[1] {
					t.Fatalf("cite %d is %s/%d, want %v", i, got.SessionID, got.CallIndex, w)
				}
			}
			if len(want.UnsignedParameters) > 0 {
				got := encode(report.UnsignedParameters)
				if len(report.UnsignedParameters) == 0 {
					got = []byte(`[]`)
				}
				if !bytes.Equal(canonTest(t, got), canonTest(t, want.UnsignedParameters)) {
					t.Fatalf("unsignedParameters\n got %s\nwant %s", got, want.UnsignedParameters)
				}
			}
			agree++
		})
	}
	if agree != 88-len(format41Unread) {
		t.Fatalf("%d of the 88 vectors agree", agree)
	}
}

// A dialect of format 4.1.0 is refused on every path but verify-preparation,
// as one of format 4.0.0 is (dialect_format4.go), and a paged source's
// acquisition reaches a run by no request: pages is no member an input
// decodes.
func TestFormat41IsNotAdmittedOutsideTheVerifier(t *testing.T) {
	var m InputMapping
	must(t, strictJSON(format41File(t, "paged-three-pages-no-next/dialect.json"), &m))
	if m.FormatVersion != format41 || len(m.Sources) != 1 || len(m.Sources[0].Paging) == 0 {
		t.Fatal("the paged source was not read", m.FormatVersion)
	}
	notAdmitted(t, validateMappingV2(m), codeFormatNotAdmitted)
	_, err := releaseDialectIdentity(Release{InputMapping: &m})
	notAdmitted(t, err, codeFormatNotAdmitted)
	at, _ := time.Parse("2006-01-02T15:04:05Z", "2026-11-15T12:00:00Z")
	_, err = normalizeV2(Input{Source: &SourceInput{Mapping: m, Case: format41File(t, "paged-three-pages-no-next/case.json")}}, nil, at)
	notAdmitted(t, err, codeFormatNotAdmitted)
	if !strings.Contains(err.Error(), "4.1.0") {
		t.Fatal("the refusal does not name the format", err)
	}
	var input SourceInput
	if strictJSON([]byte(`{"mapping":{},"sources":{"changes":{"pages":[]}}}`), &input) == nil {
		t.Fatal("a request decoded a paged acquisition")
	}
}

// paging is format 4.1.0's alone: a dialect of an earlier format that
// carries it is refused on every path, the verifier's too.
func TestPagingIsRefusedInEarlierFormats(t *testing.T) {
	var m InputMapping
	must(t, strictJSON(format41File(t, "paged-three-pages-no-next/dialect.json"), &m))
	for _, format := range []string{format4, "3.0.0"} {
		m.FormatVersion = format
		if _, err := pagingOf(m.FormatVersion, m.Sources[0]); err == nil || !strings.Contains(err.Error(), "format 4.1.0 alone") {
			t.Fatal(format, "admitted paging", err)
		}
	}
}
