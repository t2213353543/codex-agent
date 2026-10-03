package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var errBPSOpaqueOwnerConflict = errors.New("encrypted history has conflicting account ownership")

// This index records only ciphertext returned by a completed gateway response.
// It is a routing proof within one authenticated API key and inference mode,
// never permission to use a different account or to discard opaque history.
// Ownership proofs expire after the same hour as the bounded response cache.
func bpsOpaqueOwnerKey(keyID, mode, cipher string) string {
	digest := sha256.Sum256([]byte(cipher)) // Decode JSON strings, never base64.
	framed, _ := json.Marshal([]string{"codex-agent:bps:opaque-owner:v1", keyID, mode, hex.EncodeToString(digest[:])})
	key := sha256.Sum256(framed)
	return hex.EncodeToString(key[:])
}

func bpsOpaqueItemOwnerKeys(keyID, mode string, items []json.RawMessage) []string {
	seen := make(map[string]bool)
	var keys []string
	for _, raw := range items {
		if !json.Valid(raw) {
			continue
		}
		fields := gatewayValidatedJSONFieldSpans(raw, "type", "encrypted_content")
		kind := gatewayJSONSpanString(raw, fields[0])
		if kind != "reasoning" && kind != "compaction" {
			continue
		}
		cipher := gatewayJSONSpanString(raw, fields[1])
		if cipher == "" {
			continue
		}
		key := bpsOpaqueOwnerKey(keyID, mode, cipher)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

func (p *Proxy) bpsResolveOpaqueOwner(ctx context.Context, keyID string, body []byte, previousOwner string) (string, error) {
	if !gatewayUnboundedPayload(ctx) {
		return previousOwner, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mode, _, err := gatewayHistoryMode(ctx)
	if err != nil {
		return "", err
	}
	// Expansion already validated the object. Only actual input items are
	// inspected; opaque-looking tool arguments or ordinary text are not proofs.
	if !json.Valid(body) {
		return "", errors.New("request must be valid JSON")
	}
	input := gatewayValidatedJSONFieldSpans(body, "input", "")[0]
	if input.end == 0 {
		return previousOwner, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(body[input.start:input.end], &items) != nil {
		return previousOwner, nil // Prepare owns input schema validation.
	}
	keys := bpsOpaqueItemOwnerKeys(keyID, mode, items)
	if len(keys) == 0 {
		return previousOwner, nil
	}
	var owners []string
	if db := p.Store.DB; db != nil {
		if db.Redis == nil {
			return "", errors.New("encrypted history ownership storage unavailable")
		}
		redisKeys := make([]string, len(keys))
		for i, key := range keys {
			redisKeys[i] = db.Namespace + bpsOpaqueOwnerPrefix + key
		}
		values, err := db.Redis.Eval(ctx, bpsLookupOpaqueOwners, redisKeys).Slice()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("encrypted history ownership storage unavailable")
		}
		if bpsHistoryLegacyReadDeadline() != 0 {
			legacyKeys := make([]string, len(keys))
			for i, key := range keys {
				legacyKeys[i] = db.Namespace + "bps:opaque-owner:" + key
			}
			legacy, legacyErr := db.Redis.Eval(ctx, bpsLookupOpaqueOwners, legacyKeys).Slice()
			if legacyErr != nil || len(legacy) != len(keys) {
				return "", errors.New("encrypted history ownership storage unavailable")
			}
			for i := range values {
				if values[i] == nil {
					values[i] = legacy[i]
				}
			}
		}
		if len(values) != len(keys) {
			return "", errors.New("encrypted history ownership storage invalid")
		}
		for _, value := range values {
			if value == nil {
				continue
			}
			owner, ok := value.(string)
			if !ok || owner == "" {
				return "", errors.New("encrypted history ownership storage invalid")
			}
			owners = append(owners, owner)
		}
	} else {
		h := &p.BPS.history
		h.mu.Lock()
		for _, key := range keys {
			if owner := h.opaqueOwners[key]; owner != "" && h.opaqueExpires[key] > time.Now().Unix() {
				owners = append(owners, owner)
			}
		}
		h.mu.Unlock()
	}
	owner := previousOwner
	for _, observed := range owners {
		if owner != "" && owner != observed {
			return "", errBPSOpaqueOwnerConflict
		}
		owner = observed
	}
	return owner, nil
}

// The same transaction publishes the successful response history and the
// ownership proofs for its actual final output. It rejects all conflicting
// keys before writing any value, including when the conflict is last in a batch.
const bpsLookupOpaqueOwners = `
local result={}
for i=1,#KEYS do
 local value=redis.call('GET',KEYS[i])
 result[i]=value or false
end
return result`

func (p *Proxy) bpsSaveHistoryWithOpaqueOwners(ctx context.Context, keyID, responseID string, record bpsHistoryRecord, actualOutput []json.RawMessage) error {
	if !gatewayUnboundedPayload(ctx) {
		return p.bpsSaveHistory(ctx, keyID, responseID, record)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	mode, _, err := gatewayHistoryMode(ctx)
	if err != nil {
		return err
	}
	keys := bpsOpaqueItemOwnerKeys(keyID, mode, actualOutput)
	if len(keys) > 0 && (keyID == "" || responseID == "" || record.Owner == "") {
		return errors.New("completed response ownership is missing")
	}
	return p.bpsSaveHistoryRecord(ctx, keyID, responseID, record, keys)
}
