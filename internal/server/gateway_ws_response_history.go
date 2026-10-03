package server

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

const maxGatewayWSCompletedResponses = 256

// Response IDs are retained as connection-local digests, never response content.
// Legacy routes keep only a recent FIFO window; the lightweight official route
// retains policy for every response until the connection closes so a long-lived
// client can continue an earlier branch without losing its cache identity.
type gatewayWSResponseHistory struct {
	ids       map[[32]byte]gatewayWSCachePolicy
	order     [][32]byte
	next      int
	unbounded bool
}

// The enclosing gatewayWSTurns mutex guards both history and pending turns.
func (h *gatewayWSResponseHistory) contains(id string) bool {
	if id == "" || len(id) > 1024 && !h.unbounded {
		return false
	}
	_, exists := h.ids[sha256.Sum256([]byte(id))]
	return exists
}

func (h *gatewayWSResponseHistory) add(id string, policies ...gatewayWSCachePolicy) {
	if id == "" || len(id) > 1024 && !h.unbounded {
		return
	}
	digest := sha256.Sum256([]byte(id))
	if _, exists := h.ids[digest]; exists {
		return // Duplicates must not pin an old ID by refreshing its FIFO position.
	}
	if h.ids == nil {
		h.ids = make(map[[32]byte]gatewayWSCachePolicy)
	}
	if !h.unbounded {
		if len(h.order) < maxGatewayWSCompletedResponses {
			h.order = append(h.order, digest)
		} else {
			delete(h.ids, h.order[h.next])
			h.order[h.next] = digest
			h.next = (h.next + 1) % maxGatewayWSCompletedResponses
		}
	}
	var policy gatewayWSCachePolicy
	if len(policies) > 0 {
		policy = policies[0]
	}
	h.ids[digest] = policy
}

// Policy has exactly the same lifetime as its enclosing response identity.
func (h *gatewayWSResponseHistory) cachePolicy(id string) (gatewayWSCachePolicy, bool) {
	if id == "" || len(id) > 1024 && !h.unbounded {
		return gatewayWSCachePolicy{}, false
	}
	value, exists := h.ids[sha256.Sum256([]byte(id))]
	return value, exists
}

// An outgoing cache value is retained as a digest, not request metadata.
type gatewayWSCachePolicy struct {
	enabled        bool
	tracked        bool
	kind           string
	keyDigest      [32]byte
	rewriteTracked bool
	rewriteDigest  [32]byte
}

func gatewayWSPromptCacheDigest(payload []byte) [32]byte {
	if !json.Valid(payload) {
		return [32]byte{}
	}
	span := gatewayValidatedJSONFieldSpans(payload, "prompt_cache_key", "")[0]
	var raw []byte
	if span.end != 0 {
		raw = payload[span.start:span.end]
	}
	var value *string
	if json.Unmarshal(raw, &value) == nil {
		if value == nil {
			return sha256.Sum256([]byte{0})
		}
		return sha256.Sum256(append([]byte{1}, []byte(*value)...))
	}
	if len(raw) == 0 {
		return sha256.Sum256([]byte{0})
	}
	return sha256.Sum256(append([]byte{2}, raw...))
}

// Only policy that can rewrite model context is bound to a response chain.
// Keep its fingerprint, never the configured prompt text, in connection-local
// history. Disabled/empty prompts and default/prepend spelling are equivalent.
func gatewayWSRewritePolicyDigest(settings Settings) [32]byte {
	var policy struct {
		Compatibility bool
		Prompt        string
		Mode          string
	}
	policy.Compatibility = settings.CompatibilityNormalizationEnabled
	if settings.BuiltinPromptEnabled && strings.TrimSpace(settings.BuiltinPrompt) != "" {
		policy.Prompt = settings.BuiltinPrompt
		policy.Mode = settings.BuiltinPromptMode
		if policy.Mode != "append" && policy.Mode != "if_empty" {
			policy.Mode = "prepend"
		}
	}
	raw, _ := json.Marshal(policy)
	return sha256.Sum256(raw)
}

// A response may omit a redundant stream_id after response.created established
// its exact identity. Only a unique current head can supply that missing lane;
// explicit lanes, unidentified/queued turns and retired responses never do.
// Call with t.mu held. The second result records an exact active-head match,
// including the default lane; the third rejects conflicting/ambiguous identity.
func (t *gatewayWSTurns) responseLaneLocked(streamID, responseID string, explicit bool) (string, bool, bool) {
	if responseID == "" {
		return streamID, false, true
	}
	lane, found := streamID, false
	for name, queue := range t.pending {
		if len(queue) == 0 || queue[0].responseID != responseID {
			continue
		}
		if found {
			return "", false, false
		}
		lane, found = name, true
	}
	if explicit {
		// An explicit lane may not claim an identity already bound to another
		// active lane, even before its own response.created event arrives.
		if found && lane != streamID {
			return "", false, false
		}
		return streamID, found, true
	}
	return lane, found, true
}
