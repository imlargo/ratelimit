# ratelimit

**The only Go rate limiter that tells you when it stopped limiting.**

**And that lets you see what it would have blocked, before it blocks it.**

Rate limiting for incoming HTTP APIs. No dependencies in the core, no framework
assumptions, one line for the common case, and every guarantee it makes is
written down with its bound.

```go
lim, err := ratelimit.New(ratelimit.Rule{Quota: ratelimit.PerMinute(100)})
if err != nil {
    return err
}
defer lim.Close()

mux.Handle("/", lim.Limit(handler))
```

That is the whole of the common case. No algorithm to pick, no store to wire up,
no key function to write, and a default key that cannot be forged with a header.

---

## Why another one

`golang.org/x/time/rate`, `go-redis/redis_rate`, `sethvargo/go-limiter`,
`ulule/limiter`, `throttled/throttled` and `didip/tollbooth` all exist, several
are mature, and this is not sold on a gap that does not exist. It is sold on four
specific things, each verifiable in their source today:

**None of them has a degradation policy.** When the shared store stops
answering, you get an `error`. What happens next is whatever you wrote:
`throttled` answers **500** by default, `redis_rate` hands the error back and
leaves it to you. A Redis blip becomes an API outage, or a limiter that quietly
switched off — and either way nothing tells you which. Here, losing the backend
puts the limiter in single-node mode, every decision taken that way says
`ReasonAllowedDegraded`, a metric moves, and the log says it once.

**None of them emits the current standard fields.** `RateLimit` and
`RateLimit-Policy`, from the IETF httpapi working group draft, are what turn a
429 into something a client can cooperate with. `sethvargo/go-limiter` emits
`X-RateLimit-*` and its own source calls them *"the recommended return header
values from IETF"* — they are not, and never were.

**All of them put the algorithm in the interface.** `sethvargo/go-limiter`'s
store returns `(tokens, remaining, reset uint64, ok bool, err error)`. A number
whose meaning depends on which algorithm produced it cannot be in a public API:
the caller reads "remaining tokens" as a retry hint and tells clients to come
back at a time that means nothing.

**Two things none of them do at all.** Several rules evaluated together with
quota given back when a later one denies, and shadow mode.

There is also one thing several of them get actively dangerous.
`sethvargo/go-limiter`'s `IPKeyFunc(headers ...string)` trusts whatever header
you name, unconditionally. That is a limiter anyone can side-step by setting a
header, which is worse than no limiter, because it manufactures confidence. Here
the IP dimension **refuses to build** without declared trusted proxies.

---

## What it guarantees, and what those guarantees cost

Every number below is asserted by a test in this repository, not argued for.

### Single node

| | |
|---|---|
| Sustained rate | exactly the configured quota |
| First window from a fully recovered counter | up to **1 + burst/limit** times the quota |
| Default burst | the full quota, so the default first-window bound is **1.99x** |
| Decision latency | ~120 ns, one rule, warm counter (Apple M3) |
| Allocations per decision | **0**, asserted as a build gate |
| Allocations per HTTP request | **5**, all of them rendering headers |
| Memory | exactly `capacity x 16 bytes`, and it cannot grow past it |
| Memory with a backend configured | exactly `capacity x 40 bytes` |
| Background goroutines | **none** |

**About that 1.99x.** `PerMinute(100)` admits 100 events immediately and then one
every 600 ms. A client hammering from a cold counter therefore gets up to 199 in
the first minute, and exactly 100 in every minute after. That is inherent to a
token bucket with a burst equal to the quota — it is the same 2x that makes fixed
windows unusable, except it happens once from a recovered counter rather than at
every window boundary, and it is smooth rather than a cliff.

If you need it tighter, say so and the bound moves with it:

```go
ratelimit.PerMinute(100).WithBurst(10)   // first-window bound 1.10x, measured 1.09x
```

**What this is not.** A quota here is not "no more than N in any moving window of
W". It is "N per W, with a burst". If you need the moving-window guarantee, this
package does not provide it and will not pretend to.

### Several nodes

Local state always decides. The backend never sits on the decision path: it
exchanges *demand* in the background and adjusts how much of the global quota
this node hands out.

Per key, over the first window, with `N` nodes and a sync interval `T`:

```
nodes x burst          the uncoordinated cold start
+ limit                the window's own sustained allowance
+ nodes x rate x T     one coordination gap
```

Measured against that formula at 61% to 100% of it, across node counts, bursts
and intervals:

| nodes | burst | interval | admitted in the first window | as a multiple of the quota |
|---|---|---|---|---|
| 1 | 600 | 200 ms | 1199 | 2.00x |
| 4 | 600 | 200 ms | 2407 | 4.01x |
| 4 | 60 | 200 ms | 666 | **1.11x** |
| 8 | 60 | 200 ms | 672 | **1.12x** |
| 2 | 600 | 500 ms | 6801 | 1.13x |

Read the third and fourth rows against the second. **The burst dominates, not
the sync interval.** Eight coordinated nodes with a burst of a tenth of the quota
are tighter than four nodes with a full burst. If distributed accuracy matters to
you, lower the burst; shortening the sync interval buys almost nothing.

In steady state the sustained rate is the configured quota plus the slack from
the floor under an idle node's share, which is bounded at 25%.

**On recovery, nothing is replayed.** What a node admitted while the backend was
down was over-admission, and it is declared as such. Pushing it back would
produce a synchronised wall of denials at the worst possible moment — right after
an incident ended.

### Keys

| | |
|---|---|
| Fingerprint collision, `n` active keys | `n²/2⁶⁵` — about 1.4e-8 at a million keys |
| New key refused, capacity ≥ 2x active keys | under 1 in 10,000 |
| Time horizon from limiter construction | ~292 years |

Size `Capacity` at two or more times the peak number of *simultaneously active*
keys — keys with quota consumed and not yet recovered, not every key you have
ever seen. Measured refusal rates by load factor are in
`TestSaturationOnsetByLoadFactor`.

---

## What it will not do

Declared out loud, because finding out later is worse.

- **Outbound rate limiting.** Respecting someone else's limits — waiting, backing
  off, reading their headers — shares the core and nothing else. Out of scope.
- **Queueing incoming requests.** Deliberately absent. Queueing instead of
  refusing turns a cheap rejection into a held connection, a goroutine and a file
  descriptor. Under abuse — which is when a limiter matters — that amplifies the
  damage instead of containing it.
- **Exact quota with transactional guarantees.** This is not a billing system.
  Neither mode here is fit for accounting anything with legal weight.
- **Load shedding and adaptive limiting.** Refusing because the *server* is
  struggling is a different problem with a different status code. Out of scope.
- **Abuse detection, automatic banning, reputation.** A rate limiter counts. It
  does not decide who is malicious.
- **Volumetric DDoS protection.** That gets stopped earlier, at the edge.
- **Being a standalone service.** Envoy's rate limit service exists for that.
  This is a library to embed.

---

## The shape of it

### Selector and key are different questions

| | Answers | Example |
|---|---|---|
| **Selector** | which rule applies to this request | `GET /api/v1/` |
| **Key** | which requests share a counter | caller · address · caller+endpoint |

The same rule with a different key behaves completely differently. `/api/v1/`
keyed by caller is "100 a minute across all of v1, per caller". The same selector
keyed by caller and path is "100 a minute on each endpoint, per caller". Both are
ordinary requirements, so they are declared separately and never derived from one
another.

Selectors use `net/http.ServeMux` pattern syntax, so there is nothing new to
learn: an optional method, an optional host, `{single}` and `{rest...}` wildcards
and the `{$}` anchor. Patterns are validated by the standard library's own
parser, so the errors are the ones you already know — plus a check the standard
library does not do, which is that the method is a real one. `ServeMux` accepts
`GTE /api/` and then never matches anything.

The matching is this package's own, for two reasons. A `ServeMux` reports only
its single most specific match, and here every rule that matches is evaluated. A
`ServeMux` also *panics* when two registered patterns overlap without one being
more specific — which is the shape of a global rule plus a narrower one.

### Rules add up; they do not replace each other

A more specific rule does not relax a general one. Both apply and the tighter one
governs. This is deliberately unlike routing: a security control that can be
loosened by declaring something narrower is a hole.

### Quota is given back

When a later rule denies, the quota earlier rules already took is returned.
Without it, a caller who keeps hitting one strict endpoint also burns its global
quota, and its effective limit is tighter than the configured one in a way nobody
can predict from the configuration.

Rules are also evaluated strictest first, so a request that is going to be
refused is usually refused before any broader rule has charged for it at all.

```go
lim, err := ratelimit.NewWith(ratelimit.Config{
    Identity: func(r *http.Request) (string, bool) { ... },
    Rules: []ratelimit.Rule{
        {Name: "health",  Selector: "GET /healthz",     Exempt: true},
        {Name: "auth",    Selector: "POST /api/login",  Quota: ratelimit.PerMinute(5),
            Key: ratelimit.ByIP(ratelimit.PrivateRanges...)},
        {Name: "search",  Selector: "GET /api/search",  Quota: ratelimit.PerMinute(600),
            Key: ratelimit.ByIdentity(), Cost: 20},
        {Name: "global",  Quota: ratelimit.PerMinute(1000),
            Key: ratelimit.ByIdentityOrIP(ratelimit.PrivateRanges...)},
    },
})
```

### Shadow mode

```go
{Name: "export", Selector: "POST /api/export", Quota: ratelimit.PerHour(10), Shadow: true}
```

The rule evaluates, consumes, counts and reports — and never denies. It is the
same code path as a live rule with a different reason and its own counter, so
what it measures is what it would do. Deploy every new limit this way first;
reading the shadow counter is the only way to size a limit that does not involve
finding out in production.

### The decision is a value

```go
d := lim.CheckRequest(r)
d.WriteHeaders(w.Header())
if !d.Allowed {
    // d.Reason, d.Rule, d.Limit, d.Remaining, d.ResetAfter, d.RetryAfter,
    // d.Degraded, d.Shadowed, d.ShadowRule
}
```

A `bool` cannot carry which rule denied, how long to wait, whether the limiter
was running degraded, or whether a shadow rule would have refused. Header
rendering lives on the decision rather than in the middleware, so writing your
own middleware does not mean reimplementing the format.

### The IP dimension will not build unsafely

```go
ratelimit.ByIP()                              // error: no trusted proxies declared
ratelimit.ByIP("10.0.0.0/8")                  // fine
ratelimit.ByIP(ratelimit.PrivateRanges...)    // fine
```

Not a warning and not a sensible default: a build error, because the failure is
silent and total. Extraction is rightmost-non-trusted over `X-Forwarded-For`:
a proxy appends to the right, so everything a client can inject sits to the left
of the first hop you can vouch for. Reading the leftmost value instead — which
is what most Go middleware does — lets any caller pick its own identity.

Only `X-Forwarded-For` is consulted. RFC 7239 `Forwarded` is not supported,
deliberately: almost nothing emits it, and consulting two headers is how the
spoofing hole they were both meant to close gets reopened.

The default key, when you declare nothing, is the address that opened the
connection. It needs no configuration and cannot be forged. Behind a proxy it
groups everything into one counter, which would be a silent misconfiguration, so
the limiter says so once if it keeps seeing private peer addresses.

---

## Headers

By default, the two fields the current IETF draft defines, plus `Retry-After`:

```
RateLimit-Policy: "search";q=10;w=60, "global";q=1000;w=60
RateLimit: "search";r=3;t=42
Retry-After: 44
```

`RateLimit-Policy` never varies for a given rule table, so it is rendered once
when the limiter is built.

`t` is the effective window and is exact. `Retry-After` is advice, and carries a
small **positive** jitter so that a crowd denied in the same instant does not
return in the same instant. One-sided on purpose: the draft requires
`Retry-After` to take precedence and says it should not point earlier than the
end of the effective window, so a symmetric jitter would violate that half the
time and send clients back before they have quota.

**429, never 503.** 429 means the caller exceeded its quota. 503 means the server
cannot cope. They are different situations with different client behaviour.

The de-facto `X-RateLimit-*` family is opt-in:

```go
Legacy: ratelimit.LegacyXRateLimit
```

Off by default, and only one reading is offered, because there is no correct
default. `X-RateLimit-Reset` means a count of seconds at X/Twitter and a Unix
timestamp at GitHub. A number whose meaning depends on who reads it is exactly
the silent failure this package exists to avoid, so the delta-seconds reading is
the one offered — it agrees with the standard field's own semantics — and clients
expecting GitHub's will misread it.

---

## Beyond HTTP

The core does not need HTTP. Queue consumers, gRPC handlers and background jobs
use the same rules:

```go
d := lim.Check(ctx, ratelimit.Subject{Identity: tenantID, Path: "/jobs", Cost: bytes})
```

---

## Distributed

```go
be, err := rlredis.New(rdb, rlredis.Options{Prefix: "myservice"})

lim, err := ratelimit.NewWith(ratelimit.Config{
    Rules:      rules,
    Backend:    be,
    ClusterKey: os.Getenv("RATELIMIT_CLUSTER_KEY"), // the same on every node
})
```

Local state decides; the backend adjusts how much of the quota this node hands
out. Nothing on the decision path waits on a remote.

`ClusterKey` is a shared secret, not a name. Key fingerprints are derived from
it, so nodes only agree on which counter is which if they agree on this — and
anyone who knows it could search for a key that collides with a victim's and
drain their quota. Without a backend the derivation is random per process
instead, which is stronger and needs no configuration.

**Why the backend exchanges demand and not counters.** The obvious design —
publish what you consumed, read the global total, subtract — bounds nothing. Each
node's local counter recovers at wall-clock speed, so N nodes sustain N times the
configured rate however often they synchronise. That is measured, not argued: it
was the first implementation here, and `TestOvershootBoundIsPublished` showed
eight nodes sustaining 8.46x. So each node reports what it is being asked for and
enforces its share instead. Nodes under load get most of the quota, idle nodes
keep a bounded floor, and the sustained total is the configured rate.

The whole backend interface is one method:

```go
type Backend interface {
    Sync(ctx context.Context, node string, demand []Demand) ([]Share, error)
    Close() error
}
```

It knows nothing about rules, selectors, quotas, costs, headers or algorithms.
The Redis implementation is a few hundred lines and one Lua script.

---

## Modules

```
github.com/imlargo/ratelimit                       core — standard library only
github.com/imlargo/ratelimit/ratelimittest         test doubles, leak detector, helpers
github.com/imlargo/ratelimit/backends/redis        separate go.mod
github.com/imlargo/ratelimit/metrics/prometheus    separate go.mod
```

The `net/http` middleware is in the core: `net/http` is the standard library, it
adds no requirement, and an HTTP endpoint is what most rate limiting problems
are. Everything that adds a `require` lives in a satellite, and CI checks the
root module's `go.mod` mechanically rather than trusting this paragraph.

Requires Go 1.25 or newer: `testing/synctest` became stable there, and every
time-dependent test in this repository runs inside it. There is not a single
`time.Sleep` in the suite.

---

## Observability

`Metrics` is a struct of optional functions, all nil-safe, so a limiter with no
metrics configured does no metrics work. The Prometheus satellite hands you a
filled-in one — you should never implement it yourself.

Rule names are labels; the rule table is fixed when the limiter is built, so the
label set is bounded and stable. **Keys are never labels, and there is no API
that would let them be.** A key is client-controlled data, so using it as a label
is the same unbounded-cardinality failure as an unbounded key store, moved into
your monitoring. Metrics that are local to the process say so in their names.

---

## Testing your own code

`ratelimittest` ships as part of the product, because a library that is hard to
test does not get tested.

```go
func TestMyHandler(t *testing.T) {
    defer ratelimittest.NoGoroutineLeaks(t)()

    be := ratelimittest.NewBackend()   // a correct in-memory Backend
    // ... build a limiter with it ...

    ratelimittest.AssertQuota(t, lim, subject, 5)
    be.Fail(errors.New("boom"))        // now test degraded operation
}
```

There is not one real `time.Sleep` in this repository's own suite outside the
Redis integration tests. Everything time-dependent runs inside a `testing/synctest`
bubble or against a manually advanced clock, which is why a test covering a
degraded window, a recovery and two full quota windows finishes in ten
milliseconds.

---

## Status

`v0.x` until this has a user who is not its author. The version number is a
promise about the syntax; the guarantees above are the promise about the
semantics, and that is the one you need first.
