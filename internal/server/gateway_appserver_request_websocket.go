package server

import (
	"encoding/json"
	"errors"
	"strconv"
)

// appServerWebSocketRequestState is supplied by the connection owner. The
// writer never infers previous_response_id from a local cache or consumes tool
// results. InputDelta must come from the validated full-history prefix check.
type appServerWebSocketRequestState struct {
	PreviousResponseID                   *string
	InputDelta                           *[]json.RawMessage
	Warmup                               bool
	StreamRequestStartMS                 int64
	Traceparent, Tracestate, RoutingHint string
	IncludeTimingMetrics                 bool
}

func buildAppServerWebSocketRequest(in appServerRequestInput, state appServerWebSocketRequestState) (appServerRequestWire, error) {
	if (state.PreviousResponseID == nil) != (state.InputDelta == nil) {
		return appServerRequestWire{}, errors.New("App Server websocket continuation requires both response id and validated input delta")
	}
	wire, err := buildAppServerRequest(in)
	if err != nil {
		return appServerRequestWire{}, err
	}
	m, err := parseAppServerRequestModel(in.ModelMetadataRaw)
	if err != nil {
		return appServerRequestWire{}, err
	}
	if in.StandardTools {
		m.UseResponsesLite = false
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(wire.Body, &body); err != nil {
		return appServerRequestWire{}, err
	}
	body["type"] = json.RawMessage(`"response.create"`)
	if state.PreviousResponseID != nil {
		body["previous_response_id"], _ = marshalGatewayRequestJSON(*state.PreviousResponseID)
		delta := make([]json.RawMessage, 0, len(*state.InputDelta))
		for _, raw := range *state.InputDelta {
			item, err := appServerPrepareInputItem(raw, m, in.ContentItemKinds)
			if err != nil {
				return appServerRequestWire{}, err
			}
			delta = append(delta, item)
		}
		body["input"], _ = marshalGatewayRequestJSON(delta)
	}
	if state.Warmup {
		body["generate"] = json.RawMessage(`false`)
	}
	metadata := make(map[string]string, len(in.ClientMetadata)+5)
	for key, value := range in.ClientMetadata {
		metadata[key] = value
	}
	if m.UseResponsesLite {
		metadata["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
	} else if in.StandardTools {
		delete(metadata, "ws_request_header_x_openai_internal_codex_responses_lite")
	}
	if in.TurnState != "" {
		metadata["x-codex-turn-state"] = in.TurnState
	}
	if state.Traceparent != "" {
		metadata["ws_request_header_traceparent"] = state.Traceparent
	}
	if state.Tracestate != "" {
		metadata["ws_request_header_tracestate"] = state.Tracestate
	}
	metadata["x-codex-ws-stream-request-start-ms"] = strconv.FormatInt(state.StreamRequestStartMS, 10)
	body["client_metadata"], _ = marshalGatewayRequestJSON(metadata)
	wire.Body, err = marshalGatewayRequestJSON(body)
	if err != nil {
		return appServerRequestWire{}, err
	}
	// client.rs build_websocket_headers does not put these HTTP request
	// controls in the handshake; the Lite/turn-state values ride each frame.
	wire.Headers.Del("Accept")
	wire.Headers.Del("x-openai-internal-codex-responses-lite")
	wire.Headers.Del("x-codex-turn-state")
	wire.Headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	if state.RoutingHint != "" {
		if !gatewayNativeHeaderValue(state.RoutingHint) {
			return appServerRequestWire{}, errors.New("invalid App Server routing hint")
		}
		wire.Headers.Set("x-codex-routing-hint", state.RoutingHint)
	}
	if state.IncludeTimingMetrics {
		wire.Headers.Set("x-responsesapi-include-timing-metrics", "true")
	}
	return wire, nil
}
