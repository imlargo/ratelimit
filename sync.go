package ratelimit

// The background loop that keeps a node's share of the quota up to date, and
// the arithmetic that turns everyone's demand into this node's emission
// interval.
//
// None of this is on the decision path. It is the only goroutine this package
// ever starts, it exists only when a Backend is configured, and it is stopped by
// Limiter.Close.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"time"
)

func randomNodeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "node"
	}
	return hex.EncodeToString(b[:])
}

// syncLoop is the only goroutine this package ever starts, and it exists only
// when a Backend is configured. It is stopped by [Limiter.Close].
func (l *Limiter) syncLoop(ctx context.Context) {
	defer close(l.syncDone)

	t := time.NewTicker(l.syncInterval)
	defer t.Stop()

	// Buffers are reused across rounds, so a steady state round allocates
	// nothing.
	var (
		batch   []Demand
		indices []uint64
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			batch, indices = l.syncOnce(ctx, batch, indices)
		}
	}
}

func (l *Limiter) syncOnce(ctx context.Context, batch []Demand, indices []uint64) ([]Demand, []uint64) {
	batch = batch[:0]
	indices = indices[:0]

	// Walk the store and collect the cells worth coordinating.
	//
	// Two different questions, answered by two different signals:
	//
	//   - Is this cell worth a round trip? Only if it is anywhere near its
	//     limit, measured as its consumed level against the burst tolerance.
	//     A key using one percent of its quota needs no coordination, and
	//     skipping it is what keeps sync traffic proportional to the keys under
	//     pressure rather than to every key the process has ever seen.
	//   - How much of the quota should this node get? Its share of everyone's
	//     demand. That signal decays by half each round instead of resetting, so
	//     a cell that goes quiet for one interval does not lose its allocation
	//     and snap back to the full global quota - which would let a caller
	//     collect the whole limit again just by pausing.
	now := l.clk.now()
	for idx := range l.store.slots {
		if l.store.slots[idx].fp.Load() == 0 {
			continue
		}
		e := &l.store.ext[idx]
		observed := e.demand.Load()
		if observed > 0 {
			e.demand.Add(-(observed - observed/2)) // halve, without losing concurrent additions
		}
		ruleIdx := e.rule.Load()
		if int(ruleIdx) >= len(l.rules) {
			continue
		}
		r := &l.rules[ruleIdx]

		if observed <= 0 {
			if e.emission.Load() != 0 {
				l.releaseAllocation(uint64(idx), r.q.emission) // fully decayed: hand the quota back
			}
			continue
		}
		if l.store.level(idx, now) <= int64(l.syncThreshold*float64(r.q.tau)) {
			continue // in use, but nowhere near its limit
		}
		batch = append(batch, Demand{
			Key:    l.store.slots[idx].fp.Load(),
			Amount: time.Duration(observed),
			TTL:    time.Duration(r.q.window) + l.syncInterval*4,
		})
		indices = append(indices, uint64(idx))
	}

	if l.metrics.StoreOccupancyLocal != nil {
		l.metrics.StoreOccupancyLocal(l.store.Occupied(), l.store.Capacity())
	}

	// A round with nothing to say still proves the backend is reachable, so we
	// make the call anyway rather than reporting a healthy backend we have not
	// spoken to.
	callCtx, cancel := context.WithTimeout(ctx, l.syncInterval)
	start := time.Now()
	view, err := l.backend.Sync(callCtx, l.nodeID, batch)
	took := time.Since(start)
	cancel()

	if err == nil && len(view) != len(batch) {
		err = &badBackendError{want: len(batch), got: len(view)}
	}
	if l.metrics.BackendSync != nil {
		l.metrics.BackendSync(len(batch), took, err)
	}

	if err != nil {
		l.enterDegraded(err)
		return batch, indices
	}
	l.leaveDegraded()

	for i := range view {
		idx := indices[i]
		mine := int64(batch[i].Amount)
		others := int64(view[i].Others)
		if others < 0 {
			others = 0
		}
		nodes := view[i].Nodes
		if nodes < 0 {
			nodes = 0
		}
		ruleIdx := l.store.ext[idx].rule.Load()
		if int(ruleIdx) >= len(l.rules) {
			continue
		}
		base := l.rules[ruleIdx].q.emission
		l.setAllocation(idx, base, allocate(base, mine, others, nodes))
	}
	return batch, indices
}

type badBackendError struct{ want, got int }

func (e *badBackendError) Error() string {
	return "ratelimit: backend returned " + itoa(e.got) + " cells for " + itoa(e.want) +
		"; Sync must return one entry per input cell, in order"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// enterDegraded records that the backend is not answering.
//
// Losing the backend is not an error path. The decision path reads a correction
// that has simply stopped being updated, which leaves the limiter in
// single-node mode: the most exercised configuration this package has, rather
// than a fallback branch that only ever runs during an incident and that nobody
// has tested.
//
// It is, however, a declared condition. Every decision taken this way says so,
// a metric moves, and the log says it once - on the transition, never per
// request.
func (l *Limiter) enterDegraded(err error) {
	if l.degraded.Swap(true) {
		return
	}
	l.metrics.degradedChanged(true)
	l.log.Warn("ratelimit: the rate limit backend is not answering, so limits are now enforced per process only. "+
		"Requests allowed while this lasts are reported with ReasonAllowedDegraded. "+
		"Expect up to nodes*rate*sync_interval of overshoot per key until it recovers.",
		"error", err, "sync_interval", l.syncInterval)
}

func (l *Limiter) leaveDegraded() {
	if !l.degraded.Swap(false) {
		return
	}
	l.metrics.degradedChanged(false)
	// Nothing accumulated locally is pushed on recovery. The degraded window
	// was over-admission and is declared as such; replaying it would produce a
	// synchronised wall of denials the moment the incident ended.
	l.log.Info("ratelimit: the rate limit backend is answering again. " +
		"Consumption accumulated while degraded is not replayed, so there is no denial spike.")
}

// shareFloorDivisor sets the smallest slice a node is guaranteed: one over this
// many times an even split.
//
// A node with no recent demand would otherwise be allocated nothing and could
// not serve the request that wakes it up. The floor costs a bounded amount of
// overshoot - at most one extra even split's worth spread over every idle node -
// and it is in the published bound.
const shareFloorDivisor = 4

// allocate turns this node's demand and everyone else's into the emission
// interval this node will enforce.
//
// Scaling the emission interval by the reciprocal of a node's share scales both
// its rate and its burst by that share, because the burst is the tolerance
// divided by the emission and the tolerance is untouched.
func allocate(baseEmission, mine, others int64, otherNodes int) int64 {
	total := mine + others
	if total <= 0 {
		return 0 // nothing is being asked for anywhere; no allocation needed
	}
	nodes := otherNodes + 1

	// share = my demand as a fraction of all of it, never below a floor.
	share := float64(mine) / float64(total)
	floor := 1 / (float64(nodes) * shareFloorDivisor)
	if share < floor {
		share = floor
	}
	if share >= 1 {
		return 0 // the whole quota is ours; the global interval already says so
	}
	scaled := float64(baseEmission) / share
	if scaled > float64(maxHorizon) {
		scaled = float64(maxHorizon)
	}
	e := int64(scaled)
	if e < baseEmission {
		e = baseEmission
	}
	return e
}

// setAllocation installs a new emission interval for a cell and rescales what
// the cell has already consumed to match.
//
// Without the rescale, tightening a node's share would silently re-value the
// quota it had already spent: consumption recorded at the global interval would
// be read back against a longer one and look like fewer events, handing the node
// a fresh slice of burst every time its allocation moved. Scaling the level by
// the same ratio keeps the number of events consumed invariant.
func (l *Limiter) setAllocation(idx uint64, baseEmission, newEmission int64) {
	e := &l.store.ext[idx]
	old := e.emission.Load()
	if old == 0 {
		old = baseEmission
	}
	next := newEmission
	if next == 0 {
		next = baseEmission
	}
	e.emission.Store(newEmission)
	l.rescaleLevel(idx, old, next)
}

func (l *Limiter) releaseAllocation(idx uint64, baseEmission int64) {
	e := &l.store.ext[idx]
	old := e.emission.Load()
	if old == 0 {
		return
	}
	e.emission.Store(0)
	l.rescaleLevel(idx, old, baseEmission)
}

// rescaleLevel multiplies a cell's consumed level by to/from, so the count of
// events it represents does not change when the interval charged for them does.
func (l *Limiter) rescaleLevel(idx uint64, from, to int64) {
	if from <= 0 || to <= 0 || from == to {
		return
	}
	sl := &l.store.slots[idx]
	for spins := 0; ; spins++ {
		now := l.clk.now()
		old := sl.tat.Load()
		level := old - now
		if level <= 0 {
			return // fully recovered; there is nothing to re-value
		}
		scaled := int64(float64(level) * float64(to) / float64(from))
		if scaled > maxHorizon {
			scaled = maxHorizon
		}
		if sl.CompareAndSwap(old, now+scaled) {
			return
		}
		// This runs on the sync goroutine against a cell that request handlers
		// are writing to. Yield rather than spin: losing a rescale costs a
		// little accuracy for one interval, starving the runtime costs more.
		if spins%casSpinBeforeYield == 0 {
			runtime.Gosched()
		}
		if spins > 4*casSpinBeforeYield {
			return
		}
	}
}
