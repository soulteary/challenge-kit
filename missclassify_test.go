package challenge

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	rediskitcache "github.com/soulteary/redis-kit/cache"
)

// isNotFound decides whether verifyLocked answers ReasonExpired (terminal,
// consuming nothing) or ReasonBackendUnavailable (fail closed). Getting it wrong
// in the permissive direction hands back a free probe on an infrastructure
// fault, so the classification is pinned here.
func TestIsNotFoundClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil is not a miss",
			err:  nil,
			want: false,
		},
		{
			name: "redis.Nil is a miss",
			err:  redis.Nil,
			want: true,
		},
		{
			name: "wrapped redis.Nil is a miss",
			err:  fmt.Errorf("get: %w", redis.Nil),
			want: true,
		},
		{
			name: "redis-kit's ErrKeyNotFound sentinel is a miss",
			err:  rediskitcache.ErrKeyNotFound,
			want: true,
		},
		{
			name: "wrapped ErrKeyNotFound is a miss",
			err:  fmt.Errorf("get challenge: %w", rediskitcache.ErrKeyNotFound),
			want: true,
		},
		{
			// The text fallback is gone. A message that merely reads like a miss
			// carries no sentinel and must not be classified as one -- that is
			// the whole point of depending on redis-kit's sentinel instead.
			name: "a bare message that looks like a miss is not a miss",
			err:  fmt.Errorf("key not found: %s", "ch_abc"),
			want: false,
		},
		{
			// redis-kit's own backend path. Must fail closed.
			name: "redis-kit's backend failure is not a miss",
			err:  fmt.Errorf("failed to get cache: %w", errors.New("dial tcp: connection refused")),
			want: false,
		},
		{
			// The regression the sentinel removes for good: a backend error that
			// mentions the phrase used to become a reported expiry consuming no
			// attempt.
			name: "backend error merely mentioning the phrase is not a miss",
			err:  errors.New("CLUSTERDOWN the cluster is down: key not found: ch_abc"),
			want: false,
		},
		{
			// The manager's own wrapper, built only AFTER isNotFound says true.
			name: "the manager's own expiry wrapper is not itself a miss",
			err:  errors.New("challenge not found or expired"),
			want: false,
		},
		{
			name: "unrelated error is not a miss",
			err:  errors.New("some other failure"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// The table above asserts against sentinels this package names itself. This one
// asserts against what redis-kit's cache actually returns, so a change to its
// error representation is caught here rather than by a misclassified expiry in
// production.
func TestIsNotFoundAgainstARealCacheMiss(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	cache := rediskitcache.NewCache(client, "challenge-kit-miss-test:")

	var out Challenge
	err := cache.Get(context.Background(), "definitely-absent", &out)
	if err == nil {
		t.Fatal("reading an absent key must return an error")
	}
	if !isNotFound(err) {
		t.Fatalf("a real redis-kit cache miss must classify as a miss, got %v", err)
	}

	// And a value that IS present is not a miss.
	if err := cache.Set(context.Background(), "present", Challenge{ID: "ch_1"}, 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cache.Get(context.Background(), "present", &out); err != nil {
		t.Fatalf("Get on a present key: %v", err)
	}
	if out.ID != "ch_1" {
		t.Errorf("round-tripped ID = %q, want ch_1", out.ID)
	}
}
