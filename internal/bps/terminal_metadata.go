package bps

import (
	"bytes"
	"encoding/json"
)

// terminalObservationMetadata is an accounting-only view, never a response or
// history snapshot. Identity and terminal envelope must be valid before it is
// built; executable/output validation can still reject the same response.
func terminalObservationMetadata(kind string, fields, response map[string]json.RawMessage, expectedID string) []byte {
	if response == nil {
		return nil
	}
	id := streamString(response, "id")
	if id == "" || expectedID != "" && id != expectedID {
		return nil
	}
	if value, ok := fields["response_id"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var outer string
		if json.Unmarshal(value, &outer) != nil || outer != "" && outer != id {
			return nil
		}
	}
	status := streamString(response, "status")
	switch kind {
	case "response.completed", "response.done":
		if status != "completed" {
			return nil
		}
		for _, value := range []json.RawMessage{fields["error"], response["error"]} {
			if len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil
			}
		}
	case "response.failed":
		if status != "failed" {
			return nil
		}
	case "response.incomplete":
		if status != "incomplete" {
			return nil
		}
	case "response.cancelled", "basispoints.response.cancelled":
		if status != "cancelled" {
			return nil
		}
	default:
		return nil
	}
	output, exists := response["output"]
	if !exists && (kind == "response.completed" || kind == "response.done") {
		return nil
	}
	if exists {
		var items []json.RawMessage
		if json.Unmarshal(output, &items) != nil || bytes.Equal(bytes.TrimSpace(output), []byte("null")) {
			return nil
		}
		for _, item := range items {
			object, err := streamObject(item)
			if err != nil || streamString(object, "type") == "" {
				return nil
			}
		}
	}
	metadata := map[string]json.RawMessage{"id": response["id"]}
	if model, ok := response["model"]; ok {
		var name string
		if json.Unmarshal(model, &name) == nil && name != "" {
			metadata["model"] = model
		}
	}
	if usage, ok := terminalObservationUsage(response["usage"]); ok {
		metadata["usage"] = usage
	}
	encoded, _ := json.Marshal(metadata)
	return encoded
}

// Retain only measured nonnegative integer counters, including explicit zeros.
// Unknown fields and untrusted usage text never enter this early view. Present
// invalid counters retain null sentinels so lower-priority aliases cannot win.
func terminalObservationUsage(value json.RawMessage) (json.RawMessage, bool) {
	source, err := streamObject(value)
	if err != nil {
		return nil, false
	}
	copyCounts := func(input map[string]json.RawMessage, names []string) map[string]json.RawMessage {
		out := make(map[string]json.RawMessage)
		for _, name := range names {
			raw, exists := input[name]
			if !exists {
				continue
			}
			var count int64
			out[name] = json.RawMessage("null")
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &count) == nil && count >= 0 {
				out[name] = raw
			}
		}
		return out
	}
	out := copyCounts(source, []string{"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cached_tokens"})
	for _, detail := range []struct {
		name   string
		counts []string
	}{{"input_tokens_details", []string{"cached_tokens", "cache_write_tokens", "image_tokens"}}, {"output_tokens_details", []string{"reasoning_tokens", "image_tokens"}}, {"prompt_tokens_details", []string{"cached_tokens", "image_tokens"}}, {"completion_tokens_details", []string{"image_tokens"}}} {
		value, exists := source[detail.name]
		if !exists {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			out[detail.name] = json.RawMessage("null")
			continue
		}
		if fields, err := streamObject(value); err == nil {
			out[detail.name], _ = json.Marshal(copyCounts(fields, detail.counts))
		} else {
			// Existing accounting rejects non-object native/prompt detail shapes.
			// A fixed non-object sentinel keeps that fact without private text.
			out[detail.name] = json.RawMessage("false")
		}
	}
	encoded, _ := json.Marshal(out)
	return encoded, true
}
