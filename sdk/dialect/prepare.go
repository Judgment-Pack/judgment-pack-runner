package dialect

import (
	"bytes"
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/dialect4"
)

// verification is what a preparation says of itself (spec/dialect.md §9.1).
const verification = "verified acquisitions; session sealing and input truth are not established"

// Preparation is one case's preparation through a Dialect.
type Preparation struct {
	bytes      []byte
	digest     string
	facts      []byte
	evidence   []byte
	parameters map[string]any
}

// Bytes is the preparation in the canonical form spec/dialect.md §9.5
// gives it: its members sorted, its number literals kept, and &, <, >,
// U+2028 and U+2029 escaped. Each call returns a new copy, which the
// caller may modify: the preparation, and its Digest, are unchanged.
func (p *Preparation) Bytes() []byte { return bytes.Clone(p.bytes) }

// Digest is the SHA-256 of Bytes, written sha256:<hex>, which a record's
// binding of the preparation names.
func (p *Preparation) Digest() string { return p.digest }

// Facts is the facts document the preparation writes, every present fact
// target's value at its target, in the canonical form of §9.5. Each call
// returns a new copy.
func (p *Preparation) Facts() []byte { return bytes.Clone(p.facts) }

// Evidence is the evidence document it writes, each present requirement's
// availability, in the canonical form of §9.5. Each call returns a new
// copy.
func (p *Preparation) Evidence() []byte { return bytes.Clone(p.evidence) }

// Prepare computes the preparation of one case through the dialect at the
// instant at, truncated to the second, which is its verifiedAt and the
// runAt its rule reads (spec/dialect.md §2.2.1, §2.2.6, §9). It holds the
// case to §12's bounds (the dialect, the case and an empty acquisitions
// document {} together at most 2 MiB; the case nested at most 32 levels),
// then takes each case parameter, in the order of their names, measuring a
// list's bounds before its type (§2.2.7); then applies the case's copy and
// its rule to the case, and holds every target to the dialect's admission
// for the class asserted. It returns an *Error for a case it does not
// prepare.
func (d *Dialect) Prepare(caseDocument []byte, at time.Time) (*Preparation, error) {
	if d.size+len(caseDocument)+2 > maxEnvelopeBytes {
		return nil, refuse(CodeOverBound, "the dialect, the case and the acquisitions are together over 2097152 bytes")
	}
	if err := dialect4.Measure(caseDocument, maxValueDepth); err != nil {
		return nil, fromEngine(err)
	}
	value, err := dialect4.ReadJSON(caseDocument, maxValueDepth)
	if err != nil {
		return nil, refuse(CodeMalformed, "the case is not one JSON document read strictly: "+err.Error())
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, refuse(CodeMalformed, "the case is not one object")
	}
	verifiedAt := at.UTC().Format("2006-01-02T15:04:05Z")
	scope := map[string]any{"runAt": verifiedAt}
	given := map[string]any{}
	for _, p := range d.parameters {
		v, ok := dialect4.Resolve(value, p.pointer)
		if ok && dialect4.IsList(p.typ) {
			if err := dialect4.ListBounds(v); err != nil {
				return nil, fromEngine(err)
			}
		}
		if !ok || !dialect4.OfType(v, p.typ) {
			return nil, refuse(CodeParameter, "case parameter "+p.name+" is missing or not of its type")
		}
		scope[p.name], given[p.name] = v, v
	}

	// The case's copy (spec/dialect.md §6.2).
	copied := dialect4.Claim{Facts: map[string]any{}, Evidence: map[string]string{}, Status: "resolved", Reason: "copied"}
	basis := map[string]bool{}
	for _, f := range d.facts {
		basis[f.source] = true
		v, ok := dialect4.Resolve(value, f.source)
		if !ok {
			continue
		}
		if _, err := dialect4.Canon(v); err != nil {
			return nil, refuse(CodeReadFailed, "a copied value is outside the canon domain")
		}
		dialect4.Write(copied.Facts, f.target, v)
	}
	for _, e := range d.evidence {
		basis[e.source] = true
		v, ok := dialect4.Resolve(value, e.source)
		if !ok {
			continue
		}
		s, _ := v.(string)
		if s != "present" && s != "absent" && s != "unknown" {
			return nil, refuse(CodeReadFailed, "a copied availability is not present, absent or unknown")
		}
		copied.Evidence[e.target] = s
	}
	copied.Basis = sortedKeys(basis)

	// The case's rule (spec/dialect.md §2.2.1), after its copy.
	var ruled dialect4.Claim
	var derivations dialect4.Derivations
	if d.rule != nil {
		if ruled, derivations, err = d.rule.Apply(value, scope); err != nil {
			return nil, fromEngine(err)
		}
	}

	facts, evidence := map[string]any{}, map[string]any{}
	lineage := []any{}
	segment := func(factTargets, evidenceTargets []string, claim dialect4.Claim, rule bool, candidates func(kind, target string) []string) error {
		for _, kind := range []string{"fact", "evidence"} {
			targets := factTargets
			if kind == "evidence" {
				targets = evidenceTargets
			}
			for _, target := range targets {
				classes, listed := d.admits[[2]string{kind, target}]
				if !listed {
					classes = []string{"asserted", "record"}
				}
				admitted := false
				for _, c := range classes {
					admitted = admitted || c == "asserted"
				}
				if !admitted {
					return refuse(CodeAdmission, "the "+kind+" target "+target+" does not admit the class asserted")
				}
				var present bool
				if kind == "fact" {
					var v any
					if v, present = dialect4.Resolve(claim.Facts, target); present {
						dialect4.Write(facts, target, v)
					}
				} else {
					var a string
					if a, present = claim.Evidence[target]; present {
						evidence[target] = a
					}
				}
				entry := map[string]any{
					"target": target, "kind": kind, "source": "case", "class": "asserted", "generatedInfluence": false,
					"present": present, "status": claim.Status, "reason": claim.Reason, "readDigest": d.readDigest,
					"basis": nonNil(claim.Basis),
				}
				if c := candidates(kind, target); len(c) > 0 {
					entry["candidateFrom"] = c
				}
				if rule {
					entry["derivation"] = derivations.Of(kind, target)
				}
				lineage = append(lineage, entry)
			}
		}
		return nil
	}
	copyFacts, copyEvidence := make([]string, 0, len(d.facts)), make([]string, 0, len(d.evidence))
	sources := map[[2]string][]string{}
	for _, f := range d.facts {
		copyFacts = append(copyFacts, f.target)
		sources[[2]string{"fact", f.target}] = []string{f.source}
	}
	for _, e := range d.evidence {
		copyEvidence = append(copyEvidence, e.target)
		sources[[2]string{"evidence", e.target}] = []string{e.source}
	}
	sort.Strings(copyFacts)
	sort.Strings(copyEvidence)
	if err := segment(copyFacts, copyEvidence, copied, false, func(kind, target string) []string { return sources[[2]string{kind, target}] }); err != nil {
		return nil, err
	}
	outcome := map[string]any{"name": "case", "status": copied.Status, "reason": copied.Reason, "parameters": scope, "claim": claimDocument(copied)}
	if d.rule != nil {
		if err := segment(d.ruleFacts, d.ruleEvidence, ruled, true, func(kind, target string) []string {
			if kind == "fact" {
				return d.rule.Candidates(target)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		outcome["rule"] = map[string]any{"claim": claimDocument(ruled), "status": ruled.Status, "reason": ruled.Reason}
	}
	document := map[string]any{
		"formatVersion": Format, "verifiedAt": verifiedAt, "mappingDigest": d.digest, "verification": verification,
		"cites": []any{}, "lineage": lineage, "outcomes": []any{outcome},
	}
	text, err := json.Marshal(document)
	if err != nil {
		return nil, refuse(CodeReadFailed, err.Error())
	}
	factBytes, _ := json.Marshal(facts)
	evidenceBytes, _ := json.Marshal(evidence)
	return &Preparation{bytes: text, digest: digestOf(text), facts: factBytes, evidence: evidenceBytes, parameters: given}, nil
}

func claimDocument(c dialect4.Claim) map[string]any {
	return map[string]any{"facts": c.Facts, "evidenceAvailability": c.Evidence, "acquisitionStatus": c.Status, "reason": c.Reason, "basis": nonNil(c.Basis)}
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// Result is one case's preparation in a batch, or why it was not prepared.
type Result struct {
	Preparation *Preparation
	Err         error
}

// PrepareBatch prepares every case through the dialect at one instant, as
// Prepare prepares each, and returns their results in the order of cases.
// The cases are prepared in parallel, on up to GOMAXPROCS goroutines; each
// result depends only on its case, so the batch's results are those of
// Prepare, whatever the order in which they are computed.
func (d *Dialect) PrepareBatch(cases [][]byte, at time.Time) []Result {
	out := make([]Result, len(cases))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(cases) {
		workers = len(cases)
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				p, err := d.Prepare(cases[i], at)
				out[i] = Result{p, err}
			}
		}()
	}
	for i := range cases {
		next <- i
	}
	close(next)
	wg.Wait()
	return out
}
