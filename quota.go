package ratelimit

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Quota is how much a rule allows: n events per window, with an instantaneous
// burst allowance.
//
// The zero Quota is invalid. Build one with [PerSecond], [PerMinute],
// [PerHour], [PerDay] or [Per].
//
// # What "n per window" means here
//
// A Quota compiles to a GCRA cell: events are admitted on a nominal schedule of
// one every window/n, and up to burst events may arrive ahead of that schedule.
// The default burst is n, so PerMinute(100) admits 100 events immediately and
// then one every 600ms.
//
// That default is what most callers expect, and it has a cost that is published
// rather than hidden: from a fully recovered cell, a client that hammers as
// fast as it can is admitted up to 1 + burst/n times the quota inside the first
// window (1.99x for the default burst), settling to exactly 1.00x in steady
// state. See [Quota.FirstWindowFactor].
//
// A Quota is NOT "no more than n in any moving window of W". If you need that
// guarantee, this package does not provide it; see the README.
type Quota struct {
	n      int64
	window time.Duration
	// burstP1 is the burst plus one, so that the zero value means "unset" and
	// WithBurst(0) is still distinguishable from it. A burst of zero admits
	// nothing at all and has to be rejected, not silently read as the default.
	burstP1 int64
}

// PerSecond allows n events per second.
func PerSecond(n int) Quota { return Per(n, time.Second) }

// PerMinute allows n events per minute.
func PerMinute(n int) Quota { return Per(n, time.Minute) }

// PerHour allows n events per hour.
func PerHour(n int) Quota { return Per(n, time.Hour) }

// PerDay allows n events per 24 hours.
func PerDay(n int) Quota { return Per(n, 24*time.Hour) }

// Per allows n events per window.
func Per(n int, window time.Duration) Quota {
	return Quota{n: int64(n), window: window}
}

// WithBurst overrides how many events may arrive ahead of the nominal
// schedule. The default is the full quota.
//
// Lowering the burst lowers the first-window overshoot: with burst b and quota
// n, the bound is 1 + b/n. Burst must be at least 1; a burst of 0 would admit
// nothing at all.
func (q Quota) WithBurst(burst int) Quota {
	q.burstP1 = int64(burst) + 1
	return q
}

// Limit is the number of events allowed per window.
func (q Quota) Limit() int64 { return q.n }

// Window is the period the limit applies to.
func (q Quota) Window() time.Duration { return q.window }

// Burst is the instantaneous burst allowance, defaulting to the full quota.
func (q Quota) Burst() int64 {
	if q.burstP1 == 0 {
		return q.n
	}
	return q.burstP1 - 1
}

// FirstWindowFactor is the published upper bound on how much of the quota a
// single client can consume inside one window, starting from a fully recovered
// cell: 1 + burst/limit. In steady state the factor is exactly 1.
func (q Quota) FirstWindowFactor() float64 {
	if q.n <= 0 {
		return 0
	}
	return 1 + float64(q.Burst())/float64(q.n)
}

func (q Quota) String() string {
	if b := q.Burst(); b != q.n {
		return fmt.Sprintf("%d/%s burst %d", q.n, shortDur(q.window), b)
	}
	return fmt.Sprintf("%d/%s", q.n, shortDur(q.window))
}

func shortDur(d time.Duration) string {
	switch d {
	case time.Second:
		return "s"
	case time.Minute:
		return "m"
	case time.Hour:
		return "h"
	case 24 * time.Hour:
		return "d"
	}
	return d.String()
}

// IsZero reports whether q was never configured.
func (q Quota) IsZero() bool { return q == Quota{} }

// quota is the compiled, hot-path form. All fields are nanoseconds except
// limit and burst.
type quota struct {
	limit    int64 // events per window
	burst    int64 // events admissible ahead of schedule
	window   int64 // nanoseconds
	emission int64 // nanoseconds per event = window/limit
	tau      int64 // burst tolerance = burst*emission
}

// ErrInvalidQuota is the class of all quota validation failures.
var ErrInvalidQuota = errors.New("invalid quota")

func (q Quota) compile() (quota, error) {
	if q.IsZero() {
		return quota{}, fmt.Errorf("%w: quota is not set; use ratelimit.PerMinute(n) or ratelimit.Per(n, window)", ErrInvalidQuota)
	}
	if q.n < 1 {
		return quota{}, fmt.Errorf("%w: limit is %d, must be at least 1", ErrInvalidQuota, q.n)
	}
	if q.window <= 0 {
		return quota{}, fmt.Errorf("%w: window is %v, must be positive", ErrInvalidQuota, q.window)
	}
	burst := q.Burst()
	if burst < 1 {
		return quota{}, fmt.Errorf(
			"%w: burst is %d, must be at least 1. A burst below one event rejects the very first request against an idle "+
				"counter and then every request after it, forever; it is not strict pacing, it is a limiter that admits nothing. "+
				"For the tightest useful pacing use WithBurst(1)", ErrInvalidQuota, burst)
	}

	window := int64(q.window)
	emission := window / q.n
	if emission < 1 {
		return quota{}, fmt.Errorf(
			"%w: %d events per %v is one event every %v, which is below nanosecond resolution; widen the window or lower the limit",
			ErrInvalidQuota, q.n, q.window, time.Duration(window)/time.Duration(q.n))
	}
	// tau = burst*emission must not overflow, and must stay well inside the
	// int64 nanosecond horizon so that now+tau never wraps.
	if burst > math.MaxInt64/emission {
		return quota{}, fmt.Errorf("%w: burst %d times %v overflows the time representation", ErrInvalidQuota, burst, time.Duration(emission))
	}
	tau := burst * emission
	if tau > maxHorizon {
		return quota{}, fmt.Errorf("%w: burst tolerance %v exceeds the %v limiter horizon", ErrInvalidQuota, time.Duration(tau), time.Duration(maxHorizon))
	}
	return quota{
		limit:    q.n,
		burst:    burst,
		window:   window,
		emission: emission,
		tau:      tau,
	}, nil
}

// maxHorizon caps any single tolerance so that arithmetic on the monotonic
// nanosecond clock cannot overflow. int64 nanoseconds is ~292 years from the
// limiter's construction; we reserve a generous margin.
const maxHorizon = int64(100 * 365 * 24 * time.Hour)
