package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// JSON object key order is not a tool-schema change. Preserve array order and
// exact JSON numbers, but avoid restarting the official thread (and losing its
// cache prefix/turn state) when a client serializes the same schema differently.
func officialResponseSignature(input officialResponseInput) (string, error) {
	raw, err := json.Marshal([]any{input.Model, input.Instructions, input.Tools})
	if err != nil {
		return "", err
	}
	canonical, err := canonicalGatewayCacheJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
