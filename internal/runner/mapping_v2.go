package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/derivation"
)

func (s *Service) normalizeInput(i Input) (Input, error) {
	if i.Source == nil || !i.Source.Mapping.dialect() && i.Source.Import == nil {
		return normalizeInput(i)
	}
	return normalizeV2(i, s.cfg.InputProfiles, time.Now().UTC().Truncate(time.Second))
}
func sameJSON(a, b []byte) bool {
	ac, e := canonical(a)
	if e != nil {
		return false
	}
	bc, e := canonical(b)
	return e == nil && bytes.Equal(ac, bc)
}
func writeTarget(root map[string]any, pointer string, value any) {
	parts, _ := pointerTokens(pointer, false)
	at := root
	for _, k := range parts[:len(parts)-1] {
		if at[k] == nil {
			at[k] = map[string]any{}
		}
		at = at[k].(map[string]any)
	}
	at[parts[len(parts)-1]] = value
}
func copyClaim(c CopyMapping, artifact any) (derivedClaim, error) {
	facts := map[string]any{}
	evidence := map[string]string{}
	basis := []string{}
	for _, f := range c.Facts {
		p, _ := pointerTokens(f.Source, true)
		v, ok := pointerRead(artifact, p)
		basis = append(basis, f.Source)
		if ok {
			writeTarget(facts, f.Target, v)
		}
	}
	for _, f := range c.Evidence {
		p, _ := pointerTokens(f.Source, true)
		v, ok := pointerRead(artifact, p)
		basis = append(basis, f.Source)
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || s != "present" && s != "absent" && s != "unknown" {
			return derivedClaim{}, errors.New("copied evidence must be present, absent or unknown")
		}
		evidence[f.Requirement] = s
	}
	sort.Strings(basis)
	return derivedClaim{encode(facts), evidence, "resolved", "copied", basis}, nil
}
func applyRead(read SourceRead, artifact any, params map[string]any) (derivedClaim, error) {
	artifact, err := unwrapRead(read, artifact)
	if err != nil {
		return derivedClaim{}, err
	}
	return deriveRead(read, artifact, params)
}

// unwrapRead applies a read's unwrap steps and bounds the value a read applies to.
func unwrapRead(read SourceRead, artifact any) (any, error) {
	for _, p := range read.Unwrap {
		tokens, _ := pointerTokens(p, true)
		v, ok := pointerRead(artifact, tokens)
		s, isString := v.(string)
		if !ok || !isString || len(s) > maxInputDocument {
			return nil, errors.New("unwrap requires one bounded JSON string")
		}
		next, e := readInputJSON([]byte(s))
		if e != nil {
			return nil, errors.New("unwrap contains invalid JSON")
		}
		artifact = next
	}
	if err := boundedArtifact(artifact); err != nil {
		return nil, err
	}
	return artifact, nil
}

// deriveRead applies a read's copy or rule to an unwrapped value.
func deriveRead(read SourceRead, artifact any, params map[string]any) (derivedClaim, error) {
	if read.Copy != nil {
		c, e := copyClaim(*read.Copy, artifact)
		if e != nil {
			return c, e
		}
		if _, e = proofCanon(encode(c)); e != nil {
			return c, e
		}
		return c, nil
	}
	out, e := derivation.Derive(read.Rule, encode(artifact), encode(params))
	if e != nil {
		return derivedClaim{}, fmt.Errorf("derivation rejected: %w", e)
	}
	var c derivedClaim
	e = json.Unmarshal(out, &c)
	return c, e
}
func candidateFrom(read SourceRead, target string) []string {
	set := map[string]bool{}
	if read.Copy != nil {
		for _, f := range read.Copy.Facts {
			if f.Target == target {
				set[f.Source] = true
			}
		}
		for _, e := range read.Copy.Evidence {
			if e.Requirement == target {
				set[e.Source] = true
			}
		}
	} else {
		var d ruleDocument
		_ = json.Unmarshal(read.Rule, &d)
		for _, cl := range d.Clauses {
			for _, f := range cl.Claim.Facts {
				if f.Pointer == target {
					set[f.From] = true
				}
			}
		}
	}
	return sortedKeys(set)
}
func normalizeV2(i Input, profiles []InputProfile, at time.Time) (Input, error) {
	return normalizeV2Mode(i, profiles, at, false)
}
func normalizeV2Mode(i Input, profiles []InputProfile, at time.Time, planning bool) (Input, error) {
	return prepareV2(i, profiles, at, planning, nil)
}

// prepareV2 is normalizeV2Mode. Given judged, it also lists there, in the
// mapping's order of sources, each source whose freshness it judged against
// at (mapping_v2_freshness.go): every verified acquisition, and whether the
// source is a calculator that computed, whose tables were judged too. What it
// lists is meaningful only when it returns no error.
func prepareV2(i Input, profiles []InputProfile, at time.Time, planning bool, judged *[]judgedFreshness) (Input, error) {
	profileKeys, e := validateProfileKeys(profiles)
	if e != nil {
		return i, e
	}
	// An imported dialect is bound again, under the profiles given, every
	// time it is prepared: the mapping must be the bound dialect its import
	// makes, or is made from it when the request names none.
	c, e := importedSource(i.Source, profiles)
	if e != nil {
		return i, e
	}
	if c == nil || !c.Mapping.dialect() {
		return i, inputProblem("mapping v2 required")
	}
	m := c.Mapping
	if e := m.validate(); e != nil {
		return i, e
	}
	if len(c.Snapshot) > 0 {
		return i, inputProblem("v2 uses named sources, not the v1 snapshot")
	}
	if len(encode(c)) > MaxBody {
		return i, inputProblem("v2 input exceeds aggregate limit")
	}
	if err := preflightPreparation(m, profiles, planning); err != nil {
		var typed researchError
		if !errors.As(err, &typed) {
			err = preparationCause{err, researchFail(codeMismatch, "profile does not match source")}
		}
		return i, preparationCause{inputProblem(err.Error()), err}
	}
	// A research source's acquisition is the record its caller retains
	// (research.go). Once the dialect has been held to its own form, its
	// profiles and its bindings (above), every record is held to the maxima
	// of §12, then to its form, and then each retained response's profile to
	// the source's assignments, before any acquisition is checked: the
	// maxima and the forms are refusals, the maxima first, and a profile
	// that does not fit precedes every other finding (spec/dialect.md §11).
	recipes := map[string]*researchRecipe{}
	retained := map[string]*researchAcquisition{}
	for _, source := range m.Sources {
		if !isResearch(source) {
			continue
		}
		recipe, err := readResearchRecipe(source, nil, false)
		if err != nil {
			return i, inputProblem("Mapping v3: " + err.Error())
		}
		recipes[source.Name] = recipe
	}
	for _, source := range m.Sources {
		if value, ok := c.Sources[source.Name]; ok && recipes[source.Name] != nil {
			if err := researchAcquisitionBounds(value, recipes[source.Name]); err != nil {
				return i, sourceFailure(source.Name, err)
			}
		}
	}
	for _, source := range m.Sources {
		if value, ok := c.Sources[source.Name]; ok && recipes[source.Name] != nil {
			a, err := readResearchAcquisition(value, recipes[source.Name])
			if err != nil {
				return i, sourceFailure(source.Name, err)
			}
			retained[source.Name] = a
		}
	}
	for _, source := range m.Sources {
		if a := retained[source.Name]; a != nil {
			if err := checkResponseProfiles(source, recipes[source.Name], a, profiles); err != nil {
				return i, sourceFailure(source.Name, err)
			}
		}
	}
	// Reject unknown responses even when planning stops at an earlier missing source.
	for name := range c.Sources {
		known := false
		for _, source := range m.Sources {
			known = known || source.Name == name
		}
		if planning && !known {
			return i, preparationCause{inputProblem("unexpected source response"), researchFail(codeUnexpected, "unexpected source response")}
		}
	}
	hash := mappingV2Hash(m)
	if c.MappingDigest != "" && c.MappingDigest != hash {
		return i, inputProblem("mapping digest changed")
	}
	if len(c.Case) == 0 {
		c.Case = json.RawMessage(`{}`)
	}
	caseValue, e := readInputJSON(c.Case)
	if e != nil {
		return i, inputProblem("invalid case JSON")
	}
	if _, ok := caseValue.(map[string]any); !ok {
		return i, inputProblem("case must be one object")
	}
	facts := map[string]any{}
	evidence := map[string]string{}
	caseParams := map[string]any{"runAt": at.UTC().Format("2006-01-02T15:04:05Z")}
	caseTypes := map[string]string{"runAt": "timestamp"}
	prep := &Preparation{Version: m.Version, VerifiedAt: at.UTC().Format("2006-01-02T15:04:05Z"), MappingDigest: hash, Verification: "verified acquisitions; session sealing and input truth are not established", Cites: []Citation{}, Lineage: []TargetLineage{}, Outcomes: []SourceOutcome{}}
	claims := map[string]derivedClaim{}
	generated := map[string]bool{}
	used := map[string]bool{}
	cites := map[string]bool{}
	// The acquisitions of one input are in one of two forms, and an input
	// that is in neither is refused. Either they share a session, as a
	// caller that prepares every source in one session leaves them; or each
	// is alone in a session of its own and is the first call of it, as
	// Runner leaves them when it acquires unattended. One acquisition alone
	// shares a session with itself, at any call index. Neither form says a
	// session is sealed or complete.
	shared, apart, first := true, true, ""
	admit := func(cite Citation) error {
		key := fmt.Sprintf("%s/%d", cite.SessionID, cite.CallIndex)
		if cites[key] {
			return researchFail(codeSessions, "one receipt cannot stand in for multiple source acquisitions")
		}
		cites[key] = true
		if len(prep.Cites) == 0 {
			first = cite.SessionID
		}
		// Two first calls of one session would be one receipt twice,
		// which is refused above: so first calls are of sessions apart.
		shared = shared && cite.SessionID == first
		apart = apart && cite.CallIndex == 0
		if !shared && !apart {
			return researchFail(codeSessions, "acquisitions must share one session, or each be the first call of a session of its own")
		}
		prep.Cites = append(prep.Cites, cite)
		return nil
	}
	// The research records acquired, by their sources' names, which a later
	// reconciliation reads (spec/dialect.md §6.4).
	records := map[string]any{}
	var admissionFailure error
	merge := func(name, class string, gen bool, read SourceRead, claim derivedClaim, deps []Dependency, cite *Citation, receipts *[]Citation, calc *CalculationLineage) error {
		ft, et, err := readTargets(read)
		if err != nil {
			return err
		}
		fv, err := readInputJSON(claim.Facts)
		if err != nil {
			return err
		}
		b, _ := proofCanon(encode(read))
		rd := digest(b)
		for _, f := range ft {
			if !admitted(m, "facts", f.Target, class, gen) {
				if admissionFailure == nil {
					message := "fact target does not admit source class or generated dependency"
					if name != "case" {
						message = fmt.Sprintf("source %s: %s", name, message)
					}
					typed := researchFail(codeAdmission, "%s", message)
					if recipes[name] != nil {
						message = typed.Error()
					}
					admissionFailure = preparationCause{inputProblem(message), typed}
				}
			}
			tokens, _ := pointerTokens(f.Target, false)
			v, present := pointerRead(fv, tokens)
			if present {
				writeTarget(facts, f.Target, v)
			}
			prep.Lineage = append(prep.Lineage, TargetLineage{f.Target, "fact", name, class, gen, present, claim.Status, claim.Reason, rd, claim.Basis, candidateFrom(read, f.Target), deps, cite, receipts, calc})
		}
		for _, f := range et {
			if !admitted(m, "evidence", f.Requirement, class, gen) {
				if admissionFailure == nil {
					message := "evidence target does not admit source class or generated dependency"
					if name != "case" {
						message = fmt.Sprintf("source %s: %s", name, message)
					}
					typed := researchFail(codeAdmission, "%s", message)
					if recipes[name] != nil {
						message = typed.Error()
					}
					admissionFailure = preparationCause{inputProblem(message), typed}
				}
			}
			v, present := claim.Evidence[f.Requirement]
			if present {
				evidence[f.Requirement] = v
			}
			prep.Lineage = append(prep.Lineage, TargetLineage{f.Requirement, "evidence", name, class, gen, present, claim.Status, claim.Reason, rd, claim.Basis, candidateFrom(read, f.Requirement), deps, cite, receipts, calc})
		}
		return nil
	}
	if m.Case != nil {
		for _, n := range sortedKeys(m.Case.Parameters) {
			p := m.Case.Parameters[n]
			tokens, _ := pointerTokens(p.Pointer, true)
			v, ok := pointerRead(caseValue, tokens)
			if !ok || !paramValue(v, p.Type) {
				return i, sourceFailure("case", researchFail("parameter-invalid", "parameter %s is missing or mistyped", n))
			}
			caseParams[n] = v
			caseTypes[n] = p.Type
		}
		read := SourceRead{Copy: &CopyMapping{m.Case.Facts, m.Case.Evidence}}
		claim, err := applyRead(read, caseValue, caseParams)
		if err != nil {
			return i, sourceFailure("case", err)
		}
		if err = merge("case", "asserted", false, read, claim, nil, nil, nil, nil); err != nil {
			return i, sourceFailure("case", err)
		}
		prep.Outcomes = append(prep.Outcomes, SourceOutcome{Name: "case", Status: claim.Status, Reason: claim.Reason, Parameters: encode(caseParams), Claim: encode(claim)})
	}
	for _, source := range m.Sources {
		fail := func(err error) (Input, error) { return i, sourceFailure(source.Name, err) }
		params := map[string]any{}
		types := map[string]string{}
		for n, v := range caseParams {
			params[n] = v
			types[n] = caseTypes[n]
		}
		deps := []Dependency{}
		gen := false
		unavailable := false
		for _, n := range sortedKeys(source.Parameters) {
			p := source.Parameters[n]
			up := claims[p.From]
			tokens, _ := pointerTokens(p.Pointer, true)
			fv, _ := readInputJSON(up.Facts)
			v, ok := pointerRead(fv, tokens)
			deps = append(deps, Dependency{n, p.From, p.Pointer})
			gen = gen || generated[p.From]
			if up.Status != "resolved" || !ok {
				unavailable = true
				continue
			}
			if !paramValue(v, p.Type) {
				return fail(researchFail("parameter-invalid", "validated parameter %s is mistyped", n))
			}
			params[n] = v
			types[n] = p.Type
		}
		class := "asserted"
		var profile InputProfile
		verified := source.Kind == "operation" || source.Provider == "google-drive"
		research := isResearch(source)
		if verified {
			profile, e = profileFor(source, profiles)
			if e != nil {
				return fail(e)
			}
			class = profile.Class
		}
		if research {
			// Its profiles were held to its assignments in preflight: each
			// of class generated, the AI profile serving answer.
			served, err := researchProfiles(source, recipes[source.Name], profiles)
			if err != nil {
				return fail(err)
			}
			profile, class = served["answer"], classGenerated
		}
		gen = gen || class == "generated"
		generated[source.Name] = gen
		value, present := c.Sources[source.Name]
		used[source.Name] = true
		claim := derivedClaim{json.RawMessage(`{}`), map[string]string{}, "unknown", "dependency-unavailable", []string{}}
		var args json.RawMessage
		var cite *Citation
		var receipts *[]Citation
		var calc *CalculationLineage
		if unavailable {
			if present && research {
				return fail(researchFail(codeUnexpected, "a skipped dependent source must not supply a response"))
			}
			if present {
				return fail(researchFail(codeUnexpected, "a skipped dependent source must not supply a response"))
			}
		} else if research {
			// The request is the recipe, recorded and never sent: Runner
			// asks no model and reads no page (research.go).
			if args, e = researchRequest(source, params, types); e != nil {
				return fail(e)
			}
			if !present {
				if planning {
					return i, &acquisitionNeeded{PlannedSource{Name: source.Name, Kind: source.Kind, Profile: profile.ID, Source: profile.Source, Arguments: args}}
				}
				return fail(researchFail(codeMissing, "the research record its caller retains is missing"))
			}
			cite, receipts, e = replayResearch(retained[source.Name], args, profile, profiles, profileKeys, at, source.MaxAge, admit)
			if e != nil {
				return fail(e)
			}
			records[source.Name] = retained[source.Name].record
			claim, e = readResearch(source, retained[source.Name].record, records, params)
			if e != nil {
				return fail(e)
			}
		} else {
			var artifact any
			if verified {
				template, _ := readInputJSON(source.Arguments)
				request, err := instantiate(template, params, types, false)
				if err != nil {
					return fail(err)
				}
				args, e = proofCanon(encode(request))
				if e != nil {
					return fail(e)
				}
				// As instantiated for this case, an MCP operation's request
				// is still a tool call its profile allows; the request of an
				// operation of another shape is the one its receipt commits
				// to, and is held to nothing more (adapterShapes).
				if source.Kind == "operation" && profile.Shape == "mcp" {
					o := request.(map[string]any)
					tool, ok := o["tool"].(string)
					allowed := false
					for _, t := range profile.Tools {
						allowed = allowed || tool == t
					}
					if !ok || !allowed || len(o) != 2 {
						return fail(errors.New("MCP request must name an allowed tool and arguments"))
					}
					if _, ok := o["arguments"].(map[string]any); !ok {
						return fail(errors.New("MCP tool arguments must be an object"))
					}
				}
			}
			if !present {
				if planning {
					return i, &acquisitionNeeded{PlannedSource{Name: source.Name, Kind: source.Kind, Provider: source.Provider, Profile: source.Profile, Source: profile.Source, Arguments: args}}
				}
				return fail(researchFail(codeMissing, "required source response is missing"))
			}
			if source.Kind == "selected-file" {
				if len(value.Response) > 0 {
					return fail(errors.New("selected files retain their snapshot proof"))
				}
				temporary := &SourceInput{Mapping: InputMapping{Version: 1, Provider: source.Provider}, Snapshot: value.Snapshot}
				doc, snap, _, err := readSourceDocument(temporary, false)
				if err != nil {
					return fail(err)
				}
				artifact = doc
				if verified {
					result, citation, err := verifyPreparationAcquisition([]byte(snap.Proof.Response), args, profile, profileKeys[profile.ID], at, source.MaxAge)
					if err != nil {
						return fail(err)
					}
					cite = citation
					if snap.Proof.PublicKey != profile.PublicKey || snap.Proof.Authority != profile.Authority || snap.Proof.Session != cite.SessionID {
						return fail(errors.New("file proof identity differs from the verified receipt"))
					}
					var record sourceRecord
					_ = json.Unmarshal(result, &record)
					var request map[string]any
					requestValue, _ := readInputJSON(args)
					request = requestValue.(map[string]any)
					if request["fileId"] != snap.Proof.Drive.FileID || request["grant"] != snap.Proof.Drive.Grant || len(request) != 2 || record.Original.Bytes != snap.Original.Bytes {
						return fail(errors.New("selected file differs from the committed request"))
					}
				}
			} else {
				if len(value.Snapshot) > 0 {
					return fail(errors.New("operations must supply a response only"))
				}
				result, citation, err := verifyPreparationAcquisition(value.Response, args, profile, profileKeys[profile.ID], at, source.MaxAge)
				if err != nil {
					return fail(err)
				}
				cite = citation
				artifact, e = readInputJSON(result)
				if e != nil {
					return fail(e)
				}
			}
			if cite != nil {
				if e = admit(*cite); e != nil {
					return fail(e)
				}
			}
			artifact, e = unwrapRead(source.Read, artifact)
			if e != nil {
				return fail(e)
			}
			if source.Calculation != nil {
				if profile.Calculator == nil {
					return fail(errors.New("a calculated source requires a calculator's profile"))
				}
				calc, e = checkCalculation(source, *profile.Calculator, parameterOrigins(m.Case, source), artifact, params, at)
				if e != nil {
					return fail(e)
				}
			}
			if calc != nil && calc.Status != calculationComputed {
				claim, e = calculationUnknown(source.Read, calc.Status)
			} else {
				claim, e = deriveRead(source.Read, artifact, params)
			}
			if e != nil {
				return fail(e)
			}
		}
		if e = merge(source.Name, class, gen, source.Read, claim, deps, cite, receipts, calc); e != nil {
			return fail(e)
		}
		// A verified source that was not skipped had its acquisition's times
		// judged against at, and a calculator that computed its tables'
		// instants too: either failure would have failed preparation above.
		// So did a research source each response it retains.
		if (verified || research) && !unavailable && judged != nil {
			*judged = append(*judged, judgedFreshness{source.Name, calc != nil && calc.Status == calculationComputed})
		}
		claims[source.Name] = claim
		prep.Outcomes = append(prep.Outcomes, SourceOutcome{source.Name, claim.Status, claim.Reason, encode(params), args, encode(claim)})
	}
	for n := range c.Sources {
		if !used[n] {
			return i, preparationCause{inputProblem("unexpected source response"), researchFail(codeUnexpected, "unexpected source response")}
		}
	}
	if admissionFailure != nil {
		return i, admissionFailure
	}
	// By call index, as before. Where each acquisition is the first call of
	// its own session the indexes are equal, and the order is the mapping's
	// order of sources: the sort keeps equal elements as they were. No test
	// tells this sort from one that does not promise to: for as many
	// sources as a mapping may have, the other keeps them too. A preparation
	// with a research source keeps the order of sources instead, each
	// research source's citations in the order its record retains them, the
	// record's own last (spec/dialect.md §9.1).
	if len(recipes) == 0 {
		sort.SliceStable(prep.Cites, func(a, b int) bool { return prep.Cites[a].CallIndex < prep.Cites[b].CallIndex })
	}
	c.MappingDigest = hash
	mapped := Input{Facts: encode(facts), Source: c, Preparation: prep}
	for _, l := range prep.Lineage {
		if l.Kind == "evidence" {
			mapped.Evidence = encode(evidence)
			break
		}
	}
	if len(i.Facts) > 0 && !sameJSON(i.Facts, mapped.Facts) {
		return i, inputProblem("supplied facts differ from the verified derivation")
	}
	if len(i.Evidence) > 0 && !sameJSON(i.Evidence, mapped.Evidence) {
		return i, inputProblem("supplied evidence differs from the verified derivation")
	}
	return mapped, mapped.validate()
}
