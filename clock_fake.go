package ratelimit

import "sync/atomic"

// fakeClock is a manually advanced clock. It is reachable only through
// ratelimit.TestingClock, which lives in this package so that the clock seam
// stays unexported, and is documented as test-only.
type fakeClock struct{ ns atomic.Int64 }

func (f *fakeClock) load() int64     { return f.ns.Load() }
func (f *fakeClock) advance(d int64) { f.ns.Add(d) }
func (f *fakeClock) set(ns int64)    { f.ns.Store(ns) }
