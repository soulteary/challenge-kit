// Package challenge issues and verifies one-time codes (OTP) backed by Redis.
//
// A challenge is minted by [Manager.Create], which returns the plaintext code
// exactly once -- only an Argon2 hash of it is stored -- and redeemed by
// [Manager.Verify]. Verification is atomic: the read-modify-write cycle runs
// under a per-challenge distributed lock, so a correct code can be consumed at
// most once and concurrent wrong codes each cost exactly one attempt.
//
// # Failing closed
//
// Every answer this package gives is one of three things: verified, rejected,
// or unknown. It never turns the third into the first. A Redis failure is
// reported as [ErrBackendUnavailable] with reason [ReasonBackendUnavailable],
// lock contention as [ErrLockUnavailable] with reason [ReasonLockContention]
// (retryable, consuming no attempt), and [Manager.IsUserLocked] reports a user
// as locked when it cannot tell. Callers switch on VerifyResult.Reason, so
// those reasons are part of the API.
//
// # Which Redis
//
// [NewManager] takes [RedisClient], the seven commands the manager issues, not
// a concrete client. *redis.Client, *redis.ClusterClient, *redis.Ring and
// redis.UniversalClient all satisfy it, so a standalone server, a Sentinel
// failover setup, a cluster and a ring are all usable -- as is a caller's own
// wrapper, for metrics, tracing or a fake in tests.
//
// Every command names a single key, so nothing here requires keys to share a
// hash slot; a cluster may place the challenge, user-lock, verification-lock
// and active-index keys wherever it likes.
//
// A nil client, including a typed nil such as an unassigned *redis.Client
// field, is tolerated rather than dereferenced: operations fail closed instead
// of panicking.
//
// # Key layout
//
// Four prefixes, all configurable on [Config]:
//
//	otp:ch:<challenge id>     the challenge itself, JSON, TTL = Config.Expiry
//	otp:lock:<user id>        the user lockout, TTL = Config.LockoutDuration
//	otp:vlock:<challenge id>  the verification mutex, TTL = Config.VerifyLockTTL
//	otp:active:<digest>       the single active challenge for an identity
//
// The active-index key holds an irreversible SHA-256 digest of the identity
// (user, purpose, channel, destination), so no raw identifier is ever written
// into a key name. The challenge VALUE still holds the destination in the
// clear; keep the instance access-controlled and Config.Expiry short.
//
// # Two-phase send
//
// Create stores a challenge as pending. Hand the code to your provider, then
// call [Manager.SwapActive] to make it the active challenge for its identity;
// it returns the challenge it displaced, which you can revoke. If the send
// failed, call [Manager.RevokePending] instead, so a code nobody received is
// never redeemable.
//
// # Dependencies
//
// Redis access goes through go-redis, and code hashing through secure-kit's
// passwd subpackage. There is no adapter subpackage to import: unlike a health
// probe, neither storage nor hashing is optional here -- a manager with no
// Redis is not a manager, and the point of the challenge is that the code is
// hashed -- so splitting either one out would save nobody anything.
package challenge
