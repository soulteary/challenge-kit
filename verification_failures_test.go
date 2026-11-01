package challenge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Keep the real store for all operations except the failure under test.
type verificationFaultStore struct {
	store
	readErr, writeErr, deleteErr, ttlErr, existsErr, setErr error
	beforeSet                                               func()
}

func (s *verificationFaultStore) GetSnapshot(ctx context.Context, key string, dest interface{}) (string, error) {
	if s.readErr != nil {
		return "", s.readErr
	}
	return s.store.GetSnapshot(ctx, key, dest)
}

func (s *verificationFaultStore) CompareAndSet(ctx context.Context, key, snapshot string, value interface{}) error {
	if s.beforeSet != nil {
		s.beforeSet()
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.store.CompareAndSet(ctx, key, snapshot, value)
}

func (s *verificationFaultStore) CompareAndDelete(ctx context.Context, key, snapshot string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.store.CompareAndDelete(ctx, key, snapshot)
}

func (s *verificationFaultStore) TTL(ctx context.Context, key string) (time.Duration, error) {
	if s.ttlErr != nil {
		return 0, s.ttlErr
	}
	return s.store.TTL(ctx, key)
}

func (s *verificationFaultStore) Exists(ctx context.Context, key string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
	}
	return s.store.Exists(ctx, key)
}

func (s *verificationFaultStore) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	if s.setErr != nil {
		return s.setErr
	}
	return s.store.Set(ctx, key, value, ttl)
}

func TestVerifyStorageFailuresAndConflicts(t *testing.T) {
	backendErr := errors.New("test backend unavailable")
	cases := []struct {
		name       string
		configure  func(*Challenge, *verificationFaultStore, *verificationFaultStore)
		code       string
		wantReason string
		wantError  error
	}{
		{"read", func(_ *Challenge, s, _ *verificationFaultStore) { s.readErr = backendErr }, "correct", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"user lock read", func(_ *Challenge, _, s *verificationFaultStore) { s.existsErr = backendErr }, "correct", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"TTL read", func(_ *Challenge, s, _ *verificationFaultStore) { s.ttlErr = backendErr }, "wrong", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"consume backend", func(_ *Challenge, s, _ *verificationFaultStore) { s.deleteErr = backendErr }, "correct", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"consume conflict", func(_ *Challenge, s, _ *verificationFaultStore) { s.deleteErr = errSnapshotChanged }, "correct", ReasonLockContention, ErrLockUnavailable},
		{"attempt backend", func(_ *Challenge, s, _ *verificationFaultStore) { s.writeErr = backendErr }, "wrong", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"attempt conflict", func(_ *Challenge, s, _ *verificationFaultStore) { s.writeErr = errSnapshotChanged }, "wrong", ReasonLockContention, ErrLockUnavailable},
		{"expired cleanup backend", func(ch *Challenge, s, _ *verificationFaultStore) {
			ch.ExpiresAt = time.Now().Add(-time.Minute)
			s.deleteErr = backendErr
		}, "correct", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"expired cleanup conflict", func(ch *Challenge, s, _ *verificationFaultStore) {
			ch.ExpiresAt = time.Now().Add(-time.Minute)
			s.deleteErr = errSnapshotChanged
		}, "correct", ReasonLockContention, ErrLockUnavailable},
		{"expired cleanup missing", func(ch *Challenge, s, _ *verificationFaultStore) {
			ch.ExpiresAt = time.Now().Add(-time.Minute)
			s.deleteErr = ErrNotFound
		}, "correct", ReasonExpired, nil},
		{"exhausted lock read", func(ch *Challenge, _, s *verificationFaultStore) {
			ch.Attempts = ch.MaxAttempts
			s.existsErr = backendErr
		}, "wrong", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"exhausted lock write", func(ch *Challenge, _, s *verificationFaultStore) { ch.Attempts = ch.MaxAttempts; s.setErr = backendErr }, "wrong", ReasonBackendUnavailable, ErrBackendUnavailable},
		{"exhausted state write", func(ch *Challenge, s, _ *verificationFaultStore) {
			ch.Attempts = ch.MaxAttempts
			s.writeErr = backendErr
		}, "wrong", ReasonBackendUnavailable, ErrBackendUnavailable},
	}
	// Hash once for this table rather than making each infrastructure case pay
	// for a new random code and hash.
	hasher := NewManager(nil, DefaultConfig()).argon2Hasher
	hash, err := hasher.Hash("correct")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr, client := setupMiniRedis(t)
			defer mr.Close()
			m := NewManager(client, DefaultConfig())
			realStore := m.cache
			challengeStore := &verificationFaultStore{store: realStore}
			lockStore := &verificationFaultStore{store: m.lockCache}
			ch := Challenge{ID: "ch", UserID: "user", CodeHash: hash, MaxAttempts: 2, ExpiresAt: time.Now().Add(time.Minute)}
			tc.configure(&ch, challengeStore, lockStore)
			ctx := context.Background()
			if err := realStore.Set(ctx, ch.ID, ch, time.Minute); err != nil {
				t.Fatal(err)
			}
			var stored Challenge
			before, err := realStore.GetSnapshot(ctx, ch.ID, &stored)
			if err != nil {
				t.Fatal(err)
			}
			m.cache, m.lockCache = challengeStore, lockStore
			result, err := m.Verify(ctx, ch.ID, tc.code, "")
			if err == nil || result.OK || result.Reason != tc.wantReason {
				t.Fatalf("Verify = %+v, %v", result, err)
			}
			if tc.wantError != nil && !errors.Is(err, tc.wantError) {
				t.Fatalf("error = %v, want %v", err, tc.wantError)
			}
			after, err := realStore.GetSnapshot(ctx, ch.ID, &stored)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("failed verification changed challenge: got %q, want %q", after, before)
			}
			if mr.Exists(m.config.VerifyLockPrefix + ch.ID) {
				t.Fatal("verification lease leaked after error")
			}
		})
	}
}

func TestVerifyChannelMismatchDoesNotConsume(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	m := NewManager(client, DefaultConfig())
	ctx := context.Background()
	ch, code, err := m.Create(ctx, CreateRequest{UserID: "user", Channel: ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.VerifyWithOptions(ctx, ch.ID, code, "", VerifyOptions{ExpectedChannel: ChannelSMS})
	if err == nil || result.OK || result.Reason != ReasonContextMismatch {
		t.Fatalf("mismatch = %+v, %v", result, err)
	}
	after, err := m.Get(ctx, ch.ID)
	if err != nil || after.Attempts != 0 {
		t.Fatalf("mismatch consumed attempt: %+v, %v", after, err)
	}
	result, err = m.VerifyWithOptions(ctx, ch.ID, code, "", VerifyOptions{ExpectedChannel: ChannelEmail})
	if err != nil || !result.OK {
		t.Fatalf("matching channel = %+v, %v", result, err)
	}
}

func TestRecordLockoutConditionalWrites(t *testing.T) {
	backendErr := errors.New("test backend unavailable")
	for _, mode := range []string{"missing at read", "read failure", "different user", "different hash", "missing at commit", "write failure", "preserve current fields"} {
		t.Run(mode, func(t *testing.T) {
			mr, client := setupMiniRedis(t)
			defer mr.Close()
			m := NewManager(client, DefaultConfig())
			ctx := context.Background()
			original := Challenge{ID: "ch", UserID: "user", CodeHash: "hash", Attempts: 2}
			current := original
			current.Attempts = 3
			s := &verificationFaultStore{store: m.cache}
			m.cache = s
			wantError := configureLockoutFault(t, mode, ctx, s, &current, backendErr)

			if mode != "missing at read" {
				if err := s.store.Set(ctx, "ch", current, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			mr.FastForward(1500 * time.Millisecond)
			wantTTL := mr.TTL(m.config.ChallengeKeyPrefix + "ch")
			err := m.recordLockoutApplied(ctx, "ch", &original)
			assertLockoutError(t, err, wantError)

			if mode == "missing at read" || mode == "missing at commit" {
				if mr.Exists(m.config.ChallengeKeyPrefix+"ch") || original.LockoutApplied {
					t.Fatal("absent record recreated or local state marked committed")
				}
				return
			}
			var after Challenge
			if err := s.Get(ctx, "ch", &after); err != nil {
				t.Fatal(err)
			}
			if mode == "preserve current fields" {
				current.LockoutApplied = true
			}
			if after != current || original.LockoutApplied != (mode == "preserve current fields") {
				t.Fatalf("unexpected state: stored %+v, local %+v", after, original)
			}
			if got := mr.TTL(m.config.ChallengeKeyPrefix + "ch"); got != wantTTL {
				t.Fatalf("TTL = %s, want %s", got, wantTTL)
			}
		})
	}
}

func configureLockoutFault(t *testing.T, mode string, ctx context.Context, s *verificationFaultStore, current *Challenge, backendErr error) error {
	t.Helper()

	var wantError error
	switch mode {
	case "read failure":
		s.readErr = backendErr
		wantError = ErrBackendUnavailable
	case "different user":
		current.UserID = "replacement"
		wantError = ErrLockUnavailable
	case "different hash":
		current.CodeHash = "replacement"
		wantError = ErrLockUnavailable
	case "missing at commit":
		s.beforeSet = func() {
			if err := s.Del(ctx, "ch"); err != nil {
				t.Fatal(err)
			}
		}
	case "write failure":
		s.writeErr = backendErr
		wantError = ErrBackendUnavailable
	}
	return wantError
}

func assertLockoutError(t *testing.T, err, wantError error) {
	t.Helper()

	if wantError == nil && err != nil || wantError != nil && !errors.Is(err, wantError) {
		t.Fatalf("record lockout = %v, want %v", err, wantError)
	}
}
