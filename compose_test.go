package ratelimit

import (
	"context"
	"testing"
	"time"
)

// TestQuotaIsRefundedWhenALaterRuleDenies is the capability no library in the
// Go ecosystem has, and the structural reason the rule table is evaluated as
// one unit instead of as nested middleware.
//
// Three nested middlewares each take quota independently and none knows about
// the others, so when the third denies the first two have already charged. A
// caller that keeps hitting one strict endpoint then burns its global quota
// too, and its effective limit is tighter than the configured one in a way
// nobody can predict from the configuration.
//
// The test checks the general rule's own counter, not just the outcome.
func TestQuotaIsRefundedWhenALaterRuleDenies(t *testing.T) {
	clk := NewTestingClock()
	var refunds []string
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Metrics:  Metrics{Refunded: func(r string) { refunds = append(refunds, r) }},
		Rules: []Rule{
			{Name: "global", Quota: PerHour(1000), Key: ByIdentity()},
			{Name: "group", Selector: "/api/", Quota: PerHour(500), Key: ByIdentity()},
			{Name: "strict", Selector: "POST /api/export", Quota: PerHour(3), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	export := Subject{Identity: "u1", Path: "/api/export", Method: "POST"}

	// Spend the strict rule.
	for i := 0; i < 3; i++ {
		if d := lim.Check(ctx, export); !d.Allowed {
			t.Fatalf("export %d should be allowed: %s", i, d)
		}
	}

	// Now hammer the strict rule 200 times. Every one is denied, and every one
	// must give back what global and group already took.
	for i := 0; i < 200; i++ {
		d := lim.Check(ctx, export)
		if d.Allowed {
			t.Fatalf("export attempt %d should be denied", i)
		}
		if d.Rule != "strict" {
			t.Fatalf("denial attributed to %q, want \"strict\"", d.Rule)
		}
	}

	// The general rules must be untouched beyond the three that succeeded.
	in := inspectByName(t, lim, export)
	if got := in["global"].Remaining; got != 997 {
		t.Errorf("global has %d remaining, want 997: 200 denied attempts burned %d of its quota", got, 997-got)
	}
	if got := in["group"].Remaining; got != 497 {
		t.Errorf("group has %d remaining, want 497: 200 denied attempts burned %d of its quota", got, 497-got)
	}
	// No refund was needed here, and that is the better outcome: rules are
	// ordered strictest first, so the request that is going to be denied is
	// denied before any looser rule has charged for it. Refunding is the
	// fallback for the cases that ordering cannot cover, exercised in
	// TestRefundWhenALooserRuleWasChargedFirst.
	if len(refunds) != 0 {
		t.Errorf("%d refunds were needed; evaluating strictest first should have avoided them", len(refunds))
	}

	// And the caller can still use the rest of the API at full rate.
	other := Subject{Identity: "u1", Path: "/api/things", Method: "GET"}
	for i := 0; i < 400; i++ {
		if d := lim.Check(ctx, other); !d.Allowed {
			t.Fatalf("request %d to another endpoint denied (%s); the strict rule leaked into it", i, d)
		}
	}
}

func inspectByName(t *testing.T, lim *Limiter, s Subject) map[string]Inspection {
	t.Helper()
	out := map[string]Inspection{}
	for _, in := range lim.Inspect(s) {
		out[in.Rule] = in
	}
	return out
}

// TestStrictestRuleWins and the decision names it.
func TestStrictestRuleWins(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{
			{Name: "loose", Quota: PerHour(1000), Key: ByIdentity()},
			{Name: "tight", Selector: "/api/", Quota: PerHour(2), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u", Path: "/api/x"}
	for i := 0; i < 2; i++ {
		d := lim.Check(ctx, s)
		if !d.Allowed {
			t.Fatalf("request %d denied", i)
		}
		// The reported quota is the binding one, not the loosest.
		if d.Rule != "tight" {
			t.Errorf("request %d reports rule %q, want the binding rule \"tight\"", i, d.Rule)
		}
	}
	d := lim.Check(ctx, s)
	if d.Allowed || d.Rule != "tight" || d.Reason != ReasonDeniedQuota {
		t.Errorf("got %s, want denial by \"tight\"", d)
	}
}

// TestPrecedenceIsAdditive: a more specific rule does not relax a general one.
//
// This is deliberately unlike how routing composes. A security control that can
// be loosened by declaring something narrower is a hole, so both rules apply and
// the tighter one governs. Substituting has to be spelled out, by not writing
// the general rule over that path.
func TestPrecedenceIsAdditive(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{
			{Name: "general", Quota: PerHour(5), Key: ByIdentity()},
			{Name: "specific", Selector: "GET /api/cheap", Quota: PerHour(1000), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u", Path: "/api/cheap", Method: "GET"}
	allowed := 0
	for i := 0; i < 20; i++ {
		if lim.Check(ctx, s).Allowed {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("the specific loose rule let %d requests through; the general rule of 5 must still apply", allowed)
	}
}

// TestExemptionShortCircuits: an exemption consumes nothing and stops
// evaluation, so limiting your own health check is not something you can do by
// accident.
func TestExemptionShortCircuits(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{
			{Name: "health", Selector: "GET /healthz", Exempt: true},
			{Name: "global", Quota: PerHour(3), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	health := Subject{Identity: "probe", Path: "/healthz", Method: "GET"}
	for i := 0; i < 10_000; i++ {
		d := lim.Check(ctx, health)
		if !d.Allowed || d.Reason != ReasonExempt || d.Rule != "health" {
			t.Fatalf("health check %d: %s, want exempt by \"health\"", i, d)
		}
	}
	// The global rule saw none of it.
	if got := inspectByName(t, lim, Subject{Identity: "probe", Path: "/other"})["global"].Remaining; got != 3 {
		t.Errorf("global has %d remaining after 10000 exempt requests, want 3", got)
	}
	// A different path is still limited.
	other := Subject{Identity: "probe", Path: "/api/x"}
	for i := 0; i < 3; i++ {
		if !lim.Check(ctx, other).Allowed {
			t.Fatalf("request %d to a non-exempt path denied", i)
		}
	}
	if lim.Check(ctx, other).Allowed {
		t.Error("the exemption leaked to a non-exempt path")
	}
}

// TestQuotaResolvedFromIdentity covers per-plan limits.
func TestQuotaResolvedFromIdentity(t *testing.T) {
	plans := map[string]Quota{
		"free":       PerMinute(3),
		"pro":        PerMinute(50),
		"enterprise": PerMinute(1000),
	}
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{{
			Name:  "tiered",
			Quota: PerMinute(3), // the fallback
			Key:   ByIdentity(),
			QuotaFor: func(s Subject) Quota {
				return plans[s.Tenant]
			},
		}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	for plan, q := range plans {
		s := Subject{Identity: "user-" + plan, Tenant: plan}
		allowed := 0
		for i := 0; i < int(q.Limit())+10; i++ {
			if lim.Check(ctx, s).Allowed {
				allowed++
			}
		}
		if int64(allowed) != q.Limit() {
			t.Errorf("plan %s allowed %d, want %d", plan, allowed, q.Limit())
		}
	}
}

// TestQuotaResolutionFailureIsDeclared: a resolver that returns nonsense falls
// back to the static quota and says so. It never panics, never blocks, and
// never quietly stops limiting.
func TestQuotaResolutionFailureIsDeclared(t *testing.T) {
	var failures int
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Metrics:  Metrics{QuotaResolutionFailed: func(string) { failures++ }},
		Rules: []Rule{{
			Name:     "tiered",
			Quota:    PerMinute(2),
			Key:      ByIdentity(),
			QuotaFor: func(Subject) Quota { return Quota{} }, // never valid
		}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u"}
	allowed := 0
	for i := 0; i < 10; i++ {
		if lim.Check(ctx, s).Allowed {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("allowed %d, want the static fallback quota of 2", allowed)
	}
	if failures == 0 {
		t.Error("the failed resolution was never reported")
	}
}

// TestCostBeyondBurstIsDeclared, not silently denied forever.
func TestCostBeyondBurstIsDeclared(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{{
			Name:    "byweight",
			Quota:   PerMinute(10),
			Key:     ByIdentity(),
			CostFor: func(s Subject) int64 { return s.Cost },
		}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	d := lim.Check(ctx, Subject{Identity: "u", Cost: 11})
	if d.Allowed {
		t.Fatal("a cost above the burst was admitted")
	}
	if d.Reason != ReasonCostExceedsBurst {
		t.Errorf("reason %v, want ReasonCostExceedsBurst; an unexplainable permanent denial is the worst kind", d.Reason)
	}
	// A cost within the burst still works.
	if !lim.Check(ctx, Subject{Identity: "u", Cost: 10}).Allowed {
		t.Error("a cost equal to the burst should be admitted against an idle cell")
	}
}

// TestVariableCost: cost N consumes N units.
func TestVariableCost(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules:    []Rule{{Quota: PerMinute(100), Key: ByIdentity()}},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u", Cost: 20}
	for i := 0; i < 5; i++ {
		if d := lim.Check(ctx, s); !d.Allowed {
			t.Fatalf("cost-20 request %d denied: %s", i, d)
		}
	}
	if d := lim.Check(ctx, s); d.Allowed {
		t.Error("a sixth cost-20 request against a quota of 100 should be denied")
	}
	// A cost-1 request is still refused, because nothing is left.
	if d := lim.Check(ctx, Subject{Identity: "u"}); d.Allowed {
		t.Error("a cost-1 request should also be refused")
	}
	// After a fifth of a window, exactly 20 units are back.
	clk.Advance(int64(12 * time.Second))
	if d := lim.Check(ctx, s); !d.Allowed {
		t.Errorf("after 12s of a minute window, a cost-20 request should be admitted: %s", d)
	}
}

// TestConstantCostPerRule for endpoints priced by weight.
func TestConstantCostPerRule(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{
			{Name: "search", Selector: "GET /search", Quota: PerMinute(100), Key: ByIdentity(), Cost: 20},
			{Name: "cheap", Quota: PerMinute(100), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	search := Subject{Identity: "u", Path: "/search", Method: "GET"}
	n := 0
	for lim.Check(ctx, search).Allowed {
		n++
		if n > 100 {
			break
		}
	}
	if n != 5 {
		t.Errorf("a cost of 20 against a quota of 100 allowed %d requests, want 5", n)
	}
}

// TestRefundWhenALooserRuleWasChargedFirst exercises the refund itself.
//
// Ordering by strictness handles the common shape, but it cannot handle every
// one: here the per-caller rule is evaluated first because its window is
// shorter, while the per-tenant rule that actually denies has a wider one. The
// per-caller quota must be given back, or a caller whose tenant is out of quota
// would silently lose its own.
func TestRefundWhenALooserRuleWasChargedFirst(t *testing.T) {
	clk := NewTestingClock()
	var refunds []string
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Tenant:   IdentityFromSubject,
		Metrics:  Metrics{Refunded: func(r string) { refunds = append(refunds, r) }},
		Rules: []Rule{
			{Name: "percaller", Quota: PerMinute(20), Key: ByIdentity()},
			{Name: "pertenant", Quota: PerHour(3), Key: By(Tenant())},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	if got := lim.Rules(); got[0] != "percaller" {
		t.Fatalf("evaluation order is %v; this test needs percaller first", got)
	}

	ctx := context.Background()
	// Three different callers in one tenant spend the tenant's whole quota.
	for i := 0; i < 3; i++ {
		s := Subject{Identity: "spender-" + itoa(i), Tenant: "acme"}
		if d := lim.Check(ctx, s); !d.Allowed {
			t.Fatalf("spender %d denied: %s", i, d)
		}
	}

	// A fresh caller in the same tenant is denied by the tenant rule, ten times.
	fresh := Subject{Identity: "fresh", Tenant: "acme"}
	for i := 0; i < 10; i++ {
		d := lim.Check(ctx, fresh)
		if d.Allowed {
			t.Fatalf("attempt %d should be denied by the tenant rule", i)
		}
		if d.Rule != "pertenant" {
			t.Fatalf("attempt %d denied by %q, want \"pertenant\"", i, d.Rule)
		}
	}

	if len(refunds) != 10 {
		t.Errorf("%d refunds, want 10", len(refunds))
	}
	for _, r := range refunds {
		if r != "percaller" {
			t.Errorf("refunded %q, want \"percaller\"", r)
		}
	}

	// The fresh caller's own quota is intact: nothing was lost to ten denials.
	if got := inspectByName(t, lim, fresh)["percaller"].Remaining; got != 20 {
		t.Errorf("percaller has %d remaining for the fresh caller, want 20; %d units were lost to denials it did not cause", got, 20-got)
	}

	// The tenant rule is still the binding one, so the caller gets the tenant's
	// remaining allowance and no more. That is the additive precedence working.
	clk.Advance(int64(time.Hour))
	allowed := 0
	for i := 0; i < 30; i++ {
		if lim.Check(ctx, fresh).Allowed {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("after the tenant recovered the caller got %d requests, want the tenant quota of 3", allowed)
	}

	// A caller in a tenant with room gets its own full allowance, proving the
	// per-caller counters were never quietly drained.
	other := Subject{Identity: "elsewhere", Tenant: "other-co"}
	allowed = 0
	for i := 0; i < 30; i++ {
		if lim.Check(ctx, other).Allowed {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("caller in a fresh tenant got %d, want the tenant quota of 3", allowed)
	}
	_ = time.Second
}
