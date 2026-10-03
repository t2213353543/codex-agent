package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Mirror Sub2API's separate HTTP and WS session-header projections. The raw
// client cache key is captured before account scoping; hashing the prepared key
// again would create a different cache route. Native identity cardinality stays
// intact; only session/thread identifiers participate in this cache namespace.
func applyGatewayCacheHeaders(headers http.Header, base, endpoint, keyID string, account Account, rawCacheKey string, settings Settings, websocket bool) {
	if !settings.PromptCacheKeyEnabled || headers == nil || keyID == "" ||
		!gatewayIsChatGPTCodexBackend(base) || (endpoint != "/responses" && endpoint != "/responses/compact") {
		return
	}
	original := headers.Clone()
	read := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(original.Get(name)); value != "" {
				return value
			}
		}
		return ""
	}
	// Canonicalization has already removed Connection-nominated identities.
	// Do not read from the unfiltered inbound request to resurrect hop headers.
	for _, pair := range [][2]string{{"session-id", "session"}, {"thread-id", "thread"}, {"thread_id", "thread"}} {
		if value := original.Get(pair[0]); strings.TrimSpace(value) != "" {
			headers.Set(pair[0], gatewayScopeCacheIdentityValue(account, keyID, pair[1], value))
		}
	}
	// The CLI repeats the same session/thread IDs in this JSON header. Apply
	// the same kinds while retaining opaque members, precision, escape spelling,
	// and real installation/turn/window/request identities byte-for-byte.
	if raw := []byte(original.Get("X-Codex-Turn-Metadata")); json.Valid(raw) {
		edits := gatewayCacheMetadataEdits(raw, 0, func(kind, value string) string {
			return gatewayScopeCacheIdentityValue(account, keyID, kind, value)
		}, nil)
		if len(edits) > 0 {
			headers.Set("X-Codex-Turn-Metadata", string(gatewayApplyJSONEdits(raw, edits, gatewayJSONEditedLength(raw, edits))))
		}
	}
	headers.Del("session_id")
	headers.Del("conversation_id")
	if websocket {
		session := read("session-id", "session_id", "conversation_id")
		if session == "" {
			session = rawCacheKey
		}
		if session != "" {
			headers.Set("session_id", gatewayUpstreamCacheSession(keyID, account, session))
		}
		if conversation := read("conversation_id"); conversation != "" {
			headers.Set("conversation_id", gatewayUpstreamCacheSession(keyID, account, conversation))
		}
		return
	}
	if rawCacheKey != "" {
		value := gatewayUpstreamCacheSession(keyID, account, rawCacheKey)
		headers.Set("session_id", value)
		headers.Set("conversation_id", value)
	} else if endpoint == "/responses/compact" {
		if session := read("session-id", "session_id", "conversation_id"); session != "" {
			headers.Set("session_id", gatewayUpstreamCacheSession(keyID, account, session))
		}
	}
}

// Sub2API strips unsupported retention for OAuth, and options from generic
// compatibility clients. Native CLI options remain caller-owned; no default TTL,
// explicit breakpoint or store policy is injected by cache optimization.
func normalizeGatewayCacheOptions(r *http.Request, fields map[string]json.RawMessage) bool {
	changed := false
	if _, exists := fields["prompt_cache_retention"]; exists {
		delete(fields, "prompt_cache_retention")
		changed = true
	}
	native := r != nil && isGatewayNativeUserAgent(r.UserAgent(), r.Header.Get("Originator"))
	if !native {
		if _, exists := fields["prompt_cache_options"]; exists {
			delete(fields, "prompt_cache_options")
			changed = true
		}
	}
	return changed
}
