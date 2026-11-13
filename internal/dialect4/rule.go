package dialect4

import (
	"encoding/json"
	"math/big"
	"sort"
)

// Rule is a rule of version 2.0.0 held to its form (spec/dialect.md §2.2.4).
// It is read-only once checked, and safe for concurrent use.
type Rule struct {
	// Parameters are the parameters the rule declares, by name, with their
	// types.
	Parameters map[string]string
	// FactTargets and EvidenceTargets are the targets its clauses write,
	// each once, in the order the clauses first name them.
	FactTargets     []string
	EvidenceTargets []string
	clauses         []map[string]any
	derived         []map[string]any
	builtins        map[string]string
	candidates      map[string]map[string]bool
	listed          []string
}

// IsRule2 reports whether raw, a rule as written or decoded, names rule
// version 2.0.0.
func IsRule2(raw any) bool {
	if b, ok := raw.(json.RawMessage); ok {
		raw = []byte(b)
	}
	if b, ok := raw.([]byte); ok {
		var head struct {
			Version any `json:"ruleVersion"`
		}
		if json.Unmarshal(b, &head) != nil {
			return false
		}
		return head.Version == RuleVersion
	}
	m, ok := raw.(map[string]any)
	return ok && m["ruleVersion"] == RuleVersion
}

// closed holds v to an object with every member of required, and no member
// that is not in required or optional.
func closed(v any, required, optional []string, what string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, malformed("%s is not an object", what)
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, malformed("%s lacks %s", what, k)
		}
	}
	for k := range m {
		if !contains(required, k) && !contains(optional, k) {
			return nil, malformed("%s has a member its form does not have", what)
		}
	}
	return m, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The leaves of rule version 2.0.0, each with the members beside its
// operand, and the types each parameter-naming member takes (nil: any).
var leaves = map[string][]string{
	"exists":          nil,
	"equals":          {"to"},
	"equalsParam":     {"param"},
	"isTrue":          nil,
	"isDecimalString": nil,
	"freshWithin":     {"asOf", "maxAge"},
	"aheadWithin":     {"asOf", "maxAhead"},
	"compareParam":    {"relation", "param"},
	"inParam":         {"param"},
}

var derivedOperand = map[string]bool{"exists": true, "equals": true, "equalsParam": true, "compareParam": true}

var parameterTypes = map[[2]string][]string{
	{"equalsParam", "param"}:    nil,
	{"freshWithin", "asOf"}:     {TypeTimestamp},
	{"freshWithin", "maxAge"}:   {TypeInteger},
	{"aheadWithin", "asOf"}:     {TypeTimestamp},
	{"aheadWithin", "maxAhead"}: {TypeInteger},
	{"compareParam", "param"}:   {TypeInteger, TypeDecimal},
	{"inParam", "param"}:        {TypeStringList, TypeAddressRangeList},
}

var relations = []string{"less-than", "less-than-or-equal", "greater-than", "greater-than-or-equal"}

func checkCondition(c any, declared, derived map[string]string, what string) error {
	m, ok := c.(map[string]any)
	op, isString := m["op"].(string)
	if !ok || !isString {
		return malformed("%s: a condition is not an object with an op that is a string", what)
	}
	switch op {
	case "always":
		_, err := closed(m, []string{"op"}, nil, what)
		return err
	case "all", "any":
		if _, err := closed(m, []string{"op", "of"}, nil, what); err != nil {
			return err
		}
		of, ok := m["of"].([]any)
		if !ok {
			return malformed("%s: of is not a list", what)
		}
		if len(of) > maxListed {
			return failure(CodeOverBound, "%s: more than %d conditions listed", what, maxListed)
		}
		for _, x := range of {
			if err := checkCondition(x, declared, derived, what); err != nil {
				return err
			}
		}
		return nil
	case "not":
		if _, err := closed(m, []string{"op", "of"}, nil, what); err != nil {
			return err
		}
		return checkCondition(m["of"], declared, derived, what)
	}
	members, known := leaves[op]
	if !known {
		return malformed("%s: an unknown op", what)
	}
	operands := []string{"field"}
	if derivedOperand[op] {
		operands = []string{"field", "derived"}
	}
	if _, err := closed(m, append([]string{"op"}, members...), operands, what); err != nil {
		return err
	}
	_, hasField := m["field"]
	_, hasDerived := m["derived"]
	if hasField == hasDerived {
		return malformed("%s: a leaf names exactly one of field and derived", what)
	}
	if hasField && !isPointer(m["field"], true) {
		return malformed("%s: a field that is not a pointer", what)
	}
	if hasDerived {
		name, ok := m["derived"].(string)
		if _, declaredHere := derived[name]; !ok || !declaredHere {
			return malformed("%s: a derived value the rule does not declare", what)
		}
	}
	if op == "equals" && !inCanonDomain(m["to"]) {
		return malformed("%s: a literal outside the canon domain", what)
	}
	if op == "compareParam" {
		relation, _ := m["relation"].(string)
		if !contains(relations, relation) {
			return malformed("%s: a relation other than the four", what)
		}
	}
	for _, member := range members {
		if member == "to" || member == "relation" {
			continue
		}
		name, ok := m[member].(string)
		typ, declaredHere := declared[name]
		if !ok || !declaredHere {
			return malformed("%s: %s names no parameter the rule declares", what, member)
		}
		if admitted := parameterTypes[[2]string{op, member}]; admitted != nil && !contains(admitted, typ) {
			return malformed("%s: %s names a parameter of a type %s does not take", what, member, op)
		}
	}
	return nil
}

func checkOperand(v any, declared map[string]string, kind, what string) error {
	m, err := closed(v, nil, []string{"field", "param"}, what)
	if err != nil {
		return err
	}
	if len(m) != 1 {
		return malformed("%s: an operand names exactly one of field and param", what)
	}
	if field, ok := m["field"]; ok {
		if !isPointer(field, true) {
			return malformed("%s: an operand field that is not a pointer", what)
		}
		return nil
	}
	name, ok := m["param"].(string)
	if !ok || declared[name] != kind {
		return malformed("%s: an operand param that names no declared parameter of type %s", what, kind)
	}
	return nil
}

func checkDerived(v any, declared map[string]string, what string) ([]map[string]any, map[string]string, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, nil, malformed("%s: derived is not an array of one to %d derived values", what, MaxDerived)
	}
	if len(list) > MaxDerived {
		return nil, nil, failure(CodeOverBound, "%s: more than %d derived values", what, MaxDerived)
	}
	names := map[string]string{}
	var out []map[string]any
	for _, entry := range list {
		here := what + " derived value"
		m, _ := entry.(map[string]any)
		builtin, _ := m["builtin"].(string)
		switch builtin {
		case "duration":
			if _, err := closed(entry, []string{"name", "builtin", "from", "to"}, nil, here); err != nil {
				return nil, nil, err
			}
			for _, end := range []string{"from", "to"} {
				if err := checkOperand(m[end], declared, TypeTimestamp, here); err != nil {
					return nil, nil, err
				}
			}
		case "distance":
			if _, err := closed(entry, []string{"name", "builtin", "from", "to"}, nil, here); err != nil {
				return nil, nil, err
			}
			for _, end := range []string{"from", "to"} {
				point, err := closed(m[end], []string{"latitude", "longitude"}, nil, here)
				if err != nil {
					return nil, nil, err
				}
				for _, member := range []string{"latitude", "longitude"} {
					if err := checkOperand(point[member], declared, TypeDecimal, here); err != nil {
						return nil, nil, err
					}
				}
			}
		case "speed":
			if _, err := closed(entry, []string{"name", "builtin", "distance", "duration"}, nil, here); err != nil {
				return nil, nil, err
			}
			for _, member := range []string{"distance", "duration"} {
				name, ok := m[member].(string)
				if !ok || names[name] != member {
					return nil, nil, malformed("%s: a speed names no earlier distance and duration", here)
				}
			}
		default:
			return nil, nil, malformed("%s: an unknown built-in", here)
		}
		name, ok := m["name"].(string)
		if _, given := names[name]; !ok || name == "" || given {
			return nil, nil, malformed("%s: a name that is empty or given twice", here)
		}
		names[name] = builtin
		out = append(out, m)
	}
	return out, names, nil
}

var statuses = []string{"resolved", "absent", "unknown"}
var availabilities = []string{"present", "absent", "unknown"}

// CheckRule holds raw, a rule decoded with UseNumber, to the form of rule
// version 2.0.0 (spec/dialect.md §2.2.4), each parameter it declares to be
// in scope with its type. A nil scope skips that last check, for a caller
// that reads only the rule's targets of a rule already held to its scope.
func CheckRule(raw any, scope map[string]string, what string) (*Rule, error) {
	rule, err := closed(raw, []string{"ruleVersion", "parameters", "clauses"}, []string{"derived"}, what+" rule")
	if err != nil {
		return nil, err
	}
	if rule["ruleVersion"] != RuleVersion {
		return nil, malformed("%s: the rule is not of ruleVersion %q", what, RuleVersion)
	}
	params, ok := rule["parameters"].(map[string]any)
	if !ok {
		return nil, malformed("%s: rule parameters are not an object", what)
	}
	declared := map[string]string{}
	for _, name := range sortedKeys(params) {
		typ, ok := params[name].(string)
		if !ok || !KnownType(typ) {
			return nil, malformed("%s: a rule parameter of another type", what)
		}
		if scope != nil && scope[name] != typ {
			return nil, malformed("%s: a rule parameter not in scope with its type", what)
		}
		declared[name] = typ
	}
	r := &Rule{Parameters: declared, builtins: map[string]string{}, candidates: map[string]map[string]bool{}}
	if d, ok := rule["derived"]; ok {
		if r.derived, r.builtins, err = checkDerived(d, declared, what); err != nil {
			return nil, err
		}
	}
	clauses, ok := rule["clauses"].([]any)
	if !ok || len(clauses) == 0 {
		return nil, malformed("%s: clauses are not a non-empty array", what)
	}
	if len(clauses) > maxClauses {
		return nil, failure(CodeOverBound, "%s: more than %d clauses", what, maxClauses)
	}
	facts, evidence := map[string]bool{}, map[string]bool{}
	for _, c := range clauses {
		clause, err := closed(c, []string{"when", "claim", "reason"}, nil, what+" clause")
		if err != nil {
			return nil, err
		}
		if err := checkCondition(clause["when"], declared, r.builtins, what); err != nil {
			return nil, err
		}
		if _, ok := clause["reason"].(string); !ok {
			return nil, malformed("%s: a reason that is not a string", what)
		}
		claim, err := closed(clause["claim"], []string{"facts", "evidence", "acquisitionStatus"}, nil, what+" claim")
		if err != nil {
			return nil, err
		}
		entries, ok := claim["facts"].([]any)
		if !ok {
			return nil, malformed("%s: claim facts are not an array", what)
		}
		for _, e := range entries {
			entry, err := closed(e, []string{"pointer"}, []string{"from", "value", "derived"}, what+" claim fact")
			if err != nil {
				return nil, err
			}
			if len(entry) != 2 {
				return nil, malformed("%s: a claim fact names exactly one of from, value and derived", what)
			}
			target, ok := entry["pointer"].(string)
			if !ok || target == "" || !isPointer(target, false) {
				return nil, malformed("%s: a fact target that is not a pointer other than the root", what)
			}
			if from, ok := entry["from"]; ok {
				if !isPointer(from, true) {
					return nil, malformed("%s: a from that is not a pointer", what)
				}
				if r.candidates[target] == nil {
					r.candidates[target] = map[string]bool{}
				}
				r.candidates[target][from.(string)] = true
			} else if literal, ok := entry["value"]; ok {
				switch x := literal.(type) {
				case string:
					if len(x) > MaxLiteralBytes {
						return nil, failure(CodeOverBound, "%s: a literal of more than %d bytes", what, MaxLiteralBytes)
					}
				case bool:
				default:
					if !IsInteger(literal) {
						return nil, malformed("%s: a literal that is not a string, a boolean or an integer", what)
					}
				}
			} else {
				name, ok := entry["derived"].(string)
				if _, declaredHere := r.builtins[name]; !ok || !declaredHere {
					return nil, malformed("%s: a claim fact names a derived value the rule does not declare", what)
				}
			}
			if !facts[target] {
				facts[target] = true
				r.FactTargets = append(r.FactTargets, target)
			}
		}
		ev, ok := claim["evidence"].(map[string]any)
		if !ok {
			return nil, malformed("%s: claim evidence is not an object", what)
		}
		for _, requirement := range sortedKeys(ev) {
			availability, _ := ev[requirement].(string)
			if requirement == "" || !contains(availabilities, availability) {
				return nil, malformed("%s: claim evidence of another form", what)
			}
			if !evidence[requirement] {
				evidence[requirement] = true
				r.EvidenceTargets = append(r.EvidenceTargets, requirement)
			}
		}
		status, _ := claim["acquisitionStatus"].(string)
		if !contains(statuses, status) {
			return nil, malformed("%s: an acquisitionStatus of another value", what)
		}
		r.clauses = append(r.clauses, clause)
	}
	last, _ := r.clauses[len(r.clauses)-1]["when"].(map[string]any)
	if len(last) != 1 || last["op"] != "always" {
		return nil, malformed("%s: the last clause's when is not always", what)
	}
	r.listed = inParamNames(r.clauses)
	return r, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Candidates are the pointers the rule's claims copy a fact target from,
// each once, in code point order: the from of every entry that names one.
func (r *Rule) Candidates(target string) []string { return sortedSet(r.candidates[target]) }

// ParametersNamed are the parameters the rule's conditions name, at any
// depth, and every parameter an operand of a derived value it declares
// names, whether that value is read or not (spec/dialect.md §2.2.6, §11).
func (r *Rule) ParametersNamed() []string {
	named := map[string]bool{}
	var walk func(any)
	walk = func(c any) {
		m := c.(map[string]any)
		switch m["op"] {
		case "equalsParam", "compareParam", "inParam":
			named[m["param"].(string)] = true
		case "freshWithin":
			named[m["asOf"].(string)], named[m["maxAge"].(string)] = true, true
		case "aheadWithin":
			named[m["asOf"].(string)], named[m["maxAhead"].(string)] = true, true
		case "all", "any":
			for _, x := range m["of"].([]any) {
				walk(x)
			}
		case "not":
			walk(m["of"])
		}
	}
	for _, c := range r.clauses {
		walk(c["when"])
	}
	operand := func(v any) {
		if m, ok := v.(map[string]any); ok {
			if name, ok := m["param"].(string); ok {
				named[name] = true
			}
		}
	}
	for _, d := range r.derived {
		switch d["builtin"] {
		case "duration":
			operand(d["from"])
			operand(d["to"])
		case "distance":
			for _, end := range []string{"from", "to"} {
				point := d[end].(map[string]any)
				operand(point["latitude"])
				operand(point["longitude"])
			}
		}
	}
	return sortedSet(named)
}

// Claim is a read's claim (spec/dialect.md §6.3), in the members a
// preparation writes it with.
type Claim struct {
	Facts    map[string]any    `json:"facts"`
	Evidence map[string]string `json:"evidenceAvailability"`
	Status   string            `json:"acquisitionStatus"`
	Reason   string            `json:"reason"`
	Basis    []string          `json:"basis"`
}

// Derivation is how a target of a rule of version 2.0.0 was written
// (spec/dialect.md §2.2.6): by a pointer, a literal or a built-in, or not
// written by the matching clause (none).
type Derivation struct {
	By         string
	From       string
	Builtin    string
	Derived    string
	Fields     []string
	Parameters []string
}

// MarshalJSON writes a derivation with exactly its kind's members.
func (d Derivation) MarshalJSON() ([]byte, error) {
	switch d.By {
	case "pointer":
		return json.Marshal(map[string]any{"by": d.By, "from": d.From})
	case "builtin":
		return json.Marshal(map[string]any{"by": d.By, "builtin": d.Builtin, "derived": d.Derived, "fields": nonNil(d.Fields), "parameters": nonNil(d.Parameters)})
	}
	return json.Marshal(map[string]any{"by": d.By})
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// None is the derivation of a target its read did not write.
var None = Derivation{By: "none"}

// Derivations are a rule's targets' derivations, by kind ("fact" or
// "evidence") and target. A target not listed was not written: None.
type Derivations map[[2]string]Derivation

// Of is target's derivation.
func (d Derivations) Of(kind, target string) Derivation {
	if x, ok := d[[2]string{kind, target}]; ok {
		return x
	}
	return None
}

func comparable(v any) (*big.Rat, bool) {
	switch {
	case IsInteger(v):
		return new(big.Rat).SetString(string(v.(json.Number)))
	case IsDecimal(v):
		return new(big.Rat).SetString(v.(string))
	}
	return nil, false
}

func (r *Rule) evaluate(c any, value any, params map[string]any, lists map[string]list, derived map[string]*derivedValue, basis map[string]bool) bool {
	m := c.(map[string]any)
	op := m["op"].(string)
	switch op {
	case "always":
		return true
	case "all":
		for _, x := range m["of"].([]any) {
			if !r.evaluate(x, value, params, lists, derived, basis) {
				return false
			}
		}
		return true
	case "any":
		for _, x := range m["of"].([]any) {
			if r.evaluate(x, value, params, lists, derived, basis) {
				return true
			}
		}
		return false
	case "not":
		return !r.evaluate(m["of"], value, params, lists, derived, basis)
	}
	var found any
	var present bool
	if name, ok := m["derived"].(string); ok {
		held := derived[name]
		for f := range held.fields {
			basis[f] = true
		}
		found, present = held.value, !held.none
	} else {
		field := m["field"].(string)
		basis[field] = true
		found, present = Resolve(value, field)
	}
	if !present {
		return false
	}
	switch op {
	case "exists":
		return true
	case "equals":
		return Equal(found, m["to"])
	case "equalsParam":
		return Equal(found, params[m["param"].(string)])
	case "isTrue":
		return found == true
	case "isDecimalString":
		s, ok := found.(string)
		return ok && unsignedDecimal(s)
	case "freshWithin", "aheadWithin":
		at, ok1 := Instant(found)
		asOf, ok2 := Instant(params[m["asOf"].(string)])
		if !ok1 || !ok2 {
			return false
		}
		if op == "freshWithin" {
			age := asOf - at
			return age >= 0 && age <= integerOf(params[m["maxAge"].(string)])
		}
		ahead := at - asOf
		return ahead >= 0 && ahead <= integerOf(params[m["maxAhead"].(string)])
	case "compareParam":
		left, ok := comparable(found)
		if !ok {
			return false
		}
		right, _ := comparable(params[m["param"].(string)])
		switch c := left.Cmp(right); m["relation"] {
		case "less-than":
			return c < 0
		case "less-than-or-equal":
			return c <= 0
		case "greater-than":
			return c > 0
		default:
			return c >= 0
		}
	case "inParam":
		name := m["param"].(string)
		held := lists[name]
		if r.Parameters[name] == TypeStringList {
			s, ok := found.(string)
			return ok && held.members[s]
		}
		a, ok := addressOf(found)
		if !ok {
			return false
		}
		for _, rg := range held.ranges {
			if rg.in(a) {
				return true
			}
		}
		return false
	}
	return false
}

// list is the value of a list parameter an inParam names, as inParam
// compares with it: a string-list's elements as a set, an
// address-range-list's as the ranges they read as. Apply reads each once
// for the case, so that no leaf reads its elements again.
type list struct {
	members map[string]bool
	ranges  []addressRange
}

// readLists reads the value each list parameter an inParam names takes in
// params.
// An element that is not of its list's type is not one of its elements, as
// it is not one inParam matches.
func (r *Rule) readLists(params map[string]any) map[string]list {
	out := make(map[string]list, len(r.listed))
	for _, name := range r.listed {
		elements, _ := params[name].([]any)
		if r.Parameters[name] == TypeStringList {
			members := make(map[string]bool, len(elements))
			for _, e := range elements {
				if s, ok := e.(string); ok {
					members[s] = true
				}
			}
			out[name] = list{members: members}
			continue
		}
		ranges := make([]addressRange, 0, len(elements))
		for _, e := range elements {
			if rg, ok := rangeOf(e); ok {
				ranges = append(ranges, rg)
			}
		}
		out[name] = list{ranges: ranges}
	}
	return out
}

// inParamNames are the parameters the inParam leaves of conditions name, at
// any depth, each once, in code point order.
func inParamNames(clauses []map[string]any) []string {
	named := map[string]bool{}
	var walk func(any)
	walk = func(c any) {
		m := c.(map[string]any)
		switch m["op"] {
		case "inParam":
			named[m["param"].(string)] = true
		case "all", "any":
			for _, x := range m["of"].([]any) {
				walk(x)
			}
		case "not":
			walk(m["of"])
		}
	}
	for _, c := range clauses {
		walk(c["when"])
	}
	return sortedSet(named)
}

// unsignedDecimal is isDecimalString's grammar: 0, or digits with no
// leading zero.
func unsignedDecimal(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Apply is the claim of the rule over value, its parameters taken from
// scope (spec/dialect.md §2.2.4), and each target's derivation (§2.2.6). A
// claim fact whose from does not resolve, whose value is outside the canon
// domain, or whose derived value has none fails the read, read-failed, even
// when a later entry writes the same pointer: every entry is applied, in
// order, and a later one's value and derivation win.
func (r *Rule) Apply(value any, scope map[string]any) (Claim, Derivations, error) {
	params := make(map[string]any, len(r.Parameters))
	for name := range r.Parameters {
		params[name] = scope[name]
	}
	derived := derive(r.derived, value, params)
	lists := r.readLists(params)
	basis := map[string]bool{}
	var match map[string]any
	for _, clause := range r.clauses {
		if r.evaluate(clause["when"], value, params, lists, derived, basis) {
			match = clause
			break
		}
	}
	claim := match["claim"].(map[string]any)
	facts := map[string]any{}
	derivations := Derivations{}
	for _, e := range claim["facts"].([]any) {
		entry := e.(map[string]any)
		pointer := entry["pointer"].(string)
		var found any
		var how Derivation
		switch {
		case entry["from"] != nil:
			from := entry["from"].(string)
			v, ok := Resolve(value, from)
			if !ok {
				return Claim{}, nil, failure(CodeReadFailed, "a claim fact's from does not resolve")
			}
			if !inCanonDomain(v) {
				return Claim{}, nil, failure(CodeReadFailed, "a claim fact's value is outside the canon domain")
			}
			found, how = v, Derivation{By: "pointer", From: from}
		case hasMember(entry, "value"):
			found, how = entry["value"], Derivation{By: "literal"}
		default:
			name := entry["derived"].(string)
			held := derived[name]
			if held.none {
				return Claim{}, nil, failure(CodeReadFailed, "a claim fact names a derived value that has none")
			}
			found = held.value
			how = Derivation{By: "builtin", Builtin: r.builtins[name], Derived: name, Fields: sortedSet(held.fields), Parameters: sortedSet(held.parameters)}
		}
		Write(facts, pointer, found)
		derivations[[2]string{"fact", pointer}] = how
	}
	evidence := map[string]string{}
	for requirement, availability := range claim["evidence"].(map[string]any) {
		evidence[requirement] = availability.(string)
		derivations[[2]string{"evidence", requirement}] = Derivation{By: "literal"}
	}
	return Claim{Facts: facts, Evidence: evidence, Status: claim["acquisitionStatus"].(string), Reason: match["reason"].(string), Basis: sortedSet(basis)}, derivations, nil
}

func hasMember(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
