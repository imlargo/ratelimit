package ratelimit

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNoOverAdmissionUnderContention is the test golang.org/x/time/rate does
// not pass.
//
// In that package Allow reads the clock before taking the limiter's lock, so
// under contention advance is handed an instant in the past, rewinds the
// limiter's state and hands out extra tokens. The published reproducers exceed
// the configured rate by up to 5000x (golang/go#71360, #45245, #65508).
//
// The invariant here makes that impossible rather than unlikely: the value
// written to a cell is always max(stored, now) + increment, which is strictly
// greater than the value read, so a stale clock can only ever make a decision
// stricter. There is no path that moves a cell backwards.
//
// The window is long enough that no quota is replenished during the test, so
// the expected total is exact and any excess is over-admission.
func TestNoOverAdmissionUnderContention(t *testing.T) {
	const (
		limit   = 1000
		workers = 512
		each    = 40
	)
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(limit), Key: ByIdentity()}},
		Identity: IdentityFromSubject,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	ctx := context.Background()
	s := Subject{Identity: "one-hot-key"}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < each; i++ {
				if lim.Check(ctx, s).Allowed {
					admitted.Add(1)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	got := admitted.Load()
	if got != limit {
		t.Errorf("%d goroutines racing on one key admitted %d events for a quota of %d; "+
			"over-admission under contention is exactly the bug this package exists not to have",
			workers, got, limit)
	}
	t.Logf("%d goroutines, %d attempts, admitted %d of %d", workers, workers*each, got, limit)
}

// TestNoOverAdmissionAcrossManyKeys does the same with contention spread over
// keys, which also exercises claim and eviction under the partition lock.
func TestNoOverAdmissionAcrossManyKeys(t *testing.T) {
	const (
		limit   = 20
		keys    = 200
		workers = 128
	)
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(limit), Key: ByIdentity()}},
		Identity: IdentityFromSubject,
		Capacity: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	var counts [keys]atomic.Int64
	var wg sync.WaitGroup
	ctx := context.Background()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < keys*limit; i++ {
				k := (w*7 + i) % keys
				if lim.Check(ctx, Subject{Identity: identities[k]}).Allowed {
					counts[k].Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	for k := range counts {
		if n := counts[k].Load(); n != limit {
			t.Fatalf("key %d admitted %d events for a quota of %d", k, n, limit)
		}
	}
	if st := lim.Stats(); st.Saturations != 0 {
		t.Errorf("store saturated %d times with %d keys in %d cells", st.Saturations, keys, st.Capacity)
	}
}

var identities = func() [4096]string {
	var out [4096]string
	for i := range out {
		out[i] = "identity-" + itoa(i)
	}
	return out
}()

// TestNoGoroutineLeak covers the failure mode where a limiter starts a cleanup
// loop in its constructor with no way to stop it, so every limiter ever built
// leaks a goroutine forever.
//
// A single-node limiter here starts nothing at all: a cell expires because its
// stored instant is in the past, not because something swept it. There is no
// loop to leak.
func TestNoGoroutineLeak(t *testing.T) {
	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.GC()
			runtime.Gosched()
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	for i := 0; i < 200; i++ {
		lim, err := New(Rule{Quota: PerMinute(10)})
		if err != nil {
			t.Fatal(err)
		}
		lim.Check(context.Background(), Subject{Identity: "x"})
		if err := lim.Close(); err != nil {
			t.Fatal(err)
		}
	}

	after := settle()
	if after > before+2 {
		t.Errorf("200 limiters built and closed left %d extra goroutines (%d -> %d)", after-before, before, after)
	}
}

// TestNoGoroutineLeakWithBackend does the same for the one configuration that
// does start a goroutine.
func TestNoGoroutineLeakWithBackend(t *testing.T) {
	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.GC()
			runtime.Gosched()
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	for i := 0; i < 50; i++ {
		lim, err := NewWith(Config{
			Rules:        []Rule{{Quota: PerMinute(10)}},
			Backend:      &countingBackend{},
			ClusterKey:   "test-cluster-key-0123456789",
			SyncInterval: time.Millisecond,
			Capacity:     1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		lim.Check(context.Background(), Subject{Identity: "x"})
		if err := lim.Close(); err != nil {
			t.Fatal(err)
		}
	}

	after := settle()
	if after > before+2 {
		t.Errorf("50 limiters with a backend left %d extra goroutines (%d -> %d)", after-before, before, after)
	}
}

// TestCloseIsIdempotent because Close in a defer next to a Close on a shutdown
// path is normal.
func TestCloseIsIdempotent(t *testing.T) {
	lim, err := NewWith(Config{Rules: []Rule{{Quota: PerMinute(1)}}, Backend: &countingBackend{}, ClusterKey: "test-cluster-key-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := lim.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

type countingBackend struct {
	calls atomic.Int64
	fail  atomic.Bool
	mu    sync.Mutex
	last  []Demand
	give  map[uint64]time.Duration
}

func (b *countingBackend) Sync(ctx context.Context, node string, demand []Demand) ([]Share, error) {
	b.calls.Add(1)
	if b.fail.Load() {
		return nil, context.DeadlineExceeded
	}
	b.mu.Lock()
	b.last = append(b.last[:0], demand...)
	out := make([]Share, len(demand))
	for i, d := range demand {
		out[i] = Share{Key: d.Key, Others: b.give[d.Key], Nodes: 1}
	}
	b.mu.Unlock()
	return out, nil
}

func (b *countingBackend) Close() error { return nil }
