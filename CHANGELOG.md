# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Because Go encodes the major version in the import path, a major release would
also change the module path. The current one is
`github.com/soulteary/challenge-kit`, with no suffix, and this release keeps it.

## [Unreleased]

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

[Unreleased]: https://github.com/soulteary/challenge-kit/compare/v1.9.0...HEAD
[1.9.0]: https://github.com/soulteary/challenge-kit/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/soulteary/challenge-kit/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/soulteary/challenge-kit/compare/v1.6.0...v1.7.0
