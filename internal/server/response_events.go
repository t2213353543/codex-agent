package server

import (
	"bytes"
	"encoding/json"
	"errors"
)

// responseEvent preserves opaque response items and upstream diagnostics. Public
// Responses SSE errors use top-level code/message/param; the Codex backend may
// instead send an error object, including inside response.failed.
type responseEvent struct {
	Type     string          `json:"type"`
	Response json.RawMessage `json:"response"`
	Error    json.RawMessage `json:"error"`
	Code     json.RawMessage `json:"code"`
	Message  json.RawMessage `json:"message"`
}

func jsonObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 1 && raw[0] == '{' && json.Valid(raw)
}

func (e responseEvent) failure(payload []byte) error {
	if jsonObject(e.Error) {
		return &upstreamResponseError{Payload: e.Error, Response: e.Response}
	}
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(e.Response, &response)
	if jsonObject(response.Error) {
		return &upstreamResponseError{Payload: response.Error, Response: e.Response}
	}
	if e.Type == "error" && (len(e.Code) > 0 || len(e.Message) > 0) {
		return &upstreamResponseError{Payload: append(json.RawMessage(nil), payload...), Response: e.Response}
	}
	return &upstreamResponseError{
		Payload:  json.RawMessage(`{"message":"upstream response failed"}`),
		Response: e.Response,
	}
}

// terminalResponse is shared by JSON collection and the streaming observer so
// malformed completion events can never be counted as a successful response.
func terminalResponse(payload []byte) (json.RawMessage, bool, error) {
	// encoding/json otherwise replaces invalid UTF-8 inside strings, which can
	// corrupt response IDs and opaque content while appearing successful.
	if !validSSELine(payload) {
		return nil, false, errors.New("upstream SSE contains invalid UTF-8")
	}
	var event responseEvent
	if json.Unmarshal(payload, &event) != nil {
		return nil, false, nil
	}
	var response struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(event.Response, &response)
	return terminalResponseEvent(payload, event, response.Status)
}

// The terminal contract is shared by HTTP and WS. Each boundary supplies its
// decoded envelope/status; payload stays original for top-level diagnostics.
func terminalResponseEvent(payload []byte, event responseEvent, status string) (json.RawMessage, bool, error) {
	switch event.Type {
	case "error", "response.failed":
		return event.Response, true, event.failure(payload)
	case "response.completed", "response.done", "response.incomplete":
		if !jsonObject(event.Response) {
			return nil, true, errors.New("upstream response missing or invalid")
		}
		if status == "failed" {
			return event.Response, true, event.failure(payload)
		}
		return event.Response, true, nil
	}
	return nil, false, nil
}

// recordResponseOutcome keeps protocol status independent from HTTP status:
// incomplete is a valid response body, but not a successful model completion.
func recordResponseOutcome(rec *RequestRecord, raw []byte) {
	if !gatewayResponseMatchesRecord(rec, gatewayResponseObject(raw)) {
		rec.Status, rec.Error = "error", "upstream response ID changed"
		observeGatewayError(rec, "upstream", "response_event")
		return
	}
	observeGatewayResponse(rec, raw)
	if input, output, cached, ok := usageFromResponse(raw); ok {
		rec.InputTokens, rec.OutputTokens, rec.CachedTokens = input, output, cached
	}
	var response struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	_ = json.Unmarshal(raw, &response)
	if rec.TerminalStatus != "" {
		// The event and nested object have already been reconciled. Preserve
		// the failure signal even when response.status contradicts its event.
		response.Status = rec.TerminalStatus
	}
	switch response.Status {
	case "failed":
		rec.Status = "error"
		rec.Error = errorFromPayload(raw)
		if rec.Error == "" {
			rec.Error = "upstream response failed"
		}
	case "incomplete", "cancelled":
		rec.Status = "error"
		rec.Error = "upstream response " + response.Status
		if response.IncompleteDetails.Reason != "" {
			rec.Error += ": " + response.IncompleteDetails.Reason
		}
	default:
		if rec.Status != "aborted" {
			rec.Status = "ok"
		}
	}
}
