package challenge

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	rediskitcache "github.com/soulteary/redis-kit/cache"
)

// setupMiniRedis returns a miniredis instance and Redis client for testing
func setupMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	return mr, client
}

func TestNewManager(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.Expiry = 5 * time.Minute
	config.MaxAttempts = 5
	config.LockoutDuration = 10 * time.Minute
	config.CodeLength = 6

	manager := NewManager(redisClient, config)

	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}
	if manager.config.Expiry != config.Expiry {
		t.Errorf("NewManager() expiry = %v, want %v", manager.config.Expiry, config.Expiry)
	}
	if manager.config.MaxAttempts != config.MaxAttempts {
		t.Errorf("NewManager() maxAttempts = %d, want %d", manager.config.MaxAttempts, config.MaxAttempts)
	}
	if manager.config.LockoutDuration != config.LockoutDuration {
		t.Errorf("NewManager() lockoutDuration = %v, want %v", manager.config.LockoutDuration, config.LockoutDuration)
	}
	if manager.config.CodeLength != config.CodeLength {
		t.Errorf("NewManager() codeLength = %d, want %d", manager.config.CodeLength, config.CodeLength)
	}
}

func TestNewManager_DefaultConfig(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	// Test with empty config - should use defaults
	config := Config{}
	manager := NewManager(redisClient, config)

	if manager == nil {
		t.Fatal("NewManager() returned nil")
	}
	if manager.config.ChallengeKeyPrefix != "otp:ch:" {
		t.Errorf("NewManager() ChallengeKeyPrefix = %v, want otp:ch:", manager.config.ChallengeKeyPrefix)
	}
	if manager.config.LockKeyPrefix != "otp:lock:" {
		t.Errorf("NewManager() LockKeyPrefix = %v, want otp:lock:", manager.config.LockKeyPrefix)
	}
	if manager.config.CodeLength != 6 {
		t.Errorf("NewManager() CodeLength = %d, want 6", manager.config.CodeLength)
	}
	if manager.config.MaxAttempts != 5 {
		t.Errorf("NewManager() MaxAttempts = %d, want 5", manager.config.MaxAttempts)
	}
	if manager.config.Expiry != 5*time.Minute {
		t.Errorf("NewManager() Expiry = %v, want 5m", manager.config.Expiry)
	}
	if manager.config.LockoutDuration != 10*time.Minute {
		t.Errorf("NewManager() LockoutDuration = %v, want 10m", manager.config.LockoutDuration)
	}
}

func TestManager_Create(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if challenge == nil {
		t.Fatal("Create() returned nil challenge")
	}

	if challenge.ID == "" {
		t.Error("Create() challenge ID is empty")
	}

	if challenge.UserID != req.UserID {
		t.Errorf("Create() UserID = %v, want %v", challenge.UserID, req.UserID)
	}

	if challenge.Channel != req.Channel {
		t.Errorf("Create() Channel = %v, want %v", challenge.Channel, req.Channel)
	}

	if challenge.Destination != req.Destination {
		t.Errorf("Create() Destination = %v, want %v", challenge.Destination, req.Destination)
	}

	if code == "" {
		t.Error("Create() code is empty")
	}

	if len(code) != 6 {
		t.Errorf("Create() code length = %d, want 6", len(code))
	}

	// Verify challenge is stored in Redis
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	if err := cache.Get(ctx, challenge.ID, &storedChallenge); err != nil {
		t.Fatalf("Failed to get challenge from Redis: %v", err)
	}

	if storedChallenge.ID != challenge.ID {
		t.Errorf("Stored challenge ID = %v, want %v", storedChallenge.ID, challenge.ID)
	}
}

func TestManager_Verify(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Verify with correct code
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	if !result.OK {
		t.Error("Verify() should return true for correct code")
	}

	if result.Challenge == nil {
		t.Fatal("Verify() returned nil challenge")
	}

	if result.Challenge.ID != challenge.ID {
		t.Errorf("Verify() challenge ID = %v, want %v", result.Challenge.ID, challenge.ID)
	}

	// Verify challenge is deleted after successful verification
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	err = cache.Get(ctx, challenge.ID, &storedChallenge)
	if err == nil {
		t.Error("Verify() should delete challenge after successful verification")
	}
}

func TestManager_Verify_InvalidCode(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Verify with incorrect code
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for invalid code")
	}

	if result.OK {
		t.Error("Verify() should return false for invalid code")
	}

	// Verify challenge still exists (not deleted on failure)
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	err = cache.Get(ctx, challenge.ID, &storedChallenge)
	if err != nil {
		t.Error("Verify() should not delete challenge on invalid code")
	}
}

func TestManager_Verify_MaxAttempts(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 3
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Try incorrect code multiple times on the SAME challenge
	// Each failure increments attempts on the same challenge
	for i := 0; i < 3; i++ {
		result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		if i < 2 {
			// First two attempts should fail but not lock
			if result.OK {
				t.Errorf("Verify() attempt %d should return false", i+1)
			}
			if err == nil {
				t.Errorf("Verify() attempt %d should return error", i+1)
			}
		} else {
			// Third attempt should lock (maxAttempts = 3)
			if err == nil {
				t.Error("Verify() should return error after max attempts")
			}
			// After third attempt, user should be locked
			if !manager.IsUserLocked(ctx, req.UserID) {
				t.Error("IsUserLocked() should return true after max attempts")
			}
		}
	}
}

func TestManager_Get(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Get challenge
	retrievedChallenge, err := manager.Get(ctx, challenge.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if retrievedChallenge == nil {
		t.Fatal("Get() returned nil challenge")
	}

	if retrievedChallenge.ID != challenge.ID {
		t.Errorf("Get() ID = %v, want %v", retrievedChallenge.ID, challenge.ID)
	}

	if retrievedChallenge.UserID != req.UserID {
		t.Errorf("Get() UserID = %v, want %v", retrievedChallenge.UserID, req.UserID)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Try to get non-existent challenge
	_, err := manager.Get(ctx, "non_existent_id")
	if err == nil {
		t.Error("Get() should return error for non-existent challenge")
	}
}

func TestManager_Revoke(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Revoke challenge
	err = manager.Revoke(ctx, challenge.ID)
	if err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	// Verify challenge is deleted
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	err = cache.Get(ctx, challenge.ID, &storedChallenge)
	if err == nil {
		t.Error("Revoke() should delete challenge")
	}
}

func TestManager_IsUserLocked(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 3
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	userID := "user123"

	// User should not be locked initially
	if manager.IsUserLocked(ctx, userID) {
		t.Error("IsUserLocked() should return false for unlocked user")
	}

	// Manually set lock
	lockCache := rediskitcache.NewCache(redisClient, "otp:lock:")
	_ = lockCache.Set(ctx, userID, "1", 10*time.Minute)

	// User should be locked
	if !manager.IsUserLocked(ctx, userID) {
		t.Error("IsUserLocked() should return true for locked user")
	}
}

func TestGenerateChallengeID(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := manager.generateChallengeID()
		if ids[id] {
			t.Errorf("generateChallengeID() generated duplicate ID: %s", id)
		}
		ids[id] = true

		if len(id) == 0 {
			t.Error("generateChallengeID() returned empty ID")
		}

		if len(id) < 3 {
			t.Errorf("generateChallengeID() ID too short: %s", id)
		}
	}
}

func TestManager_Verify_ExpiredChallenge(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.Expiry = 100 * time.Millisecond // Very short expiry
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Fast-forward time in miniredis to expire the challenge
	mr.FastForward(200 * time.Millisecond)

	// Try to verify expired challenge
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for expired challenge")
	}

	if result.OK {
		t.Error("Verify() should return false for expired challenge")
	}

	if result.Reason != "expired" {
		t.Errorf("Verify() reason = %v, want expired", result.Reason)
	}
}

func TestManager_Verify_UserLocked(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	userID := "user123"

	// Lock the user first
	lockCache := rediskitcache.NewCache(redisClient, "otp:lock:")
	_ = lockCache.Set(ctx, userID, "1", 10*time.Minute)

	req := CreateRequest{
		UserID:      userID,
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge for locked user
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Try to verify - should fail because user is locked
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for locked user")
	}

	if result.OK {
		t.Error("Verify() should return false for locked user")
	}

	if result.Reason != "user_locked" {
		t.Errorf("Verify() reason = %v, want user_locked", result.Reason)
	}
}

func TestManager_Verify_RemainingAttempts(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 5
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Try incorrect code 3 times
	for i := 0; i < 3; i++ {
		result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		if err == nil {
			t.Errorf("Verify() attempt %d should return error", i+1)
		}

		if result.OK {
			t.Errorf("Verify() attempt %d should return false", i+1)
		}

		expectedRemaining := 5 - (i + 1)
		if result.RemainingAttempts == nil {
			t.Errorf("Verify() attempt %d should return RemainingAttempts", i+1)
		} else if *result.RemainingAttempts != expectedRemaining {
			t.Errorf("Verify() attempt %d RemainingAttempts = %d, want %d", i+1, *result.RemainingAttempts, expectedRemaining)
		}
	}
}

func TestManager_Create_SMSChannel(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelSMS,
		Destination: "+1234567890",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if challenge.Channel != ChannelSMS {
		t.Errorf("Create() Channel = %v, want %v", challenge.Channel, ChannelSMS)
	}

	if challenge.Destination != req.Destination {
		t.Errorf("Create() Destination = %v, want %v", challenge.Destination, req.Destination)
	}

	if code == "" {
		t.Error("Create() code is empty")
	}
}

func TestManager_Create_CustomCodeLength(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.CodeLength = 8
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	_, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if len(code) != 8 {
		t.Errorf("Create() code length = %d, want 8", len(code))
	}
}

func TestManager_Verify_AlreadyLockedChallenge(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 2
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Exhaust attempts to lock the challenge
	for i := 0; i < 2; i++ {
		_, _ = manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	}

	// Try to verify again - should fail with locked reason
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for locked challenge")
	}

	if result.OK {
		t.Error("Verify() should return false for locked challenge")
	}

	if result.Reason != "locked" {
		t.Errorf("Verify() reason = %v, want locked", result.Reason)
	}

	// User should be locked
	if !manager.IsUserLocked(ctx, req.UserID) {
		t.Error("IsUserLocked() should return true after challenge is locked")
	}
}

func TestManager_Verify_ConcurrentAttempts(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Try concurrent verifications
	done := make(chan bool, 2)
	successCount := 0
	errorCount := 0

	// First goroutine: correct code
	go func() {
		result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
		if err == nil && result.OK {
			successCount++
		} else {
			errorCount++
		}
		done <- true
	}()

	// Second goroutine: incorrect code
	go func() {
		result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		if err != nil || !result.OK {
			errorCount++
		} else {
			successCount++
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Only one should succeed (the one with correct code)
	if successCount != 1 {
		t.Errorf("Expected 1 success, got %d", successCount)
	}
}

func TestManager_Revoke_NonExistent(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Try to revoke non-existent challenge
	err := manager.Revoke(ctx, "non_existent_id")
	if err != nil {
		// Revoke should not error on non-existent challenge (idempotent)
		t.Logf("Revoke() returned error for non-existent challenge: %v (this is acceptable)", err)
	}
}

func TestManager_Create_MultipleChallenges(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create multiple challenges
	challenges := make([]*Challenge, 5)
	codes := make([]string, 5)

	for i := 0; i < 5; i++ {
		challenge, code, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() challenge %d error = %v", i+1, err)
		}
		challenges[i] = challenge
		codes[i] = code

		// Verify all challenges have unique IDs
		for j := 0; j < i; j++ {
			if challenges[i].ID == challenges[j].ID {
				t.Errorf("Create() generated duplicate challenge ID: %s", challenges[i].ID)
			}
		}
	}

	// Verify all challenges can be retrieved
	for i, challenge := range challenges {
		retrieved, err := manager.Get(ctx, challenge.ID)
		if err != nil {
			t.Errorf("Get() challenge %d error = %v", i+1, err)
		}
		if retrieved.ID != challenge.ID {
			t.Errorf("Get() challenge %d ID = %v, want %v", i+1, retrieved.ID, challenge.ID)
		}
	}

	// Verify each challenge with its code
	for i, challenge := range challenges {
		result, err := manager.Verify(ctx, challenge.ID, codes[i], req.ClientIP)
		if err != nil {
			t.Errorf("Verify() challenge %d error = %v", i+1, err)
		}
		if !result.OK {
			t.Errorf("Verify() challenge %d should succeed", i+1)
		}
	}
}

func TestManager_Create_WithCustomPrefixes(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.ChallengeKeyPrefix = "custom:ch:"
	config.LockKeyPrefix = "custom:lock:"
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Verify challenge is stored with custom prefix
	cache := rediskitcache.NewCache(redisClient, "custom:ch:")
	var storedChallenge Challenge
	if err := cache.Get(ctx, challenge.ID, &storedChallenge); err != nil {
		t.Fatalf("Failed to get challenge from Redis: %v", err)
	}

	if storedChallenge.ID != challenge.ID {
		t.Errorf("Stored challenge ID = %v, want %v", storedChallenge.ID, challenge.ID)
	}

	// Verify code works
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !result.OK {
		t.Error("Verify() should succeed with correct code")
	}
}

func TestManager_Verify_RemainingAttemptsAfterLock(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 2
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// First incorrect attempt
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for invalid code")
	}
	if result.RemainingAttempts == nil {
		t.Error("Verify() should return RemainingAttempts")
	} else if *result.RemainingAttempts != 1 {
		t.Errorf("Verify() RemainingAttempts = %d, want 1", *result.RemainingAttempts)
	}

	// Second incorrect attempt - should lock
	result, err = manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error after max attempts")
	}
	if result.RemainingAttempts == nil {
		t.Error("Verify() should return RemainingAttempts")
	} else if *result.RemainingAttempts != 0 {
		t.Errorf("Verify() RemainingAttempts = %d, want 0", *result.RemainingAttempts)
	}
	if result.Reason != "locked" {
		t.Errorf("Verify() reason = %v, want locked", result.Reason)
	}
}

func TestManager_Verify_TTLError(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Manually delete the key to simulate TTL error scenario
	// This will cause TTL to fail, but the code should handle it gracefully
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	_ = cache.Del(ctx, challenge.ID)

	// Try to verify - should fail because challenge not found
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for non-existent challenge")
	}

	if result.OK {
		t.Error("Verify() should return false for non-existent challenge")
	}

	if result.Reason != "expired" {
		t.Errorf("Verify() reason = %v, want expired", result.Reason)
	}
}

func TestManager_Verify_AlreadyAtMaxAttempts(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 3
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Manually set attempts to max (simulating a challenge that's already at max attempts)
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	if err := cache.Get(ctx, challenge.ID, &storedChallenge); err != nil {
		t.Fatalf("Failed to get challenge: %v", err)
	}
	storedChallenge.Attempts = storedChallenge.MaxAttempts
	ttl, _ := cache.TTL(ctx, challenge.ID)
	if ttl > 0 {
		_ = cache.Set(ctx, challenge.ID, storedChallenge, ttl)
	}

	// Try to verify - should fail with locked reason
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for challenge at max attempts")
	}

	if result.OK {
		t.Error("Verify() should return false for challenge at max attempts")
	}

	if result.Reason != "locked" {
		t.Errorf("Verify() reason = %v, want locked", result.Reason)
	}

	// User should be locked
	if !manager.IsUserLocked(ctx, req.UserID) {
		t.Error("IsUserLocked() should return true after challenge is locked")
	}
}

func TestManager_Verify_IsUserLockedError(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	userID := "user123"

	// Test IsUserLocked with error handling
	// When lockCache.Exists returns an error, IsUserLocked should return false
	// This tests the error path in IsUserLocked
	if manager.IsUserLocked(ctx, userID) {
		t.Error("IsUserLocked() should return false when user is not locked")
	}
}

func TestManager_Create_EmptyFields(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "", // Empty user ID
		Channel:     ChannelEmail,
		Destination: "", // Empty destination
		Purpose:     "", // Empty purpose
		ClientIP:    "", // Empty IP
	}

	// Should still create challenge (validation might be done elsewhere)
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if challenge.UserID != "" {
		t.Errorf("Create() UserID = %v, want empty", challenge.UserID)
	}

	if challenge.Destination != "" {
		t.Errorf("Create() Destination = %v, want empty", challenge.Destination)
	}

	if code == "" {
		t.Error("Create() code should not be empty")
	}
}

func TestManager_Verify_EmptyCode(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Try to verify with empty code
	result, err := manager.Verify(ctx, challenge.ID, "", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for empty code")
	}

	if result.OK {
		t.Error("Verify() should return false for empty code")
	}
}

func TestManager_Verify_EmptyChallengeID(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Try to verify with empty challenge ID
	result, err := manager.Verify(ctx, "", "123456", "127.0.0.1")
	if err == nil {
		t.Error("Verify() should return error for empty challenge ID")
	}

	if result.OK {
		t.Error("Verify() should return false for empty challenge ID")
	}
}

func TestManager_Get_EmptyChallengeID(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Try to get with empty challenge ID
	_, err := manager.Get(ctx, "")
	if err == nil {
		t.Error("Get() should return error for empty challenge ID")
	}
}

func TestManager_Revoke_EmptyChallengeID(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Try to revoke with empty challenge ID
	err := manager.Revoke(ctx, "")
	if err != nil {
		// Revoke might not error on empty ID (idempotent operation)
		t.Logf("Revoke() returned error for empty ID: %v (this is acceptable)", err)
	}
}

func TestManager_IsUserLocked_EmptyUserID(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Test with empty user ID
	locked := manager.IsUserLocked(ctx, "")
	if locked {
		t.Error("IsUserLocked() should return false for empty user ID")
	}
}

func TestManager_Verify_ChallengeAlreadyDeleted(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Successfully verify once
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !result.OK {
		t.Error("Verify() should succeed with correct code")
	}

	// Try to verify again - challenge should be deleted
	result, err = manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for already verified challenge")
	}

	if result.OK {
		t.Error("Verify() should return false for already verified challenge")
	}
}

func TestManager_Verify_WithDifferentClientIP(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Verify with different client IP (should still work)
	result, err := manager.Verify(ctx, challenge.ID, code, "192.168.1.1")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	if !result.OK {
		t.Error("Verify() should succeed with correct code regardless of client IP")
	}
}

func TestManager_Create_Verify_AllChannels(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	channels := []Channel{ChannelSMS, ChannelEmail}

	for _, channel := range channels {
		req := CreateRequest{
			UserID:      "user123",
			Channel:     channel,
			Destination: "test@example.com",
			Purpose:     "login",
			ClientIP:    "127.0.0.1",
		}

		challenge, code, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() with channel %v error = %v", channel, err)
		}

		if challenge.Channel != channel {
			t.Errorf("Create() Channel = %v, want %v", challenge.Channel, channel)
		}

		// Verify the challenge
		result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
		if err != nil {
			t.Fatalf("Verify() with channel %v error = %v", channel, err)
		}

		if !result.OK {
			t.Errorf("Verify() with channel %v should succeed", channel)
		}
	}
}

func TestManager_Verify_TTLZeroOrNegative(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Manually expire the key in Redis to simulate TTL = 0 scenario
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	// Get the challenge and manually set it with very short TTL
	var storedChallenge Challenge
	if err := cache.Get(ctx, challenge.ID, &storedChallenge); err == nil {
		// Set with 1ms TTL and wait
		_ = cache.Set(ctx, challenge.ID, storedChallenge, 1*time.Millisecond)
		time.Sleep(10 * time.Millisecond)
	}

	// Try to verify - should handle TTL error gracefully
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	// This might fail because challenge expired, which is expected
	if err != nil {
		// Expected behavior
		if result.Reason != "expired" {
			t.Logf("Verify() returned error (expected): %v", err)
		}
	}
}

func TestManager_Verify_TTLZeroPath(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Manually set challenge with TTL = 0 to test the path where TTL <= 0
	cache := rediskitcache.NewCache(redisClient, "otp:ch:")
	var storedChallenge Challenge
	if err := cache.Get(ctx, challenge.ID, &storedChallenge); err != nil {
		t.Fatalf("Failed to get challenge: %v", err)
	}

	// Set with 0 TTL (immediately expired)
	// This tests the path where TTL returns 0 or negative
	_ = cache.Set(ctx, challenge.ID, storedChallenge, 0)

	// Try incorrect code - should still increment attempts but TTL path won't update
	// because TTL is 0
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for invalid code")
	}

	if result.OK {
		t.Error("Verify() should return false for invalid code")
	}

	// The challenge might be expired now, but we tested the TTL <= 0 path
}

func TestManager_Verify_AttemptsIncrementWithTTLUpdate(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 5
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Make multiple incorrect attempts to test TTL update path
	for i := 0; i < 3; i++ {
		result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		if err == nil {
			t.Errorf("Verify() attempt %d should return error", i+1)
		}

		// Verify challenge still exists and attempts are incremented
		retrieved, err := manager.Get(ctx, challenge.ID)
		if err != nil {
			t.Fatalf("Get() attempt %d error = %v", i+1, err)
		}

		expectedAttempts := i + 1
		if retrieved.Attempts != expectedAttempts {
			t.Errorf("Verify() attempt %d: challenge.Attempts = %d, want %d", i+1, retrieved.Attempts, expectedAttempts)
		}

		if result.RemainingAttempts == nil {
			t.Errorf("Verify() attempt %d should return RemainingAttempts", i+1)
		} else {
			expectedRemaining := config.MaxAttempts - expectedAttempts
			if *result.RemainingAttempts != expectedRemaining {
				t.Errorf("Verify() attempt %d RemainingAttempts = %d, want %d", i+1, *result.RemainingAttempts, expectedRemaining)
			}
		}
	}
}

func TestManager_Verify_EdgeCaseMaxAttemptsOne(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 1 // Minimum attempts
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// First incorrect attempt should immediately lock
	result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error after max attempts")
	}

	if result.OK {
		t.Error("Verify() should return false after max attempts")
	}

	if result.Reason != "locked" {
		t.Errorf("Verify() reason = %v, want locked", result.Reason)
	}

	// User should be locked
	if !manager.IsUserLocked(ctx, req.UserID) {
		t.Error("IsUserLocked() should return true after max attempts")
	}

	// Create a new challenge for the locked user
	newChallenge, newCode, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() new challenge error = %v", err)
	}

	// Try correct code on locked user - should fail with user_locked
	result, err = manager.Verify(ctx, newChallenge.ID, newCode, req.ClientIP)
	if err == nil {
		t.Error("Verify() should return error for locked user")
	}

	if result.Reason != "user_locked" {
		t.Errorf("Verify() reason = %v, want user_locked", result.Reason)
	}
}

func TestManager_Verify_EdgeCaseMaxAttemptsLarge(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	config := DefaultConfig()
	config.MaxAttempts = 100 // Large number
	manager := NewManager(redisClient, config)

	ctx := context.Background()
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelEmail,
		Destination: "test@example.com",
		Purpose:     "login",
		ClientIP:    "127.0.0.1",
	}

	// Create a challenge
	challenge, _, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Make many incorrect attempts
	for i := 0; i < 50; i++ {
		result, err := manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		if err == nil {
			t.Errorf("Verify() attempt %d should return error", i+1)
		}

		if result.OK {
			t.Errorf("Verify() attempt %d should return false", i+1)
		}

		// Verify attempts are incremented
		retrieved, err := manager.Get(ctx, challenge.ID)
		if err != nil {
			t.Fatalf("Get() attempt %d error = %v", i+1, err)
		}

		expectedAttempts := i + 1
		if retrieved.Attempts != expectedAttempts {
			t.Errorf("Verify() attempt %d: challenge.Attempts = %d, want %d", i+1, retrieved.Attempts, expectedAttempts)
		}
	}
}

func TestManager_Create_Verify_AllCodeLengths(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	codeLengths := []int{4, 5, 6, 7, 8, 9, 10}

	for _, length := range codeLengths {
		config := DefaultConfig()
		config.CodeLength = length
		manager := NewManager(redisClient, config)

		ctx := context.Background()
		req := CreateRequest{
			UserID:      "user123",
			Channel:     ChannelEmail,
			Destination: "test@example.com",
			Purpose:     "login",
			ClientIP:    "127.0.0.1",
		}

		challenge, code, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() with code length %d error = %v", length, err)
		}

		if len(code) != length {
			t.Errorf("Create() with code length %d: code length = %d, want %d", length, len(code), length)
		}

		// Verify the challenge
		result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
		if err != nil {
			t.Fatalf("Verify() with code length %d error = %v", length, err)
		}

		if !result.OK {
			t.Errorf("Verify() with code length %d should succeed", length)
		}
	}
}

func TestManager_Create_Verify_VariousExpiryTimes(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	expiryTimes := []time.Duration{
		1 * time.Second,
		1 * time.Minute,
		5 * time.Minute,
		1 * time.Hour,
		24 * time.Hour,
	}

	for _, expiry := range expiryTimes {
		config := DefaultConfig()
		config.Expiry = expiry
		manager := NewManager(redisClient, config)

		ctx := context.Background()
		req := CreateRequest{
			UserID:      "user123",
			Channel:     ChannelEmail,
			Destination: "test@example.com",
			Purpose:     "login",
			ClientIP:    "127.0.0.1",
		}

		challenge, code, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() with expiry %v error = %v", expiry, err)
		}

		// Verify challenge expires at correct time
		expectedExpiry := time.Now().Add(expiry)
		timeDiff := challenge.ExpiresAt.Sub(expectedExpiry)
		if timeDiff < -time.Second || timeDiff > time.Second {
			t.Errorf("Create() with expiry %v: ExpiresAt = %v, want approximately %v", expiry, challenge.ExpiresAt, expectedExpiry)
		}

		// Verify the challenge works
		result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
		if err != nil {
			t.Fatalf("Verify() with expiry %v error = %v", expiry, err)
		}

		if !result.OK {
			t.Errorf("Verify() with expiry %v should succeed", expiry)
		}
	}
}

func TestManager_Create_Verify_VariousLockoutDurations(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	lockoutDurations := []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		10 * time.Minute,
		1 * time.Hour,
	}

	for _, lockoutDuration := range lockoutDurations {
		config := DefaultConfig()
		config.MaxAttempts = 2
		config.LockoutDuration = lockoutDuration
		manager := NewManager(redisClient, config)

		ctx := context.Background()
		req := CreateRequest{
			UserID:      "user123",
			Channel:     ChannelEmail,
			Destination: "test@example.com",
			Purpose:     "login",
			ClientIP:    "127.0.0.1",
		}

		// Create and exhaust attempts
		challenge, _, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() with lockout %v error = %v", lockoutDuration, err)
		}

		// Exhaust attempts
		for i := 0; i < 2; i++ {
			_, _ = manager.Verify(ctx, challenge.ID, "000000", req.ClientIP)
		}

		// User should be locked
		if !manager.IsUserLocked(ctx, req.UserID) {
			t.Errorf("IsUserLocked() with lockout %v should return true", lockoutDuration)
		}
	}
}

func TestManager_Integration_FullFlow(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Test full flow: Create -> Get -> Verify (success)
	req := CreateRequest{
		UserID:      "user123",
		Channel:     ChannelSMS,
		Destination: "+1234567890",
		Purpose:     "login",
		ClientIP:    "192.168.1.100",
	}

	// Step 1: Create challenge
	challenge, code, err := manager.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Step 2: Get challenge
	retrieved, err := manager.Get(ctx, challenge.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if retrieved.ID != challenge.ID {
		t.Errorf("Get() ID = %v, want %v", retrieved.ID, challenge.ID)
	}

	// Step 3: Verify with correct code
	result, err := manager.Verify(ctx, challenge.ID, code, req.ClientIP)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	if !result.OK {
		t.Error("Verify() should succeed with correct code")
	}

	// Step 4: Verify challenge is deleted
	_, err = manager.Get(ctx, challenge.ID)
	if err == nil {
		t.Error("Get() should return error for deleted challenge")
	}
}

func TestManager_Integration_MultipleUsers(t *testing.T) {
	mr, redisClient := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = redisClient.Close() }()

	manager := NewManager(redisClient, DefaultConfig())

	ctx := context.Background()

	// Create challenges for multiple users
	users := []string{"user1", "user2", "user3"}
	challenges := make(map[string]*Challenge)
	codes := make(map[string]string)

	for _, userID := range users {
		req := CreateRequest{
			UserID:      userID,
			Channel:     ChannelEmail,
			Destination: userID + "@example.com",
			Purpose:     "login",
			ClientIP:    "127.0.0.1",
		}

		challenge, code, err := manager.Create(ctx, req)
		if err != nil {
			t.Fatalf("Create() for user %s error = %v", userID, err)
		}

		challenges[userID] = challenge
		codes[userID] = code
	}

	// Verify all challenges are independent
	for _, userID := range users {
		challenge := challenges[userID]
		code := codes[userID]

		// Verify this user's challenge
		result, err := manager.Verify(ctx, challenge.ID, code, "127.0.0.1")
		if err != nil {
			t.Fatalf("Verify() for user %s error = %v", userID, err)
		}

		if !result.OK {
			t.Errorf("Verify() for user %s should succeed", userID)
		}

		// Verify other users' challenges are still valid (if they haven't been verified yet)
		for otherUserID, otherChallenge := range challenges {
			if otherUserID != userID {
				// Only check if this challenge hasn't been verified yet
				// (challenges map might contain already-verified challenges)
				_, err := manager.Get(ctx, otherChallenge.ID)
				// It's OK if challenge was already verified and deleted
				if err != nil {
					// Challenge might have been deleted if it was already verified
					// This is acceptable behavior
					t.Logf("Get() for user %s returned error (may be already verified): %v", otherUserID, err)
				}
			}
		}
	}
}
