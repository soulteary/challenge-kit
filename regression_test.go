package challenge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	rediskitcache "github.com/soulteary/redis-kit/cache"
)

// TestReasonsDistinguishRetryableFromTerminal is the regression test for the
// overloaded "locked" reason: lock contention is retryable and consumes no
// attempt, attempt exhaustion is terminal. Sharing one string left callers
// that switch on Reason no way to tell them apart.
func TestReasonsDistinguishRetryableFromTerminal(t *testing.T) {
	if ReasonLockContention == ReasonLocked {
		t.Fatal("lock contention and attempt exhaustion share a reason string")
	}

	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	manager := NewManager(redisClient, DefaultConfig())
	ctx := context.Background()

	ch, _, err := manager.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelSMS, Destination: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the verification lock, then verify: the caller must be told to
	// retry, not that the account is locked.
	lockKey := manager.config.VerifyLockPrefix + ch.ID
	if err := redisClient.Set(ctx, lockKey, "someone-else", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.VerifyLockWait = 50 * time.Millisecond
	fast := NewManager(redisClient, cfg)

	result, err := fast.Verify(ctx, ch.ID, "000000", "1.2.3.4")
	if !errors.Is(err, ErrLockUnavailable) {
		t.Fatalf("Verify() under contention error = %v, want ErrLockUnavailable", err)
	}
	if result.Reason != ReasonLockContention {
		t.Errorf("Reason = %q, want %q; a caller switching on Reason would treat this as an account lockout",
			result.Reason, ReasonLockContention)
	}
}

// TestGenerateChallengeIDReportsEntropyFailure: the fallback discarded its
// error and then sliced token[:22] unconditionally, panicking on an empty
// token -- in a function documented as handling the case gracefully.
func TestGenerateChallengeIDReportsEntropyFailure(t *testing.T) {
	_, redisClient := setupMiniRedis(t)
	manager := NewManager(redisClient, DefaultConfig())

	id, err := manager.generateChallengeID()
	if err != nil {
		t.Fatalf("generateChallengeID() error = %v", err)
	}
	if !strings.HasPrefix(id, "ch_") || len(id) != 25 {
		t.Errorf("generateChallengeID() = %q, want a 25-character ch_-prefixed id", id)
	}

	// Distinct across calls.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		next, err := manager.generateChallengeID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[next] {
			t.Fatalf("duplicate challenge id %q", next)
		}
		seen[next] = true
	}
}

// TestLockoutIsNotExtendedByProbing: polling an exhausted challenge used to
// push the user's lockout deadline out by another LockoutDuration each time,
// so anyone holding the challenge ID could keep a user locked out forever.
func TestLockoutIsNotExtendedByProbing(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxAttempts = 1
	cfg.LockoutDuration = 10 * time.Second
	manager := NewManager(redisClient, cfg)
	ctx := context.Background()

	ch, _, err := manager.Create(ctx, CreateRequest{UserID: "victim", Channel: ChannelSMS, Destination: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}

	// Burn the single attempt; this is what establishes the lockout.
	_, _ = manager.Verify(ctx, ch.ID, "000000", "1.2.3.4")

	lockCache := rediskitcache.NewCache(redisClient, cfg.LockKeyPrefix)
	first, err := lockCache.TTL(ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}

	mr.FastForward(3 * time.Second)

	// Probe the exhausted challenge a few times.
	for i := 0; i < 3; i++ {
		_, _ = manager.Verify(ctx, ch.ID, "000000", "1.2.3.4")
	}

	after, err := lockCache.TTL(ctx, "victim")
	if err != nil {
		t.Fatal(err)
	}
	if after >= first {
		t.Errorf("lockout TTL went from %s to %s after probing; the deadline is being extended", first, after)
	}
}

// TestVerificationConcurrencyIsBounded: every in-flight Argon2 verification
// holds its memory cost (64 MiB by default), so an unbounded number of them is
// a memory-exhaustion vector for anyone able to call Verify with distinct ids.
func TestVerificationConcurrencyIsBounded(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxConcurrentVerifications = 2
	manager := NewManager(redisClient, cfg)

	if cap(manager.verifySlots) != 2 {
		t.Fatalf("verifySlots capacity = %d, want 2", cap(manager.verifySlots))
	}

	var mu sync.Mutex
	var concurrent, peak int

	// Fill the slots by hand and confirm the limit is what bounds entry.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.verifySlots <- struct{}{}
			mu.Lock()
			concurrent++
			if concurrent > peak {
				peak = concurrent
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			concurrent--
			mu.Unlock()
			<-manager.verifySlots
		}()
	}
	wg.Wait()

	if peak > 2 {
		t.Errorf("peak concurrent verifications = %d, want at most 2", peak)
	}

	// The default is applied when the field is left unset.
	def := NewManager(redisClient, Config{})
	if cap(def.verifySlots) != DefaultMaxConcurrentVerifications {
		t.Errorf("default verifySlots capacity = %d, want %d", cap(def.verifySlots), DefaultMaxConcurrentVerifications)
	}
}

// TestDefaultConfigMatchesManagerDefaults: reading a field off DefaultConfig()
// should give the value a Manager would actually use.
func TestDefaultConfigMatchesManagerDefaults(t *testing.T) {
	_, redisClient := setupMiniRedis(t)
	cfg := DefaultConfig()
	m := NewManager(redisClient, cfg)

	if m.config.VerifyLockTTL != cfg.VerifyLockTTL {
		t.Errorf("VerifyLockTTL: DefaultConfig has %s, Manager uses %s", cfg.VerifyLockTTL, m.config.VerifyLockTTL)
	}
	if m.config.ActiveIndexPrefix != cfg.ActiveIndexPrefix {
		t.Errorf("ActiveIndexPrefix: DefaultConfig has %q, Manager uses %q", cfg.ActiveIndexPrefix, m.config.ActiveIndexPrefix)
	}
	if m.config.MaxConcurrentVerifications != cfg.MaxConcurrentVerifications {
		t.Errorf("MaxConcurrentVerifications: DefaultConfig has %d, Manager uses %d",
			cfg.MaxConcurrentVerifications, m.config.MaxConcurrentVerifications)
	}
}
