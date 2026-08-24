# Security

A rate limiter is a security control. One that can be bypassed is worse than
none, because it manufactures confidence.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository:
**Security → Report a vulnerability**. Please do not open a public issue for
anything exploitable.

Include what you can reproduce with. A failing test is the fastest possible
report.

## What counts as a vulnerability here

Anything that lets a caller consume more than its configured quota, or consume
another caller's quota:

- Deriving a client identity that the caller controls — a forwarding header that
  is trusted when it should not be, an address spelling that reads as two
  identities, a path that evades a selector.
- Causing two different keys to share a counter, or resetting a victim's counter.
  The key store only ever recycles a counter that has fully recovered, precisely
  so that churn cannot reset anyone; a way around that is a vulnerability.
- Making the limiter admit more than its published bounds. Those bounds are in
  the README, with the tests that measure them. Exceeding a published bound is a
  bug; the bound itself being wider than you hoped is not.
- Unbounded resource growth driven by request data — the key store, metric
  labels, log volume, allocations per request. All of these are bounded by
  design and asserted by tests.

## What does not

- Exceeding a quota by less than the published bound. Read the bounds first; they
  are stated with their measurements rather than implied.
- Per-process enforcement without a backend. N replicas enforce N times the rate,
  which is documented, and configuring a `Backend` is the answer.
- Denial of a legitimate new key when the key store is genuinely full. That is
  reported as `ReasonStoreSaturated` with its own metric, and it means the
  capacity is too small for the active key set. It is an operational condition,
  and refusing rather than admitting silently is what stops churn being used to
  reset counters.

## Supported versions

Pre-1.0. Fixes land on the latest minor version only.
