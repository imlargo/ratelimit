package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestClientIPSpoofingVectors is the security gate for the IP dimension.
//
// A limiter that can be sidestepped by setting a header is worse than no
// limiter, because it manufactures confidence. Most Go middleware reads the
// leftmost X-Forwarded-For value, which is entirely under the caller's control.
// This walks from the right and stops at the first address that is not one of
// the declared proxies: the closest hop we can actually vouch for.
func TestClientIPSpoofingVectors(t *testing.T) {
	trusted := []string{"10.0.0.0/8", "192.168.0.0/16"}
	pfx, err := parsePrefixes(trusted)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		remoteAddr string
		xff        []string
		want       string // "" means "the derivation failed and fell back to the peer"
		wantOK     bool
	}{
		{
			name:       "no headers, direct connection",
			remoteAddr: "203.0.113.9:1234", xff: nil,
			want: "203.0.113.9", wantOK: false,
		},
		{
			name:       "one trusted proxy appends the real client",
			remoteAddr: "10.0.0.1:1234", xff: []string{"203.0.113.9"},
			want: "203.0.113.9", wantOK: true,
		},
		{
			name:       "client injects an address; the proxy appends the real one",
			remoteAddr: "10.0.0.1:1234", xff: []string{"1.2.3.4, 203.0.113.9"},
			want: "203.0.113.9", wantOK: true,
		},
		{
			name:       "client injects a long fake chain",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"9.9.9.9, 8.8.8.8, 7.7.7.7, 203.0.113.9"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "client injects addresses inside our trusted range",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"10.9.9.9, 10.8.8.8, 203.0.113.9"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "two trusted hops",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"203.0.113.9, 192.168.5.5"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "the header is split across several fields",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"1.2.3.4", "203.0.113.9, 192.168.5.5"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "untrusted peer: headers are ignored outright",
			remoteAddr: "203.0.113.50:9999",
			xff:        []string{"1.2.3.4"},
			want:       "203.0.113.50", wantOK: false,
		},
		{
			name:       "peer is trusted but the chain is empty",
			remoteAddr: "10.0.0.1:1234", xff: []string{},
			want: "10.0.0.1", wantOK: false,
		},
		{
			name:       "the whole chain is ours: internal traffic",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"10.5.5.5, 10.6.6.6"},
			want:       "10.5.5.5", wantOK: true,
		},
		{
			name:       "garbage in the rightmost position falls back to the peer",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"203.0.113.9, not-an-address"},
			want:       "10.0.0.1", wantOK: false,
		},
		{
			name:       "an address with a port is still an address",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"203.0.113.9:44321"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "an IPv6 client behind a v4 proxy",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"2001:db8::9"},
			want:       "2001:db8::9", wantOK: true,
		},
		{
			name:       "a bracketed IPv6 with a port",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"[2001:db8::9]:5555"},
			want:       "2001:db8::9", wantOK: true,
		},
		{
			name:       "an IPv6 zone cannot be used as a second identity",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"2001:db8::9%eth0"},
			want:       "2001:db8::9", wantOK: true,
		},
		{
			name:       "an IPv4-mapped IPv6 address is the same identity as the IPv4",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"::ffff:203.0.113.9"},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "empty entries and stray whitespace",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"  ,  , 203.0.113.9 ,  "},
			want:       "203.0.113.9", wantOK: true,
		},
		{
			name:       "leading zeros are not a second spelling of an address",
			remoteAddr: "10.0.0.1:1234",
			xff:        []string{"010.000.000.001"},
			want:       "10.0.0.1", wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			got, ok := clientIP(r, pfx)
			if got.String() != tc.want || ok != tc.wantOK {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSpoofingCannotWinQuota is the same property stated as behaviour instead of
// as a parse result: an attacker rotating the header must not get more than its
// share.
func TestSpoofingCannotWinQuota(t *testing.T) {
	clk := NewTestingClock()
	lim, err := NewWith(Config{
		Rules:    []Rule{{Quota: PerHour(5), Key: ByIP("10.0.0.0/8")}},
		Capacity: 4096,
	}.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	allowed := 0
	for i := 0; i < 500; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "10.0.0.1:1234" // the trusted proxy
		// A new forged identity on every request, with the proxy appending the
		// attacker's real address on the right.
		r.Header.Set("X-Forwarded-For", "198.51.100."+itoa(i%254)+", 203.0.113.77")
		if lim.CheckRequest(r).Allowed {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("an attacker rotating X-Forwarded-For got %d requests through a quota of 5", allowed)
	}
}

// TestIPDimensionRefusesToBuildWithoutTrustedProxies. Not a warning, not a
// sensible default: a build failure, because the failure mode is silent and
// total.
func TestIPDimensionRefusesToBuildWithoutTrustedProxies(t *testing.T) {
	for _, k := range []Key{ByIP(), By(ClientIP()), By(IdentityOrIP())} {
		if _, err := New(Rule{Quota: PerMinute(10), Key: k}); err == nil {
			t.Error("an IP-keyed rule with no trusted proxies was accepted")
		}
	}
	// And it does build once they are declared.
	if _, err := NewWith(Config{
		Identity: FromSubject(),
		Rules:    []Rule{{Quota: PerMinute(10), Key: ByIdentityOrIP(PrivateRanges()...)}},
	}); err != nil {
		t.Errorf("declaring trusted ranges should be enough: %v", err)
	}
}

// TestDefaultKeyIsTheConnectionAddress: the zero Key needs no configuration and
// cannot be spoofed, which is what lets the one-line case be safe.
func TestDefaultKeyIsTheConnectionAddress(t *testing.T) {
	lim, err := NewWith(Config{Rules: []Rule{{Quota: PerHour(2)}}, Capacity: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	call := func(remote, xff string) bool {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return lim.CheckRequest(r).Allowed
	}

	// Two from one peer, then denied.
	if !call("203.0.113.1:1", "") || !call("203.0.113.1:2", "") {
		t.Fatal("the first two requests from a peer should be allowed")
	}
	if call("203.0.113.1:3", "") {
		t.Error("the third request from the same peer should be denied")
	}
	// Setting the header changes nothing, because the default key never reads it.
	if call("203.0.113.1:4", "1.2.3.4") {
		t.Error("X-Forwarded-For changed the identity under the default key")
	}
	// A different peer has its own counter.
	if !call("203.0.113.2:1", "") {
		t.Error("a different peer should have its own quota")
	}
}

func TestParseHostPortAgreesWithNetip(t *testing.T) {
	inputs := []string{
		"1.2.3.4", "255.255.255.255", "0.0.0.0", "::1", "2001:db8::1",
		"::ffff:1.2.3.4", "fe80::1", "1.2.3", "1.2.3.4.5", "256.1.1.1",
		"01.2.3.4", "", " ", "abc", "1.2.3.4:80", "[::1]:80", "-1.2.3.4",
	}
	for _, in := range inputs {
		got := parseHostPort(in)
		want, err := netip.ParseAddr(in)
		if err != nil {
			// Forms with a port are ours to handle; netip.ParseAddr rejects
			// them, so only compare the bare ones.
			if in == "1.2.3.4:80" || in == "[::1]:80" {
				if !got.IsValid() {
					t.Errorf("parseHostPort(%q) rejected a valid host:port", in)
				}
				continue
			}
			if got.IsValid() {
				t.Errorf("parseHostPort(%q) = %v, but netip rejects it: %v", in, got, err)
			}
			continue
		}
		if got != want.Unmap() {
			t.Errorf("parseHostPort(%q) = %v, netip says %v", in, got, want.Unmap())
		}
	}
}
