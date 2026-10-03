package server

import (
	"bytes"
	"encoding/json"
	"errors"
)

// HTTP SSE retains event errors until EOF and lets a later Completed win
// (codex-api/sse/responses.rs process_sse_with_treatment). WebSocket uses a
// different loop and returns event errors immediately; do not share this rule.
// The gateway's top-level error diagnostic extension follows the same deferred
// policy, so an event ignored by the CLI cannot truncate a later completion.
func gatewayHTTPDeferredResponsePriority(payload []byte) int {
	var event struct {
		Type string `json:"type"`
	}
	if gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type}) != nil {
		return 0
	}
	switch event.Type {
	case "response.failed", "response.incomplete":
		return 3
	case "error":
		return 2 // An extension cannot overwrite the CLI's last typed error.
	case "response.done":
		return 1 // EOF-only JSON compatibility, never a streaming completion.
	case "response.completed":
		if _, _, err := gatewayHTTPTerminalResponse(payload); err != nil {
			return 3
		}
	}
	return 0
}

func gatewayHTTPTerminalResponse(payload []byte) (json.RawMessage, bool, error) {
	var event responseEvent
	if !validSSELine(payload) {
		return nil, false, errors.New("upstream SSE contains invalid UTF-8")
	}
	if gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type, "response": &event.Response, "error": &event.Error, "code": &event.Code, "message": &event.Message}) != nil {
		return nil, false, nil
	}
	if event.Type != "response.completed" {
		var status string
		_ = gatewayWSDecodeExact(event.Response, map[string]any{"status": &status})
		if event.Type == "response.failed" || event.Type == "error" || (event.Type == "response.done" || event.Type == "response.incomplete") && status == "failed" {
			return event.Response, true, gatewayHTTPResponseFailure(payload, event)
		}
		return terminalResponseEvent(payload, event, status)
	}
	// ResponseCompleted does not deserialize status. The completed event wins
	// even if optional vendor status contradicts it; returned bytes stay exact.
	if !jsonObject(event.Response) {
		return nil, true, errors.New("upstream response missing or invalid")
	}
	if !gatewayHTTPCompletedFieldTypes(event.Response) {
		return nil, true, errors.New("upstream completed response contains invalid field types")
	}
	return event.Response, true, nil
}

func gatewayHTTPResponseFailure(payload []byte, event responseEvent) error {
	if jsonObject(event.Error) {
		return &upstreamResponseError{Payload: event.Error, Response: event.Response}
	}
	var nested json.RawMessage
	_ = gatewayWSDecodeExact(event.Response, map[string]any{"error": &nested})
	if jsonObject(nested) {
		return &upstreamResponseError{Payload: nested, Response: event.Response}
	}
	if event.Type == "error" && (len(event.Code) > 0 || len(event.Message) > 0) {
		return &upstreamResponseError{Payload: append(json.RawMessage(nil), payload...), Response: event.Response}
	}
	return &upstreamResponseError{Payload: json.RawMessage(`{"message":"upstream response failed"}`), Response: event.Response}
}

// Retain the existing compatible-source omissions (ID or usage counts may be
// absent), but explicit invalid types must never terminate the HTTP stream as
// a successful completion. Optional usage/details nulls match serde Option.
func gatewayHTTPCompletedFieldTypes(response []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(response, &object) != nil || object == nil {
		return false
	}
	if raw, exists := object["id"]; exists {
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return false
		}
	}
	if raw, exists := object["end_turn"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
	}
	counts := func(raw json.RawMessage, names []string) bool {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return true
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		for _, name := range names {
			if value, exists := fields[name]; exists {
				var n int64
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &n) != nil {
					return false
				}
			}
		}
		return true
	}
	if !counts(object["usage"], []string{"input_tokens", "output_tokens", "total_tokens"}) {
		return false
	}
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(object["usage"], &usage)
	return counts(usage["input_tokens_details"], []string{"cached_tokens", "cache_write_tokens"}) && counts(usage["output_tokens_details"], []string{"reasoning_tokens"})
}

func gatewayHTTPIsCompleted(payload []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	return gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type}) == nil && event.Type == "response.completed"
}

func recordGatewayHTTPResponseOutcome(rec *RequestRecord, raw []byte) {
	matches := gatewayResponseMatchesRecord(rec, gatewayResponseObject(raw))
	recordResponseOutcome(rec, raw)
	if !rec.httpCompleted || !matches {
		return
	}
	rec.TerminalStatus, rec.IncompleteReason = "completed", ""
	if rec.Status != "aborted" {
		rec.Status, rec.Error = "ok", ""
	}
	if rec.ErrorSource == "upstream" && rec.ErrorPhase == "response_event" {
		rec.ErrorSource, rec.ErrorPhase = "", ""
	}
}

// A deferred error still establishes response ownership. Commit only its raw
// identity now, never terminal status, usage or scheduling state. Otherwise an
// unrelated later completion could become the first ID seen by accounting.
func observeGatewayHTTPPendingIdentity(rec *RequestRecord, payload []byte) {
	if rec == nil {
		return
	}
	identity := gatewayHTTPCooldownIdentity{digest: rec.responseIdentityDigest, known: rec.responseIdentityKnown}
	if !identity.accepts(payload) {
		return
	}
	var event struct {
		ResponseID string          `json:"response_id"`
		Response   json.RawMessage `json:"response"`
	}
	if gatewayWSDecodeExact(payload, map[string]any{"response_id": &event.ResponseID, "response": &event.Response}) != nil {
		return
	}
	var id string
	if len(event.Response) > 0 && string(event.Response) != "null" && gatewayWSDecodeExact(event.Response, map[string]any{"id": &id}) != nil {
		return
	}
	if id == "" {
		id = event.ResponseID
	}
	if id != "" && gatewayResponseIDMatchesRecord(rec, id) {
		rec.responseIdentityDigest, rec.responseIdentityKnown = identity.digest, identity.known
		rec.ResponseID = boundedGatewayMetadata(id, 1024)
	}
}
