package ratelimit

import "time"

// clock is the package's only source of time. It reports nanoseconds since the
// limiter was constructed, read from the monotonic reading of time.Now, so it
// is immune to wall-clock jumps and is virtualised by testing/synctest.
//
// It is deliberately unexported: nothing public and mutable may exist on a
// running limiter.
type clock struct {
	origin time.Time
	fake   *fakeClock // nil in production
}

func newClock() *clock { return &clock{origin: time.Now()} }

// now returns nanoseconds since origin. Never negative.
func (c *clock) now() int64 {
	if c.fake != nil {
		return c.fake.load()
	}
	return int64(time.Since(c.origin))
}

// at converts a limiter-relative instant back to a wall time. Used only for
// diagnostics, never on the decision path.
func (c *clock) at(ns int64) time.Time { return c.origin.Add(time.Duration(ns)) }
