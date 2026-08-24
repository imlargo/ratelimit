package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStandardHeaderConformance checks the fields the IETF httpapi draft
// defines: RateLimit-Policy advertising the quota policies, and RateLimit
// carrying the live service limit for one of them.
//
// The draft supersedes the three separate fields earlier versions defined, so
// those are not emitted unless a dialect is named. See [LegacyHeaders].
func TestStandardHeaderConformance(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity:         FromSubject(),
		RetryAfterJitter: NoJitter,
		Rules: []Rule{
			{Name: "search", Selector: "POST /api/search", Quota: PerMinute(10), Key: ByIdentity()},
			{Name: "global", Quota: PerMinute(1000), Key: ByIdentity()},
			{Name: "health", Selector: "GET /healthz", Exempt: true},
			{Name: "candidate", Selector: "/api/", Quota: PerMinute(5), Key: ByIdentity(), Shadow: true},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u1", Path: "/api/search", Method: "POST"}

	d := lim.Check(ctx, s)
	h := http.Header{}
	d.WriteHeaders(h)

	// The policy field lists every enforcing policy and never varies, so it is
	// rendered once when the limiter is built.
	policy := h.Get(HeaderRateLimitPolicy)
	wantPolicy := `"search";q=10;w=60, "global";q=1000;w=60`
	if policy != wantPolicy {
		t.Errorf("RateLimit-Policy = %q, want %q", policy, wantPolicy)
	}
	if strings.Contains(policy, "health") {
		t.Error("an exempt rule has no quota and must not be advertised as a policy")
	}
	if strings.Contains(policy, "candidate") {
		t.Error("a shadow rule does not deny and must not be advertised as an enforcing policy")
	}

	// The service limit names the binding policy and carries r= and t=.
	rl := h.Get(HeaderRateLimit)
	if rl != `"search";r=9;t=6` {
		t.Errorf("RateLimit = %q, want %q", rl, `"search";r=9;t=6`)
	}

	// An allowed request carries no Retry-After.
	if got := h.Get(HeaderRetryAfter); got != "" {
		t.Errorf("Retry-After on an allowed request: %q", got)
	}

	// Drain and check the denial.
	for i := 0; i < 9; i++ {
		lim.Check(ctx, s)
	}
	d = lim.Check(ctx, s)
	if d.Allowed {
		t.Fatal("should be denied")
	}
	h = http.Header{}
	d.WriteHeaders(h)
	if got := h.Get(HeaderRateLimit); got != `"search";r=0;t=60` {
		t.Errorf("RateLimit on denial = %q, want %q", got, `"search";r=0;t=60`)
	}
	ra := h.Get(HeaderRetryAfter)
	n, err := strconv.Atoi(ra)
	if err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer number of seconds", ra)
	}
	// Retry-After must never point before the end of the effective window.
	tv, _ := strconv.Atoi(strings.TrimPrefix(strings.Split(got(h, HeaderRateLimit), ";t=")[1], ""))
	if n > tv {
		t.Errorf("Retry-After %d is beyond the effective window %d; that is allowed but unexpected here", n, tv)
	}
	if n < 1 {
		t.Errorf("Retry-After %d must be at least 1", n)
	}
}

func got(h http.Header, k string) string { return h.Get(k) }

// TestResetIsExactAndRetryAfterIsJittered.
//
// The effective window is a fact about the limiter and is reported exactly. The
// retry hint is advice, and its job is to stop a crowd denied in the same
// instant from returning in the same instant, so it carries jitter.
//
// The jitter is one sided. The standard requires Retry-After to take precedence
// over the RateLimit field and says it should not point earlier than the end of
// the effective window, so a symmetric jitter would violate it half the time and
// send clients back before they have quota.
func TestResetIsExactAndRetryAfterIsJittered(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: FromSubject(),
		Rules:    []Rule{{Name: "r", Quota: PerMinute(1), Key: ByIdentity()}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	resets := map[time.Duration]int{}
	retries := map[time.Duration]int{}
	var minRetry, maxRetry time.Duration
	minRetry = time.Hour

	for i := 0; i < 400; i++ {
		s := Subject{Identity: "u" + itoa(i)}
		lim.Check(ctx, s) // consume the single unit
		d := lim.Check(ctx, s)
		if d.Allowed {
			t.Fatalf("subject %d should be denied", i)
		}
		resets[d.ResetAfter]++
		retries[d.RetryAfter]++
		if d.RetryAfter < minRetry {
			minRetry = d.RetryAfter
		}
		if d.RetryAfter > maxRetry {
			maxRetry = d.RetryAfter
		}
		// One sided: never earlier than the fact.
		if d.RetryAfter < d.ResetAfter {
			t.Fatalf("Retry-After %v is earlier than the effective window %v; the jitter must only ever add",
				d.RetryAfter, d.ResetAfter)
		}
	}

	if len(resets) != 1 {
		t.Errorf("the effective window took %d distinct values; it is a fact and must not be jittered", len(resets))
	}
	if len(retries) < 50 {
		t.Errorf("the retry hint took only %d distinct values over 400 denials; the jitter is not spreading the crowd", len(retries))
	}
	t.Logf("reset always %v; retry hint spread over %d values in [%v, %v]", firstKey(resets), len(retries), minRetry, maxRetry)
}

func firstKey(m map[time.Duration]int) time.Duration {
	for k := range m {
		return k
	}
	return 0
}

// TestNoJitterIsDeterministic so tests and golden files are possible.
func TestNoJitterIsDeterministic(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity:         FromSubject(),
		RetryAfterJitter: NoJitter,
		Rules:            []Rule{{Quota: PerMinute(1), Key: ByIdentity()}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()
	ctx := context.Background()
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		s := Subject{Identity: "u" + itoa(i)}
		lim.Check(ctx, s)
		seen[lim.Check(ctx, s).RetryAfter] = true
	}
	if len(seen) != 1 {
		t.Errorf("NoJitter still produced %d distinct retry hints", len(seen))
	}
}

// TestLegacyDialects. There is no default, because there is no correct default:
// X-RateLimit-Reset means a Unix timestamp at GitHub and a count of seconds at
// X/Twitter, and a number whose meaning depends on the reader is the silent
// failure this package exists to avoid.
func TestLegacyDialects(t *testing.T) {
	build := func(l LegacyHeaders) http.Header {
		clk := NewTestingClock()
		lim, err := NewWith(Config{
			Identity:         FromSubject(),
			Legacy:           l,
			RetryAfterJitter: NoJitter,
			Rules:            []Rule{{Name: "r", Quota: PerMinute(10), Key: ByIdentity()}},
		}.WithClock(clk))
		if err != nil {
			t.Fatal(err)
		}
		defer lim.Close()
		d := lim.Check(context.Background(), Subject{Identity: "u"})
		h := http.Header{}
		d.WriteHeaders(h)
		return h
	}

	h := build(LegacyNone)
	for _, k := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"} {
		if h.Get(k) != "" {
			t.Errorf("LegacyNone emitted %s", k)
		}
	}

	h = build(LegacyXRateLimit)
	if h.Get("X-RateLimit-Limit") != "10" || h.Get("X-RateLimit-Remaining") != "9" {
		t.Errorf("LegacyXRateLimit: %v", h)
	}
	// Reset is a count of remaining seconds, never a Unix timestamp. GitHub
	// reads this field the other way, which is why the family is opt-in and why
	// only one reading is offered.
	if got := h.Get("X-RateLimit-Reset"); got != "6" {
		t.Errorf("X-RateLimit-Reset = %q, want %q seconds remaining", got, "6")
	}

	if _, err := NewWith(Config{Rules: []Rule{{Quota: PerMinute(1)}}, Legacy: 99}); err == nil {
		t.Error("an unknown dialect was accepted")
	}
}

// TestNoHeadersWithoutAQuota: an exempt request has no quota to report, and
// inventing one would be a lie.
func TestNoHeadersWithoutAQuota(t *testing.T) {
	lim, err := New(
		Rule{Name: "health", Selector: "GET /healthz", Exempt: true},
		Rule{Name: "global", Quota: PerMinute(10)},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "203.0.113.1:1"
	d := lim.CheckRequest(r)
	if d.Reason != ReasonExempt {
		t.Fatalf("reason %v, want exempt", d.Reason)
	}
	h := http.Header{}
	d.WriteHeaders(h)
	if len(h) != 0 {
		t.Errorf("an exempt decision wrote headers: %v", h)
	}
}

// TestMiddleware covers the one line the common case needs.
func TestMiddleware(t *testing.T) {
	clk := NewTestingClock()
	var served int
	lim, err := NewWith(Config{
		RetryAfterJitter: NoJitter,
		Rules:            []Rule{{Name: "global", Quota: PerMinute(2)}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	h := lim.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusTeapot)
	}))

	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.RemoteAddr = "203.0.113.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	for i := 0; i < 2; i++ {
		w := call()
		if w.Code != http.StatusTeapot {
			t.Fatalf("request %d: status %d, want the handler's 418", i, w.Code)
		}
		if w.Header().Get(HeaderRateLimit) == "" {
			t.Errorf("request %d: no RateLimit header on an allowed request", i)
		}
	}

	w := call()
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status %d, want 429. 429 is quota, 503 is the server not coping, and they are not the same thing", w.Code)
	}
	if served != 2 {
		t.Errorf("the handler ran %d times, want 2", served)
	}
	if w.Header().Get(HeaderRetryAfter) == "" {
		t.Error("no Retry-After on a denial")
	}
	if w.Header().Get(HeaderRateLimitPolicy) == "" {
		t.Error("no RateLimit-Policy on a denial")
	}
}

// TestMiddlewareUnderAWrappedResponseWriter, because it will run under other
// people's middleware.
func TestMiddlewareUnderAWrappedResponseWriter(t *testing.T) {
	lim, err := New(Rule{Quota: PerMinute(1)})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	inner := lim.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	var wrapped bool
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped = true
		inner.ServeHTTP(&countingWriter{ResponseWriter: w}, r)
	})

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.1:1"
	w := httptest.NewRecorder()
	outer.ServeHTTP(w, r)

	if !wrapped {
		t.Fatal("the wrapper did not run")
	}
	if w.Body.String() != "ok" || w.Header().Get(HeaderRateLimit) == "" {
		t.Errorf("body %q, headers %v", w.Body.String(), w.Header())
	}
}

type countingWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.n += len(b)
	return c.ResponseWriter.Write(b)
}

// TestCustomDeniedHandler gets the decision, so an API can render its own error
// shape without recomputing anything.
func TestCustomDeniedHandler(t *testing.T) {
	lim, err := NewWith(Config{
		RetryAfterJitter: NoJitter,
		Rules:            []Rule{{Name: "tight", Quota: PerMinute(1)}},
		DeniedHandler: func(w http.ResponseWriter, r *http.Request, d Decision) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"rule":"` + d.Rule + `","retry_after_seconds":` +
				strconv.FormatInt(int64(d.RetryAfter.Seconds()), 10) + `}`))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	h := lim.Limit(http.NotFoundHandler())
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 1 {
			if w.Code != http.StatusTooManyRequests {
				t.Errorf("status %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), `"rule":"tight"`) {
				t.Errorf("body %q does not carry the decision", w.Body.String())
			}
		}
	}
}
