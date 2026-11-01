package challenge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Hold a successful Redis read after it has obtained its snapshot. This models
// a paused process without depending on Argon2 speed or wall-clock sleeps.
type snapshotGate struct {
	RedisClient
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	ttl     bool
}

func (g *snapshotGate) pause(ctx context.Context, key string) {
	if strings.HasPrefix(key, "otp:ch:") && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
}
func (g *snapshotGate) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := g.RedisClient.Get(ctx, key)
	if !g.ttl {
		g.pause(ctx, key)
	}
	return cmd
}
func (g *snapshotGate) TTL(ctx context.Context, key string) *redis.DurationCmd {
	cmd := g.RedisClient.TTL(ctx, key)
	if g.ttl {
		g.pause(ctx, key)
	}
	return cmd
}

func TestSecurityAudit_StaleSnapshotCannotConsume(t *testing.T) {
	for _, change := range []string{"consumed_after_lock_expiry", "revoked", "expired"} {
		t.Run(change, func(t *testing.T) {
			mr, rc := setupMiniRedis(t)
			defer mr.Close()
			defer func() { _ = rc.Close() }()
			cfg := DefaultConfig()
			g := &snapshotGate{RedisClient: rc, reached: make(chan struct{}), release: make(chan struct{})}
			var once sync.Once
			resume := func() { once.Do(func() { close(g.release) }) }
			defer resume()
			m := NewManager(g, cfg)
			ch, code, err := m.Create(context.Background(), CreateRequest{UserID: "u", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
			if err != nil {
				t.Fatal(err)
			}
			g.armed.Store(true)
			result := make(chan *VerifyResult, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			go func() { r, _ := m.Verify(ctx, ch.ID, code, ""); result <- r }()
			select {
			case <-g.reached:
			case <-ctx.Done():
				t.Fatal("snapshot was not reached")
			}
			switch change {
			case "consumed_after_lock_expiry":
				mr.FastForward(cfg.VerifyLockTTL + time.Second)
				r, err := NewManager(rc, cfg).Verify(ctx, ch.ID, code, "")
				if err != nil || !r.OK {
					t.Fatalf("second verifier: result=%v err=%v", r, err)
				}
			case "revoked":
				if err = m.Revoke(ctx, ch.ID); err != nil {
					t.Fatal(err)
				}
			case "expired":
				mr.FastForward(cfg.Expiry + time.Second)
			}
			resume()
			select {
			case r := <-result:
				if r == nil || r.OK {
					t.Fatalf("stale snapshot accepted after %s: %+v", change, r)
				}
			case <-ctx.Done():
				t.Fatal("verification did not finish")
			}
		})
	}
}

func TestSecurityAudit_AttemptWriteCannotResurrectRevokedChallenge(t *testing.T) {
	mr, rc := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = rc.Close() }()
	g := &snapshotGate{RedisClient: rc, reached: make(chan struct{}), release: make(chan struct{}), ttl: true}
	var once sync.Once
	resume := func() { once.Do(func() { close(g.release) }) }
	defer resume()
	m := NewManager(g, DefaultConfig())
	ch, code, err := m.Create(context.Background(), CreateRequest{UserID: "u", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g.armed.Store(true)
	done := make(chan struct{})
	go func() { _, _ = m.Verify(ctx, ch.ID, wrong, ""); close(done) }()
	select {
	case <-g.reached:
	case <-ctx.Done():
		t.Fatal("attempt TTL was not reached")
	}
	if err = m.Revoke(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	resume()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("verification did not finish")
	}
	if mr.Exists("otp:ch:" + ch.ID) {
		t.Fatal("stale attempt write resurrected revoked challenge")
	}
}

func TestSecurityAudit_HashStorageUsesRandomSaltAndNoCodeField(t *testing.T) {
	mr, rc := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = rc.Close() }()
	m := NewManager(rc, DefaultConfig())
	ch, code, err := m.Create(context.Background(), CreateRequest{UserID: "u", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rc.Get(context.Background(), "otp:ch:"+ch.ID).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["code"]; ok {
		t.Fatal("plaintext code persisted")
	}
	if fields["code_hash"] != ch.CodeHash || ch.CodeHash == code {
		t.Fatal("Redis did not store the hash")
	}
	parts := strings.Split(ch.CodeHash, ":")
	if len(parts) != 2 {
		t.Fatal("unexpected legacy hash format")
	}
	salt, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt length=%d err=%v", len(salt), err)
	}
	again, err := m.argon2Hasher.Hash(code)
	if err != nil {
		t.Fatal(err)
	}
	if again == ch.CodeHash || !m.argon2Hasher.Verify(ch.CodeHash, code) || !m.argon2Hasher.Verify(again, code) {
		t.Fatal("salt reuse or hash verification failure")
	}
	if m.argon2Hasher.Verify("corrupt", code) {
		t.Fatal("malformed hash accepted")
	}
}
