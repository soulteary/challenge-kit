package challenge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	rediskitcache "github.com/soulteary/redis-kit/cache"
	secure "github.com/soulteary/secure-kit"
)

// ErrLockUnavailable is returned when the per-challenge verification lock cannot
// be acquired within the configured budget. It is a stable, retryable signal:
// callers MUST NOT treat it as a verification failure (which would consume an
// attempt) and MUST NOT fall back to a non-atomic path.
var ErrLockUnavailable = errors.New("challenge: verification lock unavailable, retry")

// ErrBackendUnavailable is returned when a required Redis operation fails. The
// manager fails closed: it never reports OK when the backing store is unhealthy.
var ErrBackendUnavailable = errors.New("challenge: backend unavailable")

// unlockScriptSrc releases a lock only if the caller still owns it (compare
// token then delete). This prevents a slow holder from deleting a lock
// re-acquired by another verifier after TTL expiry. It is executed via EVAL
// directly so it works against backends without a script cache (EVALSHA).
const unlockScriptSrc = `
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`

// swapActiveScriptSrc atomically sets the active-index key to a new challenge id
// (ARGV[1]) with TTL ARGV[2] ms and returns the previous value (or false/nil if
// none). Executed via EVAL directly for the same reason as unlockScriptSrc.
const swapActiveScriptSrc = `
local prev = redis.call("get", KEYS[1])
redis.call("set", KEYS[1], ARGV[1], "PX", ARGV[2])
return prev
`

// Manager handles challenge operations
type Manager struct {
	client       *redis.Client
	cache        rediskitcache.Cache
	lockCache    rediskitcache.Cache
	config       Config
	argon2Hasher *secure.Argon2Hasher
}

// NewManager creates a new challenge manager
func NewManager(redisClient *redis.Client, config Config) *Manager {
	if config.ChallengeKeyPrefix == "" {
		config.ChallengeKeyPrefix = "otp:ch:"
	}
	if config.LockKeyPrefix == "" {
		config.LockKeyPrefix = "otp:lock:"
	}
	if config.CodeLength == 0 {
		config.CodeLength = 6
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 5
	}
	if config.Expiry == 0 {
		config.Expiry = 5 * time.Minute
	}
	if config.LockoutDuration == 0 {
		config.LockoutDuration = 10 * time.Minute
	}
	if config.VerifyLockPrefix == "" {
		config.VerifyLockPrefix = "otp:vlock:"
	}
	if config.VerifyLockTTL == 0 {
		// Must comfortably exceed the worst-case Argon2 verification time so the
		// lock is not lost mid-verification under load.
		config.VerifyLockTTL = 5 * time.Second
	}
	if config.VerifyLockWait == 0 {
		config.VerifyLockWait = 2 * time.Second
	}
	if config.VerifyLockRetry == 0 {
		config.VerifyLockRetry = 25 * time.Millisecond
	}
	if config.ActiveIndexPrefix == "" {
		config.ActiveIndexPrefix = "otp:active:"
	}

	// Create cache instances with appropriate prefixes
	challengeCache := rediskitcache.NewCache(redisClient, config.ChallengeKeyPrefix)
	lockCache := rediskitcache.NewCache(redisClient, config.LockKeyPrefix)

	return &Manager{
		client:       redisClient,
		cache:        challengeCache,
		lockCache:    lockCache,
		config:       config,
		argon2Hasher: secure.NewArgon2Hasher(),
	}
}

// Create creates a new challenge and stores it in Redis
// Returns the challenge, the plaintext code (for sending), and any error
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Challenge, string, error) {
	// Generate challenge ID
	challengeID := m.generateChallengeID()

	// Generate verification code
	code, err := secure.RandomDigits(m.config.CodeLength)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate code: %w", err)
	}

	// Hash the code using Argon2
	codeHash, err := m.argon2Hasher.Hash(code)
	if err != nil {
		return nil, "", fmt.Errorf("failed to hash code: %w", err)
	}

	// Create challenge
	challenge := &Challenge{
		ID:          challengeID,
		UserID:      req.UserID,
		Channel:     req.Channel,
		Destination: req.Destination,
		CodeHash:    codeHash,
		Purpose:     req.Purpose,
		ExpiresAt:   time.Now().Add(m.config.Expiry),
		Attempts:    0,
		MaxAttempts: m.config.MaxAttempts,
		CreatedIP:   req.ClientIP,
		CreatedAt:   time.Now(),
	}

	// Store in Redis using cache interface
	if err := m.cache.Set(ctx, challengeID, challenge, m.config.Expiry); err != nil {
		return nil, "", fmt.Errorf("failed to store challenge: %w", err)
	}

	return challenge, code, nil
}

// VerifyOptions carries optional binding constraints checked atomically inside
// the verification lock before the code is consumed. Empty fields are not
// checked (v1 behaviour). A mismatch fails without consuming the challenge as a
// code error would; it returns reason "context_mismatch".
type VerifyOptions struct {
	// ExpectedUserID, when non-empty, must equal the challenge's UserID.
	ExpectedUserID string
	// ExpectedPurpose, when non-empty, must equal the challenge's Purpose.
	ExpectedPurpose string
	// ExpectedChannel, when non-empty, must equal the challenge's Channel.
	ExpectedChannel Channel
}

// Verify verifies a code against a challenge.
//
// Atomicity: the whole read-modify-write cycle (GET challenge -> Argon2 verify
// -> consume-on-success / increment-on-failure) runs while holding a
// per-challenge distributed lock. Argon2 cannot run inside a Lua script, so the
// lock (SET NX PX with a random token, released via a compare-and-delete Lua
// script) serializes concurrent verifications of the same challenge. This
// guarantees:
//   - at most one concurrent CORRECT code can consume the challenge (exactly-once);
//   - concurrent WRONG codes each increment attempts exactly once (no lost updates);
//   - the challenge is deleted before returning success, and the delete error is
//     surfaced (fail-closed): a failed delete never reports OK.
//
// On lock contention it returns ErrLockUnavailable (retryable, no attempt
// consumed). On any Redis failure it fails closed with ErrBackendUnavailable and
// never falls back to a non-atomic local path.
func (m *Manager) Verify(ctx context.Context, challengeID, code, clientIP string) (*VerifyResult, error) {
	return m.VerifyWithOptions(ctx, challengeID, code, clientIP, VerifyOptions{})
}

// VerifyWithOptions is Verify with atomic purpose/user/channel binding. The
// binding checks run inside the per-challenge lock, before code comparison, so
// a challenge minted for one purpose can never be redeemed for another.
func (m *Manager) VerifyWithOptions(ctx context.Context, challengeID, code, clientIP string, opts VerifyOptions) (*VerifyResult, error) {
	if challengeID == "" {
		return &VerifyResult{OK: false, Reason: "invalid"}, fmt.Errorf("empty challenge id")
	}

	token, err := m.acquireLock(ctx, challengeID)
	if err != nil {
		if errors.Is(err, ErrLockUnavailable) {
			return &VerifyResult{OK: false, Reason: "locked"}, err
		}
		// Redis failure acquiring the lock: fail closed.
		return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}
	defer m.releaseLock(context.WithoutCancel(ctx), challengeID, token)

	return m.verifyLocked(ctx, challengeID, code, opts)
}

// verifyLocked performs the verification assuming the per-challenge lock is held.
func (m *Manager) verifyLocked(ctx context.Context, challengeID, code string, opts VerifyOptions) (*VerifyResult, error) {
	// Get challenge from Redis using cache interface
	var challenge Challenge
	if err := m.cache.Get(ctx, challengeID, &challenge); err != nil {
		// Distinguish "not found" (expired/consumed) from a backend error so we
		// fail closed on infrastructure problems instead of reporting "expired".
		if isNotFound(err) {
			return &VerifyResult{OK: false, Reason: "expired"}, fmt.Errorf("challenge not found or expired: %w", err)
		}
		return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	// Purpose/user/channel binding: a challenge minted for one context must not
	// be redeemable for another. This check runs before code comparison and does
	// NOT consume an attempt, so it cannot be used as an oracle to burn attempts
	// on a legitimate challenge via wrong-context probing.
	if opts.ExpectedUserID != "" && opts.ExpectedUserID != challenge.UserID {
		return &VerifyResult{OK: false, Reason: "context_mismatch"}, fmt.Errorf("user id mismatch")
	}
	if opts.ExpectedPurpose != "" && opts.ExpectedPurpose != challenge.Purpose {
		return &VerifyResult{OK: false, Reason: "context_mismatch"}, fmt.Errorf("purpose mismatch")
	}
	if opts.ExpectedChannel != "" && opts.ExpectedChannel != challenge.Channel {
		return &VerifyResult{OK: false, Reason: "context_mismatch"}, fmt.Errorf("channel mismatch")
	}

	// Check if expired
	if time.Now().After(challenge.ExpiresAt) {
		if err := m.cache.Del(ctx, challengeID); err != nil {
			return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: delete expired: %v", ErrBackendUnavailable, err)
		}
		return &VerifyResult{OK: false, Reason: "expired"}, fmt.Errorf("challenge expired")
	}

	// Check if the challenge already reached max attempts (locked).
	if challenge.Attempts >= challenge.MaxAttempts {
		if err := m.lockUser(ctx, challenge.UserID); err != nil {
			return &VerifyResult{OK: false, Reason: "backend_unavailable"}, err
		}
		return &VerifyResult{OK: false, Reason: "locked"}, fmt.Errorf("challenge locked due to too many attempts")
	}

	// Check if user is locked (fail closed on Redis error).
	locked, err := m.lockCache.Exists(ctx, challenge.UserID)
	if err != nil {
		return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: user lock check: %v", ErrBackendUnavailable, err)
	}
	if locked {
		return &VerifyResult{OK: false, Reason: "user_locked"}, fmt.Errorf("user is temporarily locked")
	}

	// Verify code (constant-time Argon2 compare).
	if !m.verifyCode(code, challenge.CodeHash) {
		challenge.Attempts++
		ttl, ttlErr := m.cache.TTL(ctx, challengeID)
		if ttlErr != nil {
			return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: ttl: %v", ErrBackendUnavailable, ttlErr)
		}
		if ttl <= 0 {
			// Key has no TTL / already gone; treat as expired rather than
			// resurrecting it with a fresh lifetime.
			_ = m.cache.Del(ctx, challengeID)
			return &VerifyResult{OK: false, Reason: "expired"}, fmt.Errorf("challenge expired")
		}
		if err := m.cache.Set(ctx, challengeID, challenge, ttl); err != nil {
			return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: persist attempts: %v", ErrBackendUnavailable, err)
		}
		if challenge.Attempts >= challenge.MaxAttempts {
			if err := m.lockUser(ctx, challenge.UserID); err != nil {
				return &VerifyResult{OK: false, Reason: "backend_unavailable"}, err
			}
			remaining := 0
			return &VerifyResult{OK: false, Reason: "locked", RemainingAttempts: &remaining}, fmt.Errorf("challenge locked due to too many attempts")
		}
		remaining := challenge.MaxAttempts - challenge.Attempts
		return &VerifyResult{OK: false, Reason: "invalid", RemainingAttempts: &remaining}, fmt.Errorf("invalid code")
	}

	// Success: consume the challenge (one-time use). The delete MUST succeed
	// before we report OK, otherwise a second correct request could also
	// succeed. Fail closed if the delete fails.
	if err := m.cache.Del(ctx, challengeID); err != nil {
		return &VerifyResult{OK: false, Reason: "backend_unavailable"}, fmt.Errorf("%w: consume challenge: %v", ErrBackendUnavailable, err)
	}

	return &VerifyResult{OK: true, Challenge: &challenge}, nil
}

// lockUser marks a user as locked, surfacing Redis errors (fail closed).
func (m *Manager) lockUser(ctx context.Context, userID string) error {
	if err := m.lockCache.Set(ctx, userID, "1", m.config.LockoutDuration); err != nil {
		return fmt.Errorf("%w: lock user: %v", ErrBackendUnavailable, err)
	}
	return nil
}

// acquireLock acquires the per-challenge verification lock (SET NX PX). It polls
// until VerifyLockWait elapses, then returns ErrLockUnavailable. Any Redis error
// other than contention is returned as-is so callers can fail closed.
func (m *Manager) acquireLock(ctx context.Context, challengeID string) (string, error) {
	token, err := secure.RandomToken(16)
	if err != nil {
		token, err = secure.RandomHex(16)
		if err != nil {
			return "", err
		}
	}
	key := m.config.VerifyLockPrefix + challengeID
	deadline := time.Now().Add(m.config.VerifyLockWait)
	for {
		ok, err := m.client.SetNX(ctx, key, token, m.config.VerifyLockTTL).Result()
		if err != nil {
			return "", err
		}
		if ok {
			return token, nil
		}
		if time.Now().After(deadline) {
			return "", ErrLockUnavailable
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(m.config.VerifyLockRetry):
		}
	}
}

// releaseLock releases the lock only if we still own it. It uses EVAL directly
// (rather than Script.Run, which attempts EVALSHA first) so it works against
// backends that do not implement the script cache.
func (m *Manager) releaseLock(ctx context.Context, challengeID, token string) {
	key := m.config.VerifyLockPrefix + challengeID
	_ = m.client.Eval(ctx, unlockScriptSrc, []string{key}, token).Err()
}

// isNotFound reports whether err represents a missing key (as opposed to a
// backend failure). The redis-kit cache wraps redis.Nil into a "key not found"
// error string.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, redis.Nil) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "key not found") || strings.Contains(msg, "not found or expired")
}

// Revoke revokes a challenge
func (m *Manager) Revoke(ctx context.Context, challengeID string) error {
	return m.cache.Del(ctx, challengeID)
}

// IsUserLocked checks if a user is locked. It fails CLOSED: on a Redis error it
// reports the user as locked so a backend outage cannot be used to bypass the
// lockout.
func (m *Manager) IsUserLocked(ctx context.Context, userID string) bool {
	exists, err := m.lockCache.Exists(ctx, userID)
	if err != nil {
		return true
	}
	return exists
}

// Get retrieves a challenge by ID
func (m *Manager) Get(ctx context.Context, challengeID string) (*Challenge, error) {
	var challenge Challenge
	if err := m.cache.Get(ctx, challengeID, &challenge); err != nil {
		return nil, fmt.Errorf("challenge not found: %w", err)
	}

	return &challenge, nil
}

// Activate promotes a freshly created (pending) challenge to be the single
// active challenge for its identity (user_id + purpose + channel + destination).
// It is meant to be called AFTER the code was successfully handed to a provider.
//
// Two-phase model:
//  1. Create() stores the challenge (pending).
//  2. Provider send succeeds.
//  3. Activate() atomically swaps the active-index to this challenge and returns
//     the previously active challenge ID (if any) so the caller can revoke it.
//
// The active-index key is derived from an irreversible digest of the identity,
// SwapActive atomically sets the active-index for the challenge identity to
// ch.ID and returns the challenge ID that was previously active (empty if
// none). It fails closed on Redis errors.
//
// Two-phase model:
//  1. Create() stores the challenge (pending).
//  2. Provider send succeeds.
//  3. SwapActive() atomically swaps the active-index to this challenge and
//     returns the previously active challenge ID (if any) so the caller can
//     revoke it. On send failure, call RevokePending() and keep the old active.
//
// The active-index key is derived from an irreversible digest of the identity,
// never the raw PII.
func (m *Manager) SwapActive(ctx context.Context, ch *Challenge) (previousID string, err error) {
	if ch == nil {
		return "", fmt.Errorf("nil challenge")
	}
	key := m.activeIndexKey(ch)
	ttlMs := int64(m.config.Expiry / time.Millisecond)
	res, err := m.client.Eval(ctx, swapActiveScriptSrc, []string{key}, ch.ID, ttlMs).Result()
	if err != nil {
		// A missing previous value surfaces as redis.Nil, which is not an error
		// for us: it just means there was no prior active challenge.
		if errors.Is(err, redis.Nil) {
			return "", nil
		}
		return "", fmt.Errorf("%w: swap active index: %v", ErrBackendUnavailable, err)
	}
	if res == nil {
		return "", nil
	}
	prev, _ := res.(string)
	return prev, nil
}

// RevokePending removes a challenge that failed to send, so a failed send never
// leaves a redeemable code behind. It does NOT touch the active-index.
func (m *Manager) RevokePending(ctx context.Context, challengeID string) error {
	if err := m.cache.Del(ctx, challengeID); err != nil {
		return fmt.Errorf("%w: revoke pending: %v", ErrBackendUnavailable, err)
	}
	return nil
}

// activeIndexKey returns the Redis key for the single-active-challenge index of
// the challenge's identity. The identity is hashed (SHA-256) so no raw
// user_id/destination is ever written into a key.
func (m *Manager) activeIndexKey(ch *Challenge) string {
	prefix := m.config.ActiveIndexPrefix
	if prefix == "" {
		prefix = "otp:active:"
	}
	identity := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(ch.UserID)),
		strings.ToLower(strings.TrimSpace(ch.Purpose)),
		strings.ToLower(strings.TrimSpace(string(ch.Channel))),
		strings.ToLower(strings.TrimSpace(ch.Destination)),
	}, "\x1f")
	sum := sha256.Sum256([]byte(identity))
	return prefix + hex.EncodeToString(sum[:])
}

// Helper functions

func (m *Manager) generateChallengeID() string {
	token, err := secure.RandomToken(16)
	if err != nil {
		// This should never happen with crypto/rand, but handle gracefully
		token, _ = secure.RandomHex(16)
	}
	return "ch_" + token[:22]
}

func (m *Manager) verifyCode(code, hash string) bool {
	// secure.Argon2Hasher.Verify uses constant-time comparison internally
	return m.argon2Hasher.Verify(hash, code)
}
