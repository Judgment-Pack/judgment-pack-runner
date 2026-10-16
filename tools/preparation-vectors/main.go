// Command preparation-vectors wrote vectors/preparation once, from fixtures
// the Runner's tests keep under internal/runner/testdata, and is not run
// again: the vectors are normative data, and changing one is a change of the
// contract a second reader is held to (vectors/preparation/README.md).
//
// It uses the standard library and none of the Runner's code. What it writes
// is held to the Runner's own computation by TestPreparationVectorsAreByteExact
// (internal/runner), and to verify-run's report by
// TestVerifyRunReportsEachPreparationVector (cmd/jpack-runner).
//
// It was run once, from the repository's root, with a verify-run built from
// the same commit, whose report on each vector the manifest records:
//
//	go build -o bin/jpack-runner ./cmd/jpack-runner
//	go run ./tools/preparation-vectors -runner bin/jpack-runner
//
// It refuses to write into a directory that exists. The ninth vector,
// numeric-spellings, was added by this generator afterwards, run into a new
// directory: it wrote the first eight again byte for byte, and only the new
// vector and its manifest entry were taken from that run. The tenth,
// http-shaped-source, was added the same way: the run wrote the first nine
// again byte for byte. So were the eleventh and twelfth, version-3-bound and
// unbound-bound-at-import, the first vectors of dialect version 3: the run
// wrote the first ten again byte for byte. So were the research vectors,
// the first of the kind research, twenty-six from the fixture plan of the
// specification's dialect (§11), from the runs in testdata/research: the
// run wrote the first twelve again byte for byte. So was the thirty-ninth,
// skipped-source-dependency-unavailable, the first to hold a skipped
// source: the run wrote the first thirty-eight again byte for byte.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	fixtures = "internal/runner/testdata"
	// bindingKind is the scheme of the binding Runner passes the Runtime.
	bindingKind = "judgment-pack-runner/preparation/1"
	// otherKind is the scheme the foreign-kind vector's record names instead.
	otherKind = "another-companion/preparation/1"
	// testSeed is the seed of RFC 8032's first test vector, under which the
	// valid fixture's record is signed. The foreign-kind vector's record is
	// signed again with it, as an operator who rewrote the record would sign
	// it. It signs nothing real.
	testSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	// valid is the fixture of the valid case, from which the altered vectors
	// are made.
	valid = "mapping-v2-calculated-bound"
)

// strongest is what the valid case is verified with: its record's signature
// under the fixture's key, required; its chain entry against the checkpoint
// held from when it was written, required; and its binding, required.
var strongest = []string{"--public-key", "@public-key.txt", "--require-signed", "--expect", "checkpoints.jsonl", "--require-witnessed", "--require-bound"}

// spec is one vector: the fixture it is made from, the one thing altered, and
// the finding verify-run must report of it.
type spec struct {
	name, from, altered, note string
	alter                     func(*vector) error
	// args are verify-run's arguments beyond --file, --profiles and
	// --release-digest. "@public-key.txt" stands for the key the file holds.
	args []string
	// key and held say whether the vector carries public-key.txt and
	// checkpoints.jsonl.
	key, held bool
	// want is the status of verify-run's report, or its refusal when it
	// writes none.
	want string
}

var specs = []spec{
	{name: "valid", from: valid, args: strongest, key: true, held: true, want: "verified-inputs",
		note: "A calculated run, signed, witnessed and bound, as Runner exported it (version 5). Its source is a calculator that computed, so its lineage carries a calculation and its freshness was judged by acquisition and by table. The record carries the binding Runner passed, whose digest is preparation.sha256: the SHA-256 of preparation.json, which is the preparation the Runner computes from mapping.json, case.json, sources.json and profiles.json at verifiedAt. A reader holding the record's binding to the preparation concludes that they match."},
	{name: "recomputed-one-second-later", from: valid, altered: "The export's preparation is recomputed one second later: its verifiedAt, and the runAt each outcome retains among its parameters. The record, its signature and its chain entry are as they were.", alter: oneSecondLater, args: strongest, key: true, held: true, want: "binding-mismatch",
		note: "preparation.json is what the Runner computes at the later instant, and the export retains it: every check of the inputs passes, and freshness is still within its limits. Its digest is not the one the record binds. A reader concludes binding-mismatch: the preparation, its instant included, is not the one the signed record binds."},
	{name: "unbound-record", from: "mapping-v2-before-calculators", args: []string{"--require-bound"}, want: "unbound",
		note: "A run Runner exported (version 2) before it passed a binding: its record carries none, as a Runtime before 0.31.0 records. Its preparation verifies. A reader concludes the binding is absent, and nothing in the record commits the preparation or its instant; --require-bound refuses it as unbound."},
	{name: "foreign-binding-kind", from: valid, altered: "The record's binding names another scheme, another-companion/preparation/1, with the preparation's digest; the record's chain entry and checkpoint are made again for the rewritten record, and the record is signed again under the fixture's key, as an operator who rewrote it would sign it.", alter: foreignKind, args: []string{"--public-key", "@public-key.txt", "--require-signed"}, key: true, want: "binding-foreign-kind",
		note: "The rewritten record's signature verifies. A reader concludes binding-foreign-kind before any digest is compared: a binding of another scheme commits nothing a reader of this scheme can check. A checkpoint held from before the rewrite would refuse the chain first, so none is supplied."},
	{name: "changed-table-lineage", from: valid, altered: "The export's retained lineage names another instant for the calculator's table: the first lineage entry's calculation.asOf, ecb-rates, is the preparation's verifiedAt.", alter: changedTable, args: strongest, key: true, held: true, want: "retained lineage does not match recomputation",
		note: "preparation.json is what the Runner computes from the inputs; the export retains another. A reader concludes that the retained preparation is not the recomputation, and verify-run refuses it with no report."},
	{name: "disposition-differs-from-audit", from: valid, altered: "The run's retained result names another outcome, decline-redirect, than its audit record, proceed.", alter: otherDisposition, args: strongest, key: true, held: true, want: "audit-disposition-mismatch",
		note: "The preparation is unchanged and verifies. The retained disposition is checked first, independently of any re-execution: a reader concludes audit-disposition-mismatch."},
	{name: "unsigned-parameters", from: "mapping-v2-unsigned-parameters", want: "verified-inputs",
		note: "A run of two acquired sources whose rules read parameters their receipts do not commit, as Runner exported it (version 2), before it passed a binding. Its preparation verifies, and the report names the parameters: runAt for the first source, a case parameter and one carried in ambiguous text for the second."},
	{name: "escaped-strings", from: "mapping-v2-before-exact-bytes", want: "verified-inputs",
		note: "A case-only run, as Runner exported it (version 2), before it passed a binding: every target is asserted, and nothing was acquired. A case value holds &, <, >, a letter with an accent and a snowman, so preparation.json shows the canonical form's escaping: &, < and > escaped as \\u0026, \\u003c and \\u003e, the others written as UTF-8."},
	{name: "numeric-spellings", from: "mapping-v2-calculated-numeric", args: strongest, key: true, held: true, want: "verified-inputs",
		note: "The valid case's mapping and inputs, run again with three integer case parameters more, which nothing reads: zero written -0, largest 9007199254740991 and least -9007199254740991, the largest and the least integers mapping v2 admits. Each outcome keeps them among its parameters as the case wrote them, and so does preparation.json: -0 is not 0, and neither large integer is written with an exponent. Numbers such as 1.00 and 1e+02, and integers beyond 2^53-1, are outside what mapping v2 admits (integers from -(2^53-1) to 2^53-1, with no fraction or exponent): preparation refuses them as a parameter and as a copied value, so no preparation holds them and no vector can. A reader concludes verified-inputs, the record's binding matching the preparation as written."},
	{name: "http-shaped-source", from: "mapping-v2-http-shaped", args: strongest, key: true, held: true, want: "verified-inputs",
		note: "A run whose one source is of shape http, as Runner exported it (version 5), signed, witnessed and bound. Its retained record, {result, receipt, salts}, was recorded from a stand-in, in the test's process, of a gateway source its operator declared http: an endpoint the test served over TLS, read as adapter-http reads one, and a receipt naming acquisition.shape http, the shape the profile pins, signed under the profile's key. No live service was asked. The request is the mapping's path, method and query, not a tool call; the result is the adapter's status, headers, bodyEncoding and body, and the rule reads it as any source's result is read. preparation.json has the form of every other vector's preparation: nothing in it depends on the source's shape, and nothing in it names the shape, which the profile the mapping pins by digest and the receipt it cites carry. A reader concludes verified-inputs, the record's binding matching the preparation."},
	{name: "version-3-bound", from: "mapping-v3-bound", args: strongest, key: true, held: true, want: "verified-inputs",
		note: "The valid case's mapping written as a dialect document of version 3, its one source bound: it names its input profile by id and digest, as a source of version 2 does. Runner writes a document of version 3 without provider, facts and evidence, the members of its version-1 mapping that its encoding of a version-2 document carries empty, so mappingDigest is the digest of the canon bytes of mapping.json as it is. The run, as Runner exported it (version 5), is signed, witnessed and bound. preparation.json has the form of the version-2 vectors' preparations, its version 3, the document's: a source of version 3 that is bound is prepared as one of version 2 is. A reader concludes verified-inputs, the record's binding matching the preparation."},
	{name: "unbound-bound-at-import", from: "mapping-v3-imported", args: strongest, key: true, held: true, want: "verified-inputs",
		note: "A dialect of version 3 published with its one source unbound, unbound.json: the source names the catalog platform intake-api and operation requests.read, the shape http and the record adapter-http, and no input profile. The binding document, binding.json, names that dialect by the digest of its canon bytes and binds the source to the installation's profile intake, by its id and digest. Runner bound them at import, as the run's request asked (source.import), and made the bound dialect, mapping.json: the published dialect with the source's platform, operation, shape and record removed and the profile and its digest added, every other member as written. The run keeps the bound dialect as its mapping and the two documents beside it, as given, and verify-run binds them again under the profiles it is given and holds the mapping to the result. The preparation's mappingDigest is the bound dialect's digest; it holds neither document's digest nor the names the binding removed. The source's record was recorded from a stand-in, in the test's process, of a gateway source of shape http, as http-shaped-source's was. A reader binds unbound.json with binding.json under profiles.json, holds the result to mapping.json, prepares it, and concludes verified-inputs, the record's binding matching the preparation."},
}

// afterResearch are the vectors added after the research vectors, each by a
// run into a new directory that wrote every earlier vector again byte for
// byte.
var afterResearch = []spec{
	{name: "skipped-source-dependency-unavailable", from: "mapping-v2-skipped-source", args: strongest, key: true, held: true, want: "verified-inputs",
		note: "A run of three operations of one profile, as Runner exported it (version 5), signed, witnessed and bound; its two records were signed by the test under a key made for the run, standing in for a gateway, and no live service was asked. intake reads the case's request and its type, and resolves them. sponsor reads the request's sponsor, and its record names none: its claim is unknown, no-sponsor-on-record. endorsement declares two source parameters, each a fact target of an earlier source: approver, sponsor's /sponsor/id, and requestType, intake's /intake/type. sponsor's claim is not resolved, so endorsement is skipped: it is not acquired, sources.json holds no record of it, cites holds no citation of it, and its read does not apply. A reader concludes that the skipped source's outcome carries its claim, {facts: {}, evidenceAvailability: {}, acquisitionStatus: unknown, reason: dependency-unavailable, basis: []}, the status unknown and the reason dependency-unavailable, no arguments, and as parameters those that have a value: runAt, requestId and requestType, and not approver. Each of its lineage entries, the fact /endorsement and the evidence sponsor-endorsement, carries the status unknown, the reason dependency-unavailable, present false, basis [], no receipt, and dependencies: the source parameters it waited on, each as {parameter, source, pointer}, in the order of the parameters' names, approver from sponsor before requestType from intake, though intake comes first among the sources. Runner writes this form for a skipped source of every kind. A reader recomputing the preparation concludes verified-inputs, the record's binding matching the preparation."},
}

// vector is a vector's files as they are made.
type vector struct {
	run, profiles []byte
	releaseDigest string
	key, held     []byte
	// preparation is the preparation the Runner computes, canonical: the one
	// the fixture's export retains, unless an alteration recomputed it.
	preparation []byte
	retained    string
	verifiedAt  string
}

type manifest struct {
	Family        string  `json:"family"`
	BindingKind   string  `json:"bindingKind"`
	CanonicalForm string  `json:"canonicalForm"`
	Generator     string  `json:"generator"`
	Vectors       []entry `json:"vectors"`
}

type entry struct {
	Name        string            `json:"name"`
	Note        string            `json:"note"`
	From        string            `json:"from"`
	Altered     *string           `json:"altered"`
	VerifiedAt  string            `json:"verifiedAt"`
	Files       map[string]string `json:"files"`
	Preparation preparationEntry  `json:"preparation"`
	// Import says, for a vector of a dialect bound at import, which file is
	// which; it is absent for every other vector.
	Import        *importEntry `json:"import,omitempty"`
	RecordBinding *string      `json:"recordBinding"`
	VerifyRun     verifyEntry  `json:"verifyRun"`
}

// importEntry names the files of a dialect bound at import: the published
// dialect, run.input.source.import.dialect; the binding document,
// run.input.source.import.binding; and the bound dialect Runner made of
// them, run.input.source.mapping, whose digest the preparation names.
type importEntry struct {
	Published string `json:"published"`
	Binding   string `json:"binding"`
	Bound     string `json:"bound"`
}

type preparationEntry struct {
	SHA256   string `json:"sha256"`
	Binding  string `json:"binding"`
	Retained string `json:"retained"`
}

type verifyEntry struct {
	Arguments []string `json:"arguments"`
	Exit      int      `json:"exit"`
	Status    *string  `json:"status"`
	Report    *string  `json:"report"`
	Error     *string  `json:"error"`
}

func main() {
	runner := flag.String("runner", "", "a jpack-runner built from this commit, whose verify-run report on each vector the manifest records")
	out := flag.String("out", "vectors/preparation", "the directory to write, which must not exist")
	flag.Parse()
	if err := generate(*runner, *out); err != nil {
		fmt.Fprintln(os.Stderr, "preparation-vectors:", err)
		os.Exit(1)
	}
}

func generate(runner, out string) error {
	if runner == "" {
		return errors.New("-runner is required")
	}
	runner, err := filepath.Abs(runner)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(out); err == nil {
		return fmt.Errorf("%s exists: the vectors are not regenerated", out)
	}
	m := manifest{
		Family:        "preparation",
		BindingKind:   bindingKind,
		CanonicalForm: "docs/MAPPING-V2.md, \"The canonical form of a preparation\"",
		Generator:     "tools/preparation-vectors, run once from the fixtures named by each vector's from; the vectors are not regenerated",
	}
	for _, s := range append(append(append([]spec{}, specs...), researchSpecs()...), afterResearch...) {
		e, err := write(s, runner, filepath.Join(out, s.name))
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		m.Vectors = append(m.Vectors, e)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "manifest.json"), append(b, '\n'), 0o644)
}

// write makes one vector's directory and its manifest entry.
func write(s spec, runner, dir string) (entry, error) {
	v, err := load(s)
	if err != nil {
		return entry{}, err
	}
	if s.alter != nil {
		if err = s.alter(v); err != nil {
			return entry{}, err
		}
	}
	files := map[string][]byte{
		"run.json":           v.run,
		"profiles.json":      v.profiles,
		"release-digest.txt": []byte(v.releaseDigest),
		"preparation.json":   v.preparation,
		"preparation.sha256": []byte(sha(v.preparation)),
	}
	for name, path := range map[string][]string{"mapping.json": {"mapping"}, "case.json": {"case"}, "sources.json": {"sources"}, "unbound.json": {"import", "dialect"}, "binding.json": {"import", "binding"}} {
		start, end, err := span(v.run, append([]string{"run", "input", "source"}, path...)...)
		if errors.Is(err, errNoMember) {
			continue
		}
		if err != nil {
			return entry{}, err
		}
		files[name] = v.run[start:end]
	}
	if s.key {
		files["public-key.txt"] = v.key
	}
	if s.held {
		files["checkpoints.jsonl"] = v.held
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return entry{}, err
	}
	digests := map[string]string{}
	for name, data := range files {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return entry{}, err
		}
		digests[name] = sha(data)
	}
	args := []string{"--file", "run.json", "--profiles", "profiles.json", "--release-digest", v.releaseDigest}
	for _, a := range s.args {
		if a == "@public-key.txt" {
			a = strings.TrimSpace(string(v.key))
		}
		args = append(args, a)
	}
	run, err := verifyRun(runner, dir, args)
	if err != nil {
		return entry{}, err
	}
	got := run.Error
	if run.Status != nil {
		got = run.Status
	}
	if got == nil || *got != s.want {
		return entry{}, fmt.Errorf("verify-run found %v, not %s: %+v", got, s.want, run)
	}
	record, err := recordBinding(v.run)
	if err != nil {
		return entry{}, err
	}
	e := entry{Name: s.name, Note: s.note, From: fixtures + "/" + s.from, VerifiedAt: v.verifiedAt, Files: digests, RecordBinding: record, VerifyRun: run,
		Preparation: preparationEntry{SHA256: sha(v.preparation), Binding: `{"kind":"` + bindingKind + `","digest":"` + sha(v.preparation) + `"}`, Retained: v.retained}}
	if s.altered != "" {
		e.Altered = &s.altered
	}
	if _, ok := files["unbound.json"]; ok {
		e.Import = &importEntry{Published: "unbound.json", Binding: "binding.json", Bound: "mapping.json"}
	}
	return e, nil
}

// load reads a fixture: its export, profiles, release digest and, where it
// keeps them, its public key and held checkpoints; and the preparation the
// export retains, in the canonical form.
func load(s spec) (*vector, error) {
	dir := filepath.Join(fixtures, s.from)
	v := &vector{retained: "equal"}
	var err error
	if v.run, err = os.ReadFile(filepath.Join(dir, "run.json")); err != nil {
		return nil, err
	}
	if v.profiles, err = os.ReadFile(filepath.Join(dir, "profiles.json")); err != nil {
		return nil, err
	}
	digest, err := os.ReadFile(filepath.Join(dir, "release-digest.txt"))
	if err != nil {
		return nil, err
	}
	v.releaseDigest = string(digest)
	if s.key {
		if v.key, err = os.ReadFile(filepath.Join(dir, "public-key.txt")); err != nil {
			return nil, err
		}
	}
	if s.held {
		if v.held, err = os.ReadFile(filepath.Join(dir, "checkpoints.jsonl")); err != nil {
			return nil, err
		}
	}
	start, end, err := span(v.run, "run", "input", "preparation")
	if err != nil {
		return nil, err
	}
	if v.preparation, err = canonical(v.run[start:end]); err != nil {
		return nil, err
	}
	var p struct {
		VerifiedAt string `json:"verifiedAt"`
	}
	if err = json.Unmarshal(v.preparation, &p); err != nil || p.VerifiedAt == "" {
		return nil, fmt.Errorf("the export's preparation names no verifiedAt: %v", err)
	}
	v.verifiedAt = p.VerifiedAt
	return v, nil
}

// oneSecondLater recomputes the export's preparation one second later, as
// the audit of runner #55 did: its verifiedAt, and the runAt each outcome
// retains among its parameters. Nothing in this preparation reads runAt, so
// nothing else changes. The record, its signature and its chain entry are as
// they were.
func oneSecondLater(v *vector) error {
	at, err := time.Parse("2006-01-02T15:04:05Z", v.verifiedAt)
	if err != nil {
		return err
	}
	later := at.Add(time.Second).Format("2006-01-02T15:04:05Z")
	start, end, err := span(v.run, "run", "input", "preparation")
	if err != nil {
		return err
	}
	var outcomes struct {
		Outcomes []json.RawMessage `json:"outcomes"`
	}
	if err = json.Unmarshal(v.run[start:end], &outcomes); err != nil || len(outcomes.Outcomes) == 0 {
		return fmt.Errorf("the preparation has no outcomes: %v", err)
	}
	moved, err := replace(v.run[start:end], `"verifiedAt":"`+v.verifiedAt+`"`, `"verifiedAt":"`+later+`"`, 1)
	if err != nil {
		return err
	}
	if moved, err = replace(moved, `"runAt":"`+v.verifiedAt+`"`, `"runAt":"`+later+`"`, len(outcomes.Outcomes)); err != nil {
		return err
	}
	v.run = splice(v.run, start, end, moved)
	v.verifiedAt = later
	v.preparation, err = canonical(moved)
	return err
}

// changedTable changes the instant the export's first lineage entry retains
// for the calculator's table to the preparation's own instant. The
// preparation the Runner computes is the fixture's.
func changedTable(v *vector) error {
	start, end, err := span(v.run, "run", "input", "preparation")
	if err != nil {
		return err
	}
	var p struct {
		Lineage []struct {
			Calculation *struct {
				AsOf map[string]string `json:"asOf"`
			} `json:"calculation"`
		} `json:"lineage"`
	}
	if err = json.Unmarshal(v.run[start:end], &p); err != nil || len(p.Lineage) == 0 || p.Lineage[0].Calculation == nil || p.Lineage[0].Calculation.AsOf["ecb-rates"] == "" {
		return fmt.Errorf("the first lineage entry has no table: %v", err)
	}
	asOf := p.Lineage[0].Calculation.AsOf["ecb-rates"]
	old := `"asOf":{"ecb-rates":"` + asOf + `"}`
	if n := bytes.Count(v.run[start:end], []byte(old)); n != len(p.Lineage) {
		return fmt.Errorf("the table's instant is retained %d times, not once per lineage entry", n)
	}
	// The lineage entries come in order, so the first is the first entry's.
	changed := bytes.Replace(v.run[start:end], []byte(old), []byte(`"asOf":{"ecb-rates":"`+v.verifiedAt+`"}`), 1)
	v.run = splice(v.run, start, end, changed)
	v.retained = "altered"
	return nil
}

// otherDisposition changes the outcome the run's retained result names; the
// audit record is as it was.
func otherDisposition(v *vector) error {
	start, end, err := span(v.run, "run", "result")
	if err != nil {
		return err
	}
	changed, err := replace(v.run[start:end], `"outcomeId":"proceed"`, `"outcomeId":"decline-redirect"`, 1)
	if err != nil {
		return err
	}
	v.run = splice(v.run, start, end, changed)
	return nil
}

// foreignKind rewrites the record's binding to name another scheme, with the
// preparation's digest, and makes the record's chain entry, its checkpoint
// and its signature again for the rewritten record: what an operator who
// rewrote the record would export.
func foreignKind(v *vector) error {
	from, to := `"binding":{"kind":"`+bindingKind+`",`, `"binding":{"kind":"`+otherKind+`",`
	var line []byte
	if err := member(v.run, &line, "run", "auditBytes"); err != nil {
		return err
	}
	rewritten, err := replace(line, from, to, 1)
	if err != nil {
		return err
	}
	if err = set(v, rewritten, "run", "auditBytes"); err != nil {
		return err
	}
	start, end, err := span(v.run, "run", "audit")
	if err != nil {
		return err
	}
	audit, err := replace(v.run[start:end], from, to, 1)
	if err != nil {
		return err
	}
	v.run = splice(v.run, start, end, audit)
	// The chain entry names the record by its digest, and the checkpoint the
	// entry by its own.
	var entryLine []byte
	if err = member(v.run, &entryLine, "chain", "entry"); err != nil {
		return err
	}
	newEntry, err := replace(entryLine, `"auditDigest":"`+sha(line)+`"`, `"auditDigest":"`+sha(rewritten)+`"`, 1)
	if err != nil {
		return err
	}
	if err = set(v, newEntry, "chain", "entry"); err != nil {
		return err
	}
	if start, end, err = span(v.run, "chain", "checkpoint"); err != nil {
		return err
	}
	checkpoint, err := replace(v.run[start:end], `"recordDigest":"`+sha(entryLine)+`"`, `"recordDigest":"`+sha(newEntry)+`"`, 1)
	if err != nil {
		return err
	}
	v.run = splice(v.run, start, end, checkpoint)
	// The sidecar's one line signs the record's trail, its sequence and the
	// digest of its line.
	var sidecar []byte
	if err = member(v.run, &sidecar, "run", "auditSignatures"); err != nil {
		return err
	}
	var signed struct {
		Record    string `json:"record"`
		Sequence  int    `json:"sequence"`
		Signature string `json:"signature"`
		Trail     string `json:"trail"`
	}
	if bytes.Count(sidecar, []byte("\n")) != 1 || json.Unmarshal(sidecar, &signed) != nil || signed.Record != sha(line) || signed.Sequence != 1 {
		return errors.New("the sidecar is not one line signing the record as the first of its trail")
	}
	seed, _ := hex.DecodeString(testSeed)
	key := ed25519.NewKeyFromSeed(seed)
	message := func(record string) []byte {
		return fmt.Appendf(nil, `judgment-pack-runtime/record-signature/1:{"record":"%s","sequence":%d,"trail":"%s"}`, record, signed.Sequence, signed.Trail)
	}
	old, err := hex.DecodeString(signed.Signature)
	if err != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), message(signed.Record), old) {
		return errors.New("the fixture's record is not signed under the test seed as the generator signs it")
	}
	newSidecar, err := replace(sidecar, `"record":"`+signed.Record+`"`, `"record":"`+sha(rewritten)+`"`, 1)
	if err != nil {
		return err
	}
	if newSidecar, err = replace(newSidecar, `"signature":"`+signed.Signature+`"`, `"signature":"`+hex.EncodeToString(ed25519.Sign(key, message(sha(rewritten))))+`"`, 1); err != nil {
		return err
	}
	return set(v, newSidecar, "run", "auditSignatures")
}

// researchRun is what every research vector's run is: how its documents
// were made, which no reader can check, and what nothing in it is.
const researchRun = "A run of the example %s of the specification's fixture plan (spec/dialect.md, section 11), as Runner exported it (version 5), signed, witnessed and bound. Its dialect, of version 3, was published with its research sources unbound (unbound.json), naming no profile, and bound at import by the binding document (binding.json) to the installation's AI profile and web profiles, each of class generated and declaring the capabilities it serves and the model (profiles.json); mapping.json is the bound dialect. Each research source's acquisition (sources.json) is the research record its caller assembled: the responses retained, each with the profile it came through, its request, its result, the receipt a gateway signed and the salts, and the typed answers, each citing the responses by their indexes. The responses were served, in the test's process, by a stand-in of a gateway over TLS, an AI connection's answer, a search connection's links and a public page, each receipt minted as a gateway mints one, under the stand-in's key, at call indexes of one session. No model was asked, no page outside the test's process was read, and every identifier and observation is synthetic. "

// researchReads is what a reader does with a research vector that reads.
const researchReads = " A reader binds unbound.json with binding.json under profiles.json and holds the result to mapping.json, replays each retained response under the profile it names, with the record's own receipt when it has one, recomputes the preparation from the records, its cites in the order of the sources, and concludes verified-inputs, the record's binding matching the preparation."

// researchSpecs are the research vectors: for each example of the fixture
// plan, the readings Runner can record, and two findings made from them.
func researchSpecs() []spec {
	var out []spec
	for _, example := range []string{"gtm-company-person", "work-item-triage"} {
		run := fmt.Sprintf(researchRun, example)
		reading := func(variant, says string) spec {
			name := example + "-" + variant
			return spec{name: name, from: "research/" + name, args: strongest, key: true, held: true, want: "verified-inputs", note: run + says + researchReads}
		}
		out = append(out, reading("valid-types-all-six", researchValid[example]))
		for _, variant := range []string{"optional-omitted", "required-not-found", "integer-zero", "decimal-zero", "boolean-false", "text-empty"} {
			out = append(out, reading("presence-"+variant, researchPresence[example+"/"+variant]+" Nothing absent, null or false is replaced by a default."))
		}
		out = append(out, reading("stale-and-conflicting-unknown", researchStale[example]))
		for _, variant := range []string{"all-unavailable", "secondary-found"} {
			out = append(out, reading("failed-fallback-"+variant, researchFallback+researchFallbackVariant[variant]))
		}
		out = append(out, reading("changed-binding-second-installation", researchSecond[example]))
		old := "research/" + example + "-valid-types-all-six"
		out = append(out, spec{name: example + "-changed-binding-old-preparation", from: "research/" + example + "-changed-binding-second-installation", args: strongest, key: true, held: true, want: "retained lineage does not match recomputation",
			altered: "The export of " + example + "-changed-binding-second-installation retains, in place of its own preparation, the preparation of " + example + "-valid-types-all-six, recorded under the first installation's binding seconds before.",
			alter:   oldPreparation(old),
			note:    run + researchSecond[example] + " Its export retains the first installation's preparation instead, the old preparation held fixed: preparation.json is what the Runner computes from the second installation's inputs, which differs. A reader given the retained preparation recomputes the second installation's at its verifiedAt, under its bound dialect, and concludes preparation-differs; verify-run refuses it with no report."})
		out = append(out, researchChanged[example](run))
	}
	return out
}

var researchValid = map[string]string{
	"gtm-company-person": "Its company source asks for answer alone, retaining one answer, and its read copies the record's whole fields object to /company. Its person source asks for answer, search and page, retaining an answer, a search's links and a page, each through its assigned profile, and its rule writes the person's id and the qualification /qualification/contactable from found values. All six types are answered: text, an integer, a decimal of scale two and a signed one, booleans, a list with values and an empty one, an integer range and a decimal range whose endpoints have different scales; every target is generated, with generated influence.",
	"work-item-triage":   "Its item source asks for answer, search and page, its search served by its AI profile and its page by a web profile, and its rule writes the item's id, its points and /triage/blocked from found values. Its follow-up source asks for answer alone, its question a $param, its record came back through the AI connection with a receipt of its own over the source's request and the whole record, and its read copies the record's whole fields object to /follow-up; its lineage names that receipt, and its receipts end with it. All six types are answered: text, integers, decimals, booleans, a list with duplicates and an empty string, an empty list, a decimal range with a signed endpoint and an integer range.",
}

var researchPresence = map[string]string{
	"gtm-company-person/optional-omitted":   "The valid-types run with every optional entry omitted: the company's and the person's roles and ranges. The copy writes the entries there are.",
	"gtm-company-person/required-not-found": "The valid-types run with a required entry not-found in each source, citing the response about the unsuccessful search: the company's headcount, which the copy keeps as recorded, and the person's contactability, so that the rule's found clause does not match and its last derives unknown, with no fact.",
	"gtm-company-person/integer-zero":       "The valid-types run with integer zero: the company's headcount and the person's years of experience, each found.",
	"gtm-company-person/decimal-zero":       "The valid-types run with the decimal \"0\": the company's revenue and the person's allocation, each found.",
	"gtm-company-person/boolean-false":      "The valid-types run with false: the company is not active, and the person may not be contacted, which the rule writes as /qualification/contactable false.",
	"gtm-company-person/text-empty":         "The valid-types run with the empty text: the company's id and the person's, which the rule writes as /person/id \"\".",
	"work-item-triage/optional-omitted":     "The valid-types run with every optional entry omitted: the item's labels and range, and every follow-up field but its id.",
	"work-item-triage/required-not-found":   "The valid-types run with a required entry not-found in each source: whether the item is blocked, citing the page, so that the rule derives unknown, and the follow-up's id, citing nothing.",
	"work-item-triage/integer-zero":         "The valid-types run with integer zero: the item's points, which the rule writes, and the follow-up's.",
	"work-item-triage/decimal-zero":         "The valid-types run with the decimal \"0\": the item's estimate and the follow-up's.",
	"work-item-triage/boolean-false":        "The valid-types run with false: the item is not blocked, which the rule writes as /triage/blocked false, and neither is the follow-up.",
	"work-item-triage/text-empty":           "The valid-types run with the empty text: the item's id, which the rule writes as /work-item/id \"\", and the follow-up's.",
}

var researchStale = map[string]string{
	"gtm-company-person": "The valid-types run, its receipts fresh, with explicitly stale observations, one citing nothing and one citing the search, and conflicting typed values, each observation with its own citations: the company's headcount stale and its revenue conflicting, which the copy keeps as recorded; the person's experience stale and its contactability conflicting, true on the answer and false on the page, so that the rule's clause reading an observation derives unknown, choosing neither.",
	"work-item-triage":   "The valid-types run, its receipts fresh, with explicitly stale observations, one citing nothing and one citing the search, and conflicting typed values, each observation with its own citations: the item's points stale and whether it is blocked conflicting, so that the rule derives unknown, choosing neither; the follow-up's estimate conflicting between \"1.0\" and \"1.00\", spelled apart, and its labels stale, which the copy keeps as recorded.",
}

const researchFallback = "Three research sources each ask for answer alone and declare the example's count field: the primary and the secondary copy their entry's status, and the third reconciles the two before it and itself (section 6.4) by a rule that tests their statuses in a declared order: the primary's found value, then the secondary's, a conflict anywhere unknown, then its own found value, and unknown when no candidate is left; each absence, not-found, stale and conflicting status is named by an exact guard. The reconciliation's citations are its own response's; the earlier sources keep theirs. "

var researchFallbackVariant = map[string]string{
	"all-unavailable": "The primary answers not-found, the secondary stale and the third conflicting: the failed fallback derives unknown, with no resolved target.",
	"secondary-found": "The primary answers not-found and the secondary found: the reconciliation resolves the target from the secondary's value.",
}

var researchSecond = map[string]string{
	"gtm-company-person": "The valid-types run as a second installation binds it: the same published dialect, the person's page bound to the profile reader, of the same gateway at another endpoint, in place of page. Every response through it names reader, and its receipt verifies under reader; the bound dialect and its digest differ from the first installation's, and nothing else does but the responses, asked again in a session of the gateway's own.",
	"work-item-triage":   "The valid-types run as a second installation binds it: the same published dialect, the follow-up's AI profile assistant-2, of the same gateway at another endpoint, in place of assistant. Every response through it, and the record's own receipt, name assistant-2 and verify under it; the bound dialect and its digest differ from the first installation's, and nothing else does but the responses, asked again in a session of the gateway's own.",
}

var researchChanged = map[string]func(string) spec{
	"gtm-company-person": func(run string) spec {
		return spec{name: "gtm-company-person-changed-response-retained-result", from: "research/gtm-company-person-valid-types-all-six", args: strongest, key: true, held: true,
			want:    "Source person: retained response 2, through the profile page: result digest failed (acquisition-invalid). No decision was requested.",
			altered: "In the export's research record of the source person, the result of the third retained response, the page, says person-0013 where it said person-0012; its receipt is as it was.",
			alter:   changedResponse(`A synthetic page about person-0012.`, `A synthetic page about person-0013.`),
			note:    run + "The valid-types run with one retained result changed and its receipt and the expected preparation held fixed: preparation.json is the preparation the run recorded, which the export retains, and the Runner computes none from the changed inputs. A reader replays the third response under its profile and concludes acquisition-invalid: its result is not the bytes its receipt names; verify-run refuses it with no report."}
	},
	"work-item-triage": func(run string) spec {
		return spec{name: "work-item-triage-changed-response-record-fields", from: "research/work-item-triage-valid-types-all-six", args: strongest, key: true, held: true,
			want:    "Source follow-up: the record's own receipt, through the profile assistant: result digest failed (acquisition-invalid). No decision was requested.",
			altered: "In the export's research record of the source follow-up, the answer for points is 3 where it was 2; the record's own receipt is as it was.",
			alter:   changedResponse(`"points":{"cites":[0],"status":"found","value":2}},"responses":[`, `"points":{"cites":[0],"status":"found","value":3}},"responses":[`),
			note:    run + "The valid-types run with the follow-up record's fields changed under the record's own receipt, which is held fixed, and the expected preparation held fixed: preparation.json is the preparation the run recorded, which the export retains, and the Runner computes none from the changed inputs. A reader replays the record's own receipt over the whole record and concludes acquisition-invalid; verify-run refuses it with no report."}
	},
}

// oldPreparation makes the export retain, in place of its own preparation,
// the preparation another fixture's export retains: the old preparation held
// fixed against inputs that make another. The Runner computes the export's
// own from its inputs, which preparation.json holds.
func oldPreparation(from string) func(*vector) error {
	return func(v *vector) error {
		other, err := os.ReadFile(filepath.Join(fixtures, from, "run.json"))
		if err != nil {
			return err
		}
		start, end, err := span(other, "run", "input", "preparation")
		if err != nil {
			return err
		}
		old := other[start:end]
		if start, end, err = span(v.run, "run", "input", "preparation"); err != nil {
			return err
		}
		if bytes.Equal(v.run[start:end], old) {
			return errors.New("the two preparations are one")
		}
		v.run = splice(v.run, start, end, old)
		v.retained = "altered"
		return nil
	}
}

// changedResponse changes, once, the export's research records, its
// run.input.source.sources, and nothing else: no receipt, and not the
// preparation the run recorded, which the export retains and preparation.json
// holds, since the Runner computes none from the changed inputs.
func changedResponse(from, to string) func(*vector) error {
	return func(v *vector) error {
		start, end, err := span(v.run, "run", "input", "source", "sources")
		if err != nil {
			return err
		}
		changed, err := replace(v.run[start:end], from, to, 1)
		if err != nil {
			return err
		}
		v.run = splice(v.run, start, end, changed)
		v.retained = "recorded"
		return nil
	}
}

// verifyRun runs verify-run on a vector's files, in its directory, and reads
// its report and its refusal.
func verifyRun(runner, dir string, args []string) (verifyEntry, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(runner, append([]string{"verify-run"}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, []string{}, &stdout, &stderr
	err := cmd.Run()
	e := verifyEntry{Arguments: args}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		e.Exit = exit.ExitCode()
		said := strings.TrimSuffix(stderr.String(), "\n")
		refusal, ok := strings.CutPrefix(said, "runner: ")
		if !ok || strings.Contains(refusal, "\n") {
			return e, fmt.Errorf("verify-run refused without one line of refusal: %q", stderr.String())
		}
		e.Error = &refusal
	case err != nil:
		return e, err
	}
	if stdout.Len() > 0 {
		report, ended := strings.CutSuffix(stdout.String(), "\n")
		var status struct {
			Status string `json:"status"`
		}
		if !ended || strings.Contains(report, "\n") || json.Unmarshal([]byte(report), &status) != nil || status.Status == "" {
			return e, fmt.Errorf("verify-run's report is not one line naming its status: %q", stdout.String())
		}
		e.Report, e.Status = &report, &status.Status
	}
	return e, nil
}

// recordBinding is the record's binding, as recorded: the value of its member
// named binding, read from the record's bytes in an export of version 3 or
// later, and from run.audit in a version-2 export, which carries none.
func recordBinding(run []byte) (*string, error) {
	var version int
	if err := member(run, &version, "version"); err != nil {
		return nil, err
	}
	var record []byte
	if version >= 3 {
		if err := member(run, &record, "run", "auditBytes"); err != nil {
			return nil, err
		}
	} else {
		start, end, err := span(run, "run", "audit")
		if err != nil {
			return nil, err
		}
		record = run[start:end]
	}
	start, end, err := span(record, "binding")
	if errors.Is(err, errNoMember) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	binding := string(record[start:end])
	return &binding, nil
}

var errNoMember = errors.New("no such member")

// span is where the value at path lies in raw: the bytes of the member named
// by each step in turn, exactly as written.
func span(raw []byte, path ...string) (int, int, error) {
	start, end := 0, len(raw)
	for _, name := range path {
		d := json.NewDecoder(bytes.NewReader(raw[start:end]))
		d.UseNumber()
		if t, err := d.Token(); err != nil || t != json.Delim('{') {
			return 0, 0, fmt.Errorf("%s: not an object", name)
		}
		found := false
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return 0, 0, err
			}
			var value json.RawMessage
			if err = d.Decode(&value); err != nil {
				return 0, 0, err
			}
			if t == name {
				valueEnd := start + int(d.InputOffset())
				valueStart := valueEnd - len(value)
				if !bytes.Equal(raw[valueStart:valueEnd], value) {
					return 0, 0, fmt.Errorf("%s: the member's bytes were not found", name)
				}
				start, end, found = valueStart, valueEnd, true
				break
			}
		}
		if !found {
			return 0, 0, fmt.Errorf("%s: %w", name, errNoMember)
		}
	}
	return start, end, nil
}

// member reads the value at path into dest.
func member(raw []byte, dest any, path ...string) error {
	start, end, err := span(raw, path...)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw[start:end], dest)
}

// set writes data, as base64 in a JSON string, as the value at path.
func set(v *vector, data []byte, path ...string) error {
	start, end, err := span(v.run, path...)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	v.run = splice(v.run, start, end, encoded)
	return nil
}

func splice(raw []byte, start, end int, with []byte) []byte {
	return append(append(append([]byte{}, raw[:start]...), with...), raw[end:]...)
}

// replace replaces old with new in b, which must hold it exactly n times.
func replace(b []byte, old, new string, n int) ([]byte, error) {
	if got := bytes.Count(b, []byte(old)); got != n {
		return nil, fmt.Errorf("%.60s occurs %d times, not %d", old, got, n)
	}
	return bytes.ReplaceAll(b, []byte(old), []byte(new)), nil
}

// canonical is the canonical form of a preparation (docs/MAPPING-V2.md): the
// value read with its numbers as written, and written again with Go's
// encoding/json, so its members sorted by name in byte order, no
// insignificant whitespace, and its strings escaped as that package escapes
// them.
func canonical(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("one JSON value required")
	}
	return json.Marshal(v)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}
