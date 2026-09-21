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
	secure "github.com/soulteary/secure-kit/v2"
	"github.com/soulteary/secure-kit/v2/passwd"
)

// Verification outcome reasons. These are part of the API: callers switch on
// them to decide what to tell the user.
const (
	// ReasonInvalid means the code did not match.
	ReasonInvalid = "invalid"
	// ReasonExpired means the challenge is gone or past its lifetime.
	ReasonExpired = "expired"
	// ReasonLocked means the challenge exhausted its attempts and the user is
	// now locked out. This is terminal: a new challenge is required.
	ReasonLocked = "locked"
	// ReasonUserLocked means the user was already locked out.
	ReasonUserLocked = "user_locked"
	// ReasonContextMismatch means the challenge was minted for a different
	// user, purpose or channel.
	ReasonContextMismatch = "context_mismatch"
	// ReasonBackendUnavailable means Redis could not be reached; the result is
	// unknown rather than negative.
	ReasonBackendUnavailable = "backend_unavailable"
	// ReasonLockContention means another verification of the same challenge is
	// in progress. It is RETRYABLE and consumes no attempt.
	//
	// This used to share the "locked" reason with attempt exhaustion, which is
	// terminal and the opposite of retryable, leaving callers that switch on
	// Reason no way to tell them apart -- while the documentation told them
	// they must.
	ReasonLockContention = "lock_contention"
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
	client       RedisClient
	cache        store
	lockCache    store
	config       Config
	argon2Hasher *passwd.Argon2Hasher

	// verifySlots bounds how many Argon2 verifications run at once.
	//
	// The per-challenge lock serialises verifications of the SAME challenge,
	// but nothing bounded them across different ones, and every in-flight
	// verification holds the Argon2 memory cost (64 MiB at the library
	// default). A caller hammering distinct challenge IDs could therefore
	// exhaust the process's memory: 100 in flight is 6.4 GiB.
	verifySlots chan struct{}
}

// NewManager creates a new challenge manager.
//
// The client is taken as [RedisClient], the handful of commands the manager
// issues, so *redis.Client, *redis.ClusterClient, *redis.Ring and
// redis.UniversalClient are all accepted. A nil client -- including a typed nil
// -- is tolerated rather than dereferenced: every operation then fails closed
// with an error instead of panicking.
func NewManager(redisClient RedisClient, config Config) *Manager {
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
	if config.MaxConcurrentVerifications <= 0 {
		config.MaxConcurrentVerifications = DefaultMaxConcurrentVerifications
	}

	// Normalise a typed nil (an unassigned *redis.Client field, say) to an
	// untyped one, so the nil checks guarding the direct client calls below
	// are a plain comparison rather than a reflect call per command.
	if isNilClient(redisClient) {
		redisClient = nil
	}

	// Create store instances with appropriate prefixes
	challengeCache := newRedisStore(redisClient, config.ChallengeKeyPrefix)
	lockCache := newRedisStore(redisClient, config.LockKeyPrefix)

	return &Manager{
		client:       redisClient,
		cache:        challengeCache,
		lockCache:    lockCache,
		config:       config,
		argon2Hasher: passwd.NewArgon2Hasher(),
		verifySlots:  make(chan struct{}, config.MaxConcurrentVerifications),
	}
}

// Create creates a new challenge and stores it in Redis
// Returns the challenge, the plaintext code (for sending), and any error
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Challenge, string, error) {
	// Generate challenge ID
	challengeID, err := m.generateChallengeID()
	if err != nil {
		return nil, "", err
	}

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
		return &VerifyResult{OK: false, Reason: ReasonInvalid}, fmt.Errorf("empty challenge id")
	}

	// The Argon2 budget is claimed BEFORE the per-challenge lock, and given
	// back whenever the lock is contended.
	//
	// Queueing for a slot while already holding the lock let the lock's lease
	// run out underneath the waiter: a second request for the same challenge
	// could then take a fresh lock, read the same snapshot, and queue behind
	// the same slots. Once slots opened, both verified identical state -- two
	// correct requests both returning OK, or two wrong ones overwriting each
	// other's attempt counter.
	//
	// But holding a slot while POLLING for the lock is its own denial of
	// service: a burst against one challenge parks every slot on
	// VerifyLockWait of polling, so unrelated challenges are blocked with
	// roughly one Argon2 comparison actually running. The slot is therefore
	// released before each wait and re-taken on the next attempt, so neither
	// budget is ever held while waiting for the other.
	deadline := time.Now().Add(m.config.VerifyLockWait)
	for {
		releaseSlot, err := m.acquireVerifySlot(ctx)
		if err != nil {
			// The caller went away. Nothing was decided, so no attempt is
			// consumed. The context error is wrapped, not formatted away, so
			// errors.Is(err, context.DeadlineExceeded) still holds.
			return &VerifyResult{OK: false, Reason: ReasonLockContention}, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
		}

		token, err := m.tryAcquireLock(ctx, challengeID)
		if err != nil {
			releaseSlot()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return &VerifyResult{OK: false, Reason: ReasonLockContention}, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
			}
			// Redis failure acquiring the lock: fail closed.
			return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
		}

		if token != "" {
			defer releaseSlot()
			defer m.releaseLock(context.WithoutCancel(ctx), challengeID, token)
			return m.verifyLocked(ctx, challengeID, code, opts)
		}

		// Contended. Give the slot back BEFORE waiting.
		releaseSlot()

		if time.Now().After(deadline) {
			return &VerifyResult{OK: false, Reason: ReasonLockContention}, ErrLockUnavailable
		}
		select {
		case <-ctx.Done():
			return &VerifyResult{OK: false, Reason: ReasonLockContention}, fmt.Errorf("%w: %w", ErrLockUnavailable, ctx.Err())
		case <-time.After(m.config.VerifyLockRetry):
		}
	}
}

// verifyLocked performs the verification assuming the per-challenge lock is held.
func (m *Manager) verifyLocked(ctx context.Context, challengeID, code string, opts VerifyOptions) (*VerifyResult, error) {
	// Get challenge from Redis using cache interface
	var challenge Challenge
	if err := m.cache.Get(ctx, challengeID, &challenge); err != nil {
		// Distinguish "not found" (expired/consumed) from a backend error so we
		// fail closed on infrastructure problems instead of reporting "expired".
		if isNotFound(err) {
			return &VerifyResult{OK: false, Reason: ReasonExpired}, fmt.Errorf("challenge not found or expired: %w", err)
		}
		return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: %v", ErrBackendUnavailable, err)
	}

	// Purpose/user/channel binding: a challenge minted for one context must not
	// be redeemable for another. This check runs before code comparison and does
	// NOT consume an attempt, so it cannot be used as an oracle to burn attempts
	// on a legitimate challenge via wrong-context probing.
	if opts.ExpectedUserID != "" && opts.ExpectedUserID != challenge.UserID {
		return &VerifyResult{OK: false, Reason: ReasonContextMismatch}, fmt.Errorf("user id mismatch")
	}
	if opts.ExpectedPurpose != "" && opts.ExpectedPurpose != challenge.Purpose {
		return &VerifyResult{OK: false, Reason: ReasonContextMismatch}, fmt.Errorf("purpose mismatch")
	}
	if opts.ExpectedChannel != "" && opts.ExpectedChannel != challenge.Channel {
		return &VerifyResult{OK: false, Reason: ReasonContextMismatch}, fmt.Errorf("channel mismatch")
	}

	// Check if expired
	if time.Now().After(challenge.ExpiresAt) {
		if err := m.cache.Del(ctx, challengeID); err != nil {
			return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: delete expired: %v", ErrBackendUnavailable, err)
		}
		return &VerifyResult{OK: false, Reason: ReasonExpired}, fmt.Errorf("challenge expired")
	}

	// Check if the challenge already reached max attempts (locked).
	//
	// A lockout is established at most ONCE per challenge, recorded on the
	// challenge itself. Asking the lock cache instead -- "is this user locked
	// right now?" -- only stopped a lockout still in effect from being
	// refreshed. Once it elapsed the answer was no again, so a challenge that
	// outlives LockoutDuration could be polled to mint a fresh full-duration
	// lockout every time, keeping the user out for as long as the challenge
	// stayed valid.
	if challenge.Attempts >= challenge.MaxAttempts {
		if !challenge.LockoutApplied {
			if err := m.ensureUserLocked(ctx, challenge.UserID); err != nil {
				return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, err
			}
			if err := m.recordLockoutApplied(ctx, challengeID, &challenge); err != nil {
				return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, err
			}
		}
		return &VerifyResult{OK: false, Reason: ReasonLocked}, fmt.Errorf("challenge locked due to too many attempts")
	}

	// Check if user is locked (fail closed on Redis error).
	locked, err := m.lockCache.Exists(ctx, challenge.UserID)
	if err != nil {
		return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: user lock check: %v", ErrBackendUnavailable, err)
	}
	if locked {
		return &VerifyResult{OK: false, Reason: ReasonUserLocked}, fmt.Errorf("user is temporarily locked")
	}

	// Verify code (constant-time Argon2 compare).
	matched, err := m.verifyCode(ctx, code, challenge.CodeHash)
	if err != nil {
		// The caller went away. Nothing was decided, so no attempt is consumed.
		// %w for BOTH: err is context.Canceled or DeadlineExceeded here, and
		// folding it in with %v left callers with only the retryable
		// ErrLockUnavailable, so errors.Is(err, context.Canceled) was false
		// and abandoned work looked worth retrying.
		return &VerifyResult{OK: false, Reason: ReasonLockContention}, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
	}
	if !matched {
		challenge.Attempts++
		ttl, ttlErr := m.cache.TTL(ctx, challengeID)
		if ttlErr != nil {
			return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: ttl: %v", ErrBackendUnavailable, ttlErr)
		}
		if ttl <= 0 {
			// Key has no TTL / already gone; treat as expired rather than
			// resurrecting it with a fresh lifetime.
			_ = m.cache.Del(ctx, challengeID)
			return &VerifyResult{OK: false, Reason: ReasonExpired}, fmt.Errorf("challenge expired")
		}
		if err := m.cache.Set(ctx, challengeID, challenge, ttl); err != nil {
			return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: persist attempts: %v", ErrBackendUnavailable, err)
		}
		if challenge.Attempts >= challenge.MaxAttempts {
			if err := m.lockUser(ctx, challenge.UserID); err != nil {
				return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, err
			}
			// Recorded AFTER the lock, never before: a failure here costs at
			// most one extra lockout on a later probe, whereas marking it
			// first and then failing to lock would leave the user unlocked
			// with the challenge believing otherwise.
			if err := m.recordLockoutApplied(ctx, challengeID, &challenge); err != nil {
				return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, err
			}
			remaining := 0
			return &VerifyResult{OK: false, Reason: ReasonLocked, RemainingAttempts: &remaining}, fmt.Errorf("challenge locked due to too many attempts")
		}
		remaining := challenge.MaxAttempts - challenge.Attempts
		return &VerifyResult{OK: false, Reason: ReasonInvalid, RemainingAttempts: &remaining}, fmt.Errorf("invalid code")
	}

	// Success: consume the challenge (one-time use). The delete MUST succeed
	// before we report OK, otherwise a second correct request could also
	// succeed. Fail closed if the delete fails.
	if err := m.cache.Del(ctx, challengeID); err != nil {
		return &VerifyResult{OK: false, Reason: ReasonBackendUnavailable}, fmt.Errorf("%w: consume challenge: %v", ErrBackendUnavailable, err)
	}

	return &VerifyResult{OK: true, Challenge: &challenge}, nil
}

// recordLockoutApplied persists that this challenge has had its lockout
// established, leaving the key's remaining lifetime untouched.
func (m *Manager) recordLockoutApplied(ctx context.Context, challengeID string, challenge *Challenge) error {
	ttl, err := m.cache.TTL(ctx, challengeID)
	if err != nil {
		return fmt.Errorf("%w: ttl: %v", ErrBackendUnavailable, err)
	}
	if ttl <= 0 {
		// Gone or without a lifetime of its own. Nothing to mark, and
		// nothing that can be polled again either.
		return nil
	}
	challenge.LockoutApplied = true
	if err := m.cache.Set(ctx, challengeID, *challenge, ttl); err != nil {
		return fmt.Errorf("%w: persist lockout: %v", ErrBackendUnavailable, err)
	}
	return nil
}

// ensureUserLocked locks the user only if no lockout is currently in effect,
// so an existing deadline is never extended.
func (m *Manager) ensureUserLocked(ctx context.Context, userID string) error {
	locked, err := m.lockCache.Exists(ctx, userID)
	if err != nil {
		return fmt.Errorf("%w: user lock check: %v", ErrBackendUnavailable, err)
	}
	if locked {
		return nil
	}
	return m.lockUser(ctx, userID)
}

// lockUser marks a user as locked, surfacing Redis errors (fail closed).
func (m *Manager) lockUser(ctx context.Context, userID string) error {
	if err := m.lockCache.Set(ctx, userID, "1", m.config.LockoutDuration); err != nil {
		return fmt.Errorf("%w: lock user: %v", ErrBackendUnavailable, err)
	}
	return nil
}

// tryAcquireLock makes ONE attempt at the per-challenge lock.
//
// It returns ("", nil) when the lock is held by somebody else, so the caller
// decides whether to wait -- and, importantly, can give up its Argon2 slot
// first.
func (m *Manager) tryAcquireLock(ctx context.Context, challengeID string) (string, error) {
	if m.client == nil {
		return "", ErrNilClient
	}

	token, err := secure.RandomToken(16)
	if err != nil {
		token, err = secure.RandomHex(16)
		if err != nil {
			return "", err
		}
	}

	err = m.client.SetArgs(ctx, m.config.VerifyLockPrefix+challengeID, token, redis.SetArgs{
		Mode: "NX",
		TTL:  m.config.VerifyLockTTL,
	}).Err()
	switch {
	case err == nil:
		return token, nil
	case errors.Is(err, redis.Nil):
		return "", nil // held by somebody else
	default:
		return "", err
	}
}

// releaseLock releases the lock only if we still own it. It uses EVAL directly
// (rather than Script.Run, which attempts EVALSHA first) so it works against
// backends that do not implement the script cache.
func (m *Manager) releaseLock(ctx context.Context, challengeID, token string) {
	if m.client == nil {
		return
	}
	key := m.config.VerifyLockPrefix + challengeID
	_ = m.client.Eval(ctx, unlockScriptSrc, []string{key}, token).Err()
}

// isNotFound reports whether err represents a missing key (as opposed to a
// backend failure). Only the caller in verifyLocked uses this, and the
// distinction decides whether a request is answered ReasonExpired (terminal,
// consuming nothing) or ReasonBackendUnavailable (fail closed).
//
// Both sentinels are checked because a miss from the store satisfies both --
// ErrNotFound, and redis.Nil which it also matches -- while a direct go-redis
// call yields redis.Nil on its own.
//
// This used to fall back to matching the error TEXT, because the cache it then
// used reported a miss as a plain fmt.Errorf that errors.Is could not see
// through. That fallback is gone: it could turn a backend error merely
// mentioning the phrase into a reported expiry, which consumes no attempt and
// so handed back a free probe on an infrastructure fault.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, redis.Nil) || errors.Is(err, ErrNotFound)
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
	if m.client == nil {
		return "", fmt.Errorf("%w: %v", ErrBackendUnavailable, ErrNilClient)
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

// ErrEntropyUnavailable is returned when the system CSPRNG cannot be read.
// Continuing without it would mean issuing a guessable challenge ID or code.
var ErrEntropyUnavailable = errors.New("challenge: secure random source unavailable")

func (m *Manager) generateChallengeID() (string, error) {
	// RandomToken returns the RawURLEncoding of 16 bytes, which is exactly 22
	// characters. The previous code discarded the fallback's error and then
	// sliced token[:22] unconditionally: if both draws failed -- they share
	// crypto/rand, so they fail together -- token was "" and this panicked
	// with a slice bounds error, in a function whose comment said it handled
	// the case gracefully.
	token, err := secure.RandomToken(16)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEntropyUnavailable, err)
	}
	if len(token) < 22 {
		return "", fmt.Errorf("%w: short token (%d bytes)", ErrEntropyUnavailable, len(token))
	}
	return "ch_" + token[:22], nil
}

// verifyCode runs the Argon2 comparison, bounded by verifySlots so the total
// memory held by in-flight verifications stays bounded.
func (m *Manager) verifyCode(ctx context.Context, code, hash string) (bool, error) {
	// The verifySlots budget is claimed by VerifyWithOptions before it takes
	// the per-challenge lock; see acquireVerifySlot. Only the caller's
	// cancellation is checked here.
	if err := ctx.Err(); err != nil {
		return false, err
	}

	// passwd.Argon2Hasher.Verify uses constant-time comparison internally
	return m.argon2Hasher.Verify(hash, code), nil
}

// acquireVerifySlot claims one of the bounded Argon2 verification slots and
// returns the function that releases it.
func (m *Manager) acquireVerifySlot(ctx context.Context) (func(), error) {
	select {
	case m.verifySlots <- struct{}{}:
		return func() { <-m.verifySlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
