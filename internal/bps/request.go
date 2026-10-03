// Package bps adapts Codex Responses requests to the PowerPoint BPS profile.
// It performs no I/O and never fabricates tool calls, results or encrypted state.
package bps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const BaseURL = "https://bps.openai.com/basispoints/api"
const DefaultToolsVersion = "tools-powerpoint-core-2026-08-10-2b886486"

type Options struct {
	TaskID, TurnID, ToolsVersion string
	Compact                      bool
	// SearchDisabled is an explicit gateway policy; absent tool declarations do
	// not implicitly disable BPS native search.
	SearchDisabled bool
	// ResolveCapabilities lets the gateway retain client-only effort names until
	// the admitted account's live catalog selects an advertised level. Standalone
	// Prepare callers retain the strict upstream contract unless explicitly opted in.
	ResolveCapabilities bool
	// A caller opting in must use StreamStructured for non-compact responses.
	StructuredOutput bool
}

// Policy records client semantics handled locally rather than silently discarded.
// Callers must apply ValidateResponse before accepting a completed response.
type Policy struct {
	ToolChoice         json.RawMessage
	ParallelToolCalls  *bool
	SearchDisabled     bool
	CompactionTriggers []json.RawMessage
	// ClientControls contains non-forwarded transport, formatting and telemetry
	// fields. Adaptations reports every such field to the caller for diagnostics.
	ClientControls map[string]json.RawMessage
	choice         string
	forced         *ToolIdentity
}

type Request struct {
	Body                                  []byte
	Compact, ClientStream, SearchDisabled bool
	Tools                                 *ToolCatalog
	Policy                                Policy
	Structured                            *StructuredFormat
	Model, TaskID, TurnID                 string
	Adaptations                           []string
}

func object(raw []byte) (map[string]json.RawMessage, error) {
	// RawMessage prevents float64 coercion of IDs, JSON Schemas and large ints.
	// Unmarshal validates the whole document without Decoder's extra read buffer
	// and second decode solely to reject trailing JSON.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("bps: expected JSON object: %v", err)
	}
	return obj, nil
}

func raw(v any) json.RawMessage               { b, _ := json.Marshal(v); return b }
func clone(b json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), b...) }
func stringField(m map[string]json.RawMessage, key string) string {
	value := m[key]
	if len(value) == 0 {
		return ""
	}
	// RawMessage fields come from validated objects. Common protocol names and
	// IDs have no escapes; avoid constructing a decoder for each tiny string.
	// Fall back for whitespace, escapes and invalid UTF-8 to keep JSON's exact
	// decoding behavior (including replacement of invalid UTF-8) unchanged.
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' && utf8.Valid(value) {
		plain := true
		for _, c := range value[1 : len(value)-1] {
			if c == '\\' || c == '"' || c < 0x20 {
				plain = false
				break
			}
		}
		if plain {
			return string(value[1 : len(value)-1])
		}
	}
	var s string
	_ = json.Unmarshal(value, &s)
	return s
}
func stringValue(b json.RawMessage) (string, error) {
	var s string
	if len(b) == 0 || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return "", fmt.Errorf("expected string")
	}
	err := json.Unmarshal(b, &s)
	return s, err
}
func message(role, text string) json.RawMessage {
	return raw(map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": "input_text", "text": text}}})
}
func additionalToolsID(definitions json.RawMessage) (string, error) {
	// A generated item precedes the entire caller history. Random IDs here
	// defeat prefix reuse even when tools and history are unchanged. Hash only
	// this validated declaration; caller-owned item IDs remain untouched.
	decoder := json.NewDecoder(bytes.NewReader(definitions))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("codex-agent:bps-additional-tools:v1\x00"), canonical...))
	return "at_" + hex.EncodeToString(sum[:16]), nil
}

// Prepare preserves every native history item except compaction_trigger control
// items, which are retained in Policy and select the compact endpoint. The rest
// of the context is never summarized, truncated or converted to plain text.
func Prepare(input []byte, opt Options) (*Request, error) {
	src, err := object(input)
	if err != nil {
		return nil, err
	}
	r := &Request{Compact: opt.Compact, ClientStream: false, TaskID: opt.TaskID, TurnID: opt.TurnID,
		Policy: Policy{SearchDisabled: opt.SearchDisabled, ClientControls: map[string]json.RawMessage{}}}
	if r.TaskID == "" || r.TurnID == "" {
		return nil, fmt.Errorf("bps: task_id and turn_id are required")
	}
	if value := src["stream"]; len(value) > 0 {
		if err = json.Unmarshal(value, &r.ClientStream); err != nil {
			return nil, fmt.Errorf("bps: invalid stream: %w", err)
		}
	}
	if value := src["previous_response_id"]; len(value) > 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`)) {
		return nil, fmt.Errorf("bps: previous_response_id must be expanded into full history by the session layer")
	}
	if v := src["store"]; len(v) > 0 {
		var store bool
		if json.Unmarshal(v, &store) != nil || store {
			return nil, fmt.Errorf("bps: persistent store is unsupported; use client-managed history")
		}
	}
	if v := src["background"]; len(v) > 0 && !bytes.Equal(v, []byte("false")) {
		return nil, fmt.Errorf("bps: background generation is unsupported")
	}
	if v := src["service_tier"]; len(v) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		// Compare decoded JSON values so escaped forms of supported tiers keep
		// the same semantics as their literal spelling.
		tier, err := stringValue(v)
		if err != nil || (tier != "auto" && tier != "default") {
			return nil, unsupportedParameter("service_tier", "requested service_tier is not supported by the PowerPoint upstream")
		}
	}
	var items []json.RawMessage
	if v := src["input"]; len(v) > 0 {
		if s, e := stringValue(v); e == nil {
			items = []json.RawMessage{message("user", s)}
		} else if json.Unmarshal(v, &items) != nil || items == nil {
			return nil, fmt.Errorf("bps: input must be an array or string")
		}
	}
	filtered := make([]json.RawMessage, 0, len(items))
	var additionalTools, explicitAdditionalTools []json.RawMessage
	for _, item := range items {
		obj, e := object(item)
		if e != nil {
			return nil, e
		}
		switch stringField(obj, "type") {
		case "compaction_trigger":
			r.Compact = true
			r.Policy.CompactionTriggers = append(r.Policy.CompactionTriggers, clone(item))
			continue
		case "additional_tools":
			additionalTools = append(additionalTools, obj["tools"])
			explicitAdditionalTools = append(explicitAdditionalTools, obj["tools"])
		case "tool_search_output":
			additionalTools = append(additionalTools, obj["tools"])
		}
		// items already own their bytes from Unmarshal; no caller slice aliases
		// them, and they are marshaled into Body before returning.
		filtered = append(filtered, item)
	}
	if v := src["compaction_trigger"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		r.Compact = true
		r.Policy.CompactionTriggers = append(r.Policy.CompactionTriggers, clone(v))
	}
	items = filtered
	r.Tools, err = collectTools(src["tools"], additionalTools)
	if err != nil {
		return nil, err
	}
	items, err = r.Tools.NormalizeHistory(items)
	if err != nil {
		return nil, err
	}
	if err = r.Policy.read(src, r.Tools); err != nil {
		return nil, err
	}
	r.SearchDisabled = r.Policy.SearchDisabled
	r.Tools.policy = r.Policy
	var parsedFormat *StructuredFormat
	if opt.StructuredOutput {
		parsedFormat, err = ParseStructuredFormat(src["text"])
		if err != nil {
			return nil, err
		}
		if !r.Compact {
			r.Structured = parsedFormat
		}
		if r.Structured != nil {
			for _, identity := range r.Tools.Declared {
				// The upstream may omit a tool namespace on its first stub.
				// Reject every same-name client function before ambiguous output.
				if identity.Type == "function" && identity.Name == r.Structured.ToolName {
					return nil, unsupportedParameter("text.format", "formatting function conflicts with a client-declared tool")
				}
			}
		}
	}
	var prefix []json.RawMessage
	if v := src["instructions"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		s, e := stringValue(v)
		if e != nil {
			return nil, fmt.Errorf("bps: instructions must be a string")
		}
		if s != "" {
			prefix = append(prefix, message("developer", s))
			r.Adaptations = append(r.Adaptations, "instructions -> developer message")
		}
	}
	if v := src["tools"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		var defs []json.RawMessage
		if json.Unmarshal(v, &defs) != nil {
			return nil, fmt.Errorf("bps: tools must be an array")
		}
		// Keep caller-owned declarations and their original IDs. A late discovery
		// result extends the catalog without replacing this stable current prefix.
		duplicate := false
		for _, definitions := range explicitAdditionalTools {
			if equalJSON(definitions, v) {
				duplicate = true
				break
			}
		}
		if len(defs) > 0 && !duplicate {
			id, e := additionalToolsID(v)
			if e != nil {
				return nil, e
			}
			prefix = append(prefix, raw(map[string]any{"type": "additional_tools", "id": id, "role": "developer", "tools": defs}))
			r.Adaptations = append(r.Adaptations, "tools -> input.additional_tools")
		}
	}
	items = append(prefix, items...)
	if !r.Compact {
		var constraint string
		if r.Structured != nil {
			definitions := raw([]json.RawMessage{r.Structured.ToolDefinition()})
			id, e := additionalToolsID(definitions)
			if e != nil {
				return nil, e
			}
			// Keep the caller's original prefix stable when only the output schema
			// changes. The current formatter is declared before its policy suffix.
			items = append(items, raw(map[string]any{"type": "additional_tools", "id": id, "role": "developer", "tools": definitions}))
			constraint = r.Policy.structuredDeveloperConstraint(r.Tools, r.Structured)
			r.Adaptations = append(r.Adaptations, "text.format -> validated formatting function")
		} else {
			constraint = r.Policy.developerConstraint(r.Tools)
		}
		items = append(items, message("developer", constraint))
	}
	body := map[string]json.RawMessage{"input": raw(items), "stream": raw(true), "store": raw(false), "model_selection": raw("explicit")}
	model, e := stringValue(src["model"])
	if e != nil || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("bps: explicit model is required")
	}
	r.Model = model
	body["model"] = clone(src["model"])
	version := opt.ToolsVersion
	if version == "" {
		version = DefaultToolsVersion
	}
	metadata := map[string]json.RawMessage{"task_id": raw(opt.TaskID), "turn_id": raw(opt.TurnID), "bps_tools_version_id": raw(version), "agent_iteration": raw("1")}
	if v := src["metadata"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		m, e := object(v)
		if e != nil {
			return nil, e
		}
		for _, key := range []string{"agent_iteration", "attachment_file_ids"} {
			if value, ok := m[key]; ok {
				metadata[key] = clone(value)
			}
		}
	}
	body["metadata"] = raw(metadata)
	if v := src["context_management"]; len(v) > 0 {
		body["context_management"] = clone(v)
	}
	if v := src["prompt_cache_key"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		if _, e := stringValue(v); e != nil {
			return nil, fmt.Errorf("bps: prompt_cache_key must be a string")
		}
		body["prompt_cache_key"] = clone(v)
		r.Adaptations = append(r.Adaptations, "prompt_cache_key forwarded; caller must namespace by client key and selected account")
	}
	effort := clone(src["reasoning_effort"])
	if v := src["reasoning"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		m, e := object(v)
		if e != nil {
			return nil, e
		}
		if value, ok := m["effort"]; ok {
			effort = clone(value)
		}
		for key, value := range m {
			if key != "effort" {
				r.Policy.ClientControls["reasoning."+key] = clone(value)
			}
		}
	}
	if len(effort) > 0 {
		if s, e := stringValue(effort); e != nil || s == "" {
			return nil, fmt.Errorf("bps: reasoning effort must be nonempty string")
		}
		if s, _ := stringValue(effort); !opt.ResolveCapabilities && (s == "max" || s == "ultra") {
			return nil, unsupportedParameter("reasoning.effort", "requested reasoning effort "+s+" is not supported by the PowerPoint upstream; consult /models for supported levels")
		}
		body["reasoning_effort"] = effort
	}
	// Every accepted non-forwarded control is visible to the caller. Unknown
	// semantics are rejected, never guessed to be safe to drop.
	known := map[string]bool{}
	for _, key := range strings.Fields("input model stream store metadata instructions tools reasoning reasoning_effort model_selection context_management compaction_trigger previous_response_id tool_choice parallel_tool_calls prompt_cache_key") {
		known[key] = true
	}
	for key, value := range src {
		if known[key] {
			continue
		}
		switch key {
		case "stream_options":
			// This compatibility control belongs to downstream serialization.
			// Responses terminal events already retain the observed usage for
			// both JSON and SSE; forwarding it changes the upstream contract.
			if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				controls, e := object(value)
				if e != nil {
					return nil, unsupportedParameter(key, "stream_options must be an object")
				}
				for name, control := range controls {
					switch name {
					case "reasoning_summary_delivery":
						if mode, err := stringValue(control); err != nil || mode != "sequential_cutoff" {
							return nil, unsupportedParameter(key+"."+name, "reasoning_summary_delivery must be sequential_cutoff")
						}
						// BPS already sends complete summary.done events before the
						// active reasoning item ends. Codex applies the requested
						// cutoff itself; retain their original identity/text/order.
						r.Adaptations = append(r.Adaptations, "sequential_cutoff uses native reasoning summary done events")
					case "include_usage":
						if !bytes.Equal(bytes.TrimSpace(control), []byte("true")) && !bytes.Equal(bytes.TrimSpace(control), []byte("false")) {
							return nil, unsupportedParameter(key+"."+name, "stream_options.include_usage must be a boolean")
						}
					default:
						return nil, unsupportedParameter(key+"."+name, "requested stream option is not supported by the upstream adapter")
					}
				}
			}
			r.Policy.ClientControls[key] = clone(value)
		case "max_output_tokens":
			return nil, unsupportedParameter(key, "max_output_tokens is not supported by the PowerPoint upstream")
		case "top_p":
			return nil, unsupportedParameter(key, "top_p is not supported by the selected upstream")
		case "include", "prompt_cache_key", "client_metadata", "safety_identifier", "background", "service_tier", "text", "prompt_cache_retention":
			r.Policy.ClientControls[key] = clone(value)
		default:
			return nil, fmt.Errorf("bps: unsupported request field %q", key)
		}
	}
	if v := src["text"]; len(v) > 0 && !bytes.Equal(v, []byte("null")) {
		m, e := object(v)
		if e != nil {
			return nil, e
		}
		if f := m["format"]; len(f) > 0 && !bytes.Equal(bytes.TrimSpace(f), []byte("null")) {
			fm, e := object(f)
			if parsedFormat == nil && (e != nil || stringField(fm, "type") != "text") {
				return nil, unsupportedParameter("text.format", "structured text.format is not supported by the PowerPoint upstream")
			}
		}
		for key := range m {
			if key != "format" && key != "verbosity" {
				return nil, fmt.Errorf("bps: unsupported text control %q", key)
			}
		}
	}
	keys := make([]string, 0, len(r.Policy.ClientControls))
	for key := range r.Policy.ClientControls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r.Adaptations = append(r.Adaptations, "client control retained locally: "+key)
	}
	if r.Compact {
		r.Adaptations = append(r.Adaptations, "compaction handled by /responses/compact; native history retained")
		for key := range body {
			if key != "model" && key != "input" && key != "metadata" {
				r.Policy.ClientControls[key] = clone(body[key])
				delete(body, key)
			}
		}
	}
	r.Tools.policy = r.Policy
	r.Body, err = json.Marshal(body)
	return r, err
}

func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	d := json.NewDecoder(bytes.NewReader(a))
	d.UseNumber()
	if d.Decode(&x) != nil {
		return false
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
