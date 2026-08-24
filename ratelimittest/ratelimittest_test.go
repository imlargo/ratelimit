package ratelimittest_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/imlargo/ratelimit"
	"github.com/imlargo/ratelimit/ratelimittest"
)

func TestHelpersOnASingleNode(t *testing.T) {
	defer ratelimittest.NoGoroutineLeaks(t)()

	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: ratelimit.FromSubject(),
		Rules:    []ratelimit.Rule{{Name: "r", Quota: ratelimit.PerHour(5), Key: ratelimit.ByIdentity()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	s := ratelimit.Subject{Identity: "u"}
	ratelimittest.AssertQuota(t, lim, s, 5)
	d := ratelimittest.AssertDenied(t, lim, s)
	if d.Reason != ratelimit.ReasonDeniedQuota || d.Rule != "r" {
		t.Errorf("got %s", d)
	}
	ratelimittest.AssertAllowed(t, lim, ratelimit.Subject{Identity: "other"})
}

func TestBackendDrivesDistributedBehaviour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		be := ratelimittest.NewBackend()
		be.Horizon = 2 * time.Second

		mk := func(id string) *ratelimit.Limiter {
			l, err := ratelimit.NewWith(ratelimit.Config{
				Identity:      ratelimit.FromSubject(),
				Rules:         []ratelimit.Rule{{Quota: ratelimit.PerMinute(60).WithBurst(6), Key: ratelimit.ByIdentity()}},
				Backend:       be,
				ClusterKey:    "shared-cluster-secret-for-tests",
				NodeID:        id,
				SyncInterval:  100 * time.Millisecond,
				SyncThreshold: 1e-9,
				Capacity:      1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			return l
		}
		a, b := mk("a"), mk("b")
		defer a.Close()
		defer b.Close()

		s := ratelimit.Subject{Identity: "hot"}
		// Both nodes burst before any coordination.
		na := ratelimittest.Drain(t, a, s)
		nb := ratelimittest.Drain(t, b, s)
		if na+nb != 12 {
			t.Errorf("cold start admitted %d, want 2 nodes times a burst of 6", na+nb)
		}

		time.Sleep(400 * time.Millisecond)
		synctest.Wait()

		if be.Calls() == 0 {
			t.Fatal("the backend was never called")
		}
		if be.Keys() != 1 {
			t.Errorf("the backend was told about %d keys, want 1", be.Keys())
		}

		// Degradation.
		be.Fail(errors.New("boom"))
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		if !a.Degraded() {
			t.Error("a failing backend did not put the limiter into degraded operation")
		}
		d := a.Check(t.Context(), s)
		if !d.Degraded {
			t.Errorf("a decision taken while degraded did not say so: %s", d)
		}

		be.Fail(nil)
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		if a.Degraded() {
			t.Error("still degraded after the backend recovered")
		}
	})
}

func TestLeakDetectorCatchesALeak(t *testing.T) {
	// Prove the detector works, using a deliberately leaked goroutine.
	sub := &testing.T{}
	stop := make(chan struct{})
	check := ratelimittest.NoGoroutineLeaks(sub)
	go func() { <-stop }()
	check()
	close(stop)
	if !sub.Failed() {
		t.Error("the goroutine leak detector did not notice a leaked goroutine")
	}
}
