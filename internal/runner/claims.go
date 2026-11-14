package runner

// A claim on a kind that holds it only for its lease
// (docs/design/store-postgresql.md, D3, "The margin"): its holder renews it
// while the run's attempt goes on, stops the Runtime well before the lease
// the store holds can run out when a renewal does not succeed, and writes
// nothing more of the run once one has failed. The sqlite kind's lock is its
// fence: it leases nothing and renews nothing.

import (
	"context"
	"errors"
	"time"
)

// leasingStore is a kind whose claims hold only for their lease, renewed by
// their holder (D3).
type leasingStore interface {
	runStore
	leasesClaims() bool
}

// leases says whether st holds a claim only for its lease.
func leases(st runStore) bool {
	l, ok := st.(leasingStore)
	return ok && l.leasesClaims()
}

// claimTimes are how often a holder renews its claim, and how long after the
// start of the last claim or renewal that succeeded it begins stopping the
// run's Runtime when none since has: half the lease, so the Runtime is being
// stopped at least claimLease/2 before the lease the store holds can run out,
// whatever the Runner's wall clock says (the store's lease runs from a
// now() after that start). Tests shorten them.
var claimTimes = struct{ renewal, stop time.Duration }{claimRenewal, 2 * claimRenewal}

// errClaimLost is the end of an attempt whose claim was not renewed: the
// renewal failed, met another claim or a lease that ran out, took longer than
// the renewal's interval, or none succeeded before the stop. Its claimant
// writes nothing more of the run, and its dispatcher stops, as on any storage
// error: the run is interrupted once its lease runs out (interruptLapsed).
var errClaimLost = errors.New("the dispatcher's claim of its run was not renewed: it wrote nothing more of the run and stopped")

// holdClaim holds the claim of run, made at claimed (by the monotonic clock,
// before the claim's transaction began), while its attempt goes on. On a kind
// that leases claims, the context it returns is cancelled, stopping the
// attempt's Runtime, when a renewal fails or claimTimes.stop passes after the
// start of the last that succeeded; release ends the renewals, waiting for
// one in flight so that none is made after it, and returns errClaimLost when
// the claim was lost. On any other kind it returns ctx and a release that
// renews and returns nothing.
func (s *Service) holdClaim(ctx context.Context, run string, claim runClaim, claimed time.Time) (context.Context, func() error) {
	if !leases(s.store) {
		return ctx, func() error { return nil }
	}
	held, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	lost := false
	go func() {
		defer close(done)
		stop := time.NewTimer(time.Until(claimed.Add(claimTimes.stop)))
		defer stop.Stop()
		next := time.NewTimer(time.Until(claimed.Add(claimTimes.renewal)))
		defer next.Stop()
		var renewing chan error
		var began time.Time
		for {
			select {
			case <-held.Done():
				if renewing != nil {
					<-renewing
				}
				return
			case <-stop.C:
				lost = true
				cancel()
			case <-next.C:
				if held.Err() != nil {
					continue
				}
				began = time.Now()
				renewing = make(chan error, 1)
				go func(result chan<- error) {
					result <- s.store.update(func(tx runTx) error { return tx.renewClaim(run, claim) })
				}(renewing)
			case err := <-renewing:
				renewing = nil
				if err != nil || time.Since(began) > claimTimes.renewal {
					lost = true
					cancel()
					continue
				}
				stop.Reset(time.Until(began.Add(claimTimes.stop)))
				next.Reset(time.Until(began.Add(claimTimes.renewal)))
			}
		}
	}()
	return held, func() error {
		cancel()
		<-done
		if lost {
			return errClaimLost
		}
		return nil
	}
}

// interruptLapsed is the dispatcher's tick on a kind that leases claims: each
// run held running whose lease ran out, which no live claim holds, is
// interrupted, as the restart interrupts it (D3). A run whose lease is live
// is its holder's and is left as it is.
func (s *Service) interruptLapsed() error {
	return s.store.update(func(tx runTx) error {
		return interruptRunning(tx, now(), func(r Run) EventDetail {
			return EventDetail{Interruption: &EventInterruption{LastKnownRunning: r.StartedAt}}
		})
	})
}
