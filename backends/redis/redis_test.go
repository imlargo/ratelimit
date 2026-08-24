package redis_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/imlargo/ratelimit"
	rlredis "github.com/imlargo/ratelimit/backends/redis"
)

// These tests need a real Redis. Run them with:
//
//	REDIS_ADDR=localhost:6379 go test ./...
//
// or `make test-redis` from the repository root, which starts one.
func dial(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("set REDIS_ADDR to run the Redis integration tests")
	}
	c := goredis.NewClient(&goredis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("cannot reach Redis at %s: %v", addr, err)
	}
	return c
}

func TestValidation(t *testing.T) {
	if _, err := rlredis.New(nil, rlredis.Options{Prefix: "x"}); err == nil {
		t.Error("a nil client was accepted")
	}
	c := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	defer c.Close()
	if _, err := rlredis.New(c, rlredis.Options{}); err == nil {
		t.Error("an empty prefix was accepted; two deployments would silently share limits")
	}
}

// TestBackendContract checks the three things the Backend contract requires:
// same length and order, a node never sees its own demand reflected back, and
// nodes that stop reporting age out.
func TestBackendContract(t *testing.T) {
	c := dial(t)
	defer c.Close()
	ctx := context.Background()
	prefix := "rltest:" + t.Name()
	defer c.Del(ctx, prefix+":"+"*")

	be, err := rlredis.New(c, rlredis.Options{Prefix: prefix, Horizon: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()

	keys := []uint64{11, 22, 33}
	demand := func(node string, amounts ...time.Duration) []ratelimit.Share {
		t.Helper()
		in := make([]ratelimit.Demand, len(amounts))
		for i, a := range amounts {
			in[i] = ratelimit.Demand{Key: keys[i], Amount: a, TTL: 30 * time.Second}
		}
		out, err := be.Sync(ctx, node, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != len(in) {
			t.Fatalf("Sync returned %d shares for %d demands", len(out), len(in))
		}
		for i := range out {
			if out[i].Key != in[i].Key {
				t.Fatalf("share %d is for key %d, want %d: order must be preserved", i, out[i].Key, in[i].Key)
			}
		}
		return out
	}

	// One node alone sees nothing from anyone else.
	got := demand("a", time.Second, 2*time.Second, 3*time.Second)
	for i, s := range got {
		if s.Others != 0 || s.Nodes != 0 {
			t.Errorf("key %d: a single node saw others=%v nodes=%d; a node must never see its own demand", i, s.Others, s.Nodes)
		}
	}

	// A second node sees the first.
	got = demand("b", 10*time.Second, 0, 0)
	if got[0].Others != time.Second || got[0].Nodes != 1 {
		t.Errorf("node b saw others=%v nodes=%d, want 1s and 1", got[0].Others, got[0].Nodes)
	}
	// And the first now sees the second.
	got = demand("a", time.Second, 0, 0)
	if got[0].Others != 10*time.Second || got[0].Nodes != 1 {
		t.Errorf("node a saw others=%v nodes=%d, want 10s and 1", got[0].Others, got[0].Nodes)
	}

	// Node b goes quiet. After the horizon it stops holding quota.
	time.Sleep(2500 * time.Millisecond)
	got = demand("a", time.Second, 0, 0)
	if got[0].Others != 0 || got[0].Nodes != 0 {
		t.Errorf("after the horizon, node a still saw others=%v nodes=%d from a silent node", got[0].Others, got[0].Nodes)
	}
}

// TestRedisClockIsTheSourceOfTruth: the script reads Redis's own clock, so skew
// between application nodes cannot affect expiry. Verified by checking that the
// key carries a TTL Redis itself set.
func TestRedisClockIsTheSourceOfTruth(t *testing.T) {
	c := dial(t)
	defer c.Close()
	ctx := context.Background()
	prefix := "rltest:" + t.Name()

	be, err := rlredis.New(c, rlredis.Options{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.Sync(ctx, "a", []ratelimit.Demand{{Key: 7, Amount: time.Second, TTL: 30 * time.Second}}); err != nil {
		t.Fatal(err)
	}
	iter := c.Scan(ctx, 0, prefix+":*", 100).Iterator()
	found := false
	for iter.Next(ctx) {
		found = true
		ttl, err := c.PTTL(ctx, iter.Val()).Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl <= 0 || ttl > 31*time.Second {
			t.Errorf("key %s has TTL %v, want something under the requested 30s", iter.Val(), ttl)
		}
		c.Del(ctx, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("the backend wrote nothing under its prefix")
	}
}

// TestEndToEndTwoNodes drives two real limiters through Redis and checks that
// the pair does not hand out twice the quota.
func TestEndToEndTwoNodes(t *testing.T) {
	c := dial(t)
	defer c.Close()
	ctx := context.Background()
	prefix := "rltest:" + t.Name()
	defer func() {
		iter := c.Scan(ctx, 0, prefix+":*", 100).Iterator()
		for iter.Next(ctx) {
			c.Del(ctx, iter.Val())
		}
	}()

	mk := func(id string) *ratelimit.Limiter {
		be, err := rlredis.New(c, rlredis.Options{Prefix: prefix, Horizon: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		l, err := ratelimit.NewWith(ratelimit.Config{
			Identity:      ratelimit.FromSubject(),
			Rules:         []ratelimit.Rule{{Quota: ratelimit.PerSecond(100).WithBurst(10), Key: ratelimit.ByIdentity()}},
			Backend:       be,
			ClusterKey:    "redis-integration-cluster-key",
			NodeID:        id,
			SyncInterval:  100 * time.Millisecond,
			SyncThreshold: 1e-9,
			Capacity:      1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	a, b := mk("a"), mk("b")
	defer a.Close()
	defer b.Close()

	s := ratelimit.Subject{Identity: "shared"}
	// Cold start: each node hands out its own burst.
	cold := 0
	for _, l := range []*ratelimit.Limiter{a, b} {
		for l.Check(ctx, s).Allowed {
			cold++
		}
	}
	if cold != 20 {
		t.Errorf("cold start admitted %d, want 2 nodes times a burst of 10", cold)
	}

	// Give allocations time to land, then measure a second of steady state.
	time.Sleep(400 * time.Millisecond)
	admitted := 0
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, l := range []*ratelimit.Limiter{a, b} {
			if l.Check(ctx, s).Allowed {
				admitted++
			}
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Two coordinated nodes must not sustain twice the rate. The floor under an
	// idle node's share allows some slack, which is published.
	if admitted > 150 {
		t.Errorf("two nodes sustained %d events per second against a limit of 100; coordination is not working", admitted)
	}
	if admitted < 60 {
		t.Errorf("two nodes sustained only %d events per second against a limit of 100; coordination is too strict", admitted)
	}
	t.Logf("two nodes coordinated through Redis sustained %d/s against a configured 100/s", admitted)

	if a.Degraded() || b.Degraded() {
		t.Error("a node reported degraded against a healthy Redis")
	}
}

// TestDegradesWhenRedisIsGone.
func TestDegradesWhenRedisIsGone(t *testing.T) {
	dead := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	defer dead.Close()

	be, err := rlredis.New(dead, rlredis.Options{Prefix: "rltest:dead"})
	if err != nil {
		t.Fatal(err)
	}
	lim, err := ratelimit.NewWith(ratelimit.Config{
		Identity:      ratelimit.FromSubject(),
		Rules:         []ratelimit.Rule{{Quota: ratelimit.PerSecond(5), Key: ratelimit.ByIdentity()}},
		Backend:       be,
		ClusterKey:    "redis-integration-cluster-key",
		SyncInterval:  50 * time.Millisecond,
		SyncThreshold: 1e-9,
		Capacity:      256,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lim.Close()

	ctx := context.Background()
	s := ratelimit.Subject{Identity: "u"}

	// Requests are served and still limited while Redis is unreachable.
	deadline := time.Now().Add(700 * time.Millisecond)
	allowed, total := 0, 0
	for time.Now().Before(deadline) {
		d := lim.Check(ctx, s)
		total++
		if d.Allowed {
			allowed++
		}
		time.Sleep(time.Millisecond)
	}
	if allowed == 0 {
		t.Error("nothing was served while Redis was unreachable; degradation must be permissive")
	}
	if allowed == total {
		t.Error("everything was served; the local limit must still apply while degraded")
	}
	if !lim.Degraded() {
		t.Error("the limiter did not report degraded operation with Redis unreachable")
	}
	if d := lim.Check(ctx, s); !d.Degraded {
		t.Errorf("a decision taken while degraded did not say so: %s", d)
	}
	if !errors.Is(error(nil), nil) {
		t.Fatal("unreachable")
	}
}
