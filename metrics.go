package ratelimit

import "time"

// Metrics is a set of optional callbacks. Every field may be nil and a nil
// field costs a branch, so a limiter with no metrics configured does no metrics
// work at all.
//
// It is a struct of functions rather than an interface so that there is nothing
// to implement: the satellite modules hand you a filled-in Metrics. It carries
// no map and no variadic label bag, because either would allocate on the
// decision path.
//
// Rule names are safe as metric labels: the rule set is fixed when the limiter
// is built, so the label set is bounded and stable. Keys are never labels. A key
// is client-controlled data and would blow up any metrics backend, which is the
// same unbounded-cardinality failure as an unbounded key store, in a different
// place.
type Metrics struct {
	// Decision fires once per decision with its outcome and the rule the
	// reported quota belongs to. The rule may be empty when no rule matched.
	Decision func(reason Reason, rule string)

	// Denied fires when a rule actually denies a request.
	Denied func(rule string)

	// ShadowDenied fires when a rule in shadow mode would have denied. It is a
	// separate counter on purpose: comparing it against Denied is how you size
	// a rule before turning it on.
	ShadowDenied func(rule string)

	// Refunded fires when quota already taken by an earlier rule is given back
	// because a later rule denied.
	Refunded func(rule string)

	// DegradedChanged fires on each transition in and out of degraded
	// operation, never per request.
	DegradedChanged func(degraded bool)

	// Saturated fires when the key store could not allocate a cell for a new
	// key. Persistent non-zero values mean Config.Capacity is too small.
	Saturated func(rule string)

	// Evicted fires when a fully recovered cell was recycled to make room.
	Evicted func()

	// QuotaResolutionFailed fires when Rule.QuotaFor returned a quota that does
	// not validate, and the rule's static quota was used instead.
	QuotaResolutionFailed func(rule string)

	// DecisionLatencyLocal measures the time spent inside the decision itself.
	// The name says local because that is all it can measure.
	//
	// Setting it costs two clock reads per request. Leave it nil unless you
	// want them.
	DecisionLatencyLocal func(time.Duration)

	// StoreOccupancyLocal reports how much of this process's key store is in
	// use, sampled once per background sync rather than per request.
	StoreOccupancyLocal func(occupied, capacity int)

	// BackendSync reports the outcome of one background synchronisation round.
	BackendSync func(cells int, took time.Duration, err error)
}

func (m *Metrics) decision(reason Reason, rule string) {
	if m.Decision != nil {
		m.Decision(reason, rule)
	}
}

func (m *Metrics) denied(rule string) {
	if m.Denied != nil {
		m.Denied(rule)
	}
}

func (m *Metrics) shadowDenied(rule string) {
	if m.ShadowDenied != nil {
		m.ShadowDenied(rule)
	}
}

func (m *Metrics) degradedChanged(v bool) {
	if m.DegradedChanged != nil {
		m.DegradedChanged(v)
	}
}

func (m *Metrics) saturated(rule string) {
	if m.Saturated != nil {
		m.Saturated(rule)
	}
}

func (m *Metrics) evicted() {
	if m.Evicted != nil {
		m.Evicted()
	}
}

func (m *Metrics) quotaResolutionFailed(rule string) {
	if m.QuotaResolutionFailed != nil {
		m.QuotaResolutionFailed(rule)
	}
}
