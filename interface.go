package challenge

import "context"

// ManagerInterface defines the interface for challenge management
// This allows for alternative implementations or mocking in tests
type ManagerInterface interface {
	// Create creates a new challenge and stores it in Redis
	// Returns the challenge, the plaintext code (for sending), and any error
	Create(ctx context.Context, req CreateRequest) (*Challenge, string, error)

	// Verify verifies a code against a challenge
	Verify(ctx context.Context, challengeID, code, clientIP string) (*VerifyResult, error)

	// Revoke revokes a challenge
	Revoke(ctx context.Context, challengeID string) error

	// IsUserLocked checks if a user is locked
	IsUserLocked(ctx context.Context, userID string) bool

	// Get retrieves a challenge by ID
	Get(ctx context.Context, challengeID string) (*Challenge, error)
}
