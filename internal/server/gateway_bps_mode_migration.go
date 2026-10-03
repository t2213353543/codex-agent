package server

import (
	"bytes"
	"context"
	"encoding/json"
)

type bpsModeMigrationKey struct{}

// A retired runtime's transport binding can move to the active backend only
// when the caller supplied plaintext history. Do not drop opaque reasoning,
// encrypted compaction, account-owned file references, pending encrypted arguments or previous-response state
// to make an incompatible request appear successful.
func withBPSModeMigration(ctx context.Context, raw []byte) context.Context {
	return withBPSModeMigrationObserved(ctx, raw, nil)
}

func withBPSModeMigrationObserved(ctx context.Context, raw []byte, replayClass *string) context.Context {
	if replayClass != nil {
		*replayClass = "unobserved"
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return ctx
	}
	var previous string
	if value := body["previous_response_id"]; len(value) > 0 && string(value) != "null" {
		if json.Unmarshal(value, &previous) != nil {
			return ctx
		}
		if previous != "" {
			if replayClass != nil {
				*replayClass = "previous_response"
			}
			return ctx
		}
	}
	var input any
	decoder := json.NewDecoder(bytes.NewReader(body["input"]))
	decoder.UseNumber()
	if decoder.Decode(&input) != nil {
		return ctx
	}
	var safe func(any) bool
	safe = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "encrypted_content" || key == "encrypted_function_args" || key == "file_id" {
					if child != nil && child != "" {
						return false
					}
				}
				if key == "type" && (child == "compaction" || child == "compaction_summary") {
					return false
				}
				if !safe(child) {
					return false
				}
			}
		case []any:
			for _, child := range v {
				if !safe(child) {
					return false
				}
			}
		}
		return true
	}
	allowed := true
	if items, ok := input.([]any); ok {
		allowed = true
		for _, item := range items {
			fields, object := item.(map[string]any)
			kind, _ := fields["type"].(string)
			if !object || kind != "additional_tools" && kind != "tool_search_output" {
				allowed = allowed && safe(item)
				continue
			}
			// These two protocol items carry tool declarations. Schema property
			// names such as file_id describe client arguments, not account-owned
			// provider state. Only this top-level declaration tree is exempt;
			// unknown sibling fields and all other history remain conservative.
			for key, child := range fields {
				if key == "tools" {
					allowed = allowed && bpsMigrationToolDefinitions(child, false)
					continue
				}
				allowed = allowed && safe(map[string]any{key: child})
			}
		}
	} else {
		allowed = safe(input)
	}
	if replayClass != nil {
		switch input.(type) {
		case string, []any:
			*replayClass = "opaque_or_file"
			if allowed {
				*replayClass = "full_plaintext"
			}
		}
	}
	return context.WithValue(ctx, bpsModeMigrationKey{}, allowed)
}

// Malformed/unknown declarations cannot grant migration permission.
// Prepare remains authoritative for the complete request and tool schema.
func bpsMigrationToolDefinitions(value any, nested bool) bool {
	definitions, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range definitions {
		definition, ok := value.(map[string]any)
		if !ok {
			return false
		}
		kind, _ := definition["type"].(string)
		switch kind {
		case "function", "custom":
			if name, _ := definition["name"].(string); name == "" {
				return false
			}
		case "namespace":
			name, _ := definition["name"].(string)
			if nested || name == "" || !bpsMigrationToolDefinitions(definition["tools"], true) {
				return false
			}
		case "web_search", "web_search_preview":
		case "tool_search":
			if nested {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func bpsAllowsModeMigration(ctx context.Context, mode string) bool {
	allowed, _ := ctx.Value(bpsModeMigrationKey{}).(bool)
	return mode == inferencePowerPoint && allowed
}
