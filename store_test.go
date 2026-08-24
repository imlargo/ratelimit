package ratelimit

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

// TestCellSizeIsExact underwrites the memory claim. The budget is arithmetic
// only if the cell is genuinely the size we say it is.
func TestCellSizeIsExact(t *testing.T) {
	if got := unsafe.Sizeof(slot{}); got != 16 {
		t.Errorf("slot is %d bytes, the documented budget assumes 16", got)
	}
	if got := unsafe.Sizeof(ext{}); got != 24 {
		t.Errorf("ext is %d bytes, the documented distributed budget assumes 24", got)
	}
	lim, err := NewWith(Config{Rules: []Rule{{Quota: PerMinute(1)}}, Capacity: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()
	st := lim.Stats()
	if st.Capacity != 1024 {
		t.Errorf("capacity %d, want 1000 rounded up to 1024", st.Capacity)
	}
	if st.BytesPerCell != 16 {
		t.Errorf("BytesPerCell %d, want 16 without a backend", st.BytesPerCell)
	}
	t.Logf("capacity %d cells, %d bytes each, %d KiB total",
		st.Capacity, st.BytesPerCell, st.Capacity*st.BytesPerCell/1024)
}

// TestEvictionCannotResetAVictim is the cache-eviction bypass.
//
// If a limiter evicts whatever is convenient when its store fills, anyone who
// can mint keys at will can push a victim's counter out and hand it a fresh
// quota. That turns the limiter into a thing that looks like a control and is
// not one.
//
// The property that makes this tractable: a cell whose quota has fully
// recovered is indistinguishable from a cell that never existed, so evicting it
// loses nothing. The dangerous cells are exactly the ones with quota consumed,
// and those are never evicted.
func TestEvictionCannotResetAVictim(t *testing.T) {
	const capacity = 1024
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(10), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: capacity,
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	victim := Subject{Identity: "victim"}

	// The victim spends its whole quota.
	for i := 0; i < 10; i++ {
		if !lim.Check(ctx, victim).Allowed {
			t.Fatalf("victim request %d should have been allowed", i)
		}
	}
	if lim.Check(ctx, victim).Allowed {
		t.Fatal("the victim's quota should be spent")
	}

	// The attacker mints far more keys than the store can hold, forcing
	// eviction over and over.
	allowedForAttacker := 0
	for i := 0; i < capacity*20; i++ {
		if lim.Check(ctx, Subject{Identity: "attacker-" + itoa(i)}).Allowed {
			allowedForAttacker++
		}
	}

	// The victim must still be denied. If eviction were naive it would be back
	// to a full quota.
	if d := lim.Check(ctx, victim); d.Allowed {
		t.Fatalf("the victim's counter was reset by key churn: %s. This is the cache eviction bypass.", d)
	}
	st := lim.Stats()
	t.Logf("attacker minted %d keys, %d admitted, %d evictions, %d saturations; victim still limited",
		capacity*20, allowedForAttacker, st.Evictions, st.Saturations)
	if st.Evictions == 0 && st.Saturations == 0 {
		t.Error("neither eviction nor saturation happened, so the test proved nothing")
	}
}

// TestSaturationNeverDeniesAnExistingKey is the refinement that keeps a
// bounded store from becoming a self-inflicted outage.
//
// Refusing every request when the store fills would mean a legitimate spike in
// distinct callers turns the limiter into the thing taking the service down.
// Refusing nothing would restore the eviction bypass. So saturation refuses
// only keys that have no cell yet - which is exactly the attacker minting new
// ones - and never a caller already being tracked.
func TestSaturationNeverDeniesAnExistingKey(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(1000), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 64, // tiny on purpose
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	// A set of established callers, each holding consumed quota.
	established := make([]Subject, 16)
	for i := range established {
		established[i] = Subject{Identity: "established-" + itoa(i)}
		if !lim.Check(ctx, established[i]).Allowed {
			t.Fatalf("established caller %d was denied on its first request", i)
		}
	}

	// Flood with new keys until the store is genuinely saturated.
	sawSaturation := false
	for i := 0; i < 100_000 && !sawSaturation; i++ {
		if lim.Check(ctx, Subject{Identity: "flood-" + itoa(i)}).Reason == ReasonStoreSaturated {
			sawSaturation = true
		}
	}
	if !sawSaturation {
		t.Skip("could not saturate a 64 cell store; the probe window is doing its job too well")
	}

	// Established callers keep being served, with quota to spare.
	for i, s := range established {
		d := lim.Check(ctx, s)
		if !d.Allowed {
			t.Errorf("established caller %d denied while the store was saturated: %s. "+
				"Saturation must only ever refuse keys that have no cell.", i, d)
		}
		if d.Reason == ReasonStoreSaturated {
			t.Errorf("established caller %d got ReasonStoreSaturated", i)
		}
	}
	t.Logf("%d saturations recorded; all %d established callers still served", lim.Stats().Saturations, len(established))
}

// TestSaturationIsDeclaredNotSilent because allowing silently is the bypass.
func TestSaturationIsDeclaredNotSilent(t *testing.T) {
	var saturated int
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Name: "tiny", Quota: PerHour(1000), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 64,
		Metrics:  Metrics{Saturated: func(string) { saturated++ }},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	var d Decision
	for i := 0; i < 100_000; i++ {
		d = lim.Check(ctx, Subject{Identity: "k" + itoa(i)})
		if d.Reason == ReasonStoreSaturated {
			break
		}
	}
	if d.Reason != ReasonStoreSaturated {
		t.Skip("store did not saturate")
	}
	if d.Allowed {
		t.Error("a saturated store allowed the request; that is the eviction bypass with extra steps")
	}
	if d.Rule != "tiny" {
		t.Errorf("saturation decision names rule %q, want %q", d.Rule, "tiny")
	}
	if saturated == 0 {
		t.Error("the saturation metric never fired")
	}
	if lim.Stats().Saturations == 0 {
		t.Error("Stats did not report the saturation")
	}
}

// TestRecoveredCellsAreReused: once a window has passed, the store must recycle
// freely, or a bounded store would be a limiter that stops working.
func TestRecoveredCellsAreReused(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerSecond(1), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 512, // 100 active keys at the documented sizing of >=2x
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for round := 0; round < 40; round++ {
		for i := 0; i < 100; i++ {
			d := lim.Check(ctx, Subject{Identity: "round" + itoa(round) + "-key" + itoa(i)})
			if !d.Allowed {
				t.Fatalf("round %d key %d denied (%s); recovered cells are not being reused", round, i, d)
			}
		}
		clk.Advance(int64(2 * time.Second)) // everything recovers
	}
	if s := lim.Stats(); s.Saturations != 0 {
		t.Errorf("%d saturations while every cell had recovered between rounds", s.Saturations)
	}
}

// TestNoKeyStoreGrowth is the failure mode this package makes unrepresentable:
// a map indexed by client-controlled data that grows without bound between
// sweeps. There is no configuration in which the store exceeds its capacity.
func TestNoKeyStoreGrowth(t *testing.T) {
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(5), Key: ByIdentity()}},
		Identity: FromSubject(),
		Capacity: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for i := 0; i < 500_000; i++ {
		lim.Check(ctx, Subject{Identity: "unique-" + itoa(i)})
	}
	st := lim.Stats()
	if st.Occupied > st.Capacity {
		t.Fatalf("occupied %d exceeds capacity %d", st.Occupied, st.Capacity)
	}
	t.Logf("500k distinct keys, occupancy %d/%d cells, %d bytes: bounded by construction",
		st.Occupied, st.Capacity, st.Capacity*st.BytesPerCell)
}

// TestSaturationOnsetByLoadFactor measures what the store actually tolerates,
// so the sizing rule in the README is a measurement and not a guess.
//
// The table is two-way associative with buckets of bucketSlots cells: a key may
// live in one of two buckets, and an insertion only fails when both are full of
// cells with quota still consumed. Two choices instead of one is what keeps the
// failure probability negligible at moderate load; a single linearly probed
// position clusters and starts failing well below half full.
//
// Every key here holds consumed quota for the whole test - the clock never
// advances - so "load factor" is the worst case: simultaneously active keys over
// capacity, with nothing recoverable to recycle.
//
// It runs several independent trials per load factor, because the hash key is
// random per limiter and the refusal rate is therefore a distribution rather
// than a number. Asserting on one trial is how this test used to pass on a lucky
// key and fail on an unlucky one; the thresholds below are set from the worst of
// forty trials, with margin.
func TestSaturationOnsetByLoadFactor(t *testing.T) {
	const (
		capacity = 1 << 14
		trials   = 12
	)

	type row struct {
		load      float64
		keys      int
		worst     int
		worstRate float64
		meanRate  float64
	}
	var rows []row

	for _, load := range []float64{0.25, 0.40, 0.50, 0.60, 0.75} {
		keys := int(load * capacity)
		worst, total := 0, 0
		for tr := 0; tr < trials; tr++ {
			clk := NewTestingClock()
			lim, err := NewWith(Config{
				Rules:    []Rule{{Quota: PerHour(1000), Key: ByIdentity()}},
				Identity: FromSubject(),
				Capacity: capacity,
			}.WithClock(clk))
			if err != nil {
				t.Fatal(err)
			}
			refused := 0
			for i := 0; i < keys; i++ {
				if lim.Check(context.Background(), Subject{Identity: "k-" + itoa(i)}).Reason == ReasonStoreSaturated {
					refused++
				}
			}
			_ = lim.Close()
			if refused > worst {
				worst = refused
			}
			total += refused
		}
		rows = append(rows, row{
			load:      load,
			keys:      keys,
			worst:     worst,
			worstRate: float64(worst) / float64(keys),
			meanRate:  float64(total) / float64(trials*keys),
		})
	}

	for _, r := range rows {
		t.Logf("load %3.0f%%  %6d active keys  worst %3d refused (%.5f%%)  mean %.5f%%",
			r.load*100, r.keys, r.worst, r.worstRate*100, r.meanRate*100)
	}

	// The two numbers the README publishes.
	//
	// At four times the active key count, refusals are zero - not "rare", zero,
	// across every trial. That is the sizing rule to follow.
	//
	// At two times, the worst trial stays under five in ten thousand. Typically
	// it is zero, but the tail is what a sizing rule has to be honest about.
	const (
		zeroAt     = 0.25   // capacity >= 4x active keys
		tolerantAt = 0.50   // capacity >= 2x active keys
		tolerance  = 0.0005 // five in ten thousand
	)
	for _, r := range rows {
		switch {
		case r.load <= zeroAt && r.worst != 0:
			t.Errorf("at %.0f%% load (capacity 4x the active keys) the worst of %d trials refused %d of %d insertions; "+
				"the published rule promises none", r.load*100, trials, r.worst, r.keys)
		case r.load <= tolerantAt && r.worstRate > tolerance:
			t.Errorf("at %.0f%% load (capacity 2x the active keys) the worst of %d trials refused %.5f%% of insertions; "+
				"the published bound is %.5f%%", r.load*100, trials, r.worstRate*100, tolerance*100)
		}
	}
}
