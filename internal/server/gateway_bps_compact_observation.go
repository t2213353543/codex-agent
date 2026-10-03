package server

import (
	"encoding/json"

	"codex-agent/internal/bps"
)

// Accounting eligibility is independent of usable compaction state. A complete
// identified failure may have real usage even though it cannot be continued.
// This never commits history, delivers output, or authorizes an account retry.
func recordBPSCompactResponseUsage(rec *RequestRecord, raw []byte) bool {
	metadata := bps.CompactResponseMetadata(raw)
	if len(metadata) == 0 {
		return false
	}
	var observed, root map[string]json.RawMessage
	if json.Unmarshal(metadata, &observed) != nil || json.Unmarshal(raw, &root) != nil {
		return false
	}
	if !gatewayResponseMatchesRecord(rec, observed) {
		return false
	}
	// Keep the observed root status and failure provenance, never an embedded
	// response object or opaque output. Counters use the shared thin validator.
	for _, name := range []string{"status", "error", "incomplete_details", "service_tier", "reasoning", "usage_metadata"} {
		if value, exists := root[name]; exists {
			observed[name] = value
		}
	}
	observation, err := json.Marshal(observed)
	if err != nil {
		return false
	}
	recordBPSTerminalUsage(rec, observation)
	return true
}
