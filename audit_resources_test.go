// Tests for the resource claims: a key store that cannot grow, a heap that does
// not either, and no exported mutable state.

package ratelimit

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Soak: millions of distinct keys through a fixed store, checking the heap does
// not grow. The store cannot grow by construction, but a retained Subject, a
// leaked closure or a growing internal buffer would show up here.
func TestAuditNoHeapGrowth(t *testing.T) {
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerMinute(10), Key: By(Identity(), Path(), Method())}},
		Identity: FromSubject(),
		Capacity: 1 << 14,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	warm := func(n int) {
		for i := 0; i < n; i++ {
			lim.Check(ctx, Subject{Identity: "k" + itoa(i), Path: "/api/v1/things", Method: "GET"})
		}
	}
	settle := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	warm(200_000) // let every buffer reach its steady size
	before := settle()
	warm(2_000_000)
	after := settle()

	growth := int64(after) - int64(before)
	t.Logf("heap after 200k keys: %d KiB; after 2.2M keys: %d KiB; growth %d KiB",
		before/1024, after/1024, growth/1024)
	// A little jitter is normal; anything proportional to the key count is not.
	if growth > 1<<20 {
		t.Errorf("heap grew by %d KiB over two million distinct keys", growth/1024)
	}
}

// The same with a backend configured, so the sync loop and its reused buffers
// are in the picture.
func TestAuditNoHeapGrowthWithBackend(t *testing.T) {
	be := &nopBackend{}
	lim, err := NewWith(Config{
		Rules:         []Rule{{Quota: PerMinute(10), Key: ByIdentity()}},
		Identity:      FromSubject(),
		Backend:       be,
		ClusterKey:    "audit-cluster-key-0123456789",
		SyncInterval:  time.Millisecond,
		SyncThreshold: 1e-9,
		Capacity:      1 << 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	run := func(n int) {
		for i := 0; i < n; i++ {
			lim.Check(ctx, Subject{Identity: "k" + itoa(i)})
		}
	}
	settle := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	run(100_000)
	time.Sleep(20 * time.Millisecond)
	before := settle()
	run(1_000_000)
	time.Sleep(20 * time.Millisecond)
	after := settle()
	growth := int64(after) - int64(before)
	t.Logf("with a backend: heap %d KiB -> %d KiB, growth %d KiB, sync calls %d",
		before/1024, after/1024, growth/1024, be.calls.Load())
	if growth > 1<<20 {
		t.Errorf("heap grew by %d KiB", growth/1024)
	}
}

type nopBackend struct{ calls atomic.Int64 }

func (b *nopBackend) Sync(ctx context.Context, node string, d []Demand) ([]Share, error) {
	b.calls.Add(1)
	out := make([]Share, len(d))
	for i := range d {
		out[i] = Share{Key: d[i].Key}
	}
	return out, nil
}
func (b *nopBackend) Close() error { return nil }

// Are the sentinel errors usable with errors.Is, as the API implies?
func TestAuditErrorsAreMatchable(t *testing.T) {
	cases := []struct {
		rule Rule
		want error
	}{
		{Rule{}, ErrInvalidQuota},
		{Rule{Quota: PerMinute(0)}, ErrInvalidQuota},
		{Rule{Quota: PerMinute(1), Selector: "/{"}, ErrInvalidSelector},
		{Rule{Quota: PerMinute(1), Key: ByIP()}, ErrInvalidKey},
		{Rule{Quota: PerMinute(1), Cost: -1}, ErrInvalidRule},
		{Rule{Exempt: true, Shadow: true}, ErrInvalidRule},
	}
	for _, tc := range cases {
		_, err := New(tc.rule)
		if err == nil {
			t.Errorf("%+v: expected an error", tc.rule)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%+v: error %q does not match the %v sentinel", tc.rule, err, tc.want)
		}
	}
}

// TestCloseReleasesAllocationsAndKeepsWorking. After Close there is nobody left
// to coordinate with, so a limiter still being called must enforce the full
// quota locally rather than stay frozen at whatever share the last sync round
// handed it.
func TestCloseReleasesAllocationsAndKeepsWorking(t *testing.T) {
	be := &stingyBackend{}
	lim, err := NewWith(Config{
		Rules:         []Rule{{Quota: PerHour(100), Key: ByIdentity()}},
		Identity:      FromSubject(),
		Backend:       be,
		ClusterKey:    "audit-cluster-key-0123456789",
		SyncInterval:  2 * time.Millisecond,
		SyncThreshold: 1e-9,
		Capacity:      256,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	s := Subject{Identity: "u"}

	// Consume a little, then let the backend hand down a tiny share.
	for i := 0; i < 5; i++ {
		lim.Check(ctx, s)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		lim.Check(ctx, s)
		if lim.store.ext[0].emission.Load() != 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	allocated := 0
	for i := range lim.store.ext {
		if lim.store.ext[i].emission.Load() != 0 {
			allocated++
		}
	}
	if allocated == 0 {
		t.Skip("no allocation landed; nothing to check")
	}

	if err := lim.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range lim.store.ext {
		if e := lim.store.ext[i].emission.Load(); e != 0 {
			t.Fatalf("cell %d still holds an allocated interval of %v after Close", i, time.Duration(e))
		}
	}

	// And it still limits, at the full local quota, rather than panicking or
	// admitting everything.
	fresh := Subject{Identity: "after-close"}
	admitted := 0
	for i := 0; i < 300; i++ {
		if lim.Check(ctx, fresh).Allowed {
			admitted++
		}
	}
	if admitted != 100 {
		t.Errorf("after Close a fresh key was admitted %d times, want the full local quota of 100", admitted)
	}
}

// stingyBackend always reports heavy demand from others, so this node's share
// shrinks and an allocation is certain to land.
type stingyBackend struct{}

func (stingyBackend) Sync(ctx context.Context, node string, d []Demand) ([]Share, error) {
	out := make([]Share, len(d))
	for i := range d {
		out[i] = Share{Key: d[i].Key, Others: 10 * time.Hour, Nodes: 9}
	}
	return out, nil
}
func (stingyBackend) Close() error { return nil }
