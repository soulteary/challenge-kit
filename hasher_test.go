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

func testHMACKey() []byte {
	key := make([]byte, HMACCodeHasherMinKeyLen)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

func newTestHMACHasher(t testing.TB) CodeHasher {
	t.Helper()
	h, err := NewHMACCodeHasher(testHMACKey())
	if err != nil {
		t.Fatalf("NewHMACCodeHasher: %v", err)
	}
	return h
}

// wrongCodeFor returns a code of the same length that differs from code.
func wrongCodeFor(code string) string {
	if strings.HasPrefix(code, "0") {
		return "1" + code[1:]
	}
	return "0" + code[1:]
}

func storedChallenge(t *testing.T, m *Manager, id string) Challenge {
	t.Helper()
	var ch Challenge
	if err := m.cache.Get(context.Background(), id, &ch); err != nil {
		t.Fatalf("read challenge %s: %v", id, err)
	}
	return ch
}

func TestHMACCodeHasher_KnownAnswer(t *testing.T) {
	// Computed outside this package from the documented construction:
	// HMAC-SHA-256(key = 0x00..0x1f, label || 0x00 || len64(id) || id ||
	// len64(code) || code). It pins the stored format.
	const want = "hmac-sha256$v1$I85kKuzuggL4LKERjYrMKAlJq-RIVLNvnCl8YmdrG4s"
	h := newTestHMACHasher(t)
	got, err := h.Hash(context.Background(), "ch_0123456789abcdefghijkl", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
}

func TestHMACCodeHasher_RoundTripAndMismatch(t *testing.T) {
	ctx := context.Background()
	h := newTestHMACHasher(t)
	stored, err := h.Hash(ctx, "ch_a", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "123456") || isBuiltinCodeHash(stored) {
		t.Fatalf("stored value %q holds the code or has a built-in form", stored)
	}
	cases := []struct {
		name, id, code string
		want           bool
	}{
		{"same id and code", "ch_a", "123456", true},
		{"wrong code", "ch_a", "123457", false},
		{"empty code", "ch_a", "", false},
		{"other challenge id", "ch_b", "123456", false},
	}
	for _, tc := range cases {
		ok, err := h.Verify(ctx, tc.id, stored, tc.code)
		if err != nil || ok != tc.want {
			t.Errorf("%s: Verify = %v, %v; want %v, nil", tc.name, ok, err, tc.want)
		}
	}

	other := make([]byte, HMACCodeHasherMinKeyLen)
	otherKey, err := NewHMACCodeHasher(other)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := otherKey.Verify(ctx, "ch_a", stored, "123456"); err != nil || ok {
		t.Errorf("Verify under another key = %v, %v; want false, nil", ok, err)
	}
}

func TestHMACCodeHasher_BindsChallengeIDUnambiguously(t *testing.T) {
	ctx := context.Background()
	h := newTestHMACHasher(t)
	hash := func(id, code string) string {
		t.Helper()
		s, err := h.Hash(ctx, id, code)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if hash("ch_a", "123456") == hash("ch_b", "123456") {
		t.Error("the same code under two challenge IDs stored the same value")
	}
	// The same bytes split differently between ID and code must not collide.
	if hash("ch_1", "23456") == hash("ch_12", "3456") {
		t.Error("framing is ambiguous: shifted id/code boundary stored the same value")
	}
	first, second := hash("ch_a", "123456"), hash("ch_a", "123456")
	if first != second {
		t.Error("Hash is not deterministic for one id and code")
	}
}

func TestHMACCodeHasher_RejectsUnrecognisedStoredValues(t *testing.T) {
	ctx := context.Background()
	h := newTestHMACHasher(t)
	good, err := h.Hash(ctx, "ch_a", "123456")
	if err != nil {
		t.Fatal(err)
	}
	mac := strings.TrimPrefix(good, "hmac-sha256$v1$")
	cases := map[string]string{
		"empty":                "",
		"argon2 simple form":   "c2FsdHNhbHRzYWx0c2FsdA==:aGFzaA==",
		"argon2 phc form":      "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA",
		"other scheme":         "hmac-sha512$v1$" + mac,
		"unknown version":      "hmac-sha256$v2$" + mac,
		"missing version":      "hmac-sha256$" + mac,
		"extra field":          good + "$x",
		"empty mac":            "hmac-sha256$v1$",
		"truncated mac":        "hmac-sha256$v1$" + mac[:len(mac)-4],
		"mac not base64url":    "hmac-sha256$v1$" + strings.Repeat("!", len(mac)),
		"padded mac":           good + "=",
		"standard alphabet":    "hmac-sha256$v1$" + strings.Repeat("+", len(mac)),
		"trailing line break":  good + "\n",
		"embedded line break":  "hmac-sha256$v1$" + mac[:10] + "\n" + mac[10:],
		"over-long mac":        "hmac-sha256$v1$" + mac + "AAAA",
		"non-canonical ending": "hmac-sha256$v1$" + mac[:len(mac)-1] + "_",
	}
	for name, stored := range cases {
		// The submitted code is the right one: only the format decides.
		ok, err := h.Verify(ctx, "ch_a", stored, "123456")
		if ok || !errors.Is(err, ErrCodeHashFormat) {
			t.Errorf("%s: Verify(%q) = %v, %v; want false and ErrCodeHashFormat", name, stored, ok, err)
		}
	}
}

func TestNewHMACCodeHasher_Key(t *testing.T) {
	for _, n := range []int{0, 1, 16, HMACCodeHasherMinKeyLen - 1} {
		if h, err := NewHMACCodeHasher(make([]byte, n)); err == nil || h != nil {
			t.Errorf("a %d-byte key was accepted", n)
		}
	}
	if _, err := NewHMACCodeHasher(nil); err == nil {
		t.Error("a nil key was accepted")
	}

	// The key is copied: changing the caller's slice afterwards changes nothing.
	ctx := context.Background()
	key := testHMACKey()
	h, err := NewHMACCodeHasher(key)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := h.Hash(ctx, "ch_a", "123456")
	for i := range key {
		key[i] = 0xff
	}
	after, _ := h.Hash(ctx, "ch_a", "123456")
	if before != after {
		t.Error("the hasher kept a reference to the caller's key slice")
	}

	if _, err := h.Hash(ctx, "", "123456"); err == nil {
		t.Error("Hash accepted an empty challenge id")
	}
	if _, err := h.Hash(ctx, "ch_a", ""); err == nil {
		t.Error("Hash accepted an empty code")
	}
}

func TestIsBuiltinCodeHash(t *testing.T) {
	m := NewManager(nil, DefaultConfig())
	simple, err := m.argon2Hasher.Hash("123456")
	if err != nil {
		t.Fatal(err)
	}
	phc, err := m.argon2Hasher.HashWithParams("123456")
	if err != nil {
		t.Fatal(err)
	}
	hmacValue, err := newTestHMACHasher(t).Hash(context.Background(), "ch_a", "123456")
	if err != nil {
		t.Fatal(err)
	}
	for stored, want := range map[string]bool{
		simple:    true,
		phc:       true,
		hmacValue: false,
		"":        false,
		"corrupt": false,
	} {
		if got := isBuiltinCodeHash(stored); got != want {
			t.Errorf("isBuiltinCodeHash(%q) = %v, want %v", stored, got, want)
		}
	}
}

func TestManager_HMACCodeHasher_CreateAndVerify(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.CodeHasher = newTestHMACHasher(t)
	m := NewManager(client, cfg)

	ch, code, err := m.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(ch.CodeHash, "hmac-sha256$v1$") {
		t.Fatalf("CodeHash = %q, want the hmac-sha256 v1 form", ch.CodeHash)
	}
	if got := storedChallenge(t, m, ch.ID).CodeHash; got != ch.CodeHash {
		t.Fatalf("stored CodeHash = %q, returned %q", got, ch.CodeHash)
	}

	res, err := m.Verify(ctx, ch.ID, wrongCodeFor(code), "")
	if err == nil || res.OK || res.Reason != ReasonInvalid || res.RemainingAttempts == nil || *res.RemainingAttempts != cfg.MaxAttempts-1 {
		t.Fatalf("wrong code: Verify = %+v, %v", res, err)
	}
	if got := storedChallenge(t, m, ch.ID).Attempts; got != 1 {
		t.Fatalf("attempts after a wrong code = %d, want 1", got)
	}

	res, err = m.Verify(ctx, ch.ID, code, "")
	if err != nil || !res.OK {
		t.Fatalf("right code: Verify = %+v, %v", res, err)
	}
	if _, err := m.Get(ctx, ch.ID); err == nil {
		t.Fatal("the challenge was not consumed by a successful verification")
	}
}

// A code of another challenge must not verify: the digest is bound to the
// challenge ID, so copying a stored value between records does not help.
func TestManager_HMACCodeHasher_StoredValueIsBoundToChallenge(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.CodeHasher = newTestHMACHasher(t)
	m := NewManager(client, cfg)

	a, codeA, err := m.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := m.Create(ctx, CreateRequest{UserID: "u2", Channel: ChannelEmail, Destination: "b@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	// Put A's stored value on B.
	moved := storedChallenge(t, m, b.ID)
	moved.CodeHash = a.CodeHash
	if err := m.cache.Set(ctx, b.ID, moved, time.Minute); err != nil {
		t.Fatal(err)
	}
	res, err := m.Verify(ctx, b.ID, codeA, "")
	if err == nil || res.OK || res.Reason != ReasonInvalid {
		t.Fatalf("Verify with another challenge's stored value = %+v, %v", res, err)
	}
}

func TestManager_Argon2ChallengeVerifiesAfterSwitchToHMAC(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	old := NewManager(client, DefaultConfig())
	req := CreateRequest{UserID: "u1", Channel: ChannelSMS, Destination: "+10000000000", Purpose: "login"}
	first, firstCode, err := old.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, secondCode, err := old.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !isBuiltinCodeHash(first.CodeHash) {
		t.Fatalf("the default manager stored %q, not a built-in form", first.CodeHash)
	}

	// The same deployment after the consumer enables the HMAC hasher.
	cfg := DefaultConfig()
	cfg.CodeHasher = newTestHMACHasher(t)
	cfg.LockoutThreshold = 2 // keep the first exhausted challenge from locking the user
	switched := NewManager(client, cfg)

	res, err := switched.Verify(ctx, first.ID, firstCode, "")
	if err != nil || !res.OK {
		t.Fatalf("Argon2 challenge on the HMAC manager: Verify = %+v, %v", res, err)
	}

	// Wrong codes on an Argon2 challenge are still counted, up to the limit.
	wrong := wrongCodeFor(secondCode)
	for i := 1; i <= cfg.MaxAttempts; i++ {
		res, err := switched.Verify(ctx, second.ID, wrong, "")
		wantReason := ReasonInvalid
		if i == cfg.MaxAttempts {
			wantReason = ReasonLocked
		}
		if err == nil || res.OK || res.Reason != wantReason {
			t.Fatalf("wrong code %d: Verify = %+v, %v; want reason %q", i, res, err, wantReason)
		}
		if got := storedChallenge(t, switched, second.ID).Attempts; got != i {
			t.Fatalf("attempts after wrong code %d = %d", i, got)
		}
	}
	res, err = switched.Verify(ctx, second.ID, secondCode, "")
	if err == nil || res.OK || res.Reason != ReasonLocked {
		t.Fatalf("right code after exhaustion: Verify = %+v, %v", res, err)
	}
}

func TestManager_HMACChallengeFailsClosedWithoutHasher(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.CodeHasher = newTestHMACHasher(t)
	withHasher := NewManager(client, cfg)
	ch, code, err := withHasher.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}

	// A manager without the hasher treats the value as it treats any stored
	// value Argon2 cannot read: the code does not match and an attempt is spent.
	without := NewManager(client, DefaultConfig())
	res, err := without.Verify(ctx, ch.ID, code, "")
	if err == nil || res.OK || res.Reason != ReasonInvalid {
		t.Fatalf("Verify without the hasher = %+v, %v; want a rejection with reason %q", res, err, ReasonInvalid)
	}
	if got := storedChallenge(t, without, ch.ID).Attempts; got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}

	// A manager with a different key also rejects it, as a wrong code.
	otherCfg := DefaultConfig()
	otherCfg.CodeHasher, err = NewHMACCodeHasher(make([]byte, HMACCodeHasherMinKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	res, err = NewManager(client, otherCfg).Verify(ctx, ch.ID, code, "")
	if err == nil || res.OK || res.Reason != ReasonInvalid {
		t.Fatalf("Verify under another key = %+v, %v", res, err)
	}

	// The manager that has the hasher still verifies it.
	res, err = withHasher.Verify(ctx, ch.ID, code, "")
	if err != nil || !res.OK {
		t.Fatalf("Verify with the hasher = %+v, %v", res, err)
	}
}

// The intermediate stage of a rolling change: a verify-only manager keeps
// creating Argon2 challenges and verifies both forms, so it can run next to
// managers that already create HMAC challenges, and next to ones without the
// hasher.
func TestManager_CodeHasherVerifyOnly(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	req := CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"}

	cfg := DefaultConfig()
	cfg.CodeHasher = newTestHMACHasher(t)
	full := NewManager(client, cfg)
	cfg.CodeHasherVerifyOnly = true
	verifyOnly := NewManager(client, cfg)
	plain := NewManager(client, DefaultConfig())

	created, code, err := verifyOnly.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !isBuiltinCodeHash(created.CodeHash) {
		t.Fatalf("a verify-only manager stored %q, not a built-in Argon2 value", created.CodeHash)
	}
	if res, err := plain.Verify(ctx, created.ID, code, ""); err != nil || !res.OK {
		t.Fatalf("manager without the hasher: Verify = %+v, %v", res, err)
	}

	hmacChallenge, hmacCode, err := full.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := verifyOnly.Verify(ctx, hmacChallenge.ID, wrongCodeFor(hmacCode), ""); err == nil || res.Reason != ReasonInvalid {
		t.Fatalf("verify-only manager, wrong code: Verify = %+v, %v", res, err)
	}
	if res, err := verifyOnly.Verify(ctx, hmacChallenge.ID, hmacCode, ""); err != nil || !res.OK {
		t.Fatalf("verify-only manager, HMAC challenge: Verify = %+v, %v", res, err)
	}

	// Without a CodeHasher the flag changes nothing.
	flagOnly := DefaultConfig()
	flagOnly.CodeHasherVerifyOnly = true
	again, againCode, err := NewManager(client, flagOnly).Create(ctx, req)
	if err != nil || !isBuiltinCodeHash(again.CodeHash) {
		t.Fatalf("flag without a hasher: Create = %+v, %v", again, err)
	}
	if res, err := plain.Verify(ctx, again.ID, againCode, ""); err != nil || !res.OK {
		t.Fatalf("flag without a hasher: Verify = %+v, %v", res, err)
	}
}

func TestManager_UnrecognisedStoredValueIsNotAWrongCode(t *testing.T) {
	cases := map[string]func(good string) string{
		"unknown version": func(good string) string { return strings.Replace(good, "$v1$", "$v9$", 1) },
		"malformed mac":   func(good string) string { return good[:len(good)-5] },
		"other scheme":    func(string) string { return "plain$123456" },
		"no scheme":       func(string) string { return "corrupt" },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			mr, client := setupMiniRedis(t)
			defer mr.Close()
			defer func() { _ = client.Close() }()
			ctx := context.Background()

			cfg := DefaultConfig()
			cfg.CodeHasher = newTestHMACHasher(t)
			m := NewManager(client, cfg)
			ch, code, err := m.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
			if err != nil {
				t.Fatal(err)
			}
			record := storedChallenge(t, m, ch.ID)
			record.CodeHash = damage(record.CodeHash)
			if err := m.cache.Set(ctx, ch.ID, record, time.Minute); err != nil {
				t.Fatal(err)
			}

			res, err := m.Verify(ctx, ch.ID, code, "")
			if res == nil || res.OK || res.Reason != ReasonBackendUnavailable || res.RemainingAttempts != nil {
				t.Fatalf("Verify = %+v, %v; want reason %q", res, err, ReasonBackendUnavailable)
			}
			for _, target := range []error{ErrCodeHasher, ErrBackendUnavailable, ErrCodeHashFormat} {
				if !errors.Is(err, target) {
					t.Errorf("error %q does not match %q", err, target)
				}
			}
			if errors.Is(err, ErrLockUnavailable) {
				t.Errorf("error %q is reported as retryable lock contention", err)
			}
			assertChallengeUntouched(t, m, client, record)
		})
	}
}

// assertChallengeUntouched checks that a verification which could not decide
// left no trace: no attempt, the same stored value, no user lock, and the
// per-challenge lock released.
func assertChallengeUntouched(t *testing.T, m *Manager, client *redis.Client, want Challenge) {
	t.Helper()
	ctx := context.Background()
	after := storedChallenge(t, m, want.ID)
	if after.Attempts != 0 || after.CodeHash != want.CodeHash {
		t.Fatalf("the record changed: attempts = %d, hash = %q", after.Attempts, after.CodeHash)
	}
	if locked, err := m.IsUserLockedE(ctx, want.UserID); err != nil || locked {
		t.Fatalf("user lock = %v, %v", locked, err)
	}
	if n, err := client.Exists(ctx, m.config.VerifyLockPrefix+want.ID).Result(); err != nil || n != 0 {
		t.Fatalf("verification lock still held: %d, %v", n, err)
	}
}

// stubCodeHasher is a CodeHasher whose answers a test sets.
type stubCodeHasher struct {
	hash        func(ctx context.Context, challengeID, code string) (string, error)
	verify      func(ctx context.Context, challengeID, stored, code string) (bool, error)
	hashCalls   int
	verifyCalls int
	lastID      string
}

func (s *stubCodeHasher) Hash(ctx context.Context, challengeID, code string) (string, error) {
	s.hashCalls++
	s.lastID = challengeID
	return s.hash(ctx, challengeID, code)
}

func (s *stubCodeHasher) Verify(ctx context.Context, challengeID, stored, code string) (bool, error) {
	s.verifyCalls++
	s.lastID = challengeID
	return s.verify(ctx, challengeID, stored, code)
}

func redisKeyCount(t *testing.T, client *redis.Client) int {
	t.Helper()
	keys, err := client.Keys(context.Background(), "*").Result()
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func TestManager_Create_CodeHasherFailure(t *testing.T) {
	boom := errors.New("kms unreachable")
	cases := map[string]struct {
		hash    func(context.Context, string, string) (string, error)
		wantErr error
	}{
		"hasher error":      {func(context.Context, string, string) (string, error) { return "", boom }, boom},
		"empty value":       {func(context.Context, string, string) (string, error) { return "", nil }, ErrCodeHasher},
		"argon2 phc form":   {func(context.Context, string, string) (string, error) { return "$argon2id$x", nil }, ErrCodeHasher},
		"argon2 colon form": {func(context.Context, string, string) (string, error) { return "v1:abc", nil }, ErrCodeHasher},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mr, client := setupMiniRedis(t)
			defer mr.Close()
			defer func() { _ = client.Close() }()

			stub := &stubCodeHasher{hash: tc.hash}
			cfg := DefaultConfig()
			cfg.CodeHasher = stub
			m := NewManager(client, cfg)

			ch, code, err := m.Create(context.Background(), CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
			if ch != nil || code != "" {
				t.Fatalf("Create returned a challenge (%v) or a code (%q) with a failed hasher", ch, code)
			}
			if !errors.Is(err, tc.wantErr) || !errors.Is(err, ErrCodeHasher) || !strings.Contains(err.Error(), "failed to hash code") {
				t.Fatalf("Create error = %v", err)
			}
			if stub.hashCalls != 1 || !strings.HasPrefix(stub.lastID, "ch_") {
				t.Fatalf("Hash calls = %d, challenge id = %q", stub.hashCalls, stub.lastID)
			}
			if n := redisKeyCount(t, client); n != 0 {
				t.Fatalf("%d Redis keys were written", n)
			}
		})
	}
}

func TestManager_Verify_CodeHasherFailure(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	boom := errors.New("kms unreachable")
	fail := true
	stub := &stubCodeHasher{
		hash: func(_ context.Context, id, code string) (string, error) { return "stub$" + id + "$" + code, nil },
		verify: func(_ context.Context, id, stored, code string) (bool, error) {
			if fail {
				return false, boom
			}
			return stored == "stub$"+id+"$"+code, nil
		},
	}
	cfg := DefaultConfig()
	cfg.CodeHasher = stub
	m := NewManager(client, cfg)
	ch, code, err := m.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}

	// More failures than MaxAttempts: none of them may count.
	for i := 0; i < cfg.MaxAttempts+2; i++ {
		res, err := m.Verify(ctx, ch.ID, code, "")
		if res == nil || res.OK || res.Reason != ReasonBackendUnavailable || res.RemainingAttempts != nil {
			t.Fatalf("Verify = %+v, %v", res, err)
		}
		if !errors.Is(err, ErrCodeHasher) || !errors.Is(err, ErrBackendUnavailable) || !errors.Is(err, boom) {
			t.Fatalf("Verify error = %v", err)
		}
	}
	assertChallengeUntouched(t, m, client, *ch)
	if stub.lastID != ch.ID {
		t.Fatalf("Verify was given challenge id %q, want %q", stub.lastID, ch.ID)
	}

	// Once the hasher answers again the same challenge verifies.
	fail = false
	res, err := m.Verify(ctx, ch.ID, code, "")
	if err != nil || !res.OK {
		t.Fatalf("Verify after recovery = %+v, %v", res, err)
	}
}

// A hasher that gives up because the caller's context ended is reported like
// every other abandoned verification, not as a hasher fault.
func TestManager_Verify_CodeHasherContextError(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &stubCodeHasher{
		hash: func(_ context.Context, id, code string) (string, error) { return "stub$" + code, nil },
		verify: func(ctx context.Context, _, _, _ string) (bool, error) {
			cancel()
			return false, ctx.Err()
		},
	}
	cfg := DefaultConfig()
	cfg.CodeHasher = stub
	m := NewManager(client, cfg)
	ch, code, err := m.Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Verify(ctx, ch.ID, code, "")
	if res == nil || res.OK || res.Reason != ReasonLockContention {
		t.Fatalf("Verify = %+v, %v", res, err)
	}
	if !errors.Is(err, ErrLockUnavailable) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrCodeHasher) {
		t.Fatalf("Verify error = %v", err)
	}
	if got := storedChallenge(t, m, ch.ID).Attempts; got != 0 {
		t.Fatalf("attempts = %d, want 0", got)
	}
}

// With a CodeHasher set, built-in values never reach it and its own values
// never reach Argon2.
func TestManager_VerifyDispatchesOnStoredForm(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	stub := &stubCodeHasher{
		hash:   func(_ context.Context, _, code string) (string, error) { return "stub$" + code, nil },
		verify: func(_ context.Context, _, stored, code string) (bool, error) { return stored == "stub$"+code, nil },
	}
	cfg := DefaultConfig()
	cfg.CodeHasher = stub
	m := NewManager(client, cfg)

	argon, argonCode, err := NewManager(client, DefaultConfig()).Create(ctx, CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := m.Verify(ctx, argon.ID, argonCode, ""); err != nil || !res.OK {
		t.Fatalf("Argon2 challenge: Verify = %+v, %v", res, err)
	}
	if stub.verifyCalls != 0 {
		t.Fatalf("the CodeHasher was asked to verify a built-in value %d times", stub.verifyCalls)
	}

	own, ownCode, err := m.Create(ctx, CreateRequest{UserID: "u2", Channel: ChannelEmail, Destination: "b@example.com", Purpose: "login"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := m.Verify(ctx, own.ID, ownCode, ""); err != nil || !res.OK {
		t.Fatalf("CodeHasher challenge: Verify = %+v, %v", res, err)
	}
	if stub.verifyCalls != 1 {
		t.Fatalf("CodeHasher.Verify calls = %d, want 1", stub.verifyCalls)
	}
}

func benchmarkCreateVerify(b *testing.B, hasher CodeHasher) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("failed to start miniredis: %v", err)
	}
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.CodeHasher = hasher
	m := NewManager(client, cfg)
	req := CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch, code, err := m.Create(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		res, err := m.Verify(ctx, ch.ID, code, "")
		if err != nil || !res.OK {
			b.Fatalf("Verify = %+v, %v", res, err)
		}
	}
}

// BenchmarkCreateVerify measures one Create followed by one successful Verify
// against an in-process Redis, with the built-in Argon2 hasher and with the
// HMAC hasher. The Redis round trips are the same in both.
func BenchmarkCreateVerify(b *testing.B) {
	b.Run("argon2", func(b *testing.B) { benchmarkCreateVerify(b, nil) })
	b.Run("hmac-sha256", func(b *testing.B) { benchmarkCreateVerify(b, newTestHMACHasher(b)) })
}

// BenchmarkCodeHash measures the hashing alone: one Hash and one Verify.
func BenchmarkCodeHash(b *testing.B) {
	ctx := context.Background()
	b.Run("argon2", func(b *testing.B) {
		h := NewManager(nil, DefaultConfig()).argon2Hasher
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			stored, err := h.Hash("123456")
			if err != nil || !h.Verify(stored, "123456") {
				b.Fatal("argon2 round trip failed")
			}
		}
	})
	b.Run("hmac-sha256", func(b *testing.B) {
		h := newTestHMACHasher(b)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			stored, err := h.Hash(ctx, "ch_0123456789abcdefghijkl", "123456")
			if err != nil {
				b.Fatal(err)
			}
			if ok, err := h.Verify(ctx, "ch_0123456789abcdefghijkl", stored, "123456"); err != nil || !ok {
				b.Fatal("hmac round trip failed")
			}
		}
	})
}
