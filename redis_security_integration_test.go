package challenge

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Opt in with an isolated Redis instance. No database-wide cleanup is used.
func TestSecurityAudit_RealRedisLeaseExpiry(t *testing.T) {
	addr := os.Getenv("CHALLENGE_KIT_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set CHALLENGE_KIT_TEST_REDIS_ADDR for real Redis")
	}
	rc := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = rc.Close() }()
	cfg := DefaultConfig()
	prefix := "challenge-kit-audit:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":"
	cfg.ChallengeKeyPrefix = prefix + "ch:"
	cfg.VerifyLockPrefix = prefix + "vlock:"
	cfg.LockKeyPrefix = prefix + "lock:"
	gate := &snapshotGate{RedisClient: rc, reached: make(chan struct{}), release: make(chan struct{})}
	// The gate also works with unique integration prefixes.
	gateKey := cfg.ChallengeKeyPrefix
	wrapper := &integrationGate{snapshotGate: gate, prefix: gateKey}
	manager := NewManager(wrapper, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, code, err := manager.Create(ctx, CreateRequest{UserID: "u", Channel: ChannelEmail, Destination: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = rc.Del(context.Background(), cfg.ChallengeKeyPrefix+ch.ID).Err()
		_ = rc.Del(context.Background(), cfg.VerifyLockPrefix+ch.ID).Err()
	}()
	var once sync.Once
	resume := func() { once.Do(func() { close(gate.release) }) }
	defer resume()
	gate.armed.Store(true)
	result := make(chan *VerifyResult, 1)
	go func() { r, _ := manager.Verify(ctx, ch.ID, code, ""); result <- r }()
	select {
	case <-gate.reached:
	case <-ctx.Done():
		t.Fatal("snapshot was not reached")
	}
	if err = rc.PExpire(ctx, cfg.VerifyLockPrefix+ch.ID, time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	for {
		count, err := rc.Exists(ctx, cfg.VerifyLockPrefix+ch.ID).Result()
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("lease did not expire")
		case <-time.After(time.Millisecond):
		}
	}
	second, err := NewManager(rc, cfg).Verify(ctx, ch.ID, code, "")
	if err != nil || !second.OK {
		t.Fatalf("second result=%v err=%v", second, err)
	}
	resume()
	select {
	case first := <-result:
		if first == nil || first.OK {
			t.Fatalf("real Redis double consumption: %+v", first)
		}
	case <-ctx.Done():
		t.Fatal("first verifier did not finish")
	}
}

type integrationGate struct {
	*snapshotGate
	prefix string
}

func (g *integrationGate) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := g.RedisClient.Get(ctx, key)
	if len(key) >= len(g.prefix) && key[:len(g.prefix)] == g.prefix && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return cmd
}
