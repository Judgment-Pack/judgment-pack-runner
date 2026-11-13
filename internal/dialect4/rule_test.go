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
