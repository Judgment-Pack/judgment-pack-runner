package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// expectFlags is --expect, which may be given more than once: each a file of
// checkpoints a holder kept, one per line.
type expectFlags []string

func (e *expectFlags) String() string { return strings.Join(*e, ",") }
func (e *expectFlags) Set(path string) error {
	*e = append(*e, path)
	return nil
}

// errUnwitnessed is --require-witnessed's refusal of a run whose entry no held
// checkpoint covers.
var errUnwitnessed = errors.New("unwitnessed")

// errChainInvalid is a chain check that failed: the entry, the supplied chain
// or a held checkpoint does not agree with the rest.
var errChainInvalid = errors.New("chain-invalid")

// checkChain checks a verified run's entry in the installation's chain of
// runs, along the chain at chainPath when one is given and against the
// checkpoints in the files expect names. It answers nil for an export that
// carries no entry, unless a flag asks for a chain check, which such an export
// cannot have and is refused.
func checkChain(v runner.VerifiedRun, chainPath string, expect []string, requireWitnessed bool) (*runner.RunChainReport, error) {
	if _, _, ok := v.ChainEntry(); !ok {
		if chainPath != "" || len(expect) > 0 || requireWitnessed {
			return nil, fmt.Errorf("%w: it is version %d. A run recorded before Runner chained its runs has no entry and is unchained, and an export asked for as version 2 or 3 carries none; --chain, --expect and --require-witnessed check a version-4 or version-5 export", runner.ErrNoChainEntry, v.ExportVersion())
		}
		return nil, nil
	}
	var held []runner.Checkpoint
	for _, path := range expect {
		raw, err := readLimit(path, runner.MaxHeldCheckpoints)
		if err != nil {
			return nil, err
		}
		checkpoints, err := runner.ParseCheckpoints(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		held = append(held, checkpoints...)
	}
	var chain io.Reader
	if chainPath != "" {
		f, err := os.Open(chainPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		chain = f
	}
	report, err := v.CheckRunChain(chain, held)
	if err != nil {
		return nil, err
	}
	return &report, nil
}

// chainRefusal is the error a failed chain check, or --require-witnessed,
// answers, with the status its report carries; nil when there is none.
func chainRefusal(c *runner.RunChainReport, requireWitnessed bool) (string, error) {
	switch {
	case c == nil:
		return "", nil
	case c.Status != "valid":
		f := c.Findings[0]
		more := ""
		if c.FindingsTotal > 1 {
			more = fmt.Sprintf(" (and %d more)", c.FindingsTotal-1)
		}
		return "chain-invalid", fmt.Errorf("%w: %s at line %d: %s%s", errChainInvalid, f.Name, f.Line, f.Detail, more)
	case requireWitnessed && !c.Witnessed:
		return "unwitnessed", fmt.Errorf("%w: no held checkpoint covers the run's entry, at sequence %d, and --require-witnessed refuses it: nothing here establishes the entry against the operator, who keeps the chain", errUnwitnessed, c.Checkpoint.Sequence)
	}
	return "", nil
}

// chainSentence is what the line on standard error says of the run's entry in
// the installation's chain of runs, after what it says of the record's bytes.
// It adds nothing for an export that carries no entry.
func chainSentence(c *runner.RunChainReport) string {
	if c == nil {
		return ""
	}
	said := fmt.Sprintf(" The export's entry in the installation's chain of runs, at sequence %d, binds this run to those bytes", c.Checkpoint.Sequence)
	switch {
	case c.Witnessed:
		return said + fmt.Sprintf(", and the held checkpoints, through sequence %d, cover it: it is the entry that existed when they were made, if they were held independently of the operator. Nothing here shows when that was, or that the chain is complete after sequence %d.", c.Held.Through, c.Held.Through)
	case c.Scope == runner.ScopeCheckpoint:
		return said + fmt.Sprintf(", and the supplied chain agrees with the held checkpoints, through sequence %d. None of them covers this run's entry: it is unwitnessed, and nothing here establishes it against the operator, who keeps the chain.", c.Held.Through)
	case c.Scope == runner.ScopeOneSuppliedChain:
		return said + fmt.Sprintf(", and the supplied chain of %s is consistent with it. That is the integrity of one supplied chain, which establishes nothing against the operator, who keeps the chain and could have rewritten it with its links recomputed: a checkpoint held independently, supplied with --expect, would.", lines(c.Chain.Lines))
	}
	return said + ". Only that one supplied entry was checked, which establishes nothing against the operator, who keeps the chain and could have written the entry with the export: a checkpoint held independently, supplied with --expect, would. The report gives the entry's checkpoint, for a holder to keep from now on."
}

// lines is a count of a chain's lines, in words.
func lines(n int64) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}
