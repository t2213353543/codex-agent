package server

import (
	"bytes"
	"encoding/json"
	"errors"
)

// normalizeGatewayCompatibility adapts incompatibilities confirmed on
// the ChatGPT Codex endpoint. Official Codex requests omit max_output_tokens and
// temperature: neither has an equivalent supported control on this endpoint.
// Only top-level fields are removed; tool schemas and opaque history stay intact.
func normalizeGatewayCompatibility(body map[string]json.RawMessage) (bool, error) {
	changed := false
	for _, key := range []string{"max_output_tokens", "temperature", "user"} {
		if _, exists := body[key]; exists {
			delete(body, key)
			changed = true
		}
	}
	// Foreground defaults from generic Responses clients are harmless to omit.
	// A true background request requires asynchronous polling/storage support;
	// do not silently turn it into a synchronous generation.
	if raw, exists := body["background"]; exists {
		switch string(bytes.TrimSpace(raw)) {
		case "false", "null":
			delete(body, "background")
			changed = true
		case "true":
			return false, errors.New("background=true is not supported by the Codex upstream; use background=false or omit background")
		default:
			return false, errors.New("background must be a boolean or null")
		}
	}

	var input []json.RawMessage
	if json.Unmarshal(body["input"], &input) != nil {
		// Strings are handled by the existing HTTP preparation path; malformed
		// and future input forms retain their existing validation behavior.
		return changed, nil
	}
	inputChanged := false
	for i, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		if itemType, exists := item["type"]; exists {
			var kind string
			if json.Unmarshal(itemType, &kind) != nil || kind != "message" {
				continue
			}
		}
		var role string
		if json.Unmarshal(item["role"], &role) != nil {
			continue
		}
		itemChanged := false
		if role == "system" {
			// Keep instructions in place, including content and IDs.
			item["role"] = json.RawMessage(`"developer"`)
			itemChanged = true
		}
		// Some clients replay UI author metadata alongside the canonical role.
		// Remove only the rejected message-level field, never tool/history data
		// or an author-only input that has no independently usable role.
		if role == "system" || role == "developer" || role == "user" || role == "assistant" {
			if _, exists := item["author"]; exists {
				delete(item, "author")
				itemChanged = true
			}
		}
		if !itemChanged {
			continue
		}
		encoded, err := marshalGatewayRequestJSON(item)
		if err != nil {
			return false, err
		}
		input[i] = encoded
		inputChanged = true
	}
	if inputChanged {
		encoded, err := marshalGatewayRequestJSON(input)
		if err != nil {
			return false, err
		}
		body["input"] = encoded
		changed = true
	}
	return changed, nil
}
