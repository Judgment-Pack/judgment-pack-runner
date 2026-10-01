package runner

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/url"
)

// A digest of an audit record, such as the decision.recordDigest of a gateway
// action receipt, is taken over the bytes the Runtime wrote. Runner encodes the
// record it parses with other bytes: Go's encoder escapes &, < and >, which the
// Runtime writes as they are. So a run keeps the record's line too, exactly,
// and only a version-3 verification export carries it.

// retainAudit keeps an operational evaluation's audit trail on its run: the
// record parsed, as Runner has always kept it, and its line exactly.
func (r *Run) retainAudit(trail []byte) {
	r.Audit = bytes.TrimSpace(trail)
	r.AuditBytes = recordLine(trail)
}

// recordLine is the one record an attempt's audit trail holds, as gateway SPEC
// §4 step 6 reads a line of a .jsonl file and hashes it: without the newline
// that ends it. The Runtime writes an attempt's trail as one such line. A trail
// of another shape has no line that is the record, and nil is returned.
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
// before version 3 existed. The query is parsed strictly: a malformed one,
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
	}
	return 0, &apiError{400, "invalid_version", "Ask once for verification export version 2 or 3, in a well-formed query."}
}

// auditBytesMember is what run.auditBytes adds to an export, around its
// base64 text, as Runner encodes it.
const auditBytesMember = `,"auditBytes":""`

// MaxExportSize is the most a verification export can hold: the 8 MiB that
// version 2 has always been held to, and, in version 3, the member carrying the
// audit record's bytes, at most maxOutput of them in base64. An export that
// version 2 accepts is accepted as version 3 too.
const MaxExportSize = 8<<20 + len(auditBytesMember) + (maxOutput+2)/3*4

// exportWithinLimits holds a decoded export to 8 MiB, not counting a version-3
// export's member that carries the record's bytes. Those are held to
// maxOutput, the most of an audit trail Runner reads.
func exportWithinLimits(raw []byte, b VerificationBundle) error {
	size := len(raw)
	if b.Version == 3 {
		if len(b.Run.AuditBytes) > maxOutput {
			return errors.New("the audit record's bytes exceed 8 MiB")
		}
		size -= len(auditBytesMember) + base64.StdEncoding.EncodedLen(len(b.Run.AuditBytes))
	}
	if size > 8<<20 {
		return errors.New("verification bundle exceeds 8 MiB")
	}
	return nil
}

// verificationExport is a run's verification export in the version asked for.
// Version 2 is the export Runner has always made. It carries no original bytes
// of the audit record: its readers decode it strictly and would refuse a new
// member. Version 3 carries them as run.auditBytes, beside the parsed
// run.audit. A run that holds none, because it was recorded before Runner kept
// them or has no audit record, is exported as version 2 whatever is asked:
// nothing is made up in their place.
func verificationExport(release Release, run Run, asked int) VerificationBundle {
	version := 2
	if asked == 3 && len(run.AuditBytes) > 0 {
		version = 3
	} else {
		run.AuditBytes = nil
	}
	return VerificationBundle{Version: version, ReleaseDigest: releaseDigest(release), Release: release, Run: run}
}

// checkAuditBytes holds an export's original bytes of the audit record to the
// record it carries. Version 2 carries none. Version 3 carries them for a
// completed run, as one line, which must parse, as strictly as the export
// does, to the same JSON value as the parsed record.
func checkAuditBytes(b VerificationBundle) error {
	r := b.Run
	switch {
	case b.Version == 2 && len(r.AuditBytes) == 0:
		return nil
	case b.Version == 2:
		return errors.New("a version-2 export carries no original bytes of the audit record")
	case r.State != "completed" || !oneLine(r.AuditBytes):
		return errors.New("a version-3 export carries the original bytes of a completed run's audit record, as one line")
	}
	if _, e := readInputJSONDepth(r.AuditBytes, 64); e != nil || !sameJSON(r.AuditBytes, r.Audit) {
		return errors.New("the audit record differs from its original bytes")
	}
	return nil
}

// ExportVersion is the verified export's version: 3 when it carries the audit
// record's original bytes, and 2 when it carries none.
func (v VerifiedRun) ExportVersion() int { return v.bundle.Version }

// RecordDigest is the SHA-256 of the audit record's bytes that a version-3
// export carries: the digest a gateway action receipt would name, as its
// decision.recordDigest, for those bytes. Verification shows only that the
// bytes parse to the exported record. That they are the bytes the Runtime
// wrote for this run is shown only by an equal digest held independently, such
// as a receipt's: changed bytes change the digest. It is empty for version 2,
// which carries no bytes to take it over. A digest of the parsed record would
// be of a re-encoding, and is never given.
func (v VerifiedRun) RecordDigest() string {
	if v.bundle.Version != 3 {
		return ""
	}
	return digest(v.bundle.Run.AuditBytes)
}
