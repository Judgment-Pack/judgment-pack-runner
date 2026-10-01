package main

import (
	"errors"
	"fmt"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// unsignedTargets counts the targets of the sources whose rules read an
// unsigned parameter the operator supplied (from the case or a local file), and
// of those whose rules read runAt, the export's own verification time. A source
// whose rule reads both counts in both.
func unsignedTargets(unsigned []runner.UnsignedParameters) (operator, clock int) {
	for _, u := range unsigned {
		fromOperator, fromClock := false, false
		for _, p := range u.Parameters {
			fromOperator = fromOperator || p.Operator()
			fromClock = fromClock || !p.Operator()
		}
		if fromOperator {
			operator += u.Targets
		}
		if fromClock {
			clock += u.Targets
		}
	}
	return operator, clock
}

// ofTheInputs says how many of the run's inputs, with the verb agreeing.
func ofTheInputs(n, total int) string {
	switch {
	case n == total:
		return "All of the run's inputs were"
	case n == 1:
		return fmt.Sprintf("1 of the run's %d inputs was", total)
	}
	return fmt.Sprintf("%d of the run's %d inputs were", n, total)
}

// unsignedSentence is what the line on standard error adds when an acquired
// source's rule reads a parameter that no signed request commits.
func unsignedSentence(c runner.InputClasses, unsigned []runner.UnsignedParameters) string {
	total := c.Asserted + c.Record + c.Generated
	operator, clock := unsignedTargets(unsigned)
	s := ""
	if operator > 0 {
		s += " " + ofTheInputs(operator, total) + " derived by a rule that reads a parameter from the case or a local file, which no signed request commits: nothing here checks that parameter against a source."
	}
	if clock > 0 {
		s += " " + ofTheInputs(clock, total) + " derived by a rule that reads runAt, which rests on the export's own verification time."
	}
	return s
}

// refuseUnsourced is --require-sourced's check, and the status of its refusal:
// an asserted target first, then a target derived by a rule that reads an
// unsigned parameter the operator supplied.
func refuseUnsourced(c runner.InputClasses, unsigned []runner.UnsignedParameters) (string, error) {
	if err := requireSourcedInputs(c); err != nil {
		return "inputs-asserted", err
	}
	if err := requireSignedParameters(c, unsigned); err != nil {
		return "parameters-unsigned", err
	}
	return "", nil
}

// errParametersUnsigned is --require-sourced's refusal of a run with a target of
// an acquired source whose rule reads a parameter from the case or a local file
// that no signed request commits. runAt alone is reported and not
// refused: every freshness rule reads it.
var errParametersUnsigned = errors.New("parameters-unsigned")

func requireSignedParameters(c runner.InputClasses, unsigned []runner.UnsignedParameters) error {
	total := c.Asserted + c.Record + c.Generated
	operator, _ := unsignedTargets(unsigned)
	if operator == 0 {
		return nil
	}
	them := "them"
	if operator == 1 && total > 1 {
		them = "it"
	}
	return fmt.Errorf("%w: %s derived by a rule that reads a parameter from the case or a local file, which no signed request commits, and --require-sourced refuses %s: nothing here checks that parameter against a source", errParametersUnsigned, ofTheInputs(operator, total), them)
}
