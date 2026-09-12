package challenge

import (
	"errors"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"
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
			// The exact message redis-kit v1.5.0 returns for a miss.
			name: "redis-kit's miss message is a miss",
			err:  fmt.Errorf("key not found: %s", "ch_abc"),
			want: true,
		},
		{
			// redis-kit's own backend path. Must fail closed.
			name: "redis-kit's backend failure is not a miss",
			err:  fmt.Errorf("failed to get cache: %w", errors.New("dial tcp: connection refused")),
			want: false,
		},
		{
			// The regression this test exists for: strings.Contains matched the
			// phrase anywhere, so a backend error mentioning it became a
			// reported expiry with no attempt consumed.
			name: "backend error merely mentioning the phrase is not a miss",
			err:  errors.New("CLUSTERDOWN the cluster is down: key not found: ch_abc"),
			want: false,
		},
		{
			name: "backend error wrapping the phrase is not a miss",
			err:  fmt.Errorf("failed to get cache: %w", errors.New("key not found: ch_abc")),
			want: false,
		},
		{
			// The manager's own wrapper, built only AFTER isNotFound says true.
			// The old code matched it too, which was unreachable at the single
			// call site; this pins that it is not treated as a miss on its own.
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

// Once go.mod points at the redis-kit release that exports cache.ErrKeyNotFound,
// that sentinel wraps redis.Nil and the errors.Is branch subsumes the text
// check. This pins the property that makes the deletion safe: a miss carrying
// redis.Nil is classified without the message being consulted at all.
func TestMissCarryingRedisNilNeedsNoTextMatch(t *testing.T) {
	err := fmt.Errorf("a message that matches nothing: %w", redis.Nil)
	if !isNotFound(err) {
		t.Fatal("a miss wrapping redis.Nil must classify without the text check")
	}
}
