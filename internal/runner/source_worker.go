package runner

// SourceWorker is a durable caller of Gateway's ordinary /acquire API. Its
// private state is caller custody, never part of Gateway's receipt store. It
// holds no signing seed and must run independently from an individual Runner.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type SourceWorkerConfig struct{ Dir, Gateway, GatewayTokenFile string }
type sourceRequest struct {
	ID        string          `json:"id"`
	Gateway   string          `json:"gateway"`
	Source    string          `json:"source"`
	Arguments json.RawMessage `json:"arguments"`
	Deadline  string          `json:"deadline"`
}
type sourceStatus struct {
	ID       string          `json:"id"`
	State    string          `json:"state"`
	Deadline string          `json:"deadline"`
	Response json.RawMessage `json:"response,omitempty"`
	Reason   string          `json:"reason,omitempty"`
}
type SourceWorker struct {
	cfg          SourceWorkerConfig
	db           *sql.DB
	lock         *os.File
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	active       map[string]context.CancelFunc
	workers      sync.WaitGroup
	closed       bool
	token        string
	gatewayToken string
	beforeRetain func() // test barrier after reception, before committing completion
}

var sourceOperationID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var readSourceName = regexp.MustCompile(`^[a-zA-Z0-9._:/-]{1,128}$`)

func validGatewayOrigin(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && !u.ForceQuery && u.Opaque == "" && (u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))
}
func readPrivateToken(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("absolute private token file required")
	}
	if e := privateFile(path); e != nil {
		return "", e
	}
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1025))
	if e != nil || len(b) > 1024 {
		return "", errors.New("invalid token file")
	}
	value := strings.TrimSpace(string(b))
	if len(value) < 32 || strings.ContainsAny(value, "\r\n\t ") {
		return "", errors.New("invalid token file")
	}
	return value, nil
}
func OpenSourceWorker(cfg SourceWorkerConfig) (_ *SourceWorker, err error) {
	if !filepath.IsAbs(cfg.Dir) || !validGatewayOrigin(cfg.Gateway) {
		return nil, errors.New("absolute private worker directory and Gateway origin required")
	}
	if err = privateDir(cfg.Dir); err != nil {
		return nil, err
	}
	// A worker directory is deliberately not a signer store or a child of one.
	for parent := cfg.Dir; ; parent = filepath.Dir(parent) {
		if st, e := os.Stat(filepath.Join(parent, "receipts")); e == nil && st.IsDir() {
			return nil, errors.New("source worker state must be outside the Gateway receipt store")
		}
		if next := filepath.Dir(parent); next == parent {
			break
		}
	}
	s := &SourceWorker{cfg: cfg, active: map[string]context.CancelFunc{}}
	s.lock, err = instanceLock(filepath.Join(cfg.Dir, "source-worker.lock"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if s.db != nil {
				s.db.Close()
			}
			s.lock.Close()
		}
	}()
	tokenPath := filepath.Join(cfg.Dir, "source-worker.token")
	if _, e := os.Lstat(tokenPath); os.IsNotExist(e) {
		var b [32]byte
		if _, err = rand.Read(b[:]); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(tokenPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.WriteString(hex.EncodeToString(b[:]) + "\n")
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if s.token, err = readPrivateToken(tokenPath); err != nil {
		return nil, err
	}
	if cfg.GatewayTokenFile != "" {
		if s.gatewayToken, err = readPrivateToken(cfg.GatewayTokenFile); err != nil {
			return nil, err
		}
	}
	dbpath := filepath.Join(cfg.Dir, "sources.sqlite")
	f, e := os.OpenFile(dbpath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e == nil {
		f.Close()
	} else if !os.IsExist(e) {
		return nil, e
	}
	if err = privateFile(dbpath); err != nil {
		return nil, err
	}
	if s.db, err = sql.Open("sqlite", dbpath); err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	_, err = s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS worker_metadata (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS source_operations (id TEXT PRIMARY KEY,request TEXT NOT NULL,state TEXT NOT NULL,status TEXT NOT NULL);`)
	if err != nil {
		return nil, err
	}
	if _, err = s.db.Exec("INSERT OR IGNORE INTO worker_metadata VALUES ('gateway',?)", cfg.Gateway); err != nil {
		return nil, err
	}
	var gateway string
	if err = s.db.QueryRow("SELECT value FROM worker_metadata WHERE key='gateway'").Scan(&gateway); err != nil {
		return nil, err
	}
	if gateway != cfg.Gateway {
		return nil, errors.New("worker store belongs to a different Gateway")
	}
	// A durable claim is never reassigned after a process disappears.
	_, err = s.db.Exec(`UPDATE source_operations SET state='needs-attention',status=json_set(status,'$.state','needs-attention','$.reason','The acquisition worker stopped before retaining the response. The source was not called again.') WHERE state='running'`)
	if err != nil {
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}
func (s *SourceWorker) TokenFile() string { return filepath.Join(s.cfg.Dir, "source-worker.token") }
func (s *SourceWorker) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
	s.db.Close()
	s.lock.Close()
}
func (s *SourceWorker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
			write(w, 401, map[string]string{"error": "source worker authorization required"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			write(w, 200, map[string]string{"protocol": "source-worker/1"})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/operations" {
			raw, e := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
			var req sourceRequest
			if e != nil || len(raw) > MaxBody || strictJSON(raw, &req) != nil {
				write(w, 400, map[string]string{"error": "invalid operation request"})
				return
			}
			status, code, e := s.submit(req)
			if e != nil {
				write(w, code, map[string]string{"error": e.Error()})
				return
			}
			write(w, code, status)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/operations/")
		cancel := strings.HasSuffix(id, "/cancel")
		id = strings.TrimSuffix(id, "/cancel")
		if !strings.HasPrefix(r.URL.Path, "/operations/") || !sourceOperationID.MatchString(id) || cancel && r.Method != http.MethodPost || !cancel && r.Method != http.MethodGet {
			write(w, 404, map[string]string{"error": "not found"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		status, e := s.status(id)
		if e != nil {
			write(w, 404, map[string]string{"error": "operation not found"})
			return
		}
		if cancel && (status.State == "running" || status.State == "queued" || status.State == "needs-attention") {
			status.State = "cancelled"
			status.Response = nil
			status.Reason = "Cancelled locally; the provider may still finish."
			if _, e = s.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=?", status.State, string(encode(status)), id); e != nil {
				write(w, 500, map[string]string{"error": "cancellation could not be retained"})
				return
			}
			if stop := s.active[id]; stop != nil {
				stop()
			}
		}
		write(w, 200, status)
	})
}
func (s *SourceWorker) status(id string) (sourceStatus, error) {
	var raw string
	var status sourceStatus
	e := s.db.QueryRow("SELECT status FROM source_operations WHERE id=?", id).Scan(&raw)
	if e == nil {
		e = json.Unmarshal([]byte(raw), &status)
		if e != nil {
			return status, e
		}
		previous := status.State
		if status.State == "running" && s.active[id] == nil {
			status.State = "needs-attention"
			status.Reason = "The source response was not retained. The source will not be called again."
		}
		if status.State == "queued" || status.State == "running" || status.State == "needs-attention" {
			if deadline, err := time.Parse(time.RFC3339Nano, status.Deadline); err == nil && !time.Now().Before(deadline) {
				status.State = "expired"
				status.Reason = "The source deadline elapsed; remote completion may still occur."
			}
		}
		if status.State != previous {
			status.Response = nil
			if _, e = s.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=?", status.State, string(encode(status)), id); e != nil {
				return sourceStatus{}, e
			}
			if status.State == "expired" {
				if stop := s.active[id]; stop != nil {
					stop()
				}
			}
		}
	}
	return status, e
}
func (s *SourceWorker) submit(req sourceRequest) (sourceStatus, int, error) {
	if !sourceOperationID.MatchString(req.ID) || (!readSourceName.MatchString(req.Source) || strings.HasSuffix(req.Source, "/write")) || req.Gateway != s.cfg.Gateway || !json.Valid(req.Arguments) {
		return sourceStatus{}, 400, errors.New("invalid operation binding")
	}
	deadline, e := time.Parse(time.RFC3339Nano, req.Deadline)
	if e != nil {
		return sourceStatus{}, 400, errors.New("invalid operation deadline")
	}
	var compact bytes.Buffer
	if e = json.Compact(&compact, req.Arguments); e != nil {
		return sourceStatus{}, 400, e
	}
	req.Arguments = compact.Bytes()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sourceStatus{}, 503, errors.New("source worker is stopping")
	}
	var held string
	e = s.db.QueryRow("SELECT request FROM source_operations WHERE id=?", req.ID).Scan(&held)
	if e == nil {
		if held != string(encode(req)) {
			return sourceStatus{}, 409, errors.New("operation ID already names another request")
		}
	} else if errors.Is(e, sql.ErrNoRows) {
		if !time.Now().Before(deadline) || time.Until(deadline) > 7*24*time.Hour {
			return sourceStatus{}, 400, errors.New("deadline must be within the next seven days")
		}
		var count int
		if e = s.db.QueryRow("SELECT count(*) FROM source_operations").Scan(&count); e != nil {
			return sourceStatus{}, 500, e
		}
		if count >= 10000 {
			return sourceStatus{}, 507, errors.New("worker operation capacity reached; retain existing records for reconciliation")
		}
		status := sourceStatus{ID: req.ID, State: "queued", Deadline: req.Deadline}
		if _, e = s.db.Exec("INSERT INTO source_operations VALUES (?,?,?,?)", req.ID, string(encode(req)), status.State, string(encode(status))); e != nil {
			return sourceStatus{}, 500, e
		}
	} else {
		return sourceStatus{}, 500, e
	}
	status, e := s.status(req.ID)
	if e != nil {
		return status, 500, e
	}
	if status.State == "queued" && len(s.active) < 4 {
		status.State = "running"
		// SQLite FULL/WAL commits the claim before the first external request.
		if _, e = s.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=? AND state='queued'", status.State, string(encode(status)), req.ID); e != nil {
			return status, 500, e
		}
		ctx, stop := context.WithDeadline(s.ctx, deadline)
		s.active[req.ID] = stop
		s.workers.Add(1)
		go s.acquire(ctx, stop, req)
	}
	code := 200
	if status.State == "queued" || status.State == "running" {
		code = 202
	}
	return status, code, nil
}
func (s *SourceWorker) acquire(ctx context.Context, stop context.CancelFunc, req sourceRequest) {
	defer s.workers.Done()
	defer stop()
	raw, e := s.call(ctx, "/acquire", encode(map[string]any{"session": "async." + req.ID, "source": req.Source, "arguments": req.Arguments}), MaxBody)
	status := sourceStatus{ID: req.ID, State: "completed", Deadline: req.Deadline}
	if e != nil || ctx.Err() != nil {
		status.State = "needs-attention"
		status.Reason = "Source acquisition did not complete verifiably. It was not repeated automatically."
		if deadline, _ := time.Parse(time.RFC3339Nano, req.Deadline); !time.Now().Before(deadline) {
			status.State = "expired"
			status.Reason = "The source deadline elapsed; remote completion may still occur."
		}
	} else {
		var proof struct {
			Receipt struct {
				Session string `json:"sessionId"`
				Index   *int   `json:"callIndex"`
			} `json:"receipt"`
		}
		if json.Unmarshal(raw, &proof) != nil || proof.Receipt.Session != "async."+req.ID || (proof.Receipt.Index == nil || *proof.Receipt.Index != 0) {
			status.State = "needs-attention"
			status.Reason = "The Gateway response does not belong to this operation's first acquisition. It was not repeated."
		} else {
			status.Response = raw
		}
	}
	if s.beforeRetain != nil {
		s.beforeRetain()
	}
	s.mu.Lock()
	if deadline, _ := time.Parse(time.RFC3339Nano, req.Deadline); !time.Now().Before(deadline) {
		status.State = "expired"
		status.Response = nil
		status.Reason = "The source deadline elapsed; remote completion may still occur."
	}
	// Cancellation/expiry wins even when a response was already in transit.
	_, _ = s.db.Exec("UPDATE source_operations SET state=?,status=? WHERE id=? AND state='running'", status.State, string(encode(status)), req.ID)
	delete(s.active, req.ID)
	s.mu.Unlock()
	// Sealing cannot keep a preparation worker occupied indefinitely.
	cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s.call(cleanup, "/seal", encode(map[string]string{"session": "async." + req.ID}), 64<<10)
}
func (s *SourceWorker) call(ctx context.Context, path string, body []byte, limit int64) ([]byte, error) {
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Gateway+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	request.Header.Set("Content-Type", "application/json")
	if s.gatewayToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.gatewayToken)
	}
	// The process, not a browser or Runner request, owns this long connection.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Gateway redirect refused") }}
	defer client.CloseIdleConnections()
	response, e := client.Do(request)
	if e != nil {
		return nil, e
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if e != nil || int64(len(raw)) > limit {
		return nil, errors.New("Gateway response exceeded its limit")
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Gateway returned status %d", response.StatusCode)
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid Gateway response")
	}
	return raw, nil
}
