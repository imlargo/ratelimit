package ratelimit

import "net/http"

// DeniedHandler serves a denied request. It receives the decision so it can
// render the error in whatever shape the API uses.
type DeniedHandler func(w http.ResponseWriter, r *http.Request, d Decision)

// DefaultDeniedHandler answers 429 with a short plain-text body.
//
// 429 is the code for "the caller exceeded its quota". 503 is for "the server
// cannot cope". They are different situations with different client behaviour,
// and this package never confuses them.
func DefaultDeniedHandler(w http.ResponseWriter, r *http.Request, d Decision) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	switch d.Reason {
	case ReasonStoreSaturated:
		_, _ = w.Write([]byte("Too Many Requests: rate limiter at capacity\n"))
	default:
		_, _ = w.Write([]byte("Too Many Requests\n"))
	}
}

// Limit wraps a handler so every request is evaluated first.
//
// This is the one line the common case needs:
//
//	mux.Handle("/", lim.Limit(handler))
//
// It works under any wrapping ResponseWriter, because it only ever writes
// headers before the handler runs and never inspects what the handler wrote. It
// does not touch the request context, so it adds no allocation to an allowed
// request beyond rendering the headers themselves.
func (l *Limiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := l.subject(r)
		d := l.decide(&s, s.cost())

		d.WriteHeaders(w.Header())

		if l.onDecision != nil {
			l.onDecision(r, d)
		}
		if !d.Allowed {
			l.deniedHandler(w, r, d)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LimitFunc is Limit for a bare handler function.
func (l *Limiter) LimitFunc(next func(http.ResponseWriter, *http.Request)) http.Handler {
	return l.Limit(http.HandlerFunc(next))
}

// Middleware returns Limit in the shape most routers expect for a middleware
// chain.
func (l *Limiter) Middleware() func(http.Handler) http.Handler {
	return l.Limit
}
