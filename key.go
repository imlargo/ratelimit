package ratelimit

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ErrInvalidKey is the class of all key validation failures.
var ErrInvalidKey = errors.New("invalid key")

// IdentityFunc pulls a caller identity out of a request: an account id, an API
// key id, a tenant, whatever the application authenticates by. It reports false
// when the request carries no identity.
//
// It is configured once, on [Config], rather than per rule: who the caller is
// is a property of your authentication, not of an individual rate limit. It is
// called at most once per request.
//
// It runs on the decision path. It must not allocate, must not block, and must
// not panic. Returning a string that already exists - a context value, a field
// on a session your auth middleware already resolved - costs nothing;
// formatting a new one costs an allocation per request.
//
// If the identity is a secret compared against something, compare it with
// crypto/subtle.ConstantTimeCompare in your own code. This package only ever
// hashes what you return.
type IdentityFunc func(*http.Request) (string, bool)

// IdentityFromSubject declares that you fill [Subject.Identity] yourself,
// because you are not driving the limiter from HTTP: a queue consumer, a gRPC
// handler, a background job.
//
//	Config{Identity: ratelimit.IdentityFromSubject, Rules: ...}
//
// A rule that keys by identity needs to know where the identity comes from, and
// guessing is not an option, so it is stated either way. If this is used
// together with the HTTP middleware then requests carry no identity at all:
// [IdentityOrIP] falls back to the client address, which is safe, and
// [Identity] groups every request into one counter, which is visible in the
// metrics but is almost certainly not what you meant.
var IdentityFromSubject IdentityFunc = func(*http.Request) (string, bool) { return "", false }

// Subject is what a decision is made about. The HTTP middleware fills one in
// from the request; callers outside HTTP build one directly.
type Subject struct {
	// Identity is the authenticated caller, if any.
	Identity string
	// Tenant is the organisation or account the caller belongs to, if any.
	Tenant string
	// Path and Method locate the request. Selectors match on these.
	Path   string
	Method string
	// Host is the request host, matched by selectors that specify one.
	Host string
	// IP is the caller address. Outside HTTP it is whatever the caller says;
	// inside HTTP it is derived safely from the connection and, if trusted
	// proxies were declared, from the forwarding headers.
	IP netip.Addr
	// Cost is how much this request consumes. Zero means one.
	Cost int64

	// Request is the originating request, or nil when [Limiter.Check] was
	// called directly. Rule hooks receive it so they can read anything the
	// declared dimensions do not cover.
	Request *http.Request

	// pathEndsSlash is remembered when Path is normalised, because "/a" and
	// "/a/" are different requests and the {$} anchor has to tell them apart.
	pathEndsSlash bool
}

// normalise makes a hand-built Subject safe to match selectors against. It is
// idempotent.
func (s *Subject) normalise() {
	if s.Path == "" {
		s.Path = "/"
	}
	s.Path, s.pathEndsSlash = normalisePath(s.Path)
}

type dimKind uint8

const (
	dimPeer dimKind = iota + 1
	dimClientIP
	dimIdentity
	dimIdentityOrIP
	dimTenant
	dimPath
	dimMethod
	dimHost
	dimContext
)

// Dimension is one component of a key: what makes two requests share a counter.
//
// Dimensions are declared, not computed into a string. The library hashes them
// directly, so composing a key allocates nothing.
type Dimension struct {
	kind    dimKind
	trusted []netip.Prefix
	ctxKey  any
	label   string
	err     error
}

// Peer keys by the address that opened the connection. It cannot be spoofed by
// a header and needs no configuration, which is why it is the default.
//
// Behind a reverse proxy every request appears to come from the proxy, so all
// traffic shares one counter. Use [ClientIP] there.
func Peer() Dimension { return Dimension{kind: dimPeer, label: "peer"} }

// ClientIP keys by the caller address taken from X-Forwarded-For, using the
// rightmost address that is not one of the trusted proxy ranges.
//
// X-Forwarded-For is the only header consulted. RFC 7239 Forwarded is not
// supported, deliberately: nearly nothing emits it, and reading two headers is
// how the spoofing hole they were both meant to close gets reopened.
//
// At least one trusted CIDR is required. There is no safe default: a limiter
// that reads a forwarding header without knowing which hops it can vouch for is
// bypassed by sending the header, and a bypassable limiter is worse than none
// because it manufactures confidence.
func ClientIP(trustedCIDRs ...string) Dimension {
	return clientIPDim(dimClientIP, "client_ip", trustedCIDRs)
}

func clientIPDim(kind dimKind, label string, cidrs []string) Dimension {
	if len(cidrs) == 0 {
		return Dimension{kind: kind, label: label, err: fmt.Errorf(
			"%w: %s requires at least one trusted proxy CIDR. Without one, any caller can set the forwarding header and choose its own rate limit identity. "+
				"Declare the ranges your proxies actually occupy, e.g. ratelimit.ClientIP(\"10.0.0.0/8\") or ratelimit.ClientIP(ratelimit.PrivateRanges...). "+
				"If nothing sits in front of this server, use ratelimit.Peer() instead", ErrInvalidKey, label)}
	}
	pfx, err := parsePrefixes(cidrs)
	if err != nil {
		return Dimension{kind: kind, label: label, err: fmt.Errorf("%w: %s: %v", ErrInvalidKey, label, err)}
	}
	return Dimension{kind: kind, trusted: pfx, label: label}
}

// Identity keys by the authenticated caller, resolved by [Config.Identity].
// Requests with no identity all share one counter, which is usually not what
// you want; see [IdentityOrIP].
func Identity() Dimension { return Dimension{kind: dimIdentity, label: "identity"} }

// IdentityOrIP keys by the authenticated caller when there is one and by the
// client address otherwise. This is the shape almost every API that serves both
// signed-in and anonymous traffic wants, and it is one line.
func IdentityOrIP(trustedCIDRs ...string) Dimension {
	return clientIPDim(dimIdentityOrIP, "identity_or_ip", trustedCIDRs)
}

// Tenant keys by organisation, account or workspace, resolved by
// [Config.Tenant].
func Tenant() Dimension { return Dimension{kind: dimTenant, label: "tenant"} }

// Path keys by request path, so each endpoint gets its own counter.
func Path() Dimension { return Dimension{kind: dimPath, label: "path"} }

// Method keys by request method.
func Method() Dimension { return Dimension{kind: dimMethod, label: "method"} }

// Host keys by request host, for one limiter serving several domains.
func Host() Dimension { return Dimension{kind: dimHost, label: "host"} }

// ContextValue keys by a string your own middleware put in the request context.
//
// This is the escape hatch for anything the declared dimensions do not cover -
// a plan id, a shard, a feature flag, a header you validated yourself. Your
// code runs in your middleware, not on this library's decision path, so a slow
// or panicking extractor cannot take the limiter down with it.
//
// The value must be a string. Any other type, or a missing value, hashes as
// empty.
func ContextValue(key any, label string) Dimension {
	if key == nil {
		return Dimension{kind: dimContext, label: label, err: fmt.Errorf("%w: ContextValue requires a context key", ErrInvalidKey)}
	}
	if label == "" {
		label = "context"
	}
	return Dimension{kind: dimContext, ctxKey: key, label: label}
}

// Key is what makes two requests share a counter, composed of one or more
// dimensions.
//
// The zero Key means [Peer]: the address that opened the connection. That is
// always safe and needs no configuration.
type Key struct {
	dims []Dimension
	err  error
}

// By builds a key from an ordered list of dimensions.
func By(dims ...Dimension) Key {
	if len(dims) == 0 {
		return Key{err: fmt.Errorf("%w: By requires at least one dimension", ErrInvalidKey)}
	}
	for _, d := range dims {
		if d.err != nil {
			return Key{err: d.err}
		}
	}
	return Key{dims: dims}
}

// ByPeer keys by the connection address. Same as the zero Key.
func ByPeer() Key { return By(Peer()) }

// ByIP keys by the client address behind the given trusted proxies.
func ByIP(trustedCIDRs ...string) Key { return By(ClientIP(trustedCIDRs...)) }

// ByIdentity keys by the authenticated caller.
func ByIdentity() Key { return By(Identity()) }

// ByIdentityOrIP keys by the authenticated caller, falling back to the client
// address for anonymous traffic.
func ByIdentityOrIP(trustedCIDRs ...string) Key {
	return By(IdentityOrIP(trustedCIDRs...))
}

// IsZero reports whether the key was left at its default.
func (k Key) IsZero() bool { return len(k.dims) == 0 && k.err == nil }

// compiledKey is the hot-path form.
type compiledKey struct {
	dims          []Dimension
	label         string // human readable, for diagnostics only
	implicit      bool   // true when the caller never chose, so we may warn about proxies
	needsIP       bool
	needsIdentity bool
	needsTenant   bool
}

func (k Key) compile() (compiledKey, error) {
	if k.err != nil {
		return compiledKey{}, k.err
	}
	dims := k.dims
	implicit := false
	if len(dims) == 0 {
		dims = []Dimension{Peer()}
		implicit = true
	}
	var sb strings.Builder
	needsIP, needsIdentity, needsTenant := false, false, false
	for i, d := range dims {
		if d.err != nil {
			return compiledKey{}, d.err
		}
		if d.kind == 0 {
			return compiledKey{}, fmt.Errorf("%w: dimension %d is the zero Dimension; build one with ratelimit.Peer(), ratelimit.ClientIP(...), ratelimit.Identity(...) and so on", ErrInvalidKey, i)
		}
		switch d.kind {
		case dimPeer, dimClientIP, dimIdentityOrIP:
			needsIP = true
		}
		switch d.kind {
		case dimIdentity, dimIdentityOrIP:
			needsIdentity = true
		case dimTenant:
			needsTenant = true
		}
		if i > 0 {
			sb.WriteByte('+')
		}
		sb.WriteString(d.label)
	}
	// Copy so a caller mutating their slice afterwards cannot reach into a
	// running limiter.
	own := make([]Dimension, len(dims))
	copy(own, dims)
	return compiledKey{
		dims:          own,
		label:         sb.String(),
		implicit:      implicit,
		needsIP:       needsIP,
		needsIdentity: needsIdentity,
		needsTenant:   needsTenant,
	}, nil
}

// dimension tags give each kind its own domain in the hash. Without them
// Identity("a")+Path("b") would collide with Identity("ab").
func (d *Dimension) tag() byte { return byte(d.kind) }

// hash folds the dimensions with one seeded hash per dimension instead of
// streaming them all through a single hasher.
//
// A key almost always has one or two dimensions, so paying one keyed hash per
// dimension and mixing the results beats setting up and draining a streaming
// hasher over all of them.
//
// The mixing folds in the dimension tag and is sequential, so it is sensitive to
// both which dimension a value came from and where it sat. Without that,
// Identity("a") plus Path("b") would collide with Path("a") plus Identity("b"),
// and with a plain concatenating hasher it would collide with Identity("ab").
func (ck *compiledKey) hash(k hashKey, ruleTag uint64, s *Subject) uint64 {
	h := ruleTag
	for i := range ck.dims {
		d := &ck.dims[i]
		h = mix64(h ^ (uint64(d.tag()) * 0x100000001b3))
		switch d.kind {
		case dimPeer, dimClientIP:
			h = mix64(h ^ hashAddr(k, s.IP))
		case dimIdentity:
			h = mix64(h ^ sipString(k, s.Identity))
		case dimIdentityOrIP:
			if s.Identity != "" {
				h = mix64(h ^ sipString(k, s.Identity))
			} else {
				h = mix64(^h ^ hashAddr(k, s.IP))
			}
		case dimTenant:
			h = mix64(h ^ sipString(k, s.Tenant))
		case dimPath:
			h = mix64(h ^ sipString(k, s.Path))
		case dimMethod:
			h = mix64(h ^ sipString(k, s.Method))
		case dimHost:
			h = mix64(h ^ sipString(k, s.Host))
		case dimContext:
			h = mix64(h ^ sipString(k, contextString(s, d.ctxKey)))
		}
	}
	return h
}

func hashAddr(k hashKey, a netip.Addr) uint64 {
	if !a.IsValid() {
		return sipString(k, "")
	}
	b := a.As16()
	return sipBytes(k, b[:])
}

func contextString(s *Subject, key any) string {
	if s.Request == nil {
		return ""
	}
	v := s.Request.Context().Value(key)
	if str, ok := v.(string); ok {
		return str
	}
	return ""
}
