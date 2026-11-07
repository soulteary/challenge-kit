// Package challenge issues and verifies one-time codes (OTP) backed by Redis.
//
// A challenge is minted by [Manager.Create], which returns the plaintext code
// exactly once -- only an Argon2 hash of it is stored -- and redeemed by
// [Manager.Verify]. A lease lock coordinates expensive verification work; it
// can expire during a process pause. Atomic Redis comparisons of the exact
// record snapshot protect one-time consumption and incorrect-attempt updates.
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
// Compatibility-mode verification uses single-key operations. Strict activation
// uses a multi-key Lua script: its challenge and active-index prefixes must use
// the same Redis Cluster hash tag. See the README strict-activation migration.
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
// Config.RequireActiveVerification is opt-in and defaults to false. Enable it
// to make Create persist pending, unverifiable state until [Manager.SwapActive]
// succeeds after provider acceptance. Use a disjoint namespace and replace all
// verifiers before sharing strict records. Older readers ignore activation state.
// On send failure use [Manager.RevokePending]; after an uncertain activation
// reply it leaves committed active records intact. Redis script errors do not
// roll back earlier writes; see README recovery and TTL limits. Compatibility
// mode preserves direct Create-to-Verify callers and has no pending-state gate.
//
// # Dependencies
//
// Redis access goes through go-redis, and code hashing through secure-kit's
// passwd subpackage. There is no adapter subpackage to import: unlike a health
// probe, neither storage nor hashing is optional here -- a manager with no
// Redis is not a manager, and the point of the challenge is that the code is
// hashed -- so splitting either one out would save nobody anything.
package challenge
