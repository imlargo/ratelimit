package ratelimit

// The manually advanced clock is test-only surface. Defining the exported
// wrappers in a test file keeps them out of the package's public API while the
// seam itself stays unexported in clock_fake.go, where clock.go can reach it.
//
// Production has no use for this: the real clock is monotonic and is already
// virtualised by testing/synctest.

// TestingClock is a manually advanced clock for tests. Pass one in
// [Config.testingClock] via [Config.WithTestingClock].
//
// Production code has no reason to use this: the real clock is monotonic and is
// already virtualised by testing/synctest. It exists for tests that want to
// step time deterministically without a synctest bubble.
type TestingClock struct{ f *fakeClock }

// NewTestingClock returns a clock parked at t=0.
func NewTestingClock() *TestingClock { return &TestingClock{f: &fakeClock{}} }

// Advance moves the clock forward by ns nanoseconds.
func (t *TestingClock) Advance(ns int64) { t.f.advance(ns) }

// Set parks the clock at ns nanoseconds since limiter construction.
func (t *TestingClock) Set(ns int64) { t.f.set(ns) }

// Now reports the current offset in nanoseconds.
func (t *TestingClock) Now() int64 { return t.f.load() }

// WithClock returns a copy of the config driven by a manually advanced clock.
func (c Config) WithClock(tc *TestingClock) Config {
	c.clock = tc.f
	return c
}
