package runner

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unicode/utf8"
)

// A run's attempt diagnostics: what the Runtime wrote to its standard error
// and standard output when it evaluated the run, kept in the run's attempt
// directory (evaluation.stderr and evaluation.stdout, runtime.go). They are
// served as they were written, under a bound, once the run has finished. They
// explain a run, and establish nothing about it: the Runtime's own words, kept
// in the operator's store.

// maxDiagnostics is the most of a stream the route serves: its first 64 KiB.
// The Runtime's standard error is kept up to 1 MiB, and its standard output up
// to 8 MiB.
const maxDiagnostics = 64 << 10

// diagnosticsStream is the stream a request asks for: stderr, the default, or
// stdout, at most once in a well-formed query.
func diagnosticsStream(raw string) (string, error) {
	q, err := url.ParseQuery(raw)
	switch v := q["stream"]; {
	case err != nil:
	case len(v) == 0:
		return "stderr", nil
	case len(v) == 1 && (v[0] == "stderr" || v[0] == "stdout"):
		return v[0], nil
	}
	return "", &apiError{400, "invalid_stream", "Ask once for the stream stderr or stdout, in a well-formed query."}
}

// diagnosticsHook, when a test sets it, runs at each step of a read: at
// "attempt" between looking at the run's directory and opening it, at
// "stream" between looking at the stream and opening it, and at "read" after
// the opened stream was checked and before its bytes are read.
var diagnosticsHook func(stage string)

func diagnosticsPause(stage string) {
	if diagnosticsHook != nil {
		diagnosticsHook(stage)
	}
}

// errNotRetained is a stream the store does not hold.
var errNotRetained = errors.New("no such stream is retained")

// errNotPrivate is what is at a stream's name, or its directory's, when it is
// not a private file, or directory, of the operator's own, or is not what the
// name held when Runner looked at it. Nothing of it is read.
var errNotPrivate = errors.New("not a private file in the run's own attempt directory")

// openHeldRoot opens a private directory of the store as a root that Runner
// holds: the directory the path names when it is opened, which a later
// rename or link at that path does not change.
func openHeldRoot(path string) (*os.Root, error) {
	looked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !looked.IsDir() {
		return nil, errors.New("runner state must not use symlink directories")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	held, err := root.Stat(".")
	if err == nil && !os.SameFile(looked, held) {
		err = errors.New("runner state changed while it was opened")
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// private says whether an opened file is what its name held when Runner
// looked at it, and is the operator's alone.
func private(looked, held os.FileInfo) bool {
	return os.SameFile(looked, held) && held.Mode().Perm()&0077 == 0 && checkOwner(held) == nil
}

// readStream reads up to limit bytes from the start of a run's stream, and
// says the whole stream's size. It goes through the attempts root Runner
// holds, one name at a time, and serves only the objects it looked at and then
// opened: the run's directory and the stream are each looked at without
// following a link, opened, and held by os.SameFile on the opened descriptor
// to what was looked at, a directory or a regular file private to the
// operator. A name that resolves to any other object when opened is refused,
// and one that vanishes in between is not retained; one that comes back to the
// same object, through a link or otherwise, is that object. The bytes are read
// from the descriptor that was checked, never by name again. A cut never ends
// inside a character.
func (s *Service) readStream(run, stream string, limit int) ([]byte, int64, error) {
	if s.attempts == nil {
		return nil, 0, errors.New("the attempts directory is not held")
	}
	looked, err := s.attempts.Lstat(run)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errNotRetained
	}
	if err != nil {
		return nil, 0, err
	}
	if !looked.IsDir() {
		return nil, 0, errNotPrivate
	}
	diagnosticsPause("attempt")
	dir, err := s.attempts.OpenRoot(run)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errNotRetained
	}
	if err != nil {
		return nil, 0, errNotPrivate
	}
	defer dir.Close()
	if held, err := dir.Stat("."); err != nil || !private(looked, held) {
		return nil, 0, errNotPrivate
	}
	name := "evaluation." + stream
	looked, err = dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errNotRetained
	}
	if err != nil {
		return nil, 0, err
	}
	if !looked.Mode().IsRegular() {
		return nil, 0, errNotPrivate
	}
	diagnosticsPause("stream")
	// Not blocking: a name that has become a pipe is refused, not waited on.
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errNotRetained
	}
	if err != nil {
		return nil, 0, errNotPrivate
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !held.Mode().IsRegular() || !private(looked, held) {
		return nil, 0, errNotPrivate
	}
	diagnosticsPause("read")
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(b)) < held.Size() {
		for back := 1; back <= utf8.UTFMax && back <= len(b); back++ {
			if utf8.RuneStart(b[len(b)-back]) {
				if !utf8.FullRune(b[len(b)-back:]) {
					b = b[:len(b)-back]
				}
				break
			}
		}
	}
	return b, held.Size(), nil
}

// diagnosticsHandler serves a finished run's attempt diagnostics as
// text/plain: the first maxDiagnostics bytes of the stream asked for, with
// X-Diagnostics-Bytes, the stream's whole size, and X-Diagnostics-Truncated.
// A run still queued or running is refused. A run whose stream is not retained
// answers 404 with its state and what the store records, never an inference
// from the missing file; a name that resolves to any object but the run's own
// private directory and file it looked at is refused with 500, unread. A read changes nothing, and writes no entry in the
// journal of job activity.
func (s *Service) diagnosticsHandler(w http.ResponseWriter, r *http.Request) {
	stream, err := diagnosticsStream(r.URL.RawQuery)
	if err != nil {
		failure(w, err)
		return
	}
	run, err := s.run(r.PathValue("run"))
	if err != nil {
		failure(w, err)
		return
	}
	if run.State == "queued" || run.State == "running" {
		failure(w, &apiError{409, "run_not_finished", "A run's diagnostics are served once it has finished."})
		return
	}
	// A run's directory is named by its id, one path element.
	var head []byte
	var size int64
	if run.ID == "" || run.ID == "." || run.ID == ".." || filepath.Base(run.ID) != run.ID {
		err = errNotRetained
	} else {
		head, size, err = s.readStream(run.ID, stream, maxDiagnostics)
	}
	if errors.Is(err, errNotRetained) {
		// Only what the store records: a missing file says nothing of
		// whether the Runtime ran.
		known := ""
		switch {
		case run.Reason == runQueueExpired:
			known = ": it expired in the queue (" + runQueueExpired + ") and never started"
		case run.StartedAt == "":
			known = ": it never started"
		}
		failure(w, &apiError{404, "no_diagnostics", "No " + stream + " is retained for this run, whose state is " + run.State + known + "."})
		return
	}
	if errors.Is(err, errNotPrivate) {
		failure(w, &apiError{500, "diagnostics_refused", "This run's " + stream + " is not a private file in its own attempt directory, so it was not read."})
		return
	}
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Diagnostics-Bytes", strconv.FormatInt(size, 10))
	w.Header().Set("X-Diagnostics-Truncated", strconv.FormatBool(int64(len(head)) < size))
	w.WriteHeader(200)
	w.Write(head)
}
