package challenge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestConditionalStorePreservesSnapshotAndLifetime(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	s := newRedisStore(client, "conditional:")
	ctx := context.Background()
	// Whitespace and field order must survive decoding: CAS compares stored bytes.
	raw := `{ "user_id": "user", "id": "challenge", "attempts": 1 }`
	if err := client.Set(ctx, "conditional:challenge", raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	var ch Challenge
	snapshot, err := s.GetSnapshot(ctx, "challenge", &ch)
	if err != nil || snapshot != raw || ch.Attempts != 1 {
		t.Fatalf("snapshot = %q, challenge = %+v, err = %v", snapshot, ch, err)
	}
	mr.FastForward(1250 * time.Millisecond)
	wantTTL := mr.TTL("conditional:challenge")
	ch.Attempts++
	if err := s.CompareAndSet(ctx, "challenge", snapshot, ch); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("conditional:challenge"); ttl != wantTTL {
		t.Fatalf("TTL reset: got %s, want %s", ttl, wantTTL)
	}
	var updated Challenge
	current, err := s.GetSnapshot(ctx, "challenge", &updated)
	if err != nil || updated.Attempts != 2 {
		t.Fatalf("updated = %+v, err = %v", updated, err)
	}
	if err := s.CompareAndDelete(ctx, "challenge", snapshot); !errors.Is(err, errSnapshotChanged) {
		t.Fatalf("stale delete = %v", err)
	}
	if err := s.CompareAndSet(ctx, "challenge", snapshot, ch); !errors.Is(err, errSnapshotChanged) {
		t.Fatalf("stale update = %v", err)
	}
	if got, err := client.Get(ctx, "conditional:challenge").Result(); err != nil || got != current {
		t.Fatalf("conflict changed record: %q, %v", got, err)
	}
	if err := s.CompareAndDelete(ctx, "challenge", current); err != nil || mr.Exists("conditional:challenge") {
		t.Fatalf("consume = %v, key still exists = %v", err, mr.Exists("conditional:challenge"))
	}
}

func TestConditionalStoreRejectsMissingExpiredAndImmortalRecords(t *testing.T) {
	for _, state := range []string{"missing", "expired", "immortal"} {
		for _, operation := range []string{"delete", "update"} {
			t.Run(state+"/"+operation, func(t *testing.T) {
				mr, client := setupMiniRedis(t)
				defer mr.Close()
				s := newRedisStore(client, "conditional:")
				ctx := context.Background()
				const raw = `{"attempts":1}`
				if state != "missing" {
					ttl := time.Second
					if state == "immortal" {
						ttl = 0
					}
					if err := client.Set(ctx, "conditional:ch", raw, ttl).Err(); err != nil {
						t.Fatal(err)
					}
					if state == "expired" {
						mr.FastForward(2 * time.Second)
					}
				}
				var err error
				if operation == "delete" {
					err = s.CompareAndDelete(ctx, "ch", raw)
				} else {
					err = s.CompareAndSet(ctx, "ch", raw, Challenge{Attempts: 2})
				}
				if !errors.Is(err, ErrNotFound) || !errors.Is(err, redis.Nil) {
					t.Fatalf("mutation = %v, want both miss sentinels", err)
				}
				if state == "immortal" {
					if got, err := client.Get(ctx, "conditional:ch").Result(); err != nil || got != raw {
						t.Fatalf("immortal record changed: %q, %v", got, err)
					}
				} else if mr.Exists("conditional:ch") {
					t.Fatal("mutation recreated absent record")
				}
			})
		}
	}
}

func TestConditionalStoreFailuresLeaveRecordUntouched(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	s := newRedisStore(client, "conditional:")
	ctx := context.Background()
	const raw = `{"attempts":1}`
	if err := client.Set(ctx, "conditional:ch", raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		s.Set(ctx, "ch", make(chan int), time.Minute),
		s.CompareAndSet(ctx, "ch", raw, make(chan int)),
	} {
		if err == nil {
			t.Fatal("unsupported JSON value accepted")
		}
	}
	if got, err := client.Get(ctx, "conditional:ch").Result(); err != nil || got != raw {
		t.Fatalf("marshal failure changed record: %q, %v", got, err)
	}
	// A Redis command failure must not be reported as an absent challenge.
	mr.SetError("ERR test backend failure")
	if err := s.CompareAndDelete(ctx, "ch", raw); err == nil || isNotFound(err) {
		t.Fatalf("backend failure = %v", err)
	}
	for _, s := range []*redisStore{newRedisStore(nil, ""), newRedisStore((*redis.Client)(nil), "")} {
		if err := s.CompareAndDelete(ctx, "ch", raw); !errors.Is(err, ErrNilClient) {
			t.Fatalf("nil-client delete = %v", err)
		}
		if err := s.CompareAndSet(ctx, "ch", raw, Challenge{}); !errors.Is(err, ErrNilClient) {
			t.Fatalf("nil-client update = %v", err)
		}
	}
}
