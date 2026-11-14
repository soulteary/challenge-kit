package challenge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

// HMACCodeHasherMinKeyLen is the shortest key [NewHMACCodeHasher] accepts.
const HMACCodeHasherMinKeyLen = 32

const (
	hmacCodeHashScheme  = "hmac-sha256"
	hmacCodeHashVersion = "v1"
	// hmacCodeHashLabel separates this use of the key from any other HMAC
	// computed with the same key. It is part of the v1 format.
	hmacCodeHashLabel = "challenge-kit/code-hash/v1"
)

type hmacCodeHasher struct {
	key []byte
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
// challenges needs the same key. There is one key: after a key change the
// challenges created under the previous key no longer match, and a submitted
// code for one of them is counted as a wrong code until it expires.
//
// Verify returns an error wrapping [ErrCodeHashFormat] for a stored value with
// another scheme, an unknown version or a malformed MAC.
func NewHMACCodeHasher(key []byte) (CodeHasher, error) {
	if len(key) < HMACCodeHasherMinKeyLen {
		return nil, fmt.Errorf("challenge: HMAC code hasher key is %d bytes, need at least %d", len(key), HMACCodeHasherMinKeyLen)
	}
	return &hmacCodeHasher{key: append([]byte(nil), key...)}, nil
}

func (h *hmacCodeHasher) Hash(_ context.Context, challengeID, code string) (string, error) {
	if challengeID == "" {
		return "", errors.New("challenge: HMAC code hasher: empty challenge id")
	}
	if code == "" {
		return "", errors.New("challenge: HMAC code hasher: empty code")
	}
	return hmacCodeHashScheme + "$" + hmacCodeHashVersion + "$" +
		base64.RawURLEncoding.EncodeToString(h.mac(challengeID, code)), nil
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
	return hmac.Equal(h.mac(challengeID, code), want), nil
}

func (h *hmacCodeHasher) mac(challengeID, code string) []byte {
	mac := hmac.New(sha256.New, h.key)
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
