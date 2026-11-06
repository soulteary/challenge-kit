package challenge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// A pending record cannot be redeemed, even if activation and cleanup both
// fail. Promotion and deletion of the previous active record share one Redis
// operation. Snapshot comparison also prevents a stale activation from reviving
// a record that verification, revocation, or another activation changed.
const strictActivationScript = `
local current = redis.call('GET', KEYS[2])
if not current or current ~= ARGV[2] then
 return redis.error_reply('challenge changed before activation')
end
local ttl = redis.call('PTTL', KEYS[2])
if ttl <= 0 then return redis.error_reply('challenge expired before activation') end
local previous = redis.call('GET', KEYS[1])
if ARGV[5] == 'active' then
 if previous == ARGV[1] then return '' end
 return redis.error_reply('challenge already activated outside this index')
end
if previous and previous ~= ARGV[1] then
 redis.call('DEL', ARGV[4] .. previous)
end
redis.call('SET', KEYS[2], ARGV[3], 'PX', ttl)
redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
return previous or ''
`

func (m *Manager) swapStrictActive(ctx context.Context, ch *Challenge) (string, error) {
	if ch == nil {
		return "", fmt.Errorf("nil challenge")
	}
	var persisted Challenge
	snapshot, err := m.cache.GetSnapshot(ctx, ch.ID, &persisted)
	if err != nil {
		return "", fmt.Errorf("%w: read activation record: %v", ErrBackendUnavailable, err)
	}
	if persisted.ID != ch.ID || (persisted.ActivationState != ActivationPending && persisted.ActivationState != ActivationActive) {
		return "", fmt.Errorf("%w: invalid activation state", ErrBackendUnavailable)
	}
	previousState := persisted.ActivationState
	persisted.ActivationState = ActivationActive
	payload, err := json.Marshal(persisted)
	if err != nil {
		return "", fmt.Errorf("%w: encode activation record", ErrBackendUnavailable)
	}
	key := m.strictActiveIndexKey(&persisted)
	if m.client == nil {
		return "", fmt.Errorf("%w: %v", ErrBackendUnavailable, ErrNilClient)
	}
	previous, err := m.client.Eval(ctx, strictActivationScript, []string{key, m.config.ChallengeKeyPrefix + ch.ID}, ch.ID, snapshot, string(payload), m.config.ChallengeKeyPrefix, string(previousState)).Text()
	if err != nil {
		return "", fmt.Errorf("%w: activate challenge: %v", ErrBackendUnavailable, err)
	}
	ch.ActivationState = ActivationActive
	return previous, nil
}

func (m *Manager) strictActiveIndexKey(ch *Challenge) string {
	identity, _ := json.Marshal([]string{ch.UserID, ch.Purpose, string(ch.Channel), ch.Destination})
	sum := sha256.Sum256(identity)
	return m.config.ActiveIndexPrefix + "v2:" + hex.EncodeToString(sum[:])
}

// A lost activation reply is ambiguous: cleanup must not delete a record that
// was committed active. General Revoke remains available for explicit revocation.
func (m *Manager) revokeStrictPending(ctx context.Context, id string) error {
	var ch Challenge
	snapshot, err := m.cache.GetSnapshot(ctx, id, &ch)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read pending challenge: %v", ErrBackendUnavailable, err)
	}
	if ch.ActivationState != ActivationPending {
		return nil
	}
	if err = m.cache.CompareAndDelete(ctx, id, snapshot); err != nil && !isNotFound(err) {
		return fmt.Errorf("%w: revoke pending: %v", ErrBackendUnavailable, err)
	}
	return nil
}
