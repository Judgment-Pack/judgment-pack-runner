package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trailLine is the record the Runtime wrote to a run's attempt audit trail:
// the trail's one line, without the newline that ends it.
func trailLine(t *testing.T, store, run string) []byte {
	t.Helper()
	trail, e := os.ReadFile(filepath.Join(store, "attempts", run, "audit", "evaluations.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	line, ended := bytes.CutSuffix(trail, []byte("\n"))
	if !ended || bytes.ContainsAny(line, "\r\n") {
		t.Fatalf("the trail is not one line: %q", trail)
	}
	return line
}

// get answers a GET as the runner's API does, with its owner's token.
func get(t *testing.T, h http.Handler, path string, want int) []byte {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != want {
		t.Fatal(path, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// The Runtime writes its audit record without escaping &, < and >, and keeps
// a number's spelling; Runner's encoding of the record it parses escapes them.
// A run keeps the record's line too, exactly: through the store, a restart and
// the run's API answer. Lists and briefs, which carry no audit body, do not
// carry the bytes either.
func TestARunKeepsItsAuditRecordsBytes(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	release, e := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: sample()})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Exact", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	input := sample()
	input.Facts = json.RawMessage(`{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false,"vendor":"A&B <x> café ☃","score":1.0}}`)
	run, _, e := s.submit(job.ID, "exact", input)
	if e != nil {
		t.Fatal(e)
	}
	done := waitRun(t, s, run.ID)
	if done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	line := trailLine(t, cfg.Dir, run.ID)
	// The fixture is what it says. The Runtime wrote the pack's identifier with
	// its & as it is. It copied the facts as Runner handed them, encoded with &,
	// < and > escaped, and kept 1.0 and the other characters as they are. The
	// parsed record Runner stores is the same JSON with other bytes.
	var stored []byte
	if e = s.db.QueryRow("SELECT record FROM runs WHERE id=?", run.ID).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	var record map[string]json.RawMessage
	if e = json.Unmarshal(stored, &record); e != nil {
		t.Fatal(e)
	}
	facts := `"vendor":` + string(encode("A&B <x> café ☃")) + `,"score":1.0`
	if !bytes.Contains(line, []byte(`?edition=A&B"`)) || !bytes.Contains(line, []byte(facts)) || bytes.Equal(record["audit"], line) || !sameJSON(record["audit"], line) {
		t.Fatalf("the fixture's record is not as described: %s", line)
	}
	exact := func(where string, got []byte) {
		t.Helper()
		if !bytes.Equal(got, line) || digest(got) != digest(line) {
			t.Fatalf("%s: the record's bytes changed: %q", where, got)
		}
	}
	exact("retained", done.AuditBytes)
	s.Close()
	if s, e = Open(cfg); e != nil {
		t.Fatal(e)
	}
	again, e := s.run(run.ID)
	if e != nil {
		t.Fatal(e)
	}
	exact("after a restart", again.AuditBytes)
	h := s.Handler("test")
	answer := get(t, h, "/v1/runs/"+run.ID, 200)
	var shown struct {
		AuditBytes []byte `json:"auditBytes"`
	}
	if e = json.Unmarshal(answer, &shown); e != nil {
		t.Fatal(e)
	}
	exact("in the API answer", shown.AuditBytes)
	if !bytes.Contains(answer, []byte(`"auditBytes":"`+base64.StdEncoding.EncodeToString(line)+`"`)) {
		t.Fatal("the API answer does not carry the bytes in standard base64")
	}
	list := get(t, h, "/v1/runs", 200)
	snapshot, e := s.briefSnapshot("run", run.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(list, []byte(run.ID)) || bytes.Contains(list, []byte(`"auditBytes"`)) || !bytes.Contains(snapshot, []byte(`"audit":{`)) || bytes.Contains(snapshot, []byte(`"auditBytes"`)) {
		t.Fatal("a list or brief carries the record's bytes")
	}
}

// packWithAmpersand is the triage pack with an identifier holding &, which the
// Runtime writes in its audit record as it is. A run's facts reach the Runtime
// as Runner encoded them when it stored the run, with &, < and > escaped, and
// are recorded so; the pack's identifier is not.
func packWithAmpersand(t *testing.T) string {
	t.Helper()
	pack, e := os.ReadFile("testdata/triage.pack.json")
	if e != nil {
		t.Fatal(e)
	}
	from := `"id": "https://example.invalid/judgment-packs/data-request-intake-triage",`
	if !bytes.Contains(pack, []byte(from)) {
		t.Fatal("the pack's identifier moved")
	}
	return strings.Replace(string(pack), from, `"id": "https://example.invalid/judgment-packs/data-request-intake-triage?edition=A&B",`, 1)
}

// caseRun is a mapping v2 input whose case is kase and maps its request and
// two evidence requirements.
func caseRun(kase string) Input {
	return Input{Source: &SourceInput{Mapping: InputMapping{Version: 2, UnmappedEvidence: []string{"sensitive-data-approvals"}, Case: &CaseMapping{
		Facts:    []FactMapping{{"/request", "/facts/request"}},
		Evidence: []EvidenceMapping{{"intake-form", "/evidence/intake-form"}, {"sponsor-endorsement", "/evidence/sponsor-endorsement"}},
	}}, Case: json.RawMessage(kase)}}
}

// A version-3 export carries the record's bytes as the Runtime wrote them,
// after a restart, and verification reports their digest. Version 2, served
// when nothing is asked, is the export Runner made before: the same run's
// export without them.
func TestVersion3ExportCarriesTheRecordsBytes(t *testing.T) {
	cfg := testConfig(t)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close() }()
	input := caseRun(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false,"vendor":"A&B <x> café ☃"}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`)
	release, e := s.preview(context.Background(), PreviewRequest{Pack: packWithAmpersand(t), Input: input})
	if e != nil {
		t.Fatal(e)
	}
	job, e := s.createJob("Exact", release.ID)
	if e != nil {
		t.Fatal(e)
	}
	run, _, e := s.submit(job.ID, "one", input)
	if e != nil {
		t.Fatal(e)
	}
	if done := waitRun(t, s, run.ID); done.State != "completed" {
		t.Fatal(done.State, done.Problem)
	}
	line := trailLine(t, cfg.Dir, run.ID)
	if !bytes.Contains(line, []byte(`?edition=A&B"`)) || !bytes.Contains(line, []byte(`"vendor":`+string(encode("A&B <x> café ☃")))) || bytes.Equal(encode(json.RawMessage(line)), line) {
		t.Fatalf("the fixture's record is not as described: %s", line)
	}
	s.Close()
	if s, e = Open(cfg); e != nil {
		t.Fatal(e)
	}
	h := s.Handler("test")
	raw := get(t, h, "/v1/runs/"+run.ID+"/verification?version=3", 200)
	var v3 VerificationBundle
	if e = strictJSON(raw, &v3); e != nil {
		t.Fatal(e)
	}
	if v3.Version != 3 || !bytes.Equal(v3.Run.AuditBytes, line) || digest(v3.Run.AuditBytes) != digest(line) {
		t.Fatalf("version 3 does not carry the record's bytes: %d %q", v3.Version, v3.Run.AuditBytes)
	}
	if bytes.Contains(raw, line) {
		t.Fatal("the export's parsed record has the line's own bytes: the fixture shows nothing")
	}
	verified, e := VerifyInputs(raw, nil, v3.ReleaseDigest)
	if e != nil || verified.ExportVersion() != 3 || verified.RecordDigest() != digest(line) {
		t.Fatal(e, verified.ExportVersion(), verified.RecordDigest())
	}
	// Version 2 is version 3 without the bytes, and says nothing of them.
	v2 := v3
	v2.Version, v2.Run.AuditBytes = 2, nil
	var want bytes.Buffer
	if e = json.NewEncoder(&want).Encode(v2); e != nil {
		t.Fatal(e)
	}
	for _, query := range []string{"", "?version=2"} {
		if got := get(t, h, "/v1/runs/"+run.ID+"/verification"+query, 200); !bytes.Equal(got, want.Bytes()) || bytes.Contains(got, []byte("auditBytes")) {
			t.Fatalf("version 2%s is not the export without the bytes: %s", query, got)
		}
	}
	if verified, e = VerifyInputs(want.Bytes(), nil, v3.ReleaseDigest); e != nil || verified.ExportVersion() != 2 || verified.RecordDigest() != "" {
		t.Fatal(e, verified.ExportVersion(), verified.RecordDigest())
	}
}

// fixtureStore holds a release and a run that an earlier Runner exported, as
// it stored them, in a store whose Runtime is never run.
func fixtureStore(t *testing.T, export []byte) (Config, VerificationBundle) {
	t.Helper()
	var b VerificationBundle
	if e := strictJSON(export, &b); e != nil {
		t.Fatal(e)
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	placeholder := filepath.Join(t.TempDir(), "never-run")
	if e = os.WriteFile(placeholder, []byte("not a Runtime: no test runs it\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := Config{Dir: dir, Runtime: placeholder, Workspace: "fixture", Owner: "owner", disableAutomation: true}
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.db.Exec("INSERT INTO releases(id,record) VALUES (?,?)", b.Release.ID, string(encode(b.Release))); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("INSERT INTO runs(id,job_id,caller,idem,request_digest,state,record) VALUES (?,?,?,?,?,?,?)", b.Run.ID, b.Run.JobID, b.Run.RequestedBy, "fixture", "", b.Run.State, string(encode(b.Run))); e != nil {
		t.Fatal(e)
	}
	return cfg, b
}

// A run recorded before Runner kept the record's bytes has none to give. Asked
// for version 3, its export is version 2, byte for byte the export an earlier
// Runner made of it: one before calculators, and one at the commit before this
// change, whose record holds an & that the export escapes.
func TestARunRecordedBeforeExactBytesExportsVersion2Unchanged(t *testing.T) {
	for _, c := range []struct{ dir, served string }{
		{"testdata/mapping-v2-before-calculators/", "\n"},
		{"testdata/mapping-v2-before-exact-bytes/", ""},
	} {
		t.Run(c.dir, func(t *testing.T) {
			export, e := os.ReadFile(c.dir + "run.json")
			if e != nil {
				t.Fatal(e)
			}
			// The export before calculators was encoded with json.Marshal, which adds
			// no newline, and the API answer ends with one. The other is the answer.
			export = append(export, c.served...)
			cfg, b := fixtureStore(t, export)
			s, e := Open(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			h := s.Handler("test")
			for _, query := range []string{"", "?version=2", "?version=3"} {
				if got := get(t, h, "/v1/runs/"+b.Run.ID+"/verification"+query, 200); !bytes.Equal(got, export) {
					t.Fatalf("the export asked for with %q differs from the earlier Runner's", query)
				}
			}
			trusted, e := os.ReadFile(c.dir + "release-digest.txt")
			if e != nil {
				t.Fatal(e)
			}
			rawProfiles, e := os.ReadFile(c.dir + "profiles.json")
			if e != nil {
				t.Fatal(e)
			}
			profiles, e := ParseInputProfiles(rawProfiles)
			if e != nil {
				t.Fatal(e)
			}
			v, e := VerifyInputs(export, profiles, strings.TrimSpace(string(trusted)))
			if e != nil || v.ExportVersion() != 2 || v.RecordDigest() != "" {
				t.Fatal(e, v.ExportVersion(), v.RecordDigest())
			}
		})
	}
	// The record that the export made at that commit holds re-encoded: the
	// Runtime's line is not in it, though it parses to the same record.
	dir := "testdata/mapping-v2-before-exact-bytes/"
	export, e := os.ReadFile(dir + "run.json")
	if e != nil {
		t.Fatal(e)
	}
	trail, e := os.ReadFile(dir + "audit-record.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	line := bytes.TrimSuffix(trail, []byte("\n"))
	var b VerificationBundle
	if e = json.Unmarshal(export, &b); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(line, []byte("?edition=A&B")) || bytes.Contains(export, line) || !sameJSON(b.Run.Audit, line) || len(b.Run.AuditBytes) != 0 {
		t.Fatal("the fixture is not an export of that record re-encoded")
	}
}

// Version 3 carries the bytes as base64, which passes through any encoder
// unchanged: here a number spelled 1.0, a line separator that Go's encoder
// escapes in a string, and a byte that is not UTF-8, which no JSON string can
// carry. The bytes come back exactly, after a restart, from the export and the
// run's API answer. (No mapping v2 run's facts hold 1.0: the mapping refuses
// fractional numbers. The Runtime's own spelling of it is kept by
// TestARunKeepsItsAuditRecordsBytes.)
func TestVersion3ExportPassesAnyBytesThrough(t *testing.T) {
	export, e := os.ReadFile("testdata/mapping-v2-before-exact-bytes/run.json")
	if e != nil {
		t.Fatal(e)
	}
	var b VerificationBundle
	if e = json.Unmarshal(export, &b); e != nil {
		t.Fatal(e)
	}
	held := []byte("{\"note\":\"A&B <x> café \u2028 \xff\",\"amount\":1.0}")
	b.Run.AuditBytes = held
	cfg, _ := fixtureStore(t, encode(b))
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	h := s.Handler("test")
	var v3 VerificationBundle
	if e = json.Unmarshal(get(t, h, "/v1/runs/"+b.Run.ID+"/verification?version=3", 200), &v3); e != nil {
		t.Fatal(e)
	}
	var run Run
	if e = json.Unmarshal(get(t, h, "/v1/runs/"+b.Run.ID, 200), &run); e != nil {
		t.Fatal(e)
	}
	if v3.Version != 3 || !bytes.Equal(v3.Run.AuditBytes, held) || !bytes.Equal(run.AuditBytes, held) {
		t.Fatalf("the bytes changed: %q %q", v3.Run.AuditBytes, run.AuditBytes)
	}
}

// v3Export is a completed run's version-3 export, with the bytes the Runtime
// wrote to its attempt's trail, and what verification needs of it.
func v3Export(t *testing.T) (VerificationBundle, []InputProfile, []byte) {
	t.Helper()
	b, profiles, store := completedExport(t, "asserted")
	line := trailLine(t, store, b.Run.ID)
	b.Version, b.Run.AuditBytes = 3, line
	return b, profiles, line
}

// Verification holds a version-3 export's bytes to its record: a record and
// bytes that parse to other values, bytes that are not one line of a trail or
// parse only by taking one of two members of a name, and bytes where the
// version does not carry them, are refused. The surface member that is changed
// is read by no other check.
func TestVerifyHoldsTheRecordToItsBytes(t *testing.T) {
	bundle, profiles, line := v3Export(t)
	v, e := VerifyInputs(encode(bundle), profiles, bundle.ReleaseDigest)
	if e != nil || v.ExportVersion() != 3 || v.RecordDigest() != digest(line) {
		t.Fatal(e, v.ExportVersion(), v.RecordDigest())
	}
	surface, other := []byte(`"surface":"experimental evaluate"`), []byte(`"surface":"experimental evaluate again"`)
	if !bytes.Contains(line, surface) || !bytes.Contains(bundle.Run.Audit, surface) {
		t.Fatal("the fixture has no surface to change")
	}
	differs := "the audit record differs from its original bytes"
	noLine := "a version-3 export carries the original bytes of a completed run's audit record, as one line"
	for name, c := range map[string]struct {
		change  func(*VerificationBundle)
		refusal string
	}{
		"bytes changed":  {func(b *VerificationBundle) { b.Run.AuditBytes = bytes.Replace(line, surface, other, 1) }, differs},
		"record changed": {func(b *VerificationBundle) { b.Run.Audit = bytes.Replace(b.Run.Audit, surface, other, 1) }, differs},
		"not JSON":       {func(b *VerificationBundle) { b.Run.AuditBytes = line[:len(line)-1] }, differs},
		"a member twice": {func(b *VerificationBundle) {
			b.Run.AuditBytes = append(bytes.Clone(line[:len(line)-1]), ","+string(surface)+"}"...)
		}, differs},
		"ended by a newline": {func(b *VerificationBundle) { b.Run.AuditBytes = append(bytes.Clone(line), '\n') }, noLine},
		"ended by a return":  {func(b *VerificationBundle) { b.Run.AuditBytes = append(bytes.Clone(line), '\r') }, noLine},
		"none in version 3":  {func(b *VerificationBundle) { b.Run.AuditBytes = nil }, noLine},
		"a failed run":       {func(b *VerificationBundle) { b.Run.State = "failed" }, noLine},
		"in version 2":       {func(b *VerificationBundle) { b.Version = 2 }, "a version-2 export carries no original bytes of the audit record"},
		"in version 4":       {func(b *VerificationBundle) { b.Version = 4 }, "release does not match the independently trusted digest"},
	} {
		t.Run(name, func(t *testing.T) {
			b := bundle
			c.change(&b)
			if _, e := VerifyInputs(encode(b), profiles, b.ReleaseDigest); e == nil || e.Error() != c.refusal {
				t.Fatal(e)
			}
		})
	}
	// Bytes that differ only in insignificant whitespace parse to the record, and
	// their digest, which no receipt names, is what is reported.
	b := bundle
	b.Run.AuditBytes = append([]byte(" "), line...)
	if v, e = VerifyInputs(encode(b), profiles, b.ReleaseDigest); e != nil || v.RecordDigest() != digest(b.Run.AuditBytes) || v.RecordDigest() == digest(line) {
		t.Fatal(e, v.RecordDigest())
	}
}

// A run keeps the record's bytes only when its attempt's trail is one line
// ended by a newline, as the Runtime writes it. Of another trail it keeps none,
// and the run's export is version 2.
func TestRecordLineIsTheTrailsOneLine(t *testing.T) {
	for trail, want := range map[string]string{
		"{}\n":     "{}",
		"{}":       "",
		"{}\r\n":   "",
		"{}\n{}\n": "",
		"{}\n\n":   "",
		"\n":       "",
		" {} \n":   " {} ",
	} {
		if got := recordLine([]byte(trail)); string(got) != want || (want == "") != (got == nil) {
			t.Errorf("%q: %q", trail, got)
		}
	}
}

// beforeExactBytes is the export made at the commit before version 3, the
// record's line the Runtime wrote, its trusted release digest and profiles.
func beforeExactBytes(t *testing.T) (export, line []byte, release string, profiles []InputProfile) {
	t.Helper()
	dir := "testdata/mapping-v2-before-exact-bytes/"
	export, e := os.ReadFile(dir + "run.json")
	if e != nil {
		t.Fatal(e)
	}
	trail, e := os.ReadFile(dir + "audit-record.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	trusted, e := os.ReadFile(dir + "release-digest.txt")
	if e != nil {
		t.Fatal(e)
	}
	rawProfiles, e := os.ReadFile(dir + "profiles.json")
	if e != nil {
		t.Fatal(e)
	}
	if profiles, e = ParseInputProfiles(rawProfiles); e != nil {
		t.Fatal(e)
	}
	return export, bytes.TrimSuffix(trail, []byte("\n")), strings.TrimSpace(string(trusted)), profiles
}

// The version asked for is read from a query parsed strictly: a malformed
// query, whose entries a lenient reader would drop, is refused, as is a version
// asked for twice or one that is not 2 or 3.
func TestAnExportVersionIsAskedForOnceInAWellFormedQuery(t *testing.T) {
	export, _, _, _ := beforeExactBytes(t)
	cfg, b := fixtureStore(t, export)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	h := s.Handler("test")
	path := "/v1/runs/" + b.Run.ID + "/verification"
	for _, query := range []string{"", "?version=2", "?version=3", "?other=1"} {
		get(t, h, path+query, 200)
	}
	for _, query := range []string{"?version=4", "?version=", "?version=3&version=3", "?version=03", "?version=%ZZ", "?version=2&version=%ZZ", "?version=3;bad", "?other=%ZZ"} {
		if got := get(t, h, path+query, 400); !bytes.Contains(got, []byte(`"invalid_version"`)) {
			t.Fatal(query, string(got))
		}
	}
}

// A version-3 export is held to the 8 MiB version 2 is held to, beside the
// member that carries the record's bytes, which are held to the 8 MiB of an
// audit trail Runner reads. So an export that version 2 accepts at its limit is
// accepted as version 3, though it is larger. Padding with whitespace, which a
// reader ignores, sets the sizes.
func TestVersion3IsHeldToVersion2sLimitBesideItsBytes(t *testing.T) {
	export, line, release, profiles := beforeExactBytes(t)
	var v2 VerificationBundle
	if e := json.Unmarshal(export, &v2); e != nil {
		t.Fatal(e)
	}
	v3 := v2
	v3.Version, v3.Run.AuditBytes = 3, line
	member := len(encode(v3)) - len(encode(v2))
	if member != len(`,"auditBytes":""`)+base64.StdEncoding.EncodedLen(len(line)) {
		t.Fatal("the member's size is not as counted:", member)
	}
	pad := func(raw []byte, to int) []byte {
		return append(bytes.Clone(raw), bytes.Repeat([]byte(" "), to-len(raw))...)
	}
	verify := func(raw []byte) error {
		_, e := VerifyInputs(raw, profiles, release)
		return e
	}
	for name, c := range map[string]struct {
		raw     []byte
		refusal string
	}{
		"version 2 at 8 MiB":                     {pad(encode(v2), 8<<20), ""},
		"version 2 past 8 MiB":                   {pad(encode(v2), 8<<20+1), "verification bundle exceeds 8 MiB"},
		"version 3 at 8 MiB beside its bytes":    {pad(encode(v3), 8<<20+member), ""},
		"version 3 past 8 MiB beside its bytes":  {pad(encode(v3), 8<<20+member+1), "verification bundle exceeds 8 MiB"},
		"anything past the most an export holds": {bytes.Repeat([]byte("x"), MaxExportSize+1), "verification bundle exceeds 8 MiB"},
	} {
		t.Run(name, func(t *testing.T) {
			if e := verify(c.raw); c.refusal == "" && e != nil || c.refusal != "" && (e == nil || e.Error() != c.refusal) {
				t.Fatal(len(c.raw), e)
			}
		})
	}
	// The record's bytes may take up the 8 MiB of a trail, and no more. Spaces
	// after the record's first brace keep them one line and the same record.
	// With 8 MiB of them beside 8 MiB of the rest, an export is as large as one
	// can be, counted here apart from MaxExportSize.
	most := 8<<20 + len(`,"auditBytes":""`) + base64.StdEncoding.EncodedLen(maxOutput)
	for _, c := range []struct {
		size, to int
		refusal  string
	}{
		{maxOutput, 0, ""},
		{maxOutput + 1, 0, "the audit record's bytes exceed 8 MiB"},
		{maxOutput, most, ""},
		{maxOutput, most + 1, "verification bundle exceeds 8 MiB"},
	} {
		b := v3
		b.Run.AuditBytes = append(append([]byte("{"), bytes.Repeat([]byte(" "), c.size-len(line))...), line[1:]...)
		raw := encode(b)
		if c.to > 0 {
			raw = pad(raw, c.to)
		}
		if e := verify(raw); c.refusal == "" && e != nil || c.refusal != "" && (e == nil || e.Error() != c.refusal) {
			t.Fatal(c.size, len(raw), e)
		}
	}
}
