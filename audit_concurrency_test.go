// Adversarial tests for the properties this package claims about concurrency
// and about arithmetic on caller-supplied values. Each one here exists because
// the property is either load-dependent or easy to get quietly wrong, and a
// reviewer cannot see either by reading.

package ratelimit

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hammer the claim and evict paths concurrently with the race detector on, while
// the clock advances so cells keep recovering and being recycled. This is the
// path with a lock and a lock-free reader looking at the same cells.
func TestAuditStoreChurnUnderRace(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerSecond(4), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 256, // tiny, so eviction and saturation both happen constantly
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	var stop atomic.Bool
	var wg sync.WaitGroup

	// A goroutine advancing the clock, so cells recover and get recycled.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			clk.Advance(int64(50 * time.Millisecond))
			runtime.Gosched()
		}
	}()

	var seen [8]atomic.Int64
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; !stop.Load(); i++ {
				d := lim.Check(ctx, Subject{Identity: "k" + itoa((w*31+i)%2000)})
				switch d.Reason {
				case ReasonAllowed:
					seen[0].Add(1)
				case ReasonDeniedQuota:
					seen[1].Add(1)
				case ReasonStoreSaturated:
					seen[2].Add(1)
				default:
					seen[3].Add(1)
				}
			}
		}(w)
	}
	time.Sleep(300 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	st := lim.Stats()
	t.Logf("allowed=%d denied=%d saturated=%d other=%d | evictions=%d saturations=%d occupied=%d/%d",
		seen[0].Load(), seen[1].Load(), seen[2].Load(), seen[3].Load(),
		st.Evictions, st.Saturations, st.Occupied, st.Capacity)
	if st.Evictions == 0 {
		t.Error("the eviction path was never exercised; the test proves nothing")
	}
	if st.Occupied > st.Capacity {
		t.Errorf("occupied %d exceeds capacity %d", st.Occupied, st.Capacity)
	}
	if seen[3].Load() != 0 {
		t.Errorf("%d decisions had an unexpected reason", seen[3].Load())
	}
}

// Close while requests are in flight, with a backend, repeatedly.
func TestAuditCloseUnderTraffic(t *testing.T) {
	for round := 0; round < 20; round++ {
		be := &flakyBackend{}
		lim, err := NewWith(Config{
			Rules:         []Rule{{Quota: PerSecond(1000), Key: ByIdentity()}},
			Identity:      FromSubject(),
			Backend:       be,
			ClusterKey:    "audit-cluster-key-0123456789",
			SyncInterval:  time.Millisecond,
			SyncThreshold: 1e-9,
			Capacity:      512,
		})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var stop atomic.Bool
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				ctx := context.Background()
				for i := 0; !stop.Load(); i++ {
					lim.Check(ctx, Subject{Identity: "k" + itoa(i%50)})
				}
			}(w)
		}
		time.Sleep(2 * time.Millisecond)

		start := time.Now()
		if err := lim.Close(); err != nil {
			t.Fatalf("round %d: Close: %v", round, err)
		}
		took := time.Since(start)
		if took > 500*time.Millisecond {
			t.Errorf("round %d: Close took %v; it must not block on an in-flight backend call", round, took)
		}

		// Requests must keep working after Close rather than panicking.
		for i := 0; i < 100; i++ {
			lim.Check(context.Background(), Subject{Identity: "after-close"})
		}
		stop.Store(true)
		wg.Wait()
	}
}

type flakyBackend struct {
	n      atomic.Int64
	closed atomic.Bool
}

func (b *flakyBackend) Close() error { b.closed.Store(true); return nil }

func (b *flakyBackend) Sync(ctx context.Context, node string, d []Demand) ([]Share, error) {
	if b.n.Add(1)%3 == 0 {
		return nil, errors.New("flaky")
	}
	// Block a little, so Close can catch a call in flight.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Millisecond):
	}
	out := make([]Share, len(d))
	for i := range d {
		out[i] = Share{Key: d[i].Key, Others: time.Duration(i), Nodes: 1}
	}
	return out, nil
}

// A metrics callback that blocks would block the decision. Verify it is the
// caller's own foot and nothing deadlocks internally.
func TestAuditSlowMetricsDoNotDeadlock(t *testing.T) {
	var n atomic.Int64
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerSecond(1000), Key: ByIdentity()}},
		Identity: FromSubject(),
		Metrics: Metrics{
			Decision: func(Reason, string) { n.Add(1); runtime.Gosched() },
			Denied:   func(string) { runtime.Gosched() },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 2000; i++ {
					lim.Check(context.Background(), Subject{Identity: "u"})
				}
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlocked with metrics callbacks configured")
	}
	if n.Load() != 16000 {
		t.Errorf("the decision metric fired %d times, want 16000", n.Load())
	}
}

// The three fixes, as tests.

// TestEvictionMetricFires. It counted evictions and told nobody, which is a
// documented metric that does not exist.
func TestEvictionMetricFires(t *testing.T) {
	var evicted atomic.Int64
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerSecond(2), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 128,
		Metrics:  Metrics{Evicted: func() { evicted.Add(1) }},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for round := 0; round < 20; round++ {
		for i := 0; i < 200; i++ {
			lim.Check(ctx, Subject{Identity: "r" + itoa(round) + "-k" + itoa(i)})
		}
		clk.Advance(int64(2 * time.Second)) // everything recovers, so cells are recyclable
	}
	st := lim.Stats()
	if st.Evictions == 0 {
		t.Fatal("no evictions happened; the test proves nothing")
	}
	if evicted.Load() != st.Evictions {
		t.Errorf("the Evicted metric fired %d times for %d evictions", evicted.Load(), st.Evictions)
	}
	t.Logf("%d evictions, %d metric calls", st.Evictions, evicted.Load())
}

// TestCostArithmeticSaturates. Wrapping turned a nonsense cost into a small one
// and charged for it: a wrong answer, given quietly.
func TestCostArithmeticSaturates(t *testing.T) {
	lim, err := NewWith(Config{
		Rules:    []Rule{{Name: "weighted", Quota: PerMinute(100), Key: ByIdentity(), Cost: 20}},
		Identity: FromSubject(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for _, cost := range []int64{math.MaxInt64, math.MaxInt64/20 + 1, math.MaxInt64 / 2} {
		d := lim.Check(ctx, Subject{Identity: "u", Cost: cost})
		if d.Allowed {
			t.Errorf("cost %d was admitted", cost)
		}
		if d.Reason != ReasonCostExceedsBurst {
			t.Errorf("cost %d gave reason %v, want ReasonCostExceedsBurst; it used to wrap and be charged as 1",
				cost, d.Reason)
		}
	}
	// A sane cost still works: 20 x 5 = 100, exactly the quota.
	if d := lim.Check(ctx, Subject{Identity: "v", Cost: 5}); !d.Allowed {
		t.Errorf("a cost of 5 against Cost:20 and a quota of 100 should be admitted: %s", d)
	}
	if d := lim.Check(ctx, Subject{Identity: "v", Cost: 1}); d.Allowed {
		t.Error("the quota should now be spent")
	}
}

func TestMulCostSaturates(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{1, 1, 1}, {20, 5, 100}, {0, 5, 1}, {5, 0, 1}, {-1, 5, 1}, {5, -1, 1},
		{math.MaxInt64, 2, math.MaxInt64},
		{2, math.MaxInt64, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{1 << 31, 1 << 31, 1 << 62}, // fits, so it must not saturate
		{1 << 32, 1 << 32, math.MaxInt64},
		{1 << 62, 4, math.MaxInt64},
	}
	for _, c := range cases {
		if got := mulCost(c.a, c.b); got != c.want {
			t.Errorf("mulCost(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
