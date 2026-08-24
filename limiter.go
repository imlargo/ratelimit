package ratelimit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"
)

// DefaultCapacity is how many cells the key store holds unless Config says
// otherwise. At 16 bytes per cell that is 2 MiB, room for roughly 65k
// simultaneously active keys at the recommended sizing.
const DefaultCapacity = 1 << 17

// DefaultRetryAfterJitter is the fraction of Retry-After added as positive
// jitter, so that clients denied in the same instant do not return in the same
// instant.
const DefaultRetryAfterJitter = 0.2

// NoJitter disables the Retry-After jitter. Pass it as Config.RetryAfterJitter
// when a test needs a deterministic value.
const NoJitter = -1.0

// Config is the tuning surface. Every field has a working zero value, so
// Config{Rules: rules} is equivalent to New(rules...).
type Config struct {
	// Rules is the rule table. Required.
	Rules []Rule

	// Capacity is the number of cells in the key store, rounded up to a power
	// of two. Defaults to [DefaultCapacity].
	//
	// Memory is exactly Capacity*16 bytes, or Capacity*40 with a Backend.
	//
	// Size it at four or more times the number of keys you expect to be
	// simultaneously active - that is, keys with quota consumed and not yet
	// recovered, not the total number of keys you have ever seen. At that sizing
	// no new key is ever refused, measured rather than derived. At two times,
	// the worst case is a few refusals in ten thousand insertions and the typical
	// case is none. See TestSaturationOnsetByLoadFactor for the curve.
	Capacity int

	// Identity resolves the authenticated caller. Required by any rule whose
	// key uses [Identity] or [IdentityOrIP].
	//
	// It lives here rather than on a rule because who the caller is belongs to
	// your authentication, not to an individual limit. It is called at most once
	// per request. See [IdentityFunc].
	Identity IdentityFunc

	// Tenant resolves the organisation, account or workspace. Required by any
	// rule whose key uses [Tenant].
	Tenant IdentityFunc

	// Backend turns on distributed correction. Without one the limiter enforces
	// per process, which with N replicas means N times the configured rate.
	Backend Backend

	// SyncInterval is how often local levels are published and the global view
	// read back. Defaults to one second. It appears directly in the published
	// overshoot bound: nodes * rate * SyncInterval.
	SyncInterval time.Duration

	// SyncThreshold is how much of its burst a cell must have consumed before it
	// is worth coordinating, as a fraction from 0 to 1. Zero means the default
	// of 0.25; pass a very small positive value to coordinate every cell in use.
	//
	// A key using one percent of its quota needs no coordination, and skipping
	// it is what keeps sync traffic proportional to the keys under pressure
	// rather than to every key the process has ever seen. Measured: 503 keys
	// touched, three of them near a limit, three cells published per round.
	SyncThreshold float64

	// Legacy adds a non-standard header family alongside the standard fields.
	// Defaults to [LegacyNone]; see [LegacyHeaders] for why there is no
	// default dialect.
	Legacy LegacyHeaders

	// RetryAfterJitter is the fraction of Retry-After added as positive jitter.
	// Zero means [DefaultRetryAfterJitter]; pass [NoJitter] to switch it off.
	RetryAfterJitter float64

	// Metrics is a set of optional callbacks. See [Metrics].
	Metrics Metrics

	// Logger receives the handful of operational messages this package emits:
	// entering and leaving degraded operation, and configuration that looks
	// wrong at runtime. Never per request. Defaults to slog.Default().
	Logger *slog.Logger

	// ClusterKey is a shared secret that every node in the deployment sets to
	// the same value. It is required when Backend is set and ignored otherwise.
	//
	// The key fingerprints a backend sees are derived from it, so nodes only
	// agree on which cell is which if they agree on this. It is a secret, not a
	// name: an attacker who knew it could search for a key that collides with a
	// victim's and drain the victim's quota. Treat it like a database password -
	// any random string of 16 bytes or more will do, and it never leaves the
	// process.
	//
	// Without a backend the derivation is random per process instead, which is
	// strictly stronger and needs no configuration.
	ClusterKey string

	// NodeID identifies this process to the backend. Defaults to a random
	// value, which is right unless you want stable identities in backend
	// diagnostics.
	NodeID string

	// DeniedHandler serves denied requests. It receives the decision, so it can
	// render whatever shape of error your API uses without recomputing
	// anything. Defaults to a 429 with a short plain-text body.
	//
	// The rate limit headers are already set when it runs. It must not write a
	// status other than 429 for [ReasonDeniedQuota]: 429 means the caller
	// exceeded its quota, 503 means the server cannot cope, and conflating them
	// tells clients to retry on the wrong signal.
	DeniedHandler DeniedHandler

	// OnDecision is called for every decision, before the request is served or
	// denied. It is the hook to log shadow denials through: comparing what a
	// shadow rule would have blocked against real traffic is the entire point
	// of deploying a rule that way first.
	OnDecision func(*http.Request, Decision)

	// clock is the test-only seam for a manually advanced clock. It is
	// unexported, and the wrappers that set it live in a test file, so a running
	// limiter has no public mutable state and production cannot reach it.
	clock *fakeClock
}

// Limiter evaluates a rule table against requests. It is safe for concurrent
// use and immutable once built.
type Limiter struct {
	rules  []rule
	store  *store
	clk    *clock
	hkey   hashKey
	policy string

	legacy        LegacyHeaders
	jitter        float64
	metrics       Metrics
	log           *slog.Logger
	deniedHandler DeniedHandler
	onDecision    func(*http.Request, Decision)

	backend       Backend
	syncInterval  time.Duration
	syncThreshold float64
	nodeID        string
	syncStop      context.CancelFunc
	syncDone      chan struct{}
	degraded      atomic.Bool

	closed atomic.Bool

	// One-shot operational warnings.
	privatePeers  atomic.Int64
	chainMisses   atomic.Int64
	chainHits     atomic.Int64
	proxyWarned   atomic.Bool
	quotaWarned   atomic.Bool
	needsIP       bool
	needsIdentity bool
	needsTenant   bool
	implicitKey   bool
	identity      IdentityFunc
	tenant        IdentityFunc
}

// New builds a limiter from a rule table. This is the whole of the common case.
//
//	lim, err := ratelimit.New(ratelimit.Rule{Quota: ratelimit.PerMinute(100)})
//	if err != nil {
//	    return err
//	}
//	mux.Handle("/", lim.Limit(handler))
//
// Everything that can be wrong with a rule table is wrong here, at startup,
// with an error that says what to write instead - never on the first request.
func New(rules ...Rule) (*Limiter, error) {
	return NewWith(Config{Rules: rules})
}

// NewWith builds a limiter with tuning. See [Config].
func NewWith(cfg Config) (*Limiter, error) {
	compiled, err := compileRules(cfg.Rules, cfg.Identity != nil, cfg.Tenant != nil)
	if err != nil {
		return nil, err
	}
	if !cfg.Legacy.valid() {
		return nil, fmt.Errorf("invalid config: Legacy is %d, which is not a known dialect", cfg.Legacy)
	}
	if cfg.SyncThreshold < 0 || cfg.SyncThreshold > 1 {
		return nil, fmt.Errorf("invalid config: SyncThreshold is %v, must be between 0 and 1", cfg.SyncThreshold)
	}
	if cfg.Capacity < 0 {
		return nil, fmt.Errorf("invalid config: Capacity is %d", cfg.Capacity)
	}
	if cfg.SyncInterval < 0 {
		return nil, fmt.Errorf("invalid config: SyncInterval is %v", cfg.SyncInterval)
	}

	capacity := cfg.Capacity
	if capacity == 0 {
		capacity = DefaultCapacity
	}
	jitter := cfg.RetryAfterJitter
	if jitter == 0 {
		jitter = DefaultRetryAfterJitter
	}
	if jitter < 0 {
		jitter = 0
	}
	interval := cfg.SyncInterval
	if interval == 0 {
		interval = time.Second
	}
	threshold := cfg.SyncThreshold
	if threshold == 0 {
		threshold = 0.25
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	denied := cfg.DeniedHandler
	if denied == nil {
		denied = DefaultDeniedHandler
	}

	hkey, err := deriveHashKey(cfg.ClusterKey, cfg.Backend != nil)
	if err != nil {
		return nil, err
	}

	clk := newClock()
	if cfg.clock != nil {
		clk.fake = cfg.clock
	}

	l := &Limiter{
		rules:         compiled,
		store:         newStore(capacity, cfg.Backend != nil, clk),
		clk:           clk,
		hkey:          hkey,
		policy:        policyHeader(compiled),
		legacy:        cfg.Legacy,
		jitter:        jitter,
		metrics:       cfg.Metrics,
		log:           logger,
		deniedHandler: denied,
		onDecision:    cfg.OnDecision,
		backend:       cfg.Backend,
		syncInterval:  interval,
		syncThreshold: threshold,
		nodeID:        cfg.NodeID,
		identity:      cfg.Identity,
		tenant:        cfg.Tenant,
	}
	if l.nodeID == "" {
		l.nodeID = randomNodeID()
	}
	for i := range compiled {
		if compiled[i].key.needsIP {
			l.needsIP = true
		}
		if compiled[i].key.implicit {
			l.implicitKey = true
		}
		if compiled[i].key.needsIdentity {
			l.needsIdentity = true
		}
		if compiled[i].key.needsTenant {
			l.needsTenant = true
		}
	}

	if l.backend != nil {
		ctx, cancel := context.WithCancel(context.Background())
		l.syncStop = cancel
		l.syncDone = make(chan struct{})
		go l.syncLoop(ctx)
	}
	return l, nil
}

// Close releases anything the limiter started. It is safe to call more than
// once.
//
// In single-node mode there is nothing to release: expiry is a property of the
// cell state rather than a sweep, so no limiter ever starts a goroutine you have
// to remember to stop. Close still exists, and calling it is still the right
// habit, because a limiter with a Backend does run one.
//
// Decisions taken after Close still work and still limit. What they lose is
// coordination, so they enforce the full quota per process, exactly as a limiter
// with no Backend does.
func (l *Limiter) Close() error {
	if l.closed.Swap(true) {
		return nil
	}
	if l.syncStop != nil {
		l.syncStop()
		<-l.syncDone
	}
	if l.store.ext != nil {
		// Coordination has stopped, so every cell's allocated share of the quota
		// is now frozen at whatever the last round said. Release them, which
		// leaves the limiter enforcing the full quota locally: the single-node
		// behaviour, and the only sane reading of "there is no longer anyone to
		// coordinate with". Leaving them frozen would make a limiter that is
		// still being called quietly stricter than it was configured to be.
		for idx := range l.store.ext {
			if l.store.ext[idx].emission.Load() != 0 {
				l.store.ext[idx].emission.Store(0)
			}
		}
	}
	if l.backend != nil {
		return l.backend.Close()
	}
	return nil
}

// Stats is a snapshot of the key store and the backend, for operators and for
// pull-based metrics collectors.
type Stats struct {
	// Capacity is the exact number of cells.
	Capacity int
	// Occupied is how many cells have ever been claimed. Cells are recycled in
	// place, so this only grows and settles at the working set.
	Occupied int
	// Evictions counts fully recovered cells recycled to make room. Steady
	// eviction is normal; it means the store is smaller than the key space,
	// which is the point.
	Evictions int64
	// Saturations counts new keys refused because every candidate slot held a
	// key with quota still consumed. Anything but zero means Capacity is too
	// small for the active key set.
	Saturations int64
	// Degraded reports whether the backend is currently not answering.
	Degraded bool
	// BytesPerCell is the exact per-cell footprint, so the total is arithmetic.
	BytesPerCell int
}

// Stats returns a snapshot. It is cheap and safe to call at any time.
func (l *Limiter) Stats() Stats {
	per := 16
	if l.store.ext != nil {
		per = 40 // 16 for the cell, 24 for the coordination state
	}
	return Stats{
		Capacity:     l.store.Capacity(),
		Occupied:     l.store.Occupied(),
		Evictions:    l.store.evictions.Load(),
		Saturations:  l.store.saturations.Load(),
		Degraded:     l.degraded.Load(),
		BytesPerCell: per,
	}
}

// Degraded reports whether the remote backend is currently not answering, so
// decisions are being made from local state alone.
func (l *Limiter) Degraded() bool { return l.degraded.Load() }

// Check decides for a subject built by hand, outside HTTP. Queue consumers,
// gRPC handlers and background workers use this.
//
// ctx is accepted for cancellation and for backends that may later need it.
// Nothing on the decision path waits on a remote, so a cancelled context does
// not change the answer today.
func (l *Limiter) Check(ctx context.Context, s Subject) Decision {
	_ = ctx
	s.normalise()
	return l.decide(&s, s.cost())
}

// Peek reports what Check would say without consuming anything. Useful for a
// "what is my quota" endpoint.
func (l *Limiter) Peek(s Subject) Decision {
	s.normalise()
	return l.decide(&s, -s.cost())
}

// CheckRequest decides for an HTTP request without serving it, for callers
// writing their own middleware. Use [Decision.WriteHeaders] to emit the
// headers.
func (l *Limiter) CheckRequest(r *http.Request) Decision {
	s := l.subject(r)
	return l.decide(&s, s.cost())
}

func (s *Subject) cost() int64 {
	if s.Cost > 0 {
		return s.Cost
	}
	return 1
}

// subject builds a Subject from a request, resolving the client address once
// for every rule that needs it.
func (l *Limiter) subject(r *http.Request) Subject {
	p, endsSlash := normalisePath(r.URL.Path)
	s := Subject{
		Path:    p,
		Method:  r.Method,
		Host:    r.Host,
		Request: r,
		Cost:    0,
	}
	s.pathEndsSlash = endsSlash
	if l.needsIdentity {
		if id, ok := l.identity(r); ok {
			s.Identity = id
		}
	}
	if l.needsTenant {
		if t, ok := l.tenant(r); ok {
			s.Tenant = t
		}
	}
	if l.needsIP {
		s.IP = l.resolveIP(r)
	}
	return s
}

// resolveIP finds the client address for whichever dimension asked for it. The
// dimensions themselves carry the trusted ranges, so the first one that needs an
// address decides how it is derived.
func (l *Limiter) resolveIP(r *http.Request) netip.Addr {
	for i := range l.rules {
		for j := range l.rules[i].key.dims {
			d := &l.rules[i].key.dims[j]
			switch d.kind {
			case dimClientIP, dimIdentityOrIP:
				a, ok := clientIP(r, d.trusted)
				l.noteChain(r, ok)
				return a
			}
		}
	}
	a := peerAddr(r)
	if l.implicitKey {
		l.maybeWarnImplicitPeer(a)
	}
	return a
}

// decide is the whole evaluation. It allocates nothing.
func (l *Limiter) decide(s *Subject, cost int64) Decision {
	var start time.Time
	measure := l.metrics.DecisionLatencyLocal != nil
	if measure {
		start = time.Now()
	}

	d := l.evaluate(s, cost)

	if measure {
		l.metrics.DecisionLatencyLocal(time.Since(start))
	}
	l.metrics.decision(d.Reason, d.Rule)
	switch {
	case d.Reason == ReasonDeniedQuota:
		l.metrics.denied(d.Rule)
	case d.Reason == ReasonStoreSaturated:
		l.metrics.saturated(d.Rule)
	}
	if d.Shadowed {
		l.metrics.shadowDenied(d.ShadowRule)
	}
	return d
}

// refundEntry records quota already taken, so a later denial can give it back.
//
// It is 16 bytes and the ledger is a fixed stack array, so the whole ledger is
// what a request pays for the ability to refund. Keeping the entry small
// matters: a wider entry made the zeroing of this array the single largest cost
// in the decision.
type refundEntry struct {
	slot uint32
	rule uint32
	dec  int64
}

// evaluate walks the rule table. A negative cost means peek: evaluate in full,
// write nothing.
func (l *Limiter) evaluate(s *Subject, cost int64) Decision {
	dry := cost < 0
	if dry {
		cost = -cost
	}
	now := l.clk.now()
	degraded := l.backend != nil && l.degraded.Load()

	var (
		ledger [maxRules]refundEntry
		n      int

		bindSet       bool
		bindRule      string
		bindLimit     int64
		bindRemaining int64
		bindReset     time.Duration

		shadowed   bool
		shadowRule string
	)

	for i := range l.rules {
		r := &l.rules[i]
		if !r.sel.matches(s.Method, s.Host, s.Path, s.pathEndsSlash) {
			continue
		}

		if r.exempt {
			// Exempt rules sort first, so nothing has been consumed yet.
			return l.finish(Decision{
				Allowed: true,
				Reason:  ReasonExempt,
				Rule:    r.name,
			}, degraded, shadowed, shadowRule)
		}

		q := l.quotaFor(r, s)
		c := cost
		if r.costFor != nil {
			c = r.costFor(*s)
		} else if r.cost > 1 {
			c = mulCost(r.cost, cost)
		}
		if c < 1 {
			// Zero or negative means one, as Subject.Cost and Rule.CostFor both
			// document. Silently charging one for a nonsense value is the
			// documented behaviour rather than an accident.
			c = 1
		}

		if c > q.burst {
			l.refundAll(ledger[:n], now)
			rem, reset, _ := quotaState(&q, q.emission, now, now, 0)
			return l.finish(Decision{
				Reason:     ReasonCostExceedsBurst,
				Rule:       r.name,
				Limit:      q.limit,
				Remaining:  rem,
				ResetAfter: reset,
			}, degraded, shadowed, shadowRule)
		}

		fp := r.key.hash(l.hkey, r.tag, s)
		out := l.store.consume(fp, r.idx, now, c, q.emission, q.tau, dry)
		inc := c * out.emission

		if out.evicted {
			l.metrics.evicted()
		}
		if out.saturated {
			l.refundAll(ledger[:n], now)
			return l.finish(Decision{
				Reason: ReasonStoreSaturated,
				Rule:   r.name,
				Limit:  q.limit,
			}, degraded, shadowed, shadowRule)
		}

		if !out.allowed {
			rem, reset, retry := quotaState(&q, out.emission, now, out.effTat, inc)
			if r.shadow {
				// A live rule would have denied and would have consumed
				// nothing, which is exactly what just happened. Shadow mode is
				// a reason, not a branch that skips the work.
				if !shadowed {
					shadowed, shadowRule = true, r.name
				}
				continue
			}
			l.refundAll(ledger[:n], now)
			return l.finish(Decision{
				Reason:     ReasonDeniedQuota,
				Rule:       r.name,
				Limit:      q.limit,
				Remaining:  rem,
				ResetAfter: reset,
				RetryAfter: jitterRetryAfter(retry, l.jitter),
			}, degraded, shadowed, shadowRule)
		}

		if !dry {
			ledger[n] = refundEntry{slot: out.slot, rule: r.idx, dec: inc}
			n++
		}

		// The reported quota is the binding one: the rule with the least
		// headroom left. Shadow rules are excluded, because advertising a limit
		// that does not deny would tell the client something untrue.
		if r.shadow {
			continue
		}
		rem, reset, _ := quotaState(&q, out.emission, now, out.effTat, 0)
		if !bindSet || rem < bindRemaining {
			bindSet, bindRule, bindLimit, bindRemaining, bindReset = true, r.name, q.limit, rem, reset
		}
	}

	if !bindSet {
		// Nothing matched, or only shadow rules did.
		return l.finish(Decision{Allowed: true, Reason: ReasonAllowed}, degraded, shadowed, shadowRule)
	}
	return l.finish(Decision{
		Allowed:    true,
		Reason:     ReasonAllowed,
		Rule:       bindRule,
		Limit:      bindLimit,
		Remaining:  bindRemaining,
		ResetAfter: bindReset,
	}, degraded, shadowed, shadowRule)
}

// finish stamps the cross-cutting facts onto a decision. Shadow and degraded
// are separate fields from Reason because both can be true at once and Reason
// holds one value.
func (l *Limiter) finish(d Decision, degraded, shadowed bool, shadowRule string) Decision {
	d.Allowed = d.Reason.Allows()
	d.policy = l.policy
	d.legacy = l.legacy

	if degraded {
		d.Degraded = true
	}
	if shadowed {
		d.Shadowed = true
		d.ShadowRule = shadowRule
	}
	if d.Allowed {
		switch {
		case shadowed:
			d.Reason = ReasonAllowedShadow
		case degraded:
			d.Reason = ReasonAllowedDegraded
		}
	}
	return d
}

func (l *Limiter) refundAll(entries []refundEntry, now int64) {
	// Without this, a client that keeps hitting one strict rule also burns
	// quota on every broader rule it passed on the way in, and its effective
	// limit is tighter than the one configured, unpredictably so.
	for i := range entries {
		l.store.refund(entries[i].slot, now, entries[i].dec)
		if l.metrics.Refunded != nil {
			l.metrics.Refunded(l.rules[entries[i].rule].name)
		}
	}
}

// quotaFor resolves a rule's quota, falling back to the static one and saying
// so if the resolver hands back something invalid.
func (l *Limiter) quotaFor(r *rule, s *Subject) quota {
	if r.quotaFor == nil {
		return r.q
	}
	q, err := r.quotaFor(*s).compile()
	if err != nil {
		l.metrics.quotaResolutionFailed(r.name)
		if l.quotaWarned.CompareAndSwap(false, true) {
			l.log.Warn("ratelimit: QuotaFor returned an invalid quota, falling back to the rule's static quota",
				"rule", r.name, "error", err)
		}
		return r.q
	}
	return q
}

// mulCost multiplies two costs, saturating instead of wrapping.
//
// Both operands can come from the caller: Rule.Cost from the configuration and
// Subject.Cost from the request. Wrapping would turn a nonsense cost into a
// small one and charge for that, which is a wrong answer given quietly.
// Saturating turns it into a cost no burst can cover, so it comes back as
// [ReasonCostExceedsBurst] and says so.
func mulCost(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 1
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// quotaState turns a cell's effective arrival time into the numbers a client is
// told about.
//
// remaining is how many more unit-cost events would be admitted right now.
// resetAfter is how long until the cell is fully recovered - a fact, never
// jittered. retryAfter is how long until an event of the given increment would
// be admitted, and is only meaningful after a denial.
func quotaState(q *quota, emission, now, effTat, inc int64) (remaining int64, resetAfter, retryAfter time.Duration) {
	if effTat < now {
		effTat = now
	}
	if emission <= 0 {
		emission = q.emission
	}
	remaining = (now + q.tau - effTat) / emission
	if remaining < 0 {
		remaining = 0
	}
	resetAfter = time.Duration(effTat - now)
	if inc > 0 {
		retryAfter = time.Duration(effTat + inc - q.tau - now)
		if retryAfter < 0 {
			retryAfter = 0
		}
	}
	return remaining, resetAfter, retryAfter
}

// Inspection describes one rule's view of a subject. It materialises the key as
// a readable string, which the decision path deliberately never does.
type Inspection struct {
	Rule       string
	Selector   string
	Matches    bool
	KeyLabel   string
	Key        uint64
	Quota      Quota
	Shadow     bool
	Exempt     bool
	Remaining  int64
	ResetAfter time.Duration
}

// Inspect reports what every rule makes of a subject, without consuming
// anything.
//
// This is the diagnostic route that pays for hashing keys instead of building
// them: you cannot list keys, so there is a way to ask about one. It is not for
// the decision path and does allocate.
func (l *Limiter) Inspect(s Subject) []Inspection {
	s.normalise()
	now := l.clk.now()
	out := make([]Inspection, 0, len(l.rules))
	for i := range l.rules {
		r := &l.rules[i]
		in := Inspection{
			Rule:     r.name,
			Selector: r.sel.raw,
			Matches:  r.sel.matches(s.Method, s.Host, s.Path, s.pathEndsSlash),
			KeyLabel: r.key.label,
			Shadow:   r.shadow,
			Exempt:   r.exempt,
		}
		if !r.exempt {
			q := l.quotaFor(r, &s)
			in.Quota = Quota{n: q.limit, window: time.Duration(q.window), burstP1: q.burst + 1}
			in.Key = r.key.hash(l.hkey, r.tag, &s)
			o := l.store.consume(in.Key, r.idx, now, 1, q.emission, q.tau, true)
			in.Remaining, in.ResetAfter, _ = quotaState(&q, o.emission, now, o.effTat, 0)
		}
		out = append(out, in)
	}
	return out
}

// Rules lists the compiled rule names in evaluation order.
func (l *Limiter) Rules() []string {
	out := make([]string, len(l.rules))
	for i := range l.rules {
		out[i] = l.rules[i].name
	}
	return out
}

// noteChain watches whether the forwarding header is usable, and says something
// once if it never is.
//
// A single request without the header is normal - internal traffic that bypasses
// the proxy, a health probe on the pod address. Many of them with never a single
// success is a broken configuration, and it is a quiet one: every such request
// falls back to the connection address and they all share one counter.
func (l *Limiter) noteChain(r *http.Request, ok bool) {
	if ok {
		l.chainHits.Add(1)
		return
	}
	if l.chainMisses.Add(1) < 128 || l.chainHits.Load() > 0 {
		return
	}
	if !l.proxyWarned.CompareAndSwap(false, true) {
		return
	}
	l.log.Warn("ratelimit: no request so far has carried a usable forwarding header behind a trusted proxy, "+
		"so every one of them is sharing a single counter under the connection address. "+
		"Check that your proxy appends to the header, and that the trusted ranges match the hop directly in front of this server.",
		"requests", l.chainMisses.Load(), "remote_addr", r.RemoteAddr)
}

// maybeWarnImplicitPeer says something when the default key is in use and the
// peer keeps looking like a proxy.
//
// The default key is the connection address, which is the only choice that is
// both configuration-free and unspoofable. Behind a proxy it groups all traffic
// into one counter, which would otherwise be a silent misconfiguration: the
// limiter appears to work and is limiting the wrong thing.
func (l *Limiter) maybeWarnImplicitPeer(a netip.Addr) {
	if l.proxyWarned.Load() {
		return
	}
	if !a.IsValid() || !(a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()) {
		l.privatePeers.Store(0)
		return
	}
	if l.privatePeers.Add(1) < 128 {
		return
	}
	if !l.proxyWarned.CompareAndSwap(false, true) {
		return
	}
	l.log.Warn("ratelimit: the default key is the connection address, and this server keeps seeing private peer addresses, "+
		"which usually means a proxy or load balancer sits in front of it. If so, every caller currently shares one counter. "+
		"Declare your proxy ranges and key by client address, e.g. Key: ratelimit.ByIP(ratelimit.PrivateRanges()...), "+
		"or set Key: ratelimit.ByPeer() explicitly to silence this.",
		"peer", a.String())
}

// deriveHashKey produces the key used to fingerprint rate limit keys.
//
// Without a backend it is random per process, which is the strongest option and
// needs no configuration: nothing outside this process ever needs to agree with
// it, and an attacker cannot search for a collision against a value they cannot
// compute.
//
// With a backend that is not available: every node has to derive the same
// fingerprint for the same key or the backend cannot correlate them, so the key
// comes from a shared secret. Refusing to start without one is the only honest
// option. Defaulting to something derivable - the rule name, the hostname, a
// constant - would hand an attacker the ability to compute a collision with a
// victim's key offline and share its cell.
func deriveHashKey(clusterKey string, hasBackend bool) (hashKey, error) {
	if clusterKey == "" {
		if hasBackend {
			return hashKey{}, fmt.Errorf(
				"invalid config: Backend is set but ClusterKey is empty. Every node has to derive the same key fingerprint " +
					"or the backend cannot match one node's cells to another's, so the fingerprint is keyed by a shared secret. " +
					"Set Config.ClusterKey to the same random string on every node, from your existing secret store. " +
					"It must be a secret: anyone who knows it can search for a key that collides with a victim's")
		}
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return hashKey{}, fmt.Errorf("ratelimit: cannot read random bytes for the key fingerprint: %w", err)
		}
		return hashKey{k0: le64(b[:], 0), k1: le64(b[:], 8)}, nil
	}
	if len(clusterKey) < 16 {
		return hashKey{}, fmt.Errorf(
			"invalid config: ClusterKey is %d bytes; use at least 16. It is a secret that protects against an attacker "+
				"searching for a key fingerprint that collides with a victim's", len(clusterKey))
	}
	sum := sha256.Sum256([]byte("github.com/imlargo/ratelimit/v1 cluster key\x00" + clusterKey))
	return hashKey{k0: le64(sum[:], 0), k1: le64(sum[:], 8)}, nil
}
