package challenge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestStoreRoundTrip(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()

	s := newRedisStore(client, "test:ch:")
	ctx := context.Background()

	in := Challenge{ID: "ch_1", UserID: "u1", Channel: ChannelEmail, Attempts: 2}
	if err := s.Set(ctx, "ch_1", in, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The prefix is part of the key layout callers see in Redis, so it is
	// pinned rather than inferred.
	if !mr.Exists("test:ch:ch_1") {
		t.Fatalf("stored under the wrong key; keys = %v", mr.Keys())
	}

	var out Challenge
	if err := s.Get(ctx, "ch_1", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out != in {
		t.Errorf("round-tripped %+v, want %+v", out, in)
	}

	ok, err := s.Exists(ctx, "ch_1")
	if err != nil || !ok {
		t.Errorf("Exists = %t, %v; want true, nil", ok, err)
	}

	ttl, err := s.TTL(ctx, "ch_1")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		t.Errorf("TTL = %s, want (0, 1m]", ttl)
	}

	if err := s.Del(ctx, "ch_1"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if ok, err := s.Exists(ctx, "ch_1"); err != nil || ok {
		t.Errorf("Exists after Del = %t, %v; want false, nil", ok, err)
	}
	// Deleting what is already gone is not an error.
	if err := s.Del(ctx, "ch_1"); err != nil {
		t.Errorf("Del on an absent key = %v, want nil", err)
	}
}

func TestStoreWithoutAPrefix(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()

	s := newRedisStore(client, "")
	if err := s.Set(context.Background(), "bare", Challenge{ID: "ch_1"}, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !mr.Exists("bare") {
		t.Errorf("an empty prefix must leave the key untouched; keys = %v", mr.Keys())
	}
}

// A miss and a backend failure are the two answers the manager must never
// confuse: the first consumes no attempt, the second has to fail closed.
func TestStoreMissIsDistinguishableFromFailure(t *testing.T) {
	mr := miniredis.RunT(t)

	// Short timeouts and no retries: the second half of this test talks to a
	// server that is gone, and waiting out go-redis's default backoff would
	// put seconds on every run of the suite.
	client := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		MaxRetries:   -1,
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })

	s := newRedisStore(client, "test:")
	ctx := context.Background()

	var out Challenge
	err := s.Get(ctx, "absent", &out)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a miss must satisfy ErrNotFound, got %v", err)
	}
	if !errors.Is(err, redis.Nil) {
		t.Errorf("a miss must still satisfy redis.Nil, got %v", err)
	}
	if !isNotFound(err) {
		t.Errorf("isNotFound on a real miss = false (%v)", err)
	}

	// Now take the server away. The same call must NOT read as a miss.
	mr.Close()
	err = s.Get(ctx, "absent", &out)
	if err == nil {
		t.Fatal("reading from a dead server returned no error")
	}
	if isNotFound(err) {
		t.Errorf("a dead backend classified as a miss: %v", err)
	}
	if _, err := s.Exists(ctx, "absent"); err == nil {
		t.Error("Exists against a dead server returned no error")
	}
	if _, err := s.TTL(ctx, "absent"); err == nil {
		t.Error("TTL against a dead server returned no error")
	}
	if err := s.Set(ctx, "absent", Challenge{}, time.Minute); err == nil {
		t.Error("Set against a dead server returned no error")
	}
}

// TTL reports a negative duration for a key that is missing or immortal rather
// than an error; the manager relies on that to refuse resurrecting one.
func TestStoreTTLOfMissingAndImmortalKeys(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()

	s := newRedisStore(client, "test:")
	ctx := context.Background()

	ttl, err := s.TTL(ctx, "absent")
	if err != nil {
		t.Fatalf("TTL of an absent key: %v", err)
	}
	if ttl > 0 {
		t.Errorf("TTL of an absent key = %s, want <= 0", ttl)
	}

	if err := s.Set(ctx, "immortal", Challenge{ID: "ch_1"}, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ttl, err = s.TTL(ctx, "immortal")
	if err != nil {
		t.Fatalf("TTL of a key without an expiry: %v", err)
	}
	if ttl > 0 {
		t.Errorf("TTL of a key without an expiry = %s, want <= 0", ttl)
	}
}

func TestStoreRejectsUnreadableValues(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()

	// Something other than this package wrote the key.
	if err := mr.Set("test:junk", "not json"); err != nil {
		t.Fatal(err)
	}

	s := newRedisStore(client, "test:")
	var out Challenge
	err := s.Get(context.Background(), "junk", &out)
	if err == nil {
		t.Fatal("reading a non-JSON value returned no error")
	}
	if isNotFound(err) {
		t.Errorf("an unreadable value classified as a miss: %v", err)
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("err = %v, want it to name the unmarshal failure", err)
	}
}

func TestStoreWithoutAClientFailsClosed(t *testing.T) {
	cases := map[string]*redisStore{
		"untyped nil": newRedisStore(nil, "test:"),
		// The case a *redis.Client parameter used to make unrepresentable: a
		// field nobody assigned, which is not == nil inside an interface.
		"typed nil": newRedisStore((*redis.Client)(nil), "test:"),
	}

	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			if err := s.Set(ctx, "k", Challenge{}, time.Minute); !errors.Is(err, ErrNilClient) {
				t.Errorf("Set = %v, want ErrNilClient", err)
			}
			var out Challenge
			err := s.Get(ctx, "k", &out)
			if !errors.Is(err, ErrNilClient) {
				t.Errorf("Get = %v, want ErrNilClient", err)
			}
			// Crucially not a miss: a manager that read this as "expired"
			// would answer a verification without consuming an attempt.
			if isNotFound(err) {
				t.Errorf("a missing client classified as a miss: %v", err)
			}
			if err := s.Del(ctx, "k"); !errors.Is(err, ErrNilClient) {
				t.Errorf("Del = %v, want ErrNilClient", err)
			}
			if _, err := s.Exists(ctx, "k"); !errors.Is(err, ErrNilClient) {
				t.Errorf("Exists = %v, want ErrNilClient", err)
			}
			if _, err := s.TTL(ctx, "k"); !errors.Is(err, ErrNilClient) {
				t.Errorf("TTL = %v, want ErrNilClient", err)
			}
		})
	}
}

func TestIsNilClient(t *testing.T) {
	mr := miniredis.RunT(t)
	live := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = live.Close() })

	cases := []struct {
		name   string
		client RedisClient
		want   bool
	}{
		{"untyped nil", nil, true},
		{"nil *redis.Client", (*redis.Client)(nil), true},
		{"nil *redis.ClusterClient", (*redis.ClusterClient)(nil), true},
		{"nil *redis.Ring", (*redis.Ring)(nil), true},
		{"a live client", live, false},
		{"a wrapper around a live client", &countingClient{RedisClient: live}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNilClient(tc.client); got != tc.want {
				t.Errorf("isNilClient(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}
