package challenge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The manager took a *redis.Client, so a Cluster, Sentinel or Ring deployment
// could not use this package at all. These tests pin the widened contract: what
// compiles, what runs, and that widening it did not reopen the nil hole the
// concrete type closed by construction.

// runManagerRoundTrip exercises the whole surface -- create, wrong code,
// correct code, one-time use, the active index and the lockout -- against
// whichever client implementation is handed in.
func runManagerRoundTrip(t *testing.T, client RedisClient) {
	t.Helper()

	cfg := DefaultConfig()
	cfg.MaxAttempts = 2
	manager := NewManager(client, cfg)
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, CreateRequest{
		UserID:      "u1",
		Channel:     ChannelSMS,
		Destination: "13800000000",
		Purpose:     "login",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The active index is a second key touched through the same client, and on
	// a cluster it lands in a different slot from the challenge itself.
	prev, err := manager.SwapActive(ctx, ch)
	if err != nil {
		t.Fatalf("SwapActive: %v", err)
	}
	if prev != "" {
		t.Errorf("SwapActive on a fresh identity returned %q, want no previous", prev)
	}

	// A wrong code consumes exactly one attempt.
	res, err := manager.Verify(ctx, ch.ID, "000000", "1.2.3.4")
	if err == nil {
		t.Fatal("Verify accepted a wrong code")
	}
	if res.Reason != ReasonInvalid {
		t.Errorf("wrong code reason = %q, want %q", res.Reason, ReasonInvalid)
	}
	if res.RemainingAttempts == nil || *res.RemainingAttempts != 1 {
		t.Errorf("RemainingAttempts = %v, want 1", res.RemainingAttempts)
	}

	// The correct code succeeds and consumes the challenge.
	res, err = manager.Verify(ctx, ch.ID, code, "1.2.3.4")
	if err != nil {
		t.Fatalf("Verify with the correct code: %v", err)
	}
	if !res.OK {
		t.Fatalf("Verify with the correct code returned OK=false (%q)", res.Reason)
	}

	// One-time use: the challenge is gone.
	res, err = manager.Verify(ctx, ch.ID, code, "1.2.3.4")
	if err == nil {
		t.Fatal("a consumed challenge was accepted a second time")
	}
	if res.Reason != ReasonExpired {
		t.Errorf("replayed challenge reason = %q, want %q", res.Reason, ReasonExpired)
	}

	if manager.IsUserLocked(ctx, "u1") {
		t.Error("IsUserLocked after a successful verification = true, want false")
	}
}

// TestManagerAcceptsAUniversalClient: redis.UniversalClient is the type a
// service that switches between standalone, Sentinel and Cluster by
// configuration actually holds.
func TestManagerAcceptsAUniversalClient(t *testing.T) {
	mr := miniredis.RunT(t)

	var client redis.UniversalClient = redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: []string{mr.Addr()},
	})
	t.Cleanup(func() { _ = client.Close() })

	runManagerRoundTrip(t, client)
}

// TestManagerAcceptsARing: a Ring is not a *redis.Client at all, so it could
// not be passed before. It also proves the EVAL paths work on a client that
// routes per key.
func TestManagerAcceptsARing(t *testing.T) {
	mr := miniredis.RunT(t)

	ring := redis.NewRing(&redis.RingOptions{
		Addrs: map[string]string{"shard1": mr.Addr()},
	})
	t.Cleanup(func() { _ = ring.Close() })

	runManagerRoundTrip(t, ring)
}

// TestManagerStillAcceptsAConcreteClient: widening the parameter must not cost
// existing callers, who pass *redis.Client.
func TestManagerStillAcceptsAConcreteClient(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()

	runManagerRoundTrip(t, client)
}

// TestNewManagerToleratesATypedNilClient is the hole that taking an interface
// reopens. A *redis.Client field nobody assigned is not == nil once it is
// inside an interface, and calling a command on it panics -- turning a
// misconfiguration into a downed process. Every entry point must instead fail
// closed.
func TestNewManagerToleratesATypedNilClient(t *testing.T) {
	var nilClient *redis.Client // never assigned

	manager := NewManager(nilClient, DefaultConfig())
	ctx := context.Background()

	if _, _, err := manager.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelSMS, Destination: "13800000000"}); err == nil {
		t.Error("Create with a nil client returned no error")
	}

	res, err := manager.Verify(ctx, "ch_whatever", "123456", "")
	if err == nil {
		t.Fatal("Verify with a nil client returned no error")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("Verify err = %v, want it to wrap ErrBackendUnavailable", err)
	}
	if res.Reason != ReasonBackendUnavailable {
		t.Errorf("Verify reason = %q, want %q", res.Reason, ReasonBackendUnavailable)
	}

	// Fails CLOSED: an unusable backend must never read as "not locked".
	if !manager.IsUserLocked(ctx, "u1") {
		t.Error("IsUserLocked with a nil client = false; a broken backend must not unlock users")
	}

	if _, err := manager.SwapActive(ctx, &Challenge{ID: "ch_1", UserID: "u1"}); !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("SwapActive err = %v, want it to wrap ErrBackendUnavailable", err)
	}
	if err := manager.RevokePending(ctx, "ch_1"); !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("RevokePending err = %v, want it to wrap ErrBackendUnavailable", err)
	}
	if err := manager.Revoke(ctx, "ch_1"); err == nil {
		t.Error("Revoke with a nil client returned no error")
	}
	if _, err := manager.Get(ctx, "ch_1"); err == nil {
		t.Error("Get with a nil client returned no error")
	}
}

// TestNewManagerToleratesAnUntypedNilClient covers the plainer mistake, which
// an interface parameter now also lets through the compiler.
func TestNewManagerToleratesAnUntypedNilClient(t *testing.T) {
	manager := NewManager(nil, DefaultConfig())

	if _, _, err := manager.Create(context.Background(), CreateRequest{UserID: "u1"}); err == nil {
		t.Error("Create with a nil client returned no error")
	}
	if !manager.IsUserLocked(context.Background(), "u1") {
		t.Error("IsUserLocked with a nil client = false, want true (fail closed)")
	}
}

// countingClient records which commands a caller issues, so the set of
// commands the manager needs stays the documented one.
type countingClient struct {
	RedisClient
	calls map[string]int
}

func (c *countingClient) note(name string) {
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[name]++
}

func (c *countingClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	c.note("Del")
	return c.RedisClient.Del(ctx, keys...)
}

func (c *countingClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	c.note("Eval")
	return c.RedisClient.Eval(ctx, script, keys, args...)
}

func (c *countingClient) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	c.note("Exists")
	return c.RedisClient.Exists(ctx, keys...)
}

func (c *countingClient) Get(ctx context.Context, key string) *redis.StringCmd {
	c.note("Get")
	return c.RedisClient.Get(ctx, key)
}

func (c *countingClient) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	c.note("Set")
	return c.RedisClient.Set(ctx, key, value, expiration)
}

func (c *countingClient) SetArgs(ctx context.Context, key string, value interface{}, a redis.SetArgs) *redis.StatusCmd {
	c.note("SetArgs")
	return c.RedisClient.SetArgs(ctx, key, value, a)
}

func (c *countingClient) TTL(ctx context.Context, key string) *redis.DurationCmd {
	c.note("TTL")
	return c.RedisClient.TTL(ctx, key)
}

// TestAWrapperIsEnoughToInstrumentTheManager: the point of naming an interface
// rather than a concrete client is that a caller can put their own type in
// front of it -- metrics, tracing, a fake. This also pins that every command
// the manager issues is single-key, which is what makes it cluster-safe.
func TestAWrapperIsEnoughToInstrumentTheManager(t *testing.T) {
	mr, base := setupMiniRedis(t)
	defer mr.Close()

	client := &countingClient{RedisClient: base}
	runManagerRoundTrip(t, client)

	for _, want := range []string{"Set", "Get", "Del", "Exists", "TTL", "SetArgs", "Eval"} {
		if client.calls[want] == 0 {
			t.Errorf("the manager never issued %s; RedisClient declares a command it does not use", want)
		}
	}
	if len(client.calls) != 7 {
		t.Errorf("commands issued = %v, want exactly the seven RedisClient declares", client.calls)
	}
}

// TestEveryCommandIsSingleKey is what allows a cluster to place the challenge,
// user-lock, verify-lock and active-index keys in different slots: no command
// the manager issues ever names two keys.
func TestEveryCommandIsSingleKey(t *testing.T) {
	mr, base := setupMiniRedis(t)
	defer mr.Close()

	client := &keyCountingClient{RedisClient: base, t: t}
	runManagerRoundTrip(t, client)
}

type keyCountingClient struct {
	RedisClient
	t *testing.T
}

func (c *keyCountingClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	if len(keys) != 1 {
		c.t.Errorf("DEL issued with %d keys, want 1 (a cluster cannot span slots)", len(keys))
	}
	return c.RedisClient.Del(ctx, keys...)
}

func (c *keyCountingClient) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	if len(keys) != 1 {
		c.t.Errorf("EXISTS issued with %d keys, want 1 (a cluster cannot span slots)", len(keys))
	}
	return c.RedisClient.Exists(ctx, keys...)
}

func (c *keyCountingClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	if len(keys) != 1 {
		c.t.Errorf("EVAL issued with %d keys, want 1 (a cluster cannot span slots)", len(keys))
	}
	return c.RedisClient.Eval(ctx, script, keys, args...)
}
