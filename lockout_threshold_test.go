package challenge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const lockoutTestUser = "user@example.com"

func lockoutTestManager(t *testing.T, threshold int, identity func(string) string) (*Manager, *miniredis.Miniredis) {
	t.Helper()
	mr, client := setupMiniRedis(t)
	t.Cleanup(func() {
		_ = client.Close()
		mr.Close()
	})
	cfg := DefaultConfig()
	cfg.MaxAttempts = 3
	cfg.LockoutThreshold = threshold
	cfg.LockIdentity = identity
	return NewManager(client, cfg), mr
}

// exhaustChallenge creates a challenge for userID and spends all its attempts
// on wrong codes. It returns the reason of the last attempt.
func exhaustChallenge(t *testing.T, m *Manager, userID string) (id, lastReason string) {
	t.Helper()
	ctx := context.Background()
	ch, code, err := m.Create(ctx, CreateRequest{UserID: userID, Channel: ChannelEmail, Destination: "user@example.com", Purpose: "login"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < m.config.MaxAttempts; i++ {
		result, _ := m.Verify(ctx, ch.ID, wrong, "")
		lastReason = result.Reason
	}
	return ch.ID, lastReason
}

func mustVerifyOK(t *testing.T, m *Manager, userID string) {
	t.Helper()
	ctx := context.Background()
	ch, code, err := m.Create(ctx, CreateRequest{UserID: userID, Channel: ChannelEmail, Destination: "user@example.com", Purpose: "login"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	result, err := m.Verify(ctx, ch.ID, code, "")
	if err != nil || !result.OK {
		t.Fatalf("Verify() = %+v, %v; want success", result, err)
	}
}

// The default and a threshold of one keep the behaviour of earlier releases:
// the first exhausted challenge locks the user.
func TestLockoutThresholdOneLocksOnFirstExhaustedChallenge(t *testing.T) {
	for _, threshold := range []int{0, 1} {
		m, mr := lockoutTestManager(t, threshold, nil)
		if _, reason := exhaustChallenge(t, m, lockoutTestUser); reason != ReasonLocked {
			t.Fatalf("threshold %d: last reason = %q, want %q", threshold, reason, ReasonLocked)
		}
		if !m.IsUserLocked(context.Background(), lockoutTestUser) {
			t.Fatalf("threshold %d: user not locked after one exhausted challenge", threshold)
		}
		for _, key := range mr.Keys() {
			if strings.HasPrefix(key, "otp:lockcount:") {
				t.Fatalf("threshold %d: unexpected counter key %q", threshold, key)
			}
		}
	}
}

func TestLockoutThresholdLocksAfterThatManyExhaustedChallenges(t *testing.T) {
	m, mr := lockoutTestManager(t, 3, nil)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		if _, reason := exhaustChallenge(t, m, lockoutTestUser); reason != ReasonLocked {
			t.Fatalf("challenge %d: last reason = %q, want %q", i, reason, ReasonLocked)
		}
		if m.IsUserLocked(ctx, lockoutTestUser) {
			t.Fatalf("user locked after %d exhausted challenges, threshold is 3", i)
		}
		// Another challenge for the same user still verifies.
		mustVerifyOK(t, m, lockoutTestUser)
	}

	exhaustChallenge(t, m, lockoutTestUser)
	if !m.IsUserLocked(ctx, lockoutTestUser) {
		t.Fatal("user not locked after 3 exhausted challenges")
	}
	if ttl := mr.TTL("otp:lock:" + lockoutTestUser); ttl != m.config.LockoutDuration {
		t.Fatalf("lock TTL = %v, want %v", ttl, m.config.LockoutDuration)
	}
	if mr.Exists("otp:lockcount:" + lockoutTestUser) {
		t.Fatal("counter kept after the lock was set")
	}

	// While locked, a correct code is refused.
	ch, code, err := m.Create(ctx, CreateRequest{UserID: lockoutTestUser, Channel: ChannelEmail, Destination: "user@example.com", Purpose: "login"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result, _ := m.Verify(ctx, ch.ID, code, ""); result.OK || result.Reason != ReasonUserLocked {
		t.Fatalf("Verify() while locked = %+v, want %q", result, ReasonUserLocked)
	}
	// Another user is unaffected.
	mustVerifyOK(t, m, "other@example.com")

	// The lock ends after LockoutDuration and the count starts again.
	mr.FastForward(m.config.LockoutDuration + time.Second)
	if m.IsUserLocked(ctx, lockoutTestUser) {
		t.Fatal("lock did not expire")
	}
	exhaustChallenge(t, m, lockoutTestUser)
	if m.IsUserLocked(ctx, lockoutTestUser) {
		t.Fatal("one exhausted challenge after the lock ended locked the user again")
	}
}

// The window runs from the first exhausted challenge and is not extended.
func TestLockoutThresholdWindowIsFixed(t *testing.T) {
	m, mr := lockoutTestManager(t, 3, nil)
	counter := "otp:lockcount:" + lockoutTestUser

	exhaustChallenge(t, m, lockoutTestUser)
	if ttl := mr.TTL(counter); ttl != m.config.LockoutDuration {
		t.Fatalf("counter TTL = %v, want %v", ttl, m.config.LockoutDuration)
	}
	mr.FastForward(4 * time.Minute)
	exhaustChallenge(t, m, lockoutTestUser)
	if ttl := mr.TTL(counter); ttl != m.config.LockoutDuration-4*time.Minute {
		t.Fatalf("counter TTL after second challenge = %v, want %v", ttl, m.config.LockoutDuration-4*time.Minute)
	}

	// Both leave the window before a third arrives.
	mr.FastForward(m.config.LockoutDuration)
	exhaustChallenge(t, m, lockoutTestUser)
	if m.IsUserLocked(context.Background(), lockoutTestUser) {
		t.Fatal("exhausted challenges outside the window were counted")
	}
}

// Probing an exhausted challenge again does not add to the count.
func TestLockoutThresholdCountsEachChallengeOnce(t *testing.T) {
	m, mr := lockoutTestManager(t, 2, nil)
	ctx := context.Background()

	id, _ := exhaustChallenge(t, m, lockoutTestUser)
	for i := 0; i < 5; i++ {
		if result, _ := m.Verify(ctx, id, "000000", ""); result.Reason != ReasonLocked {
			t.Fatalf("probe %d: reason = %q, want %q", i, result.Reason, ReasonLocked)
		}
	}
	if m.IsUserLocked(ctx, lockoutTestUser) {
		t.Fatal("re-probing one exhausted challenge locked the user")
	}
	members, err := mr.SMembers("otp:lockcount:" + lockoutTestUser)
	if err != nil || len(members) != 1 || members[0] != id {
		t.Fatalf("counter members = %v, %v; want [%s]", members, err, id)
	}
}

// A successful verification does not clear the count: whoever controls one
// channel of an account could otherwise reset it while guessing another.
func TestLockoutThresholdSuccessDoesNotResetCount(t *testing.T) {
	m, _ := lockoutTestManager(t, 2, nil)
	exhaustChallenge(t, m, lockoutTestUser)
	mustVerifyOK(t, m, lockoutTestUser)
	exhaustChallenge(t, m, lockoutTestUser)
	if !m.IsUserLocked(context.Background(), lockoutTestUser) {
		t.Fatal("a successful verification reset the exhausted-challenge count")
	}
}

func TestLockIdentityKeepsUserIDOutOfLockKeys(t *testing.T) {
	digest := func(userID string) string {
		sum := sha256.Sum256([]byte("pepper:" + userID))
		return hex.EncodeToString(sum[:])
	}
	for _, threshold := range []int{1, 2} {
		m, mr := lockoutTestManager(t, threshold, digest)
		ctx := context.Background()
		for i := 0; i < threshold; i++ {
			if i > 0 && m.IsUserLocked(ctx, lockoutTestUser) {
				t.Fatalf("threshold %d: locked early", threshold)
			}
			exhaustChallenge(t, m, lockoutTestUser)
			for _, key := range mr.Keys() {
				if strings.HasPrefix(key, "otp:lock") && strings.Contains(key, lockoutTestUser) {
					t.Fatalf("threshold %d: key %q contains the user ID", threshold, key)
				}
			}
		}
		if !mr.Exists("otp:lock:" + digest(lockoutTestUser)) {
			t.Fatalf("threshold %d: lock not stored under the derived identity; keys %v", threshold, mr.Keys())
		}
		if !m.IsUserLocked(ctx, lockoutTestUser) {
			t.Fatalf("threshold %d: IsUserLocked does not use the derived identity", threshold)
		}
		if m.IsUserLocked(ctx, "other@example.com") {
			t.Fatalf("threshold %d: another user reported locked", threshold)
		}
	}
}

func TestLockIdentityEmptyResultFallsBackToUserID(t *testing.T) {
	m, mr := lockoutTestManager(t, 1, func(string) string { return "" })
	exhaustChallenge(t, m, lockoutTestUser)
	if !mr.Exists("otp:lock:" + lockoutTestUser) {
		t.Fatalf("lock not stored under the user ID; keys %v", mr.Keys())
	}
}

func TestIsUserLockedEReportsBackendError(t *testing.T) {
	m, mr := lockoutTestManager(t, 1, nil)
	ctx := context.Background()

	if locked, err := m.IsUserLockedE(ctx, lockoutTestUser); locked || err != nil {
		t.Fatalf("IsUserLockedE() = %v, %v; want false, nil", locked, err)
	}
	exhaustChallenge(t, m, lockoutTestUser)
	if locked, err := m.IsUserLockedE(ctx, lockoutTestUser); !locked || err != nil {
		t.Fatalf("IsUserLockedE() = %v, %v; want true, nil", locked, err)
	}

	mr.Close()
	locked, err := m.IsUserLockedE(ctx, lockoutTestUser)
	if !locked || !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("IsUserLockedE() on outage = %v, %v; want true and ErrBackendUnavailable", locked, err)
	}
	if !m.IsUserLocked(ctx, lockoutTestUser) {
		t.Fatal("IsUserLocked() must still fail closed")
	}

	var checker UserLockChecker = m
	_ = checker
}

// An exhaustion that cannot be recorded is an error, not "not locked".
func TestLockoutThresholdFailsClosedWhenCountCannotBeRecorded(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	cfg := DefaultConfig()
	cfg.MaxAttempts = 1
	cfg.LockoutThreshold = 2
	failing := &evalFailingClient{RedisClient: client}
	m := NewManager(failing, cfg)
	ctx := context.Background()

	ch, code, err := m.Create(ctx, CreateRequest{UserID: lockoutTestUser, Channel: ChannelEmail, Destination: "user@example.com", Purpose: "login"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	failing.fail = true
	result, err := m.Verify(ctx, ch.ID, wrong, "")
	if !errors.Is(err, ErrBackendUnavailable) || result.Reason != ReasonBackendUnavailable {
		t.Fatalf("Verify() = %+v, %v; want backend unavailable", result, err)
	}
	// The retry records the exhaustion; the correct code is refused throughout.
	failing.fail = false
	if result, _ := m.Verify(ctx, ch.ID, code, ""); result.OK || result.Reason != ReasonLocked {
		t.Fatalf("Verify() after recovery = %+v, want %q", result, ReasonLocked)
	}
	if members, _ := mr.SMembers("otp:lockcount:" + lockoutTestUser); len(members) != 1 {
		t.Fatalf("counter members = %v, want one", members)
	}
}

// Parallel exhaustion of several challenges ends in a lock and never counts
// one challenge twice.
func TestLockoutThresholdConcurrentExhaustion(t *testing.T) {
	m, _ := lockoutTestManager(t, 3, nil)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exhaustChallenge(t, m, lockoutTestUser)
		}()
	}
	wg.Wait()
	if !m.IsUserLocked(context.Background(), lockoutTestUser) {
		t.Fatal("three exhausted challenges did not lock the user")
	}
}

// evalFailingClient fails the counter script on demand. Challenge mutations
// also use Eval, so only scripts that touch the counter prefix are failed.
type evalFailingClient struct {
	RedisClient
	fail bool
}

func (c *evalFailingClient) Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	if c.fail && len(keys) == 1 && strings.HasPrefix(keys[0], "otp:lockcount:") {
		cmd := redis.NewCmd(ctx)
		cmd.SetErr(errors.New("injected counter failure"))
		return cmd
	}
	return c.RedisClient.Eval(ctx, script, keys, args...)
}
