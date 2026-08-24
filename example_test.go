package ratelimit_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/imlargo/ratelimit"
)

// Level 0: one limit over the whole API.
//
// No selector, no key, no algorithm, no backend. The default key is the address
// that opened the connection, which needs no configuration and cannot be forged
// with a header.
func Example_level0() {
	lim, err := ratelimit.New(ratelimit.Rule{Name: "global", Quota: ratelimit.PerMinute(100)})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mux := http.NewServeMux()
	mux.Handle("/", lim.Limit(handler()))

	fmt.Println(serve(mux, "GET", "/api/things", "203.0.113.10:5000"))
	// Output: 200 RateLimit="global";r=99;t=1
}

// Level 1: change what shares a counter.
//
// By caller when the request is authenticated, by client address when it is not.
// The trusted proxy ranges are required: without them anyone could set
// X-Forwarded-For and pick their own identity, and a limiter that can be
// side-stepped by a header is worse than none.
func Example_level1() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: func(r *http.Request) (string, bool) {
			id := r.Header.Get("X-Api-Key-Id")
			return id, id != ""
		},
		Rules: []ratelimit.Rule{{
			Name:  "per-caller",
			Quota: ratelimit.PerMinute(100),
			Key:   ratelimit.ByIdentityOrIP(ratelimit.PrivateRanges...),
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mux := http.NewServeMux()
	mux.Handle("/", lim.Limit(handler()))
	fmt.Println(serve(mux, "GET", "/api/things", "10.0.0.1:5000", "X-Api-Key-Id", "acct_42"))
	// Output: 200 RateLimit="per-caller";r=99;t=1
}

// Level 2 and 3: different limits for different parts of the API, all at once.
//
// There is no level 3 API. Every rule whose selector matches is evaluated, the
// tightest one governs, and quota taken by a broader rule is given back when a
// narrower one denies. That is what the rule table is for, and it is the same
// code as level 2.
func Example_level2and3() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: func(r *http.Request) (string, bool) {
			id := r.Header.Get("X-Api-Key-Id")
			return id, id != ""
		},
		Rules: []ratelimit.Rule{
			// Health checks are exempt, declared out loud rather than hidden in
			// a middleware condition. Limiting your own probe is how a service
			// takes itself down with its own protection.
			{Name: "health", Selector: "GET /healthz", Exempt: true},

			// Brute force protection, by address, regardless of who you claim
			// to be.
			{Name: "auth", Selector: "POST /api/login", Quota: ratelimit.PerMinute(5),
				Key: ratelimit.ByIP(ratelimit.PrivateRanges...)},

			// Search is expensive: same quota, twenty times the price.
			{Name: "search", Selector: "GET /api/search", Quota: ratelimit.PerMinute(600),
				Key: ratelimit.ByIdentity(), Cost: 20},

			// A ceiling over everything else.
			{Name: "global", Quota: ratelimit.PerMinute(1000),
				Key: ratelimit.ByIdentityOrIP(ratelimit.PrivateRanges...)},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mux := http.NewServeMux()
	mux.Handle("/", lim.Limit(handler()))

	fmt.Println(serve(mux, "GET", "/healthz", "10.0.0.1:1"))
	fmt.Println(serve(mux, "GET", "/api/search", "10.0.0.1:1", "X-Api-Key-Id", "acct_42"))
	// Output:
	// 200 RateLimit=
	// 200 RateLimit="search";r=580;t=2
}

// Per-plan quotas: the limit depends on who is calling, not on what they ask
// for.
func Example_perPlanQuota() {
	plans := map[string]ratelimit.Quota{
		"free":       ratelimit.PerMinute(60),
		"pro":        ratelimit.PerMinute(6000),
		"enterprise": ratelimit.PerMinute(60000),
	}

	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: func(r *http.Request) (string, bool) {
			id := r.Header.Get("X-Api-Key-Id")
			return id, id != ""
		},
		Rules: []ratelimit.Rule{{
			Name:  "by-plan",
			Quota: ratelimit.PerMinute(60), // the fallback if the lookup fails
			Key:   ratelimit.ByIdentity(),
			QuotaFor: func(s ratelimit.Subject) ratelimit.Quota {
				// Read something your auth middleware already resolved. This
				// runs on the decision path, so it is a map lookup and not a
				// database query.
				return plans[s.Request.Header.Get("X-Plan")]
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mux := http.NewServeMux()
	mux.Handle("/", lim.Limit(handler()))
	fmt.Println(serve(mux, "GET", "/api/x", "10.0.0.1:1", "X-Api-Key-Id", "a", "X-Plan", "pro"))
	// Output: 200 RateLimit="by-plan";r=5999;t=1
}

// Shadow mode: see what a rule would have blocked before it blocks anything.
//
// Deploy every new limit this way first. Reading the shadow counter is the only
// way to size a limit that does not involve finding out in production.
func Example_shadowMode() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Rules: []ratelimit.Rule{
			{Name: "export-candidate", Selector: "POST /api/export",
				Quota: ratelimit.PerHour(2), Shadow: true},
			{Name: "global", Quota: ratelimit.PerMinute(1000)},
		},
		OnDecision: func(r *http.Request, d ratelimit.Decision) {
			if d.Shadowed {
				fmt.Printf("would have blocked %s %s (rule %s)\n", r.Method, r.URL.Path, d.ShadowRule)
			}
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mux := http.NewServeMux()
	mux.Handle("/", lim.Limit(handler()))
	for i := 0; i < 4; i++ {
		if code := status(serve(mux, "POST", "/api/export", "203.0.113.1:1")); code != "200" {
			fmt.Println("blocked, which should not happen in shadow mode")
		}
	}
	// Output:
	// would have blocked POST /api/export (rule export-candidate)
	// would have blocked POST /api/export (rule export-candidate)
}

// Away from HTTP: the same limits for a queue consumer or a gRPC handler.
func Example_withoutHTTP() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		// You are filling Subject.Identity yourself, so say so.
		Identity: ratelimit.IdentityFromSubject,
		Rules: []ratelimit.Rule{{
			Name:  "per-tenant",
			Quota: ratelimit.PerSecond(3),
			Key:   ratelimit.ByIdentity(),
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		d := lim.Check(ctx, ratelimit.Subject{Identity: "tenant-7"})
		if d.Allowed {
			fmt.Println("processing job", i)
			continue
		}
		// d.RetryAfter is how long to wait before trying this job again. It is
		// not printed here because it counts down against the real clock, and an
		// example with verified output has to be deterministic.
		fmt.Printf("deferring job %d: %v, retry hint %v\n", i, d.Reason, d.RetryAfter > 0)
	}
	// Output:
	// processing job 0
	// processing job 1
	// processing job 2
	// deferring job 3: denied_quota, retry hint true
	// deferring job 4: denied_quota, retry hint true
}

// Variable cost: an upload priced by size.
//
// The numbers here are deliberately coarse. A quota's internal resolution is its
// window divided by its limit, so with a limit in the millions that resolution is
// microseconds, and a printed Remaining would drift with however long the example
// itself took to run. Real code does not care; an example with verified output
// does.
func Example_variableCost() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: ratelimit.IdentityFromSubject,
		Rules: []ratelimit.Rule{{
			Name:  "upload-kb",
			Quota: ratelimit.PerMinute(100), // 100 KB a minute
			Key:   ratelimit.ByIdentity(),
			CostFor: func(s ratelimit.Subject) int64 {
				return s.Cost // whatever the caller measured, in KB
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for _, kb := range []int64{40, 40, 40} {
		d := lim.Check(ctx, ratelimit.Subject{Identity: "acct_1", Cost: kb})
		fmt.Printf("%d KB: allowed=%v remaining=%d KB\n", kb, d.Allowed, d.Remaining)
	}
	// Output:
	// 40 KB: allowed=true remaining=60 KB
	// 40 KB: allowed=true remaining=20 KB
	// 40 KB: allowed=false remaining=20 KB
}

// Writing your own middleware: the decision knows how to render its own
// headers, so you do not reimplement the format.
func Example_ownMiddleware() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Rules:  []ratelimit.Rule{{Name: "global", Quota: ratelimit.PerMinute(2)}},
		Legacy: ratelimit.LegacyXRateLimit, // opt in to the de-facto family too
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := lim.CheckRequest(r)
			d.WriteHeaders(w.Header())
			if !d.Allowed {
				http.Error(w, "slow down: "+d.Reason.String(), http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	mux := http.NewServeMux()
	mux.Handle("/", mw(handler()))
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "203.0.113.1:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	fmt.Println(w.Header().Get("RateLimit-Policy"))
	fmt.Println(w.Header().Get("RateLimit"))
	fmt.Println(w.Header().Get("X-RateLimit-Remaining"))
	// Output:
	// "global";q=2;w=60
	// "global";r=1;t=30
	// 1
}

// Sizing the key store. The budget is arithmetic, not an estimate.
func Example_memoryBudget() {
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Rules:    []ratelimit.Rule{{Quota: ratelimit.PerMinute(100)}},
		Capacity: 200_000, // at least twice the peak simultaneously active keys
	})
	if err != nil {
		log.Fatal(err)
	}
	defer lim.Close()

	s := lim.Stats()
	fmt.Printf("%d cells x %d bytes = %d MiB, and it cannot grow past that\n",
		s.Capacity, s.BytesPerCell, s.Capacity*s.BytesPerCell/(1<<20))
	// Output: 262144 cells x 16 bytes = 4 MiB, and it cannot grow past that
}

func handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func serve(h http.Handler, method, path, remote string, headers ...string) string {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return fmt.Sprintf("%d RateLimit=%s", w.Code, w.Header().Get("RateLimit"))
}

func status(s string) string { return s[:3] }
