# challenge-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/challenge-kit.svg)](https://pkg.go.dev/github.com/soulteary/challenge-kit)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/challenge-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/challenge-kit)

[中文文档](README_CN.md)

A Go library for managing OTP (one-time password) challenges: lifecycle,
code generation, Argon2 verification under a per-challenge lock, attempt
tracking and user lockout, all backed by Redis.

## Features

- **Challenge lifecycle**: create, verify, revoke, and swap the single active challenge
- **OTP generation**: numeric codes from `crypto/rand`
- **Verification**: Argon2 hashing with constant-time comparison
- **Atomic attempts**: concurrent wrong codes each consume exactly one attempt
- **Fail-closed**: a Redis failure never reports success
- **Bounded work**: concurrent Argon2 comparisons are capped so verification cannot exhaust memory
- **User lockout**: established once per challenge, never extended by probing
- **Purpose binding**: a challenge minted for one purpose cannot be redeemed for another
- **Privacy**: the active-index key is an irreversible digest, never raw PII

## Requirements

- **Go 1.27+** (`go.mod` declares `go 1.27.0`)
- Redis, via `github.com/redis/go-redis/v9`
- `github.com/soulteary/redis-kit` and `github.com/soulteary/secure-kit`

## Installation

```bash
go get github.com/soulteary/challenge-kit
```

## Quick Start

```go
package main

import (
    "context"
    "errors"
    "log"

    "github.com/redis/go-redis/v9"
    challenge "github.com/soulteary/challenge-kit"
)

func main() {
    redisClient := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
    defer redisClient.Close()

    manager := challenge.NewManager(redisClient, challenge.DefaultConfig())
    ctx := context.Background()

    ch, code, err := manager.Create(ctx, challenge.CreateRequest{
        UserID:      "user123",
        Channel:     challenge.ChannelEmail,
        Destination: "user@example.com",
        Purpose:     "login",
        ClientIP:    "127.0.0.1",
    })
    if err != nil {
        log.Fatal(err)
    }

    // Send `code` to the user over the chosen channel, then:
    result, err := manager.Verify(ctx, ch.ID, code, "127.0.0.1")
    switch {
    case errors.Is(err, challenge.ErrLockUnavailable):
        // Retryable. No attempt was consumed — do NOT report a failed code.
        return
    case errors.Is(err, challenge.ErrBackendUnavailable):
        // Redis is unhealthy; the manager failed closed.
        return
    case err != nil:
        log.Fatal(err)
    }

    if result.OK {
        // Authenticated. The challenge is already deleted.
        return
    }

    switch result.Reason {
    case challenge.ReasonInvalid:       // wrong code
    case challenge.ReasonExpired:       // challenge TTL elapsed
    case challenge.ReasonLocked:        // attempts exhausted; the user is now locked
    case challenge.ReasonUserLocked:    // the user was already locked
    case challenge.ReasonLockContention: // retryable, no attempt consumed
    case challenge.ReasonContextMismatch: // purpose/user/channel binding failed
    case challenge.ReasonBackendUnavailable:
    }
    if result.RemainingAttempts != nil {
        log.Printf("%d attempts left", *result.RemainingAttempts)
    }
}
```

## Usage

### Handling the verification outcome

`Verify` separates three things that callers must not conflate:

| Situation | Signal | Attempt consumed? | Retry? |
|-----------|--------|-------------------|--------|
| Wrong code | `Reason == ReasonInvalid` | yes | no, ask again |
| Attempts exhausted | `Reason == ReasonLocked` | yes | no, user is now locked |
| User already locked | `Reason == ReasonUserLocked` | no | after `LockoutDuration` |
| Lock contention | `ErrLockUnavailable`, `Reason == ReasonLockContention` | **no** | **yes, immediately** |
| Redis failure | `ErrBackendUnavailable`, `Reason == ReasonBackendUnavailable` | no | yes, with backoff |
| Binding mismatch | `Reason == ReasonContextMismatch` | yes | no |

`ErrLockUnavailable` is a momentary retry, not a verification failure. Treating
it as one consumes an attempt the user never spent, and telling them "your
account is locked" for it is simply wrong.

### Binding a challenge to its purpose

`VerifyWithOptions` checks the binding inside the per-challenge lock, before the
code is compared, so a code minted for `"login"` can never be redeemed for
`"password_reset"`.

```go
result, err := manager.VerifyWithOptions(ctx, challengeID, code, clientIP,
    challenge.VerifyOptions{
        ExpectedPurpose: "login",
        ExpectedUserID:  "user123",
        ExpectedChannel: challenge.ChannelEmail,
    })
// result.Reason == challenge.ReasonContextMismatch when the binding fails
```

Every field is optional; an empty one is not checked.

### Single active challenge (two-phase send)

`Create` stores a challenge as *pending*. Make it the active one only once the
provider has accepted the message, so a failed send never leaves a redeemable
code behind:

```go
ch, code, err := manager.Create(ctx, req)
if err != nil {
    return err
}

if err := smsProvider.Send(ch.Destination, code); err != nil {
    // Nothing was sent — remove the pending challenge and keep the old active one.
    _ = manager.RevokePending(ctx, ch.ID)
    return err
}

previousID, err := manager.SwapActive(ctx, ch)
if err != nil {
    return err
}
if previousID != "" {
    _ = manager.Revoke(ctx, previousID) // retire the challenge this one replaces
}
```

`SwapActive` is atomic and fails closed on a Redis error. Its index key is
derived from an irreversible digest of the identity, never the raw phone number
or email. `RevokePending` does not touch the active index; `Revoke` does.

### Lockout state

```go
if manager.IsUserLocked(ctx, "user123") {
    // Reject early, without spending a challenge.
}
```

A lockout is established at most once per challenge, recorded in
`Challenge.LockoutApplied`. Probing an exhausted challenge repeatedly cannot
extend it.

### Testing against the interface

`ManagerInterface` covers every `Manager` method, so a fake is easy to inject:

```go
type svc struct{ challenges challenge.ManagerInterface }
```

### Helpers

```go
code, err := challenge.GenerateCode(6)            // numeric, crypto/rand
ok := challenge.ValidateCodeFormat(code, 6)       // numeric and correct length
```

`GenerateCode` returns an error wrapping `ErrEntropyUnavailable` when the system
CSPRNG cannot be read, rather than returning a guessable code.

## Configuration

```go
config := challenge.DefaultConfig()
config.Expiry = 5 * time.Minute
config.MaxAttempts = 5
config.MaxConcurrentVerifications = 16

manager := challenge.NewManager(redisClient, config)
```

| Option | Default | Notes |
|--------|---------|-------|
| `Expiry` | `5m` | challenge TTL, enforced by Redis |
| `MaxAttempts` | `5` | wrong codes before lockout |
| `LockoutDuration` | `10m` | how long a locked user stays locked |
| `CodeLength` | `6` | 1–10 digits |
| `ChallengeKeyPrefix` | `"otp:ch:"` | Redis key prefix for challenges |
| `LockKeyPrefix` | `"otp:lock:"` | Redis key prefix for user lockouts |
| `VerifyLockPrefix` | `"otp:vlock:"` | Redis key prefix for the per-challenge lock |
| `VerifyLockTTL` | `5s` | lease held while verifying |
| `VerifyLockWait` | `2s` | total time spent waiting for the lock |
| `VerifyLockRetry` | `25ms` | interval between lock attempts |
| `ActiveIndexPrefix` | `"otp:active:"` | Redis key prefix for the active-challenge index |
| `MaxConcurrentVerifications` | `16` | concurrent Argon2 comparisons |

`DefaultConfig()` fills every field with the value a `Manager` actually uses, so
reading one back gives a real default rather than a zero.

**Sizing `MaxConcurrentVerifications`**: each in-flight verification holds the
Argon2 memory cost — 64 MiB at the library default — so the cap is also a memory
cap. The default of 16 bounds verification at roughly 1 GiB. Raise it only if
you have the headroom for `MaxConcurrentVerifications × Argon2 memory`.

## API Reference

### Manager

| Method | Description |
|--------|-------------|
| `Create(ctx, req)` | Store a pending challenge; returns it plus the plaintext code |
| `Verify(ctx, id, code, clientIP)` | Verify a code |
| `VerifyWithOptions(ctx, id, code, clientIP, opts)` | Verify with purpose/user/channel binding |
| `Get(ctx, id)` | Fetch a challenge |
| `Revoke(ctx, id)` | Delete a challenge and clear the active index |
| `RevokePending(ctx, id)` | Delete a challenge that failed to send; leaves the index |
| `SwapActive(ctx, ch)` | Make `ch` active; returns the previously active ID |
| `IsUserLocked(ctx, userID)` | Whether the user is currently locked out |

### Types

```go
type Challenge struct {
    ID             string
    UserID         string
    Channel        Channel // "sms" | "email" | "dingtalk"
    Destination    string  // raw phone/email — see Security Notes
    CodeHash       string
    Purpose        string
    ExpiresAt      time.Time
    Attempts       int
    MaxAttempts    int
    CreatedIP      string
    CreatedAt      time.Time
    LockoutApplied bool // this challenge has already caused a lockout
}

type CreateRequest struct {
    UserID      string
    Channel     Channel
    Destination string
    Purpose     string
    ClientIP    string
}

type VerifyOptions struct {
    ExpectedPurpose string
    ExpectedUserID  string
    ExpectedChannel Channel
}

type VerifyResult struct {
    OK                bool
    Challenge         *Challenge
    Reason            string // one of the Reason* constants
    RemainingAttempts *int
}
```

### Reasons

| Constant | Value |
|----------|-------|
| `ReasonInvalid` | `"invalid"` |
| `ReasonExpired` | `"expired"` |
| `ReasonLocked` | `"locked"` |
| `ReasonUserLocked` | `"user_locked"` |
| `ReasonLockContention` | `"lock_contention"` |
| `ReasonContextMismatch` | `"context_mismatch"` |
| `ReasonBackendUnavailable` | `"backend_unavailable"` |

### Errors

| Sentinel | Meaning |
|----------|---------|
| `ErrLockUnavailable` | The per-challenge lock could not be taken within the budget. Retryable; **no attempt consumed**. Never treat it as a verification failure, and never fall back to a non-atomic path. |
| `ErrBackendUnavailable` | A required Redis operation failed. The manager fails closed and never reports OK on an unhealthy store. |
| `ErrEntropyUnavailable` | The system CSPRNG could not be read. Returned instead of issuing a guessable ID or code. |

Match them with `errors.Is`.

## Upgrade Notes (v1.7.0)

This release changes what callers are told about a failed verification, and caps
the work a verification can do. `Config` and `Challenge` each gained a field; no
API was removed.

- **`Reason` no longer conflates retry with lockout.** Lock contention
  (retryable, no attempt consumed) and attempt exhaustion (terminal, the account
  is locked) both reported `"locked"`. If you switch on `Reason`, add a
  `ReasonLockContention` case — otherwise a momentary retry is still being shown
  to the user as "your account is locked". The reasons are now exported
  constants; the string values of the existing ones are unchanged.
- **A user lockout can no longer be extended by polling.** Every probe of an
  exhausted challenge called `lockUser` again, pushing the deadline out by
  another `LockoutDuration` — so anyone holding a challenge ID could keep a user
  locked out indefinitely. The lockout is established if missing and never
  refreshed, tracked by the new `Challenge.LockoutApplied`. Stored challenges
  written by an earlier release simply default to `false`.
- **Concurrent Argon2 work is bounded.** Verifications of *different*
  challenges were unbounded, and each holds the Argon2 memory cost (64 MiB by
  default), so 100 in flight was 6.4 GiB — a memory-exhaustion vector for anyone
  who can call `Verify` with distinct challenge IDs. `MaxConcurrentVerifications`
  (default 16) now caps it. Under saturation, callers wait; a caller whose
  context is cancelled while waiting gets a retryable result instead of a
  consumed attempt.
- **`Create` can now fail on entropy.** `generateChallengeID` discarded the
  error from its fallback draw, and since both draws come from `crypto/rand` they
  fail together — leaving an empty token and a slice-bounds panic in a function
  documented as handling the case gracefully. It returns an error wrapping
  `ErrEntropyUnavailable` now, and `Create` propagates it rather than issuing a
  guessable identifier. Handle an error from `Create` you may previously have
  treated as impossible.
- **Cancellation survives in the error chain.** `ErrLockUnavailable` used to
  fold the context error in with `%v`, so `errors.Is(err, context.Canceled)` was
  false and callers retried work they had already abandoned. It wraps with `%w`
  now.
- **A backend error is no longer mistaken for an expiry.** A cache miss was
  detected by matching the error *text*, because redis-kit v1.5.0 reported one as
  a plain `fmt.Errorf` that `errors.Is` could not see through. A backend error
  that merely mentioned "key not found" therefore became a reported
  `ReasonExpired` — which consumes no attempt, handing back a free probe during an
  infrastructure fault. This release depends on redis-kit v1.6.0 and classifies a
  miss with `errors.Is` against `cache.ErrKeyNotFound` and `redis.Nil`, so an
  unhealthy backend now fails closed with `ReasonBackendUnavailable` as intended.
- **`DefaultConfig()` returns real defaults for every field.** `VerifyLock*`,
  `ActiveIndexPrefix` and `MaxConcurrentVerifications` came back as zero values
  even though `NewManager` normalised them internally. If you built a `Config`
  by copying `DefaultConfig()` and overriding a couple of fields, you now get the
  documented values instead of zeros.

## Security Notes

- **`Challenge.Destination` is stored in Redis in the clear.** The active-index
  key is deliberately an irreversible digest so no raw identifier appears in a
  key, but the challenge *value* still holds the phone number or email — anyone
  with read access to the instance can enumerate destinations. Keep Redis
  access-controlled and `Expiry` short.
- **Codes are never stored in plaintext**: Argon2 hashed, compared in constant
  time.
- **One-time use**: a challenge is deleted before success is returned, and a
  failed delete never reports OK.
- **Fail-closed**: on any Redis failure the manager returns
  `ErrBackendUnavailable` rather than falling back to a non-atomic local path.
- **Atomic attempts**: concurrent wrong codes each increment the counter exactly
  once; no lost updates.

## Testing

```bash
go test ./...

# With coverage
go test ./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
```

## Dependencies

- `github.com/redis/go-redis/v9` — Redis client
- `github.com/soulteary/redis-kit` — Redis cache and lock interfaces
- `github.com/soulteary/secure-kit` — Argon2 hashing and secure random

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
