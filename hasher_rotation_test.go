package challenge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// testHMACKeyN returns a key of the minimum length filled with b.
func testHMACKeyN(b byte) []byte {
	key := make([]byte, HMACCodeHasherMinKeyLen)
	for i := range key {
		key[i] = b
	}
	return key
}

func mustHMACHasher(t testing.TB, key []byte, previous ...[]byte) CodeHasher {
	t.Helper()
	h, err := NewHMACCodeHasherWithPrevious(key, previous...)
	if err != nil {
		t.Fatalf("NewHMACCodeHasherWithPrevious: %v", err)
	}
	return h
}

func TestHMACCodeHasherWithPrevious_WritesWithPrimaryOnly(t *testing.T) {
	ctx := context.Background()
	keyA, keyB := testHMACKey(), testHMACKeyN(0xb0)
	onlyA := mustHMACHasher(t, keyA)
	rotated := mustHMACHasher(t, keyA, keyB)

	// The primary key alone decides the stored value, so the known-answer
	// value of the single-key hasher still holds and the format is unchanged.
	const want = "hmac-sha256$v1$I85kKuzuggL4LKERjYrMKAlJq-RIVLNvnCl8YmdrG4s"
	got, err := rotated.Hash(ctx, "ch_0123456789abcdefghijkl", "123456")
	if err != nil || got != want {
		t.Fatalf("Hash with a previous key = %q, %v; want %q", got, err, want)
	}
	single, _ := onlyA.Hash(ctx, "ch_a", "123456")
	multi, _ := rotated.Hash(ctx, "ch_a", "123456")
	if single != multi {
		t.Fatalf("Hash differs with a previous key: %q and %q", single, multi)
	}
	// No previous keys is the single-key hasher.
	plain, _ := mustHMACHasher(t, keyA).Hash(ctx, "ch_a", "123456")
	if plain != single {
		t.Fatalf("Hash without previous keys = %q, want %q", plain, single)
	}
}

func TestHMACCodeHasherWithPrevious_VerifiesEveryKey(t *testing.T) {
	ctx := context.Background()
	keys := [][]byte{testHMACKeyN(0xa0), testHMACKeyN(0xb0), testHMACKeyN(0xc0), testHMACKeyN(0xd0), testHMACKeyN(0xe0)}
	if len(keys) != HMACCodeHasherMaxPreviousKeys+1 {
		t.Fatalf("the test needs %d keys", HMACCodeHasherMaxPreviousKeys+1)
	}
	all := mustHMACHasher(t, keys[0], keys[1:]...)
	unknown := mustHMACHasher(t, testHMACKeyN(0xf0))

	for i, key := range keys {
		stored, err := mustHMACHasher(t, key).Hash(ctx, "ch_a", "123456")
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := all.Verify(ctx, "ch_a", stored, "123456"); err != nil || !ok {
			t.Errorf("key %d: right code: Verify = %v, %v; want true, nil", i, ok, err)
		}
		if ok, err := all.Verify(ctx, "ch_a", stored, "123457"); err != nil || ok {
			t.Errorf("key %d: wrong code: Verify = %v, %v; want false, nil", i, ok, err)
		}
		if ok, err := all.Verify(ctx, "ch_b", stored, "123456"); err != nil || ok {
			t.Errorf("key %d: other challenge id: Verify = %v, %v; want false, nil", i, ok, err)
		}
		if ok, err := unknown.Verify(ctx, "ch_a", stored, "123456"); err != nil || ok {
			t.Errorf("key %d: hasher without the key: Verify = %v, %v; want false, nil", i, ok, err)
		}
	}

	// A value written under a key that is not configured is a wrong code, not
	// an error: the format carries no key identifier.
	stored, _ := unknown.Hash(ctx, "ch_a", "123456")
	if ok, err := all.Verify(ctx, "ch_a", stored, "123456"); err != nil || ok {
		t.Errorf("unconfigured key: Verify = %v, %v; want false, nil", ok, err)
	}
	// Format errors are reported as before.
	if _, err := all.Verify(ctx, "ch_a", "hmac-sha256$v2$AAAA", "123456"); !errors.Is(err, ErrCodeHashFormat) {
		t.Errorf("unknown version: err = %v, want ErrCodeHashFormat", err)
	}
}

func TestNewHMACCodeHasherWithPrevious_Keys(t *testing.T) {
	key := testHMACKey()
	for _, n := range []int{0, 1, 16, HMACCodeHasherMinKeyLen - 1} {
		if h, err := NewHMACCodeHasherWithPrevious(make([]byte, n), testHMACKeyN(0xb0)); err == nil || h != nil {
			t.Errorf("a %d-byte primary key was accepted", n)
		}
		h, err := NewHMACCodeHasherWithPrevious(key, testHMACKeyN(0xb0), make([]byte, n))
		if err == nil || h != nil {
			t.Errorf("a %d-byte previous key was accepted", n)
		} else if !strings.Contains(err.Error(), "previous key 2") {
			t.Errorf("the error does not name the short previous key: %v", err)
		}
	}
	if _, err := NewHMACCodeHasherWithPrevious(key, nil); err == nil {
		t.Error("a nil previous key was accepted")
	}

	previous := make([][]byte, HMACCodeHasherMaxPreviousKeys+1)
	for i := range previous {
		previous[i] = testHMACKeyN(byte(0xa0 + i))
	}
	if _, err := NewHMACCodeHasherWithPrevious(key, previous[:HMACCodeHasherMaxPreviousKeys]...); err != nil {
		t.Errorf("%d previous keys were refused: %v", HMACCodeHasherMaxPreviousKeys, err)
	}
	if h, err := NewHMACCodeHasherWithPrevious(key, previous...); err == nil || h != nil {
		t.Errorf("%d previous keys were accepted", len(previous))
	}

	// Duplicates are accepted and add no key.
	h, err := NewHMACCodeHasherWithPrevious(key, key, testHMACKeyN(0xb0), testHMACKeyN(0xb0))
	if err != nil {
		t.Fatalf("duplicate keys were refused: %v", err)
	}
	if got := len(h.(*hmacCodeHasher).previous); got != 1 {
		t.Errorf("previous keys kept = %d, want 1", got)
	}

	// Previous keys are copied.
	ctx := context.Background()
	old := testHMACKeyN(0xc0)
	stored, _ := mustHMACHasher(t, old).Hash(ctx, "ch_a", "123456")
	rotated := mustHMACHasher(t, key, old)
	for i := range old {
		old[i] = 0xff
	}
	if ok, err := rotated.Verify(ctx, "ch_a", stored, "123456"); err != nil || !ok {
		t.Errorf("after changing the caller's slice: Verify = %v, %v; want true, nil", ok, err)
	}
}

// The three stages of a key change on instances that restart one at a time:
// in every stage, each manager verifies what any manager of the same or the
// neighbouring stage creates.
func TestManager_HMACKeyRotationStages(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	req := CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"}
	keyA, keyB := testHMACKeyN(0xa0), testHMACKeyN(0xb0)

	manager := func(key []byte, previous ...[]byte) *Manager {
		cfg := DefaultConfig()
		cfg.CodeHasher = mustHMACHasher(t, key, previous...)
		return NewManager(client, cfg)
	}
	stages := []struct {
		name string
		m    *Manager
	}{
		{"A", manager(keyA)},
		{"A+B", manager(keyA, keyB)},
		{"B+A", manager(keyB, keyA)},
		{"B", manager(keyB)},
	}
	for i := 0; i+1 < len(stages); i++ {
		for _, pair := range [][2]int{{i, i + 1}, {i + 1, i}} {
			creator, verifier := stages[pair[0]], stages[pair[1]]
			ch, code, err := creator.m.Create(ctx, req)
			if err != nil {
				t.Fatalf("%s: Create: %v", creator.name, err)
			}
			if !strings.HasPrefix(ch.CodeHash, "hmac-sha256$v1$") {
				t.Fatalf("%s stored %q", creator.name, ch.CodeHash)
			}
			res, err := verifier.m.Verify(ctx, ch.ID, wrongCodeFor(code), "")
			if err == nil || res.Reason != ReasonInvalid {
				t.Errorf("created by %s, verified by %s, wrong code: Verify = %+v, %v", creator.name, verifier.name, res, err)
			}
			res, err = verifier.m.Verify(ctx, ch.ID, code, "")
			if err != nil || !res.OK {
				t.Errorf("created by %s, verified by %s: Verify = %+v, %v", creator.name, verifier.name, res, err)
			}
		}
	}

	// Skipping the first stage: an instance with only A counts a correct code
	// for a challenge written under B as a wrong code.
	ch, code, err := stages[2].m.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	res, err := stages[0].m.Verify(ctx, ch.ID, code, "")
	if err == nil || res.OK || res.Reason != ReasonInvalid {
		t.Errorf("created by B+A, verified by A: Verify = %+v, %v; want ReasonInvalid", res, err)
	}
	if got := storedChallenge(t, stages[0].m, ch.ID).Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestManager_CodeHasherVerifyOnlyWithPreviousKeys(t *testing.T) {
	mr, client := setupMiniRedis(t)
	defer mr.Close()
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	req := CreateRequest{UserID: "u1", Channel: ChannelEmail, Destination: "a@example.com", Purpose: "login"}
	keyA, keyB := testHMACKeyN(0xa0), testHMACKeyN(0xb0)

	cfg := DefaultConfig()
	cfg.CodeHasher = mustHMACHasher(t, keyB, keyA)
	cfg.CodeHasherVerifyOnly = true
	verifyOnly := NewManager(client, cfg)

	created, code, err := verifyOnly.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !isBuiltinCodeHash(created.CodeHash) {
		t.Fatalf("a verify-only manager stored %q, not a built-in Argon2 value", created.CodeHash)
	}
	if res, err := NewManager(client, DefaultConfig()).Verify(ctx, created.ID, code, ""); err != nil || !res.OK {
		t.Fatalf("manager without the hasher: Verify = %+v, %v", res, err)
	}

	for name, key := range map[string][]byte{"primary": keyB, "previous": keyA} {
		writerCfg := DefaultConfig()
		writerCfg.CodeHasher = mustHMACHasher(t, key)
		ch, code, err := NewManager(client, writerCfg).Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := verifyOnly.Verify(ctx, ch.ID, code, ""); err != nil || !res.OK {
			t.Errorf("verify-only manager, challenge under the %s key: Verify = %+v, %v", name, res, err)
		}
	}
}

func TestHMACCodeHasherWithPrevious_ConcurrentUse(t *testing.T) {
	ctx := context.Background()
	keyA, keyB, keyC := testHMACKeyN(0xa0), testHMACKeyN(0xb0), testHMACKeyN(0xc0)
	h := mustHMACHasher(t, keyC, keyA, keyB)
	storedA, _ := mustHMACHasher(t, keyA).Hash(ctx, "ch_a", "123456")
	storedB, _ := mustHMACHasher(t, keyB).Hash(ctx, "ch_a", "123456")

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				stored, err := h.Hash(ctx, "ch_a", "123456")
				if err != nil {
					errs <- "Hash: " + err.Error()
					return
				}
				for _, s := range []string{stored, storedA, storedB} {
					if ok, err := h.Verify(ctx, "ch_a", s, "123456"); err != nil || !ok {
						errs <- "Verify did not accept " + s
						return
					}
					if ok, err := h.Verify(ctx, "ch_a", s, "654321"); err != nil || ok {
						errs <- "Verify accepted a wrong code for " + s
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
