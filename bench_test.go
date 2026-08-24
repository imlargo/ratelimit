package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The benchmark set the README quotes, kept comparable between versions:
// an allowed decision, a denied one, three composed rules, and high cardinality.
//
// Run with:
//
//	go test -run xxx -bench . -benchmem -count=6 ./...

func benchLimiter(tb testing.TB, rules ...Rule) *Limiter {
	tb.Helper()
	lim, err := NewWith(Config{
		Identity: FromSubject(),
		Tenant:   FromSubject(),
		Rules:    rules,
		Capacity: 1 << 18,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return lim
}

func BenchmarkAllowed(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1 << 30), Key: ByIdentity()})
	defer lim.Close()
	ctx := context.Background()
	s := Subject{Identity: "caller-1", Path: "/api/v1/things", Method: "GET"}
	lim.Check(ctx, s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lim.Check(ctx, s)
	}
}

func BenchmarkDenied(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1).WithBurst(1), Key: ByIdentity()})
	defer lim.Close()
	ctx := context.Background()
	s := Subject{Identity: "caller-1", Path: "/api/v1/things", Method: "GET"}
	lim.Check(ctx, s)
	lim.Check(ctx, s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lim.Check(ctx, s)
	}
}

func BenchmarkThreeComposedRules(b *testing.B) {
	lim := benchLimiter(b,
		Rule{Name: "global", Quota: PerHour(1 << 30), Key: ByIdentity()},
		Rule{Name: "group", Selector: "/api/", Quota: PerHour(1 << 30), Key: By(Identity(), Path())},
		Rule{Name: "endpoint", Selector: "GET /api/v1/things", Quota: PerHour(1 << 30), Key: By(Identity(), Tenant(), Method(), Path())},
	)
	defer lim.Close()
	ctx := context.Background()
	s := Subject{Identity: "caller-1", Tenant: "acme", Path: "/api/v1/things", Method: "GET"}
	lim.Check(ctx, s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lim.Check(ctx, s)
	}
}

func BenchmarkHighCardinality(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1 << 30), Key: ByIdentity()})
	defer lim.Close()
	ctx := context.Background()

	const keys = 1 << 16
	subjects := make([]Subject, keys)
	for i := range subjects {
		subjects[i] = Subject{Identity: "caller-" + itoa(i), Path: "/api/v1/things", Method: "GET"}
		lim.Check(ctx, subjects[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lim.Check(ctx, subjects[i&(keys-1)])
	}
}

func BenchmarkOneHotKeyParallel(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1 << 40), Key: ByIdentity()})
	defer lim.Close()
	s := Subject{Identity: "the-hot-key", Path: "/x", Method: "GET"}
	lim.Check(context.Background(), s)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			_ = lim.Check(ctx, s)
		}
	})
}

func BenchmarkManyKeysParallel(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1 << 30), Key: ByIdentity()})
	defer lim.Close()
	const keys = 1 << 14
	subjects := make([]Subject, keys)
	for i := range subjects {
		subjects[i] = Subject{Identity: "caller-" + itoa(i), Path: "/x", Method: "GET"}
		lim.Check(context.Background(), subjects[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		i := 0
		for pb.Next() {
			_ = lim.Check(ctx, subjects[i&(keys-1)])
			i++
		}
	})
}

func BenchmarkMiddlewareAllowed(b *testing.B) {
	lim := benchLimiter(b, Rule{Name: "global", Quota: PerHour(1 << 30), Key: ByIP("10.0.0.0/8")})
	defer lim.Close()
	h := lim.Limit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/things", nil)
	r.RemoteAddr = "10.1.2.3:4567"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := &benchWriter{h: make(http.Header, 8)}
	h.ServeHTTP(w, r)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		h.ServeHTTP(w, r)
	}
}

func BenchmarkKeyComposition(b *testing.B) {
	lim := benchLimiter(b, Rule{Quota: PerHour(1 << 30), Key: By(Identity(), Tenant(), Method(), Path())})
	defer lim.Close()
	r := &lim.rules[0]
	s := Subject{Identity: "caller-1", Tenant: "acme", Path: "/api/v1/things", Method: "GET"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.key.hash(lim.hkey, r.tag, &s)
	}
}

func BenchmarkClientIPFromHeader(b *testing.B) {
	pfx, err := parsePrefixes([]string{"10.0.0.0/8", "192.168.0.0/16"})
	if err != nil {
		b.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:4567"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7, 10.9.9.9")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = clientIP(r, pfx)
	}
}

type benchWriter struct {
	h    http.Header
	code int
}

func (w *benchWriter) Header() http.Header         { return w.h }
func (w *benchWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *benchWriter) WriteHeader(c int)           { w.code = c }
