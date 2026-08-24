package ratelimit

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
)

// The key store is a fixed-capacity table of two-way associative buckets.
//
// A key may live in exactly one of two buckets, so a lookup reads at most
// 2*bucketSlots cells and never walks the table. Two independent choices per key
// is what keeps the table usable when it is nearly full. A single linearly
// probed position clusters, and clustering made a table at 78% occupancy behave
// as if it were overflowing - measured, which is how this design was arrived at.
//
// The saturation rate is measured rather than derived: the obvious formula,
// (active/capacity) to the power of the probe length, assumes the cells a key
// can land in are occupied independently, and they are not. At the recommended
// sizing of capacity >= 4*active the rate is zero. See
// TestSaturationOnsetByLoadFactor for the curve.
//
// A bucket is a contiguous run of cells, so probing one is a handful of
// sequential cache line fetches rather than a scatter.
const (
	bucketSlots  = 16              // 16 * 16 bytes = 256 bytes, 4 cache lines
	minPartSlots = 8 * bucketSlots // smallest useful partition
)

// casSpinBeforeYield is how many failed compare-and-swaps to attempt before
// yielding. Contention on one hot cell is expected; livelock is not.
const casSpinBeforeYield = 32

// slot is one cell: a key fingerprint and a theoretical arrival time.
//
// Exactly 16 bytes. This is the whole of the per-key state in single-node mode,
// and it is why the memory budget is arithmetic rather than an estimate.
type slot struct {
	fp  atomic.Uint64 // 0 means never claimed
	tat atomic.Int64  // nanoseconds since limiter origin; <= now means fully recovered
}

// ext is the extra per-cell state only distributed mode needs. It is allocated
// only when a Backend is configured, so a single-node limiter pays nothing for
// the abstraction existing.
//
// 24 bytes, so a distributed cell costs 40 bytes in total and the memory budget
// stays arithmetic.
type ext struct {
	// demand is how much this node has been asked for since the last sync, in
	// nanoseconds of unscaled emission, counting denied attempts as well as
	// admitted ones. Counting only what was admitted would starve a throttled
	// node: less admitted, less demand reported, a smaller share, less
	// admitted.
	demand atomic.Int64

	// emission is this node's allocated emission interval for the cell, which is
	// the global one divided by this node's share of the quota. Zero means no
	// allocation has landed yet and the global interval applies.
	//
	// Scaling the emission scales the rate and the burst by the same factor,
	// because the burst is the tolerance divided by the emission and the
	// tolerance does not change.
	emission atomic.Int64

	rule atomic.Uint32
	_    [4]byte
}

// partition owns a contiguous run of buckets. Its mutex is taken only to claim
// or recycle a cell; a request whose key already has one never touches it.
type partition struct {
	mu sync.Mutex
	_  [56]byte // keep partitions off each other's cache lines
}

type store struct {
	slots []slot
	ext   []ext // nil unless a backend is configured
	parts []partition

	clk *clock

	partMask       uint64 // partitions-1
	bucketsPerPart uint64
	bucketMask     uint64 // bucketsPerPart-1

	occupied    atomic.Int64
	evictions   atomic.Int64
	saturations atomic.Int64
}

func newStore(capacity int, withExt bool, clk *clock) *store {
	cap2 := uint64(minPartSlots)
	if capacity > minPartSlots {
		cap2 = uint64(1) << bits.Len64(uint64(capacity-1))
	}

	parts := uint64(1) << bits.Len(uint(4*runtime.GOMAXPROCS(0)-1))
	for parts > 1 && cap2/parts < minPartSlots {
		parts /= 2
	}

	s := &store{
		slots:          make([]slot, cap2),
		parts:          make([]partition, parts),
		clk:            clk,
		partMask:       parts - 1,
		bucketsPerPart: cap2 / parts / bucketSlots,
	}
	s.bucketMask = s.bucketsPerPart - 1
	if withExt {
		s.ext = make([]ext, cap2)
	}
	return s
}

// Capacity is the exact number of cells the store can hold.
func (s *store) Capacity() int { return len(s.slots) }

// Occupied is the number of cells ever claimed. Cells are recycled in place and
// never released, so this only grows and settles at the working set.
func (s *store) Occupied() int { return int(s.occupied.Load()) }

// mix64 is the SplitMix64 finaliser. It gives the second bucket choice a hash
// that is independent of the first, which is what makes two choices worth more
// than one.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// locate picks the partition and the two candidate buckets for a fingerprint.
// The two buckets are always inside the same partition, so one lock covers both.
func (s *store) locate(fp uint64) (part *partition, b1, b2 uint64) {
	p := (fp >> 32) & s.partMask
	base := p * s.bucketsPerPart
	b1 = base + (fp & s.bucketMask)
	b2 = base + (mix64(fp) & s.bucketMask)
	return &s.parts[p], b1, b2
}

// outcome is what the store reports back about one cell operation.
type outcome struct {
	// effTat is the effective theoretical arrival time after the operation,
	// including any remote correction. Everything reported to the client is
	// derived from it.
	effTat int64
	// emission is the interval actually charged, which is the cell's allocated
	// one in distributed mode.
	emission int64
	// slot is where the cell lives, so a refund is a direct index rather than a
	// second lookup. It is only valid when allowed is true, and it stays valid
	// until the refund: a cell that was just consumed cannot be recycled,
	// because only a fully recovered cell ever is.
	slot uint32
	// evicted is set when this operation recycled a fully recovered cell to make
	// room. The store counts evictions itself; this is how the limiter learns
	// about one in time to report it.
	evicted   bool
	allowed   bool
	saturated bool
}

// consume applies one GCRA admission of cost events against the cell for fp,
// creating the cell if necessary.
//
// The emission interval it charges is the cell's allocated one when a backend
// has handed one down, and the rule's global one otherwise. It reports which it
// used, so the caller derives what it tells the client from the same number it
// enforced.
//
// When dry is true the admission test runs in full but the cell is never
// written, which is what a peek needs: the answer to "would this be admitted",
// not "is the cell exactly at its limit".
func (s *store) consume(fp uint64, rule uint32, now, cost, emission, tau int64, dry bool) outcome {
	if fp == 0 {
		fp = 1 // 0 is the "never claimed" sentinel
	}
	part, b1, b2 := s.locate(fp)

	// Fast path: a cell that already exists. Lock free, and the only path a
	// steady state request takes.
	if idx, ok := s.find(b1, b2, fp); ok {
		return s.apply(idx, now, cost, emission, tau, dry)
	}
	if dry {
		// A peek must not bring a cell into existence, or asking about a key
		// would be a way to fill the store.
		inc := cost * emission
		return outcome{effTat: now, emission: emission, allowed: inc <= tau}
	}
	return s.claim(part, b1, b2, fp, rule, now, cost, emission, tau)
}

func (s *store) find(b1, b2, fp uint64) (uint64, bool) {
	for i := uint64(0); i < bucketSlots; i++ {
		if s.slots[b1*bucketSlots+i].fp.Load() == fp {
			return b1*bucketSlots + i, true
		}
	}
	if b2 == b1 {
		return 0, false
	}
	for i := uint64(0); i < bucketSlots; i++ {
		if s.slots[b2*bucketSlots+i].fp.Load() == fp {
			return b2*bucketSlots + i, true
		}
	}
	return 0, false
}

// apply runs the GCRA admission test against an existing cell.
//
// The invariant that makes this safe under contention, and that
// golang.org/x/time/rate violates, is that the value written is always strictly
// greater than the value read: max(stored, now) + increment. A stale clock can
// therefore only make a decision stricter; there is no path that rewinds a cell
// and hands out extra quota. The clock is re-read on every retry so that
// "stricter" stays negligible.
func (s *store) apply(idx uint64, now, cost, emission, tau int64, dry bool) outcome {
	sl := &s.slots[idx]
	globalEmission := emission

	if s.ext != nil {
		if allocated := s.ext[idx].emission.Load(); allocated > 0 {
			emission = allocated
		}
		if !dry {
			// Demand is what was asked for, in global units, whether or not it
			// ends up granted. Global units so that the signal a node publishes
			// does not depend on the allocation it was last given, which would
			// make the allocation feed on itself.
			s.ext[idx].demand.Add(cost * globalEmission)
		}
	}
	inc := cost * emission

	for spins := 0; ; spins++ {
		old := sl.tat.Load()
		base := old
		if base < now {
			base = now // the cell has fully recovered
		}
		newEff := base + inc

		if newEff-tau > now {
			// Would arrive ahead of what the burst tolerance permits.
			return outcome{effTat: base, emission: emission, allowed: false}
		}
		if dry || inc == 0 {
			return outcome{effTat: base, emission: emission, slot: uint32(idx), allowed: true}
		}
		if sl.tat.CompareAndSwap(old, newEff) {
			return outcome{effTat: newEff, emission: emission, slot: uint32(idx), allowed: true}
		}
		if spins > 0 && spins%casSpinBeforeYield == 0 {
			runtime.Gosched()
		}
		now = s.clk.now()
	}
}

// claim finds or makes room for a cell, under the partition lock. Only a
// request whose key has no cell yet gets here.
func (s *store) claim(part *partition, b1, b2, fp uint64, rule uint32, now, cost, emission, tau int64) outcome {
	part.mu.Lock()

	// Someone may have inserted the key while we waited for the lock.
	if idx, ok := s.find(b1, b2, fp); ok {
		part.mu.Unlock()
		return s.apply(idx, now, cost, emission, tau, false)
	}

	// An empty cell is free real estate.
	if idx, ok := s.scan(b1, b2, func(sl *slot) bool { return sl.fp.Load() == 0 }); ok {
		s.take(idx, fp, rule)
		s.occupied.Add(1)
		part.mu.Unlock()
		return s.apply(idx, now, cost, emission, tau, false)
	}

	// No empty cell. Recycle one whose quota has fully recovered: such a cell is
	// indistinguishable from one that never existed, so discarding it loses no
	// information and cannot reset a victim's counter.
	//
	// The recycled key may have one decision in flight that lands after the
	// swap. Because only a fully recovered cell is ever recycled, that decision
	// was within the recycled key's own burst allowance anyway, so the race
	// cannot over-admit.
	if idx, ok := s.scan(b1, b2, func(sl *slot) bool { return sl.tat.Load() <= now }); ok {
		s.take(idx, fp, rule)
		s.evictions.Add(1)
		part.mu.Unlock()
		out := s.apply(idx, now, cost, emission, tau, false)
		out.evicted = true
		return out
	}

	// Every candidate cell holds a key with quota still consumed. Refusing here
	// is what makes recycling unusable as a bypass, and it only ever refuses a
	// key that has no cell: every key that already has one took the fast path
	// and never reaches this code.
	part.mu.Unlock()
	s.saturations.Add(1)
	return outcome{saturated: true}
}

// scan looks for a cell matching pred across both candidate buckets. Called
// with the partition lock held.
func (s *store) scan(b1, b2 uint64, pred func(*slot) bool) (uint64, bool) {
	for i := uint64(0); i < bucketSlots; i++ {
		idx := b1*bucketSlots + i
		if pred(&s.slots[idx]) {
			return idx, true
		}
	}
	if b2 == b1 {
		return 0, false
	}
	for i := uint64(0); i < bucketSlots; i++ {
		idx := b2*bucketSlots + i
		if pred(&s.slots[idx]) {
			return idx, true
		}
	}
	return 0, false
}

// take repoints a cell at a new key. Called with the partition lock held.
func (s *store) take(idx, fp uint64, rule uint32) {
	s.slots[idx].tat.Store(0)
	if s.ext != nil {
		s.ext[idx].demand.Store(0)
		s.ext[idx].emission.Store(0)
		s.ext[idx].rule.Store(rule)
	}
	s.slots[idx].fp.Store(fp) // published last, so a reader never sees a torn cell
}

// refund gives back dec nanoseconds of consumed quota to a cell we just
// consumed from. It never pushes a cell below fully recovered.
func (s *store) refund(idx uint32, now, dec int64) {
	if dec <= 0 {
		return
	}
	sl := &s.slots[idx]
	for spins := 0; ; spins++ {
		old := sl.tat.Load()
		next := old - dec
		if next < now {
			next = now
		}
		if next >= old || sl.tat.CompareAndSwap(old, next) {
			return
		}
		if spins > 0 && spins%casSpinBeforeYield == 0 {
			runtime.Gosched()
		}
	}
}

// CompareAndSwap swaps a cell's arrival time.
func (sl *slot) CompareAndSwap(old, next int64) bool { return sl.tat.CompareAndSwap(old, next) }

// level reports how much quota a cell has consumed, in nanoseconds, counting
// only what this node admitted. Zero means fully recovered.
func (s *store) level(idx int, now int64) int64 {
	l := s.slots[idx].tat.Load() - now
	if l < 0 {
		return 0
	}
	return l
}
