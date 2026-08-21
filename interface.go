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

	// VerifyWithOptions verifies a code with atomic purpose/user/channel binding.
	VerifyWithOptions(ctx context.Context, challengeID, code, clientIP string, opts VerifyOptions) (*VerifyResult, error)

	// Revoke revokes a challenge
	Revoke(ctx context.Context, challengeID string) error

	// RevokePending removes a challenge whose delivery failed (two-phase send).
	RevokePending(ctx context.Context, challengeID string) error

	// SwapActive promotes a challenge to be the single active one for its
	// identity, returning the previously active challenge id (if any).
	SwapActive(ctx context.Context, ch *Challenge) (previousID string, err error)

	// IsUserLocked checks if a user is locked
	IsUserLocked(ctx context.Context, userID string) bool

	// Get retrieves a challenge by ID
	Get(ctx context.Context, challengeID string) (*Challenge, error)
}
