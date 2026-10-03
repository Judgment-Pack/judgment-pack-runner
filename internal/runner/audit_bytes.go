package runner

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
)

// A digest of an audit record, such as the decision.recordDigest of a gateway
// action receipt, is taken over the bytes the Runtime wrote. Runner encodes the
// record it parses with other bytes: Go's encoder escapes &, < and >, which the
// Runtime writes as they are. So a run keeps the record's line too, exactly,
// and only a verification export of version 3, 4 or 5 carries it. Versions 4
// and 5 also carry the run's entry in the installation's chain of runs
// (run_chain.go), and version 5 the record's signature sidecar
// (signatures.go).

// retainAudit keeps an operational evaluation's audit trail on its run: the
// record parsed, as Runner has always kept it, and its line exactly; and the
// trail's signature sidecar exactly, when the Runtime wrote one. They are kept
// in the transaction that records the run completed.
func (r *Run) retainAudit(trail, signatures []byte) {
	r.Audit = bytes.TrimSpace(trail)
	r.AuditBytes = recordLine(trail)
	r.AuditSignatures = nil
	if len(r.AuditBytes) > 0 && len(signatures) > 0 {
		r.AuditSignatures = signatures
	}
}

// recordLine is the one record an attempt's audit trail holds, as gateway SPEC
// §4 step 6 reads a line of a .jsonl file and hashes it: without the newline
// that ends it. The Runtime writes an attempt's trail as one such line. A trail
// of another shape has no line that is the record, and nil is returned: an
// operational evaluation whose trail has none is refused (evaluateWith).
func recordLine(trail []byte) []byte {
	line, ended := bytes.CutSuffix(trail, []byte("\n"))
	if !ended || !oneLine(line) {
		return nil
	}
	return line
}

// oneLine says whether b can be one line of an audit trail: it is not empty,
// and holds neither a line feed nor a carriage return.
func oneLine(b []byte) bool { return len(b) > 0 && !bytes.ContainsAny(b, "\r\n") }

// exportVersionAsked is the verification export version a caller asks for,
// with ?version=. A caller that asks for none is served version 2, as it was
// before versions 3, 4 and 5 existed. The query is parsed strictly: a malformed one,
// which a lenient reader would drop entries of, is refused, since what it asks
// for cannot be told.
func exportVersionAsked(rawQuery string) (int, error) {
	q, e := url.ParseQuery(rawQuery)
	switch v := q["version"]; {
	case e != nil:
		// Refused below.
	case len(v) == 0:
		return 2, nil
	case len(v) == 1 && v[0] == "2":
		return 2, nil
	case len(v) == 1 && v[0] == "3":
		return 3, nil
	case len(v) == 1 && v[0] == "4":
		return 4, nil
	case len(v) == 1 && v[0] == "5":
		return 5, nil
	}
	return 0, &apiError{400, "invalid_version", "Ask once for verification export version 2, 3, 4 or 5, in a well-formed query."}
}

// auditBytesMember is what run.auditBytes adds to an export, around its
// base64 text, as Runner encodes it.
const auditBytesMember = `,"auditBytes":""`

// chainMember is what a version-4 export's chain member adds to it, around the
// entry's base64 text and the checkpoint; mostCheckpoint is the longest a
// checkpoint encodes to.
const (
	chainMember    = `,"chain":{"entry":"","checkpoint":}`
	mostCheckpoint = len(`{"checkpointVersion":"1","recordDigest":"sha256:","sequence":,"trail":""}`) + 64 + len("9007199254740990") + 32
)

// auditSignaturesMember is what run.auditSignatures adds to an export, around
// its base64 text.
const auditSignaturesMember = `,"auditSignatures":""`

// MaxExportSize is the most a verification export can hold: the 8 MiB that
// version 2 has always been held to; from version 3 on, the member carrying
// the audit record's bytes, at most maxOutput of them in base64; from version
// 4 on, the member carrying the run's chain entry, at most maxChainLine bytes
// in base64, and its checkpoint; and in version 5, the member carrying the
// record's signature sidecar, at most maxSidecar bytes in base64. An export
// that version 2 accepts is accepted as every later version too.
const MaxExportSize = 8<<20 + len(auditBytesMember) + (maxOutput+2)/3*4 + len(chainMember) + (maxChainLine+2)/3*4 + mostCheckpoint + len(auditSignaturesMember) + (maxSidecar+2)/3*4

// exportWithinLimits holds a decoded export to 8 MiB, not counting the member
// that carries the record's bytes from version 3 on, the chain member from
// version 4 on, nor version 5's member that carries the record's signature
// sidecar. The bytes are held to maxOutput, the most of an audit trail Runner
// reads, the chain entry to maxChainLine and the sidecar to maxSidecar.
func exportWithinLimits(raw []byte, b VerificationBundle) error {
	size := len(raw)
	if len(b.Run.AuditSignatures) > 0 {
		if len(b.Run.AuditSignatures) > maxSidecar {
			return fmt.Errorf("the record's signature sidecar exceeds %d bytes", maxSidecar)
		}
		size -= len(auditSignaturesMember) + base64.StdEncoding.EncodedLen(len(b.Run.AuditSignatures))
	}
	if b.Version >= 3 {
		if len(b.Run.AuditBytes) > maxOutput {
			return errors.New("the audit record's bytes exceed 8 MiB")
		}
		size -= len(auditBytesMember) + base64.StdEncoding.EncodedLen(len(b.Run.AuditBytes))
	}
	if b.Chain != nil {
		if len(b.Chain.Entry) > maxChainLine {
			return fmt.Errorf("the run's chain entry exceeds %d bytes", maxChainLine)
		}
		size -= len(`,"chain":`) + len(encode(b.Chain))
	}
	if size > 8<<20 {
		return errors.New("verification bundle exceeds 8 MiB")
	}
	return nil
}

// verificationExport is a run's verification export in the version asked for,
// 2 or 3. Version 2 is the export Runner has always made. It carries no
// original bytes of the audit record: its readers decode it strictly and would
// refuse a new member. Version 3 carries them as run.auditBytes, beside the
// parsed run.audit. A run that holds none, because it was recorded before
// Runner kept them or has no audit record, is exported as version 2 whatever is
// asked: nothing is made up in their place. Version 4 is chainedExport's.
func verificationExport(release Release, run Run, asked int) VerificationBundle {
	run.AuditSignatures = nil
	version := 2
	if asked == 3 && len(run.AuditBytes) > 0 {
		version = 3
	} else {
		run.AuditBytes = nil
	}
	return VerificationBundle{Version: version, ReleaseDigest: releaseDigest(release), Release: release, Run: run}
}

// chainedExport is a run's version-4 export: version 3, with the run's entry in
// the installation's chain of runs, exactly as stored, and the entry's
// checkpoint. Version 3's readers decode strictly and accept only versions 2
// and 3, so the entry is a new version's. A run with no entry, recorded before
// Runner chained its runs, is exported as version 3 (or 2, without the
// record's bytes) whatever is asked: it is unchained, and nothing is made up in
// its entry's place. An entry is exported as it is stored, whatever it names:
// verification holds it to the run, and a store whose entry does not read as
// one is refused here rather than exported without it.
func chainedExport(release Release, run Run, entry []byte) (VerificationBundle, error) {
	b := verificationExport(release, run, 3)
	if b.Version != 3 || len(entry) == 0 {
		return b, nil
	}
	checkpoint, err := checkpointOf(entry)
	if err != nil {
		return VerificationBundle{}, fmt.Errorf("this run's chain entry is not one Runner writes: %w", err)
	}
	b.Version, b.Chain = 4, &RunChainExport{Entry: entry, Checkpoint: checkpoint}
	return b, nil
}

// signedExport is a run's version-5 export: version 4, with the signature
// sidecar of the attempt's audit trail, exactly as the Runtime wrote it, as
// run.auditSignatures. Version 4's readers decode strictly and accept only
// versions 2 to 4, so the sidecar is a new version's. A run with no sidecar,
// recorded without a signing key, by a Runtime that cannot sign, or before
// Runner kept the sidecar, is exported as version 4 (or 3, or 2) whatever is
// asked: it is unsigned, and nothing is made up in the sidecar's place. What
// the sidecar holds is exported as it is: verification holds it to the record.
func signedExport(b VerificationBundle, run Run) VerificationBundle {
	if b.Version != 4 || len(run.AuditSignatures) == 0 {
		return b
	}
	b.Version, b.Run.AuditSignatures = 5, run.AuditSignatures
	return b
}

// checkAuditSignatures holds an export's record signature sidecar to its
// version: version 5 carries one, not empty and at most maxSidecar bytes, and
// earlier versions carry none. Whether it signs the record is checked only
// under a key the verifier gives (VerifiedRun.CheckSignature).
func checkAuditSignatures(b VerificationBundle) error {
	n := len(b.Run.AuditSignatures)
	switch {
	case b.Version != 5 && n == 0:
		return nil
	case b.Version != 5:
		return fmt.Errorf("a version-%d export carries no record signatures", b.Version)
	case n == 0:
		return errors.New("a version-5 export carries the signature sidecar of the run's audit record")
	}
	return nil
}

// checkAuditBytes holds an export's original bytes of the audit record to the
// record it carries. Version 2 carries none. Versions 3 and 4 carry them for a
// completed run, as one line, which must parse, as strictly as the export
// does, to the same JSON value as the parsed record. Versions 4 and 5 carry
// the same bytes as version 3.
func checkAuditBytes(b VerificationBundle) error {
	r := b.Run
	switch {
	case b.Version == 2 && len(r.AuditBytes) == 0:
		return nil
	case b.Version == 2:
		return errors.New("a version-2 export carries no original bytes of the audit record")
	case r.State != "completed" || !oneLine(r.AuditBytes):
		return fmt.Errorf("a version-%d export carries the original bytes of a completed run's audit record, as one line", b.Version)
	}
	if _, e := readInputJSONDepth(r.AuditBytes, 64); e != nil || !sameJSON(r.AuditBytes, r.Audit) {
		return errors.New("the audit record differs from its original bytes")
	}
	return nil
}

// checkChainEntry holds a version-4 or version-5 export's chain entry to its
// run: it is an
// entry as Runner writes one, one line, naming this run and the SHA-256 of the
// audit record's bytes the export carries, and the export's checkpoint is that
// entry's. Versions 2 and 3 carry no entry. That the entry is the one the
// installation's chain holds at its sequence, and that the chain was not
// rewritten, is shown only along the chain and against a checkpoint held
// independently (VerifiedRun.CheckRunChain).
func checkChainEntry(b VerificationBundle) error {
	switch {
	case b.Version < 4 && b.Chain == nil:
		return nil
	case b.Version < 4:
		return fmt.Errorf("a version-%d export carries no entry of the installation's chain of runs", b.Version)
	case b.Chain == nil:
		return fmt.Errorf("a version-%d export carries its run's entry in the installation's chain of runs", b.Version)
	}
	e, err := ParseChainEntry(b.Chain.Entry)
	switch {
	case err != nil:
		return fmt.Errorf("the run's chain entry is not one Runner writes: %v", err)
	case e.Run != b.Run.ID:
		return errors.New("the run's chain entry names another run")
	case e.AuditDigest != digest(b.Run.AuditBytes):
		return errors.New("the run's chain entry names other bytes of the audit record than the export carries")
	case b.Chain.Checkpoint != Checkpoint{CheckpointVersion: CheckpointVersion, RecordDigest: digest(b.Chain.Entry), Sequence: e.Sequence, Trail: e.Trail}:
		return errors.New("the export's checkpoint is not its run's chain entry's")
	}
	return nil
}

// ExportVersion is the verified export's version: 5 when it carries the
// record's signature sidecar, 4 when it carries the run's chain entry and no
// sidecar, 3 when it carries the audit record's original bytes and no entry,
// and 2 when it carries none of them.
func (v VerifiedRun) ExportVersion() int { return v.bundle.Version }

// RecordDigest is the SHA-256 of the audit record's bytes that a version-3
// export carries: the digest a gateway action receipt would name, as its
// decision.recordDigest, for those bytes. Verification shows only that the
// bytes parse to the exported record. That they are the bytes the Runtime
// wrote for this run is shown only by an equal digest held independently, such
// as a receipt's: changed bytes change the digest. It is empty for version 2,
// which carries no bytes to take it over. A digest of the parsed record would
// be of a re-encoding, and is never given. Versions 4 and 5 carry the same
// bytes as version 3.
func (v VerifiedRun) RecordDigest() string {
	if v.bundle.Version < 3 {
		return ""
	}
	return digest(v.bundle.Run.AuditBytes)
}
