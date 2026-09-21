package challenge_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	challenge "github.com/soulteary/challenge-kit"
)

// Example shows the whole life of a challenge: mint a code, fail a
// verification, then redeem it. The manager is built from a
// redis.UniversalClient, which is what a service that switches between
// standalone, Sentinel and Cluster by configuration actually holds.
func Example() {
	// A real program passes its own client. miniredis stands in here so the
	// example runs as a test.
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	var client redis.UniversalClient = redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: []string{mr.Addr()},
	})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, challenge.CreateRequest{
		UserID:      "user-1",
		Channel:     challenge.ChannelSMS,
		Destination: "13800000000",
		Purpose:     "login",
		ClientIP:    "203.0.113.7",
	})
	if err != nil {
		panic(err)
	}
	// Send `code` over the channel; never log it, and never store it.

	res, err := manager.Verify(ctx, ch.ID, "000000", "203.0.113.7")
	fmt.Println("wrong code:", res.OK, res.Reason, "remaining", *res.RemainingAttempts, err != nil)

	res, err = manager.Verify(ctx, ch.ID, code, "203.0.113.7")
	fmt.Println("right code:", res.OK, res.Challenge.Purpose, err)

	// One-time use: the challenge is consumed before OK is reported.
	res, _ = manager.Verify(ctx, ch.ID, code, "203.0.113.7")
	fmt.Println("replay:    ", res.OK, res.Reason)

	// Output:
	// wrong code: false invalid remaining 4 true
	// right code: true login <nil>
	// replay:     false expired
}

// ExampleManager_VerifyWithOptions binds a challenge to the context it was
// minted for. The checks run inside the verification lock, before the code is
// compared, and a mismatch consumes no attempt -- so probing with the wrong
// purpose cannot burn a legitimate challenge.
func ExampleManager_VerifyWithOptions() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, challenge.CreateRequest{
		UserID:      "user-1",
		Channel:     challenge.ChannelEmail,
		Destination: "user@example.com",
		Purpose:     "login",
	})
	if err != nil {
		panic(err)
	}

	// A code minted for "login" cannot be redeemed to change a password.
	res, _ := manager.VerifyWithOptions(ctx, ch.ID, code, "", challenge.VerifyOptions{
		ExpectedPurpose: "password_reset",
	})
	fmt.Println("wrong purpose:", res.OK, res.Reason)

	// The attempt was not consumed, so the real purpose still works.
	res, err = manager.VerifyWithOptions(ctx, ch.ID, code, "", challenge.VerifyOptions{
		ExpectedUserID:  "user-1",
		ExpectedPurpose: "login",
		ExpectedChannel: challenge.ChannelEmail,
	})
	fmt.Println("right purpose:", res.OK, err)

	// Output:
	// wrong purpose: false context_mismatch
	// right purpose: true <nil>
}

// ExampleManager_SwapActive shows the two-phase send. The challenge is stored
// first, promoted to "the active one for this identity" only once the provider
// accepted it, and removed outright if the send failed -- so a failed send
// never leaves a redeemable code behind, and a successful one tells you which
// older challenge to revoke.
func ExampleManager_SwapActive() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	ctx := context.Background()
	req := challenge.CreateRequest{
		UserID:      "user-1",
		Channel:     challenge.ChannelSMS,
		Destination: "13800000000",
		Purpose:     "login",
	}

	first, _, err := manager.Create(ctx, req)
	if err != nil {
		panic(err)
	}
	if _, err := manager.SwapActive(ctx, first); err != nil {
		panic(err)
	}

	// The user asks for another code.
	second, _, err := manager.Create(ctx, req)
	if err != nil {
		panic(err)
	}

	// Pretend the provider rejected this one.
	sendFailed := true
	if sendFailed {
		fmt.Println("send failed, pending revoked:", manager.RevokePending(ctx, second.ID))
		res, _ := manager.Verify(ctx, second.ID, "000000", "")
		fmt.Println("unsent challenge:", res.Reason)
	}

	// A third attempt goes through, and displaces the first.
	third, _, err := manager.Create(ctx, req)
	if err != nil {
		panic(err)
	}
	previous, err := manager.SwapActive(ctx, third)
	if err != nil {
		panic(err)
	}
	fmt.Println("displaced the first challenge:", previous == first.ID)
	fmt.Println("revoked it:", manager.Revoke(ctx, previous))

	// Output:
	// send failed, pending revoked: <nil>
	// unsent challenge: expired
	// displaced the first challenge: true
	// revoked it: <nil>
}

// ExampleNewManager_cluster builds a manager against a Redis Cluster. Every
// command the manager issues names a single key, so the challenge, lock,
// verification-lock and active-index keys are free to land in different slots.
//
// It has no Output comment because it needs a real cluster; go test compiles
// it, which is what keeps it honest.
func ExampleNewManager_cluster() {
	client := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs: []string{"10.0.0.1:6379", "10.0.0.2:6379", "10.0.0.3:6379"},
	})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	_ = manager
}

// ExampleNewManager_sentinel builds a manager against a Sentinel-managed
// failover setup, which is a redis.UniversalClient like any other.
//
// No Output comment, for the same reason as the cluster example.
func ExampleNewManager_sentinel() {
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		MasterName: "mymaster",
		Addrs:      []string{"10.0.0.1:26379", "10.0.0.2:26379"},
	})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	_ = manager
}

// ExampleDefaultConfig documents the defaults a zero-valued field falls back
// to. NewManager applies the same values, so reading one off DefaultConfig()
// gives what a manager would actually use.
func ExampleDefaultConfig() {
	cfg := challenge.DefaultConfig()

	fmt.Println("expiry:                ", cfg.Expiry)
	fmt.Println("max attempts:          ", cfg.MaxAttempts)
	fmt.Println("lockout:               ", cfg.LockoutDuration)
	fmt.Println("code length:           ", cfg.CodeLength)
	fmt.Println("concurrent Argon2 work:", cfg.MaxConcurrentVerifications)

	// Output:
	// expiry:                 5m0s
	// max attempts:           5
	// lockout:                10m0s
	// code length:            6
	// concurrent Argon2 work: 16
}

// ExampleErrLockUnavailable shows how to tell the retryable outcome from the
// terminal ones. Lock contention consumes no attempt, so it must be retried
// rather than reported to the user as a wrong code.
func ExampleErrLockUnavailable() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	manager := challenge.NewManager(client, challenge.DefaultConfig())
	ctx := context.Background()

	ch, code, err := manager.Create(ctx, challenge.CreateRequest{
		UserID: "user-1", Channel: challenge.ChannelSMS, Destination: "13800000000",
	})
	if err != nil {
		panic(err)
	}

	var res *challenge.VerifyResult
	for attempt := 0; attempt < 3; attempt++ {
		res, err = manager.Verify(ctx, ch.ID, code, "")
		if errors.Is(err, challenge.ErrLockUnavailable) {
			time.Sleep(25 * time.Millisecond) // retryable: no attempt was spent
			continue
		}
		break
	}

	switch {
	case errors.Is(err, challenge.ErrBackendUnavailable):
		fmt.Println("backend down; the answer is unknown, not negative")
	case res.OK:
		fmt.Println("verified")
	default:
		fmt.Println("rejected:", res.Reason)
	}

	// Output:
	// verified
}
