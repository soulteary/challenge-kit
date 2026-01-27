# challenge-kit

A Go library for managing OTP (One-Time Password) challenges with support for challenge lifecycle management, code generation, verification, and user lockout mechanisms.

## Features

- **Challenge Lifecycle Management**: Create, verify, and revoke challenges
- **OTP Code Generation**: Secure random numeric code generation
- **Code Verification**: Argon2-based secure code hashing and verification
- **Attempt Tracking**: Automatic tracking of verification attempts
- **User Lockout**: Automatic user lockout after maximum failed attempts
- **Redis Storage**: Built-in Redis support for challenge and lock storage
- **Configurable**: Flexible configuration for expiry, attempts, and lockout duration
- **Thread-Safe**: Safe for concurrent access

## Installation

```bash
go get github.com/soulteary/challenge-kit
```

## Quick Start

### Basic Usage

```go
package main

import (
    "context"
    "time"
    
    "github.com/redis/go-redis/v9"
    "github.com/soulteary/challenge-kit"
)

func main() {
    // Create Redis client
    redisClient := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })
    defer redisClient.Close()

    // Create challenge manager with default config
    config := challenge.DefaultConfig()
    manager := challenge.NewManager(redisClient, config)

    ctx := context.Background()

    // Create a challenge
    req := challenge.CreateRequest{
        UserID:      "user123",
        Channel:     challenge.ChannelEmail,
        Destination: "user@example.com",
        Purpose:     "login",
        ClientIP:    "127.0.0.1",
    }

    ch, code, err := manager.Create(ctx, req)
    if err != nil {
        panic(err)
    }

    // Send code to user (via SMS/Email)
    // ...

    // Verify the code
    result, err := manager.Verify(ctx, ch.ID, code, req.ClientIP)
    if err != nil {
        // Handle error
    }

    if result.OK {
        // Challenge verified successfully
        // Proceed with authentication
    } else {
        // Handle verification failure
        // result.Reason contains: "expired", "invalid", "locked", "user_locked"
        // result.RemainingAttempts contains remaining attempts (if applicable)
    }
}
```

### Custom Configuration

```go
config := challenge.Config{
    Expiry:           5 * time.Minute,  // Challenge TTL
    MaxAttempts:      5,                // Maximum verification attempts
    LockoutDuration:  10 * time.Minute, // User lockout duration
    CodeLength:       6,                 // OTP code length (1-10)
    ChallengeKeyPrefix: "otp:ch:",       // Redis key prefix for challenges
    LockKeyPrefix:     "otp:lock:",      // Redis key prefix for locks
}

manager := challenge.NewManager(redisClient, config)
```

## API Reference

### Types

#### Challenge

```go
type Challenge struct {
    ID          string
    UserID      string
    Channel     Channel  // "sms" | "email"
    Destination string
    CodeHash    string
    Purpose     string
    ExpiresAt   time.Time
    Attempts    int
    MaxAttempts int
    CreatedIP   string
    CreatedAt   time.Time
}
```

#### CreateRequest

```go
type CreateRequest struct {
    UserID      string
    Channel     Channel
    Destination string
    Purpose     string
    ClientIP    string
}
```

#### VerifyResult

```go
type VerifyResult struct {
    OK                bool
    Challenge         *Challenge
    Reason            string  // "expired", "invalid", "locked", "user_locked"
    RemainingAttempts *int
}
```

### Methods

#### Create

Creates a new challenge and stores it in Redis. Returns the challenge, the plaintext code (for sending), and any error.

```go
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*Challenge, string, error)
```

#### Verify

Verifies a code against a challenge. Returns a VerifyResult with the verification status.

```go
func (m *Manager) Verify(ctx context.Context, challengeID, code, clientIP string) (*VerifyResult, error)
```

#### Revoke

Revokes a challenge, removing it from storage.

```go
func (m *Manager) Revoke(ctx context.Context, challengeID string) error
```

#### IsUserLocked

Checks if a user is currently locked due to too many failed attempts.

```go
func (m *Manager) IsUserLocked(ctx context.Context, userID string) bool
```

#### Get

Retrieves a challenge by ID.

```go
func (m *Manager) Get(ctx context.Context, challengeID string) (*Challenge, error)
```

### Helper Functions

#### GenerateCode

Generates a random numeric code of specified length.

```go
func GenerateCode(length int) (string, error)
```

#### ValidateCodeFormat

Validates that a code matches the expected format (numeric, correct length).

```go
func ValidateCodeFormat(code string, length int) bool
```

## Security Features

- **Secure Code Generation**: Uses `crypto/rand` for cryptographically secure random number generation
- **Argon2 Hashing**: Codes are hashed using Argon2 before storage (never stored in plaintext)
- **Constant-Time Verification**: Code verification uses constant-time comparison to prevent timing attacks
- **One-Time Use**: Challenges are automatically deleted after successful verification
- **Automatic Expiration**: Challenges expire based on TTL (handled by Redis)
- **User Lockout**: Users are automatically locked after maximum failed attempts

## Dependencies

- `github.com/redis/go-redis/v9` - Redis client
- `github.com/soulteary/redis-kit` - Redis cache interface
- `github.com/soulteary/secure-kit` - Secure hashing and random generation

## License

Apache License 2.0
