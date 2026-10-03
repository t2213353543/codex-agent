package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// appServerToolAdapter exposes only tools supplied by the client. Its stable
// ordered Tools bytes are suitable for the official Lite prefix UUID writer.
// The mapper has no pending calls, executor, hidden tools or conversation state.
type appServerToolAdapter struct {
	Tools      json.RawMessage
	lite       bool
	identities map[string]appServerClientToolIdentity
	builtins   map[string]bool
}

type appServerClientToolIdentity struct{ Namespace, Name, Kind string }

// buildAppServerTools mirrors 0.155.0 ToolSpec serialization and Lite namespace
// grouping, preserving the client tool contract. Unlike DynamicTool RPC, direct
// Responses supports native custom grammars and explicit strict/schema fields;
// these are kept, not lowered to a function wrapper or compacted by a host agent.
func buildAppServerTools(raw json.RawMessage, lite bool) (appServerToolAdapter, error) {
	adapter := appServerToolAdapter{lite: lite, identities: map[string]appServerClientToolIdentity{}, builtins: map[string]bool{}}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = json.RawMessage(`[]`)
	}
	var declarations []json.RawMessage
	if err := json.Unmarshal(raw, &declarations); err != nil || declarations == nil {
		return adapter, errors.New("App Server tools must be an array")
	}
	tools := make([]json.RawMessage, 0, len(declarations))
	defaultTools := []json.RawMessage{}
	defaultDescription := ""
	defaultIndex := -1
	namespaces := map[string]bool{}
	for _, declaration := range declarations {
		var head struct {
			Type, Name, Description string
			Tools                   []json.RawMessage
		}
		if err := json.Unmarshal(declaration, &head); err != nil {
			return adapter, errors.New("invalid App Server tool declaration")
		}
		if head.Type == "namespace" {
			if err := appServerToolIdentifier(head.Name, 64); err != nil {
				return adapter, err
			}
			if namespaces[head.Name] {
				return adapter, errors.New("duplicate client tool namespace")
			}
			namespaces[head.Name] = true
			if head.Tools == nil {
				return adapter, errors.New("tool namespace requires a tools array")
			}
			children := make([]json.RawMessage, 0, len(head.Tools))
			for _, child := range head.Tools {
				encoded, err := adapter.encodeClientTool(child, head.Name)
				if err != nil {
					return adapter, err
				}
				children = append(children, encoded)
			}
			if lite && head.Name == "functions" {
				if strings.TrimSpace(head.Description) != "" {
					defaultDescription = head.Description
				}
				defaultTools = append(defaultTools, children...)
				if defaultIndex < 0 {
					defaultIndex = len(tools)
				}
			} else {
				encoded, err := appServerToolNamespace(head.Name, head.Description, children)
				if err != nil {
					return adapter, err
				}
				tools = append(tools, encoded)
			}
			continue
		}
		encoded, err := adapter.encodeClientTool(declaration, "")
		if err != nil {
			return adapter, err
		}
		if lite && (head.Type == "function" || head.Type == "custom") {
			defaultTools = append(defaultTools, encoded)
			if defaultIndex < 0 {
				defaultIndex = len(tools)
			}
		} else {
			tools = append(tools, encoded)
		}
	}
	if lite && len(defaultTools) > 0 {
		group, err := appServerToolNamespace("functions", defaultDescription, defaultTools)
		if err != nil {
			return adapter, err
		}
		tools = append(tools, nil)
		copy(tools[defaultIndex+1:], tools[defaultIndex:])
		tools[defaultIndex] = group
	}
	encoded, err := marshalGatewayRequestJSON(tools)
	adapter.Tools = encoded
	return adapter, err
}

func (adapter *appServerToolAdapter) encodeClientTool(raw json.RawMessage, namespace string) (json.RawMessage, error) {
	var tool struct {
		Type, Name, Description string
		Strict                  bool
		DeferLoading            *bool `json:"defer_loading"`
		Parameters              json.RawMessage
		Format                  json.RawMessage
	}
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, errors.New("invalid client tool")
	}
	if tool.Type == "function" || tool.Type == "custom" {
		if err := appServerToolIdentifier(tool.Name, 128); err != nil {
			return nil, err
		}
		wireNamespace := namespace
		if adapter.lite && wireNamespace == "" {
			wireNamespace = "functions"
		}
		key := wireNamespace + "\x00" + tool.Name
		if _, exists := adapter.identities[key]; exists {
			return nil, errors.New("client tool identities collide in the effective namespace")
		}
		adapter.identities[key] = appServerClientToolIdentity{Namespace: namespace, Name: tool.Name, Kind: tool.Type}
	}
	switch tool.Type {
	case "function":
		if len(tool.Parameters) == 0 {
			tool.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]json.RawMessage
		if json.Unmarshal(tool.Parameters, &schema) != nil || schema == nil {
			return nil, errors.New("function parameters must be a JSON schema object")
		}
		// Field order matches tools/src/responses_api.rs ResponsesApiTool.
		return marshalGatewayRequestJSON(struct {
			Type         string          `json:"type"`
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			Strict       bool            `json:"strict"`
			DeferLoading *bool           `json:"defer_loading,omitempty"`
			Parameters   json.RawMessage `json:"parameters"`
		}{"function", tool.Name, tool.Description, tool.Strict, tool.DeferLoading, tool.Parameters})
	case "custom":
		var format struct{ Type, Syntax, Definition string }
		if json.Unmarshal(tool.Format, &format) != nil || format.Type != "grammar" || (format.Syntax != "lark" && format.Syntax != "regex") || format.Definition == "" {
			return nil, errors.New("custom tool requires a lark or regex grammar format")
		}
		return marshalGatewayRequestJSON(struct {
			Type         string `json:"type"`
			Name         string `json:"name"`
			Description  string `json:"description"`
			DeferLoading *bool  `json:"defer_loading,omitempty"`
			Format       struct {
				Type       string `json:"type"`
				Syntax     string `json:"syntax"`
				Definition string `json:"definition"`
			} `json:"format"`
		}{Type: "custom", Name: tool.Name, Description: tool.Description, DeferLoading: tool.DeferLoading, Format: struct {
			Type       string `json:"type"`
			Syntax     string `json:"syntax"`
			Definition string `json:"definition"`
		}{format.Type, format.Syntax, format.Definition}})
	case "web_search", "web_search_preview":
		if namespace != "" {
			return nil, errors.New("web_search cannot be nested in a client tool namespace")
		}
		adapter.builtins["web_search"] = true
		var search struct {
			ExternalWebAccess  *bool           `json:"external_web_access"`
			IndexedWebAccess   *bool           `json:"indexed_web_access"`
			Filters            json.RawMessage `json:"filters"`
			UserLocation       json.RawMessage `json:"user_location"`
			SearchContextSize  *string         `json:"search_context_size"`
			SearchContentTypes *[]string       `json:"search_content_types"`
		}
		if err := json.Unmarshal(raw, &search); err != nil {
			return nil, err
		}
		// Built-in search is a real backend tool, not an executable client
		// function named web/run. Preserve false and all supplied controls.
		return marshalGatewayRequestJSON(struct {
			Type               string          `json:"type"`
			ExternalWebAccess  *bool           `json:"external_web_access,omitempty"`
			IndexedWebAccess   *bool           `json:"indexed_web_access,omitempty"`
			Filters            json.RawMessage `json:"filters,omitempty"`
			UserLocation       json.RawMessage `json:"user_location,omitempty"`
			SearchContextSize  *string         `json:"search_context_size,omitempty"`
			SearchContentTypes *[]string       `json:"search_content_types,omitempty"`
		}{"web_search", search.ExternalWebAccess, search.IndexedWebAccess, appServerToolOptionalRaw(search.Filters), appServerToolOptionalRaw(search.UserLocation), search.SearchContextSize, search.SearchContentTypes})
	case "tool_search":
		if namespace != "" {
			return nil, errors.New("tool_search cannot be nested in a client tool namespace")
		}
		adapter.builtins["tool_search"] = true
		var search struct {
			Execution, Description string
			Parameters             json.RawMessage
		}
		if err := json.Unmarshal(raw, &search); err != nil {
			return nil, err
		}
		if search.Execution == "" {
			search.Execution = "server"
		}
		if search.Execution != "client" && search.Execution != "server" {
			return nil, errors.New("invalid tool_search execution")
		}
		if len(search.Parameters) == 0 {
			search.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		return marshalGatewayRequestJSON(struct {
			Type        string          `json:"type"`
			Execution   string          `json:"execution"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}{"tool_search", search.Execution, search.Description, search.Parameters})
	default:
		return nil, fmt.Errorf("unsupported client tool type %q", tool.Type)
	}
}

func appServerToolIdentifier(name string, limit int) error {
	if name == "" || len(name) > limit {
		return errors.New("client tool identifier is empty or too long")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return errors.New("client tool identifier must contain only ASCII letters, digits, underscore or hyphen")
		}
	}
	return nil
}

func appServerToolOptionalRaw(raw json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

func appServerToolNamespace(name, description string, tools []json.RawMessage) (json.RawMessage, error) {
	return marshalGatewayRequestJSON(struct {
		Type        string            `json:"type"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Tools       []json.RawMessage `json:"tools"`
	}{"namespace", name, description, tools})
}

// PrepareInput restores only the default namespace required by Lite. Call IDs,
// item IDs, arguments, custom grammar text, opaque reasoning and output arrays
// remain exact. Historical retired tools remain context rather than permission.
func (adapter appServerToolAdapter) PrepareInput(items []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		item, err := adapter.mapItem(raw, true)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (adapter appServerToolAdapter) MapOutputItem(raw json.RawMessage) (json.RawMessage, error) {
	return adapter.mapItem(raw, false)
}

// PrepareToolChoice preserves the caller's explicit selection. This is an
// intentional client-contract override of the official agent writer's fixed
// "auto", not evidence that the App Server runtime supports forced choice.
func (adapter appServerToolAdapter) PrepareToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage(`"auto"`), nil
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		if mode != "auto" && mode != "none" && mode != "required" {
			return nil, errors.New("unsupported client tool choice")
		}
		return raw, nil
	}
	var choice map[string]json.RawMessage
	if json.Unmarshal(raw, &choice) != nil || choice == nil {
		return nil, errors.New("invalid client tool choice")
	}
	var kind, name, namespace string
	_ = json.Unmarshal(choice["type"], &kind)
	_ = json.Unmarshal(choice["name"], &name)
	_ = json.Unmarshal(choice["namespace"], &namespace)
	if kind == "allowed_tools" {
		var choices []json.RawMessage
		if json.Unmarshal(choice["tools"], &choices) != nil || choices == nil {
			return nil, errors.New("allowed_tools requires tool choices")
		}
		for i, item := range choices {
			mapped, err := adapter.PrepareToolChoice(item)
			if err != nil {
				return nil, err
			}
			choices[i] = mapped
		}
		choice["tools"], _ = marshalGatewayRequestJSON(choices)
		return marshalGatewayRequestJSON(choice)
	}
	if kind == "web_search_preview" {
		kind = "web_search"
		choice["type"] = json.RawMessage(`"web_search"`)
	}
	if kind == "web_search" || kind == "tool_search" {
		if !adapter.builtins[kind] {
			return nil, errors.New("tool choice names an undeclared built-in tool")
		}
		return marshalGatewayRequestJSON(choice)
	}
	if kind != "function" && kind != "custom" {
		return nil, errors.New("unsupported client tool choice type")
	}
	lookup := namespace
	if adapter.lite && lookup == "" {
		lookup = "functions"
	}
	identity, exists := adapter.identities[lookup+"\x00"+name]
	if !exists || identity.Kind != kind {
		return nil, errors.New("tool choice names an undeclared client tool")
	}
	// Captured Responses rejection: tool_choice.namespace is an unknown
	// parameter, even when the declaration lives in Lite's functions namespace.
	// Do not invent it. An explicitly supplied namespace remains caller-owned.
	return marshalGatewayRequestJSON(choice)
}

func (adapter appServerToolAdapter) mapItem(raw json.RawMessage, toUpstream bool) (json.RawMessage, error) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil || item == nil {
		return nil, errors.New("invalid Responses item")
	}
	var kind, name, namespace string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["name"], &name)
	_ = json.Unmarshal(item["namespace"], &namespace)
	if kind != "function_call" && kind != "custom_tool_call" && kind != "function_call_output" {
		return raw, nil
	}
	if name == "" {
		return raw, nil
	}
	lookup := namespace
	if adapter.lite && lookup == "" {
		lookup = "functions"
	}
	identity, ok := adapter.identities[lookup+"\x00"+name]
	if !ok {
		return raw, nil
	}
	if kind == "function_call" && identity.Kind != "function" || kind == "custom_tool_call" && identity.Kind != "custom" {
		return nil, errors.New("response tool type differs from declared client tool")
	}
	want := identity.Namespace
	if toUpstream {
		want = lookup
	}
	if namespace == want {
		return raw, nil
	}
	if want == "" {
		delete(item, "namespace")
	} else {
		item["namespace"], _ = marshalGatewayRequestJSON(want)
	}
	return marshalGatewayRequestJSON(item)
}

// MapEvent accepts one decoded SSE/WS JSON event, including response.completed
// envelopes. Partial arguments/input deltas pass through without parsing their
// content. It also supports a nonstream Responses object with an output array.
func (adapter appServerToolAdapter) MapEvent(raw []byte) ([]byte, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		return raw, nil
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(raw, &event) != nil || event == nil {
		return nil, errors.New("invalid Responses event")
	}
	changed := false
	if item, ok := event["item"]; ok && !bytes.Equal(bytes.TrimSpace(item), []byte("null")) {
		mapped, err := adapter.MapOutputItem(item)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(item, mapped) {
			event["item"] = mapped
			changed = true
		}
	}
	if response, ok := event["response"]; ok && !bytes.Equal(bytes.TrimSpace(response), []byte("null")) {
		mapped, err := adapter.MapEvent(response)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(response, mapped) {
			event["response"] = mapped
			changed = true
		}
	}
	if output, ok := event["output"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(output, &items) != nil {
			return nil, errors.New("invalid Responses output array")
		}
		outputChanged := false
		for i, item := range items {
			mapped, err := adapter.MapOutputItem(item)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(item, mapped) {
				items[i] = mapped
				outputChanged = true
			}
		}
		if outputChanged {
			event["output"], _ = marshalGatewayRequestJSON(items)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return marshalGatewayRequestJSON(event)
}
