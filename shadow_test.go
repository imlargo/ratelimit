package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestShadowConsumesExactlyLikeALiveRule is the property that makes shadow mode
// worth having.
//
// Shadow mode exists so a limit can be sized before it starts denying. If it
// were an "if" that skipped the evaluation, it would measure something other
// than what the live rule would do, which is the only thing it is asked for. So
// it is the same code path with a different reason and its own counter.
//
// This compares the store state between a shadow rule and an identical live
// rule, cell by cell.
func TestShadowConsumesExactlyLikeALiveRule(t *testing.T) {
	build := func(shadow bool) (*Limiter, *TestingClock) {
		clk := NewTestingClock()
		lim, err := NewWith(Config{
			Identity: FromSubject(),
			Capacity: 1024,
			Rules:    []Rule{{Name: "r", Quota: PerMinute(5), Key: ByIdentity(), Shadow: shadow}},
		}.WithClock(clk))
		if err != nil {
			t.Fatal(err)
		}
		return lim, clk
	}

	live, _ := build(false)
	defer live.Close()
	shadow, _ := build(true)
	defer shadow.Close()

	ctx := context.Background()
	for i := 0; i < 40; i++ {
		s := Subject{Identity: "u" + itoa(i%4)}
		live.Check(ctx, s)
		shadow.Check(ctx, s)
	}

	// The cells must hold identical levels. The fingerprints differ because the
	// hash seed is per limiter, so compare the multiset of levels.
	levels := func(l *Limiter) map[int64]int {
		out := map[int64]int{}
		now := l.clk.now()
		for i := range l.store.slots {
			if l.store.slots[i].fp.Load() == 0 {
				continue
			}
			out[l.store.level(i, now)]++
		}
		return out
	}
	lv, sv := levels(live), levels(shadow)
	if len(lv) != len(sv) {
		t.Fatalf("live store holds %d distinct levels, shadow %d", len(lv), len(sv))
	}
	for k, n := range lv {
		if sv[k] != n {
			t.Errorf("level %d appears %d times live and %d times in shadow; shadow mode is not consuming identically", k, n, sv[k])
		}
	}
	t.Logf("both stores hold the same levels: %v", lv)
}

// TestShadowNeverDeniesButSaysSo.
func TestShadowNeverDeniesButSaysSo(t *testing.T) {
	var shadowDenials, realDenials int
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: FromSubject(),
		Metrics: Metrics{
			ShadowDenied: func(string) { shadowDenials++ },
			Denied:       func(string) { realDenials++ },
		},
		Rules: []Rule{
			{Name: "candidate", Quota: PerMinute(3), Key: ByIdentity(), Shadow: true},
			{Name: "live", Quota: PerMinute(1000), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u"}
	for i := 0; i < 20; i++ {
		d := lim.Check(ctx, s)
		if !d.Allowed {
			t.Fatalf("request %d was denied; a shadow rule must never deny", i)
		}
		if i >= 3 {
			if !d.Shadowed {
				t.Errorf("request %d: Shadowed is false, but the candidate rule was out of quota", i)
			}
			if d.ShadowRule != "candidate" {
				t.Errorf("request %d: ShadowRule = %q, want \"candidate\"", i, d.ShadowRule)
			}
			if d.Reason != ReasonAllowedShadow {
				t.Errorf("request %d: reason %v, want ReasonAllowedShadow", i, d.Reason)
			}
		} else if d.Shadowed {
			t.Errorf("request %d was flagged as shadowed while the candidate still had quota", i)
		}
	}

	if shadowDenials != 17 {
		t.Errorf("%d shadow denials counted, want 17", shadowDenials)
	}
	if realDenials != 0 {
		t.Errorf("%d real denials counted, want 0", realDenials)
	}
}

// TestShadowIsVisibleToTheApplication through the OnDecision hook, which is how
// you get "what would this rule have blocked" into your logs.
func TestShadowIsVisibleToTheApplication(t *testing.T) {
	type entry struct {
		path string
		rule string
	}
	var wouldBlock []entry

	lim, err := NewWith(Config{
		Rules: []Rule{
			{Name: "export", Selector: "POST /api/export", Quota: PerHour(2), Shadow: true},
			{Name: "global", Quota: PerHour(10000)},
		},
		OnDecision: func(r *http.Request, d Decision) {
			if d.Shadowed {
				wouldBlock = append(wouldBlock, entry{r.URL.Path, d.ShadowRule})
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	h := lim.Limit(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/export", nil)
		r.RemoteAddr = "203.0.113.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d got %d; shadow mode must not deny", i, w.Code)
		}
	}
	if len(wouldBlock) != 4 {
		t.Errorf("%d shadow denials reported to the application, want 4", len(wouldBlock))
	}
	for _, e := range wouldBlock {
		if e.rule != "export" || e.path != "/api/export" {
			t.Errorf("unexpected entry %+v", e)
		}
	}
}

// TestShadowDoesNotSuppressARealDenial: a live rule still denies while a shadow
// rule is also complaining.
func TestShadowDoesNotSuppressARealDenial(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Identity: FromSubject(),
		Rules: []Rule{
			{Name: "candidate", Quota: PerHour(1), Key: ByIdentity(), Shadow: true},
			{Name: "live", Quota: PerHour(2), Key: ByIdentity()},
		},
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u"}
	for i := 0; i < 2; i++ {
		if !lim.Check(ctx, s).Allowed {
			t.Fatalf("request %d should be allowed by the live rule", i)
		}
	}
	d := lim.Check(ctx, s)
	if d.Allowed {
		t.Fatal("the live rule is out of quota and must deny")
	}
	if d.Rule != "live" || d.Reason != ReasonDeniedQuota {
		t.Errorf("got %s, want denial by \"live\"", d)
	}
	if !d.Shadowed || d.ShadowRule != "candidate" {
		t.Errorf("the shadow rule's verdict was lost on a real denial: %s", d)
	}
}
