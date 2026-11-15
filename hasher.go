package challenge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// CodeHasher turns a code into the value stored with a challenge and checks a
// submitted code against a stored value. A nil CodeHasher in [Config] keeps
// the built-in Argon2 behaviour.
//
// An implementation must be safe for concurrent use. Its stored values must
// not be in a built-in Argon2 form (see [Config.CodeHasher]): such values are
// always verified by the built-in hasher, so [Manager.Create] refuses them.
type CodeHasher interface {
	// Hash returns the value to store. challengeID lets an implementation
	// bind the digest to the challenge, so equal codes do not produce equal
	// stored values.
	Hash(ctx context.Context, challengeID, code string) (stored string, err error)
	// Verify reports whether code matches stored. It must compare in constant
	// time and must return an error (not false) for a stored value it does
	// not recognise. A false result is counted as a wrong code and consumes
	// an attempt; an error consumes none.
	Verify(ctx context.Context, challengeID, stored, code string) (bool, error)
}

// ErrCodeHasher reports that the configured [CodeHasher] returned an error or
// an unusable value.
//
// From [Manager.Create] it means nothing was stored. From [Manager.Verify] the
// result is unknown rather than negative: no attempt is consumed and the
// challenge is left as it was; the error also matches [ErrBackendUnavailable],
// and the result carries [ReasonBackendUnavailable].
var ErrCodeHasher = errors.New("challenge: code hasher failed")

// ErrCodeHashFormat is returned by the hasher from [NewHMACCodeHasher] when a
// stored value is not one it wrote: another scheme, an unknown version, or a
// damaged value.
var ErrCodeHashFormat = errors.New("challenge: unrecognised stored code hash")

// HMACCodeHasherMinKeyLen is the shortest key [NewHMACCodeHasher] and
// [NewHMACCodeHasherWithPrevious] accept, for the primary key and for every
// previous key.
const HMACCodeHasherMinKeyLen = 32

// HMACCodeHasherMaxPreviousKeys is the largest number of previous keys
// [NewHMACCodeHasherWithPrevious] accepts. Every verification computes one MAC
// per key, and a key is needed only for one challenge lifetime after it stops
// being written, so a rotation needs one previous key at a time.
const HMACCodeHasherMaxPreviousKeys = 4

const (
	hmacCodeHashScheme  = "hmac-sha256"
	hmacCodeHashVersion = "v1"
	// hmacCodeHashLabel separates this use of the key from any other HMAC
	// computed with the same key. It is part of the v1 format.
	hmacCodeHashLabel = "challenge-kit/code-hash/v1"
)

type hmacCodeHasher struct {
	// key computes the values Hash writes.
	key []byte
	// previous holds the other keys Verify accepts. It is never written to
	// after construction.
	previous [][]byte
}

// NewHMACCodeHasher returns a [CodeHasher] that stores a keyed digest of the
// code instead of an Argon2 hash.
//
// A short numeric code has too little entropy for any unkeyed hash, however
// slow, to resist an offline search by someone who can read the stored value:
// a 6-digit code has 10^6 candidates. With this hasher the stored value cannot
// be tested against candidate codes without the key, and computing it costs
// microseconds and no memory to speak of.
//
// That holds only while the key is unknown to whoever reads Redis. Keep the
// key out of the Redis instance that holds the challenges, and out of anything
// stored or backed up with it. Someone who has both the key and a stored value
// recovers the code at once.
//
// The stored value is
//
//	hmac-sha256$v1$<base64url, unpadded, of the 32-byte MAC>
//
// where the MAC is HMAC-SHA-256 under key of
//
//	"challenge-kit/code-hash/v1" || 0x00 ||
//	uint64be(len(challengeID)) || challengeID ||
//	uint64be(len(code)) || code
//
// The length prefixes make the framing unambiguous, and the challenge ID makes
// equal codes of different challenges store different values.
//
// The key must be at least [HMACCodeHasherMinKeyLen] bytes and should be
// random. It is copied. Every instance that creates or verifies the same
// challenges needs the same key. This hasher has one key: after a key change
// the challenges created under the previous key no longer match, and a
// submitted code for one of them is counted as a wrong code until it expires.
// Use [NewHMACCodeHasherWithPrevious] to change the key without that.
//
// Verify returns an error wrapping [ErrCodeHashFormat] for a stored value with
// another scheme, an unknown version or a malformed MAC.
func NewHMACCodeHasher(key []byte) (CodeHasher, error) {
	return NewHMACCodeHasherWithPrevious(key)
}

// NewHMACCodeHasherWithPrevious returns the [CodeHasher] of
// [NewHMACCodeHasher] with additional keys that are accepted on verify, so
// that the key can be changed while challenges created under another key are
// still alive.
//
// Hash computes the stored value under key alone. Verify accepts a stored
// value computed under key or under any of previous. The stored format is the
// one [NewHMACCodeHasher] documents and carries no key identifier, so Verify
// computes the MAC of the submitted code under every configured key and
// compares each with the stored one. Every key is always tried, in constant
// time each, so neither the result nor the time taken tells which key matched.
// A code that matches under none of them is a wrong code, as with one key.
//
// "Previous" names the common use; the list is the set of keys that are
// verified but not written. During a change of key on instances that restart
// one at a time it first holds the next key, then the old one. With A the
// current key and B the new one:
//
//  1. Every instance: key A, previous B. Nothing changes in what is stored;
//     every instance learns to verify B.
//  2. Every instance: key B, previous A. Instances switch one by one. Those
//     still in stage 1 verify the B values the switched ones write, and the
//     switched ones verify A values.
//  3. No sooner than one Config.Expiry after the last instance left stage 1,
//     every instance: key B, no previous key.
//
// Going straight from "key A" to "key B, previous A" skips stage 1 and is
// safe only when all instances restart together: an instance that still has
// only A counts a correct code for a challenge written under B as a wrong
// code, which consumes an attempt.
//
// Every key, previous ones included, must be at least
// [HMACCodeHasherMinKeyLen] bytes; all are copied. At most
// [HMACCodeHasherMaxPreviousKeys] previous keys are accepted. A previous key
// equal to key or to an earlier previous key is accepted and has no effect. A
// previous key is as sensitive as the primary one for as long as values
// written under it exist: keep it out of Redis as well, and remove it once
// those challenges have expired.
//
// With Config.CodeHasherVerifyOnly set, Create stores Argon2 hashes and Verify
// accepts values written under any of the keys.
func NewHMACCodeHasherWithPrevious(key []byte, previous ...[]byte) (CodeHasher, error) {
	if len(key) < HMACCodeHasherMinKeyLen {
		return nil, fmt.Errorf("challenge: HMAC code hasher key is %d bytes, need at least %d", len(key), HMACCodeHasherMinKeyLen)
	}
	if len(previous) > HMACCodeHasherMaxPreviousKeys {
		return nil, fmt.Errorf("challenge: HMAC code hasher has %d previous keys, at most %d are accepted", len(previous), HMACCodeHasherMaxPreviousKeys)
	}
	h := &hmacCodeHasher{key: append([]byte(nil), key...)}
	for i, p := range previous {
		if len(p) < HMACCodeHasherMinKeyLen {
			return nil, fmt.Errorf("challenge: HMAC code hasher previous key %d is %d bytes, need at least %d", i+1, len(p), HMACCodeHasherMinKeyLen)
		}
	}
	for _, p := range previous {
		if h.hasKey(p) {
			continue
		}
		h.previous = append(h.previous, append([]byte(nil), p...))
	}
	return h, nil
}

// hasKey reports whether k is already one of the hasher's keys. It runs at
// construction only.
func (h *hmacCodeHasher) hasKey(k []byte) bool {
	if hmac.Equal(h.key, k) {
		return true
	}
	for _, p := range h.previous {
		if hmac.Equal(p, k) {
			return true
		}
	}
	return false
}

func (h *hmacCodeHasher) Hash(_ context.Context, challengeID, code string) (string, error) {
	if challengeID == "" {
		return "", errors.New("challenge: HMAC code hasher: empty challenge id")
	}
	if code == "" {
		return "", errors.New("challenge: HMAC code hasher: empty code")
	}
	return hmacCodeHashScheme + "$" + hmacCodeHashVersion + "$" +
		base64.RawURLEncoding.EncodeToString(codeMAC(h.key, challengeID, code)), nil
}

func (h *hmacCodeHasher) Verify(_ context.Context, challengeID, stored, code string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != hmacCodeHashScheme {
		return false, fmt.Errorf("%w: not an %s value", ErrCodeHashFormat, hmacCodeHashScheme)
	}
	if parts[1] != hmacCodeHashVersion {
		return false, fmt.Errorf("%w: unknown %s version", ErrCodeHashFormat, hmacCodeHashScheme)
	}
	// The length is checked first: the decoder skips line breaks, and a
	// stored value is accepted only in the exact form Hash writes.
	if len(parts[2]) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false, fmt.Errorf("%w: malformed %s MAC", ErrCodeHashFormat, hmacCodeHashScheme)
	}
	want, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(want) != sha256.Size {
		return false, fmt.Errorf("%w: malformed %s MAC", ErrCodeHashFormat, hmacCodeHashScheme)
	}
	// Every key is tried, with no early exit, and the results are combined
	// without a branch: the time taken does not depend on which key matched,
	// or on whether one did.
	matched := subtle.ConstantTimeCompare(codeMAC(h.key, challengeID, code), want)
	for _, p := range h.previous {
		matched |= subtle.ConstantTimeCompare(codeMAC(p, challengeID, code), want)
	}
	return matched == 1, nil
}

// codeMAC computes the v1 MAC of challengeID and code under key.
func codeMAC(key []byte, challengeID, code string) []byte {
	mac := hmac.New(sha256.New, key)
	var n [8]byte
	mac.Write([]byte(hmacCodeHashLabel))
	mac.Write([]byte{0})
	binary.BigEndian.PutUint64(n[:], uint64(len(challengeID)))
	mac.Write(n[:])
	mac.Write([]byte(challengeID))
	binary.BigEndian.PutUint64(n[:], uint64(len(code)))
	mac.Write(n[:])
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

// isBuiltinCodeHash reports whether stored is in a form the built-in Argon2
// hasher reads: the PHC form, or the "salt:hash" form Create writes when no
// CodeHasher is set. The test is on shape only, so that a damaged built-in
// value is still handled by the built-in hasher, as it always was.
func isBuiltinCodeHash(stored string) bool {
	return strings.HasPrefix(stored, "$argon2id$") || strings.Contains(stored, ":")
}
