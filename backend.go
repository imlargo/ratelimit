package ratelimit

import (
	"context"
	"time"
)

// Demand is how much one node has been asked for on one key since it last
// synchronised.
//
// Amount is expressed as time rather than as a count, because that is what the
// algorithm's state is: a cell charged D of demand will admit nothing more until
// D has elapsed. A backend therefore needs to know nothing about quotas,
// windows, costs, algorithms or rules.
type Demand struct {
	// Key is the cell's fingerprint. It is opaque - a keyed hash of the rule and
	// the key's dimensions - and it is the only identifier a backend ever sees.
	// It is never a user identifier.
	Key uint64

	// Amount is how much was asked for since the last sync, admitted or not.
	Amount time.Duration

	// TTL is how long the backend should remember this entry.
	TTL time.Duration
}

// Share is what a backend reports back for one key: what everyone else wants.
type Share struct {
	// Key echoes the Demand's key.
	Key uint64

	// Others is the sum of Amount reported by every *other* live node for this
	// key over a comparable window. A node must never see its own contribution
	// reflected back, or the correction feeds on itself.
	Others time.Duration

	// Nodes is how many other live nodes contributed. It sets the floor under a
	// node's share, so a node that has been idle can still serve the request
	// that wakes it up.
	Nodes int
}

// Backend is the whole remote seam. One method.
//
// It deliberately knows nothing about rules, selectors, quotas, costs, headers
// or algorithms: it exchanges demand for what everyone else is demanding. That
// is what keeps a new backend a few hundred lines instead of a reimplementation
// of this package, and it is why local state always decides and the remote only
// adjusts how much of the quota this node may hand out.
//
// # Why demand and not counters
//
// The obvious design - publish what you consumed, read back the global total,
// subtract - does not bound anything. Each node's local counter recovers at
// wall-clock speed, so N nodes sustain N times the configured rate no matter how
// often they synchronise. Measured, not argued: see
// TestOvershootBoundIsPublished.
//
// So instead each node reports what it is being asked for and enforces its share
// of the quota locally. Nodes under load get most of it, idle nodes keep a small
// floor, and the sustained total is the configured rate. This is the shape
// Google's Doorman and the distributed rate limiting literature settled on, and
// it is the only one that makes a published bound possible.
//
// # Contract
//
//   - Sync records each Demand against (key, node) and returns, for each input
//     key in the same order, the sum over every other live node and how many
//     there were. Same length, same order as the input.
//   - Entries from nodes that have stopped reporting must age out, over a few
//     sync intervals. A node that dies must not hold quota forever.
//   - Use the store's own clock for expiry, not the caller's. Clock skew between
//     nodes then affects nothing.
//   - Sync must honour the context deadline. Nothing on the decision path waits
//     on it, but a Sync that hangs stops corrections from ever landing.
//   - Sync is called from a single goroutine, never concurrently.
//
// Returning an error is a normal, expected outcome: the limiter reports degraded
// operation, keeps deciding from local state, and keeps trying.
type Backend interface {
	Sync(ctx context.Context, node string, demand []Demand) ([]Share, error)
	Close() error
}
