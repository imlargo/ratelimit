package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDecisionAllocatesNothing is a build gate, not an observation.
//
// This code sits in front of every request the service serves, so an allocation
// per decision is a regression against what the package claims. The budget is
// asserted, and exceeding it fails.
//
// Note what this covers and what it does not: the *decision* allocates nothing.
// Rendering headers does - http.Header.Set inserts into a map and the RateLimit
// field value has to be formatted - and that budget is asserted separately in
// TestMiddlewareAllocationBudget rather than quietly folded in here.
func TestDecisionAllocatesNothing(t *testing.T) {
	cases := []struct {
		name  string
		cfg   Config
		subj  Subject
		allow bool
	}{
		{
			name: "allowed, one rule",
			cfg: Config{
				Rules:    []Rule{{Quota: PerHour(1 << 30), Key: ByIdentity()}},
				Identity: FromSubject(),
			},
			subj:  Subject{Identity: "u1", Path: "/api/v1/things", Method: "GET"},
			allow: true,
		},
		{
			name: "denied",
			cfg: Config{
				Rules:            []Rule{{Quota: PerHour(1).WithBurst(1), Key: ByIdentity()}},
				Identity:         FromSubject(),
				RetryAfterJitter: NoJitter,
			},
			subj: Subject{Identity: "u1", Path: "/api/v1/things", Method: "GET"},
		},
		{
			name: "allowed, three composed rules",
			cfg: Config{
				Identity: FromSubject(),
				Tenant:   FromSubject(),
				Rules: []Rule{
					{Name: "global", Quota: PerHour(1 << 30), Key: ByIdentity()},
					{Name: "group", Selector: "/api/", Quota: PerHour(1 << 30), Key: By(Identity(), Path())},
					{Name: "endpoint", Selector: "GET /api/v1/things", Quota: PerHour(1 << 30), Key: By(Identity(), Tenant(), Method(), Path())},
				},
			},
			subj:  Subject{Identity: "u1", Tenant: "t1", Path: "/api/v1/things", Method: "GET"},
			allow: true,
		},
		{
			name: "exempt",
			cfg: Config{
				Rules: []Rule{
					{Name: "health", Selector: "GET /healthz", Exempt: true},
					{Name: "global", Quota: PerHour(1 << 30), Key: ByIdentity()},
				},
				Identity: FromSubject(),
			},
			subj:  Subject{Identity: "u1", Path: "/healthz", Method: "GET"},
			allow: true,
		},
		{
			name: "shadow rule that would deny",
			cfg: Config{
				Rules: []Rule{
					{Name: "candidate", Quota: PerHour(1).WithBurst(1), Key: ByIdentity(), Shadow: true},
					{Name: "live", Quota: PerHour(1 << 30), Key: ByIdentity()},
				},
				Identity: FromSubject(),
			},
			subj:  Subject{Identity: "u1", Path: "/x"},
			allow: true,
		},
		{
			name: "quota resolved from identity",
			cfg: Config{
				Rules: []Rule{{
					Quota:    PerHour(10),
					Key:      ByIdentity(),
					QuotaFor: func(s Subject) Quota { return PerHour(1 << 30) },
				}},
				Identity: FromSubject(),
			},
			subj:  Subject{Identity: "u1", Path: "/x"},
			allow: true,
		},
		{
			name: "cost derived from the request",
			cfg: Config{
				Rules: []Rule{{
					Quota:   PerHour(1 << 30),
					Key:     ByIdentity(),
					CostFor: func(s Subject) int64 { return 7 },
				}},
				Identity: FromSubject(),
			},
			subj:  Subject{Identity: "u1", Path: "/x"},
			allow: true,
		},
	}

	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim, err := NewWith(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer lim.Close()

			// Warm the cell so the measured runs take the lock-free path, and
			// for the denied case, drive it to its limit first.
			for i := 0; i < 4; i++ {
				lim.Check(ctx, tc.subj)
			}
			if got := lim.Check(ctx, tc.subj).Allowed; got != tc.allow {
				t.Fatalf("setup: allowed=%v, want %v", got, tc.allow)
			}

			var sink Decision
			n := testing.AllocsPerRun(2000, func() {
				sink = lim.Check(ctx, tc.subj)
			})
			if sink.Allowed != tc.allow {
				t.Fatalf("allowed=%v, want %v", sink.Allowed, tc.allow)
			}
			if n != 0 {
				t.Errorf("Check allocated %.2f times per decision; the budget is 0", n)
			}
		})
	}
}

// TestCheckRequestAllocationBudget measures the HTTP entry point without header
// rendering, which is where the safe client-address derivation happens.
func TestCheckRequestAllocationBudget(t *testing.T) {
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(1 << 30), Key: ByIP("10.0.0.0/8")}},
		Capacity: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/things", nil)
	r.RemoteAddr = "10.1.2.3:4567"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.9.9.9")
	lim.CheckRequest(r)

	n := testing.AllocsPerRun(2000, func() { lim.CheckRequest(r) })
	if n != 0 {
		t.Errorf("CheckRequest allocated %.2f times, budget 0", n)
	}
}

// TestMiddlewareAllocationBudget states the honest number.
//
// The claim "zero allocations per allowed decision" is true of the decision and
// cannot be true of the middleware: http.Header.Set writes into a map and the
// RateLimit field value has to be built. Rather than quietly widen the claim,
// the number is pinned here and published in the README.
//
// RateLimit-Policy costs nothing because it never varies for a given rule
// table, so it is rendered once when the limiter is built.
func TestMiddlewareAllocationBudget(t *testing.T) {
	const budget = 6

	lim, err := NewWith(Config{
		Rules:    []Rule{{Name: "global", Quota: PerHour(1 << 30), Key: ByIP("10.0.0.0/8")}},
		Capacity: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	h := lim.Limit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/things", nil)
	r.RemoteAddr = "10.1.2.3:4567"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: status %d", rec.Code)
	}
	if rec.Header().Get(HeaderRateLimit) == "" {
		t.Fatal("setup: no RateLimit header")
	}

	// A recorder of our own, reused, so we measure the middleware and not
	// httptest.
	w := &nullWriter{h: make(http.Header, 8)}
	h.ServeHTTP(w, r)

	n := testing.AllocsPerRun(2000, func() {
		clear(w.h)
		h.ServeHTTP(w, r)
	})
	if n > budget {
		t.Errorf("middleware allocated %.2f times per allowed request, budget %d", n, budget)
	}
	t.Logf("middleware allocations per allowed request: %.2f (budget %d)", n, budget)
}

type nullWriter struct {
	h    http.Header
	code int
}

func (w *nullWriter) Header() http.Header         { return w.h }
func (w *nullWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nullWriter) WriteHeader(c int)           { w.code = c }

// TestPeekAllocatesNothing because a "what is my quota" endpoint is on the hot
// path too.
func TestPeekAllocatesNothing(t *testing.T) {
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerMinute(100), Key: ByIdentity()}},
		Identity: FromSubject(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	s := Subject{Identity: "u1", Path: "/"}
	lim.Peek(s)
	if n := testing.AllocsPerRun(2000, func() { lim.Peek(s) }); n != 0 {
		t.Errorf("Peek allocated %.2f times, budget 0", n)
	}
}

// TestPeekConsumesNothing pins the contract.
func TestPeekConsumesNothing(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerMinute(3), Key: ByIdentity()}},
		Identity: FromSubject(),
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	s := Subject{Identity: "u1"}
	for i := 0; i < 100; i++ {
		if d := lim.Peek(s); !d.Allowed || d.Remaining != 3 {
			t.Fatalf("peek %d: allowed=%v remaining=%d, want allowed with 3 remaining", i, d.Allowed, d.Remaining)
		}
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if !lim.Check(ctx, s).Allowed {
			t.Fatalf("check %d should be allowed after 100 peeks", i)
		}
	}
	if lim.Check(ctx, s).Allowed {
		t.Fatal("the fourth check should be denied")
	}
	if d := lim.Peek(s); d.Allowed {
		t.Fatal("peek should report the exhausted state")
	}
	_ = time.Second
}
