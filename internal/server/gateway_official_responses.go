package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type officialThread struct {
	mu                                       sync.Mutex
	rpc                                      *officialRPC
	key, owner, session, id, turn, signature string
	rollout                                  string
	model, modelSource                       string
	compactResponseDetails                   map[string]any
	expected                                 map[string]string
	expectedTools                            map[string]string
	history                                  []json.RawMessage
	pending                                  map[string]json.RawMessage
	pendingTools                             map[string]string
	custom                                   map[string]bool
	last                                     time.Time
	awaiting                                 bool
	searchExecution                          string
	definitions                              map[string]json.RawMessage
	credentials                              [32]byte
}
type officialResponseInput struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions"`
	Input        json.RawMessage   `json:"input"`
	Tools        []json.RawMessage `json:"tools"`
	Stream       bool              `json:"stream"`
	Reasoning    struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	} `json:"reasoning"`
	ServiceTier string `json:"service_tier"`
	Text        struct {
		Format struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
	Compact json.RawMessage `json:"compaction_trigger"`
}

func officialTools(raw []json.RawMessage) ([]any, map[string]bool, error) {
	custom := map[string]bool{}
	searchExecution := officialToolSearchExecution(raw)
	var convert func(json.RawMessage, string) (any, error)
	convert = func(raw json.RawMessage, namespace string) (any, error) {
		var t struct {
			Type, Name, Description string
			Parameters              json.RawMessage
			Tools                   []json.RawMessage
			Defer                   bool `json:"defer_loading"`
		}
		if json.Unmarshal(raw, &t) != nil {
			return nil, errors.New("invalid tool definition")
		}
		if t.Type == "web_search" || t.Type == "web_search_preview" {
			return nil, nil
		}
		if t.Type == "tool_search" {
			var f struct {
				Execution string `json:"execution"`
			}
			_ = json.Unmarshal(raw, &f)
			if f.Execution == "" || f.Execution == "server" {
				return nil, nil
			}
			if f.Execution != "client" {
				return nil, errors.New("invalid tool_search execution")
			}
			t.Type, t.Name = "function", officialSearchToolName
			if len(t.Parameters) == 0 {
				t.Parameters = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"],"additionalProperties":false}`)
			}
			if t.Description == "" {
				t.Description = "Search the client tool catalog. The client returns the matching tool definitions."
			}
		}
		if t.Name == "" {
			return nil, errors.New("tool name is required")
		}
		if t.Type == "namespace" {
			if namespace != "" {
				return nil, errors.New("nested tool namespaces are unsupported")
			}
			out := []any{}
			for _, child := range t.Tools {
				v, err := convert(child, t.Name)
				if err != nil {
					return nil, err
				}
				if v != nil {
					out = append(out, v)
				}
			}
			return map[string]any{"type": "namespace", "name": officialToolAlias("namespace", "", t.Name), "description": t.Description, "tools": out}, nil
		}
		if t.Type != "function" && t.Type != "custom" {
			return nil, fmt.Errorf("App Server does not support tool type %q", t.Type)
		}
		identity := namespace + "\x00" + t.Name
		if _, exists := custom[identity]; exists {
			return nil, errors.New("duplicate tool identity")
		}
		custom[identity] = false
		schema := t.Parameters
		if t.Type == "custom" {
			custom[namespace+"\x00"+t.Name] = true
			schema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
			t.Description += "\nReturn the tool's exact free-form input in the JSON input string."
		}
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		return map[string]any{"type": "function", "name": officialToolAlias("tool", namespace, t.Name), "description": t.Name + ": " + t.Description, "inputSchema": schema, "deferLoading": t.Defer && searchExecution != "client"}, nil
	}
	out := []any{}
	for _, item := range raw {
		v, err := convert(item, "")
		if err != nil {
			return nil, nil, err
		}
		if v != nil {
			out = append(out, v)
		}
	}
	return out, custom, nil
}

func officialInputItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		item, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": text}}})
		return []json.RawMessage{item}, nil
	}
	var items []json.RawMessage
	if string(bytes.TrimSpace(raw)) == "null" || json.Unmarshal(raw, &items) != nil {
		return nil, errors.New("input must be text or a Responses item array")
	}
	return items, nil
}
func officialJSONEqual(a, b json.RawMessage) bool {
	var av, bv any
	ad := json.NewDecoder(bytes.NewReader(a))
	ad.UseNumber()
	bd := json.NewDecoder(bytes.NewReader(b))
	bd.UseNumber()
	if ad.Decode(&av) != nil || bd.Decode(&bv) != nil {
		return false
	}
	ax, _ := json.Marshal(av)
	bx, _ := json.Marshal(bv)
	return bytes.Equal(ax, bx)
}
func officialInputDelta(history, input []json.RawMessage) ([]json.RawMessage, bool) {
	if len(input) < len(history) {
		return nil, false
	}
	for i := range history {
		if !officialJSONEqual(officialHistoryComparable(history[i]), officialHistoryComparable(input[i])) {
			return nil, false
		}
	}
	return input[len(history):], true
}
func officialUserInput(raw json.RawMessage) ([]any, error) {
	var item struct {
		Role, Type string
		Content    json.RawMessage
	}
	if json.Unmarshal(raw, &item) != nil || item.Role != "user" {
		return nil, errors.New("a new App Server turn requires a user message")
	}
	var text string
	if json.Unmarshal(item.Content, &text) == nil {
		return []any{map[string]string{"type": "text", "text": text}}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(item.Content, &parts) != nil {
		return nil, errors.New("invalid user content")
	}
	out := []any{}
	for _, p := range parts {
		var kind, text, image string
		_ = json.Unmarshal(p["type"], &kind)
		switch kind {
		case "input_text":
			_ = json.Unmarshal(p["text"], &text)
			out = append(out, map[string]string{"type": "text", "text": text})
		case "input_image":
			_ = json.Unmarshal(p["image_url"], &image)
			out = append(out, map[string]string{"type": "image", "url": image})
		default:
			return nil, fmt.Errorf("unsupported App Server input content %q", kind)
		}
	}
	return out, nil
}

func (rt *officialRuntime) responseThread(ctx context.Context, key, session string, account Account, proxy *ProxyEntry, input officialResponseInput, items []json.RawMessage) (*officialThread, []json.RawMessage, error) {
	credentials := officialCredentialDigest(account)
	tools, custom, err := officialTools(input.Tools)
	if err != nil {
		return nil, nil, err
	}
	sig, err := officialResponseSignature(input)
	if err != nil {
		return nil, nil, err
	}
	mapkey := key + "\x00" + gatewayAccountRef(account) + "\x00" + session
	rt.mu.Lock()
	existing := rt.threads[mapkey]
	rt.mu.Unlock()
	if existing != nil {
		existing.mu.Lock()
		if existing.signature == sig && existing.rpc.alive() {
			if delta, ok := officialInputDelta(existing.history, items); ok {
				if existing.credentials != credentials {
					refreshCtx, stop := context.WithTimeout(ctx, 60*time.Second)
					_, refreshErr := existing.rpc.call(refreshCtx, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": account.AccessToken, "chatgptAccountId": account.AccountID})
					stop()
					if refreshErr != nil {
						existing.mu.Unlock()
						existing.rpc.close()
						rt.mu.Lock()
						if rt.threads[mapkey] == existing {
							delete(rt.threads, mapkey)
						}
						rt.mu.Unlock()
						return nil, nil, fmt.Errorf("official Responses credential update failed: %w", refreshErr)
					}
					existing.credentials = credentials
					existing.rpc.authMu.Lock()
					existing.rpc.authAccount = account
					existing.rpc.authMu.Unlock()
				}
				return existing, delta, nil
			}
		}
		existing.mu.Unlock()
		existing.rpc.close()
		rt.mu.Lock()
		delete(rt.threads, mapkey)
		rt.mu.Unlock()
	}
	rpc, err := rt.startClientToolRPC(ctx, account, proxy, input.Model)
	if err != nil {
		return nil, nil, err
	}
	config := map[string]any{"model_reasoning_effort": input.Reasoning.Effort, "web_search": "disabled", "features.shell_tool": false, "features.view_image": false, "features.js_repl": false, "features.code_mode": false, "features.apps": false, "features.multi_agent": false, "features.multi_agent_v2": false, "features.image_generation": false, "features.hooks": false, "features.plugins": false, "features.skip_host_skill_discovery": true, "tools.update_plan.enabled": false, "tools.experimental_request_user_input.enabled": false, "skills.include_instructions": false, "skills.bundled.enabled": false}
	// The client owns its tools and environment. The bridge's temporary cwd is
	// irrelevant model context and changes whenever an official thread restarts,
	// breaking the shared prompt prefix before the caller's entire history.
	// Hosted/native tasks retain their real environment through other paths.
	config["include_environment_context"] = false
	officialClientToolFeaturePolicy(config)
	for _, raw := range input.Tools {
		var t struct{ Type string }
		_ = json.Unmarshal(raw, &t)
		if strings.HasPrefix(t.Type, "web_search") {
			config["web_search"] = "live"
		}
	}
	params := map[string]any{"model": input.Model, "modelProvider": "openai", "cwd": rpc.work, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": false, "experimentalRawEvents": true, "dynamicTools": tools, "config": config}
	if input.Instructions != "" {
		params["baseInstructions"] = input.Instructions
	}
	result, err := rpc.call(ctx, "thread/start", params)
	if err != nil {
		rpc.close()
		return nil, nil, err
	}
	var started struct {
		Thread struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	if json.Unmarshal(result, &started) != nil || started.Thread.ID == "" || started.Model != input.Model {
		rpc.close()
		return nil, nil, errors.New("App Server thread model identity mismatch")
	}
	t := &officialThread{rpc: rpc, credentials: credentials, key: key, owner: gatewayAccountRef(account), session: session, id: started.Thread.ID, model: started.Model, signature: sig, rollout: started.Thread.Path, expected: map[string]string{}, pending: map[string]json.RawMessage{}, custom: custom, searchExecution: officialToolSearchExecution(input.Tools), definitions: officialOriginalToolDefinitions(input.Tools), last: time.Now()}
	t.mu.Lock()
	rt.mu.Lock()
	rt.threads[mapkey] = t
	rt.mu.Unlock()
	return t, items, nil
}

func officialToolOutput(raw json.RawMessage) (string, []any, error) {
	var item struct {
		Type   string          `json:"type"`
		Call   string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &item) != nil || item.Call == "" || (item.Type != "function_call_output" && item.Type != "custom_tool_call_output" && item.Type != "tool_search_output") {
		return "", nil, errors.New("expected tool output")
	}
	if item.Type == "tool_search_output" {
		return item.Call, []any{map[string]string{"type": "inputText", "text": string(raw)}}, nil
	}
	var text string
	if json.Unmarshal(item.Output, &text) == nil {
		return item.Call, []any{map[string]string{"type": "inputText", "text": text}}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(item.Output, &parts) != nil {
		return "", nil, errors.New("invalid tool output")
	}
	out := []any{}
	for _, p := range parts {
		var kind, text, image string
		_ = json.Unmarshal(p["type"], &kind)
		switch kind {
		case "input_text":
			_ = json.Unmarshal(p["text"], &text)
			out = append(out, map[string]string{"type": "inputText", "text": text})
		case "input_image":
			_ = json.Unmarshal(p["image_url"], &image)
			out = append(out, map[string]string{"type": "inputImage", "imageUrl": image})
		default:
			return "", nil, errors.New("unsupported tool output content")
		}
	}
	return item.Call, out, nil
}

// runResponse emits one real upstream response, then pauses the official agent
// at its dynamic-tool boundary. Tool results on the next HTTP request resolve
// the official JSON-RPC requests; local client tools never execute on this host.
func (t *officialThread) runResponse(ctx context.Context, input officialResponseInput, delta []json.RawMessage, emit func(map[string]any) error) (json.RawMessage, []json.RawMessage, error) {
	defer t.mu.Unlock()
	t.last = time.Now()
	completed := false
	defer func() {
		if !completed {
			t.rpc.close()
		}
	}()
	if t.awaiting {
		outputs := map[string][]any{}
		for _, raw := range delta {
			call, value, err := officialToolOutput(raw)
			if err != nil {
				return nil, nil, err
			}
			var f struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &f)
			kind, expected := t.expected[call]
			if !expected || f.Type != officialExpectedToolOutput(kind) {
				return nil, nil, errors.New("unexpected tool output identity or type")
			}
			if _, exists := outputs[call]; exists {
				return nil, nil, errors.New("duplicate tool output")
			}
			outputs[call] = value
		}
		if len(outputs) != len(t.expected) {
			return nil, nil, errors.New("all pending tool calls require one output")
		}
		for len(outputs) > 0 {
			for call, id := range t.pending {
				if value, ok := outputs[call]; ok {
					if err := t.rpc.send(map[string]any{"id": id, "result": map[string]any{"contentItems": value, "success": true}}); err != nil {
						return nil, nil, err
					}
					delete(outputs, call)
					delete(t.pending, call)
					delete(t.pendingTools, call)
				}
			}
			if len(outputs) == 0 {
				break
			}
			raw, err := t.rpc.receive(ctx)
			if err != nil {
				return nil, nil, err
			}
			var m officialResponseEvent
			if json.Unmarshal(raw, &m) != nil {
				return nil, nil, errors.New("invalid App Server event")
			}
			if m.Params.Thread != "" && m.Params.Thread != t.id {
				continue
			}
			if m.Params.Turn != "" && m.Params.Turn != t.turn {
				continue
			}
			if m.Method == "error" || m.Method == "turn/completed" {
				return nil, nil, errors.New("official turn ended before all tools were requested")
			}
			if err = t.capturePending(raw); err != nil {
				return nil, nil, err
			}
		}
	} else {
		if len(delta) == 0 {
			return nil, nil, errors.New("new turn has no input")
		}
		// Replayed function outputs can resume inference after process eviction.
		// Empty turn input is accepted by the official turn/start protocol.
		prompt := []any{}
		prior := delta
		var last struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(delta[len(delta)-1], &last)
		if last.Role == "user" {
			var err error
			prompt, err = officialUserInput(delta[len(delta)-1])
			if err != nil {
				return nil, nil, err
			}
			prior = delta[:len(delta)-1]
		}
		if len(prior) > 0 {
			injected, err := t.injectItems(prior)
			if err != nil {
				return nil, nil, err
			}
			if _, err = t.rpc.call(ctx, "thread/inject_items", map[string]any{"threadId": t.id, "items": injected}); err != nil {
				return nil, nil, err
			}
		}
		params := map[string]any{"threadId": t.id, "input": prompt, "model": input.Model, "effort": input.Reasoning.Effort}
		if input.Reasoning.Summary != "" {
			params["summary"] = input.Reasoning.Summary
		}
		if input.ServiceTier != "" {
			params["serviceTier"] = input.ServiceTier
		}
		if input.Text.Format.Type == "json_schema" {
			params["outputSchema"] = input.Text.Format.Schema
		}
		started, err := t.rpc.call(ctx, "turn/start", params)
		if err != nil {
			return nil, nil, err
		}
		var start struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(started, &start) != nil || start.Turn.ID == "" {
			return nil, nil, errors.New("official turn/start omitted turn identity")
		}
		t.turn = start.Turn.ID
	}
	if observer := gatewayAttemptObservationFrom(ctx); observer != nil {
		sent, _ := json.Marshal(map[string]string{"model": input.Model})
		observer.ObserveSent("/responses", sent)
	}
	t.expected = map[string]string{}
	t.expectedTools = map[string]string{}
	id := "resp_app_" + strings.ReplaceAll(bpsUUID(), "-", "")
	output := []json.RawMessage{}
	response := map[string]any{"id": id, "object": "response", "status": "in_progress", "output": output}
	// The request model is intent, not an observed runtime identity.
	if t.model != "" {
		if t.modelSource == "" {
			t.modelSource = "app_server_thread"
		}
		response["model"] = t.model
		response["metadata"] = map[string]string{"app_server_model_source": t.modelSource}
	}
	if err := emit(map[string]any{"type": "response.created", "response": response}); err != nil {
		return nil, nil, err
	}
	indexes := map[string]int{}
	texts := map[string]string{}
	summaries := map[string]map[int]string{}
	done := map[string]bool{}
	upstreamCompleted := false
	usageTotals := map[string]json.RawMessage{}
	usageComplete := true
	rawResponses := 0
	finish := func() (json.RawMessage, []json.RawMessage, error) {
		for _, item := range output {
			if item == nil {
				return nil, nil, errors.New("App Server completed before raw output item")
			}
		}
		t.awaiting = len(t.expected) > 0
		t.history = append(t.history, delta...)
		t.history = append(t.history, output...)
		response["status"] = "completed"
		response["output"] = output
		encoded, err := json.Marshal(response)
		if err != nil {
			return nil, nil, err
		}
		completed = true
		return encoded, output, nil
	}
	for {
		raw, err := t.rpc.receive(ctx)
		if err != nil {
			return nil, nil, err
		}
		var m officialResponseEvent
		if json.Unmarshal(raw, &m) != nil {
			return nil, nil, errors.New("invalid App Server event")
		}
		if m.Params.Thread != "" && m.Params.Thread != t.id {
			continue
		}
		if m.Params.Turn != "" && m.Params.Turn != t.turn {
			continue
		}
		if len(m.ID) > 0 {
			if err = t.capturePending(raw); err != nil {
				return nil, nil, err
			}
			continue
		}
		switch m.Method {
		case "thread/settings/updated", "model/rerouted":
			model, source := m.Params.ThreadSettings.Model, "app_server_settings"
			if m.Method == "model/rerouted" {
				model, source = m.Params.ToModel, "app_server_rerouted"
			}
			if model != "" {
				t.model, t.modelSource = model, source
				response["model"] = model
				response["metadata"] = map[string]string{"app_server_model_source": source}
			}
		case "item/reasoning/summaryPartAdded", "item/reasoning/summaryTextDelta":
			if m.Params.ItemID == "" || done[m.Params.ItemID] || m.Params.SummaryIndex < 0 || m.Params.SummaryIndex > 4096 {
				return nil, nil, errors.New("invalid or late reasoning summary delta")
			}
			index, exists := indexes[m.Params.ItemID]
			if !exists {
				index = len(output)
				indexes[m.Params.ItemID] = index
				output = append(output, nil)
				if err = emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "reasoning", "id": m.Params.ItemID, "summary": []any{}}}); err != nil {
					return nil, nil, err
				}
			}
			if summaries[m.Params.ItemID] == nil {
				summaries[m.Params.ItemID] = map[int]string{}
			}
			parts := summaries[m.Params.ItemID]
			if _, exists := parts[m.Params.SummaryIndex]; !exists {
				parts[m.Params.SummaryIndex] = ""
				if err = emit(map[string]any{"type": "response.reasoning_summary_part.added", "output_index": index, "item_id": m.Params.ItemID, "summary_index": m.Params.SummaryIndex, "part": map[string]string{"type": "summary_text", "text": ""}}); err != nil {
					return nil, nil, err
				}
			}
			if m.Method == "item/reasoning/summaryTextDelta" {
				parts[m.Params.SummaryIndex] += m.Params.Delta
				if err = emit(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": index, "item_id": m.Params.ItemID, "summary_index": m.Params.SummaryIndex, "delta": m.Params.Delta}); err != nil {
					return nil, nil, err
				}
			}
		case "item/agentMessage/delta":
			if m.Params.ItemID == "" || done[m.Params.ItemID] {
				return nil, nil, errors.New("invalid or late assistant delta")
			}
			index, ok := indexes[m.Params.ItemID]
			if !ok {
				index = len(output)
				indexes[m.Params.ItemID] = index
				output = append(output, nil)
				if err = emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "message", "id": m.Params.ItemID, "role": "assistant", "status": "in_progress", "content": []any{}}}); err != nil {
					return nil, nil, err
				}
				if err = emit(map[string]any{"type": "response.content_part.added", "output_index": index, "item_id": m.Params.ItemID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
					return nil, nil, err
				}
			}
			texts[m.Params.ItemID] += m.Params.Delta
			if err = emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": m.Params.ItemID, "content_index": 0, "delta": m.Params.Delta}); err != nil {
				return nil, nil, err
			}
		case "rawResponseItem/completed":
			var f struct {
				Type, ID, Role, CallID string
				Content                []struct{ Type, Text string }
			}
			var itemFields map[string]json.RawMessage
			if json.Unmarshal(m.Params.Item, &itemFields) != nil {
				return nil, nil, errors.New("invalid raw Responses item")
			}
			_ = json.Unmarshal(itemFields["type"], &f.Type)
			_ = json.Unmarshal(itemFields["role"], &f.Role)
			// Raw events include the official harness's own prompt and tool results.
			if f.Type == "additional_tools" || f.Type == "configuration_update" || f.Type == "compaction_trigger" {
				continue
			}
			if f.Type == "message" && f.Role != "assistant" || strings.HasSuffix(f.Type, "_output") && f.Type != "tool_search_output" {
				continue
			}
			item, err := t.restoreItem(m.Params.Item)
			if err != nil {
				return nil, nil, err
			}
			_ = json.Unmarshal(item, &itemFields)
			_ = json.Unmarshal(itemFields["id"], &f.ID)
			_ = json.Unmarshal(itemFields["type"], &f.Type)
			_ = json.Unmarshal(itemFields["call_id"], &f.CallID)
			_ = json.Unmarshal(itemFields["content"], &f.Content)
			if f.Type == "function_call" || f.Type == "custom_tool_call" || f.Type == "tool_search_call" && t.searchExecution == "client" {
				if f.CallID == "" {
					return nil, nil, errors.New("raw tool call omitted call_id")
				}
				if _, exists := t.expected[f.CallID]; exists {
					return nil, nil, errors.New("duplicate raw tool call identity")
				}
				t.expected[f.CallID] = f.Type
				var name, namespace string
				_ = json.Unmarshal(itemFields["name"], &name)
				_ = json.Unmarshal(itemFields["namespace"], &namespace)
				if f.Type == "tool_search_call" {
					name = officialSearchToolName
					namespace = ""
				}
				t.expectedTools[f.CallID] = namespace + "\x00" + name
				if pendingIdentity, exists := t.pendingTools[f.CallID]; exists && pendingIdentity != t.expectedTools[f.CallID] {
					return nil, nil, errors.New("dynamic tool request identity does not match raw call")
				}
			}
			if f.ID != "" && done[f.ID] {
				return nil, nil, errors.New("duplicate raw output item")
			}
			index, ok := indexes[f.ID]
			if !ok || f.ID == "" {
				index = len(output)
				output = append(output, item)
				if f.ID != "" {
					indexes[f.ID] = index
				}
				if err = emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item}); err != nil {
					return nil, nil, err
				}
			} else {
				output[index] = item
			}
			if f.Type == "reasoning" {
				var parts []struct{ Type, Text string }
				_ = json.Unmarshal(itemFields["summary"], &parts)
				for partIndex, streamed := range summaries[f.ID] {
					if partIndex >= len(parts) || parts[partIndex].Type != "summary_text" || parts[partIndex].Text != streamed {
						return nil, nil, errors.New("reasoning summary delta does not match raw response item")
					}
				}
				for i, part := range parts {
					if part.Type != "summary_text" {
						continue
					}
					if err = emit(map[string]any{"type": "response.reasoning_summary_text.done", "output_index": index, "item_id": f.ID, "summary_index": i, "text": part.Text}); err != nil {
						return nil, nil, err
					}
					if err = emit(map[string]any{"type": "response.reasoning_summary_part.done", "output_index": index, "item_id": f.ID, "summary_index": i, "part": map[string]string{"type": "summary_text", "text": part.Text}}); err != nil {
						return nil, nil, err
					}
				}
			}
			if f.Type == "message" {
				var text strings.Builder
				for _, part := range f.Content {
					if part.Type == "output_text" {
						text.WriteString(part.Text)
					}
				}
				if streamed, exists := texts[f.ID]; exists && streamed != text.String() {
					return nil, nil, errors.New("assistant delta text does not match raw response item")
				}
				for i, part := range f.Content {
					if part.Type != "output_text" {
						continue
					}
					if _, exists := texts[f.ID]; !exists {
						if err = emit(map[string]any{"type": "response.content_part.added", "output_index": index, "item_id": f.ID, "content_index": i, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
							return nil, nil, err
						}
						if err = emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "item_id": f.ID, "content_index": i, "delta": part.Text}); err != nil {
							return nil, nil, err
						}
					}
					if err = emit(map[string]any{"type": "response.output_text.done", "output_index": index, "item_id": f.ID, "content_index": i, "text": part.Text}); err != nil {
						return nil, nil, err
					}
					var parts []json.RawMessage
					_ = json.Unmarshal(itemFields["content"], &parts)
					if err = emit(map[string]any{"type": "response.content_part.done", "output_index": index, "item_id": f.ID, "content_index": i, "part": parts[i]}); err != nil {
						return nil, nil, err
					}
				}
			}
			if f.Type == "function_call" || f.Type == "custom_tool_call" {
				field, event := "arguments", "response.function_call_arguments.done"
				if f.Type == "custom_tool_call" {
					field, event = "input", "response.custom_tool_call_input.done"
				}
				if err = emit(map[string]any{"type": event, "output_index": index, "item_id": f.ID, field: itemFields[field]}); err != nil {
					return nil, nil, err
				}
			}
			if err = emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}); err != nil {
				return nil, nil, err
			}
			if f.ID != "" {
				done[f.ID] = true
			}
		case "rawResponse/completed":
			rawResponses++
			for call, identity := range t.pendingTools {
				if expected, ok := t.expectedTools[call]; !ok || expected != identity {
					return nil, nil, errors.New("dynamic tool request has no matching raw call")
				}
			}
			if len(m.Params.Usage) == 0 {
				usageComplete = false
			} else if err := officialAccumulateUsage(usageTotals, m.Params.Usage); err != nil {
				return nil, nil, err
			}
			if usageComplete {
				response["usage"] = officialResponseUsage(usageTotals)
			} else {
				delete(response, "usage")
			}
			if rawResponses == 1 && len(m.Params.UsageMetadata) > 0 && string(m.Params.UsageMetadata) != "null" {
				response["usage_metadata"] = m.Params.UsageMetadata
			} else {
				// Metadata describes one native response; do not report the last
				// response's billing amount as the total for a multi-response turn.
				delete(response, "usage_metadata")
			}
			upstreamCompleted = true
			if len(t.expected) > 0 {
				return finish()
			}
		case "turn/completed":
			if m.Params.TurnValue.ID != "" && m.Params.TurnValue.ID != t.turn {
				continue
			}
			if m.Params.TurnValue.Status != "completed" {
				return nil, nil, officialResponseFailure("official App Server turn failed", m.Params.TurnValue.Error)
			}
			if !upstreamCompleted {
				return nil, nil, errors.New("official turn ended without a raw response completion")
			}
			return finish()
		case "error":
			if !m.Params.WillRetry {
				return nil, nil, officialResponseFailure("official App Server inference error", m.Params.Error)
			}
		}
	}
}

type officialResponseEvent struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Thread         string                     `json:"threadId"`
		Turn           string                     `json:"turnId"`
		ItemID         string                     `json:"itemId"`
		Delta          string                     `json:"delta"`
		Item           json.RawMessage            `json:"item"`
		ResponseID     string                     `json:"responseId"`
		Usage          map[string]json.RawMessage `json:"usage"`
		UsageMetadata  json.RawMessage            `json:"usageMetadata"`
		SummaryIndex   int                        `json:"summaryIndex"`
		ToModel        string                     `json:"toModel"`
		ThreadSettings struct {
			Model string `json:"model"`
		} `json:"threadSettings"`
		WillRetry bool            `json:"willRetry"`
		Error     json.RawMessage `json:"error"`
		TurnValue struct {
			ID, Status string
			Error      json.RawMessage
		} `json:"turn"`
	} `json:"params"`
}

// App Server wraps upstream failures in TurnError. Keep its actionable message
// instead of erasing every failure into one generic 502, using the same bounded
// credential redaction as management observations. Do not expose raw event data.
func officialResponseFailure(prefix string, raw json.RawMessage) error {
	var detail struct {
		Message string          `json:"message"`
		Info    json.RawMessage `json:"codexErrorInfo"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		return errors.New(prefix)
	}
	var code string
	status := 502
	if json.Unmarshal(detail.Info, &code) != nil {
		var variants map[string]json.RawMessage
		if json.Unmarshal(detail.Info, &variants) == nil && len(variants) == 1 {
			for variant := range variants {
				code = variant
			}
		}
	}
	switch code {
	case "contextWindowExceeded", "badRequest":
		status = 400
	case "usageLimitExceeded", "rateLimitExceeded":
		status = 429
	case "unauthorized":
		status = 401
	}
	// The official TurnError may wrap a structured upstream API error as its
	// message. Inspect exact fields, never classify by free-form substrings.
	var upstream struct {
		Type   string                               `json:"type"`
		Status int                                  `json:"status"`
		Error  struct{ Type, Code, Message string } `json:"error"`
	}
	// compact_remote_v2 adds this fixed envelope before the JSON API error.
	// Strip only the official prefix; never infer status from arbitrary text.
	structuredMessage := strings.TrimPrefix(detail.Message, "Error running remote compact task: ")
	if json.Unmarshal([]byte(structuredMessage), &upstream) == nil && upstream.Type == "error" && upstream.Error.Type != "" {
		if upstream.Status >= 400 && upstream.Status < 500 {
			status = upstream.Status
		} else if upstream.Error.Type == "invalid_request_error" {
			status = 400
		}
		if upstream.Error.Code != "" {
			code = upstream.Error.Code
		}
		if upstream.Error.Message != "" {
			detail.Message = upstream.Error.Message
		}
	}
	if code == "contextWindowExceeded" {
		code = "context_length_exceeded"
	}
	if code = boundedGatewayMetadata(code, 128); code != "" {
		prefix += " (" + code + ")"
	}
	if message := boundedGatewayMetadata(detail.Message, 2048); message != "" {
		prefix += ": " + message
	}
	return &officialProtocolError{message: prefix, status: status, code: code, source: "upstream"}
}

func officialResponseUsage(raw map[string]json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := map[string]any{}
	copyCounter := func(target map[string]any, destination, source string) {
		if value := raw[source]; len(value) > 0 && string(value) != "null" {
			target[destination] = value
		}
	}
	copyCounter(out, "input_tokens", "inputTokens")
	copyCounter(out, "output_tokens", "outputTokens")
	copyCounter(out, "total_tokens", "totalTokens")
	input, output := map[string]any{}, map[string]any{}
	copyCounter(input, "cached_tokens", "cachedInputTokens")
	copyCounter(input, "cache_write_tokens", "cacheWriteInputTokens")
	copyCounter(output, "reasoning_tokens", "reasoningOutputTokens")
	if len(input) > 0 {
		out["input_tokens_details"] = input
	}
	if len(output) > 0 {
		out["output_tokens_details"] = output
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// The executable dynamic-tool schema only supports functions, but native
// ResponseItem history supports both function and custom calls. Restore wrappers
// for tools in the current catalog and retain completed calls to retired tools in
// their native form. History is context, not permission to execute a tool again.
func (t *officialThread) injectItems(items []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(items))
	wrappedCustomCalls := map[string]bool{}
	for _, raw := range items {
		var f map[string]json.RawMessage
		if json.Unmarshal(raw, &f) != nil {
			return nil, errors.New("invalid history item")
		}
		var kind, name, namespace, callID string
		_ = json.Unmarshal(f["type"], &kind)
		_ = json.Unmarshal(f["name"], &name)
		_ = json.Unmarshal(f["namespace"], &namespace)
		_ = json.Unmarshal(f["call_id"], &callID)
		if kind == "reasoning" {
			var encrypted, itemID string
			_ = json.Unmarshal(f["encrypted_content"], &encrypted)
			_ = json.Unmarshal(f["id"], &itemID)
			if encrypted != "" && itemID == "" {
				// inject_items assigns a fresh rs_UUID to missing IDs, binding an
				// existing encrypted item to the wrong identity. The official
				// request writer strips legacy IDs without an underscore. Use an
				// internal stable legacy identity so the eventual model request
				// preserves the client's absent ID and exact encrypted content.
				// Never replace an ID actually supplied with the encrypted item.
				digest := sha256.Sum256([]byte(encrypted))
				f["id"], _ = json.Marshal("gateway-reasoning-" + hex.EncodeToString(digest[:]))
			}
		}
		if kind == "" && len(f["role"]) > 0 {
			f["type"] = json.RawMessage(`"message"`)
		}
		if kind == "tool_search_call" && t.searchExecution == "client" {
			var itemID string
			_ = json.Unmarshal(f["id"], &itemID)
			if strings.HasPrefix(itemID, "tsc_") {
				f["id"], _ = json.Marshal("fc_" + strings.TrimPrefix(itemID, "tsc_"))
			}
			name = officialSearchToolName
			namespace = ""
			kind = "function_call"
			f["type"] = json.RawMessage(`"function_call"`)
			f["name"], _ = json.Marshal(name)
			f["arguments"], _ = json.Marshal(string(f["arguments"]))
			delete(f, "execution")
			delete(f, "status")
		} else if kind == "tool_search_output" && t.searchExecution == "client" {
			f = map[string]json.RawMessage{"type": json.RawMessage(`"function_call_output"`), "call_id": f["call_id"]}
			f["output"], _ = json.Marshal(string(raw))
			kind = "function_call_output"
		} else if kind == "tool_search_call" || kind == "tool_search_output" {
			f["execution"] = json.RawMessage(`"client"`)
			if kind == "tool_search_output" {
				if err := t.rewriteSearchDefinitions(f, true); err != nil {
					return nil, err
				}
			}
		}
		isCustom, declared := t.custom[namespace+"\x00"+name]
		if kind == "custom_tool_call" && declared && isCustom {
			var input string
			if json.Unmarshal(f["input"], &input) != nil {
				return nil, errors.New("invalid custom history input")
			}
			args, _ := json.Marshal(map[string]string{"input": input})
			f["arguments"], _ = json.Marshal(string(args))
			f["type"] = json.RawMessage(`"function_call"`)
			delete(f, "input")
			wrappedCustomCalls[callID] = true
		} else if kind == "custom_tool_call_output" && wrappedCustomCalls[callID] {
			f["type"] = json.RawMessage(`"function_call_output"`)
		}
		if declared && (kind == "function_call" && !isCustom || kind == "custom_tool_call" && isCustom) {
			f["name"], _ = json.Marshal(officialToolAlias("tool", namespace, name))
			if namespace != "" {
				f["namespace"], _ = json.Marshal(officialToolAlias("namespace", "", namespace))
			}
		}
		// Earlier bridge responses exposed a custom call with the function
		// wrapper's fc_ item ID. That is valid inside the wrapper, but native
		// replay rejects it once the old tool leaves the current catalog. IDs
		// are typed by Responses; retain their opaque suffix and the call_id
		// pairing while translating only the known function/custom prefixes.
		var replayKind, itemID string
		_ = json.Unmarshal(f["type"], &replayKind)
		_ = json.Unmarshal(f["id"], &itemID)
		from, to := "", ""
		switch replayKind {
		case "custom_tool_call":
			from, to = "fc_", "ctc_"
		case "function_call":
			from, to = "ctc_", "fc_"
		case "custom_tool_call_output":
			from, to = "fco_", "ctco_"
		case "function_call_output":
			from, to = "ctco_", "fco_"
		}
		if from != "" && strings.HasPrefix(itemID, from) {
			f["id"], _ = json.Marshal(to + strings.TrimPrefix(itemID, from))
		}
		if len(f["role"]) > 0 {
			var text, role string
			_ = json.Unmarshal(f["role"], &role)
			if json.Unmarshal(f["content"], &text) == nil {
				partType := "input_text"
				if role == "assistant" {
					partType = "output_text"
				}
				f["content"], _ = json.Marshal([]any{map[string]string{"type": partType, "text": text}})
			}
		}
		encoded, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded)
	}
	return out, nil
}
func (t *officialThread) capturePending(raw []byte) error {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Call      string `json:"callId"`
			Thread    string `json:"threadId"`
			Turn      string `json:"turnId"`
			Tool      string `json:"tool"`
			Namespace string `json:"namespace"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return errors.New("invalid App Server request")
	}
	if len(m.ID) == 0 {
		return nil
	}
	if m.Method == "item/tool/call" {
		if m.Params.Thread != "" && m.Params.Thread != t.id || m.Params.Turn != "" && m.Params.Turn != t.turn {
			return errors.New("dynamic tool request belongs to a different turn")
		}
		namespace, name, declared := t.originalTool(m.Params.Namespace, m.Params.Tool)
		if !declared {
			return errors.New("official runtime requested an undeclared tool")
		}
		if identity, ok := t.expectedTools[m.Params.Call]; ok && identity != namespace+"\x00"+name {
			return errors.New("dynamic tool request identity does not match raw call")
		}
		if _, duplicate := t.pending[m.Params.Call]; duplicate {
			return errors.New("duplicate dynamic tool request")
		}
		if m.Params.Call == "" {
			return errors.New("dynamic tool call has no callId")
		}
		if t.pendingTools == nil {
			t.pendingTools = map[string]string{}
		}
		t.pendingTools[m.Params.Call] = namespace + "\x00" + name
		t.pending[m.Params.Call] = m.ID
		return nil
	}
	_ = t.rpc.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "operation is unavailable on the Responses client-tool bridge"}})
	return fmt.Errorf("unexpected App Server request %s", m.Method)
}
func (t *officialThread) restoreItem(raw json.RawMessage) (json.RawMessage, error) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil, errors.New("invalid raw Responses item")
	}
	var kind, name, namespace string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["name"], &name)
	_ = json.Unmarshal(item["namespace"], &namespace)
	if kind == "function_call" {
		originalNamespace, originalName, declared := t.originalTool(namespace, name)
		if !declared {
			return nil, fmt.Errorf("official runtime emitted an undeclared tool call (namespace=%q name=%q declared=%d)", boundedGatewayMetadata(namespace, 128), boundedGatewayMetadata(name, 128), len(t.custom))
		}
		namespace, name = originalNamespace, originalName
		item["name"], _ = json.Marshal(name)
		if namespace == "" {
			delete(item, "namespace")
		} else {
			item["namespace"], _ = json.Marshal(namespace)
		}
	}
	if kind == "function_call" && name == officialSearchToolName && namespace == "" {
		var arguments string
		if json.Unmarshal(item["arguments"], &arguments) != nil || !json.Valid([]byte(arguments)) {
			return nil, errors.New("invalid client tool_search arguments")
		}
		item["type"] = json.RawMessage(`"tool_search_call"`)
		item["execution"] = json.RawMessage(`"client"`)
		item["arguments"] = json.RawMessage(arguments)
		var itemID string
		_ = json.Unmarshal(item["id"], &itemID)
		if strings.HasPrefix(itemID, "fc_") {
			item["id"], _ = json.Marshal("tsc_" + strings.TrimPrefix(itemID, "fc_"))
		}
		delete(item, "name")
		delete(item, "namespace")
	}
	if kind == "tool_search_call" || kind == "tool_search_output" {
		if t.searchExecution == "client" {
			return nil, errors.New("unexpected runtime tool_search for client-owned discovery")
		}
		// The official runtime also enables discovery implicitly when it has
		// deferred dynamic tools. An absent client tool_search declaration does
		// not make that native search a client-owned operation.
		item["execution"] = json.RawMessage(`"server"`)
		if kind == "tool_search_output" {
			if err := t.rewriteSearchDefinitions(item, false); err != nil {
				return nil, err
			}
		}
	}
	if kind == "function_call" && t.custom[namespace+"\x00"+name] {
		input, err := officialCustomToolInput(item["arguments"])
		if err != nil {
			return nil, err
		}
		delete(item, "arguments")
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		item["input"], _ = json.Marshal(input)
		return json.Marshal(item)
	}
	return json.Marshal(item)
}

// compactResponse requires t.mu. Official 0.155.0 emits compaction lifecycle and
// usage, but not its encrypted item as a raw notification. The authoritative
// replacement history comes from the new compacted rollout checkpoint.
func (t *officialThread) compactResponse(ctx context.Context, items []json.RawMessage) ([]json.RawMessage, error) {
	t.compactResponseDetails = nil
	if t.awaiting {
		return nil, errors.New("complete pending tools before compacting this App Server thread")
	}
	t.last = time.Now()
	completed := false
	defer func() {
		if !completed {
			t.rpc.close()
		}
	}()
	delta, ok := officialInputDelta(t.history, items)
	if !ok {
		return nil, errors.New("compaction input does not extend official thread history")
	}
	if len(delta) > 0 {
		injected, err := t.injectItems(delta)
		if err != nil {
			return nil, err
		}
		if _, err = t.rpc.call(ctx, "thread/inject_items", map[string]any{"threadId": t.id, "items": injected}); err != nil {
			return nil, err
		}
	}
	path, err := officialRolloutPath(t.rpc.home, t.rollout)
	if err != nil {
		return nil, err
	}
	var offset int64
	if stat, err := os.Stat(path); err == nil {
		offset = stat.Size()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err = t.rpc.call(ctx, "thread/compact/start", map[string]any{"threadId": t.id}); err != nil {
		return nil, err
	}
	if observer := gatewayAttemptObservationFrom(ctx); observer != nil {
		sent, _ := json.Marshal(map[string]any{"model": t.model})
		observer.ObserveSent("/responses/compact", sent)
	}
	turn, responseID := "", ""
	details := map[string]any{}
	if t.model != "" {
		source := t.modelSource
		if source == "" {
			source = "app_server_thread"
		}
		details["model"] = t.model
		details["metadata"] = map[string]string{"app_server_model_source": source}
	}
	usageTotals, usageComplete := map[string]json.RawMessage{}, true
	rawResponses := 0
	for {
		raw, err := t.rpc.receive(ctx)
		if err != nil {
			return nil, err
		}
		var m officialResponseEvent
		if json.Unmarshal(raw, &m) != nil {
			return nil, errors.New("invalid compaction event")
		}
		if m.Params.Thread != "" && m.Params.Thread != t.id {
			continue
		}
		if len(m.ID) > 0 {
			return nil, errors.New("unexpected server request while compacting")
		}
		if m.Method == "error" && !m.Params.WillRetry && (turn == "" || m.Params.Turn == "" || m.Params.Turn == turn) {
			return nil, officialResponseFailure("official compaction failed", m.Params.Error)
		}
		if m.Method == "turn/started" {
			turn = m.Params.TurnValue.ID
			continue
		}
		if turn == "" || m.Params.Turn != "" && m.Params.Turn != turn {
			continue
		}
		if m.Method == "rawResponse/completed" {
			rawResponses++
			responseID = m.Params.ResponseID
			if len(m.Params.Usage) == 0 {
				usageComplete = false
			} else if err := officialAccumulateUsage(usageTotals, m.Params.Usage); err != nil {
				return nil, err
			}
			if usageComplete {
				details["usage"] = officialResponseUsage(usageTotals)
			} else {
				delete(details, "usage")
			}
			if rawResponses == 1 && len(m.Params.UsageMetadata) > 0 && string(m.Params.UsageMetadata) != "null" {
				details["usage_metadata"] = m.Params.UsageMetadata
			} else {
				delete(details, "usage_metadata")
			}
		}
		if m.Method == "error" && !m.Params.WillRetry {
			return nil, officialResponseFailure("official compaction failed", m.Params.Error)
		}
		if m.Method != "turn/completed" || m.Params.TurnValue.ID != turn {
			continue
		}
		if m.Params.TurnValue.Status != "completed" {
			return nil, officialResponseFailure("official compaction turn failed", m.Params.TurnValue.Error)
		}
		if responseID == "" {
			return nil, errors.New("official compaction omitted response identity")
		}
		// The writer can finish flushing just after the completion notification.
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			output, found, err := officialReadCompaction(path, offset, responseID)
			if err != nil {
				return nil, err
			}
			if found {
				t.history = append([]json.RawMessage(nil), output...)
				t.turn = turn
				t.compactResponseDetails = details
				completed = true
				return output, nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, errors.New("official rollout omitted encrypted compaction checkpoint")
			case <-tick.C:
			}
		}
	}
}

func officialRolloutPath(home, path string) (string, error) {
	if home == "" || path == "" {
		return "", errors.New("official thread did not expose its persistent rollout path")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// systemd DynamicUser exposes /var/lib/name through /var/lib/private/name.
	// The official process may report the resolved path while CODEX_HOME keeps
	// the managed alias. Compare their real paths, not those two spellings.
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", errors.New("official runtime home is unavailable")
	}
	realPath, err := officialResolveRolloutPath(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realHome, realPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", errors.New("official rollout path is outside its runtime home")
	}
	return realPath, nil
}

func officialReadCompaction(path string, offset int64, responseID string) ([]json.RawMessage, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if offset < 0 || stat.Size() < offset {
		return nil, false, errors.New("official compaction rollout offset is invalid")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	var output []json.RawMessage
	found := false
	stopRead := errors.New("compaction checkpoint found")
	err = officialReadJSONLines(f, func(raw []byte) error {
		var line struct {
			Type    string `json:"type"`
			Payload struct {
				ResponseID string            `json:"compaction_response_id"`
				History    []json.RawMessage `json:"replacement_history"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &line) != nil {
			return nil
		} // A last line may still be flushing.
		if line.Type != "compacted" || line.Payload.ResponseID != responseID {
			return nil
		}
		count := 0
		for _, item := range line.Payload.History {
			var v struct {
				Type      string `json:"type"`
				Encrypted string `json:"encrypted_content"`
			}
			if json.Unmarshal(item, &v) != nil {
				return errors.New("invalid official compaction history item")
			}
			if v.Type == "compaction" {
				if v.Encrypted == "" {
					return errors.New("official compaction checkpoint is not encrypted")
				}
				count++
			}
		}
		if count == 0 {
			return errors.New("official compaction checkpoint contains no encrypted compaction item")
		}
		output, found = line.Payload.History, true
		return stopRead
	})
	if errors.Is(err, stopRead) {
		err = nil
	}
	return output, found, err
}

func officialToolAlias(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + namespace + "\x00" + name))
	// Official namespace identifiers allow at most 64 characters.
	return "gw_" + hex.EncodeToString(sum[:])[:60]
}
func (t *officialThread) originalTool(namespace, name string) (string, string, bool) {
	for identity := range t.custom {
		parts := strings.SplitN(identity, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		candidateNamespace := ""
		if parts[0] != "" {
			candidateNamespace = officialToolAlias("namespace", "", parts[0])
		}
		// The official ToolName protocol treats an omitted namespace and
		// "functions" as the same scope for a declared top-level tool. The
		// scoped alias must still match exactly; other namespaces cannot cross
		// into that declaration, nor can an original unaliased name execute.
		namespaceMatches := candidateNamespace == namespace || parts[0] == "" && namespace == "functions"
		if namespaceMatches && officialToolAlias("tool", parts[0], parts[1]) == name {
			return parts[0], parts[1], true
		}
	}
	return "", "", false
}

// Responses clients may omit the default message type and output-only status.
// Normalize those protocol-equivalent shapes without weakening content equality.
func officialHistoryComparable(raw json.RawMessage) json.RawMessage {
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		return raw
	}
	var kind, role, text string
	_ = json.Unmarshal(f["type"], &kind)
	_ = json.Unmarshal(f["role"], &role)
	if kind == "" && role != "" {
		kind = "message"
		f["type"] = json.RawMessage(`"message"`)
	}
	if kind == "message" || kind == "function_call" || kind == "custom_tool_call" {
		delete(f, "status")
	}
	if kind == "message" && json.Unmarshal(f["content"], &text) == nil {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		f["content"], _ = json.Marshal([]any{map[string]string{"type": partType, "text": text}})
	}
	v, err := json.Marshal(f)
	if err != nil {
		return raw
	}
	return v
}

// officialPrepareInput separates native request-control items from conversation
// history. Modern Codex places tools in input.additional_tools, not top-level tools.
func officialPrepareInput(input *officialResponseInput) ([]json.RawMessage, error) {
	items, err := officialInputItems(input.Input)
	if err != nil {
		return nil, err
	}
	definitions := append([]json.RawMessage(nil), input.Tools...)
	history := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		var f struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(raw, &f) != nil {
			return nil, errors.New("invalid Responses history item")
		}
		switch f.Type {
		case "additional_tools":
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if len(fields["tools"]) == 0 || string(fields["tools"]) == "null" {
				return nil, errors.New("additional_tools must include a tools array")
			}
			definitions = append(definitions, f.Tools...)
		case "compaction_trigger":
			if len(input.Compact) == 0 || string(input.Compact) == "null" {
				input.Compact = append(json.RawMessage(nil), raw...)
			}
		case "tool_search_output":
			definitions = append(definitions, f.Tools...)
			history = append(history, raw)
		default:
			history = append(history, raw)
		}
	}
	// Canonicalize explicit and enclosing namespaces to one grouped catalog,
	// deduplicating identical declarations while rejecting conflicting schemas.
	declarations := map[string]json.RawMessage{}
	ordered := []string{}
	descriptions := map[string]string{}
	var collect func(json.RawMessage, string) error
	collect = func(raw json.RawMessage, namespace string) error {
		var f map[string]json.RawMessage
		if json.Unmarshal(raw, &f) != nil {
			return errors.New("invalid tool definition")
		}
		var kind, name, explicit string
		_ = json.Unmarshal(f["type"], &kind)
		_ = json.Unmarshal(f["name"], &name)
		if kind == "namespace" {
			if namespace != "" || name == "" {
				return errors.New("invalid nested or unnamed tool namespace")
			}
			var children []json.RawMessage
			if json.Unmarshal(f["tools"], &children) != nil || len(children) == 0 {
				return errors.New("tool namespace must contain tools")
			}
			var description string
			_ = json.Unmarshal(f["description"], &description)
			if previous, ok := descriptions[name]; ok && previous != "" && description != "" && previous != description {
				return errors.New("conflicting tool namespace descriptions")
			}
			if description != "" {
				descriptions[name] = description
			}
			for _, child := range children {
				if err := collect(child, name); err != nil {
					return err
				}
			}
			return nil
		}
		if value, present := f["namespace"]; present && string(value) != "null" {
			if json.Unmarshal(value, &explicit) != nil || explicit == "" {
				return errors.New("invalid tool namespace")
			}
			if namespace != "" && namespace != explicit {
				return errors.New("conflicting tool namespace")
			}
			namespace = explicit
		}
		delete(f, "namespace")
		key := namespace + "\x00" + name
		if kind == "function" || kind == "custom" {
			if name == "" {
				return errors.New("tool name is required")
			}
		} else {
			key = "builtin\x00" + kind
			if namespace != "" {
				return errors.New("built-in tools cannot be namespaced")
			}
		}
		canonical, _ := json.Marshal(f)
		if previous, exists := declarations[key]; exists {
			if !officialJSONEqual(previous, canonical) {
				// Discovery activates a previously deferred declaration without
				// changing its callable schema. Full history contains both forms;
				// retain the active form, whichever declaration appeared first.
				merged, compatible := officialMergeDiscoveredDeclaration(previous, canonical, kind)
				if !compatible {
					return errors.New("conflicting tool declaration")
				}
				declarations[key] = merged
			}
			return nil
		}
		declarations[key] = canonical
		ordered = append(ordered, key)
		return nil
	}
	for _, raw := range definitions {
		if err := collect(raw, ""); err != nil {
			return nil, err
		}
	}
	out := []json.RawMessage{}
	namespaces := map[string]int{}
	groups := map[string][]json.RawMessage{}
	for _, key := range ordered {
		parts := strings.SplitN(key, "\x00", 2)
		if parts[0] == "" || parts[0] == "builtin" {
			out = append(out, declarations[key])
			continue
		}
		namespace := parts[0]
		if _, exists := namespaces[namespace]; !exists {
			namespaces[namespace] = len(out)
			out = append(out, nil)
		}
		groups[namespace] = append(groups[namespace], declarations[key])
	}
	for namespace, index := range namespaces {
		out[index], _ = json.Marshal(map[string]any{"type": "namespace", "name": namespace, "description": descriptions[namespace], "tools": groups[namespace]})
	}
	input.Tools = out
	if _, _, err := officialTools(input.Tools); err != nil {
		return nil, err
	}
	return history, nil
}

const officialSearchToolName = "__gateway_client_tool_search"

func officialMergeDiscoveredDeclaration(previous, next json.RawMessage, kind string) (json.RawMessage, bool) {
	if kind != "function" && kind != "custom" {
		return nil, false
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal(previous, &before) != nil || json.Unmarshal(next, &after) != nil {
		return nil, false
	}
	deferred := func(fields map[string]json.RawMessage) (bool, bool) {
		value := false
		if raw, exists := fields["defer_loading"]; exists && json.Unmarshal(raw, &value) != nil {
			return false, false
		}
		delete(fields, "defer_loading")
		return value, true
	}
	wasDeferred, validBefore := deferred(before)
	isDeferred, validAfter := deferred(after)
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	if !validBefore || !validAfter || !officialJSONEqual(left, right) {
		return nil, false
	}
	if wasDeferred && isDeferred {
		return previous, true
	}
	// Omitted and explicit false both mean directly available. Omitting the
	// flag also gives stable history identity across either equivalent spelling.
	return right, true
}

func officialToolSearchExecution(tools []json.RawMessage) string {
	for _, raw := range tools {
		var f struct{ Type, Execution string }
		if json.Unmarshal(raw, &f) == nil && f.Type == "tool_search" {
			if f.Execution == "" {
				return "server"
			}
			return f.Execution
		}
	}
	return ""
}
func officialExpectedToolOutput(kind string) string {
	if kind == "tool_search_call" {
		return "tool_search_output"
	}
	return kind + "_output"
}

// Only rewrite declared identities; never invent discoveries or tool results.
func (t *officialThread) rewriteSearchDefinitions(item map[string]json.RawMessage, toRuntime bool) error {
	var defs []map[string]json.RawMessage
	if json.Unmarshal(item["tools"], &defs) != nil {
		return errors.New("tool_search_output requires tool definitions")
	}
	var rewrite func(map[string]json.RawMessage, string) error
	rewrite = func(f map[string]json.RawMessage, parent string) error {
		var kind, name, namespace string
		_ = json.Unmarshal(f["type"], &kind)
		_ = json.Unmarshal(f["name"], &name)
		_ = json.Unmarshal(f["namespace"], &namespace)
		if kind == "namespace" {
			original := name
			if !toRuntime {
				found := false
				for identity := range t.custom {
					parts := strings.SplitN(identity, "\x00", 2)
					if len(parts) == 2 && parts[0] != "" && officialToolAlias("namespace", "", parts[0]) == name {
						original = parts[0]
						found = true
						break
					}
				}
				if !found {
					return errors.New("runtime search returned an undeclared namespace")
				}
			}
			var children []map[string]json.RawMessage
			if json.Unmarshal(f["tools"], &children) != nil {
				return errors.New("invalid discovered namespace")
			}
			for _, child := range children {
				if err := rewrite(child, original); err != nil {
					return err
				}
			}
			finalName := original
			if toRuntime {
				finalName = officialToolAlias("namespace", "", original)
			}
			f["name"], _ = json.Marshal(finalName)
			f["tools"], _ = json.Marshal(children)
			return nil
		}
		if parent != "" {
			namespace = parent
		}
		if toRuntime {
			if _, ok := t.custom[namespace+"\x00"+name]; !ok {
				return errors.New("undeclared discovered tool")
			}
			f["name"], _ = json.Marshal(officialToolAlias("tool", namespace, name))
			if parent == "" && namespace != "" {
				f["namespace"], _ = json.Marshal(officialToolAlias("namespace", "", namespace))
			}
		} else {
			runtimeNamespace := namespace
			if parent != "" {
				runtimeNamespace = officialToolAlias("namespace", "", parent)
			}
			originalNamespace, originalName, ok := t.originalTool(runtimeNamespace, name)
			if !ok {
				return errors.New("runtime search returned an undeclared tool")
			}
			f["name"], _ = json.Marshal(originalName)
			if parent == "" && originalNamespace != "" {
				f["namespace"], _ = json.Marshal(originalNamespace)
			}
			if original := t.definitions[originalNamespace+"\x00"+originalName]; len(original) > 0 {
				var definition map[string]json.RawMessage
				_ = json.Unmarshal(original, &definition)
				for key := range f {
					delete(f, key)
				}
				for key, value := range definition {
					f[key] = value
				}
				delete(f, "defer_loading")
				if parent == "" && originalNamespace != "" {
					f["namespace"], _ = json.Marshal(originalNamespace)
				}
			} else if t.custom[originalNamespace+"\x00"+originalName] {
				return errors.New("original custom tool definition is unavailable")
			}
		}
		return nil
	}
	for _, f := range defs {
		if err := rewrite(f, ""); err != nil {
			return err
		}
	}
	item["tools"], _ = json.Marshal(defs)
	return nil
}

func officialOriginalToolDefinitions(tools []json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var visit func(json.RawMessage, string)
	visit = func(raw json.RawMessage, namespace string) {
		var f struct {
			Type, Name, Namespace string
			Tools                 []json.RawMessage
		}
		if json.Unmarshal(raw, &f) != nil {
			return
		}
		if f.Type == "namespace" {
			for _, child := range f.Tools {
				visit(child, f.Name)
			}
			return
		}
		if f.Namespace != "" {
			namespace = f.Namespace
		}
		if f.Type == "function" || f.Type == "custom" {
			out[namespace+"\x00"+f.Name] = raw
		}
	}
	for _, raw := range tools {
		visit(raw, "")
	}
	return out
}
func officialAccumulateUsage(totals, next map[string]json.RawMessage) error {
	for _, key := range []string{"inputTokens", "outputTokens", "totalTokens", "cachedInputTokens", "cacheWriteInputTokens", "reasoningOutputTokens"} {
		raw := next[key]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		value, ok := new(big.Int).SetString(string(raw), 10)
		if !ok || value.Sign() < 0 {
			return errors.New("invalid official usage counter")
		}
		if previous := totals[key]; len(previous) > 0 {
			prior, ok := new(big.Int).SetString(string(previous), 10)
			if !ok {
				return errors.New("invalid accumulated usage counter")
			}
			value.Add(value, prior)
		}
		totals[key] = json.RawMessage(value.String())
	}
	return nil
}

// officialClientToolCatalog changes only tool transport style in the genuine
// runtime catalog. Model identity, prompts, context limits and reasoning choices
// remain the exact JSON values fetched by the official executable.
func officialClientToolCatalog(cache []byte, requested string) ([]byte, error) {
	var container map[string]json.RawMessage
	if json.Unmarshal(cache, &container) != nil {
		return nil, errors.New("official model cache is invalid")
	}
	var models []map[string]json.RawMessage
	if json.Unmarshal(container["models"], &models) != nil || len(models) == 0 {
		return nil, errors.New("official model cache is empty")
	}
	found := false
	for _, model := range models {
		var slug string
		if json.Unmarshal(model["slug"], &slug) != nil || slug == "" {
			return nil, errors.New("official model cache omitted model identity")
		}
		if slug == requested {
			found = true
		}
		if err := officialClientToolModelPolicy(model); err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, errOfficialModelUnavailable
	}
	return json.Marshal(map[string]any{"models": models})
}

func (rt *officialRuntime) startClientToolRPC(ctx context.Context, account Account, proxy *ProxyEntry, model string) (*officialRPC, error) {
	cache, catalog, err := rt.clientToolModelCatalog(ctx, account, proxy, model)
	if err != nil {
		return nil, err
	}
	return rt.startRPCConfigured(ctx, account, proxy, func(home string) error {
		return officialWriteClientToolCatalog(home, cache, catalog)
	})
}
func officialWriteClientToolCatalog(home string, cache, catalog []byte) error {
	path := filepath.Join(home, "client_tools_model_catalog.json")
	if err := os.WriteFile(path, catalog, 0600); err != nil {
		return err
	}
	configuration := filepath.Join(home, "config.toml")
	previous, err := os.ReadFile(configuration)
	if err != nil {
		return err
	}
	encodedPath, _ := json.Marshal(filepath.ToSlash(path))
	configurationData := append([]byte("model_catalog_json = "+string(encodedPath)+"\n"), previous...)
	if err = os.WriteFile(configuration, configurationData, 0600); err != nil {
		return err
	}
	before, after := sha256.Sum256(cache), sha256.Sum256(catalog)
	audit, _ := json.Marshal(map[string]any{"source": "official models_cache.json", "cache_sha256": hex.EncodeToString(before[:]), "catalog_sha256": hex.EncodeToString(after[:]), "changed_model_fields": officialClientToolModelFields, "tool_mode": "direct", "purpose": "Responses client tool execution; runtime-owned tools disabled"})
	return os.WriteFile(filepath.Join(home, "client_tools_model_catalog.audit.json"), audit, 0600)
}
