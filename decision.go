package ratelimit

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Standard header field names. The pair defined by the IETF httpapi working
// group draft supersedes the three separate fields earlier drafts defined.
const (
	HeaderRateLimit       = "RateLimit"
	HeaderRateLimitPolicy = "RateLimit-Policy"
	HeaderRetryAfter      = "Retry-After"
)

// LegacyHeaders opts in to the de-facto X-RateLimit-* family alongside the
// standard fields. Off by default.
//
// Only one dialect is offered, and only as an opt-in, because there is no
// correct default. X-RateLimit-Reset means a count of seconds at X/Twitter and
// a Unix timestamp at GitHub, and a number whose meaning depends on who reads
// it is exactly the silent failure this package exists to avoid. The
// delta-seconds reading is the one offered because it agrees with the standard
// field's own semantics; the epoch reading is deliberately not offered, so
// clients expecting GitHub's will misread it.
type LegacyHeaders uint8

const (
	// LegacyNone emits only the standard fields. This is the default.
	LegacyNone LegacyHeaders = iota

	// LegacyXRateLimit also emits X-RateLimit-Limit, X-RateLimit-Remaining and
	// X-RateLimit-Reset, with Reset as a count of remaining seconds.
	LegacyXRateLimit
)

func (l LegacyHeaders) valid() bool { return l <= LegacyXRateLimit }

// Decision is the result of evaluating every rule that applies to a request.
//
// It is a value, not a boolean, because the reason is the product: a bare bool
// cannot say which rule denied, how long to wait, whether the limiter was
// running degraded, or whether a shadow rule would have denied. It is returned
// by value and costs no allocation.
type Decision struct {
	// Allowed is whether to serve the request.
	Allowed bool

	// Reason says why, as a typed value. See [Reason].
	Reason Reason

	// Rule names the rule the reported quota belongs to: the rule that denied,
	// or on an allowed request the one with the least headroom left.
	Rule string

	// Limit is that rule's quota per window.
	Limit int64

	// Remaining is how many more events of cost one that rule would admit right
	// now.
	Remaining int64

	// ResetAfter is how long until that rule's counter is fully recovered. It
	// is a fact about the limiter and carries no jitter.
	ResetAfter time.Duration

	// RetryAfter is how long the client should wait, and is zero on an allowed
	// request. It is advice, not a fact: it carries a small positive jitter so
	// that a crowd of clients denied at the same instant does not come back at
	// the same instant. It is never earlier than the end of the effective
	// window, as the standard requires.
	RetryAfter time.Duration

	// Degraded is true when the remote backend was not answering and the
	// decision came from local state alone.
	Degraded bool

	// Shadowed is true when a rule in shadow mode would have denied this
	// request. The request was allowed and quota was consumed either way.
	Shadowed bool

	// ShadowRule names that rule.
	ShadowRule string

	policy string // precomputed RateLimit-Policy for the whole rule set
	legacy LegacyHeaders
}

// WriteHeaders writes the rate limit headers for this decision.
//
// It lives on the decision rather than in the middleware so that anyone writing
// their own middleware emits them correctly without reimplementing the format.
// The middleware in this package does nothing more than call it.
func (d Decision) WriteHeaders(h http.Header) {
	if d.Limit <= 0 {
		return // exempt, saturated, or otherwise not attributable to a quota
	}
	if d.policy != "" {
		h.Set(HeaderRateLimitPolicy, d.policy)
	}
	h.Set(HeaderRateLimit, d.rateLimitField())

	if !d.Allowed && d.RetryAfter > 0 {
		h.Set(HeaderRetryAfter, strconv.FormatInt(ceilSeconds(d.RetryAfter), 10))
	}
	d.writeLegacy(h)
}

// rateLimitField renders the RateLimit service limit item, for example
//
//	"search";r=3;t=42
func (d Decision) rateLimitField() string {
	buf := make([]byte, 0, 48)
	buf = append(buf, '"')
	buf = append(buf, d.Rule...)
	buf = append(buf, '"', ';', 'r', '=')
	buf = strconv.AppendInt(buf, max64(d.Remaining, 0), 10)
	buf = append(buf, ';', 't', '=')
	buf = strconv.AppendInt(buf, ceilSeconds(d.ResetAfter), 10)
	return string(buf)
}

func (d Decision) writeLegacy(h http.Header) {
	if d.legacy != LegacyXRateLimit {
		return
	}
	h.Set("X-RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(max64(d.Remaining, 0), 10))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(ceilSeconds(d.ResetAfter), 10))
}

// String renders the decision for logs.
func (d Decision) String() string {
	b := make([]byte, 0, 96)
	if d.Allowed {
		b = append(b, "allow"...)
	} else {
		b = append(b, "deny"...)
	}
	b = append(b, " reason="...)
	b = append(b, d.Reason.String()...)
	if d.Rule != "" {
		b = append(b, " rule="...)
		b = append(b, d.Rule...)
	}
	if d.Limit > 0 {
		b = append(b, " remaining="...)
		b = strconv.AppendInt(b, d.Remaining, 10)
		b = append(b, '/')
		b = strconv.AppendInt(b, d.Limit, 10)
	}
	if d.RetryAfter > 0 {
		b = append(b, " retry_after="...)
		b = append(b, d.RetryAfter.String()...)
	}
	if d.Degraded {
		b = append(b, " degraded"...)
	}
	if d.Shadowed {
		b = append(b, " shadow_rule="...)
		b = append(b, d.ShadowRule...)
	}
	return string(b)
}

// ceilSeconds rounds a duration up to whole seconds, never below zero. The
// standard fields and Retry-After are both integer seconds; rounding down would
// invite the client back before it has quota.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds()))
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// jitterRetryAfter adds a positive jitter to a retry hint.
//
// The jitter is one sided on purpose. The standard requires Retry-After to take
// precedence over the RateLimit field and says it should not point earlier than
// the end of the effective window, so a symmetric jitter would violate it half
// the time and tell clients to come back before they have quota. Adding only
// spreads the crowd, which is the whole point.
func jitterRetryAfter(d time.Duration, fraction float64) time.Duration {
	if d <= 0 || fraction <= 0 {
		return d
	}
	span := int64(float64(d) * fraction)
	if span <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(span+1))
}
