package ratelimit

import (
	"context"
	"math"
	"net/netip"
	"testing"
)

// TestKeyDomainSeparation is the collision that a naive key hash makes trivial.
//
// If dimensions were concatenated and hashed, Identity("a") plus Path("b")
// would be the same bytes as Identity("ab"), and two unrelated callers would
// share a counter. Order matters too: swapping which dimension a value came
// from must change the key.
func TestKeyDomainSeparation(t *testing.T) {
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Tenant:   IdentityFromSubject,
		Rules: []Rule{
			{Name: "id_path", Quota: PerHour(1000), Key: By(Identity(), Path())},
			{Name: "path_id", Quota: PerHour(1000), Key: By(Path(), Identity())},
			{Name: "id_only", Quota: PerHour(1000), Key: By(Identity())},
			{Name: "tenant", Quota: PerHour(1000), Key: By(Tenant())},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	keyOf := func(rule string, s Subject) uint64 {
		s.normalise()
		for i := range lim.rules {
			r := &lim.rules[i]
			if r.name == rule {
				return r.key.hash(lim.hkey, r.tag, &s)
			}
		}
		t.Fatalf("no rule %q", rule)
		return 0
	}

	// Concatenation collision: ("a","/b") vs ("a/b","") vs ("","a/b").
	split := keyOf("id_path", Subject{Identity: "a", Path: "/b"})
	joined := keyOf("id_only", Subject{Identity: "a/b"})
	if split == joined {
		t.Error("Identity(\"a\")+Path(\"/b\") collides with Identity(\"a/b\"): dimensions are not domain separated")
	}

	// Order sensitivity.
	if keyOf("id_path", Subject{Identity: "x", Path: "/y"}) == keyOf("path_id", Subject{Identity: "x", Path: "/y"}) {
		t.Error("swapping dimension order gives the same key")
	}

	// The same string in a different dimension must be a different key.
	if keyOf("id_only", Subject{Identity: "acme"}) == keyOf("tenant", Subject{Tenant: "acme"}) {
		t.Error("Identity(\"acme\") collides with Tenant(\"acme\")")
	}

	// Rules never share cells, even with identical keys.
	if keyOf("id_only", Subject{Identity: "z"}) == keyOf("tenant", Subject{Identity: "z"}) {
		t.Error("two rules share a cell for the same subject")
	}

	// An identity that is present must differ from one that is absent and falls
	// back to an address.
	lim2, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules:    []Rule{{Name: "either", Quota: PerHour(10), Key: ByIdentityOrIP("10.0.0.0/8")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim2.Close()
	r := &lim2.rules[0]
	withID := Subject{Identity: "10.1.2.3"}
	withID.normalise()
	withIP := Subject{IP: netip.MustParseAddr("10.1.2.3")}
	withIP.normalise()
	if r.key.hash(lim2.hkey, r.tag, &withID) == r.key.hash(lim2.hkey, r.tag, &withIP) {
		t.Error("an identity of \"10.1.2.3\" collides with the address 10.1.2.3")
	}
}

// TestKeyDistribution checks the fingerprint spreads, because a hash that
// clumps turns the two-way bucket table into a saturating one.
func TestKeyDistribution(t *testing.T) {
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules:    []Rule{{Quota: PerHour(10), Key: ByIdentity()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()
	r := &lim.rules[0]

	const n = 1 << 16
	const buckets = 256
	var hist [buckets]int
	seen := make(map[uint64]struct{}, n)

	for i := 0; i < n; i++ {
		s := Subject{Identity: "user-" + itoa(i), Path: "/"}
		h := r.key.hash(lim.hkey, r.tag, &s)
		if _, dup := seen[h]; dup {
			t.Fatalf("fingerprint collision after %d keys; expected around %.2g", i, float64(n)*float64(n)/math.Pow(2, 65))
		}
		seen[h] = struct{}{}
		hist[h&(buckets-1)]++
		hist[(h>>32)&(buckets-1)]++
	}

	// Chi-squared against uniform, generously.
	expect := float64(2*n) / buckets
	var chi float64
	for _, c := range hist {
		d := float64(c) - expect
		chi += d * d / expect
	}
	// 255 degrees of freedom: the 0.999 critical value is about 350.
	if chi > 400 {
		t.Errorf("chi-squared %.1f over %d buckets suggests the fingerprint is not uniform", chi, buckets)
	}
	t.Logf("%d distinct keys, no collisions, chi-squared %.1f over %d buckets (expect ~255)", n, chi, buckets)
}

// TestKeySeedIsPerProcess: two limiters must not agree on fingerprints, or an
// attacker could compute a collision against a victim offline.
func TestKeySeedIsPerProcess(t *testing.T) {
	mk := func() (*Limiter, *rule) {
		l, err := NewWith(Config{
			Identity: IdentityFromSubject,
			Rules:    []Rule{{Quota: PerHour(10), Key: ByIdentity()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return l, &l.rules[0]
	}
	a, ra := mk()
	defer a.Close()
	b, rb := mk()
	defer b.Close()

	s := Subject{Identity: "victim", Path: "/"}
	if ra.key.hash(a.hkey, ra.tag, &s) == rb.key.hash(b.hkey, rb.tag, &s) {
		t.Error("two limiters produced the same fingerprint; the hash key is not per instance")
	}
}

// TestInspectMaterialisesKeys is the diagnostic route that pays for never
// building a key string on the decision path.
func TestInspectMaterialisesKeys(t *testing.T) {
	lim, err := NewWith(Config{
		Identity: IdentityFromSubject,
		Rules: []Rule{
			{Name: "health", Selector: "GET /healthz", Exempt: true},
			{Name: "search", Selector: "POST /api/search", Quota: PerMinute(10), Key: By(Identity(), Path())},
			{Name: "global", Quota: PerMinute(1000), Key: ByIdentity()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := Subject{Identity: "u1", Path: "/api/search", Method: "POST"}
	for i := 0; i < 4; i++ {
		lim.Check(ctx, s)
	}

	got := lim.Inspect(s)
	if len(got) != 3 {
		t.Fatalf("Inspect returned %d rows, want 3", len(got))
	}
	byName := map[string]Inspection{}
	for _, in := range got {
		byName[in.Rule] = in
	}
	if in := byName["search"]; !in.Matches || in.KeyLabel != "identity+path" || in.Remaining != 6 {
		t.Errorf("search: %+v; want matching, key identity+path, 6 remaining", in)
	}
	if in := byName["global"]; !in.Matches || in.Remaining != 996 {
		t.Errorf("global: %+v; want matching with 996 remaining", in)
	}
	if in := byName["health"]; in.Matches {
		t.Error("the health exemption should not match POST /api/search")
	}
	// Inspect must not have consumed anything.
	if in := lim.Inspect(s); in[1].Remaining != byName["search"].Remaining {
		t.Error("Inspect consumed quota")
	}
}
