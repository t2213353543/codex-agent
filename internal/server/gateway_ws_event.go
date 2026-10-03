package server

import (
	"encoding/json"
	"errors"
	"strings"
)

// JSON protocol members are case sensitive. encoding/json's struct decoder
// accepts aliases such as Type/STREAM_ID; those unknown extension fields must
// not select a different authorization path or account-admission lane.
func gatewayWSDecodeExact(raw []byte, targets map[string]any) error {
	fields, err := gatewayWSParseFields(raw)
	if err != nil {
		return err
	}
	return gatewayWSDecodeFields(fields, targets)
}

func gatewayWSParseFields(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("WebSocket event must be a JSON object")
	}
	return fields, nil
}

func gatewayWSDecodeFields(fields map[string]json.RawMessage, targets map[string]any) error {
	for name, target := range targets {
		if value, present := fields[name]; present {
			if raw, ok := target.(*json.RawMessage); ok {
				*raw = value // The parsed envelope owns this immutable field copy.
				continue
			}
			if err := json.Unmarshal(value, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// The shared metrics readers use Go structs for HTTP compatibility. Give them
// a WS-only view without folded aliases of protocol envelope fields; the pump
// still forwards payload unchanged, including every unknown extension. Nested
// tool/history content is never projected or rewritten.
func gatewayWSObservationView(payload []byte) []byte {
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return payload
	}
	return gatewayWSObservationFields(payload, fields)
}

func gatewayWSObservationFields(payload []byte, fields map[string]json.RawMessage) []byte {
	var viewFields map[string]json.RawMessage
	makeView := func() {
		if viewFields == nil {
			viewFields = make(map[string]json.RawMessage, len(fields))
			for name, value := range fields {
				viewFields[name] = value
			}
		}
	}
	for name := range fields {
		lower := strings.ToLower(name)
		if name == lower {
			continue
		}
		switch lower {
		case "type", "stream_id", "response_id", "response", "model", "generate", "previous_response_id",
			"instructions", "stream", "service_tier", "reasoning", "input", "delta", "text", "arguments", "item", "part", "headers":
			makeView()
			delete(viewFields, name)
		}
	}
	var response map[string]json.RawMessage
	if raw := fields["response"]; len(raw) > 0 && json.Unmarshal(raw, &response) == nil && response != nil {
		responseChanged := false
		for name := range response {
			lower := strings.ToLower(name)
			if name != lower && (lower == "id" || lower == "status") {
				delete(response, name)
				responseChanged = true
			}
		}
		if responseChanged {
			makeView()
			viewFields["response"], _ = json.Marshal(response)
		}
	}
	if viewFields == nil {
		return payload
	}
	view, err := json.Marshal(viewFields)
	if err != nil {
		return payload
	}
	return view
}

// This is the WS envelope counterpart of terminalResponse. It reads exact
// protocol names for classification while the pump forwards the original raw
// event. Failure payloads remain opaque; no tool/history fields are rewritten.
func gatewayWSTerminalResponse(payload []byte) (json.RawMessage, bool, error) {
	if !validSSELine(payload) {
		return nil, false, errors.New("upstream WebSocket contains invalid UTF-8")
	}
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return nil, false, nil
	}
	return gatewayWSTerminalFields(payload, fields)
}

func gatewayWSTerminalFields(payload []byte, fields map[string]json.RawMessage) (json.RawMessage, bool, error) {
	if !validSSELine(payload) {
		return nil, false, errors.New("upstream WebSocket contains invalid UTF-8")
	}
	var event responseEvent
	if gatewayWSDecodeFields(fields, map[string]any{
		"type": &event.Type, "response": &event.Response, "error": &event.Error,
		"code": &event.Code, "message": &event.Message,
	}) != nil {
		return nil, false, nil
	}
	if event.Type != "error" && event.Type != "response.failed" && event.Type != "response.completed" && event.Type != "response.done" && event.Type != "response.incomplete" {
		return nil, false, nil
	}
	var status string
	if len(event.Response) > 0 {
		_ = gatewayWSDecodeExact(event.Response, map[string]any{"status": &status})
	}
	return terminalResponseEvent(payload, event, status)
}
