package prometheus_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/imlargo/ratelimit"
	rlprom "github.com/imlargo/ratelimit/metrics/prometheus"
)

func TestExporterWiresUpAndBoundsCardinality(t *testing.T) {
	reg := prometheus.NewRegistry()
	e, err := rlprom.New(reg, rlprom.Options{Namespace: "svc", DecisionLatency: true})
	if err != nil {
		t.Fatal(err)
	}

	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity: ratelimit.IdentityFromSubject,
		Metrics:  e.Metrics(),
		Rules: []ratelimit.Rule{
			{Name: "tight", Quota: ratelimit.PerHour(2), Key: ratelimit.ByIdentity()},
			{Name: "candidate", Quota: ratelimit.PerHour(1), Key: ratelimit.ByIdentity(), Shadow: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	// Many distinct keys, so we can check none of them became a label.
	for i := 0; i < 200; i++ {
		for j := 0; j < 4; j++ {
			lim.Check(ctx, ratelimit.Subject{Identity: string(rune('a'+i%26)) + "-" + string(rune('0'+j))})
		}
	}
	e.Observe(lim.Stats())

	if n := testutil.CollectAndCount(reg, "svc_ratelimit_decisions_total"); n == 0 {
		t.Error("no decision metrics were recorded")
	}
	if got := testutil.ToFloat64(mustCounter(t, reg, "svc_ratelimit_denied_total", "rule", "tight")); got == 0 {
		t.Error("denied_total did not move")
	}
	if got := testutil.ToFloat64(mustCounter(t, reg, "svc_ratelimit_shadow_denied_total", "rule", "candidate")); got == 0 {
		t.Error("shadow_denied_total did not move; comparing it against denied_total is the point of shadow mode")
	}

	// The only labels anywhere are reason, rule and outcome. A key must never
	// appear, and there is no API that would let it.
	out, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"reason": true, "rule": true, "outcome": true}
	rules := map[string]bool{"tight": true, "candidate": true, "": true}
	for _, mf := range out {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s carries an unexpected label %q", mf.GetName(), l.GetName())
				}
				if l.GetName() == "rule" && !rules[l.GetValue()] {
					t.Errorf("%s has rule=%q, which is not one of the configured rules; "+
						"the label set must be bounded by the rule table", mf.GetName(), l.GetValue())
				}
			}
		}
	}

	// Names that are local to the process say so.
	names := map[string]bool{}
	for _, mf := range out {
		names[mf.GetName()] = true
	}
	for n := range names {
		if strings.Contains(n, "store_cells") || strings.Contains(n, "decision_seconds") {
			if !strings.HasSuffix(n, "_local") {
				t.Errorf("%s measures something local to this process but its name does not say so", n)
			}
		}
	}
}

func mustCounter(t *testing.T, reg *prometheus.Registry, name string, lvs ...string) prometheus.Counter {
	t.Helper()
	out, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range out {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for i := 0; i+1 < len(lvs); i += 2 {
				found := false
				for _, l := range m.GetLabel() {
					if l.GetName() == lvs[i] && l.GetValue() == lvs[i+1] {
						found = true
					}
				}
				if !found {
					match = false
				}
			}
			if match {
				c := prometheus.NewCounter(prometheus.CounterOpts{Name: "tmp"})
				c.Add(m.GetCounter().GetValue())
				return c
			}
		}
	}
	t.Fatalf("no metric %s with %v", name, lvs)
	return nil
}

func TestNilRegistererIsRefused(t *testing.T) {
	if _, err := rlprom.New(nil, rlprom.Options{}); err == nil {
		t.Error("a nil registerer was accepted")
	}
}
