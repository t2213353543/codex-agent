package server

import "strings"

// Only the observed outbound request and response body can establish a match.
// Inbound aliases and compatibility response headers never fill missing sides.
func gatewayModelConsistency(r RequestRecord) string {
	for _, model := range []string{r.SentModel, r.ReturnedModel} {
		// The 512-byte collector backs up at most 3 bytes for UTF-8 boundaries.
		// Equal truncated prefixes or redactions cannot establish model identity.
		if strings.TrimSpace(model) == "" || len(model) >= 509 || strings.Contains(model, "[REDACTED]") {
			return "unknown"
		}
	}
	if r.SentModel == r.ReturnedModel {
		return "match"
	}
	return "mismatch"
}
