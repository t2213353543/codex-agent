package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// appServerRequestModel is the immutable projection of an actual 0.155.0
// ModelInfo. Keep the raw catalog outside this writer; never infer capabilities
// from a model name. Rust defaults are applied by parseAppServerRequestModel.
type appServerRequestModel struct {
	Slug                      string                    `json:"slug"`
	UseResponsesLite          bool                      `json:"use_responses_lite"`
	DefaultReasoningLevel     string                    `json:"default_reasoning_level"`
	DefaultReasoningSummary   string                    `json:"default_reasoning_summary"`
	SupportsReasoningSummary  bool                      `json:"supports_reasoning_summary_parameter"`
	SupportVerbosity          bool                      `json:"support_verbosity"`
	DefaultVerbosity          string                    `json:"default_verbosity"`
	SupportsOriginalImages    bool                      `json:"supports_image_detail_original"`
	MultiAgentReasoningEffort string                    `json:"multi_agent_reasoning_effort"`
	SupportedReasoningLevels  []struct{ Effort string } `json:"supported_reasoning_levels"`
	ServiceTiers              []struct{ ID string }     `json:"service_tiers"`
	ModelMessages             *struct {
		InstructionsTemplate  *string `json:"instructions_template"`
		InstructionsVariables *struct {
			Default   string `json:"personality_default"`
			Friendly  string `json:"personality_friendly"`
			Pragmatic string `json:"personality_pragmatic"`
		} `json:"instructions_variables"`
	} `json:"model_messages"`
}

func parseAppServerRequestModel(raw json.RawMessage) (appServerRequestModel, error) {
	m := appServerRequestModel{SupportsReasoningSummary: true, DefaultReasoningSummary: "auto"}
	if err := json.Unmarshal(raw, &m); err != nil || m.Slug == "" || string(bytes.TrimSpace(raw)) == "null" {
		return appServerRequestModel{}, errors.New("invalid App Server model metadata")
	}
	return m, nil
}

// appServerRequestInput is the final writer boundary, not a hosted agent. Input
// contains caller-owned context/history; Tools contains the official ToolSpec
// conversion (including Lite namespace grouping), serialized exactly as Rust
// hashes it. This function neither dispatches tools nor stores a conversation.
type appServerRequestInput struct {
	ModelMetadataRaw    json.RawMessage
	ThreadID, SessionID string
	Input               []json.RawMessage
	Tools               json.RawMessage
	// StandardTools selects the validated standard Responses wire profile for
	// backend-owned tools. It does not change the immutable model catalog.
	StandardTools                                                     bool
	Instructions                                                      *string
	Personality                                                       string
	Effort, Summary, Verbosity, ServiceTier                           string
	ParallelToolCalls, ConcurrentReasoningSummaries, ContentItemKinds bool
	OutputSchema                                                      json.RawMessage
	OutputSchemaStrict                                                bool
	PromptCacheKey                                                    *string
	ClientMetadata                                                    map[string]string
	// Explicit caller controls supported by the client-owned profile. The
	// captured hosted-runtime writer supplies none of these overrides.
	ClientControls map[string]json.RawMessage
	// ExtraHeaders are already resolved provider/compatibility headers. Auth,
	// attestation, compression and cookies belong to the transport, not here.
	ExtraHeaders                        http.Header
	Originator, BetaFeatures, TurnState string
}

type appServerRequestWire struct {
	Body    []byte
	Headers http.Header
}

// buildAppServerRequest mirrors core/src/client.rs build_responses_request and
// the HTTP options writer for the official OpenAI provider in Codex 0.155.0.
// It is intentionally not connected to a production handler until captured
// requests validate the context/tool adapter that supplies this boundary.
func buildAppServerRequest(in appServerRequestInput) (appServerRequestWire, error) {
	m, err := parseAppServerRequestModel(in.ModelMetadataRaw)
	if err != nil {
		return appServerRequestWire{}, err
	}
	if in.StandardTools {
		m.UseResponsesLite = false
	}
	if in.ThreadID == "" || in.SessionID == "" || in.ClientMetadata == nil {
		return appServerRequestWire{}, errors.New("App Server writer requires explicit thread, session and metadata")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(in.Tools, &tools); err != nil || tools == nil {
		return appServerRequestWire{}, errors.New("App Server writer requires serialized tool array")
	}
	instructions := ""
	if in.Instructions != nil {
		instructions = *in.Instructions
	} else if msg := m.ModelMessages; msg != nil && msg.InstructionsTemplate != nil {
		instructions = *msg.InstructionsTemplate
		if vars := msg.InstructionsVariables; vars != nil {
			personality := vars.Default
			switch in.Personality {
			case "none":
				personality = ""
			case "friendly":
				personality = vars.Friendly
			case "pragmatic":
				personality = vars.Pragmatic
			}
			instructions = strings.ReplaceAll(instructions, "{{ personality }}", personality)
		}
	}
	items := make([]json.RawMessage, 0, len(in.Input)+2)
	if m.UseResponsesLite {
		// UUID namespace OID, then thread namespace, then exact visible payload.
		oid := [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
		ns := appServerUUIDv5(oid, []byte(in.ThreadID))
		item, err := marshalGatewayRequestJSON(map[string]any{"type": "additional_tools", "id": "at_" + appServerUUIDString(appServerUUIDv5(ns, in.Tools)), "role": "developer", "tools": in.Tools})
		if err != nil {
			return appServerRequestWire{}, err
		}
		items = append(items, item)
		if instructions != "" {
			message := map[string]any{"type": "message", "id": "msg_" + appServerUUIDString(appServerUUIDv5(ns, []byte(instructions))), "role": "developer", "content": []any{map[string]string{"type": "input_text", "text": instructions}}}
			if in.ContentItemKinds {
				message["internal_chat_message_metadata_passthrough"] = map[string]any{"content_item_kinds": []string{"model.base_instructions"}}
			}
			item, err = marshalGatewayRequestJSON(message)
			if err != nil {
				return appServerRequestWire{}, err
			}
			items = append(items, item)
		}
	}
	for _, raw := range in.Input {
		item, err := appServerPrepareInputItem(raw, m, in.ContentItemKinds)
		if err != nil {
			return appServerRequestWire{}, err
		}
		items = append(items, item)
	}
	effort := in.Effort
	if effort == "" {
		effort = m.DefaultReasoningLevel
	}
	persistent := effort == "persistent"
	if effort == "ultra" {
		effort = ""
		for _, level := range m.SupportedReasoningLevels {
			if m.MultiAgentReasoningEffort != "ultra" && level.Effort == m.MultiAgentReasoningEffort {
				effort = level.Effort
				break
			}
		}
		if effort == "" {
			for _, level := range m.SupportedReasoningLevels {
				if level.Effort == "max" {
					effort = "max"
					break
				}
				if level.Effort != "ultra" {
					effort = level.Effort
				}
			}
		}
		if effort == "" {
			effort = "medium"
		}
	}
	if persistent {
		effort = "disabled"
	}
	reasoning := map[string]any{}
	if effort != "" {
		reasoning["effort"] = effort
	}
	summary := in.Summary
	if summary == "" {
		summary = m.DefaultReasoningSummary
	}
	if m.SupportsReasoningSummary && summary != "none" {
		reasoning["summary"] = summary
	}
	if m.UseResponsesLite {
		reasoning["context"] = "all_turns"
	}
	cacheKey := in.SessionID
	if in.PromptCacheKey != nil {
		cacheKey = *in.PromptCacheKey
	}
	body := map[string]any{"model": m.Slug, "input": items, "tool_choice": "auto", "parallel_tool_calls": in.ParallelToolCalls && !m.UseResponsesLite, "reasoning": reasoning, "store": false, "stream": true, "include": []string{"reasoning.encrypted_content"}, "prompt_cache_key": cacheKey, "client_metadata": in.ClientMetadata}
	if !m.UseResponsesLite {
		body["tools"] = in.Tools
		if instructions != "" {
			body["instructions"] = instructions
		}
	}
	if in.ConcurrentReasoningSummaries && reasoning["summary"] != nil {
		body["stream_options"] = map[string]string{"reasoning_summary_delivery": "sequential_cutoff"}
	}
	if in.ServiceTier != "" && in.ServiceTier != "default" {
		for _, tier := range m.ServiceTiers {
			if tier.ID == in.ServiceTier {
				body["service_tier"] = in.ServiceTier
				break
			}
		}
	}
	text := map[string]any{}
	if m.SupportVerbosity {
		verbosity := in.Verbosity
		if verbosity == "" {
			verbosity = m.DefaultVerbosity
		}
		if verbosity != "" {
			text["verbosity"] = verbosity
		}
	}
	if len(in.OutputSchema) > 0 && string(bytes.TrimSpace(in.OutputSchema)) != "null" {
		if !json.Valid(in.OutputSchema) {
			return appServerRequestWire{}, errors.New("invalid App Server output schema")
		}
		text["format"] = map[string]any{"type": "json_schema", "strict": in.OutputSchemaStrict, "schema": in.OutputSchema, "name": "codex_output_schema"}
	}
	if len(text) > 0 {
		body["text"] = text
	}
	for key, value := range in.ClientControls {
		if !json.Valid(value) {
			return appServerRequestWire{}, errors.New("invalid client request control")
		}
		switch key {
		case "model", "input", "tools", "instructions", "client_metadata", "stream", "store", "prompt_cache_key",
			"type", "generate", "previous_response_id":
			return appServerRequestWire{}, errors.New("client control overlaps an App Server writer field")
		}
		body[key] = value
	}
	encoded, err := marshalGatewayRequestJSON(body)
	if err != nil {
		return appServerRequestWire{}, err
	}
	headers := make(http.Header)
	for name, values := range in.ExtraHeaders {
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	for key, value := range map[string]string{"session-id": in.SessionID, "thread-id": in.ThreadID, "x-client-request-id": in.ThreadID, "originator": in.Originator, "x-codex-beta-features": in.BetaFeatures, "x-codex-turn-state": in.TurnState} {
		if value != "" {
			if !gatewayNativeHeaderValue(value) {
				return appServerRequestWire{}, fmt.Errorf("invalid App Server header %s", key)
			}
			headers.Set(key, value)
		}
	}
	headers.Set("Accept", "text/event-stream")
	if m.UseResponsesLite {
		headers.Set("x-openai-internal-codex-responses-lite", "true")
	} else if in.StandardTools {
		headers.Del("x-openai-internal-codex-responses-lite")
	}
	return appServerRequestWire{Body: encoded, Headers: headers}, nil
}

func appServerUUIDv5(namespace [16]byte, data []byte) [16]byte {
	h := sha1.New()
	_, _ = h.Write(namespace[:])
	_, _ = h.Write(data)
	var id [16]byte
	copy(id[:], h.Sum(nil))
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	return id
}

func appServerUUIDString(id [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

// This is only the final writer's item normalization. Thread context assembly,
// typed ResponseItem decoding and custom-tool wrappers are separate boundaries.
func appServerPrepareInputItem(raw json.RawMessage, model appServerRequestModel, contentKinds bool) (json.RawMessage, error) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return nil, errors.New("invalid App Server input item")
	}
	var id, kind string
	if rawID, ok := item["id"]; ok {
		if err := json.Unmarshal(rawID, &id); err != nil {
			return nil, errors.New("invalid App Server input item id")
		}
		if id == "" {
			delete(item, "id")
		}
	}
	_ = json.Unmarshal(item["type"], &kind)
	if prefix, suffix, ok := strings.Cut(id, "_"); id != "" && (!ok || prefix == "" || suffix == "") {
		delete(item, "id")
	}
	if !contentKinds {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(item["internal_chat_message_metadata_passthrough"], &metadata) == nil && metadata != nil {
			delete(metadata, "content_item_kinds")
			if len(metadata) == 0 {
				delete(item, "internal_chat_message_metadata_passthrough")
			} else {
				item["internal_chat_message_metadata_passthrough"], _ = marshalGatewayRequestJSON(metadata)
			}
		}
	}
	contentField := ""
	if kind == "message" {
		contentField = "content"
	}
	if kind == "function_call_output" || kind == "custom_tool_call_output" {
		contentField = "output"
	}
	var parts []map[string]json.RawMessage
	if contentField != "" && json.Unmarshal(item[contentField], &parts) == nil && parts != nil {
		changed := false
		for _, part := range parts {
			if string(part["type"]) != `"input_image"` {
				continue
			}
			if model.UseResponsesLite {
				if _, ok := part["detail"]; ok {
					delete(part, "detail")
					changed = true
				}
			} else if !model.SupportsOriginalImages && string(part["detail"]) == `"original"` {
				part["detail"] = json.RawMessage(`"high"`)
				changed = true
			}
		}
		if changed {
			item[contentField], _ = marshalGatewayRequestJSON(parts)
		}
	}
	return marshalGatewayRequestJSON(item)
}
