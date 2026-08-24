// Package ratelimit protects an HTTP API from callers using more of it than they
// are entitled to.
//
// It has no dependencies outside the standard library, no opinion about your
// router, and one line for the common case:
//
//	lim, err := ratelimit.New(ratelimit.Rule{Quota: ratelimit.PerMinute(100)})
//	if err != nil {
//	    return err
//	}
//	defer lim.Close()
//
//	mux.Handle("/", lim.Limit(handler))
//
// No algorithm to pick, no store to wire up, no key function to write, and a
// default key that cannot be forged with a header.
//
// # What it guarantees, and what that costs
//
// Every number here is asserted by a test in this repository rather than argued
// for. The full set, with the measurements behind it, is in the README.
//
// A single node holds the configured rate exactly in steady state. From a fully
// recovered counter it admits up to 1+burst/limit times the quota inside one
// window, which is 1.99x for the default burst of the full quota:
// PerMinute(100) admits 100 immediately and then one every 600ms. That is
// inherent to a token bucket with a burst equal to its quota, and
// [Quota.WithBurst] trades it away if you need it tighter.
//
// A quota is not "no more than n in any moving window". It is "n per window,
// with a burst". If you need the moving-window guarantee, this package does not
// provide it and will not pretend to.
//
// A decision allocates nothing and takes on the order of a hundred nanoseconds.
// The key store cannot grow: memory is exactly capacity times 16 bytes, and a
// single-node limiter starts no goroutines at all, because a counter expires by
// its stored instant being in the past rather than by something sweeping it.
//
// # Selector and key are different questions
//
// A selector says which rule applies to a request. A key says which requests
// share a counter. The same rule with a different key behaves completely
// differently, so the two are declared separately and never derived from each
// other. See [Rule].
//
// Selectors use net/http.ServeMux pattern syntax, so there is nothing new to
// learn. Matching is this package's own, because every rule that matches is
// evaluated rather than only the most specific one.
//
// # Rules add up
//
// A more specific rule does not relax a general one: both apply and the tighter
// one governs. This is deliberately unlike routing, where declaring something
// narrower overrides. A security control that can be loosened by adding a rule
// is a hole.
//
// When a later rule denies, quota that earlier rules already took is given back.
// Without that, a caller who keeps hitting one strict endpoint also burns its
// global quota, and its effective limit is tighter than the configured one in a
// way nobody can predict from the configuration.
//
// # Say it before you enforce it
//
// [Rule.Shadow] makes a rule evaluate, consume, count and report without ever
// denying. It is the same code path as a live rule with a different reason and
// its own metric, so what it measures is what it would do. Deploy every new
// limit that way first; reading the shadow counter is the only way to size a
// limit that does not involve finding out in production.
//
// # When it stops working, it says so
//
// [Limiter.Check] returns a [Decision], not a boolean, because the reason is the
// product. A bool cannot say which rule denied, how long to wait, whether the
// limiter was running degraded, or whether a shadow rule would have refused.
//
// With a [Backend] configured, local state still decides every request and the
// backend only adjusts how much of the global quota this node hands out.
// Nothing on the decision path waits on a remote. When the backend stops
// answering, the limiter keeps serving and keeps limiting per process, every
// decision taken that way reports [ReasonAllowedDegraded], a metric moves, and
// the log says it once. Losing coordination is not an error path: it leaves the
// limiter in single-node mode, which is the most exercised configuration there
// is.
//
// # Safety of the IP dimension
//
// [ClientIP] returns an error without at least one trusted proxy range. Not a
// warning and not a sensible default: a limiter that reads a forwarding header
// without knowing which hops it can vouch for is bypassed by sending the
// header, and a bypassable limiter is worse than none, because it manufactures
// confidence.
//
// # Beyond HTTP
//
// The core does not need HTTP. Queue consumers, gRPC handlers and background
// jobs use the same rules through [Limiter.Check] and a [Subject] they build
// themselves.
//
// # What it will not do
//
// Outbound rate limiting, queueing incoming requests instead of refusing them,
// exact quota with transactional guarantees, load shedding, abuse detection, and
// volumetric DDoS protection are all out of scope. The README says why for each.
package ratelimit
