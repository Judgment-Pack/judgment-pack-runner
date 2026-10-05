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

// readHead reads up to limit bytes from the start of a private attempt file,
// and says how large the whole file is. A cut never ends inside a character.
func readHead(path string, limit int) ([]byte, int64, error) {
	if err := privateFile(path); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(b)) < st.Size() {
		for back := 1; back <= utf8.UTFMax && back <= len(b); back++ {
			if utf8.RuneStart(b[len(b)-back]) {
				if !utf8.FullRune(b[len(b)-back:]) {
					b = b[:len(b)-back]
				}
				break
			}
		}
	}
	return b, st.Size(), nil
}

// diagnosticsHandler serves a finished run's attempt diagnostics as
// text/plain: the first maxDiagnostics bytes of the stream asked for, with
// X-Diagnostics-Bytes, the stream's whole size, and X-Diagnostics-Truncated.
// A run still queued or running is refused; one that kept no such stream
// answers 404 with the reason. A read changes nothing, and writes no entry in
// the journal of job activity.
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
	none := func(reason string) { failure(w, &apiError{404, "no_diagnostics", reason}) }
	if run.ID == "" || run.ID == "." || run.ID == ".." || filepath.Base(run.ID) != run.ID {
		none("This run kept no diagnostics.")
		return
	}
	attempt := filepath.Join(s.cfg.Dir, "attempts", run.ID)
	if _, err = os.Lstat(attempt); errors.Is(err, fs.ErrNotExist) {
		if run.Reason == runQueueExpired {
			none("This run expired in the queue and was never evaluated, so it kept no diagnostics.")
		} else {
			none("This run was never evaluated, so it kept no diagnostics.")
		}
		return
	}
	head, size, err := readHead(filepath.Join(attempt, "evaluation."+stream), maxDiagnostics)
	if errors.Is(err, fs.ErrNotExist) {
		none("This run's evaluation kept no " + stream + ": the Runtime was not invoked.")
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
