package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The exports of runs whose jobs have no lineage, written by Runner with the
// Runtime CI pins (v0.27.0) and kept as they were served: a run of a job whose
// input is typed in, whose pack's identifier holds an & that the Runtime writes
// as it is and Runner's encoding escapes, at sequence 2 of its installation's
// chain, unsigned; and a run of a job with a v1 file mapping, at sequence 1,
// which the Runtime signed with the guide's first seed.
const (
	noMappingRun     = "testdata/no-mapping-run/"
	v1FileMappingRun = "testdata/v1-file-mapping-run/"
)

// notMappedVector is one of those exports and what checking it needs: the
// record's line the Runtime wrote, the installation's chain as GET
// /v1/run-chain served it, the run's entry's checkpoint as a holder would
// keep it, and the trusted release digest.
type notMappedVector struct {
	export, line, chain []byte
	held                []Checkpoint
	release             string
	bundle              VerificationBundle
}

func readNotMappedVector(t *testing.T, dir string) notMappedVector {
	t.Helper()
	read := func(name string) []byte {
		t.Helper()
		b, e := os.ReadFile(dir + name)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	v := notMappedVector{export: read("run.json"), line: bytes.TrimSuffix(read("audit-record.jsonl"), []byte("\n")), chain: read("chain.jsonl"), release: strings.TrimSpace(string(read("release-digest.txt")))}
	var e error
	if v.held, e = ParseCheckpoints(read("checkpoints.jsonl")); e != nil {
		t.Fatal(e)
	}
	if e = strictJSON(v.export, &v.bundle); e != nil {
		t.Fatal(e)
	}
	return v
}

// notMappedStore holds a vector's release and run, and the installation's
// chain, as Runner stored them, in a store whose Runtime is never run.
func notMappedStore(t *testing.T, v notMappedVector, export []byte) *Service {
	t.Helper()
	cfg, _ := fixtureStore(t, export)
	s, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	for _, line := range bytes.Split(bytes.TrimSuffix(v.chain, []byte("\n")), []byte("\n")) {
		entry, e := ParseChainEntry(line)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.db.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)", entry.Sequence, entry.Run, line); e != nil {
			t.Fatal(e)
		}
	}
	return s
}

// The vectors are what they say: runs with no preparation, completed, whose
// record's bytes are the line the Runtime wrote, chained, and one signed.
func TestTheExportsWithoutLineageAreWhatTheySay(t *testing.T) {
	for dir, want := range map[string]struct {
		version  int
		sequence int64
		mapping  bool
	}{noMappingRun: {4, 2, false}, v1FileMappingRun: {5, 1, true}} {
		v := readNotMappedVector(t, dir)
		b := v.bundle
		entry, e := ParseChainEntry(b.Chain.Entry)
		if e != nil || b.Version != want.version || b.Inputs != InputsNotMapped || b.Run.State != "completed" || b.Run.Input.Preparation != nil || !bytes.Equal(b.Run.AuditBytes, v.line) || entry.Sequence != want.sequence || entry.Run != b.Run.ID || (b.Release.InputMapping != nil) != want.mapping || (b.Run.Input.Source != nil) != want.mapping || want.mapping && b.Release.InputMapping.Version != 1 {
			t.Fatalf("%s is not as described: %v", dir, e)
		}
		for _, member := range []string{`"preparation"`, `"lineage"`, `"cites"`} {
			if bytes.Contains(v.export, []byte(member)) {
				t.Fatalf("%s carries %s", dir, member)
			}
		}
	}
	// The record's bytes are not Runner's encoding of the record it parsed.
	v := readNotMappedVector(t, noMappingRun)
	if !bytes.Contains(v.line, []byte(`?edition=A&B"`)) || bytes.Contains(v.export, v.line) || !sameJSON(v.bundle.Run.Audit, v.line) {
		t.Fatal("the record's line is Runner's own encoding of the record")
	}
}

// A run of a job with no input mapping, or a v1 file mapping, is exported at
// the highest version its material allows, up to the one asked, with the
// inputs member saying that it carries no lineage: byte for byte as Runner
// served it. Version 2, which would carry none of the record's bytes, is
// refused for it.
func TestARunWithoutLineageIsExportedWithoutIt(t *testing.T) {
	for _, dir := range []string{noMappingRun, v1FileMappingRun} {
		t.Run(dir, func(t *testing.T) {
			v := readNotMappedVector(t, dir)
			s := notMappedStore(t, v, v.export)
			h := s.Handler("test")
			path := "/v1/runs/" + v.bundle.Run.ID + "/verification"
			if got := get(t, h, path+"?version=5", 200); !bytes.Equal(got, v.export) {
				t.Fatalf("the export differs from the one Runner served:\n%s", got)
			}
			served := func(b VerificationBundle) []byte {
				var out bytes.Buffer
				if e := json.NewEncoder(&out).Encode(b); e != nil {
					t.Fatal(e)
				}
				return out.Bytes()
			}
			v4 := v.bundle
			v4.Version, v4.Run.AuditSignatures = 4, nil
			v3 := v4
			v3.Version, v3.Chain = 3, nil
			for query, want := range map[string][]byte{"?version=4": served(v4), "?version=3": served(v3)} {
				if got := get(t, h, path+query, 200); !bytes.Equal(got, want) {
					t.Fatalf("%s: %s", query, got)
				}
			}
			for _, query := range []string{"", "?version=2"} {
				refused(t, get(t, h, path+query, 422))
			}
		})
	}
}

// refused holds an answer to the refusal of an export without lineage that
// would carry no record's bytes.
func refused(t *testing.T, answer []byte) {
	t.Helper()
	want := `{"error":{"code":"not_verified_mapping","message":"This run does not use mapping v2, and an export without lineage carries the audit record's bytes: ask for version 3, 4 or 5 of a completed run that holds them.","retryable":false}}` + "\n"
	if string(answer) != want {
		t.Fatalf("not the refusal: %s", answer)
	}
}

// A run without lineage that holds no record's bytes, because it failed, or
// completed before Runner kept them, is refused whatever version is asked:
// without lineage or bytes its export would hold nothing a verifier could
// check it by beyond its release.
func TestARunWithoutLineageOrItsRecordsBytesIsNotExported(t *testing.T) {
	v := readNotMappedVector(t, noMappingRun)
	for name, change := range map[string]func(*Run){
		"failed": func(r *Run) {
			r.State, r.Problem, r.Result, r.Audit, r.AuditBytes = "failed", "The Runtime could not evaluate this input.", nil, nil, nil
		},
		"completed without the bytes": func(r *Run) { r.AuditBytes = nil },
	} {
		t.Run(name, func(t *testing.T) {
			b := v.bundle
			change(&b.Run)
			s := notMappedStore(t, v, encode(b))
			h := s.Handler("test")
			for _, query := range []string{"", "?version=2", "?version=3", "?version=4", "?version=5"} {
				refused(t, get(t, h, "/v1/runs/"+b.Run.ID+"/verification"+query, 422))
			}
		})
	}
}

// An export without lineage verifies as any export does, by its record's
// bytes, its chain entry and its signature sidecar, and says that its inputs
// are not mapped; nothing counts its targets.
func TestAnExportWithoutLineageVerifies(t *testing.T) {
	for dir, version := range map[string]int{noMappingRun: 4, v1FileMappingRun: 5} {
		t.Run(dir, func(t *testing.T) {
			v := readNotMappedVector(t, dir)
			verified, e := VerifyInputs(v.export, nil, v.release)
			if e != nil || verified.ExportVersion() != version || verified.Inputs() != InputsNotMapped || verified.RecordDigest() != digest(v.line) || verified.Classes != (InputClasses{}) || verified.Unsigned != nil {
				t.Fatal(e, verified.ExportVersion(), verified.Inputs())
			}
			if !sameJSON(verified.prepared.Facts, v.bundle.Run.Input.Facts) || !sameJSON(verified.prepared.Evidence, v.bundle.Run.Input.Evidence) || verified.prepared.Preparation != nil {
				t.Fatal("the inputs a re-execution evaluates are not the run's")
			}
			r, e := verified.CheckRunChain(bytes.NewReader(v.chain), v.held)
			if e != nil || r.Status != "valid" || !r.Witnessed || r.Scope != ScopeCheckpoint || r.Checkpoint != v.held[0] {
				t.Fatalf("%+v %v", r, e)
			}
			signature := verified.CheckSignature(mustKey(t, vectorKey1))
			if version == 5 && (signature.Status != SignatureSigned || signature.FindingsTotal != 0) || version == 4 && signature.Status != SignatureUnsigned {
				t.Fatalf("%+v", signature)
			}
			if version == 5 {
				if r := verified.CheckSignature(mustKey(t, vectorKey2)); r.Status != SignatureInvalid || r.Findings[0].Name != FindingSignatureInvalid {
					t.Fatalf("%+v", r)
				}
			}
		})
	}
}

// What verification holds an export without lineage to: the member and its
// version, the release and the mapping it froze, the audit record's inputs
// and citations, and the bytes, entry and sidecar every export is held to. A
// mapping-v2 export stripped of its lineage and marked so is refused.
func TestVerifyHoldsAnExportWithoutLineageToItsRun(t *testing.T) {
	noMapping, v1 := readNotMappedVector(t, noMappingRun), readNotMappedVector(t, v1FileMappingRun)
	v2, _, v2Release, v2Profiles := fixtureExport(t)
	surface, other := []byte(`"surface":"experimental evaluate"`), []byte(`"surface":"experimental evaluate again"`)
	cited := func(b *VerificationBundle) {
		b.Version, b.Chain, b.Run.AuditSignatures = 3, nil, nil
		b.Run.AuditBytes = bytes.Replace(b.Run.AuditBytes, []byte(`"kind":"evaluation"`), []byte(`"kind":"evaluation","cites":[{"pointer":"/request"}]`), 1)
		b.Run.Audit = bytes.Replace(b.Run.Audit, []byte(`"kind":"evaluation"`), []byte(`"kind":"evaluation","cites":[{"pointer":"/request"}]`), 1)
	}
	for name, c := range map[string]struct {
		v       notMappedVector
		change  func(*VerificationBundle)
		refusal string
	}{
		"the member dropped":  {noMapping, func(b *VerificationBundle) { b.Inputs = "" }, "run and frozen release differ"},
		"another member":      {noMapping, func(b *VerificationBundle) { b.Inputs = "unmapped" }, `the export's inputs member is not one Runner writes: only "not-mapped" is`},
		"in version 2":        {noMapping, func(b *VerificationBundle) { b.Version, b.Chain, b.Run.AuditBytes = 2, nil, nil }, "an export without lineage carries its audit record's bytes, from version 3 on: a version-2 export of it is not one Runner makes"},
		"another release":     {noMapping, func(b *VerificationBundle) { b.Run.ReleaseID = "release_other" }, "run and frozen release differ"},
		"a mapping added":     {noMapping, func(b *VerificationBundle) { b.Run.Input.Source = v1.bundle.Run.Input.Source }, "run and frozen release differ"},
		"the mapping dropped": {v1, func(b *VerificationBundle) { b.Run.Input.Source = nil }, "run and frozen release differ"},
		"another v1 mapping": {v1, func(b *VerificationBundle) {
			source := *b.Run.Input.Source
			source.Mapping.Facts = []FactMapping{{Target: "/request", Source: "/facts/other"}}
			b.Run.Input.Source = &source
		}, "run and frozen release differ"},
		"other facts": {noMapping, func(b *VerificationBundle) {
			b.Run.Input.Facts = bytes.Replace(b.Run.Input.Facts, []byte(`"completeness":"complete"`), []byte(`"completeness":"partial"`), 1)
		}, "retained audit inputs or citations do not match the run's inputs"},
		"other evidence":          {noMapping, func(b *VerificationBundle) { b.Run.Input.Evidence = v1.bundle.Run.Input.Evidence }, "retained audit inputs or citations do not match the run's inputs"},
		"no evidence":             {v1, func(b *VerificationBundle) { b.Run.Input.Evidence = nil }, "retained audit inputs or citations do not match the run's inputs"},
		"a citation":              {noMapping, cited, "retained audit inputs or citations do not match the run's inputs"},
		"the bytes changed":       {noMapping, func(b *VerificationBundle) { b.Run.AuditBytes = bytes.Replace(b.Run.AuditBytes, surface, other, 1) }, "the audit record differs from its original bytes"},
		"another run's entry":     {noMapping, func(b *VerificationBundle) { forge(t, b, func(e *ChainEntry) { e.Run = "run_other" }) }, "the run's chain entry names another run"},
		"another record's entry":  {v1, func(b *VerificationBundle) { forge(t, b, func(e *ChainEntry) { e.AuditDigest = emptyDigest }) }, "the run's chain entry names other bytes of the audit record than the export carries"},
		"no entry in version 4":   {noMapping, func(b *VerificationBundle) { b.Chain = nil }, "a version-4 export carries its run's entry in the installation's chain of runs"},
		"a sidecar in version 4":  {noMapping, func(b *VerificationBundle) { b.Run.AuditSignatures = v1.bundle.Run.AuditSignatures }, "a version-4 export carries no record signatures"},
		"no sidecar in version 5": {v1, func(b *VerificationBundle) { b.Run.AuditSignatures = nil }, "a version-5 export carries the signature sidecar of the run's audit record"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := VerifyInputs(withBundle(t, c.v.export, c.change), nil, c.v.release); e == nil || e.Error() != c.refusal {
				t.Fatal(e)
			}
		})
	}
	// A citation is refused for its own sake: the export without it, made
	// version 3 the same way, verifies.
	if _, e := VerifyInputs(withBundle(t, noMapping.export, func(b *VerificationBundle) { b.Version, b.Chain = 3, nil }), nil, noMapping.release); e != nil {
		t.Fatal(e)
	}
	// The member is a mapping-v2 export's to omit, and a mapping-v2 run
	// stripped of its lineage is not exported without it.
	for name, c := range map[string]struct {
		change  func(*VerificationBundle)
		refusal string
	}{
		"the member on a mapping-v2 export": {func(b *VerificationBundle) { b.Inputs = InputsNotMapped }, `an export with lineage is not "not-mapped"`},
		"a mapping-v2 run's lineage dropped": {func(b *VerificationBundle) { b.Inputs, b.Run.Input.Preparation = InputsNotMapped, nil },
			"the release froze a mapping v2, and its runs are exported with their lineage"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := VerifyInputs(withBundle(t, v2, c.change), v2Profiles, v2Release); e == nil || e.Error() != c.refusal {
				t.Fatal(e)
			}
		})
	}
}
