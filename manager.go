package challenge

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	rediskitcache "github.com/soulteary/redis-kit/cache"
	secure "github.com/soulteary/secure-kit"
)

// Manager handles challenge operations
type Manager struct {
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

	// Create cache instances with appropriate prefixes
	challengeCache := rediskitcache.NewCache(redisClient, config.ChallengeKeyPrefix)
	lockCache := rediskitcache.NewCache(redisClient, config.LockKeyPrefix)

	return &Manager{
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

// Verify verifies a code against a challenge
func (m *Manager) Verify(ctx context.Context, challengeID, code, clientIP string) (*VerifyResult, error) {
	// Get challenge from Redis using cache interface
	var challenge Challenge
	if err := m.cache.Get(ctx, challengeID, &challenge); err != nil {
		return &VerifyResult{
			OK:     false,
			Reason: "expired",
		}, fmt.Errorf("challenge not found or expired: %w", err)
	}

	// Check if expired
	if time.Now().After(challenge.ExpiresAt) {
		// Delete expired challenge
		_ = m.cache.Del(ctx, challengeID)
		return &VerifyResult{
			OK:     false,
			Reason: "expired",
		}, fmt.Errorf("challenge expired")
	}

	// Check if locked
	if challenge.Attempts >= challenge.MaxAttempts {
		// Lock the user
		_ = m.lockCache.Set(ctx, challenge.UserID, "1", m.config.LockoutDuration)
		return &VerifyResult{
			OK:     false,
			Reason: "locked",
		}, fmt.Errorf("challenge locked due to too many attempts")
	}

	// Check if user is locked
	exists, err := m.lockCache.Exists(ctx, challenge.UserID)
	if err == nil && exists {
		return &VerifyResult{
			OK:     false,
			Reason: "user_locked",
		}, fmt.Errorf("user is temporarily locked")
	}

	// Verify code
	if !m.verifyCode(code, challenge.CodeHash) {
		// Increment attempts
		challenge.Attempts++
		ttl, err := m.cache.TTL(ctx, challengeID)
		if err == nil && ttl > 0 {
			_ = m.cache.Set(ctx, challengeID, challenge, ttl)
		}
		// Check if should lock after incrementing attempts
		if challenge.Attempts >= challenge.MaxAttempts {
			// Lock the user
			_ = m.lockCache.Set(ctx, challenge.UserID, "1", m.config.LockoutDuration)
			remaining := 0
			return &VerifyResult{
				OK:                false,
				Reason:            "locked",
				RemainingAttempts: &remaining,
			}, fmt.Errorf("challenge locked due to too many attempts")
		}
		remaining := challenge.MaxAttempts - challenge.Attempts
		return &VerifyResult{
			OK:                false,
			Reason:            "invalid",
			RemainingAttempts: &remaining,
		}, fmt.Errorf("invalid code")
	}

	// Success - delete challenge (one-time use)
	_ = m.cache.Del(ctx, challengeID)

	return &VerifyResult{
		OK:        true,
		Challenge: &challenge,
	}, nil
}

// Revoke revokes a challenge
func (m *Manager) Revoke(ctx context.Context, challengeID string) error {
	return m.cache.Del(ctx, challengeID)
}

// IsUserLocked checks if a user is locked
func (m *Manager) IsUserLocked(ctx context.Context, userID string) bool {
	exists, err := m.lockCache.Exists(ctx, userID)
	return err == nil && exists
}

// Get retrieves a challenge by ID
func (m *Manager) Get(ctx context.Context, challengeID string) (*Challenge, error) {
	var challenge Challenge
	if err := m.cache.Get(ctx, challengeID, &challenge); err != nil {
		return nil, fmt.Errorf("challenge not found: %w", err)
	}

	return &challenge, nil
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
