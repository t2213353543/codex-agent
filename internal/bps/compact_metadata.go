package bps

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// CompactResponseMetadata returns accounting observations only. A measured
// response need not be a successful or reusable compaction. Never pass this
// view to a completion/history callback or infer a terminal status from it.
// Invalid present counters remain unknown rather than exposing lower-priority
// aliases; output, errors, status and private extensions never enter the view.
func CompactResponseMetadata(raw []byte) []byte {
	if !utf8.Valid(raw) {
		return nil
	}
	response, err := streamObject(raw)
	if err != nil || streamString(response, "id") == "" {
		return nil
	}
	if value, present := response["error"]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if _, err := streamObject(value); err != nil {
			return nil
		}
	}
	status := ""
	if value, present := response["status"]; present {
		if json.Unmarshal(value, &status) != nil {
			return nil
		}
		switch status {
		case "completed", "failed", "incomplete", "cancelled":
		default:
			return nil
		}
	}
	output, present := response["output"]
	if !present {
		if status != "failed" {
			return nil
		}
	} else {
		var items []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(output), []byte("null")) || json.Unmarshal(output, &items) != nil {
			return nil
		}
		for _, value := range items {
			item, err := streamObject(value)
			if err != nil || streamString(item, "type") == "" {
				return nil
			}
		}
	}
	metadata := map[string]json.RawMessage{"id": response["id"]}
	if streamString(response, "model") != "" {
		metadata["model"] = response["model"]
	}
	if usage, ok := terminalObservationUsage(response["usage"]); ok {
		metadata["usage"] = usage
	}
	encoded, _ := json.Marshal(metadata)
	return encoded
}
