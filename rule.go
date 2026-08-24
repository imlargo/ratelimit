package ratelimit

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrInvalidRule is the class of all rule validation failures.
var ErrInvalidRule = errors.New("invalid rule")

// maxRules bounds a rule table.
//
// The whole table is walked per request and the refund ledger is a stack array
// of this many entries, so the bound keeps both costs flat and keeps the metric
// label set finite. Sixteen rules is a large table for one service: a global
// limit, a per-caller limit, a handful of tightened endpoints and a couple of
// exemptions is under ten.
const maxRules = 16

// Rule is one policy: which requests it applies to, which of them share a
// counter, how much it allows, and what it does when the answer is no.
//
// Every field except Quota has a useful zero value, so moving up in complexity
// adds a field or a rule and never rewrites a line:
//
//	// one global limit, default key, no selector
//	ratelimit.Rule{Quota: ratelimit.PerMinute(100)}
//
//	// same limit, keyed by caller
//	ratelimit.Rule{Quota: ratelimit.PerMinute(100), Key: ratelimit.ByIdentityOrIP(who, cidrs...)}
//
//	// and a tighter one on an expensive endpoint
//	ratelimit.Rule{Selector: "POST /api/search", Quota: ratelimit.PerMinute(10), Key: ...}
//
// The fields are exported for readability, but a Rule is compiled and copied
// when the limiter is built. Changing one afterwards has no effect: a running
// limiter has no mutable public state.
type Rule struct {
	// Selector says which requests this rule applies to, in net/http.ServeMux
	// pattern syntax: an optional method, an optional host, a path with
	// {single} and {rest...} wildcards and the {$} end anchor. Empty matches
	// every request.
	//
	// A selector is not a key. "/api/" keyed by caller is "N per minute across
	// all of /api/ per caller"; the same selector keyed by caller and path is
	// "N per minute on each endpoint per caller". Both are ordinary
	// requirements, so the two are declared separately and never derived from
	// each other.
	Selector string

	// Key says which requests share a counter. The zero value is [Peer]: the
	// address that opened the connection, which needs no configuration and
	// cannot be spoofed by a header.
	Key Key

	// Quota is how much this rule allows. Required unless Exempt is set.
	Quota Quota

	// QuotaFor resolves the quota from the request, for per-plan or per-tier
	// limits. When it is set, Quota is the fallback used if it returns
	// something that does not validate.
	//
	// It runs on the decision path. Keep it to a map lookup or a field read on
	// something your auth middleware already resolved; do not put a database
	// query behind it.
	QuotaFor func(Subject) Quota

	// Cost is how much one request consumes. Zero means one. It is checked
	// against the burst when the limiter is built.
	Cost int64

	// CostFor derives the cost from the request, for endpoints priced by size
	// or by weight. It overrides Cost. A cost larger than the rule's entire
	// burst can never be admitted, so it yields
	// [ReasonCostExceedsBurst] rather than an unexplained denial.
	CostFor func(Subject) int64

	// Shadow makes the rule evaluate, consume, count and report without ever
	// denying. It is the same code path as a live rule with a different reason
	// and its own metric, so what it measures is what it would do.
	//
	// Deploy every new rule this way first. Reading the shadow counter is the
	// only way to size a limit that does not involve finding out in production.
	Shadow bool

	// Exempt makes a match allow the request outright, consuming nothing and
	// stopping evaluation. Exempt rules are checked before all others.
	//
	// Health checks, monitoring probes and internal traffic belong here, as a
	// declared and visible rule rather than a condition buried in a middleware.
	Exempt bool

	// Name labels the rule in decisions, metrics and the RateLimit-Policy
	// header. Defaults to its position in the table.
	Name string
}

// rule is the compiled, hot-path form.
type rule struct {
	name     string
	sel      selector
	key      compiledKey
	q        quota
	quotaFor func(Subject) Quota
	cost     int64
	costFor  func(Subject) int64
	shadow   bool
	exempt   bool
	tag      uint64 // per-rule hash domain, so rules never share cells
	idx      uint32
}

func compileRules(in []Rule, haveIdentity, haveTenant bool) ([]rule, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: no rules; a limiter with no rules limits nothing", ErrInvalidRule)
	}
	if len(in) > maxRules {
		return nil, fmt.Errorf("%w: %d rules exceeds the maximum of %d", ErrInvalidRule, len(in), maxRules)
	}

	out := make([]rule, 0, len(in))
	seen := make(map[string]int, len(in))

	for i, r := range in {
		name := r.Name
		if name == "" {
			name = "r" + strconv.Itoa(i)
		}
		if err := validateName(name); err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, name, err)
		}
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("%w: rules %d and %d are both named %q; names label metrics and headers, so they must be unique",
				ErrInvalidRule, prev, i, name)
		}
		seen[name] = i

		sel, err := compileSelector(r.Selector)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%s): selector %w", i, name, err)
		}
		key, err := r.Key.compile()
		if err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, name, err)
		}
		if key.needsIdentity && !haveIdentity {
			return nil, fmt.Errorf("%w: rule %d (%s) keys by identity but Config.Identity is not set. "+
				"Set it to the function that reads the caller out of a request, for example "+
				"func(r *http.Request) (string, bool) { u, ok := auth.From(r.Context()); return u.ID, ok }. "+
				"If you are not serving HTTP and fill Subject.Identity yourself, set Config.Identity to ratelimit.IdentityFromSubject",
				ErrInvalidRule, i, name)
		}
		if key.needsTenant && !haveTenant {
			return nil, fmt.Errorf("%w: rule %d (%s) keys by tenant but Config.Tenant is not set. "+
				"If you fill Subject.Tenant yourself, set it to ratelimit.IdentityFromSubject", ErrInvalidRule, i, name)
		}

		cr := rule{
			name:     name,
			sel:      sel,
			key:      key,
			quotaFor: r.QuotaFor,
			costFor:  r.CostFor,
			shadow:   r.Shadow,
			exempt:   r.Exempt,
		}

		switch {
		case r.Exempt:
			if !r.Quota.IsZero() {
				return nil, fmt.Errorf("%w: rule %d (%s) is Exempt and also carries a quota. "+
					"An exempt rule allows unconditionally; drop the quota, or drop Exempt and set one", ErrInvalidRule, i, name)
			}
			if r.Shadow {
				return nil, fmt.Errorf("%w: rule %d (%s) is both Exempt and Shadow. "+
					"A shadowed exemption has no observable effect", ErrInvalidRule, i, name)
			}
			if r.CostFor != nil || r.Cost != 0 {
				return nil, fmt.Errorf("%w: rule %d (%s) is Exempt and sets a cost; an exempt rule consumes nothing", ErrInvalidRule, i, name)
			}
			if r.QuotaFor != nil {
				return nil, fmt.Errorf("%w: rule %d (%s) is Exempt and sets QuotaFor; an exempt rule has no quota to resolve", ErrInvalidRule, i, name)
			}
		default:
			q, err := r.Quota.compile()
			if err != nil {
				return nil, fmt.Errorf("rule %d (%s): %w", i, name, err)
			}
			cr.q = q

			cost := r.Cost
			if cost == 0 {
				cost = 1
			}
			if cost < 0 {
				return nil, fmt.Errorf("%w: rule %d (%s) has Cost %d; cost cannot be negative", ErrInvalidRule, i, name, cost)
			}
			if r.CostFor == nil && cost > q.burst {
				return nil, fmt.Errorf("%w: rule %d (%s) has Cost %d but its burst is only %d, so no request could ever be admitted. "+
					"Raise the quota, raise the burst with Quota.WithBurst, or lower the cost", ErrInvalidRule, i, name, cost, q.burst)
			}
			cr.cost = cost
		}
		out = append(out, cr)
	}

	// Evaluation order is a performance decision with no effect on the outcome:
	// exemptions first so nothing is consumed and then refunded, then the rules
	// most likely to deny, so a request that is going to be rejected is
	// rejected before other rules have taken quota that must be given back.
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].exempt != out[b].exempt {
			return out[a].exempt
		}
		if out[a].exempt {
			return false
		}
		if out[a].q.tau != out[b].q.tau {
			return out[a].q.tau < out[b].q.tau
		}
		return out[a].q.limit < out[b].q.limit
	})

	for i := range out {
		out[i].idx = uint32(i)
		// A stable, well spread per-rule hash domain. Rules must never share a
		// cell even when their keys are identical.
		out[i].tag = 0x9E3779B97F4A7C15 * (uint64(i) + 1)
	}
	return out, nil
}

// validateName keeps a rule name usable as a structured-field string in the
// RateLimit-Policy header and as a metric label.
func validateName(n string) error {
	if len(n) > 64 {
		return fmt.Errorf("%w: name %q is longer than 64 characters", ErrInvalidRule, n)
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return fmt.Errorf("%w: name %q contains %q; names must be printable ASCII without quotes or backslashes so they can go in a header", ErrInvalidRule, n, string(c))
		}
	}
	return nil
}

// policyHeader precomputes the whole RateLimit-Policy field once, at build
// time. It never varies per request, so it costs nothing to emit.
func policyHeader(rules []rule) string {
	var sb strings.Builder
	for i := range rules {
		r := &rules[i]
		// Exempt rules have no quota, and advertising a shadow rule as policy
		// would tell clients about a limit that does not deny.
		if r.exempt || r.shadow {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('"')
		sb.WriteString(r.name)
		sb.WriteString(`";q=`)
		sb.WriteString(strconv.FormatInt(r.q.limit, 10))
		sb.WriteString(";w=")
		// The field is whole seconds; round a sub-second window up so it never
		// advertises a window of zero.
		sb.WriteString(strconv.FormatInt((r.q.window+1e9-1)/1e9, 10))
	}
	return sb.String()
}
