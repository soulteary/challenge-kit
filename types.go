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
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Channel     Channel   `json:"channel"` // "sms" | "email"
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
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		Expiry:             5 * time.Minute,
		MaxAttempts:        5,
		LockoutDuration:    10 * time.Minute,
		CodeLength:         6,
		ChallengeKeyPrefix: "otp:ch:",
		LockKeyPrefix:      "otp:lock:",
	}
}
