package dialect4

import (
	"strconv"
	"strings"
	"testing"
)

// Both readers count a rule before its form (RuleCounts), so a bound
// CheckRule holds is not reached through them; CheckRule holds the bounds
// of §2.2.7 itself, for a caller that reads a rule with no count first.
func TestCheckRuleHoldsItsOwnBounds(t *testing.T) {
	always := `{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"resolved"},"reason":"r"}`
	exists := `{"op":"exists","field":"/x"}`
	rule := func(derived int, literal string, clauses, listed int) string {
		var d []string
		for i := 0; i < derived; i++ {
			d = append(d, `{"name":"d`+strconv.Itoa(i)+`","builtin":"duration","from":{"field":"/a"},"to":{"field":"/b"}}`)
		}
		c := []string{`{"when":{"op":"all","of":[` + strings.TrimSuffix(strings.Repeat(exists+",", listed), ",") + `]},` +
			`"claim":{"facts":[{"pointer":"/x","value":"` + literal + `"}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"r"}`}
		for len(c) < clauses {
			c = append(c, always)
		}
		return `{"ruleVersion":"2.0.0","parameters":{},"derived":[` + strings.Join(d, ",") + `],"clauses":[` + strings.Join(c, ",") + `]}`
	}
	for _, c := range []struct {
		name string
		rule string
		want string
	}{
		{"16 derived values", rule(16, "a", 2, 1), ""},
		{"17 derived values", rule(17, "a", 2, 1), CodeOverBound},
		{"a literal of 8192 bytes", rule(1, strings.Repeat("é", 4096), 2, 1), ""},
		{"a literal of 8193 bytes", rule(1, strings.Repeat("é", 4096)+"a", 2, 1), CodeOverBound},
		{"64 clauses", rule(1, "a", 64, 1), ""},
		{"65 clauses", rule(1, "a", 65, 1), CodeOverBound},
		{"64 conditions listed", rule(1, "a", 2, 64), ""},
		{"65 conditions listed", rule(1, "a", 2, 65), CodeOverBound},
	} {
		v, err := ReadJSON([]byte(c.rule), 64)
		if err != nil {
			t.Fatal(c.name, err)
		}
		_, err = CheckRule(v, nil, "rule")
		if got := CodeOf(err); got != c.want || c.want == "" && err != nil {
			t.Errorf("%s: %q (%v), want %q", c.name, got, err, c.want)
		}
	}
}

// inParam reads its parameter's elements once per case, not once per leaf:
// 45 conditions of 63 inParam leaves each, over one address-range-list of
// 1024 ranges, read 1024 ranges for a case however many leaves are
// evaluated, where reading them per leaf would read 2903040.
func TestInParamReadsEachListOncePerCase(t *testing.T) {
	const groups, leaves, ranges = 45, 63, MaxListElements
	leaf := `{"op":"inParam","field":"/address","param":"ranges"}`
	var clauses []string
	for g := 0; g < groups; g++ {
		clauses = append(clauses, `{"when":{"op":"any","of":[`+strings.TrimSuffix(strings.Repeat(leaf+",", leaves), ",")+`]},`+
			`"claim":{"facts":[{"pointer":"/group","value":`+strconv.Itoa(g)+`}],"evidence":{},"acquisitionStatus":"resolved"},"reason":"in"}`)
	}
	clauses = append(clauses, `{"when":{"op":"always"},"claim":{"facts":[],"evidence":{},"acquisitionStatus":"resolved"},"reason":"out"}`)
	raw, err := ReadJSON([]byte(`{"ruleVersion":"2.0.0","parameters":{"ranges":"address-range-list"},"clauses":[`+strings.Join(clauses, ",")+`]}`), 64)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := CheckRule(raw, nil, "rule")
	if err != nil {
		t.Fatal(err)
	}
	list := make([]any, ranges)
	for i := range list {
		list[i] = "10." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256) + ".0/24"
	}
	scope := map[string]any{"ranges": list}
	for _, c := range []struct {
		address, reason string
	}{
		{"198.51.100.7", "out"}, // in no range: every leaf is evaluated
		{"10.3.255.9", "in"},    // in the last range
		{"10.0.0.1", "in"},      // in the first
	} {
		before := rangeReads.Load()
		claim, _, err := rule.Apply(map[string]any{"address": c.address}, scope)
		if err != nil {
			t.Fatal(err)
		}
		if read := rangeReads.Load() - before; read != ranges {
			t.Errorf("%s: %d address ranges read for one case, want %d: one read of each element", c.address, read, ranges)
		}
		if claim.Reason != c.reason {
			t.Errorf("%s: reason %q, want %q", c.address, claim.Reason, c.reason)
		}
	}
}
