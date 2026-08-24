package ratelimit

// Reason says why a [Decision] came out the way it did. It is a typed value,
// never a string: a reason compared by string comparison is a reason that
// silently stops matching when the wording changes.
type Reason uint8

const (
	// ReasonAllowed is a normal admission with quota to spare.
	ReasonAllowed Reason = iota

	// ReasonExempt means a rule declared this request exempt. No quota was
	// consumed and no further rules were evaluated.
	ReasonExempt

	// ReasonDeniedQuota means a rule's quota is exhausted. This is the only
	// reason that denies.
	ReasonDeniedQuota

	// ReasonAllowedShadow means a rule in shadow mode would have denied this
	// request. Quota was consumed and the request is allowed anyway.
	// Decision.ShadowRule names the rule.
	ReasonAllowedShadow

	// ReasonAllowedDegraded means the remote backend is not answering, so the
	// decision was made from local state alone. Over-admission is bounded by
	// the published formula but is no longer bounded by the global quota.
	ReasonAllowedDegraded

	// ReasonStoreSaturated means no cell could be allocated for a new key
	// because every candidate slot holds a key with quota still consumed.
	// Requests from keys that already have a cell are unaffected. This is an
	// operational condition: raise Config.Capacity.
	ReasonStoreSaturated

	// ReasonCostExceedsBurst means the request's cost is larger than the
	// rule's entire burst allowance, so it could never be admitted, not even
	// against an idle cell. This is a configuration error surfaced at request
	// time because the cost was derived from the request.
	ReasonCostExceedsBurst
)

// Allows reports whether a request carrying this reason is served.
func (r Reason) Allows() bool {
	switch r {
	case ReasonAllowed, ReasonExempt, ReasonAllowedShadow, ReasonAllowedDegraded:
		return true
	}
	return false
}

func (r Reason) String() string {
	switch r {
	case ReasonAllowed:
		return "allowed"
	case ReasonExempt:
		return "exempt"
	case ReasonDeniedQuota:
		return "denied_quota"
	case ReasonAllowedShadow:
		return "allowed_shadow"
	case ReasonAllowedDegraded:
		return "allowed_degraded"
	case ReasonStoreSaturated:
		return "store_saturated"
	case ReasonCostExceedsBurst:
		return "cost_exceeds_burst"
	}
	return "unknown"
}
