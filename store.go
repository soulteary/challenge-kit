package challenge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient is the slice of a go-redis client this package uses.
//
// *redis.Client, *redis.ClusterClient, *redis.Ring and redis.UniversalClient
// all satisfy it, so the same manager works against a standalone server, a
// Sentinel failover setup, a cluster and a ring. It used to take *redis.Client
// outright, which shut every deployment but the first one out.
//
// Every command below is issued against a SINGLE key, so nothing here needs
// keys to share a hash slot: a cluster can spread the challenge, lock, verify
// lock and active-index keys wherever it likes.
//
// Taking an interface is also what makes the manager testable without a
// server: anything with these seven methods will do.
type RedisClient interface {
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	SetArgs(ctx context.Context, key string, value interface{}, a redis.SetArgs) *redis.StatusCmd
	TTL(ctx context.Context, key string) *redis.DurationCmd
}

// Compile-time proof of the claim in RedisClient's doc comment. A go-redis
// release that changed one of these signatures would break this file rather
// than a user's build.
var (
	_ RedisClient = (*redis.Client)(nil)
	_ RedisClient = (*redis.ClusterClient)(nil)
	_ RedisClient = (*redis.Ring)(nil)
	_ RedisClient = (redis.UniversalClient)(nil)
)

// ErrNotFound reports that a key is absent or has expired, as opposed to a
// backend failure. The distinction decides whether a verification is answered
// "expired" (terminal, consuming nothing) or fails closed, so it is carried by
// a sentinel rather than by the error text.
//
// The error returned for a miss also satisfies errors.Is(err, redis.Nil), so
// callers that already classified misses that way keep working.
var ErrNotFound = errors.New("challenge: key not found")

// ErrNilClient reports that the manager was built without a usable Redis
// client. Every store operation returns it instead of dereferencing the nil,
// so a misconfigured manager fails closed rather than panicking.
var ErrNilClient = errors.New("challenge: redis client is nil")

// notFoundError reports a missing key. It keeps the "key not found: <key>"
// message redis-kit's cache used, and errors.Is classifies it as both
// ErrNotFound and redis.Nil.
type notFoundError struct{ key string }

func (e notFoundError) Error() string { return "key not found: " + e.key }

func (e notFoundError) Is(target error) bool {
	return target == ErrNotFound || target == redis.Nil
}

// store is the key/value slice of Redis the manager needs: JSON values under a
// fixed prefix, with a TTL. It is an interface so tests can wrap an operation,
// and so the manager never reaches for a command it has not declared here.
type store interface {
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Get(ctx context.Context, key string, dest interface{}) error
	Del(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
}

// redisStore is the go-redis implementation of store.
//
// This is deliberately a handful of lines in this package rather than a
// dependency: redis-kit's cache takes a *redis.Client, and that concrete type
// is exactly what a Cluster or Sentinel user cannot supply.
type redisStore struct {
	client    RedisClient
	keyPrefix string
}

// newRedisStore creates a store writing JSON values under keyPrefix.
//
// A client that is nil -- including a typed nil such as an unassigned
// *redis.Client field -- is normalised to an untyped nil, so operations report
// ErrNilClient instead of panicking on a nil dereference.
func newRedisStore(client RedisClient, keyPrefix string) *redisStore {
	if isNilClient(client) {
		client = nil
	}
	return &redisStore{client: client, keyPrefix: keyPrefix}
}

// buildKey constructs the full key with prefix.
func (s *redisStore) buildKey(key string) string {
	if s.keyPrefix == "" {
		return key
	}
	return s.keyPrefix + key
}

// Set stores a JSON-encoded value with the given TTL.
func (s *redisStore) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	if s.client == nil {
		return ErrNilClient
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal value: %w", err)
	}

	if err := s.client.Set(ctx, s.buildKey(key), data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set cache: %w", err)
	}

	return nil
}

// Get reads a JSON-encoded value into dest. A missing key returns an error
// satisfying both ErrNotFound and redis.Nil; everything else is a backend
// failure and must be treated as one.
func (s *redisStore) Get(ctx context.Context, key string, dest interface{}) error {
	if s.client == nil {
		return ErrNilClient
	}

	data, err := s.client.Get(ctx, s.buildKey(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return notFoundError{key: key}
	}
	if err != nil {
		return fmt.Errorf("failed to get cache: %w", err)
	}

	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("failed to unmarshal value: %w", err)
	}

	return nil
}

// Del removes a key. Deleting an absent key is not an error.
func (s *redisStore) Del(ctx context.Context, key string) error {
	if s.client == nil {
		return ErrNilClient
	}
	return s.client.Del(ctx, s.buildKey(key)).Err()
}

// Exists reports whether a key is present.
func (s *redisStore) Exists(ctx context.Context, key string) (bool, error) {
	if s.client == nil {
		return false, ErrNilClient
	}

	count, err := s.client.Exists(ctx, s.buildKey(key)).Result()
	if err != nil {
		return false, fmt.Errorf("failed to check existence: %w", err)
	}

	return count > 0, nil
}

// TTL returns the remaining lifetime of a key. As with the TTL command itself,
// a key that is missing or carries no expiry reports a negative duration rather
// than an error; callers that must not resurrect such a key check for that.
func (s *redisStore) TTL(ctx context.Context, key string) (time.Duration, error) {
	if s.client == nil {
		return 0, ErrNilClient
	}

	ttl, err := s.client.TTL(ctx, s.buildKey(key)).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get TTL: %w", err)
	}

	return ttl, nil
}

// isNilClient reports whether there is no client to call.
//
// RedisClient is an interface, so a plain c == nil misses the case that
// actually reaches here: a typed nil, such as an unassigned *redis.Client
// field or one from a constructor that returned early. Calling a command on
// that panics, and taking down the process is a worse answer than failing the
// request closed.
func isNilClient(c RedisClient) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
