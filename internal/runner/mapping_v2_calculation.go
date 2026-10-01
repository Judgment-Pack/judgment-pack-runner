package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A calculated source is an operation whose trusted profile names a calculator
// (docs/design/calculated-values.md). Its answer carries a calculation member;
// the mapping binds what that member echoes, and Runner holds the answer to the
// binding before any rule reads it.

// maxTableAge is the oldest a mapping may let a calculator's table be: ten years.
const maxTableAge = 315360000

const calculationComputed = "computed"

var calculationStatuses = map[string]bool{calculationComputed: true, "input-missing": true, "cannot-compute": true}

func validateCalculationBinding(b CalculationBinding, kind string, params map[string]string) error {
	if kind != "operation" {
		return errors.New("only an operation can be a calculated source")
	}
	if b.Inputs == nil || b.Tables == nil || len(b.Inputs) > 64 || len(b.Tables) > 64 {
		return errors.New("a calculated source binds its inputs and its tables, at most 64 of each")
	}
	for name, param := range b.Inputs {
		if !mappingName.MatchString(name) || param == "runAt" || params[param] == "" {
			return errors.New("each calculation input must be bound to a parameter of its source other than runAt")
		}
	}
	for name, age := range b.Tables {
		if !flatToken(name) || age < 1 || age > maxTableAge {
			return fmt.Errorf("each calculation table needs a maximum age from 1 through %d seconds", maxTableAge)
		}
	}
	return nil
}

// checkCalculation holds a calculator's answer to the source's binding. It
// returns the lineage member for the source's targets. Any failure is a failure
// of preparation: the answer is not about this case, or rests on stale tables.
func checkCalculation(source MappingSource, pin CalculatorPin, origins map[string]Dependency, artifact any, params map[string]any, at time.Time) (*CalculationLineage, error) {
	b := source.Calculation
	shape := errors.New("a calculator's answer must hold a calculation member of exactly calculator, status, inputs and asOf")
	root, ok := artifact.(map[string]any)
	if !ok {
		return nil, shape
	}
	c, ok := root["calculation"].(map[string]any)
	if !ok || len(c) != 4 {
		return nil, shape
	}
	calculator, ok1 := c["calculator"].(map[string]any)
	status, ok2 := c["status"].(string)
	inputs, ok3 := c["inputs"].(map[string]any)
	asOf, ok4 := c["asOf"].(map[string]any)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, shape
	}
	if len(calculator) != 2 || calculator["name"] != pin.Name || calculator["version"] != pin.Version {
		return nil, errors.New("the answer names another calculator or version than its trusted profile")
	}
	if !calculationStatuses[status] {
		return nil, errors.New("the calculation status is not computed, input-missing or cannot-compute")
	}
	computed := status == calculationComputed
	lineage := &CalculationLineage{Calculator: pin, Status: status, Inputs: []CalculationInput{}, AsOf: map[string]string{}}
	for _, name := range sortedKeys(inputs) {
		param, bound := b.Inputs[name]
		if !bound {
			return nil, errors.New("the calculator echoed an input the mapping does not bind")
		}
		got, e1 := proofCanon(encode(inputs[name]))
		want, e2 := proofCanon(encode(params[param]))
		if e1 != nil || e2 != nil || !bytes.Equal(got, want) {
			return nil, fmt.Errorf("calculation input %s does not equal its parameter %s", name, param)
		}
		from := origins[param]
		lineage.Inputs = append(lineage.Inputs, CalculationInput{name, param, from.Source, from.Pointer})
	}
	if computed && len(inputs) != len(b.Inputs) {
		return nil, errors.New("a computed answer must echo every input the mapping binds")
	}
	for _, name := range sortedKeys(asOf) {
		limit, named := b.Tables[name]
		if !named {
			return nil, errors.New("the calculator reported a table the mapping does not name")
		}
		instant, _ := asOf[name].(string)
		if !paramValue(instant, "timestamp") {
			return nil, fmt.Errorf("calculation table %s has no as-of instant", name)
		}
		if computed {
			t, _ := time.Parse("2006-01-02T15:04:05Z", instant)
			if t.After(at.Add(30*time.Second)) || t.Before(at.Add(-time.Duration(limit)*time.Second)) {
				return nil, fmt.Errorf("calculation table %s is older than its limit or from the future", name)
			}
		}
		lineage.AsOf[name] = instant
	}
	if computed && len(asOf) != len(b.Tables) {
		return nil, errors.New("a computed answer must report every table the mapping names")
	}
	return lineage, nil
}

// calculationUnknown is the claim of a calculator that answered it could not
// compute: no facts, each mapped evidence requirement unknown, under a reason
// of Runner's, read from the status alone. The source's rule is not applied.
func calculationUnknown(read SourceRead, status string) (derivedClaim, error) {
	_, evidence, err := readTargets(read)
	if err != nil {
		return derivedClaim{}, err
	}
	ev := map[string]string{}
	for _, e := range evidence {
		ev[e.Requirement] = "unknown"
	}
	return derivedClaim{json.RawMessage(`{}`), ev, "unknown", "calculation-" + status, []string{"/calculation/status"}}, nil
}

// parameterOrigins says where each parameter a source can bind came from: the
// case and its pointer, or an earlier source and the pointer of its derived fact.
func parameterOrigins(c *CaseMapping, source MappingSource) map[string]Dependency {
	origins := map[string]Dependency{}
	if c != nil {
		for n, p := range c.Parameters {
			origins[n] = Dependency{n, "case", p.Pointer}
		}
	}
	for n, p := range source.Parameters {
		origins[n] = Dependency{n, p.From, p.Pointer}
	}
	return origins
}
