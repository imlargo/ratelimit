// Package ratelimittest provides the pieces needed to test code that uses
// ratelimit: a correct in-memory backend, a goroutine leak detector, and helpers
// that drive the limiter without sleeping.
//
// It ships as part of the product rather than as an internal test helper,
// because a library that is hard to test does not get tested.
//
// Nothing here belongs in production code.
package ratelimittest

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imlargo/ratelimit"
)

// Backend is an in-memory [ratelimit.Backend] that implements the contract
// exactly, so a test can exercise distributed behaviour without a Redis.
//
// It is also the reference implementation: a real backend has to do what this
// does, and no more.
type Backend struct {
	mu   sync.Mutex
	seen map[uint64]map[string]entry

	// Horizon is how long a node's report stays live. A node that stops
	// reporting must not hold quota forever. Defaults to four times whatever
	// sync interval you configure, which for tests means: set it, or leave it
	// and rely on the default of 4s.
	Horizon time.Duration

	// Now is the backend's clock. Every backend must use its own clock rather
	// than the caller's, so that clock skew between nodes affects nothing.
	// Defaults to time.Since of the moment the Backend was created.
	Now func() time.Duration

	start time.Time

	failWith atomic.Pointer[error]
	hang     atomic.Bool
	calls    atomic.Int64
	keys     atomic.Int64
	closed   atomic.Bool
}

type entry struct {
	amount time.Duration
	at     time.Duration
}

// NewBackend returns a ready backend.
func NewBackend() *Backend {
	b := &Backend{seen: map[uint64]map[string]entry{}, start: time.Now(), Horizon: 4 * time.Second}
	b.Now = func() time.Duration { return time.Since(b.start) }
	return b
}

// Sync implements ratelimit.Backend.
func (b *Backend) Sync(ctx context.Context, node string, demand []ratelimit.Demand) ([]ratelimit.Share, error) {
	b.calls.Add(1)
	if p := b.failWith.Load(); p != nil {
		return nil, *p
	}
	if b.hang.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.Now()
	for _, d := range demand {
		m := b.seen[d.Key]
		if m == nil {
			m = map[string]entry{}
			b.seen[d.Key] = m
			b.keys.Add(1)
		}
		m[node] = entry{d.Amount, now}
	}

	out := make([]ratelimit.Share, len(demand))
	for i, d := range demand {
		var others time.Duration
		n := 0
		for who, e := range b.seen[d.Key] {
			if who == node {
				continue // a node must never see its own contribution back
			}
			if now-e.at > b.Horizon {
				continue
			}
			others += e.amount
			n++
		}
		out[i] = ratelimit.Share{Key: d.Key, Others: others, Nodes: n}
	}
	return out, nil
}

// Close implements ratelimit.Backend.
func (b *Backend) Close() error { b.closed.Store(true); return nil }

// Fail makes every subsequent Sync return err, which is how you test degraded
// operation. Pass nil to recover.
func (b *Backend) Fail(err error) {
	if err == nil {
		b.failWith.Store(nil)
		return
	}
	b.failWith.Store(&err)
}

// Hang makes every subsequent Sync block until its context expires, which is a
// different failure from an error and is worth testing separately.
func (b *Backend) Hang(v bool) { b.hang.Store(v) }

// Calls is how many times Sync has been called.
func (b *Backend) Calls() int64 { return b.calls.Load() }

// Keys is how many distinct keys the backend has been told about. Use it to
// check that sync traffic tracks the keys under pressure and not every key the
// process has seen.
func (b *Backend) Keys() int64 { return b.keys.Load() }

// Closed reports whether Close was called, which is how you check that a
// limiter released what it owned.
func (b *Backend) Closed() bool { return b.closed.Load() }

// NoGoroutineLeaks fails the test if goroutines outlive it.
//
// Call it first in a test:
//
//	func TestSomething(t *testing.T) {
//	    defer ratelimittest.NoGoroutineLeaks(t)()
//	    ...
//	}
//
// A cleanup loop started in a constructor with no way to stop it is the most
// common way a rate limiter breaks a long-running process, so this is worth
// wiring into any test that builds one.
func NoGoroutineLeaks(tb testing.TB) func() {
	tb.Helper()
	before := stacks()
	return func() {
		tb.Helper()
		for i := 0; i < 100; i++ {
			runtime.GC()
			runtime.Gosched()
			if after := stacks(); len(after) <= len(before) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		after := stacks()
		var leaked []string
		seen := map[string]int{}
		for _, s := range before {
			seen[s]++
		}
		for _, s := range after {
			if seen[s] > 0 {
				seen[s]--
				continue
			}
			leaked = append(leaked, s)
		}
		sort.Strings(leaked)
		if len(leaked) > 0 {
			tb.Errorf("%d goroutines leaked:\n%s", len(leaked), strings.Join(leaked, "\n"))
		}
	}
}

func stacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if g == "" || strings.Contains(g, "runtime.gopark") && strings.Contains(g, "testing.tRunner") {
			continue
		}
		// Keep only the goroutine's own top frame, so identical goroutines
		// compare equal regardless of where they were parked.
		lines := strings.SplitN(g, "\n", 3)
		if len(lines) < 2 {
			continue
		}
		if strings.Contains(lines[0], "[running]") {
			continue
		}
		out = append(out, strings.TrimSpace(lines[1]))
	}
	return out
}

// Drain calls the limiter until it refuses, and reports how many times it said
// yes. It never sleeps.
func Drain(tb testing.TB, lim *ratelimit.Limiter, s ratelimit.Subject) int {
	tb.Helper()
	const runaway = 5_000_000
	n := 0
	for {
		if !lim.Check(context.Background(), s).Allowed {
			return n
		}
		n++
		if n > runaway {
			tb.Fatalf("the limiter admitted %d events without refusing; it is not limiting", n)
		}
	}
}

// AssertQuota checks that a limiter admits exactly want events for a subject
// from a fully recovered state.
func AssertQuota(tb testing.TB, lim *ratelimit.Limiter, s ratelimit.Subject, want int) {
	tb.Helper()
	if got := Drain(tb, lim, s); got != want {
		tb.Errorf("the limiter admitted %d events for %s, want %d", got, describe(s), want)
	}
}

// AssertDenied checks that the next decision refuses, and returns it so a test
// can look at the reason.
func AssertDenied(tb testing.TB, lim *ratelimit.Limiter, s ratelimit.Subject) ratelimit.Decision {
	tb.Helper()
	d := lim.Check(context.Background(), s)
	if d.Allowed {
		tb.Errorf("expected %s to be denied, got %s", describe(s), d)
	}
	return d
}

// AssertAllowed checks that the next decision admits, and returns it.
func AssertAllowed(tb testing.TB, lim *ratelimit.Limiter, s ratelimit.Subject) ratelimit.Decision {
	tb.Helper()
	d := lim.Check(context.Background(), s)
	if !d.Allowed {
		tb.Errorf("expected %s to be allowed, got %s", describe(s), d)
	}
	return d
}

func describe(s ratelimit.Subject) string {
	var b strings.Builder
	b.WriteString("subject{")
	first := true
	add := func(k, v string) {
		if v == "" {
			return
		}
		if !first {
			b.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%s", k, v)
	}
	add("identity", s.Identity)
	add("tenant", s.Tenant)
	add("method", s.Method)
	add("path", s.Path)
	if s.IP.IsValid() {
		add("ip", s.IP.String())
	}
	b.WriteString("}")
	return b.String()
}
