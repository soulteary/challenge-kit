package challenge

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// These cases need real Redis: Lua runtime ACL errors retain preceding writes.
func TestStrictActivationRealRedisPartialWrites(t *testing.T) {
	addr := os.Getenv("CHALLENGE_KIT_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set CHALLENGE_KIT_TEST_REDIS_ADDR to isolated Redis 7+")
	}
	for _, denied := range []string{"index", "record"} {
		t.Run(denied, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			admin := redis.NewClient(&redis.Options{Addr: addr, Protocol: 2, MaxRetries: -1})
			defer func() { _ = admin.Close() }()
			name := "activation-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
			cfg := strictTestConfig()
			cfg.ChallengeKeyPrefix = name + ":ch:"
			cfg.ActiveIndexPrefix = name + ":active:"
			cfg.VerifyLockPrefix = name + ":vlock:"
			cfg.LockKeyPrefix = name + ":lock:"
			trusted := NewManager(admin, cfg)
			old, _, err := trusted.Create(ctx, activationRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = trusted.SwapActive(ctx, old); err != nil {
				t.Fatal(err)
			}
			ch, code, err := trusted.Create(ctx, activationRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = admin.Del(context.Background(), cfg.ChallengeKeyPrefix+old.ID, cfg.ChallengeKeyPrefix+ch.ID, trusted.strictActiveIndexKey(ch)).Err()
			}()
			allowed := cfg.ChallengeKeyPrefix + "*"
			if denied == "record" {
				allowed = cfg.ActiveIndexPrefix + "*"
			}
			err = admin.Do(ctx, "ACL", "SETUSER", name, "reset", "on", ">synthetic-test-secret", "~*", "+eval", "+get", "+pttl", "+del", "+hello", "+ping", "+client|setinfo", "(+set ~"+allowed+" ~"+cfg.VerifyLockPrefix+"* ~"+cfg.LockKeyPrefix+"*)").Err()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admin.Do(context.Background(), "ACL", "DELUSER", name).Err() }()
			limited := redis.NewClient(&redis.Options{Addr: addr, Username: name, Password: "synthetic-test-secret", Protocol: 2, MaxRetries: -1})
			defer func() { _ = limited.Close() }()
			m := NewManager(limited, cfg)
			_, err = m.SwapActive(ctx, ch)
			if !errors.Is(err, ErrBackendUnavailable) || !strings.Contains(strings.ToLower(err.Error()), "permission") {
				t.Fatalf("expected runtime ACL error: %v", err)
			}
			pending, err := trusted.Get(ctx, ch.ID)
			if err != nil || pending.ActivationState != ActivationPending {
				t.Fatalf("partial record=%+v error=%v", pending, err)
			}
			result, err := trusted.Verify(ctx, ch.ID, code, "")
			if err == nil || result.OK || result.Reason != ReasonNotActive {
				t.Fatalf("partial verify=%+v error=%v", result, err)
			}
			// DEL precedes the failing SET, so the old record is unavailable.
			if n, e := admin.Exists(ctx, cfg.ChallengeKeyPrefix+old.ID).Result(); e != nil || n != 0 {
				t.Fatalf("old record count=%d error=%v", n, e)
			}
			index, e := admin.Get(ctx, trusted.strictActiveIndexKey(ch)).Result()
			want := old.ID
			if denied == "record" {
				want = ch.ID
			}
			if e != nil || index != want {
				t.Fatalf("index=%q want=%q error=%v", index, want, e)
			}
			// Correct the ACL and retry the same pending challenge. An index already
			// pointing at this ID must not be returned as a replacement to revoke.
			if err = admin.Do(ctx, "ACL", "SETUSER", name, "+set").Err(); err != nil {
				t.Fatal(err)
			}
			previous, err := m.SwapActive(ctx, ch)
			if err != nil {
				t.Fatal(err)
			}
			wantPrevious := old.ID
			if denied == "record" {
				wantPrevious = ""
			}
			if previous != wantPrevious {
				t.Fatalf("previous=%q want=%q", previous, wantPrevious)
			}
			if previous, err = m.SwapActive(ctx, ch); err != nil || previous != "" {
				t.Fatalf("idempotent previous=%q error=%v", previous, err)
			}
			if err = m.RevokePending(ctx, ch.ID); err != nil {
				t.Fatal(err)
			}
			result, err = trusted.Verify(ctx, ch.ID, code, "")
			if err != nil || !result.OK {
				t.Fatalf("recovered verify=%+v error=%v", result, err)
			}
		})
	}
}
