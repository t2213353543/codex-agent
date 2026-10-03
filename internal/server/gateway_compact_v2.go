package server

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// The subscription backend uses the same streaming Responses compaction as
// Codex 0.155. Keep the public JSON compact contract for older clients, while
// other upstream base paths retain their standalone compact endpoint.
func gatewayUsesResponsesCompaction(base string) bool {
	u, err := url.Parse(base)
	return err == nil && strings.TrimRight(u.Path, "/") == "/backend-api/codex"
}

// Read only the one or two fields the compact adapter consumes. Validate the
// whole object before using the shared span scanner; it is not a validator.
// Unknown opaque values are scanned, never copied into a temporary map.
// RawMessage targets borrow immutable input bytes for the current call only.
// These four call sites decode/use them locally and marshal owned output; do
// not retain such targets in a record or mutate the caller's input buffer.
func gatewayCompactDecodeExact(raw []byte, first string, firstTarget any, second string, secondTarget any) error {
	if first == "" || first == second {
		return errors.New("compaction field names must be distinct")
	}
	// Validate the original bytes, not bytes.TrimSpace(raw): non-JSON Unicode
	// whitespace or vertical tabs must not be normalized into an accepted object.
	if !json.Valid(raw) {
		return errors.New("compaction field source must be a JSON object")
	}
	start := gatewayJSONSkipSpace(raw, 0)
	if start >= len(raw) || raw[start] != '{' {
		return errors.New("compaction field source must be a JSON object")
	}
	fields := gatewayValidatedJSONFieldSpans(raw, first, second)
	for index, target := range [2]any{firstTarget, secondTarget} {
		if index == 1 && second == "" {
			continue
		}
		span := fields[index]
		if span.end == 0 {
			continue // Absent fields leave the caller's target unchanged.
		}
		value := raw[span.start:span.end]
		if borrowed, ok := target.(*json.RawMessage); ok {
			*borrowed = value
		} else if err := json.Unmarshal(value, target); err != nil {
			return err
		}
	}
	return nil
}

func prepareResponsesCompaction(raw []byte, unbounded ...bool) ([]byte, error) {
	if len(raw) > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, errGatewayPreparedBodyLimit
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, errors.New("invalid compaction request")
	}
	var input []json.RawMessage
	var text string
	if len(body["input"]) == 0 || string(body["input"]) == "null" {
		return nil, errors.New("compaction input must be a string or array")
	}
	if json.Unmarshal(body["input"], &text) == nil {
		item, _ := marshalGatewayRequestJSON(map[string]any{"role": "user", "content": []map[string]string{{"type": "input_text", "text": text}}})
		input = []json.RawMessage{item}
	} else if json.Unmarshal(body["input"], &input) != nil {
		return nil, errors.New("compaction input must be a string or array")
	}
	// A trigger is a request control, never a durable history item. A caller
	// which already supplied a trigger must not produce duplicate controls.
	items := make([]json.RawMessage, 0, len(input)+1)
	for _, item := range input {
		var kind struct {
			Type string `json:"type"`
		}
		// ResponseItem's serde tag is exactly "type". Unknown extensions
		// such as Type may be opaque objects or strings and cannot select a
		// compaction trigger (or cause encrypted history to be discarded).
		if gatewayCompactDecodeExact(item, "type", &kind.Type, "", nil) != nil {
			return nil, errors.New("compaction input items must be objects")
		}
		if kind.Type != "compaction_trigger" {
			items = append(items, item)
		}
	}
	items = append(items, json.RawMessage(`{"type":"compaction_trigger"}`))
	body["input"], _ = marshalGatewayRequestJSON(items)
	body["stream"], body["store"] = json.RawMessage("true"), json.RawMessage("false")
	if _, ok := body["instructions"]; !ok {
		body["instructions"] = json.RawMessage(`""`)
	}
	result, err := marshalGatewayRequestJSON(body)
	if len(result) > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, errGatewayPreparedBodyLimit
	}
	return result, err
}

func responsesCompactionResult(request, response []byte, unbounded ...bool) ([]byte, error) {
	var source struct {
		Input []json.RawMessage `json:"input"`
	}
	var result map[string]json.RawMessage
	if gatewayCompactDecodeExact(request, "input", &source.Input, "", nil) != nil || json.Unmarshal(response, &result) != nil || result == nil {
		return nil, errors.New("invalid upstream compaction response")
	}
	var status string
	_ = json.Unmarshal(result["status"], &status)
	if status != "completed" {
		return nil, errors.New("upstream compaction did not complete")
	}
	var items []json.RawMessage
	if json.Unmarshal(result["output"], &items) != nil {
		return nil, errors.New("upstream compaction output is invalid")
	}
	var compact json.RawMessage
	for _, item := range items {
		var value struct {
			Type      string          `json:"type"`
			Encrypted json.RawMessage `json:"encrypted_content"`
		}
		if gatewayCompactDecodeExact(item, "type", &value.Type, "encrypted_content", &value.Encrypted) != nil || value.Type == "" {
			return nil, errors.New("upstream compaction item is invalid")
		}
		if value.Type == "compaction" || value.Type == "compaction_summary" {
			var encrypted string
			if len(compact) != 0 || json.Unmarshal(value.Encrypted, &encrypted) != nil || strings.TrimSpace(encrypted) == "" {
				return nil, errors.New("upstream compaction must contain one encrypted item")
			}
			compact = item
		}
	}
	if len(compact) == 0 {
		return nil, errors.New("upstream response did not contain compaction")
	}
	// Adapt legacy JSON compact callers by retaining their instruction messages
	// and appending the opaque summary. The CLI's exact selection also depends
	// on local harness metadata and image budgets unavailable on this wire API.
	// Never replay assistant tool calls or retain a previous compaction trigger.
	// Bound retained verbatim context independently of the request size. The
	// CLI budgets 64K approximate tokens (four bytes per token); counting the
	// entire serialized message is conservative and also bounds inline media.
	// Keep complete recent messages rather than corrupting structured content;
	// the encrypted summary carries context outside this retained suffix.
	const retainedBytes = 64000 * 4
	remaining := retainedBytes
	output := make([]json.RawMessage, 0, len(source.Input)+1)
	for index := len(source.Input) - 1; index >= 0; index-- {
		item := source.Input[index]
		var value struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if gatewayCompactDecodeExact(item, "type", &value.Type, "role", &value.Role) != nil {
			continue
		}
		if (value.Type == "" || value.Type == "message") && (value.Role == "user" || value.Role == "developer" || value.Role == "system") {
			if len(item) > remaining && !gatewayPayloadUnboundedOption(unbounded) {
				break
			}
			output = append(output, item)
			if !gatewayPayloadUnboundedOption(unbounded) {
				remaining -= len(item)
			}
		}
	}
	for left, right := 0, len(output)-1; left < right; left, right = left+1, right-1 {
		output[left], output[right] = output[right], output[left]
	}
	output = append(output, compact)
	public := map[string]json.RawMessage{"object": json.RawMessage(`"response.compaction"`)}
	for _, key := range []string{"id", "created_at", "usage"} {
		if value, ok := result[key]; ok {
			public[key] = value
		}
	}
	public["output"], _ = marshalGatewayRequestJSON(output)
	encoded, err := marshalGatewayRequestJSON(public)
	if len(encoded) > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, errors.New("upstream compacted context exceeds 32 MiB")
	}
	return encoded, err
}
