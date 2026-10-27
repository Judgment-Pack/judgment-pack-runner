// jpack-runner is a local, single-owner companion. Boot authority is delivered
// over stdin, never project files, URLs, arguments, or browser-provided config.
package main

import (
	"bufio"
	"bytes"
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
	os.Exit(runnerCommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// runnerCommand resolves commands before reading boot authority. Arguments that
// do not name a command must never fall through to companion startup.
func runnerCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var err error
	switch {
	case len(args) == 0:
		err = serve(stdin, stdout, stderr)
	case args[0] == "capabilities":
		err = printCapabilities(args[1:], stdout)
	case args[0] == "version":
		if len(args) != 1 {
			err = fmt.Errorf("version takes no arguments")
		} else {
			_, err = fmt.Fprintln(stdout, "jpack-runner", buildinfo.Version())
		}
	case args[0] == "prepare-manual-store":
		if len(args) != 1 {
			err = fmt.Errorf("prepare-manual-store takes one trusted JSON request on stdin")
		} else {
			return prepareManualStore(stdin, stdout)
		}
	case args[0] == "relocate-workspace":
		if len(args) == 2 && args[1] == "--capabilities" {
			_, err = fmt.Fprintln(stdout, `{"version":1}`)
		} else if len(args) != 1 {
			err = fmt.Errorf("relocate-workspace takes one reviewed JSON request on stdin")
		} else {
			err = relocateWorkspace(stdin, stdout)
		}
	case args[0] == "verify-run":
		err = verifyRun(args[1:], stdout, stderr)
	case args[0] == "verify-preparation":
		// Its exit status is the report's (spec/dialect.md §11), and it says
		// why on standard error itself.
		return verifyPreparation(args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("unknown runner command")
	}
	if err != nil {
		fmt.Fprintln(stderr, "runner:", err)
		return 1
	}
	return 0
}

func prepareManualStore(stdin io.Reader, stdout io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 16385))
	result := runner.ManualStorePrepareResult{Version: 1, Status: "refused", Code: "invalid-request"}
	if err == nil && len(raw) > 1 && len(raw) <= 16384 && raw[len(raw)-1] == '\n' && bytes.Count(raw, []byte{'\n'}) == 1 {
		runner.RunManualStorePreparationProcess(raw[:len(raw)-1], stdout)
		return 1 // the process-only adapter always exits
	}
	if json.NewEncoder(stdout).Encode(result) != nil || result.Status != "ok" {
		return 1
	}
	return 0
}

// bootConfig reads the boot line: the service's configuration and the
// bearer.
func bootConfig(line []byte) (runner.Config, string, error) {
	if err := checkBootCapabilities(line); err != nil {
		return runner.Config{}, "", err
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
		SigningKey         string                     `json:"signingKey,omitempty"`
		SigningPublicKey   string                     `json:"signingPublicKey,omitempty"`
	}
	if json.Unmarshal(line, &boot) != nil || len(boot.Token) < 32 {
		return runner.Config{}, "", fmt.Errorf("invalid boot configuration")
	}
	ownership, err := bootExecutorOwnership(line, boot.Workspace)
	if err != nil {
		return runner.Config{}, "", err
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal(line, &envelope)
	var admission *runner.ManualAdmissionBinding
	if raw, present := envelope["executorAdmission"]; present {
		admission, err = runner.ParseManualAdmissionBinding(raw, ownership, boot.Workspace)
		if err != nil {
			return runner.Config{}, "", fmt.Errorf("invalid manual admission binding")
		}
	}
	return runner.Config{ExecutorAdmission: admission, ExecutorOwnership: ownership, Dir: boot.Dir, Runtime: boot.Runtime, Workspace: boot.Workspace, Owner: boot.Owner, InputProfiles: boot.InputProfiles, InputRoot: boot.InputRoot, CloudConnections: boot.CloudConnections, GatewayConnections: boot.GatewayConnections, RequireTestedReleases: boot.RequireTested, SigningKey: boot.SigningKey, SigningPublicKey: boot.SigningPublicKey}, boot.Token, nil
}

func serve(stdin io.Reader, stdout, stderr io.Writer) error {
	input := bufio.NewReaderSize(stdin, 128<<10)
	line, err := input.ReadSlice('\n')
	if err != nil || len(line) > 128<<10 {
		return fmt.Errorf("bounded boot configuration required")
	}
	cfg, token, err := bootConfig(line)
	if err != nil {
		return err
	}
	service, err := runner.Open(cfg)
	if err != nil {
		return err
	}
	defer service.Close()
	// Each registered profile set aside at open is named, with its reason,
	// on standard error; standard output carries the protocol alone.
	for _, p := range service.SetAside() {
		fmt.Fprintln(stderr, "runner:", p)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	// A release check can run validation, lock, rehearsal and saved tests, each
	// with a 30-second subprocess deadline. Allow the bounded sequence to finish.
	server := &http.Server{Handler: service.Handler(token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 40 * time.Second, WriteTimeout: 135 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	defer server.Close()
	if err = json.NewEncoder(stdout).Encode(map[string]string{"url": "http://" + listener.Addr().String(), "protocol": "jobs/1"}); err != nil {
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

const verificationCoverage = "Every retained result disposition must match the audit record, independently of --runtime; a " +
	"difference fails with audit-disposition-mismatch. " +
	"The preparation is verified independently of the disposition, which it does not read: beside the " +
	"finding the report carries what was checked of the export (targetsByClass, unsignedFreshness, " +
	"unsignedParameters, binding, runChain, signature), standard error says so, and a --require flag the " +
	"export fails is added to the error; if the preparation fails its own verification, the report is the " +
	"finding alone and the error carries both refusals. A refusal before the preparation is verified (the " +
	"export's envelope, the release's digest, the record's bytes, chain entry or signature sidecar) " +
	"reports alone. " +
	"The record signature covers the exact audit bytes, " +
	"not the export envelope: version, releaseDigest, release, inputs, chain (entry and checkpoint), " +
	"run.trigger, run.schemaVersion, run.id, run.jobId, run.releaseId, run.revision, run.state, run.input " +
	"(including source and preparation.verifiedAt), run.createdAt, run.startedAt, run.finishedAt, " +
	"run.requestedBy, run.attempt, run.problem, or run.result members other than disposition " +
	"(outputVersion, tool, command, status, experimental, reviewed, reviewedSet, " +
	"conformanceClaimReference, specVersion, evaluatorSpecVersion, packId, packVersion, trace, artifact, " +
	"and any additional members) are not signed by it. run.audit is checked against run.auditBytes; " +
	"run.auditSignatures carries the signature, not a signature over itself. A record's binding, which " +
	"Runtime 0.31.0 and later record for a mapping-v2 run, is under that signature: the SHA-256 of " +
	"run.input.preparation, preparation.verifiedAt included, which verify-run recomputes from the export " +
	"and refuses on a mismatch; nothing else of run.input is bound by it. The operator, who holds the key, " +
	"can recompute a preparation and sign its record again with the new binding, which only --require-witnessed, " +
	"against a checkpoint held independently, shows. Separate release-digest, " +
	"input, chain and checkpoint checks bind only what their documented scope states."

func verifyRun(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: jpack-runner verify-run [flags]")
		fmt.Fprintln(stderr, verificationCoverage)
		flags.PrintDefaults()
	}
	conversionText := flags.String("conversion-contract", "", "optional: exact installed retained-column selector in canonical base64url")
	file := flags.String("file", "", "retained verification export")
	profilesPath := flags.String("profiles", "", "independently trusted input profiles JSON")
	release := flags.String("release-digest", "", "independently trusted frozen release digest")
	runtime := flags.String("runtime", "", "optional: the release's Runtime executable, to evaluate the verified inputs again and compare the disposition")
	requireSourced := flags.Bool("require-sourced", false, "optional: refuse a run any of whose fact or evidence targets is asserted (typed into the case or read from a local file), or derived by a rule that reads a parameter its source's receipt does not commit and that rests on the operator's say (runAt is exempt)")
	chainFile := flags.String("chain", "", "optional: the installation's chain of runs, as GET /v1/run-chain serves it, to check a version-4 or version-5 export's entry along it")
	var expect expectFlags
	flags.Var(&expect, "expect", "optional, repeatable: a file of checkpoints held independently of the operator, one per line, that the run's entry and the chain must match")
	requireWitnessed := flags.Bool("require-witnessed", false, "optional: refuse a run whose entry in the installation's chain of runs no held checkpoint covers")
	publicKeyText := flags.String("public-key", "", "optional: the public key of the installation's signing key, 64 lowercase hexadecimal characters as `jpack audit key public` prints it, to check a version-5 export's record signature under; a key that is not the canonical encoding of a point of the curve whose order does not divide 8 is refused")
	requireSigned := flags.Bool("require-signed", false, "optional: refuse a run whose audit record no valid signature under --public-key covers")
	requireBound := flags.Bool("require-bound", false, "optional: refuse a run whose audit record carries no binding of its preparation: one recorded by a Runtime before 0.31.0, or before Runner passed one, and a run without lineage, which has no preparation. A binding that is not the export's preparation's, or is of another scheme, is refused without it. A binding is under the record's signature, and the operator, who holds the key, can recompute a preparation and sign its record again: only --require-witnessed, against a checkpoint held independently, shows that")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || *profilesPath == "" || *release == "" || flags.NArg() != 0 {
		return fmt.Errorf("file, profiles and release-digest are required")
	}
	// The key is held to the key check before anything signed with it is read.
	publicKey, err := publicKeyOf(*publicKeyText, *requireSigned)
	if err != nil {
		return err
	}
	// A version-3, 4 or 5 export may exceed 8 MiB by the record's bytes, the
	// chain entry and the signature sidecar it carries.
	selection, err := runner.ParseConversionSelector(*conversionText, "preparation-verification")
	if err != nil {
		return err
	}
	exportLimit, err := runner.ConversionExportLimit(selection)
	if err != nil {
		return err
	}
	raw, err := readLimit(*file, exportLimit)
	if err != nil {
		return err
	}
	profilesLimit := 8 << 20
	if selection != nil {
		profilesLimit = 80 << 10
	}
	p, err := readLimit(*profilesPath, profilesLimit)
	if err != nil {
		return err
	}
	profiles, err := runner.ParseInputProfiles(p)
	if err != nil {
		return err
	}
	verified, err := runner.VerifyInputs(raw, profiles, *release, selection)
	// A retained disposition that differs from the audit record is a finding
	// beside the preparation's verification, which reads nothing of the
	// disposition: when the preparation verified, its checked report is kept
	// beside the finding. When it did not, the refusal is in the error beside
	// the finding, and the report is the finding alone.
	var finding error
	if errors.Is(err, runner.ErrAuditDispositionMismatch) {
		if !verified.Checked() {
			json.NewEncoder(stdout).Encode(map[string]string{"status": "audit-disposition-mismatch", "retainedDisposition": "differs-from-audit"})
			return err
		}
		finding = err
	} else if err != nil {
		return err
	}
	classes, scope := targetsOf(verified), scopeOf(verified, inputsScope)
	record := recordReportOf(verified)
	// A version-4 or version-5 export's entry in the installation's chain of
	// runs is checked as far as the verifier can: alone, along a supplied
	// chain, and against checkpoints held independently. A failure, or
	// --require-witnessed, refuses the run before anything else is reported,
	// and a re-execution does not run. So does a record signature that fails,
	// or that --require-signed asks for and no valid one covers.
	chain, err := checkChain(verified, *chainFile, expect, *requireWitnessed)
	// Beside the finding, a chain check that could not run does not discard
	// the preparation's checked report: the report has no runChain member, a
	// sentence says the check did not run, and the error carries why.
	var refusals []error
	unavailable := ""
	if err != nil {
		if finding == nil {
			return err
		}
		refusals = append(refusals, err)
		unavailable = " The check of the export's entry in the installation's chain of runs did not run: " + err.Error() + "."
		chain = nil
	}
	sig := checkSignature(verified, publicKey)
	// Every report carries what was checked; each says its own status, and
	// how the retained disposition stands.
	reportOf := func(status, disposition string) verifyReport {
		return verifyReport{Binding: verified.Binding, recordReport: record, RunChain: chain, Signature: sig, Status: status, RetainedDisposition: disposition, Scope: scope, TargetsByClass: classes, UnsignedFreshness: verified.Freshness, UnsignedParameters: verified.Unsigned}
	}
	// Beside the finding a refusal below is added to the error and the report
	// says the finding; otherwise it is the report's own status, and ends the
	// check.
	refuse := func(status string, err error) bool {
		if finding != nil {
			refusals = append(refusals, err)
			return false
		}
		json.NewEncoder(stdout).Encode(reportOf(status, "matches-audit"))
		return true
	}
	if status, err := chainRefusal(chain, *requireWitnessed); err != nil && refuse(status, err) {
		return err
	}
	if status, err := signatureRefusal(sig, *requireSigned); err != nil && refuse(status, err) {
		return err
	}
	// The record's binding of the preparation is held to the export's
	// preparation once the record's chain and signature have been checked: a
	// binding of another scheme, or another preparation's, is refused, and so,
	// with --require-bound, is a record that carries none. The refusal comes
	// before a re-execution, which then does not run.
	if status, err := bindingRefusal(verified.Binding, verified.Inputs(), *requireBound); err != nil && refuse(status, err) {
		return err
	}
	// An asserted target, and a parameter that its source's receipt does not
	// commit, are checked against nothing but the export, which an operator can
	// rewrite consistently, and so are the inputs of a run without lineage.
	// The refusal comes before a re-execution, which then does not run.
	if *requireSourced {
		if status, err := refuseUnsourced(verified.Inputs(), verified.Classes, verified.Unsigned); err != nil && refuse(status, err) {
			return err
		}
	}
	said := assertedSentence(verified.Classes) + unsignedSentence(verified.Classes, verified.Unsigned) + freshnessSentence(verified.Freshness, committed(verified.Binding, sig)) + notMappedSentence(verified.Inputs()) + record.sentence() + unavailable + chainSentence(chain) + signatureSentence(sig, verified.ExportVersion()) + bindingSentence(verified.Binding, sig)
	if finding != nil {
		// What verified is reported beside the finding, with the sentence
		// that says what it covers; no evaluator is replayed.
		beside := ""
		if verified.Inputs() == "" {
			beside = " The run's preparation was verified independently of it."
		}
		fmt.Fprintln(stderr, "audit-disposition-mismatch: the retained disposition differs from the audit record."+beside+said)
		json.NewEncoder(stdout).Encode(reportOf("audit-disposition-mismatch", "differs-from-audit"))
		if len(refusals) == 0 {
			return finding
		}
		return errors.Join(append([]error{finding}, refusals...)...)
	}
	if *runtime == "" {
		// The retained answer matches the audit record; no evaluator was replayed.
		fmt.Fprintln(stderr, "verified-inputs: the run's inputs match its record. Its retained disposition matches its audit record; the evaluator was not replayed."+said)
		return json.NewEncoder(stdout).Encode(reportOf("verified-inputs", "matches-audit"))
	}
	// The release's own Runtime evaluates the verified inputs again, as a
	// rehearsal. A different disposition is a failure with a report of its own.
	scope = scopeOf(verified, reExecutedScope)
	err = verified.Disposition(context.Background(), *runtime)
	if errors.Is(err, runner.ErrDispositionDiffers) {
		json.NewEncoder(stdout).Encode(reportOf("disposition-differs", "differs-from-re-execution"))
		return err
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, "verified-disposition: the run's inputs match its record, and the release's Runtime, given them again, decides what the record says. This does not say the inputs or the policy are true."+said)
	return json.NewEncoder(stdout).Encode(reportOf("verified-disposition", "matches-re-execution"))
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

const (
	inputsScope     = "retained input derivation and audit binding; not sealed-session completeness or policy truth"
	reExecutedScope = "retained input derivation, audit binding, and the retained disposition against a re-execution by the release's Runtime; not sealed-session completeness or policy truth"
	// The scopes of an export without lineage, whose inputs nothing derives.
	notMappedScope           = "audit binding of the retained inputs, which no lineage derives; not input derivation, sealed-session completeness or policy truth"
	notMappedReExecutedScope = "audit binding of the retained inputs, which no lineage derives, and the retained disposition against a re-execution by the release's Runtime; not input derivation, sealed-session completeness or policy truth"
)

// scopeOf is the report's scope: the one given for an export with lineage,
// and its counterpart without input derivation for one without.
func scopeOf(v runner.VerifiedRun, withLineage string) string {
	switch {
	case v.Inputs() != runner.InputsNotMapped:
		return withLineage
	case withLineage == reExecutedScope:
		return notMappedReExecutedScope
	}
	return notMappedScope
}

// targetsOf is the report's targetsByClass: the counts of an export's
// targets by the class its lineage records, and nil for an export without
// lineage, which records none. Counts of zero would read as no asserted
// target.
func targetsOf(v runner.VerifiedRun) *runner.InputClasses {
	if v.Inputs() == runner.InputsNotMapped {
		return nil
	}
	return &v.Classes
}

// verifyReport is verify-run's report on standard output. Its members encode
// sorted by name, as they did when the report was a map; the counts sort last.
type verifyReport struct {
	// Present only for an export with lineage: what the record's binding of
	// the export's preparation is (runner.BindingReport).
	Binding *runner.BindingReport `json:"binding,omitempty"`
	recordReport
	RetainedDisposition string `json:"retainedDisposition"`
	// Present only for a version-4 or version-5 export: what checking the
	// run's entry in the installation's chain of runs found. Versions 2 and 3
	// carry no entry, and their report is what it was before the chain existed.
	RunChain *runner.RunChainReport `json:"runChain,omitempty"`
	Scope    string                 `json:"scope"`
	// Present only for a version-5 export, or when --public-key is given:
	// what checking the run's record signature found.
	Signature *runner.SignatureReport `json:"signature,omitempty"`
	Status    string                  `json:"status"`
	// Absent only for an export without lineage, whose inputs member says so.
	TargetsByClass *runner.InputClasses `json:"targetsByClass,omitempty"`
	// Present only when a source was acquired: its freshness was judged
	// against preparation.verifiedAt, which the export supplies, and which a
	// signature covers only through the record's binding, when it matches.
	UnsignedFreshness *runner.UnsignedFreshness `json:"unsignedFreshness,omitempty"`
	// Present only when an acquired source's rule reads a parameter that its
	// receipt does not commit.
	UnsignedParameters []runner.UnsignedParameters `json:"unsignedParameters,omitempty"`
}

// recordReport is what the report says of the audit record's bytes, by which a
// digest of the record is taken. A version-3, 4 or 5 export carries bytes,
// which parse to the record it exports; their SHA-256 is the digest a gateway
// action receipt would name for those bytes. That they are the bytes the
// Runtime wrote for this run is not shown without such a digest held
// independently. A version-2 export carries none, and nothing about them could
// be checked. Inputs is "not-mapped" for an export without lineage, as the
// export's own member says, and absent for one with lineage: it sorts here.
type recordReport struct {
	ExactBytes    string `json:"exactBytes"`
	ExportVersion int    `json:"exportVersion"`
	Inputs        string `json:"inputs,omitempty"`
	RecordDigest  string `json:"recordDigest,omitempty"`
}

func recordReportOf(v runner.VerifiedRun) recordReport {
	if v.ExportVersion() >= 3 {
		return recordReport{ExactBytes: "matches-record", ExportVersion: v.ExportVersion(), Inputs: v.Inputs(), RecordDigest: v.RecordDigest()}
	}
	return recordReport{ExactBytes: "not-in-export", ExportVersion: v.ExportVersion(), Inputs: v.Inputs()}
}

// notMappedSentence is what the line on standard error says of an export
// without lineage, after what it says of asserted inputs, which no lineage
// counts here. It adds nothing for an export with lineage.
func notMappedSentence(inputs string) string {
	if inputs != runner.InputsNotMapped {
		return ""
	}
	return " The run's job has no mapping v2, so its export carries no lineage: its inputs are the operator's own, and nothing here derives them from a source or checks them against one."
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

func relocateWorkspace(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, 16385))
	if err != nil || len(raw) > 16384 {
		return fmt.Errorf("bounded relocation request required")
	}
	request, err := runner.DecodeRelocateRequest(raw)
	if err != nil {
		return err
	}
	result, err := runner.RelocateWorkspace(request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
