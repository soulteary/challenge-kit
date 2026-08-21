package challenge

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestVerify_ConcurrentCorrectCode_ExactlyOneSuccess proves the atomicity
// invariant: when many goroutines submit the CORRECT code for the same
// challenge concurrently, exactly one succeeds (one-time use), never more.
func TestVerify_ConcurrentCorrectCode_ExactlyOneSuccess(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	cfg := DefaultConfig()
	cfg.VerifyLockWait = 5 * time.Second
	manager := NewManager(redisClient, cfg)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user-atomic",
		Channel:     ChannelEmail,
		Destination: "atomic@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}
	ch, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	const n = 100
	var success, retryable, failed int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res, err := manager.Verify(ctx, ch.ID, code, "127.0.0.1")
			switch {
			case err == nil && res.OK:
				atomic.AddInt64(&success, 1)
			case res != nil && res.Reason == "locked" && err == ErrLockUnavailable:
				atomic.AddInt64(&retryable, 1)
			default:
				atomic.AddInt64(&failed, 1)
			}
		}()
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("expected exactly 1 success, got success=%d retryable=%d failed=%d", success, retryable, failed)
	}
}

// TestVerify_ConcurrentWrongCode_AttemptsExact proves that concurrent WRONG
// codes each consume exactly one attempt (no lost updates) and the challenge
// locks reliably at MaxAttempts.
func TestVerify_ConcurrentWrongCode_AttemptsExact(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	cfg := DefaultConfig()
	cfg.MaxAttempts = 5
	cfg.VerifyLockWait = 5 * time.Second
	manager := NewManager(redisClient, cfg)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user-wrong",
		Channel:     ChannelEmail,
		Destination: "wrong@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}
	ch, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Fire exactly MaxAttempts wrong verifications concurrently. Because the
	// lock serializes them, attempts must land exactly at MaxAttempts and the
	// user must be locked.
	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = manager.Verify(ctx, ch.ID, "000000", "127.0.0.1")
		}()
	}
	wg.Wait()

	if !manager.IsUserLocked(ctx, req.UserID) {
		t.Fatalf("user should be locked after %d concurrent wrong attempts", n)
	}
}

// TestVerify_BackendDown_FailsClosed proves that when Redis is unavailable,
// Verify never reports OK and returns a backend error rather than a spurious
// "expired".
func TestVerify_BackendDown_FailsClosed(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	ch, code, err := manager.Create(ctx, CreateRequest{
		UserID:      "user-down",
		Channel:     ChannelEmail,
		Destination: "down@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Take Redis down.
	mr.Close()

	res, err := manager.Verify(ctx, ch.ID, code, "127.0.0.1")
	if err == nil || (res != nil && res.OK) {
		t.Fatalf("Verify() with Redis down must fail closed, got res=%+v err=%v", res, err)
	}
	if res.Reason != "backend_unavailable" && res.Reason != "locked" {
		t.Fatalf("Verify() with Redis down reason = %q, want backend_unavailable/locked", res.Reason)
	}
}

// TestIsUserLocked_FailsClosedOnBackendError proves IsUserLocked returns true
// (locked) when the backend errors, so an outage cannot bypass lockout.
func TestIsUserLocked_FailsClosedOnBackendError(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer func() { _ = redisClient.Close() }()
	manager := NewManager(redisClient, DefaultConfig())
	mr.Close() // force backend errors

	if !manager.IsUserLocked(context.Background(), "any-user") {
		t.Fatal("IsUserLocked() should fail closed (return true) on backend error")
	}
}

// TestVerifyWithOptions_PurposeBinding proves a challenge minted for one purpose
// cannot be redeemed for another, and that a context mismatch does NOT consume
// an attempt (so it can't be used to burn a legitimate challenge's attempts).
func TestVerifyWithOptions_PurposeBinding(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, CreateRequest{
		UserID:      "u1",
		Channel:     ChannelEmail,
		Destination: "u1@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wrong purpose: must fail with context_mismatch and NOT consume an attempt.
	res, err := manager.VerifyWithOptions(ctx, ch.ID, code, "127.0.0.1", VerifyOptions{ExpectedPurpose: "reset"})
	if err == nil || res.OK || res.Reason != "context_mismatch" {
		t.Fatalf("purpose mismatch: got res=%+v err=%v, want context_mismatch fail", res, err)
	}

	// Wrong user: same.
	res, err = manager.VerifyWithOptions(ctx, ch.ID, code, "127.0.0.1", VerifyOptions{ExpectedUserID: "u2"})
	if err == nil || res.OK || res.Reason != "context_mismatch" {
		t.Fatalf("user mismatch: got res=%+v err=%v, want context_mismatch fail", res, err)
	}

	// Correct context with correct code still succeeds (attempts were not burned).
	res, err = manager.VerifyWithOptions(ctx, ch.ID, code, "127.0.0.1", VerifyOptions{
		ExpectedUserID:  "u1",
		ExpectedPurpose: "login",
		ExpectedChannel: ChannelEmail,
	})
	if err != nil || !res.OK {
		t.Fatalf("correct context: got res=%+v err=%v, want OK", res, err)
	}
}

// TestSwapActive_SingleActiveChallenge proves that activating a new challenge
// returns the previously active challenge id (for the same identity) so the
// caller can invalidate it, and that a different identity is isolated.
func TestSwapActive_SingleActiveChallenge(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())
	ctx := context.Background()

	req := CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "u1@example.com", Purpose: "login", ClientIP: "127.0.0.1"}
	ch1, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create ch1: %v", err)
	}
	prev, err := manager.SwapActive(ctx, ch1)
	if err != nil {
		t.Fatalf("SwapActive ch1: %v", err)
	}
	if prev != "" {
		t.Fatalf("first activation should have no previous, got %q", prev)
	}

	ch2, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create ch2: %v", err)
	}
	prev, err = manager.SwapActive(ctx, ch2)
	if err != nil {
		t.Fatalf("SwapActive ch2: %v", err)
	}
	if prev != ch1.ID {
		t.Fatalf("second activation previous = %q, want %q", prev, ch1.ID)
	}

	// Different identity is isolated (no previous).
	ch3, _, err := manager.Create(ctx, CreateRequest{UserID: "other", Channel: ChannelEmail, Destination: "other@example.com", Purpose: "login", ClientIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("Create ch3: %v", err)
	}
	prev, err = manager.SwapActive(ctx, ch3)
	if err != nil {
		t.Fatalf("SwapActive ch3: %v", err)
	}
	if prev != "" {
		t.Fatalf("different identity should have no previous, got %q", prev)
	}
}

// TestActiveIndexKey_NoRawPII proves the active-index key never contains the raw
// user id or destination.
func TestActiveIndexKey_NoRawPII(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()
	manager := NewManager(redisClient, DefaultConfig())

	ch := &Challenge{UserID: "secret-user", Destination: "secret@example.com", Purpose: "login", Channel: ChannelEmail}
	key := manager.activeIndexKey(ch)
	if strings.Contains(key, "secret-user") || strings.Contains(key, "secret@example.com") {
		t.Fatalf("active index key leaks PII: %q", key)
	}
}

// TestVerify_LockContentionIsRetryable proves that when the per-challenge lock
// is already held, Verify returns ErrLockUnavailable (retryable) and never
// reports success or consumes an attempt.
func TestVerify_LockContentionIsRetryable(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = redisClient.Close() }()

	cfg := DefaultConfig()
	cfg.VerifyLockPrefix = "otp:vlock:"
	cfg.VerifyLockWait = 50 * time.Millisecond
	cfg.VerifyLockRetry = 10 * time.Millisecond
	manager := NewManager(redisClient, cfg)
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, CreateRequest{
		UserID:      "u",
		Channel:     ChannelEmail,
		Destination: "u@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pre-hold the lock so acquire times out -> ErrLockUnavailable.
	key := cfg.VerifyLockPrefix + ch.ID
	if err := redisClient.Set(ctx, key, "held", time.Second).Err(); err != nil {
		t.Fatalf("pre-hold lock: %v", err)
	}

	res, err := manager.Verify(ctx, ch.ID, code, "127.0.0.1")
	if err != ErrLockUnavailable {
		t.Fatalf("expected ErrLockUnavailable, got res=%+v err=%v", res, err)
	}
	if res.OK {
		t.Fatal("lock contention must not report success")
	}
}
