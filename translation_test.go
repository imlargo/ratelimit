package ratelimit

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// hammer drains everything the limiter will give, stepping the clock, and
// reports how many events were admitted per step window.
func hammer(t *testing.T, lim *Limiter, clk *TestingClock, s Subject, total, step time.Duration) (admitted int) {
	t.Helper()
	ctx := context.Background()
	for elapsed := time.Duration(0); elapsed < total; elapsed += step {
		for {
			d := lim.Check(ctx, s)
			if !d.Allowed {
				break
			}
			admitted++
			if admitted > 5_000_000 {
				t.Fatal("runaway: the limiter is admitting without bound")
			}
		}
		clk.Advance(int64(step))
	}
	return admitted
}

// TestConfigurationTranslation is the first test in this package, and the
// reason it is first.
//
// The most expensive way to get a rate limiter wrong is to mistranslate the
// configuration into the algorithm's parameters: expressing "N events per
// window W" as one event every W instead of one event every W/N. "100 per
// minute" then really limits to 1 per minute, and the mistake stays hidden
// behind the initial burst for the whole first window.
//
// This table asserts observed behaviour, not internal parameters.
func TestConfigurationTranslation(t *testing.T) {
	cases := []struct {
		name   string
		quota  Quota
		window time.Duration
	}{
		{"100/min", PerMinute(100), time.Minute},
		{"1/min", PerMinute(1), time.Minute},
		{"10/s", PerSecond(10), time.Second},
		{"1000/min", PerMinute(1000), time.Minute},
		{"5/hour", PerHour(5), time.Hour},
		{"60/min", PerMinute(60), time.Minute},
		{"7/s", PerSecond(7), time.Second},
		{"100/min burst 10", PerMinute(100).WithBurst(10), time.Minute},
		{"250 per 30s", Per(250, 30*time.Second), 30 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := NewTestingClock()
			lim, err := NewWith(Config{
				Rules:            []Rule{{Quota: tc.quota, Key: ByIdentity()}},
				Identity:         IdentityFromSubject,
				RetryAfterJitter: NoJitter,
			}.WithClock(clk))
			if err != nil {
				t.Fatal(err)
			}
			defer lim.Close()

			s := Subject{Identity: "u1", Path: "/"}
			step := tc.window / 1000
			if step < time.Nanosecond {
				step = time.Nanosecond
			}

			// Burn the initial burst so the cell is at its steady state.
			first := hammer(t, lim, clk, s, tc.window, step)

			// Now measure a full window in steady state. This is the number
			// that has to equal the configured quota.
			steady := hammer(t, lim, clk, s, tc.window, step)

			limit := int(tc.quota.Limit())
			// One event of slack: where a window boundary falls relative to the
			// nominal schedule can include or exclude a single event.
			if steady < limit-1 || steady > limit+1 {
				t.Errorf("steady state admitted %d events per %v, want %d (+/-1). "+
					"This is the configuration-to-primitive mistranslation bug.",
					steady, tc.window, limit)
			}

			// And the published first-window bound has to hold.
			bound := tc.quota.FirstWindowFactor()
			got := float64(first) / float64(limit)
			if got > bound+0.02 {
				t.Errorf("first window admitted %d events (%.2fx the quota), "+
					"which exceeds the published bound of %.2fx", first, got, bound)
			}
			t.Logf("first window %d (%.2fx, bound %.2fx), steady %d (want %d)",
				first, got, bound, steady, limit)
		})
	}
}

// TestFirstWindowBoundIsPublishedAndTight checks that the documented
// 1+burst/limit bound is not just an upper bound but the actual behaviour, so
// the README is not quietly conservative either.
func TestFirstWindowBoundIsPublishedAndTight(t *testing.T) {
	for _, burst := range []int{1, 5, 10, 50, 100} {
		clk := NewTestingClock()
		q := PerMinute(100).WithBurst(burst)
		lim, err := NewWith(Config{
			Rules:    []Rule{{Quota: q, Key: ByIdentity()}},
			Identity: IdentityFromSubject,
		}.WithClock(clk))
		if err != nil {
			t.Fatal(err)
		}
		first := hammer(t, lim, clk, Subject{Identity: "u"}, time.Minute, time.Millisecond)
		_ = lim.Close()

		want := q.FirstWindowFactor()
		got := float64(first) / 100
		if got > want+0.02 || got < want-0.05 {
			t.Errorf("burst %d: first window factor %.3f, documented %.3f", burst, got, want)
		}
		t.Logf("burst %3d: first window %3d events = %.2fx (documented %.2fx)", burst, first, got, want)
	}
}

// TestZeroBurstIsRefused pins down a trap that is easy to build and impossible
// to debug: with a burst tolerance of zero, GCRA rejects the very first event
// against an idle cell and then every event after it, forever.
func TestZeroBurstIsRefused(t *testing.T) {
	_, err := New(Rule{Quota: PerMinute(100).WithBurst(0)})
	if err == nil {
		t.Fatal("a burst of 0 was accepted; it admits nothing at all and must fail at build time")
	}
	t.Log(err)
}

// TestQuotaValidation covers the rest of the ways a quota can be wrong. Every
// one of them fails when the limiter is built, never on the first request.
func TestQuotaValidation(t *testing.T) {
	bad := []struct {
		name string
		rule Rule
	}{
		{"no quota", Rule{}},
		{"zero limit", Rule{Quota: PerMinute(0)}},
		{"negative limit", Rule{Quota: PerMinute(-5)}},
		{"zero window", Rule{Quota: Per(10, 0)}},
		{"negative window", Rule{Quota: Per(10, -time.Second)}},
		{"sub-nanosecond emission", Rule{Quota: Per(1_000_000_000, time.Nanosecond)}},
		{"negative cost", Rule{Quota: PerMinute(10), Cost: -1}},
		{"cost above burst", Rule{Quota: PerMinute(10), Cost: 11}},
		{"exempt with quota", Rule{Exempt: true, Quota: PerMinute(10)}},
		{"exempt and shadow", Rule{Exempt: true, Shadow: true}},
		{"exempt with cost", Rule{Exempt: true, Cost: 3}},
		{"identity without resolver", Rule{Quota: PerMinute(10), Key: ByIdentity()}},
		{"ip without trusted proxies", Rule{Quota: PerMinute(10), Key: ByIP()}},
		{"bad cidr", Rule{Quota: PerMinute(10), Key: ByIP("not-a-cidr")}},
		{"bad selector", Rule{Quota: PerMinute(10), Selector: "/{"}},
		{"unknown method", Rule{Quota: PerMinute(10), Selector: "GTE /api/"}},
		{"lowercase method", Rule{Quota: PerMinute(10), Selector: "get /api/"}},
		{"name with quote", Rule{Quota: PerMinute(10), Name: `bad"name`}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.rule)
			if err == nil {
				t.Fatalf("%s was accepted; it must fail at build time", tc.name)
			}
			if len(err.Error()) < 30 {
				t.Errorf("error is too terse to act on: %q", err)
			}
			t.Log(err)
		})
	}

	t.Run("no rules", func(t *testing.T) {
		if _, err := New(); err == nil {
			t.Fatal("a limiter with no rules was accepted")
		}
	})
	t.Run("duplicate names", func(t *testing.T) {
		_, err := New(
			Rule{Name: "same", Quota: PerMinute(1)},
			Rule{Name: "same", Quota: PerMinute(2)},
		)
		if err == nil {
			t.Fatal("duplicate rule names were accepted; they label metrics and headers")
		}
	})
	t.Run("too many rules", func(t *testing.T) {
		rules := make([]Rule, maxRules+1)
		for i := range rules {
			rules[i] = Rule{Name: fmt.Sprintf("r%d", i), Quota: PerMinute(1)}
		}
		if _, err := New(rules...); err == nil {
			t.Fatal("a rule table beyond the documented maximum was accepted")
		}
	})
}
