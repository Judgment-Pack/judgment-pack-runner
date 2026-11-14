package runner

// The postgresql kind's time bounds (pgBounds, updateWithin;
// docs/postgresql-store.md, "Time bounds"), on the server named to the test
// (JPACK_TEST_POSTGRESQL): a write that meets journal_head's lock held by
// another connection is refused within the lock bound, a renewal within its
// deadline, and a session left idle inside a transaction is ended by the
// server. Nothing here waits on a lock longer than a bound it sets.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// shortPGBounds makes pgBounds short for one test: the pools opened after it
// hold them.
func shortPGBounds(t *testing.T, lock, statement, idle time.Duration) {
	was := pgBounds
	pgBounds.lock, pgBounds.statement, pgBounds.idle = lock, statement, idle
	t.Cleanup(func() { pgBounds = was })
}

// pgHoldJournal takes journal_head's lock on a connection of the suite's
// login opened without the kind's bounds, as another writer holds it, and
// returns the function that ends the hold (also run after the test).
func pgHoldJournal(t *testing.T, s *pgTestServer, db string) func() {
	t.Helper()
	cfg, err := pgx.ParseConfig(s.url(pgRoleSuite, db))
	must(t, err)
	conn := stdlib.OpenDB(*cfg)
	tx, err := conn.BeginTx(pgContext, nil)
	must(t, err)
	if _, err = tx.ExecContext(pgContext, "LOCK TABLE jpack_runner.journal_head IN EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	end := func() {
		once.Do(func() {
			tx.Rollback()
			conn.Close()
		})
	}
	t.Cleanup(end)
	return end
}

// pgWaiting is the number of the login's server processes on db waiting
// for a lock.
func pgWaiting(t *testing.T, s *pgTestServer, db, login string) int {
	t.Helper()
	var n int
	must(t, s.admin.QueryRowContext(pgContext, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
WHERE NOT l.granted AND a.datname=$1 AND a.usename=$2`, db, s.role(login)).Scan(&n))
	return n
}

// waitWaiting waits, up to five seconds, until pgWaiting is want.
func waitWaiting(t *testing.T, s *pgTestServer, db, login string, want int, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		got := pgWaiting(t, s, db, login)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d of the %s login's processes wait for a lock, not %d", what, got, login, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// within fails the test when took is more than bound and the slack a
// loaded machine needs.
func within(t *testing.T, took, bound time.Duration, what string) {
	t.Helper()
	if took > bound+1500*time.Millisecond {
		t.Errorf("%s took %s, beyond its bound of %s", what, took, bound)
	}
}

// Every pooled connection of the kind holds the three bounds, whatever its
// connection string says.
func TestPostgreSQLHoldsEveryConnectionToItsTimeBounds(t *testing.T) {
	s, db, _, real := pgConditionWorld(t)
	u, err := url.Parse(s.url(pgRoleWorker, db))
	must(t, err)
	q := u.Query()
	for _, name := range []string{"lock_timeout", "statement_timeout", "idle_in_transaction_session_timeout"} {
		q.Set(name, "0")
	}
	u.RawQuery = q.Encode()
	named, closeNamed, err := pgOpenPool(u.String())
	must(t, err)
	t.Cleanup(func() {
		named.Close()
		closeNamed()
	})
	for login, pool := range map[string]*sql.DB{"request": real.p.request, "worker": real.p.worker, "worker, its connection string naming the bounds": named} {
		for name, want := range map[string]time.Duration{"lock_timeout": pgBounds.lock, "statement_timeout": pgBounds.statement, "idle_in_transaction_session_timeout": pgBounds.idle} {
			var same bool
			if err := pool.QueryRowContext(pgContext, "SELECT current_setting($1)::interval = make_interval(secs => $2)", name, want.Seconds()).Scan(&same); err != nil {
				t.Fatal(login, name, err)
			}
			if !same {
				t.Errorf("the %s login's connection does not hold %s at %s", login, name, want)
			}
		}
	}
	for code, want := range map[string]error{"55P03": errPostgreSQLLockWait, "57014": errPostgreSQLStatementTime, "25P03": errPostgreSQLIdleSession} {
		err := pgFault("write", &pgconn.PgError{Code: code}, true)
		if !errors.Is(err, want) || faultOf(err) != faultDurability {
			t.Errorf("SQLSTATE %s is %v (%s), not %v", code, err, faultOf(err), want)
		}
		if err = pgFault("read", &pgconn.PgError{Code: code}, false); !errors.Is(err, want) || faultOf(err) != faultUnavailable {
			t.Errorf("SQLSTATE %s on a read is %v (%s), not %v", code, err, faultOf(err), want)
		}
	}
}

// A write that meets journal_head's lock held by another connection is
// refused within the lock bound, and a renewal that meets it is a lost
// claim: the attempt's context ends and its release reports the loss.
func TestPostgreSQLRefusesAWriteThatWaitsOnAHeldLock(t *testing.T) {
	shortPGBounds(t, 300*time.Millisecond, 5*time.Second, 30*time.Second)
	shortClaimTimes(t, time.Second, 20*time.Second)
	s, db, w, real := pgConditionWorld(t)
	claimed := time.Now()
	r, ok, err := claimRun(real, w.runs["a"].ID)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	pgHoldJournal(t, s, db)
	began := time.Now()
	err = real.update(func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) })
	if !errors.Is(err, errPostgreSQLLockWait) || faultOf(err) != faultDurability {
		t.Fatal("a write that met a held lock was not refused for it:", err)
	}
	within(t, time.Since(began), pgBounds.lock, "a write that met a held lock")
	held, release := (&Service{store: real}).holdClaim(context.Background(), r.ID, r.claim, claimed)
	select {
	case <-held.Done():
	case <-time.After(claimTimes.renewal + pgBounds.lock + 3*time.Second):
		t.Fatal("a renewal that met a held lock did not end the attempt's context")
	}
	within(t, time.Since(claimed), claimTimes.renewal+pgBounds.lock, "a renewal that met a held lock")
	if err := release(); !errors.Is(err, errClaimLost) {
		t.Fatal("a renewal that met a held lock was not reported as a lost claim:", err)
	}
	waitWaiting(t, s, db, pgRoleWorker, 0, "after the refusals")
}

// A renewal still waiting on a held lock at its deadline (a lock bound
// longer than the renewal's interval) is refused at the deadline and its
// wait on the server cancelled; the claim is lost, and a release while one
// waits returns within the deadline.
func TestPostgreSQLHoldsARenewalToItsDeadline(t *testing.T) {
	shortPGBounds(t, 30*time.Second, 60*time.Second, 60*time.Second)
	shortClaimTimes(t, 400*time.Millisecond, 20*time.Second)
	s, db, w, real := pgConditionWorld(t)
	claimed := time.Now()
	r, ok, err := claimRun(real, w.runs["a"].ID)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	pgHoldJournal(t, s, db)
	renew := func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }
	began := time.Now()
	err = updateWithin(real, claimTimes.renewal, renew)
	if !errors.Is(err, errPostgreSQLDeadline) || faultOf(err) != faultDurability {
		t.Fatal("a renewal past its deadline was not refused for it:", err)
	}
	within(t, time.Since(began), claimTimes.renewal, "a renewal past its deadline")
	waitWaiting(t, s, db, pgRoleWorker, 0, "a renewal refused at its deadline")

	held, release := (&Service{store: real}).holdClaim(context.Background(), r.ID, r.claim, claimed)
	select {
	case <-held.Done():
	case <-time.After(2*claimTimes.renewal + 3*time.Second):
		t.Fatal("a renewal past its deadline did not end the attempt's context")
	}
	if err := release(); !errors.Is(err, errClaimLost) {
		t.Fatal("a renewal past its deadline was not reported as a lost claim:", err)
	}
	waitWaiting(t, s, db, pgRoleWorker, 0, "a lost claim's renewal")

	// The attempt ends while a renewal waits: its release waits for that
	// renewal no longer than its deadline.
	_, release = (&Service{store: real}).holdClaim(context.Background(), r.ID, r.claim, time.Now())
	waitWaiting(t, s, db, pgRoleWorker, 1, "a renewal behind a held lock")
	began = time.Now()
	release()
	within(t, time.Since(began), claimTimes.renewal, "a release while a renewal waits")
	waitWaiting(t, s, db, pgRoleWorker, 0, "a release while a renewal waits")
}

// A transaction of the kind left idle longer than the idle bound is ended
// by the server, which releases journal_head's lock for the writer waiting
// behind it, and the stalled one is refused for it.
func TestPostgreSQLEndsATransactionLeftIdle(t *testing.T) {
	shortPGBounds(t, 3*time.Second, 5*time.Second, 300*time.Millisecond)
	s, db, w, stalled := pgConditionWorld(t)
	r, ok, err := claimRun(stalled, w.runs["a"].ID)
	if err != nil || !ok {
		t.Fatal("the run was not claimed:", ok, err)
	}
	waiting := newPostgreSQLStore(s.pool(t, pgRoleRequest, db), s.pool(t, pgRoleWorker, db))
	must(t, waiting.start(pgTestOpening))
	renew := func(tx runTx) error { return tx.renewClaim(r.ID, r.claim) }
	inside, ended := make(chan struct{}), make(chan error, 1)
	go func() {
		var once sync.Once
		ended <- stalled.update(func(tx runTx) error {
			once.Do(func() { close(inside) })
			time.Sleep(2 * time.Second)
			return renew(tx)
		})
	}()
	<-inside
	began := time.Now()
	must(t, waiting.update(renew))
	within(t, time.Since(began), pgBounds.idle, "a write behind a transaction left idle")
	select {
	case err := <-ended:
		t.Fatal("the stalled transaction ended before the write behind it:", err)
	default:
	}
	if err := <-ended; !errors.Is(err, errPostgreSQLIdleSession) {
		t.Fatal("a transaction left idle was not refused for it:", err)
	}
}
