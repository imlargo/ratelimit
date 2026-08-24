package ratelimit

import (
	"net/http"
	"net/netip"
	"strings"
)

// PrivateRanges are the CIDR blocks normally occupied by load balancers,
// service meshes and reverse proxies inside a private network. Spread them into
// [ByIP] when your proxy hop is inside your own VPC:
//
//	Key: ratelimit.ByIP(ratelimit.PrivateRanges...)
//
// They are a convenience, not a default. Trusting a range you do not actually
// control lets anyone inside it choose their own rate limit identity.
var PrivateRanges = []string{
	"127.0.0.0/8",    // IPv4 loopback
	"10.0.0.0/8",     // RFC 1918
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"169.254.0.0/16", // link local
	"100.64.0.0/10",  // RFC 6598 carrier grade NAT
	"::1/128",        // IPv6 loopback
	"fc00::/7",       // IPv6 unique local
	"fe80::/10",      // IPv6 link local
}

func parsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func trustedContains(trusted []netip.Prefix, a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// peerAddr is the address of whoever opened the TCP connection. It cannot be
// spoofed by a header, which is why it is the default key.
func peerAddr(r *http.Request) netip.Addr {
	return parseHostPort(r.RemoteAddr)
}

// parseHostPort accepts "1.2.3.4", "1.2.3.4:80", "::1", "[::1]" and "[::1]:80"
// and tolerates surrounding space.
//
// It dispatches on the shape of the string rather than trying
// netip.ParseAddrPort first, because that function allocates when it fails and a
// bare address with no port is the common case. On the decision path a failed
// parse must cost no more than a successful one: a forwarding header is
// client-controlled data, so deliberately malformed entries must not be a way to
// make the server work harder.
func parseHostPort(s string) netip.Addr {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}
	}

	// A bracketed IPv6 literal, with or without a port.
	if s[0] == '[' {
		j := strings.IndexByte(s, ']')
		if j < 2 {
			return netip.Addr{}
		}
		return parseAddrNoZone(s[1:j])
	}

	i := strings.IndexByte(s, ':')
	switch {
	case i < 0:
		// IPv4, or a hostname we will reject.
		return parseAddrNoZone(s)
	case strings.IndexByte(s[i+1:], ':') < 0:
		// Exactly one colon: IPv4 with a port.
		return parseAddrNoZone(s[:i])
	default:
		// Several colons: a bare IPv6. Some proxies emit these unbracketed,
		// which is malformed but common, so we accept it.
		return parseAddrNoZone(s)
	}
}

// parseAddrNoZone parses an address and drops any IPv6 zone, so that
// fe80::1%eth0 and fe80::1%eth1 cannot be used as two identities for one host.
func parseAddrNoZone(s string) netip.Addr {
	if len(s) == 0 || len(s) > 45 { // longest textual IPv6 with an embedded IPv4
		return netip.Addr{}
	}
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}

	// The overwhelmingly common case, parsed here rather than by netip because
	// netip allocates when it builds a parse error. parseIPv4 validates as it
	// goes, so there is no separate character scan.
	if strings.IndexByte(s, ':') < 0 {
		a, _ := parseIPv4(s)
		return a
	}

	// IPv6. Reject anything that cannot possibly be an address on a character
	// class first, so netip is only consulted for plausible input.
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c == '.', c == ':',
			c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return netip.Addr{}
		}
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// parseIPv4 parses a dotted quad without allocating, on success or on failure.
func parseIPv4(s string) (netip.Addr, bool) {
	var out [4]byte
	field, val, digits := 0, 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			if digits == 0 || field == 3 {
				return netip.Addr{}, false
			}
			out[field] = byte(val)
			field++
			val, digits = 0, 0
			continue
		}
		if c < '0' || c > '9' {
			return netip.Addr{}, false
		}
		if digits == 3 {
			return netip.Addr{}, false
		}
		// Leading zeros are rejected: "010.1.1.1" is read as octal by some
		// stacks and as decimal by others, and an address with two readings is
		// two identities.
		if digits == 1 && val == 0 {
			return netip.Addr{}, false
		}
		val = val*10 + int(c-'0')
		digits++
		if val > 255 {
			return netip.Addr{}, false
		}
	}
	if field != 3 || digits == 0 {
		return netip.Addr{}, false
	}
	out[3] = byte(val)
	return netip.AddrFrom4(out), true
}

// chain accumulates a forwarding chain as it is walked left to right.
//
// It is a struct with methods rather than a callback because a closure over
// these four variables forces them onto the heap, and this runs on the decision
// path for every request.
type chain struct {
	found    netip.Addr // rightmost address that is not one of ours
	leftmost netip.Addr
	sawAny   bool
	blocked  bool // the rightmost non-trusted entry was not an address
}

func (c *chain) add(a netip.Addr, ok bool, trusted []netip.Prefix) {
	if !ok {
		// An entry that is not an address is neither one of our proxies nor a
		// usable client. Walking from the right we cannot look past it - we
		// have no way to tell whether it stood for a trusted hop - so it
		// invalidates anything found to its left. A trusted proxy always
		// appends a real address, so garbage in the rightmost position means
		// the header was not written by our proxy at all.
		c.found = netip.Addr{}
		c.blocked = true
		return
	}
	c.sawAny = true
	if !c.leftmost.IsValid() {
		c.leftmost = a
	}
	if !trustedContains(trusted, a) {
		c.found = a // keep overwriting: the last untrusted one is the rightmost
		c.blocked = false
	}
}

// clientIP implements the rightmost-non-trusted algorithm.
//
// A proxy appends the address of whoever connected to it, to the *right* of
// whatever was already there. Everything a client can inject therefore sits to
// the left of the first address a trusted proxy wrote. Walking from the right
// and stopping at the first address that is not one of our own proxies yields
// the closest hop we can actually vouch for.
//
// Reading the leftmost value instead - which is what most Go middleware does -
// lets any client pick its own identity and get unlimited quota.
//
// If the peer itself is not a trusted proxy, headers are ignored outright.
func clientIP(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer := peerAddr(r)
	if !trustedContains(trusted, peer) {
		return peer, false
	}

	var c chain
	for _, v := range r.Header.Values("X-Forwarded-For") {
		c.walkXFF(v, trusted)
	}

	switch {
	case c.found.IsValid():
		return c.found, true
	case c.blocked:
		return peer, false
	case c.sawAny:
		// Every hop in the chain is one of ours: legitimate internal traffic.
		return c.leftmost, true
	default:
		// No usable chain behind a trusted proxy. Fall back to the peer and say
		// so, so the caller can warn instead of silently grouping all traffic.
		return peer, false
	}
}

// walkXFF walks one comma separated X-Forwarded-For value, left to right.
func (c *chain) walkXFF(v string, trusted []netip.Prefix) {
	for len(v) > 0 {
		var item string
		if i := strings.IndexByte(v, ','); i >= 0 {
			item, v = v[:i], v[i+1:]
		} else {
			item, v = v, ""
		}
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		a := parseHostPort(item)
		c.add(a, a.IsValid(), trusted)
	}
}
