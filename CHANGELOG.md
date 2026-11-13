# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Because Go encodes the major version in the import path, a major release would
also change the module path. The current one is
`github.com/soulteary/challenge-kit`, with no suffix, and this release keeps it.

## [Unreleased]

## [1.12.0] - 2026-11-13

### Added

- `Config.LockoutThreshold`: the number of challenges a user must exhaust
  within `LockoutDuration` before the user is locked. The default, `1`, locks
  on the first exhausted challenge as before. With a higher value one
  exhausted challenge is refused with `ReasonLocked` but no longer locks the
  user out of every other challenge, so a party that can submit codes for one
  challenge cannot keep its user locked. Exhausted challenges are recorded
  under `Config.LockCounterPrefix` (default `otp:lockcount:`), each counted
  once, in a window that starts with the first one. A successful verification
  does not clear the record.
- `Config.LockIdentity`: maps a user ID to the identifier used in the lock and
  counter keys, so the raw user ID need not appear in Redis key names. The
  default uses the user ID, as before. Setting or changing it abandons the
  locks stored under the previous identifiers; they expire within
  `LockoutDuration`.
- `Manager.IsUserLockedE` and the `UserLockChecker` interface: report a user
  lock together with the backend error. `IsUserLocked` keeps answering `true`
  on a Redis error.

The defaults keep the lock behaviour, key names and TTLs of v1.11.2.
`ManagerInterface` is unchanged.

## [1.11.2] - 2026-11-13

### Changed

- Require Go 1.27.4, which carries security fixes to cgo, the go command, the
  compiler, the linker and `net/http`.
- Select `secure-kit/v2` v2.1.5, which updates `golang.org/x/crypto` to v0.59.0.

The module path and the public API are unchanged. Stored challenge formats, Redis key
formats and TTLs are unchanged.

## [1.11.1] - 2026-11-08

### Changed

- Update `secure-kit/v2` from v2.1.3 to v2.1.4 without changing challenge hashes,
  public APIs, Redis formats, activation defaults or TTLs.

### Tests

- Count committed wrong-code outcomes in the concurrent-attempt regression,
  retry only documented lock contention within a bounded context, and assert
  the stored counter and actual Redis lock. Select a guaranteed incorrect code.
  Production locking, wait settings and CI thresholds are unchanged.

### Documentation

- Clarify strict activation defaults, disjoint namespaces and Redis Cluster
  co-location, snapshot comparisons versus expiring leases, pending and binding
  outcomes, backend indeterminacy and ordinary negative results with errors.

See [release notes](docs/releases/v1.11.1.md) for upgrade and rollback.


## [1.11.0] — 2026-11-07

### Added

- Add opt-in `Config.RequireActiveVerification`. Strict `Create` persists a
  pending record; `Verify` returns `ReasonNotActive` until `SwapActive` promotes
  it. The default remains `false`, preserving direct `Create` → `Verify` use.
- Expose activation state on challenge records. Strict verification binds both
  consumption and incorrect-attempt updates to the exact active snapshot, so a
  stale verifier cannot restore a revoked or replaced record.
- Run real Redis activation partial-write and lease-expiry regressions in CI.

### Fixed

- Preserve the remaining challenge TTL during activation and atomically replace
  the previous active challenge within the activation script.
- Make activation retries return an empty previous ID when the index already
  names the requested challenge, including recovery from a partial index write.
  Callers may continue revoking a nonempty returned previous ID.
- Write the active index before marking the record active. Redis Lua runtime
  errors retain earlier writes; a failure before the final record write now
  leaves the new challenge pending and unverifiable.
- Keep `RevokePending` from deleting committed active records after a lost
  activation reply. Use `Revoke` for explicit revocation of an active record.

### Compatibility and operations

- Public function signatures and the module import path remain unchanged.
  Existing callers retain compatibility mode unless they enable strict mode.
- Use a strict record prefix disjoint from the historical prefix, such as
  `otp2:ch:` instead of `otp:ch:v2:`. Older verifiers accept arbitrary ID suffixes
  and ignore activation state; extending their prefix does not isolate records.
  Do not let older binaries read the strict namespace or fall back to old keys.
- When adopting strict mode, stop creation, drain requests, wait the previous
  maximum challenge lifetime, and replace all verifiers before resuming.
  Preserve unresolved delivery/idempotency records and reconcile them before
  resending. Strict verification intentionally rejects unmarked legacy records.
- Strict activation is a multi-key Redis script. Challenge and active-index
  prefixes must use the same hash tag in Redis Cluster deployments.
- Partial activation errors may delete the previous record or leave an index
  pointing at a pending record; availability is not rolled back. Correct the
  backend error before retrying, or clean up pending records and allow stale
  entries to expire with their original TTL.
- Upgrading does not repair active orphan records created by an earlier partial
  activation failure. Affected deployments must wait their original TTL or
  explicitly revoke known IDs. The write-order fix itself requires no further
  namespace migration.

## [1.10.4] — 2026-11-01

### Changed

- Update `secure-kit/v2` from 2.1.2 to 2.1.3 within the existing major version.
- Separate binding validation, lockout handling, and incorrect-code persistence
  without changing verification decisions, attempt counts, lockout ordering,
  remaining TTL, atomic snapshot comparisons, or error reasons.
- Split complex fault-injection and Redis lease-expiry test phases while
  retaining the existing assertions. Regenerate complete Go quality evidence
  with all six checks at 100% and no issues. No hash-format or API change.

## [1.10.3]

### Fixed

- Compare the exact Redis snapshot atomically before consuming or updating a
  challenge. A verifier paused beyond its lock lease cannot accept a consumed,
  revoked, or expired challenge, overwrite a newer attempt count, or recreate a
  revoked record. Attempt and lockout-state updates preserve the current TTL.
- Add Linux regression coverage for stale snapshots, hash storage, and real
  Redis lease expiry. No hash-format or public API change is required.

## [1.10.0] — 2026-09-21

### Dependencies

- **`secure-kit` v2.0.0 → v2.1.0.** A refresh inside the same major, with no
  API change: v2.1.0 only drops testify from secure-kit's own tests, and the
  `passwd.Argon` hasher this package builds on is untouched. Nothing was ever
  held back by the old floor — a consumer that asks for v2.1.0 itself already
  builds against it, because minimal version selection takes the highest
  version anything in the graph asks for. What was stale is this module's own
  floor, and the version its tests run against.

  The module graph loses an entry and `go.sum` gains two lines, which is worth
  writing down because it is the opposite of what it sounds like:

  | | v2.0.0 | v2.1.0 |
  |---|---|---|
  | modules in `go list -m all` | 23 | 22 |
  | `go.sum` lines | 28 | 30 |

  secure-kit v2.0.0 asked for testify v1.12.1, which was the highest request in
  the graph and therefore the one selected. With it gone, the only module still
  asking for testify is `go.uber.org/atomic` v1.11.0 — for its own tests,
  reached through go-redis's connection pool — and it asks for v1.3.0. So
  selection falls to that: `go.yaml.in/yaml/v3` leaves along with testify
  v1.12.1, and `go-spew` and `go-difflib`, which testify v1.3.0 still needs,
  get checksums again. The 1.9.0 notes credited secure-kit v2 with dropping
  those two; they were held back by the newer testify it happened to bring,
  not by secure-kit.

  None of this reaches a build. No package here imports testify, it is a test
  dependency of a dependency, and `go build ./...` links exactly the same
  packages either way.

## [1.9.0] — 2026-09-21

### Added

- **`RedisClient`**, the seven commands the manager issues:
  `Del`, `Eval`, `Exists`, `Get`, `Set`, `SetArgs` and `TTL`. `*redis.Client`,
  `*redis.ClusterClient`, `*redis.Ring` and `redis.UniversalClient` all satisfy
  it, so a Cluster or Sentinel deployment can use this package — before, a
  `*redis.Client` parameter shut every deployment but standalone out.

  Naming the commands rather than a whole client also means a caller's own
  wrapper works: metrics, tracing, or a fake with no server behind it.
  `TestAWrapperIsEnoughToInstrumentTheManager` asserts the manager issues
  exactly these seven and no others, and `TestEveryCommandIsSingleKey` asserts
  each one names a single key — which is what lets a cluster place the
  challenge, user-lock, verification-lock and active-index keys in different
  slots.

- **`ErrNotFound`**, the sentinel for a key that is absent or expired as
  opposed to a backend that failed. The distinction decides whether a
  verification is answered `ReasonExpired` (terminal, consuming nothing) or
  fails closed, so it is carried by a sentinel rather than by the error text.
  A miss still satisfies `errors.Is(err, redis.Nil)`.

- **`ErrNilClient`**, reported by every operation when the manager was built
  without a usable client.

- Runnable examples (`Example`, `ExampleManager_VerifyWithOptions`,
  `ExampleManager_SwapActive`, `ExampleNewManager_cluster`,
  `ExampleNewManager_sentinel`, `ExampleDefaultConfig`,
  `ExampleErrLockUnavailable`) that `go test` verifies, so they cannot drift
  from the API. The cluster and Sentinel ones carry no `Output:` comment — they
  need a real deployment — but they are compiled.

- A package doc in `doc.go` covering the fail-closed contract, which clients
  are accepted, the four key prefixes and the two-phase send.

- `CHANGELOG.md`.

### Changed

- **`NewManager` takes `RedisClient` instead of `*redis.Client`.** Passing a
  `*redis.Client` still compiles, so this is not a breaking change for ordinary
  callers and the module path is unchanged. Code that stores `NewManager` in a
  variable of type `func(*redis.Client, Config) *Manager` is the exception, and
  has to widen that type too.

- **The `redis-kit` dependency is gone.** Its cache is what forced the
  concrete `*redis.Client`: `cache.NewCache` takes one, and that is precisely
  the type a Cluster or Sentinel user cannot supply. The ~120 lines it provided
  (JSON value under a key prefix, with a TTL) now live in `store.go`, against
  `RedisClient`.

  A deprecated shim was not an option, for the reason a shim never is: it would
  have to import `redis-kit` to name its types, which puts the module straight
  back in the graph.

- **`secure-kit` v1.6.0 → v2.0.0**, whose Argon2 hasher moved to the `passwd`
  subpackage. This package hashes codes, so it follows it there; nothing about
  the hashing changed. Verified in both directions rather than taken on trust:
  a hash written by v1.6.0 verifies under v2.0.0 and vice versa, so a rolling
  deploy with both versions running does not invalidate in-flight challenges.

  None of this is visible to callers — no secure-kit type appears in this
  package's API.

- A cache miss no longer satisfies `errors.Is(err, rediskitcache.ErrKeyNotFound)`.
  Use `challenge.ErrNotFound`, or `redis.Nil`, which a miss still matches. The
  error message is unchanged (`key not found: <key>`).

### Measured effect

For a program whose only import is this package, built with `-trimpath` on
linux/amd64 with Go 1.27.0:

| | v1.8.0 | v1.9.0 |
|---|---|---|
| modules in `go list -m all` | 22 | 21 |
| `go.sum` lines | 34 | 28 |
| `// indirect` requirements in the consumer's `go.mod` | 6 | 5 |
| linked packages | 205 | 205 |
| binary size | 10,757,881 bytes | 10,765,144 bytes |

The honest summary: this buys a smaller dependency graph, not a smaller binary.
`redis-kit` leaves it outright; the other four `go.sum` lines go because
secure-kit v2 brings a testify that no longer drags `go-spew`, `go-difflib` and
`gopkg.in/yaml.v3` behind it. The same amount of code is linked either way, and
moving the store in-tree costs 7,263 bytes (+0.07%).

go-redis and `golang.org/x/crypto` both stay, because unlike a health probe,
neither storage nor hashing is optional here: a manager with no Redis is not a
manager, and the whole point of the challenge is that the code is hashed. That
is also why there is no adapter subpackage to import — splitting one out would
save nobody anything.

### Fixed

- **A nil client no longer panics.** With a concrete `*redis.Client` parameter,
  an unassigned field was caught by the cache's `client == nil` check. Inside an
  interface that check silently stops working — a typed nil is not `== nil` —
  and every command would dereference it, turning a misconfiguration into a
  downed process. `NewManager` detects both forms through `reflect` and every
  entry point fails closed instead: `Verify` returns `ErrBackendUnavailable`
  with `ReasonBackendUnavailable`, and `IsUserLocked` reports the user as
  locked, because an unusable backend must never read as "not locked".

### Dependencies

Everything is now on its latest published release: `go-redis` v9.22.0
(v9.23.0-beta.1 exists and is a beta), `secure-kit` v2.0.0, and `miniredis`
v2.39.0 for tests.

### CI

- The dependabot config this release adds opened its first pull request
  straight away, and it is in the release: five workflow actions were behind,
  `actions/checkout` in the Go Report Card workflow by three major versions.

  | Action | From | To |
  |---|---|---|
  | `actions/checkout` | v6, and v4 in the Go Report Card workflow | v7 |
  | `actions/setup-go` | v6 | v7 |
  | `actions/upload-artifact` | v6 | v7 |
  | `codecov/codecov-action` | v5 | v7 |
  | `soulteary/goreportcard-action` | v1.0.0 | v1.1.2 |

  Nothing a consumer imports changes — a workflow file is never compiled, even
  though it does travel inside the module zip. They are listed because the
  release gate this version also adds is itself a workflow: an action stale
  enough to stop running would take the tag check down with it, silently.

## [1.8.0]

Dependency refresh only: `miniredis` v2.36.1 → v2.39.0. No API change.

## [1.7.0]

Reasons split retryable lock contention from terminal attempt exhaustion; a
lockout can no longer be extended by polling; concurrent Argon2 work is bounded
by `MaxConcurrentVerifications`; `Create` reports `ErrEntropyUnavailable`
instead of panicking; cancellation survives in the error chain; a backend error
is no longer misread as an expiry; `DefaultConfig()` returns real defaults for
every field. See the upgrade notes in [README.md](README.md) for the details
and what each one requires of a caller.

## Earlier releases

v1.0.0 through v1.6.0 predate this file; see the commit history.

[Unreleased]: https://github.com/soulteary/challenge-kit/compare/v1.12.0...HEAD
[1.12.0]: https://github.com/soulteary/challenge-kit/compare/v1.11.2...v1.12.0
[1.11.2]: https://github.com/soulteary/challenge-kit/compare/v1.11.1...v1.11.2
[1.11.1]: https://github.com/soulteary/challenge-kit/compare/v1.11.0...v1.11.1
[1.11.0]: https://github.com/soulteary/challenge-kit/compare/v1.10.4...v1.11.0
[1.10.4]: https://github.com/soulteary/challenge-kit/compare/v1.10.3...v1.10.4
[1.10.0]: https://github.com/soulteary/challenge-kit/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/soulteary/challenge-kit/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/soulteary/challenge-kit/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/soulteary/challenge-kit/compare/v1.6.0...v1.7.0
[1.10.3]: https://github.com/soulteary/challenge-kit/compare/v1.10.2...v1.10.3
