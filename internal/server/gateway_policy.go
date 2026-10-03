package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// applyGatewayPolicy is shared by HTTP and WebSocket response.create requests.
// Compatibility normalization preserves message content and ordering, tools,
// storage and previous_response_id, and removes unsupported OAuth cache controls.
// The returned session uses explicit client identity or a first-user content
// anchor for local routing. Missing upstream cache keys remain absent.
func applyGatewayPolicy(r *http.Request, raw []byte, keyID string, settings Settings) ([]byte, string, error) {
	if len(raw) > maxBody && !officialRequestUnbounded(r) && !(r != nil && gatewayUnboundedPayload(r.Context())) {
		return nil, "", errGatewayPreparedBodyLimit
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, "", errors.New("body must be a JSON object")
	}
	session := gatewayExplicitSessionKey(r, raw)
	changed := false
	if settings.CompatibilityNormalizationEnabled {
		var err error
		changed, err = normalizeGatewayCompatibility(body)
		if err != nil {
			return nil, session, err
		}
		changed = normalizeGatewayCacheOptions(r, body) || changed
	}
	if settings.BuiltinPromptEnabled && strings.TrimSpace(settings.BuiltinPrompt) != "" {
		var instructions string
		if value, ok := body["instructions"]; ok && string(value) != "null" {
			if json.Unmarshal(value, &instructions) != nil {
				return nil, session, errors.New("instructions must be a string or null")
			}
		}
		prompt := settings.BuiltinPrompt
		combined := instructions
		// Retried requests retain one copy of the configured prefix/suffix.
		switch settings.BuiltinPromptMode {
		case "append":
			if instructions != prompt && !strings.HasSuffix(instructions, "\n\n"+prompt) {
				combined = prompt
				if instructions != "" {
					combined = instructions + "\n\n" + prompt
				}
			}
		case "if_empty":
			if strings.TrimSpace(instructions) == "" {
				combined = prompt
			}
		default:
			if instructions != prompt && !strings.HasPrefix(instructions, prompt+"\n\n") {
				combined = prompt
				if instructions != "" {
					combined += "\n\n" + instructions
				}
			}
		}
		if combined != instructions {
			body["instructions"], _ = marshalGatewayRequestJSON(combined)
			changed = true
		}
	}
	if settings.PromptCacheKeyEnabled {
		var existing string
		value, exists := body["prompt_cache_key"]
		if exists && string(value) != "null" && json.Unmarshal(value, &existing) != nil {
			return nil, session, errors.New("prompt_cache_key must be a string or null")
		}
		// Match Sub2API's ordinary Responses path: an absent key remains
		// absent. Content-derived affinity is local routing state, not an
		// invented upstream prompt-cache identity. Existing keys are scoped
		// after the actual OAuth account has been admitted.
	}
	if !changed {
		if session == "" && settings.StickySessionsEnabled {
			session = gatewayContentSessionKey(raw, r != nil && gatewayUnboundedPayload(r.Context()))
		}
		return raw, session, nil
	}
	encoded, err := marshalGatewayRequestJSON(body)
	if len(encoded) > maxBody && !officialRequestUnbounded(r) && !(r != nil && gatewayUnboundedPayload(r.Context())) {
		return nil, session, errGatewayPreparedBodyLimit
	}
	if err == nil && session == "" && settings.StickySessionsEnabled {
		session = gatewayContentSessionKey(encoded, r != nil && gatewayUnboundedPayload(r.Context()))
	}
	return encoded, session, err
}

// canonicalGatewayCacheJSON canonicalizes object keys and string escapes for
// hashing. Array ordering, explicit nulls and exact JSON numbers are retained.
// Callers supply validated JSON from a decoded request, not an arbitrary stream.
func canonicalGatewayCacheJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
