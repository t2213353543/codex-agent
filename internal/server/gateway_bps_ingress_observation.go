package server

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Classify transport before Prepare can reject a parameter. The WebSocket
// dispatcher sets its context independently of the client's JSON fields.
// Invalid JSON or a non-boolean stream is unknown, never an invented SSE mode.
func bpsIngressRequestType(websocket bool, path string, body []byte) string {
	if websocket {
		return "websocket"
	}
	if path == "/responses/compact" {
		return "compact"
	}
	if !json.Valid(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return ""
	}
	field := gatewayValidatedJSONFieldSpans(body, "stream", "")[0]
	if field.end == 0 {
		return "http_json" // This endpoint's absent stream defaults to false.
	}
	switch string(bytes.TrimSpace(body[field.start:field.end])) {
	case "true":
		return "http_sse"
	case "false":
		return "http_json"
	default:
		return ""
	}
}

// Only fixed protocol field identifiers enter persisted metadata. Dynamic
// caller-owned unknown keys collapse to a fixed family; no content hash or
// arbitrary parameter text is retained. The actual API error stays unchanged.
func bpsObservedErrorParameter(param string) string {
	switch param {
	case "stream_options", "stream_options.include_usage", "stream_options.include_obfuscation", "stream_options.reasoning_summary_delivery",
		"service_tier", "reasoning.effort", "max_output_tokens", "top_p", "text", "text.format",
		"text.format.type", "text.format.name", "text.format.strict", "text.format.description", "text.format.schema":
		return param
	}
	if strings.HasPrefix(param, "stream_options.") {
		return "stream_options.unknown"
	}
	return ""
}
