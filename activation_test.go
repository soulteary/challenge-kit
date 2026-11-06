package challenge

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"strings"
	"sync"
	"testing"
	"time"
)

func strictTestConfig() Config {
	cfg := DefaultConfig()
	cfg.RequireActiveVerification = true
	cfg.ChallengeKeyPrefix = "otp2:ch:"
	cfg.ActiveIndexPrefix = "otp2:active:"
	return cfg
}
func activationRequest() CreateRequest {
	return CreateRequest{UserID: "CaseSensitive", Channel: ChannelEmail, Destination: "user@example.com", Purpose: "login"}
}

func TestStrictChallengesRequireActivationAndPreserveLifetime(t *testing.T) {
	mr, rc := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = rc.Close() }()
	ctx := context.Background()
	cfg := strictTestConfig()
	m := NewManager(rc, cfg)
	ch, code, err := m.Create(ctx, activationRequest())
	if err != nil {
		t.Fatal(err)
	}
	if ch.ActivationState != ActivationPending {
		t.Fatalf("state=%s", ch.ActivationState)
	}
	for _, manager := range []*Manager{m, NewManager(rc, Config{ChallengeKeyPrefix: cfg.ChallengeKeyPrefix})} {
		result, err := manager.Verify(ctx, ch.ID, code, "")
		if err == nil || result.OK || result.Reason != ReasonNotActive {
			t.Fatalf("pending verify=%+v,%v", result, err)
		}
	}
	mr.FastForward(time.Second)
	wantTTL := mr.TTL(cfg.ChallengeKeyPrefix + ch.ID)
	if _, err := m.SwapActive(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if ch.ActivationState != ActivationActive || mr.TTL(cfg.ChallengeKeyPrefix+ch.ID) != wantTTL || mr.TTL(m.strictActiveIndexKey(ch)) != wantTTL {
		t.Fatal("activation changed state or extended lifetime")
	}
	previous, err := m.SwapActive(ctx, ch)
	if err != nil || previous != "" {
		t.Fatalf("activation retry returned replacement=%q,error=%v", previous, err)
	}
	// Consumers may revoke any non-empty previousID. A retry must not name itself.
	if previous != "" {
		if err := m.Revoke(ctx, previous); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RevokePending(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists(cfg.ChallengeKeyPrefix + ch.ID) {
		t.Fatal("pending cleanup deleted committed active challenge")
	}
	result, err := m.Verify(ctx, ch.ID, code, "")
	if err != nil || !result.OK {
		t.Fatalf("active verify=%+v,%v", result, err)
	}
	if _, err := m.SwapActive(ctx, ch); err == nil {
		t.Fatal("consumed challenge reactivated")
	}
}

func TestStrictCompatibilityAndNamespaces(t *testing.T) {
	mr, rc := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = rc.Close() }()
	ctx := context.Background()
	legacy := NewManager(rc, DefaultConfig())
	strict := NewManager(rc, strictTestConfig())
	if strings.HasPrefix(strict.config.ChallengeKeyPrefix, legacy.config.ChallengeKeyPrefix) {
		t.Fatal("strict prefix can be reached by a legacy suffix alias")
	}
	old, oldCode, err := legacy.Create(ctx, activationRequest())
	if err != nil {
		t.Fatal(err)
	}
	result, err := legacy.Verify(ctx, old.ID, oldCode, "")
	if err != nil || !result.OK {
		t.Fatal("default Create-to-Verify changed")
	}
	ch, code, err := strict.Create(ctx, activationRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ch.ID, "v2:" + ch.ID, "otp2:" + ch.ID} {
		if _, err := rc.Get(ctx, legacy.config.ChallengeKeyPrefix+id).Result(); !errors.Is(err, redis.Nil) {
			t.Fatalf("legacy key alias %q reached strict storage: %v", id, err)
		}
	}
	result, err = legacy.Verify(ctx, ch.ID, code, "")
	if err == nil || result.OK || result.Reason != ReasonExpired {
		t.Fatal("old namespace can read strict record")
	}
	// Opting into strict verification on an existing prefix rejects unmarked records.
	unmarked, _, err := legacy.Create(ctx, activationRequest())
	if err != nil {
		t.Fatal(err)
	}
	samePrefix := DefaultConfig()
	samePrefix.RequireActiveVerification = true
	strictOnLegacy := NewManager(rc, samePrefix)
	result, err = strictOnLegacy.Verify(ctx, unmarked.ID, "wrong", "")
	if err == nil || result.Reason != ReasonNotActive {
		t.Fatal("strict manager accepted legacy record")
	}
	if _, err := strictOnLegacy.SwapActive(ctx, unmarked); err == nil {
		t.Fatal("legacy record promoted without strict creation")
	}
	if err := strict.RevokePending(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(strict.config.ChallengeKeyPrefix + ch.ID) {
		t.Fatal("pending record survived cleanup")
	}
	if err := strict.RevokePending(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
}

type activationRaceStore struct {
	store
	before func()
	once   sync.Once
}

func (s *activationRaceStore) CompareAndDelete(ctx context.Context, key, snapshot string) error {
	s.once.Do(s.before)
	return s.store.CompareAndDelete(ctx, key, snapshot)
}
func (s *activationRaceStore) CompareAndSet(ctx context.Context, key, snapshot string, value interface{}) error {
	s.once.Do(s.before)
	return s.store.CompareAndSet(ctx, key, snapshot, value)
}

func TestStrictActivationWinsOverInFlightVerification(t *testing.T) {
	for _, correct := range []bool{true, false} {
		t.Run(map[bool]string{true: "correct", false: "incorrect"}[correct], func(t *testing.T) {
			mr, rc := setupMiniRedis(t)
			defer mr.Close()
			defer func() { _ = rc.Close() }()
			ctx := context.Background()
			cfg := strictTestConfig()
			m := NewManager(rc, cfg)
			other := NewManager(rc, cfg)
			old, code, err := m.Create(ctx, activationRequest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.SwapActive(ctx, old); err != nil {
				t.Fatal(err)
			}
			replacement, replacementCode, err := other.Create(ctx, activationRequest())
			if err != nil {
				t.Fatal(err)
			}
			m.cache = &activationRaceStore{store: m.cache, before: func() {
				if _, err := other.SwapActive(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}}
			if !correct {
				code = strings.Repeat("x", len(code))
			}
			result, err := m.Verify(ctx, old.ID, code, "")
			if err == nil || result.OK {
				t.Fatal("superseded snapshot was accepted")
			}
			if mr.Exists(cfg.ChallengeKeyPrefix + old.ID) {
				t.Fatal("superseded challenge was resurrected")
			}
			result, err = other.Verify(ctx, replacement.ID, replacementCode, "")
			if err != nil || !result.OK {
				t.Fatalf("replacement=%+v,%v", result, err)
			}
		})
	}
}

func TestStrictActivationRejectsChangesAndBackendFailures(t *testing.T) {
	mr, rc := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = rc.Close() }()
	ctx := context.Background()
	cfg := strictTestConfig()
	m := NewManager(rc, cfg)
	if _, err := m.SwapActive(ctx, nil); err == nil {
		t.Fatal("nil activation accepted")
	}
	ch, _, err := m.Create(ctx, activationRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Persist(ctx, cfg.ChallengeKeyPrefix+ch.ID).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SwapActive(ctx, ch); err == nil {
		t.Fatal("immortal pending accepted")
	}
	if err := rc.Expire(ctx, cfg.ChallengeKeyPrefix+ch.ID, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SwapActive(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := rc.Del(ctx, m.strictActiveIndexKey(ch)).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SwapActive(ctx, ch); err == nil {
		t.Fatal("unindexed active record reactivated")
	}
	nilManager := NewManager(nil, cfg)
	if _, err := nilManager.SwapActive(ctx, ch); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("nil activation=%v", err)
	}
	if err := nilManager.RevokePending(ctx, ch.ID); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("nil cleanup=%v", err)
	}
	mr.Close()
	if err := m.RevokePending(ctx, ch.ID); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("backend cleanup=%v", err)
	}
}
