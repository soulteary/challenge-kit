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

// --- Codex review follow-up (PR #3) ---

// TestVerifySlotIsTakenBeforeTheChallengeLock is the regression test for the
// ordering of the two budgets. The Argon2 slot used to be claimed inside
// verifyCode, i.e. while the per-challenge Redis lock was already held: with
// every slot occupied for longer than VerifyLockTTL, the lease expired
// underneath the waiter, a second request took a fresh lock on the same
// challenge, read the same snapshot and queued behind the same slots, and once
// slots opened both verified identical state.
//
// With the slot claimed first, a request that cannot get one never takes the
// lock at all -- so no lease is sitting there expiring.
func TestVerifySlotIsTakenBeforeTheChallengeLock(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxConcurrentVerifications = 1
	manager := NewManager(redisClient, cfg)

	ch, _, err := manager.Create(context.Background(), CreateRequest{
		UserID: "u1", Channel: ChannelSMS, Destination: "13800000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	lockKey := manager.config.VerifyLockPrefix + ch.ID

	// Occupy the only verification slot.
	manager.verifySlots <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.Verify(ctx, ch.ID, "000000", "")
	}()

	// Give the goroutine time to get as far as it can. It must be parked on
	// the slot, not on Redis.
	time.Sleep(100 * time.Millisecond)

	exists, err := redisClient.Exists(context.Background(), lockKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Error("the per-challenge lock was taken while waiting for an Argon2 slot; its lease expires with nobody making progress")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Verify did not return after its context was cancelled")
	}

	// Release the slot; a normal verification still works afterwards.
	<-manager.verifySlots

	res, err := manager.Verify(context.Background(), ch.ID, "000000", "")
	if err == nil {
		t.Fatal("Verify with a wrong code returned nil error")
	}
	if res.Reason != ReasonInvalid {
		t.Errorf("Reason = %q, want %q once a slot is free", res.Reason, ReasonInvalid)
	}
}

// --- Codex review round 2 (PR #3) ---

// TestLockContentionDoesNotPinArgon2Slots is the regression test for holding a
// verification slot while POLLING for the per-challenge lock. A burst against
// one challenge parked every slot on VerifyLockWait of polling, so unrelated
// challenges were blocked with roughly one Argon2 comparison actually running.
func TestLockContentionDoesNotPinArgon2Slots(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxConcurrentVerifications = 1
	cfg.VerifyLockWait = 2 * time.Second
	cfg.VerifyLockRetry = 20 * time.Millisecond
	manager := NewManager(redisClient, cfg)

	ctx := context.Background()

	// Two challenges; the first one's lock is held by somebody else.
	hot, _, err := manager.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelSMS, Destination: "13800000000"})
	if err != nil {
		t.Fatal(err)
	}
	cold, _, err := manager.Create(ctx, CreateRequest{UserID: "u2", Channel: ChannelSMS, Destination: "13800000001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := redisClient.Set(ctx, manager.config.VerifyLockPrefix+hot.ID, "someone-else", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	// Pile onto the contended challenge.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = manager.Verify(ctx, hot.ID, "000000", "")
		}()
	}

	// The unrelated challenge must still get a slot promptly, rather than
	// waiting out the contended challenge's VerifyLockWait.
	time.Sleep(100 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.Verify(ctx, cold.ID, "000000", "")
	}()

	select {
	case <-done:
	case <-time.After(cfg.VerifyLockWait):
		t.Error("an unrelated challenge could not get a verification slot; lock contention is pinning process-wide Argon2 capacity")
	}

	wg.Wait()
}

// TestSlotWaitPreservesTheContextError: converting the context error with %v
// dropped it from the chain, so errors.Is(err, context.DeadlineExceeded) was
// false and callers following the ErrLockUnavailable contract could retry
// work the caller had already abandoned.
func TestSlotWaitPreservesTheContextError(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxConcurrentVerifications = 1
	manager := NewManager(redisClient, cfg)

	ch, _, err := manager.Create(context.Background(), CreateRequest{
		UserID: "u1", Channel: ChannelSMS, Destination: "13800000000",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Occupy the only slot, then call with an already-cancelled context.
	manager.verifySlots <- struct{}{}
	defer func() { <-manager.verifySlots }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = manager.Verify(ctx, ch.ID, "000000", "")
	if err == nil {
		t.Fatal("Verify with a cancelled context returned nil error")
	}
	if !errors.Is(err, ErrLockUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrLockUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled) to hold", err)
	}
}

// TestExhaustedChallengeCannotRecreateAnExpiredLockout is the regression test
// for establishing the lockout from the lock cache's own state. "Is the user
// locked right now?" only stopped a lockout still in effect from being
// refreshed; once it elapsed the answer was no again, so anyone holding an
// exhausted challenge ID could poll it to mint a fresh full-duration lockout,
// over and over, for as long as the challenge stayed valid.
func TestExhaustedChallengeCannotRecreateAnExpiredLockout(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	cfg := DefaultConfig()
	cfg.MaxAttempts = 1
	cfg.Expiry = time.Hour            // the challenge outlives the lockout
	cfg.LockoutDuration = time.Minute // ...which is short
	manager := NewManager(redisClient, cfg)

	ctx := context.Background()
	ch, _, err := manager.Create(ctx, CreateRequest{
		UserID: "u1", Channel: ChannelSMS, Destination: "13800000000",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Burn the only attempt: this locks the user.
	if _, err := manager.Verify(ctx, ch.ID, "000000", ""); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	if !manager.IsUserLocked(ctx, "u1") {
		t.Fatal("IsUserLocked after exhaustion = false, want true")
	}

	// The lockout elapses. The challenge is still valid.
	mr.FastForward(2 * time.Minute)
	if manager.IsUserLocked(ctx, "u1") {
		t.Fatal("IsUserLocked after the lockout elapsed = true, want false")
	}

	// Poll the exhausted challenge again. It must not mint a new lockout.
	if _, err := manager.Verify(ctx, ch.ID, "000000", ""); err == nil {
		t.Fatal("an exhausted challenge accepted a code")
	}
	if manager.IsUserLocked(ctx, "u1") {
		t.Error("IsUserLocked after re-probing the exhausted challenge = true; the lockout was recreated")
	}
}

// cancelAfterExistsCache cancels the request context once the user-lock check
// has answered, putting the flow into verifyCode with a dead context -- the
// one window in which that error path is reachable.
type cancelAfterExistsCache struct {
	rediskitcache.Cache
	cancel context.CancelFunc
}

func (c *cancelAfterExistsCache) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := c.Cache.Exists(ctx, key)
	if c.cancel != nil {
		c.cancel()
	}
	return exists, err
}

// TestVerifyPreservesCancellationAfterTheLock is the regression test for the
// remaining "%w: %v" on the verifyCode error path. That path carries
// context.Canceled or DeadlineExceeded, and folding it in with %v left callers
// holding only the retryable ErrLockUnavailable: errors.Is(err,
// context.Canceled) was false, so work the caller had already abandoned looked
// worth retrying.
func TestVerifyPreservesCancellationAfterTheLock(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()

	manager := NewManager(redisClient, DefaultConfig())

	ch, _, err := manager.Create(context.Background(), CreateRequest{
		UserID: "u1", Channel: ChannelSMS, Destination: "13800000000",
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.lockCache = &cancelAfterExistsCache{Cache: manager.lockCache, cancel: cancel}

	_, err = manager.Verify(ctx, ch.ID, "000000", "")
	if err == nil {
		t.Fatal("Verify returned nil error after its context was cancelled")
	}
	if !errors.Is(err, ErrLockUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrLockUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled) to hold", err)
	}
}
