package challenge

import "time"

// Channel represents the communication channel for OTP delivery
type Channel string

const (
	// ChannelSMS represents SMS channel
	ChannelSMS Channel = "sms"
	// ChannelEmail represents Email channel
	ChannelEmail Channel = "email"
	// ChannelDingTalk represents DingTalk work notification channel (via herald-dingtalk)
	ChannelDingTalk Channel = "dingtalk"
)

// Challenge represents a verification challenge
type Challenge struct {
	ID      string  `json:"id"`
	UserID  string  `json:"user_id"`
	Channel Channel `json:"channel"` // "sms" | "email"
	// Destination is the phone number or email address the code was sent to.
	//
	// NOTE: this is stored in Redis in the clear. The active-index key is
	// deliberately an irreversible digest so no raw identifier appears in a
	// key, but the challenge VALUE still holds it -- anyone with read access
	// to the Redis instance can enumerate destinations. Keep the instance
	// access-controlled, and keep Expiry short.
	Destination string    `json:"destination"`
	CodeHash    string    `json:"code_hash"`
	Purpose     string    `json:"purpose"`
	ExpiresAt   time.Time `json:"expires_at"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	CreatedIP   string    `json:"created_ip"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateRequest represents a request to create a challenge
type CreateRequest struct {
	UserID      string
	Channel     Channel
	Destination string
	Purpose     string
	ClientIP    string
}

// VerifyResult represents the result of verifying a challenge
type VerifyResult struct {
	OK                bool
	Challenge         *Challenge
	Reason            string
	RemainingAttempts *int
}

// Config holds configuration for the challenge manager
type Config struct {
	// Expiry is the TTL for challenges
	Expiry time.Duration
	// MaxAttempts is the maximum number of verification attempts allowed
	MaxAttempts int
	// LockoutDuration is how long a user is locked after max attempts
	LockoutDuration time.Duration
	// CodeLength is the length of the generated OTP code
	CodeLength int
	// ChallengeKeyPrefix is the Redis key prefix for challenges
	ChallengeKeyPrefix string
	// LockKeyPrefix is the Redis key prefix for user locks
	LockKeyPrefix string

	// VerifyLockPrefix is the Redis key prefix for the per-challenge
	// verification mutex used to make Verify atomic (default "otp:vlock:").
	VerifyLockPrefix string
	// VerifyLockTTL is the lock lease duration. It must comfortably exceed the
	// worst-case Argon2 verification time (default 5s).
	VerifyLockTTL time.Duration
	// VerifyLockWait is the maximum time to wait to acquire the verification
	// lock before returning ErrLockUnavailable (default 2s).
	VerifyLockWait time.Duration
	// VerifyLockRetry is the polling interval while waiting for the lock
	// (default 25ms).
	VerifyLockRetry time.Duration

	// ActiveIndexPrefix is the Redis key prefix for the single-active-challenge
	// index keyed by an irreversible identity digest (default "otp:active:").
	ActiveIndexPrefix string

	// MaxConcurrentVerifications bounds how many Argon2 comparisons run at
	// once (default 16). Each in-flight verification holds the Argon2 memory
	// cost, 64 MiB at the library default, so an unbounded number of them is a
	// memory-exhaustion vector for anyone who can call Verify with distinct
	// challenge IDs.
	MaxConcurrentVerifications int
}

// DefaultMaxConcurrentVerifications bounds concurrent Argon2 work. At the
// default 64 MiB cost this caps verification memory at about 1 GiB.
const DefaultMaxConcurrentVerifications = 16

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		Expiry:             5 * time.Minute,
		MaxAttempts:        5,
		LockoutDuration:    10 * time.Minute,
		CodeLength:         6,
		ChallengeKeyPrefix: "otp:ch:",
		LockKeyPrefix:      "otp:lock:",

		// Kept in step with NewManager's normalisation, so reading a field off
		// DefaultConfig() gives the value a Manager would actually use.
		VerifyLockPrefix:           "otp:vlock:",
		VerifyLockTTL:              5 * time.Second,
		VerifyLockWait:             2 * time.Second,
		VerifyLockRetry:            25 * time.Millisecond,
		ActiveIndexPrefix:          "otp:active:",
		MaxConcurrentVerifications: DefaultMaxConcurrentVerifications,
	}
}
