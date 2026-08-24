// Package redis is a [ratelimit.Backend] backed by Redis.
//
// It lives in its own module so that using ratelimit with the standard library
// does not pull in a Redis client. The root module has no external dependencies
// and CI checks that mechanically.
//
// # What it does, and what it does not
//
// It moves demand: each node reports how much it is being asked for on a key,
// and reads back how much everyone else is asking for. It contains no rate
// limiting logic at all - no quotas, no windows, no costs, no algorithms - which
// is why it is a few hundred lines instead of a second implementation of the
// limiter.
//
// It is never on the decision path. Local state always decides; this only
// adjusts how much of the global quota a node hands out. If Redis is slow,
// unreachable or gone, requests are still served and still limited per process,
// and the limiter says it is degraded.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/imlargo/ratelimit"
)

// Client is the subset of a go-redis client this backend uses.
//
// It is an interface rather than a concrete type so that Client, ClusterClient,
// Ring and a Sentinel-backed client all work. *redis.Client and its siblings
// satisfy it as they are.
type Client interface {
	redis.Scripter
	Pipeline() redis.Pipeliner
}

// Options configures a Backend.
type Options struct {
	// Prefix is prepended to every key this backend writes. Required: an
	// explicit prefix is what keeps two deployments sharing one Redis from
	// silently sharing rate limits.
	Prefix string

	// Horizon is how long a node's report stays live. A node that stops
	// reporting stops holding quota after this. Defaults to 15 seconds, which
	// should be several times your sync interval.
	Horizon time.Duration
}

// Backend implements [ratelimit.Backend] against Redis.
type Backend struct {
	c       Client
	prefix  string
	horizon time.Duration

	// loaded tracks whether the script is known to be cached server side.
	//
	// Inside a pipeline there is no chance to fall back from EVALSHA to EVAL:
	// the NOSCRIPT comes back after the whole batch has been sent. So the script
	// is loaded before the first batch, and any NOSCRIPT clears this so the next
	// round loads it again - which is what happens after a Redis restart or a
	// SCRIPT FLUSH.
	loaded atomic.Bool
}

// New returns a Backend. It does not talk to Redis: the first Sync does, and a
// failure there is reported as degraded operation rather than as a startup
// error, because a rate limiter must not refuse to start because its optional
// coordinator is down.
func New(c Client, opts Options) (*Backend, error) {
	if c == nil {
		return nil, errors.New("ratelimit/redis: client is nil")
	}
	if opts.Prefix == "" {
		return nil, errors.New("ratelimit/redis: Prefix is required. Two deployments sharing one Redis without " +
			"distinct prefixes would silently share each other's rate limits")
	}
	horizon := opts.Horizon
	if horizon == 0 {
		horizon = 15 * time.Second
	}
	return &Backend{c: c, prefix: opts.Prefix, horizon: horizon}, nil
}

// syncScript records this node's demand for one key and returns what every other
// live node is demanding.
//
// The whole operation runs inside Redis, so there is no read-then-write race
// between nodes. Time comes from Redis's own clock, so skew between application
// nodes affects nothing at all - which is the single most common source of
// wrong answers in a distributed rate limiter.
//
// One hash per key, one field per node holding "amount:timestamp". Fields from
// nodes that have gone quiet are filtered on read and pruned opportunistically,
// so no version of Redis with per-field expiry is required.
var syncScript = redis.NewScript(`
local key     = KEYS[1]
local node    = ARGV[1]
local amount  = ARGV[2]
local ttl_ms  = tonumber(ARGV[3])
local horizon = tonumber(ARGV[4])

local t     = redis.call("TIME")
local now   = t[1] * 1000 + math.floor(t[2] / 1000)

redis.call("HSET", key, node, amount .. ":" .. now)
redis.call("PEXPIRE", key, ttl_ms)

local others, count, stale = 0, 0, {}
local all = redis.call("HGETALL", key)
for i = 1, #all, 2 do
  local who = all[i]
  local val = all[i + 1]
  local sep = string.find(val, ":", 1, true)
  if sep then
    local amt = tonumber(string.sub(val, 1, sep - 1))
    local at  = tonumber(string.sub(val, sep + 1))
    if amt and at then
      if now - at > horizon then
        if who ~= node then stale[#stale + 1] = who end
      elseif who ~= node then
        others = others + amt
        count  = count + 1
      end
    end
  end
end

if #stale > 0 then
  redis.call("HDEL", key, unpack(stale))
end

return { tostring(others), count }
`)

// Sync implements [ratelimit.Backend].
//
// Each key is one small script invocation, pipelined. That is deliberate: a
// single script over many keys would need every key in one Redis Cluster slot,
// which cannot be arranged for keys that are hashes of unrelated identities.
// Pipelining keeps it one round trip per sync round on a single node and one per
// slot on a cluster, and it works on Client, ClusterClient and Ring without
// special cases.
func (b *Backend) Sync(ctx context.Context, node string, demand []ratelimit.Demand) ([]ratelimit.Share, error) {
	if len(demand) == 0 {
		// A round with nothing to say still has to prove Redis is reachable, or
		// we would report a healthy backend we have not spoken to.
		return nil, b.ping(ctx)
	}

	if err := b.ensureLoaded(ctx); err != nil {
		return nil, err
	}

	ttlMs := int64(0)
	pipe := b.c.Pipeline()
	cmds := make([]*redis.Cmd, len(demand))
	for i, d := range demand {
		ttlMs = d.TTL.Milliseconds()
		if ttlMs < 1000 {
			ttlMs = 1000
		}
		cmds[i] = syncScript.Run(ctx, pipe, []string{b.key(d.Key)},
			node,
			strconv.FormatInt(int64(d.Amount), 10),
			ttlMs,
			b.horizon.Milliseconds(),
		)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		if isNoScript(err) {
			b.loaded.Store(false)
		}
		return nil, fmt.Errorf("ratelimit/redis: sync: %w", err)
	}

	out := make([]ratelimit.Share, len(demand))
	for i, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil {
			return nil, fmt.Errorf("ratelimit/redis: sync key %d: %w", i, err)
		}
		others, nodes, err := parseShare(v)
		if err != nil {
			return nil, fmt.Errorf("ratelimit/redis: sync key %d: %w", i, err)
		}

		out[i] = ratelimit.Share{Key: demand[i].Key, Others: others, Nodes: nodes}
	}
	return out, nil
}

func parseShare(v any) (time.Duration, int, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return 0, 0, fmt.Errorf("unexpected script reply %T", v)
	}
	s, ok := arr[0].(string)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected demand total %T", arr[0])
	}
	amount, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("demand total %q: %w", s, err)
	}
	n, ok := arr[1].(int64)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected node count %T", arr[1])
	}
	return time.Duration(amount), int(n), nil
}

// ensureLoaded makes sure the script is cached server side before a pipeline
// relies on EVALSHA.
func (b *Backend) ensureLoaded(ctx context.Context) error {
	if b.loaded.Load() {
		return nil
	}
	if _, err := syncScript.Load(ctx, b.c).Result(); err != nil {
		return fmt.Errorf("ratelimit/redis: loading the sync script: %w", err)
	}
	b.loaded.Store(true)
	return nil
}

// ping proves Redis is reachable on a round with nothing to publish. A round
// that reports success without having spoken to Redis would report a healthy
// backend we have not actually reached.
func (b *Backend) ping(ctx context.Context) error {
	b.loaded.Store(false)
	return b.ensureLoaded(ctx)
}

func isNoScript(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOSCRIPT")
}

// Close releases nothing: the client belongs to the caller, who created it and
// is the only one who knows when it is finished with.
func (b *Backend) Close() error { return nil }

func (b *Backend) key(fp uint64) string {
	var buf [40]byte
	n := append(buf[:0], b.prefix...)
	n = append(n, ':')
	n = strconv.AppendUint(n, fp, 36)
	return string(n)
}
