package runner

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"sort"
	"strconv"
)

// The installation's chain of runs (runtime ADR-0047, "Runner").
//
// Each attempt has its own audit directory, never reused, so the Runtime's trail
// inside it holds one record: as a chain it says nothing about the
// installation's history. Runner keeps a chain of its own over the runs it
// retains. An entry binds one completed run's id to the SHA-256 of its audit
// record's exact bytes, run.auditBytes, and entries are chained by the Runtime's
// rules: trail, sequence, and previous over the exact bytes of the line before.
//
// The chain lives in the store, in the run_chain table: one row per entry,
// holding the entry's line exactly as written. Every run recorded completed is
// given its entry in the same transaction, so a crash leaves both or neither,
// and nothing is held in memory between appends. A run whose record's exact
// bytes were not kept is not recorded completed: an evaluation whose trail is
// not one line is refused, and a completion without the bytes fails as a failed
// append does. Each append reads the
// last row inside its transaction: the next entry continues its trail, its
// sequence plus one, and links to its line's digest. SQLite serializes write
// transactions, Runner holds the store's installation lock, and the store's
// handle has one connection, so two appends never read the same last row; the
// sequence is also the table's primary key, which refuses a repeated one.
//
// Runner never deletes an entry, and nothing it offers deletes a run. A run
// removed from the store keeps its entry, which still names it. An entry removed
// leaves a gap that fails the next line's sequence and previous. An entry
// removed with every later one rewritten to close the gap is consistent again,
// and fails only against a checkpoint held independently of the operator that
// covers it. Runs recorded before the chain existed have no entry and are never
// given one: they are unchained.
//
// What the chain does not establish: anything against the operator, who keeps
// the store and the chain and can rewrite both with every link recomputed,
// unless the verifier holds a checkpoint the operator did not supply.

const (
	// ChainEntryVersion is the shape of an entry, a single integer as a string
	// on the Runtime's recordVersion precedent.
	ChainEntryVersion = "1"
	// CheckpointVersion is the shape of a checkpoint. A checkpoint is the
	// Runtime's (ADR-0047 §1, §2a): the same members, read by the same rule.
	CheckpointVersion = "1"
	// chainEntryKind is the one kind of entry Runner writes. It is never the
	// Runtime's "discontinuity", which a Runtime verifier reads differently.
	chainEntryKind = "run"
	// maxChainLine bounds an entry's line. Runner writes about 300 bytes, and
	// writes none it would not read back.
	maxChainLine = 1024
	// maxSafeInteger is 2^53-1: a sequence stops short of it, so the next one is
	// still an integer every JSON reader holds exactly.
	maxSafeInteger = 1<<53 - 1
	// MaxHeldCheckpoints bounds a document of held checkpoints, as the Runtime
	// bounds one: at about two hundred bytes a line, some eighty thousand.
	MaxHeldCheckpoints = 16 << 20
	// maxChainFindings bounds the findings a report lists; FindingsTotal counts
	// them all, as the Runtime's audit verify does.
	maxChainFindings = 100
)

const runChainSchema = `CREATE TABLE IF NOT EXISTS run_chain (sequence INTEGER PRIMARY KEY CHECK (sequence > 0), run TEXT UNIQUE NOT NULL, line BLOB NOT NULL);`

var (
	chainTrailForm  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	chainDigestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ChainEntry is one entry of the installation's chain of runs, its members in
// the order Runner writes them: its version, then the chain's three members,
// as the Runtime writes them after recordVersion, then what it binds.
type ChainEntry struct {
	EntryVersion string `json:"entryVersion"`
	Trail        string `json:"trail"`
	Sequence     int64  `json:"sequence"`
	Previous     string `json:"previous"`
	Kind         string `json:"kind"`
	Run          string `json:"run"`
	// AuditDigest is the SHA-256 of the run's audit record's exact bytes: the
	// digest a gateway action receipt's decision.recordDigest names it by.
	AuditDigest string `json:"auditDigest"`
}

// Checkpoint names one entry of a chain by what a holder needs to check it
// later: the chain's identity, the entry's sequence, and the SHA-256 of the
// entry's exact line bytes. It is the Runtime's checkpoint. The members are
// declared in code-point order and hold only lowercase hex, fixed text and an
// integer under 2^53, so its compact encoding is its RFC 8785 canonical form.
type Checkpoint struct {
	CheckpointVersion string `json:"checkpointVersion"`
	RecordDigest      string `json:"recordDigest"`
	Sequence          int64  `json:"sequence"`
	Trail             string `json:"trail"`
}

// RunChainExport is what a version-4 export adds: the run's entry, its exact
// line in base64, and the checkpoint of that entry, which a holder keeps. The
// checkpoint is a function of the entry's bytes, so every export of the run
// carries the same one.
type RunChainExport struct {
	Entry      []byte     `json:"entry"`
	Checkpoint Checkpoint `json:"checkpoint"`
}

// newChainTrail mints a chain's identity: sixteen random bytes, hex.
func newChainTrail() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// errChainHead is the refusal to append after a last entry that is not one
// Runner writes, or not at its own sequence.
var errChainHead = errors.New("the last entry of the installation's chain of runs is not one Runner writes, at its own sequence; no entry is chained after it")

// errNoAuditBytes is the refusal to chain a completed run without the exact
// bytes of its audit record, one line, which its entry's digest is taken over.
var errNoAuditBytes = errors.New("a completed run's audit record's exact bytes were not kept; it is not recorded completed without its entry")

// errEntryUnreadable is the refusal to write an entry Runner would not read
// back, which would stop the chain at the next append.
var errEntryUnreadable = errors.New("the run's entry in the installation's chain of runs would not read as one")

// appendRunEntry appends a completed run's entry to the chain, inside tx: it
// continues the last row's trail and sequence and links to that row's line, or
// starts a chain at sequence 1, linked to the SHA-256 of nothing, under a new
// identity.
func appendRunEntry(tx *sql.Tx, run string, auditBytes []byte) error {
	if !oneLine(auditBytes) {
		return errNoAuditBytes
	}
	next := ChainEntry{EntryVersion: ChainEntryVersion, Kind: chainEntryKind, Run: run, AuditDigest: digest(auditBytes)}
	var last int64
	var line []byte
	switch err := tx.QueryRow("SELECT sequence,line FROM run_chain ORDER BY sequence DESC LIMIT 1").Scan(&last, &line); {
	case errors.Is(err, sql.ErrNoRows):
		next.Trail, next.Sequence, next.Previous = newChainTrail(), 1, digest(nil)
	case err != nil:
		return err
	default:
		held, err := ParseChainEntry(line)
		if err != nil || held.Sequence != last || last+1 >= maxSafeInteger {
			return errChainHead
		}
		next.Trail, next.Sequence, next.Previous = held.Trail, last+1, digest(line)
	}
	line = encode(next)
	if _, err := ParseChainEntry(line); err != nil {
		return errEntryUnreadable
	}
	_, err := tx.Exec("INSERT INTO run_chain(sequence,run,line) VALUES (?,?,?)", next.Sequence, run, line)
	return err
}

// finishRun records a run's end. Every completed run is given its entry in the
// same transaction, so a run is never recorded completed without its entry,
// nor an entry written for a run that is not. A completion whose entry cannot
// be appended, because the record's exact bytes were not kept or the append
// failed, is not recorded: the error is returned, and the dispatcher stops.
// The run's entry in the journal of job activity (events.go) is written in the
// same transaction too, and names the chain entry's sequence.
func (s *Service) finishRun(r Run) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var detail EventDetail
	if r.State == "completed" {
		if err = appendRunEntry(tx, r.ID, r.AuditBytes); err != nil {
			return err
		}
		if err = tx.QueryRow("SELECT sequence FROM run_chain WHERE run=?", r.ID).Scan(&detail.ChainSequence); err != nil {
			return err
		}
	}
	if r.State == "interrupted" && r.InterruptedAt != "" {
		detail.Interruption = &EventInterruption{Seen: true, At: r.InterruptedAt}
	}
	if err = recordRun(tx, r, runnerActor, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// runEntry is a run's entry line, or nil when the chain holds none for it.
func (s *Service) runEntry(run string) ([]byte, error) {
	var line []byte
	err := s.db.QueryRow("SELECT line FROM run_chain WHERE run=?", run).Scan(&line)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return line, err
}

// runEntrySequence is the sequence of a run's entry, and whether there is one.
func (s *Service) runEntrySequence(run string) (int64, bool, error) {
	var sequence int64
	err := s.db.QueryRow("SELECT sequence FROM run_chain WHERE run=?", run).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return sequence, err == nil, err
}

// chainPage is up to limit entry lines after a sequence, in order, and the
// sequence of the last. Rows are only ever appended, so pages read in turn are
// a prefix of the chain as it stood at some moment during the reading.
func (s *Service) chainPage(after int64, limit int) ([][]byte, int64, error) {
	rows, err := s.db.Query("SELECT sequence,line FROM run_chain WHERE sequence>? ORDER BY sequence LIMIT ?", after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var lines [][]byte
	last := after
	for rows.Next() {
		var line []byte
		if err = rows.Scan(&last, &line); err != nil {
			return nil, 0, err
		}
		lines = append(lines, line)
	}
	return lines, last, rows.Err()
}

// checkpointOf is the checkpoint of an entry's line.
func checkpointOf(line []byte) (Checkpoint, error) {
	e, err := ParseChainEntry(line)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{CheckpointVersion: CheckpointVersion, RecordDigest: digest(line), Sequence: e.Sequence, Trail: e.Trail}, nil
}

// ParseChainEntry reads an entry's line by its members, as JSON reads them,
// the way the Runtime recognises a chained line: one JSON object naming exactly
// entryVersion "1", trail (32 lowercase hex characters), sequence (an integer
// from 1 to 2^53-2), previous and auditDigest ("sha256:" and 64 lowercase hex
// characters), kind "run" and run (a run's id, any string but the empty one),
// each once and nothing else.
// Whitespace and escapes are read as JSON reads them, and nothing is encoded
// again: a digest of the entry is of the line's own bytes.
func ParseChainEntry(line []byte) (ChainEntry, error) {
	var e ChainEntry
	if len(line) == 0 || len(line) > maxChainLine || bytes.ContainsAny(line, "\r\n") {
		return e, fmt.Errorf("an entry is one line of 1 to %d bytes", maxChainLine)
	}
	if !json.Valid(line) {
		return e, errors.New("an entry is one JSON text")
	}
	members, err := exactMembers(line)
	if err != nil {
		return e, err
	}
	for name := range members {
		switch name {
		case "entryVersion", "trail", "sequence", "previous", "kind", "run", "auditDigest":
		default:
			return e, fmt.Errorf("an entry has a member %q it does not define", name)
		}
	}
	switch {
	case memberString(members["entryVersion"], &e.EntryVersion) != nil || e.EntryVersion != ChainEntryVersion:
		return e, errors.New(`an entry's entryVersion is "1"`)
	case memberString(members["trail"], &e.Trail) != nil || !chainTrailForm.MatchString(e.Trail):
		return e, errors.New("an entry's trail is 32 lowercase hexadecimal characters")
	case memberInteger(members["sequence"], &e.Sequence) != nil || e.Sequence < 1 || e.Sequence >= maxSafeInteger:
		return e, errors.New("an entry's sequence is an integer from 1 to 9007199254740990")
	case memberString(members["previous"], &e.Previous) != nil || !chainDigestForm.MatchString(e.Previous):
		return e, errors.New("an entry's previous is sha256: and 64 lowercase hexadecimal characters")
	case memberString(members["kind"], &e.Kind) != nil || e.Kind != chainEntryKind:
		return e, errors.New(`an entry's kind is "run"`)
	case memberString(members["run"], &e.Run) != nil || e.Run == "":
		return e, errors.New("an entry's run is a run's id, a string that is not empty")
	case memberString(members["auditDigest"], &e.AuditDigest) != nil || !chainDigestForm.MatchString(e.AuditDigest):
		return e, errors.New("an entry's auditDigest is sha256: and 64 lowercase hexadecimal characters")
	}
	return e, nil
}

// ParseCheckpoint holds a checkpoint document to the Runtime's shape: one JSON
// object of exactly checkpointVersion "1", trail, sequence and recordDigest,
// each named once, with JSON's own whitespace around it allowed.
func ParseCheckpoint(document []byte) (Checkpoint, error) {
	var c Checkpoint
	trimmed := bytes.Trim(document, " \t\r\n")
	if len(trimmed) > 4096 || !json.Valid(trimmed) {
		return c, errors.New("a checkpoint is one JSON text of at most 4096 bytes")
	}
	members, err := exactMembers(trimmed)
	if err != nil {
		return c, err
	}
	for name := range members {
		switch name {
		case "checkpointVersion", "trail", "sequence", "recordDigest":
		default:
			return c, fmt.Errorf("a checkpoint has a member %q it does not define", name)
		}
	}
	switch {
	case memberString(members["checkpointVersion"], &c.CheckpointVersion) != nil || c.CheckpointVersion != CheckpointVersion:
		return c, errors.New(`a checkpoint's checkpointVersion is "1"`)
	case memberString(members["trail"], &c.Trail) != nil || !chainTrailForm.MatchString(c.Trail):
		return c, errors.New("a checkpoint's trail is 32 lowercase hexadecimal characters")
	case memberInteger(members["sequence"], &c.Sequence) != nil || c.Sequence < 1 || c.Sequence >= maxSafeInteger:
		return c, errors.New("a checkpoint's sequence is an integer from 1 to 9007199254740990")
	case memberString(members["recordDigest"], &c.RecordDigest) != nil || !chainDigestForm.MatchString(c.RecordDigest):
		return c, errors.New("a checkpoint's recordDigest is sha256: and 64 lowercase hexadecimal characters")
	}
	return c, nil
}

// ParseCheckpoints reads a holder's document of checkpoints, one per line, as
// the Runtime reads one. Blank lines are passed over; every other line is held
// to ParseCheckpoint, and the first that is not refuses the document. A
// document with none is refused.
func ParseCheckpoints(document []byte) ([]Checkpoint, error) {
	if len(document) > MaxHeldCheckpoints {
		return nil, fmt.Errorf("the checkpoints exceed %d bytes", MaxHeldCheckpoints)
	}
	var held []Checkpoint
	for number, line := range bytes.Split(document, []byte("\n")) {
		if len(bytes.Trim(line, " \t\r")) == 0 {
			continue
		}
		c, err := ParseCheckpoint(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		held = append(held, c)
	}
	if len(held) == 0 {
		return nil, errors.New("the document holds no checkpoint")
	}
	return held, nil
}

// exactMembers reads one JSON object into its members by exact name, refusing
// anything else and a member named twice.
func exactMembers(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	members := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		name, ok := t.(string)
		if err != nil || !ok {
			return nil, errors.New("not a JSON object")
		}
		if _, twice := members[name]; twice {
			return nil, fmt.Errorf("the member %q is named twice", name)
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return nil, errors.New("not a JSON object")
		}
		members[name] = value
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, errors.New("not a JSON object")
	}
	return members, nil
}

// memberString reads a JSON string and nothing else; an absent member is an
// error.
func memberString(raw json.RawMessage, into *string) error {
	if len(raw) == 0 || raw[0] != '"' {
		return errors.New("not a string")
	}
	return json.Unmarshal(raw, into)
}

// memberInteger reads a JSON number that is an integer literal, and nothing
// else. The member is part of a JSON text already held valid, which has no
// leading zero and no plus sign, so a fraction or an exponent is all that
// strconv.ParseInt has left to refuse.
func memberInteger(raw json.RawMessage, into *int64) error {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return errors.New("not an integer")
	}
	*into = n
	return nil
}

// The names of the checks a run's chain can fail. They are stable, and those
// the Runtime's audit verify also makes have its names.
const (
	FindingEntryMalformed           = "entry-malformed"
	FindingLineTooLong              = "line-too-long"
	FindingIncompleteLastLine       = "incomplete-last-line"
	FindingSequenceMismatch         = "sequence-mismatch"
	FindingPreviousMismatch         = "previous-mismatch"
	FindingTrailMismatch            = "trail-mismatch"
	FindingRunNamedTwice            = "run-named-twice"
	FindingEntryMismatch            = "entry-mismatch"
	FindingEntryBeyondChain         = "entry-beyond-chain"
	FindingCheckpointBeyondChain    = "checkpoint-beyond-chain"
	FindingCheckpointNotAnEntry     = "checkpoint-not-an-entry"
	FindingCheckpointTrailMismatch  = "checkpoint-trail-mismatch"
	FindingCheckpointRecordMismatch = "checkpoint-record-mismatch"
)

// The scopes of a run's chain check, which say what was checked. The last two
// are the Runtime's names.
const (
	ScopeOneSuppliedEntry = "one-supplied-entry"
	ScopeOneSuppliedChain = "one-supplied-chain"
	ScopeCheckpoint       = "checkpoint"
)

// ChainFinding is one check a run's chain failed: a stable name, the line it
// is about, and what was found.
type ChainFinding struct {
	Name   string `json:"name"`
	Line   int64  `json:"line"`
	Detail string `json:"detail"`
}

// ChainRead is what was read of a supplied chain: its complete lines, and the
// checkpoint of its last line when that is an entry.
type ChainRead struct {
	Head  *Checkpoint `json:"head,omitempty"`
	Lines int64       `json:"lines"`
}

// ChainHeld is what the run's entry was held to: how many checkpoints were
// supplied and matched, and, when all did, the highest sequence they reach.
type ChainHeld struct {
	Matched  int   `json:"matched"`
	Supplied int   `json:"supplied"`
	Through  int64 `json:"through,omitempty"`
}

// RunChainReport is what checking a version-4 export's entry found. Status is
// "valid" when every check passed and "invalid" when any failed. Witnessed is
// true only when the checks passed and a held checkpoint at or after the
// entry's sequence covers it. Its members encode sorted by name.
type RunChainReport struct {
	Chain         *ChainRead     `json:"chain,omitempty"`
	Checkpoint    Checkpoint     `json:"checkpoint"`
	Findings      []ChainFinding `json:"findings"`
	FindingsTotal int            `json:"findingsTotal"`
	Held          *ChainHeld     `json:"held,omitempty"`
	Scope         string         `json:"scope"`
	Status        string         `json:"status"`
	Witnessed     bool           `json:"witnessed"`
}

// ChainEntry is a verified version-4 or version-5 export's entry and its
// checkpoint, and false for an export of another version, which carries none.
func (v VerifiedRun) ChainEntry() (ChainEntry, Checkpoint, bool) {
	if v.bundle.Version < 4 || v.bundle.Chain == nil {
		return ChainEntry{}, Checkpoint{}, false
	}
	e, _ := ParseChainEntry(v.bundle.Chain.Entry)
	return e, v.bundle.Chain.Checkpoint, true
}

// ErrNoChainEntry is a chain check asked of an export that carries no entry.
var ErrNoChainEntry = errors.New("the export carries no entry of the installation's chain of runs")

// CheckRunChain holds a verified version-4 export's entry to a supplied chain,
// when chain is not nil, and to checkpoints a holder kept. Without a chain,
// every held checkpoint must name the entry's own sequence: one at another
// sequence can be checked only along the chain, and is refused as an error.
// With a chain, the chain is read as Runner writes it, over its exact bytes:
// every line an entry, at its own sequence, of the trail of the line before
// it, linked to that line's digest (the first to the SHA-256 of nothing), and
// no run named twice. The line at the entry's sequence must be the export's
// entry, byte for byte, and every held checkpoint must name a line of the
// chain that has its trail and digest, as the Runtime holds a trail to them.
// The error is for a chain that could not be read; every check that fails is
// a finding in the report.
func (v VerifiedRun) CheckRunChain(chain io.Reader, held []Checkpoint) (RunChainReport, error) {
	entry, checkpoint, ok := v.ChainEntry()
	if !ok {
		return RunChainReport{}, ErrNoChainEntry
	}
	c := &chainCheck{report: RunChainReport{Checkpoint: checkpoint, Scope: ScopeOneSuppliedEntry}, entry: entry, line: v.bundle.Chain.Entry, held: map[int64][]Checkpoint{}}
	highest := int64(0)
	for _, h := range held {
		c.held[h.Sequence] = append(c.held[h.Sequence], h)
		highest = max(highest, h.Sequence)
	}
	if chain == nil {
		for _, h := range held {
			if h.Sequence != entry.Sequence {
				return RunChainReport{}, fmt.Errorf("a held checkpoint at sequence %d does not name this run's entry, at sequence %d: it is checked along the chain, supplied with --chain", h.Sequence, entry.Sequence)
			}
		}
		c.holdTo(entry.Sequence, c.line, &entry)
	} else {
		c.report.Scope = ScopeOneSuppliedChain
		if err := c.read(chain); err != nil {
			return RunChainReport{}, err
		}
	}
	r := c.report
	if len(held) > 0 {
		r.Scope = ScopeCheckpoint
		r.Held = &ChainHeld{Supplied: len(held), Matched: len(held) - c.heldFailed}
	}
	if r.Findings == nil {
		r.Findings = []ChainFinding{}
	}
	r.Status = "valid"
	if r.FindingsTotal > 0 {
		r.Status = "invalid"
	}
	if r.Status == "valid" && len(held) > 0 {
		r.Held.Through = highest
		r.Witnessed = highest >= entry.Sequence
	}
	return r, nil
}

// chainCheck is one run's chain check under way.
type chainCheck struct {
	report     RunChainReport
	entry      ChainEntry
	line       []byte
	held       map[int64][]Checkpoint
	heldFailed int
}

func (c *chainCheck) find(name string, line int64, detail string) {
	c.report.FindingsTotal++
	if len(c.report.Findings) < maxChainFindings {
		c.report.Findings = append(c.report.Findings, ChainFinding{Name: name, Line: line, Detail: detail})
	}
}

// holdTo holds the checkpoints at a sequence to the line there: its digest,
// and the trail of the entry it is, or nil when it is not one.
func (c *chainCheck) holdTo(sequence int64, line []byte, e *ChainEntry) {
	c.holdToDigest(sequence, digest(line), e)
}

func (c *chainCheck) holdToDigest(sequence int64, lineDigest string, e *ChainEntry) {
	for _, h := range c.held[sequence] {
		switch {
		case e == nil:
			c.find(FindingCheckpointNotAnEntry, sequence, "the line at the checkpoint's sequence is not an entry")
		case e.Trail != h.Trail:
			c.find(FindingCheckpointTrailMismatch, sequence, "the entry at the checkpoint's sequence is of another chain")
		case lineDigest != h.RecordDigest:
			c.find(FindingCheckpointRecordMismatch, sequence, "the entry at the checkpoint's sequence is not the one the checkpoint names")
		default:
			continue
		}
		c.heldFailed++
	}
	delete(c.held, sequence)
}

// read reads a supplied chain line by line, over its exact bytes.
func (c *chainCheck) read(chain io.Reader) error {
	reader := bufio.NewReaderSize(chain, 64<<10)
	previous, trail := digest(nil), ""
	runs := map[string]int64{}
	var number int64
	read := &ChainRead{}
	for {
		line, length, sum, ended, err := readChainLine(reader)
		if err != nil {
			return err
		}
		if !ended {
			if length > 0 {
				c.find(FindingIncompleteLastLine, number+1, fmt.Sprintf("the chain ends in %d bytes with no newline: a line that did not complete", length))
			}
			break
		}
		number++
		var e *ChainEntry
		if length > maxChainLine {
			c.find(FindingLineTooLong, number, fmt.Sprintf("the line is %d bytes, longer than any entry", length))
		} else if parsed, err := ParseChainEntry(line); err != nil {
			c.find(FindingEntryMalformed, number, err.Error())
		} else {
			e = &parsed
			if e.Sequence != number {
				c.find(FindingSequenceMismatch, number, fmt.Sprintf("the entry's sequence is %d", e.Sequence))
			}
			if e.Previous != previous {
				c.find(FindingPreviousMismatch, number, "previous is not the digest of the line before it")
			}
			if trail != "" && e.Trail != trail {
				c.find(FindingTrailMismatch, number, "the entry names another trail than the entry before it")
			}
			if first, twice := runs[e.Run]; twice {
				c.find(FindingRunNamedTwice, number, fmt.Sprintf("the entry names the run that line %d names", first))
			} else {
				runs[e.Run] = number
			}
			trail = e.Trail
		}
		if number == c.entry.Sequence && !bytes.Equal(line, c.line) {
			c.find(FindingEntryMismatch, number, "the line at the run's sequence is not the export's entry")
		}
		c.holdToDigest(number, sum, e)
		previous = sum
		read.Lines = number
		read.Head = nil
		if e != nil {
			read.Head = &Checkpoint{CheckpointVersion: CheckpointVersion, RecordDigest: sum, Sequence: number, Trail: e.Trail}
		}
	}
	if c.entry.Sequence > number {
		c.find(FindingEntryBeyondChain, c.entry.Sequence, fmt.Sprintf("the chain has %d complete lines, fewer than the run's sequence", number))
	}
	sequences := []int64{}
	for sequence := range c.held {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	for _, sequence := range sequences {
		for range c.held[sequence] {
			c.find(FindingCheckpointBeyondChain, sequence, fmt.Sprintf("the chain has %d complete lines, fewer than the checkpoint's sequence", number))
			c.heldFailed++
		}
	}
	c.report.Chain = read
	return nil
}

// readChainLine reads one line: up to maxChainLine+1 of its bytes, its whole
// length and SHA-256 without the newline, and whether a newline ended it.
func readChainLine(r *bufio.Reader) ([]byte, int64, string, bool, error) {
	var line []byte
	var length int64
	var sum hash.Hash = sha256.New()
	for {
		piece, err := r.ReadSlice('\n')
		ended := len(piece) > 0 && piece[len(piece)-1] == '\n'
		body := piece
		if ended {
			body = piece[:len(piece)-1]
		}
		sum.Write(body)
		length += int64(len(body))
		if len(line) <= maxChainLine {
			line = append(line, body[:min(len(body), maxChainLine+1-len(line))]...)
		}
		switch {
		case ended:
			return line, length, "sha256:" + hex.EncodeToString(sum.Sum(nil)), true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, length, "", false, nil
		case err != nil:
			return nil, 0, "", false, err
		}
	}
}
