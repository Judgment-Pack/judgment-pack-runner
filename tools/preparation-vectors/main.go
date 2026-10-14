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
// vector and its manifest entry were taken from that run.
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
	Name          string            `json:"name"`
	Note          string            `json:"note"`
	From          string            `json:"from"`
	Altered       *string           `json:"altered"`
	VerifiedAt    string            `json:"verifiedAt"`
	Files         map[string]string `json:"files"`
	Preparation   preparationEntry  `json:"preparation"`
	RecordBinding *string           `json:"recordBinding"`
	VerifyRun     verifyEntry       `json:"verifyRun"`
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
	for _, s := range specs {
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
	for name, path := range map[string][]string{"mapping.json": {"mapping"}, "case.json": {"case"}, "sources.json": {"sources"}} {
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
