// jpack-runner is a local, single-owner companion. Boot authority is delivered
// over stdin, never project files, URLs, arguments, or browser-provided config.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/buildinfo"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"
)

func main() {
	err := error(nil)
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("jpack-runner", buildinfo.Version())
	} else if len(os.Args) > 1 && os.Args[1] == "verify-run" {
		err = verifyRun(os.Args[2:], os.Stdout, os.Stderr)
	} else {
		err = serve()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}
func serve() error {
	input := bufio.NewReaderSize(os.Stdin, 128<<10)
	line, err := input.ReadSlice('\n')
	if err != nil || len(line) > 128<<10 {
		return fmt.Errorf("bounded boot configuration required")
	}
	var boot struct {
		Dir                string                     `json:"dir"`
		Runtime            string                     `json:"runtime"`
		Workspace          string                     `json:"workspace"`
		Owner              string                     `json:"owner"`
		Token              string                     `json:"token"`
		CloudConnections   []runner.CloudConnection   `json:"cloudConnections,omitempty"`
		GatewayConnections []runner.GatewayConnection `json:"gatewayConnections,omitempty"`
		InputRoot          string                     `json:"inputRoot,omitempty"`
		InputProfiles      []runner.InputProfile      `json:"inputProfiles,omitempty"`
		RequireTested      bool                       `json:"requireTestedReleases,omitempty"`
	}
	if json.Unmarshal([]byte(line), &boot) != nil || len(boot.Token) < 32 {
		return fmt.Errorf("invalid boot configuration")
	}
	service, err := runner.Open(runner.Config{Dir: boot.Dir, Runtime: boot.Runtime, Workspace: boot.Workspace, Owner: boot.Owner, InputProfiles: boot.InputProfiles, InputRoot: boot.InputRoot, CloudConnections: boot.CloudConnections, GatewayConnections: boot.GatewayConnections, RequireTestedReleases: boot.RequireTested})
	if err != nil {
		return err
	}
	defer service.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	// A release check can run validation, lock, rehearsal and saved tests, each
	// with a 30-second subprocess deadline. Allow the bounded sequence to finish.
	server := &http.Server{Handler: service.Handler(boot.Token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second, WriteTimeout: 135 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String(), "protocol": "jobs/1"}); err != nil {
		return err
	}
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, input); close(closed) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()
	select {
	case <-closed:
	case <-ctx.Done():
	case err = <-failed:
		if err != http.ErrServerClosed {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	server.Shutdown(shutdown)
	return nil
}

func verifyRun(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-run", flag.ContinueOnError)
	file := flags.String("file", "", "retained verification export")
	profilesPath := flags.String("profiles", "", "independently trusted input profiles JSON")
	release := flags.String("release-digest", "", "independently trusted frozen release digest")
	runtime := flags.String("runtime", "", "optional: the release's Runtime executable, to evaluate the verified inputs again and compare the disposition")
	requireSourced := flags.Bool("require-sourced", false, "optional: refuse a run any of whose fact or evidence targets is asserted (typed into the case or read from a local file), or derived by a rule that reads a parameter its source's receipt does not commit and that rests on the operator's say (runAt is exempt)")
	chainFile := flags.String("chain", "", "optional: the installation's chain of runs, as GET /v1/run-chain serves it, to check a version-4 export's entry along it")
	var expect expectFlags
	flags.Var(&expect, "expect", "optional, repeatable: a file of checkpoints held independently of the operator, one per line, that the run's entry and the chain must match")
	requireWitnessed := flags.Bool("require-witnessed", false, "optional: refuse a run whose entry in the installation's chain of runs no held checkpoint covers")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || *profilesPath == "" || *release == "" || flags.NArg() != 0 {
		return fmt.Errorf("file, profiles and release-digest are required")
	}
	// A version-3 or version-4 export may exceed 8 MiB by the record's bytes
	// and the chain entry it carries.
	raw, err := readLimit(*file, runner.MaxExportSize)
	if err != nil {
		return err
	}
	p, err := readLimit(*profilesPath, 8<<20)
	if err != nil {
		return err
	}
	profiles, err := runner.ParseInputProfiles(p)
	if err != nil {
		return err
	}
	verified, err := runner.VerifyInputs(raw, profiles, *release)
	if err != nil {
		return err
	}
	classes := verified.Classes
	record := recordReportOf(verified)
	// A version-4 export's entry in the installation's chain of runs is checked
	// as far as the verifier can: alone, along a supplied chain, and against
	// checkpoints held independently. A failure, or --require-witnessed, refuses
	// the run before anything else is reported, and a re-execution does not run.
	chain, err := checkChain(verified, *chainFile, expect, *requireWitnessed)
	if err != nil {
		return err
	}
	if status, err := chainRefusal(chain, *requireWitnessed); err != nil {
		json.NewEncoder(stdout).Encode(verifyReport{recordReport: record, RunChain: chain, Status: status, RetainedDisposition: "not-checked", Scope: inputsScope, TargetsByClass: classes, UnsignedParameters: verified.Unsigned})
		return err
	}
	// An asserted target, and a parameter that its source's receipt does not
	// commit, are checked against nothing but the export, which an operator can
	// rewrite consistently. The refusal comes before a re-execution,
	// which then does not run.
	if *requireSourced {
		if status, err := refuseUnsourced(classes, verified.Unsigned); err != nil {
			json.NewEncoder(stdout).Encode(verifyReport{recordReport: record, RunChain: chain, Status: status, RetainedDisposition: "not-checked", Scope: inputsScope, TargetsByClass: classes, UnsignedParameters: verified.Unsigned})
			return err
		}
	}
	if *runtime == "" {
		// verified-inputs covers the inputs, not the result. Nothing here compares the
		// retained disposition with an evaluation, and both outputs say so.
		fmt.Fprintln(stderr, "verified-inputs: the run's inputs match its record. Its disposition was not checked: this does not say the run decided what its record says."+assertedSentence(classes)+unsignedSentence(classes, verified.Unsigned)+record.sentence()+chainSentence(chain))
		return json.NewEncoder(stdout).Encode(verifyReport{recordReport: record, RunChain: chain, Status: "verified-inputs", RetainedDisposition: "not-checked", Scope: inputsScope, TargetsByClass: classes, UnsignedParameters: verified.Unsigned})
	}
	// The release's own Runtime evaluates the verified inputs again, as a
	// rehearsal. A different disposition is a failure with a report of its own.
	scope := "retained input derivation, audit binding, and the retained disposition against a re-execution by the release's Runtime; not sealed-session completeness or policy truth"
	err = verified.Disposition(context.Background(), *runtime)
	if errors.Is(err, runner.ErrDispositionDiffers) {
		json.NewEncoder(stdout).Encode(verifyReport{recordReport: record, RunChain: chain, Status: "disposition-differs", RetainedDisposition: "differs-from-re-execution", Scope: scope, TargetsByClass: classes, UnsignedParameters: verified.Unsigned})
		return err
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true."+assertedSentence(classes)+unsignedSentence(classes, verified.Unsigned)+record.sentence()+chainSentence(chain))
	return json.NewEncoder(stdout).Encode(verifyReport{recordReport: record, RunChain: chain, Status: "verified-disposition", RetainedDisposition: "matches-re-execution", Scope: scope, TargetsByClass: classes, UnsignedParameters: verified.Unsigned})
}

// readLimit reads a verification input of at most limit bytes.
func readLimit(path string, limit int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(b) > limit {
		return nil, fmt.Errorf("verification input exceeds limit")
	}
	return b, e
}

const inputsScope = "retained input derivation and audit binding; not sealed-session completeness or policy truth"

// verifyReport is verify-run's report on standard output. Its members encode
// sorted by name, as they did when the report was a map; the counts sort last.
type verifyReport struct {
	recordReport
	RetainedDisposition string `json:"retainedDisposition"`
	// Present only for a version-4 export: what checking the run's entry in
	// the installation's chain of runs found. Versions 2 and 3 carry no entry,
	// and their report is what it was before the chain existed.
	RunChain       *runner.RunChainReport `json:"runChain,omitempty"`
	Scope          string                 `json:"scope"`
	Status         string                 `json:"status"`
	TargetsByClass runner.InputClasses    `json:"targetsByClass"`
	// Present only when an acquired source's rule reads a parameter that its
	// receipt does not commit.
	UnsignedParameters []runner.UnsignedParameters `json:"unsignedParameters,omitempty"`
}

// recordReport is what the report says of the audit record's bytes, by which a
// digest of the record is taken. A version-3 or version-4 export carries bytes,
// which parse to the record it exports; their SHA-256 is the digest a gateway
// action receipt would name for those bytes. That they are the bytes the
// Runtime wrote for this run is not shown without such a digest held
// independently. A version-2 export carries none, and nothing about them could
// be checked.
type recordReport struct {
	ExactBytes    string `json:"exactBytes"`
	ExportVersion int    `json:"exportVersion"`
	RecordDigest  string `json:"recordDigest,omitempty"`
}

func recordReportOf(v runner.VerifiedRun) recordReport {
	if v.ExportVersion() >= 3 {
		return recordReport{ExactBytes: "matches-record", ExportVersion: v.ExportVersion(), RecordDigest: v.RecordDigest()}
	}
	return recordReport{ExactBytes: "not-in-export", ExportVersion: v.ExportVersion()}
}

// sentence is what the line on standard error says of the bytes.
func (r recordReport) sentence() string {
	if r.ExportVersion < 3 {
		return " Exact-byte checks were not possible: a version-2 export carries no original bytes of the audit record, so nothing here gives the digest a gateway receipt names it by."
	}
	return " The export's bytes of the audit record parse to the record. Their SHA-256 is " + r.RecordDigest + ": a digest held independently, such as a gateway receipt's, shows whether they are the bytes the Runtime wrote for this run."
}

// assertedSentence is what the line on standard error adds when a run's fact or
// evidence targets were asserted: all of them, or how many. It adds nothing
// when none was.
func assertedSentence(c runner.InputClasses) string {
	total := c.Asserted + c.Record + c.Generated
	switch {
	case c.Asserted == 0:
		return ""
	case c.Asserted == total:
		return " The run's inputs are the operator's own: nothing here checks them against a source."
	case c.Asserted == 1:
		return fmt.Sprintf(" 1 of the run's %d inputs is the operator's own: nothing here checks it against a source.", total)
	}
	return fmt.Sprintf(" %d of the run's %d inputs are the operator's own: nothing here checks those against a source.", c.Asserted, total)
}

// errInputsAsserted is --require-sourced's refusal of a run with an asserted
// fact or evidence target, with a value or without. A record or generated
// target whose source was skipped has no value, not an asserted one, and is
// not refused; nor is a parameter, which is not a target.
var errInputsAsserted = errors.New("inputs-asserted")

func requireSourcedInputs(c runner.InputClasses) error {
	total := c.Asserted + c.Record + c.Generated
	switch {
	case c.Asserted == 0:
		return nil
	case c.Asserted == total:
		return fmt.Errorf("%w: the run's inputs are all asserted, %d of %d, and --require-sourced refuses them: nothing here checks them against a source", errInputsAsserted, c.Asserted, total)
	case c.Asserted == 1:
		return fmt.Errorf("%w: 1 of the run's %d inputs is asserted, and --require-sourced refuses it: nothing here checks it against a source", errInputsAsserted, total)
	}
	return fmt.Errorf("%w: %d of the run's %d inputs are asserted, and --require-sourced refuses them: nothing here checks them against a source", errInputsAsserted, c.Asserted, total)
}
