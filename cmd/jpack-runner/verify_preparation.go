package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/Judgment-Pack/judgment-pack-runner/internal/buildinfo"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/runner"
)

// couldNotRun is the status of a verify-preparation that ran no reading, as
// on an argument it does not take or a file it cannot open: it writes no
// report (spec/dialect.md §11).
const couldNotRun = 3

// preparationInputCap is the most verify-preparation reads of one file: the
// largest bound spec/dialect.md §12 gives a document, the preparation's, and
// a byte more, so that a document over its bound is refused by name, as
// input-over-bound, and not read whole.
const preparationInputCap = 8<<20 + 1

// sha256Form is a digest's form: sha256: and 64 lowercase hexadecimal
// characters.
var sha256Form = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const verifyPreparationUsage = `Usage: jpack-runner verify-preparation --dialect FILE [--binding FILE] --case FILE --profiles FILE [--sources FILE] --preparation FILE --preparation-digest sha256:HEX
       jpack-runner verify-preparation --unbound FILE --binding FILE --case FILE --profiles FILE [--sources FILE] --preparation FILE --preparation-digest sha256:HEX
       jpack-runner verify-preparation --capabilities

Verifies one preparation offline, as a preparation verifier of the specification's
dialect does (spec/dialect.md §11): it recomputes the preparation from the dialect,
the case, the acquisitions and the profiles given, at the given preparation's own
verifiedAt and at no other instant, through Runner's own preparation, and compares it
with the given preparation in the canonical form. It asks no network, model or
Runtime, needs no run, release or audit record, and writes nothing.

Before it reads anything it holds the preparation's bytes to --preparation-digest: a
precondition, as a file it can open is. A preparation the digest does not name is not
read: the command could not run.

It writes one report on standard output, one line, and the reason on standard error,
and exits 0 for a reading, 1 for a refusal and 2 for a finding; 3, writing no report
and saying why on standard error, when it could not run: an argument it does not take,
a file it cannot open, a preparation its digest does not name, or -h. --capabilities
prints what it reads, as one JSON object, and exits 0.

A reading establishes that the preparation recomputes from these inputs. Its binding
is not-checked: no record is given, so nothing here commits the preparation or its
instant to a signed record. The profiles are verification inputs, not an
installation's trusted configuration, and a reading says nothing of any record's truth.

`

func verifyPreparation(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify-preparation", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, verifyPreparationUsage)
		flags.PrintDefaults()
	}
	conversionText := flags.String("conversion-contract", "", "optional: exact installed retained-column selector in canonical base64url")
	dialect := flags.String("dialect", "", "the dialect document: bound, or, with --binding, published with unbound sources, which the binding document binds as Runner's import step does (spec/dialect.md §4.4)")
	unbound := flags.String("unbound", "", "the published dialect, with unbound sources, which --binding binds: --dialect with --binding, named so")
	binding := flags.String("binding", "", "the binding document of a dialect with unbound sources")
	casePath := flags.String("case", "", "the case document")
	profilesPath := flags.String("profiles", "", "the input profiles the verification trusts, one JSON array: verification inputs, such as a fixture's, not an installation's configuration")
	sourcesPath := flags.String("sources", "", "optional: the acquisitions, one JSON object by source name, as a fixture's sources.json holds them; absent, there are none")
	preparationPath := flags.String("preparation", "", "the expected preparation; its verifiedAt is the only instant it is recomputed at")
	preparationDigest := flags.String("preparation-digest", "", "the expected preparation's digest, sha256: and 64 lowercase hexadecimal characters, supplied apart from the files: a precondition, checked against the preparation's bytes before anything is read; a preparation it does not name is not read, and the command exits 3 with no report")
	capabilities := flags.Bool("capabilities", false, "print what verify-preparation reads, as one JSON object, and nothing else")
	couldNot := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "verify-preparation: could not run: "+format+"\n", a...)
		return couldNotRun
	}
	if err := flags.Parse(args); err != nil {
		return couldNotRun
	}
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if flags.NArg() != 0 {
		return couldNot("it takes no argument but its flags, and was given %q", flags.Arg(0))
	}
	if *capabilities {
		if len(given) != 1 {
			return couldNot("--capabilities takes no other flag")
		}
		fmt.Fprintf(stdout, "%s\n", runner.PreparationVerifierCapabilities(buildinfo.Version()))
		return 0
	}
	for name := range given {
		if flags.Lookup(name).Value.String() == "" {
			return couldNot("--%s names no file", name)
		}
	}
	switch {
	case given["dialect"] == given["unbound"]:
		return couldNot("give the dialect as --dialect, or the published dialect as --unbound with --binding: exactly one of them")
	case given["unbound"] && !given["binding"]:
		return couldNot("--unbound is the published dialect, which --binding binds: give its binding document")
	case !given["case"] || !given["profiles"] || !given["preparation"] || !given["preparation-digest"]:
		return couldNot("--case, --profiles, --preparation and --preparation-digest are required")
	case !sha256Form.MatchString(*preparationDigest):
		return couldNot("--preparation-digest is not sha256: and 64 lowercase hexadecimal characters")
	}
	dialectPath := *dialect
	if given["unbound"] {
		dialectPath = *unbound
	}
	selection, err := runner.ParseConversionSelector(*conversionText, "document-validation", "preview", "preparation", "preparation-verification")
	if err != nil {
		return couldNot("%v", err)
	}
	fixture := runner.PreparationFixture{ConversionContract: selection}
	inputCap := int64(preparationInputCap)
	if selection != nil {
		inputCap = 16<<20 + 1
	}
	for _, file := range []struct {
		path  string
		into  *[]byte
		given bool
	}{
		{dialectPath, &fixture.Dialect, true},
		{*binding, &fixture.Binding, given["binding"]},
		{*casePath, &fixture.Case, true},
		{*sourcesPath, &fixture.Sources, given["sources"]},
		{*profilesPath, &fixture.Profiles, true},
		{*preparationPath, &fixture.Preparation, true},
	} {
		if !file.given {
			continue
		}
		b, err := readAtMost(file.path, inputCap)
		if err != nil {
			return couldNot("%v", err)
		}
		*file.into = b
	}
	// The pin, before anything is read: a precondition, as a file the
	// command can open is, and no reading of §11's (runner.PreparationPin).
	if err := runner.PreparationPin(fixture.Preparation, *preparationDigest, selection); err != nil {
		return couldNot("%v", err)
	}
	report := runner.VerifyPreparation(fixture)
	fmt.Fprintf(stdout, "%s\n", report.Line())
	if report.Result == runner.ResultReading {
		fmt.Fprintln(stderr, "verify-preparation: reading: the preparation recomputes, byte for byte in the canonical form, from the dialect, the case, the acquisitions and the profiles given, at its own verifiedAt; its digest is "+report.Digest+". Its binding is not-checked: no record was given, so nothing here commits the preparation, or its instant, to a signed record. The profiles were taken as verification inputs, not as an installation's trusted configuration: this establishes recomputation, not the truth of any record.")
	} else {
		fmt.Fprintf(stderr, "verify-preparation: %s: %s\n", report.Code, report.Reason)
	}
	return report.ExitStatus()
}

// readAtMost reads a file's first limit bytes, or all of it when it is
// shorter: a document over its bound is then refused by its length.
func readAtMost(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%s is not a file", path), err)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
