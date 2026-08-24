package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// memBackend is a correct in-memory Backend. It is the reference for what a
// real backend must do, and ratelimittest ships the same thing.
type memBackend struct {
	mu      sync.Mutex
	seen    map[uint64]map[string]entryAt // key -> node -> demand
	now     func() time.Duration
	horizon time.Duration

	fail   atomic.Pointer[error]
	block  atomic.Bool
	calls  atomic.Int64
	closed atomic.Bool
}

type entryAt struct {
	amount time.Duration
	at     time.Duration
}

func newMemBackend(now func() time.Duration) *memBackend {
	return &memBackend{seen: map[uint64]map[string]entryAt{}, now: now, horizon: 4 * time.Second}
}

func (b *memBackend) Sync(ctx context.Context, node string, demand []Demand) ([]Share, error) {
	b.calls.Add(1)
	if p := b.fail.Load(); p != nil {
		return nil, *p
	}
	if b.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// The store's own clock is the source of truth, so skew between nodes
	// affects nothing.
	now := b.now()
	for _, d := range demand {
		m := b.seen[d.Key]
		if m == nil {
			m = map[string]entryAt{}
			b.seen[d.Key] = m
		}
		m[node] = entryAt{d.Amount, now}
	}
	out := make([]Share, len(demand))
	for i, d := range demand {
		var others time.Duration
		n := 0
		for who, e := range b.seen[d.Key] {
			if who == node {
				continue // a node never sees its own contribution reflected back
			}
			if now-e.at > b.horizon {
				continue // a node that stopped reporting must not hold quota forever
			}
			others += e.amount
			n++
		}
		out[i] = Share{Key: d.Key, Others: others, Nodes: n}
	}
	return out, nil
}

func (b *memBackend) Close() error { b.closed.Store(true); return nil }

// TestBackendCorrectsLocalDecisions: local state always decides, and the remote
// tightens it in the background. Nothing on the decision path waits on a remote.
func TestBackendCorrectsLocalDecisions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		be := newMemBackend(func() time.Duration { return time.Since(start) })

		nodes := make([]*Limiter, 3)
		for i := range nodes {
			l, err := NewWith(Config{
				Identity:      FromSubject(),
				Rules:         []Rule{{Name: "r", Quota: PerMinute(30), Key: ByIdentity()}},
				Backend:       be,
				ClusterKey:    "test-cluster-key-0123456789",
				NodeID:        "node-" + itoa(i),
				SyncInterval:  100 * time.Millisecond,
				SyncThreshold: 1e-9,
				Capacity:      1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			nodes[i] = l
			defer l.Close()
		}

		ctx := context.Background()
		s := Subject{Identity: "hot"}

		// Each node admits 10 without any correction: 30 total, exactly the
		// global quota.
		admitted := 0
		for _, n := range nodes {
			for i := 0; i < 10; i++ {
				if n.Check(ctx, s).Allowed {
					admitted++
				}
			}
		}
		if admitted != 30 {
			t.Fatalf("admitted %d before syncing, want 30", admitted)
		}

		// Let a couple of sync rounds land.
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()

		// Now every node knows the global level is at the quota, so nobody
		// admits anything more. Without correction each node would happily grant
		// another 20.
		extra := 0
		for _, n := range nodes {
			for i := 0; i < 20; i++ {
				if n.Check(ctx, s).Allowed {
					extra++
				}
			}
		}
		// Without coordination each node would hand out its own full quota
		// again: 60 more. With it, each node is holding a third of the global
		// limit and has already spent it.
		if extra > 6 {
			t.Errorf("%d extra requests admitted after allocations landed; "+
				"uncoordinated, three nodes would grant 60", extra)
		}
		t.Logf("after allocation, 60 further attempts across 3 nodes yielded %d admissions", extra)
		for _, n := range nodes {
			if n.Degraded() {
				t.Error("a node reported degraded while the backend was healthy")
			}
		}
	})
}

// TestOvershootBoundIsPublished derives the formula the README publishes, and
// checks it against measurement rather than against an argument.
//
// The dominant term is not the coordination gap: it is the initial burst. Every
// node starts with a full, uncoordinated burst allowance, so N nodes coming up
// cold admit N times the burst before the first correction lands. That is worth
// knowing before deploying, and it is the reason to lower the burst rather than
// to shorten the sync interval.
func TestOvershootBoundIsPublished(t *testing.T) {
	cases := []struct {
		nodes    int
		limit    int
		burst    int
		interval time.Duration
	}{
		{nodes: 1, limit: 600, burst: 600, interval: 200 * time.Millisecond},
		{nodes: 4, limit: 600, burst: 600, interval: 200 * time.Millisecond},
		{nodes: 4, limit: 600, burst: 60, interval: 200 * time.Millisecond},
		{nodes: 8, limit: 600, burst: 60, interval: 200 * time.Millisecond},
		{nodes: 4, limit: 600, burst: 60, interval: time.Second},
		{nodes: 2, limit: 6000, burst: 600, interval: 500 * time.Millisecond},
	}

	for _, tc := range cases {
		name := itoa(tc.nodes) + "nodes_burst" + itoa(tc.burst) + "_" + tc.interval.String()
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				be := newMemBackend(func() time.Duration { return time.Since(start) })

				lims := make([]*Limiter, tc.nodes)
				for i := range lims {
					l, err := NewWith(Config{
						Identity:      FromSubject(),
						Rules:         []Rule{{Quota: PerMinute(tc.limit).WithBurst(tc.burst), Key: ByIdentity()}},
						Backend:       be,
						ClusterKey:    "test-cluster-key-0123456789",
						NodeID:        "n" + itoa(i),
						SyncInterval:  tc.interval,
						SyncThreshold: 1e-9,
						Capacity:      1024,
					})
					if err != nil {
						t.Fatal(err)
					}
					lims[i] = l
					defer l.Close()
				}

				ctx := context.Background()
				s := Subject{Identity: "hot"}
				admitted := 0
				step := 10 * time.Millisecond
				for elapsed := time.Duration(0); elapsed < time.Minute; elapsed += step {
					for _, l := range lims {
						for l.Check(ctx, s).Allowed {
							admitted++
						}
					}
					time.Sleep(step)
				}
				synctest.Wait()

				// Published bound, per key, over the first window:
				//
				//   nodes*burst   the uncoordinated cold start
				// + limit        the window's own sustained allowance
				// + nodes*rate*interval   one coordination gap
				ratePerSec := float64(tc.limit) / 60
				bound := float64(tc.nodes*tc.burst) + float64(tc.limit) +
					float64(tc.nodes)*ratePerSec*tc.interval.Seconds()

				if float64(admitted) > bound*1.05 {
					t.Errorf("admitted %d in the first window; the published bound is %.0f "+
						"(nodes*burst %d + limit %d + nodes*rate*interval %.1f)",
						admitted, bound, tc.nodes*tc.burst, tc.limit,
						float64(tc.nodes)*ratePerSec*tc.interval.Seconds())
				}
				t.Logf("nodes=%d burst=%d interval=%v: admitted %d, bound %.0f (%.0f%% of bound), %.2fx the quota",
					tc.nodes, tc.burst, tc.interval, admitted, bound, 100*float64(admitted)/bound,
					float64(admitted)/float64(tc.limit))
			})
		})
	}
}

// TestDegradationIsDeclaredAndNotAnErrorPath is the central claim of the
// distributed design.
//
// Losing the backend does not take a different branch. The decision path reads a
// correction that has simply stopped being updated, which leaves the limiter in
// single-node mode: the most exercised configuration there is, rather than a
// fallback that only runs during an incident and that nobody has tested.
func TestDegradationIsDeclaredAndNotAnErrorPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		be := newMemBackend(func() time.Duration { return time.Since(start) })

		var transitions []bool
		var mu sync.Mutex
		logs := &countingHandler{}

		lim, err := NewWith(Config{
			Identity:      FromSubject(),
			Rules:         []Rule{{Name: "r", Quota: PerMinute(60), Key: ByIdentity()}},
			Backend:       be,
			ClusterKey:    "test-cluster-key-0123456789",
			SyncInterval:  100 * time.Millisecond,
			SyncThreshold: 1e-9,
			Capacity:      1024,
			Logger:        slog.New(logs),
			Metrics: Metrics{DegradedChanged: func(d bool) {
				mu.Lock()
				transitions = append(transitions, d)
				mu.Unlock()
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer lim.Close()

		ctx := context.Background()
		s := Subject{Identity: "u"}

		lim.Check(ctx, s)
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if lim.Degraded() {
			t.Fatal("degraded while the backend was healthy")
		}
		if d := lim.Check(ctx, s); d.Degraded || d.Reason == ReasonAllowedDegraded {
			t.Fatalf("a healthy decision claimed degradation: %s", d)
		}

		// Kill the backend.
		boom := errors.New("connection refused")
		be.fail.Store(&boom)
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()

		if !lim.Degraded() {
			t.Fatal("the backend has been failing for three rounds and the limiter has not noticed")
		}

		// Decisions still happen, still limit locally, and say they are degraded.
		degradedSeen, allowed := 0, 0
		for i := 0; i < 200; i++ {
			d := lim.Check(ctx, s)
			if d.Allowed {
				allowed++
			}
			if d.Degraded {
				degradedSeen++
			}
		}
		if degradedSeen != 200 {
			t.Errorf("%d of 200 decisions reported degradation, want all of them", degradedSeen)
		}
		if allowed == 0 || allowed >= 200 {
			t.Errorf("%d of 200 allowed while degraded; the local limit must still apply", allowed)
		}

		// The warning is emitted once, on the transition, not per request.
		if n := logs.count(slog.LevelWarn); n != 1 {
			t.Errorf("%d warnings logged, want exactly 1: a per-request log during an incident is its own outage", n)
		}

		// Recovery.
		be.fail.Store(nil)
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()
		if lim.Degraded() {
			t.Fatal("still degraded after the backend recovered")
		}

		mu.Lock()
		got := append([]bool(nil), transitions...)
		mu.Unlock()
		if len(got) != 2 || got[0] != true || got[1] != false {
			t.Errorf("transitions %v, want [true false]: one in, one out, never per request", got)
		}
	})
}

// TestRecoveryDoesNotReconcile: what a node admitted while the backend was down
// is not replayed when it comes back.
//
// Replaying it would produce a synchronised wall of denials the moment the
// incident ended, which is the worst possible moment. The degraded window was
// over-admission and is declared as such.
func TestRecoveryDoesNotReconcile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		be := newMemBackend(func() time.Duration { return time.Since(start) })

		mk := func(id string) *Limiter {
			l, err := NewWith(Config{
				Identity:      FromSubject(),
				Rules:         []Rule{{Quota: PerMinute(60), Key: ByIdentity()}},
				Backend:       be,
				ClusterKey:    "test-cluster-key-0123456789",
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

		ctx := context.Background()
		s := Subject{Identity: "u"}

		// Backend down. Both nodes admit locally.
		boom := errors.New("down")
		be.fail.Store(&boom)
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()

		for i := 0; i < 30; i++ {
			a.Check(ctx, s)
			b.Check(ctx, s)
		}

		// Backend returns. Let several rounds land.
		be.fail.Store(nil)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()

		if a.Degraded() || b.Degraded() {
			t.Fatal("still degraded after recovery")
		}

		// Move a whole window on so both cells are fully recovered, then check
		// there is no backlog of denials waiting.
		time.Sleep(2 * time.Minute)
		synctest.Wait()

		allowed := 0
		for i := 0; i < 60; i++ {
			if a.Check(ctx, s).Allowed {
				allowed++
			}
		}
		if allowed == 0 {
			t.Error("every request was denied after recovery; consumption from the degraded window was replayed")
		}
		t.Logf("after a degraded window and a full recovery window, %d of 60 admitted: no denial spike", allowed)
	})
}

// TestSyncDeadlineIsRespected: a backend that hangs must not stall the sync loop
// forever, and must not affect the decision path at all.
func TestSyncDeadlineIsRespected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		be := newMemBackend(func() time.Duration { return time.Since(start) })
		be.block.Store(true)

		lim, err := NewWith(Config{
			Identity:      FromSubject(),
			Rules:         []Rule{{Quota: PerMinute(60), Key: ByIdentity()}},
			Backend:       be,
			ClusterKey:    "test-cluster-key-0123456789",
			SyncInterval:  100 * time.Millisecond,
			SyncThreshold: 1e-9,
			Capacity:      1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer lim.Close()

		ctx := context.Background()
		s := Subject{Identity: "u"}
		lim.Check(ctx, s)

		// Decisions keep happening at full speed while the backend hangs.
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()

		if !lim.Degraded() {
			t.Error("a hanging backend was not reported as degraded; a hang is a failure, not a wait")
		}
		if n := be.calls.Load(); n < 2 {
			t.Errorf("the sync loop made %d calls; a timed-out call must not stop it retrying", n)
		}
		if !lim.Check(ctx, s).Allowed {
			t.Error("the decision path was affected by a hanging backend")
		}
	})
}

// TestBackendContractViolationIsDeclared: a backend that returns the wrong
// number of cells is a bug in the backend, and pretending otherwise would apply
// one key's level to another.
func TestBackendContractViolationIsDeclared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad := &truncatingBackend{}
		lim, err := NewWith(Config{
			Identity:      FromSubject(),
			Rules:         []Rule{{Quota: PerMinute(60), Key: ByIdentity()}},
			Backend:       bad,
			ClusterKey:    "test-cluster-key-0123456789",
			SyncInterval:  100 * time.Millisecond,
			SyncThreshold: 1e-9,
			Capacity:      1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer lim.Close()

		lim.Check(context.Background(), Subject{Identity: "u"})
		time.Sleep(350 * time.Millisecond)
		synctest.Wait()

		if !lim.Degraded() {
			t.Error("a backend breaking its contract was treated as healthy")
		}
	})
}

type truncatingBackend struct{}

func (truncatingBackend) Sync(ctx context.Context, node string, demand []Demand) ([]Share, error) {
	return nil, nil // wrong length whenever demand is non-empty
}
func (truncatingBackend) Close() error { return nil }

// TestSyncVolumeTracksActiveKeysNotTotal: cells far from their limit are not
// worth correcting, and skipping them is what keeps sync traffic proportional to
// active keys rather than to everything the process has ever seen.
func TestSyncVolumeTracksActiveKeysNotTotal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		be := newMemBackend(func() time.Duration { return time.Since(start) })
		var sizes []int
		var mu sync.Mutex

		lim, err := NewWith(Config{
			Identity:      FromSubject(),
			Rules:         []Rule{{Quota: PerMinute(100), Key: ByIdentity()}},
			Backend:       be,
			ClusterKey:    "test-cluster-key-0123456789",
			SyncInterval:  100 * time.Millisecond,
			SyncThreshold: 0.25,
			Capacity:      4096,
			Metrics: Metrics{BackendSync: func(n int, _ time.Duration, _ error) {
				mu.Lock()
				sizes = append(sizes, n)
				mu.Unlock()
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer lim.Close()

		ctx := context.Background()
		// 500 keys touched once each: 1% of quota, far from any limit.
		for i := 0; i < 500; i++ {
			lim.Check(ctx, Subject{Identity: "cold-" + itoa(i)})
		}
		// 3 keys driven past the threshold.
		for i := 0; i < 3; i++ {
			for j := 0; j < 40; j++ {
				lim.Check(ctx, Subject{Identity: "hot-" + itoa(i)})
			}
		}

		time.Sleep(250 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		got := append([]int(nil), sizes...)
		mu.Unlock()
		if len(got) == 0 {
			t.Fatal("no sync rounds observed")
		}
		last := got[len(got)-1]
		if last > 10 {
			t.Errorf("a sync round carried %d cells for 503 touched keys; only the %d near their limit should be published", last, 3)
		}
		t.Logf("503 keys touched, %d of them near a limit: sync rounds carried %v cells", 3, got)
	})
}

type countingHandler struct {
	mu sync.Mutex
	n  map[slog.Level]int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n == nil {
		h.n = map[slog.Level]int{}
	}
	h.n[r.Level]++
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }
func (h *countingHandler) count(l slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n[l]
}
