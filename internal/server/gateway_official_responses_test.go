package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type officialResponsesFixtureWriter struct{ write func([]byte) (int, error) }

func (w officialResponsesFixtureWriter) Write(p []byte) (int, error) { return w.write(p) }
func (officialResponsesFixtureWriter) Close() error                  { return nil }
func officialResponsesFixture(t *testing.T, onWrite func(map[string]json.RawMessage)) *officialRPC {
	t.Helper()
	rpc := &officialRPC{waiters: map[string]chan json.RawMessage{}, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: func() {}}
	rpc.stdin = officialResponsesFixtureWriter{write: func(raw []byte) (int, error) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return 0, err
		}
		onWrite(m)
		return len(raw), nil
	}}
	t.Cleanup(func() { rpc.close() })
	return rpc
}
func officialResponsesReply(rpc *officialRPC, m map[string]json.RawMessage, result any) {
	raw, _ := json.Marshal(map[string]any{"id": m["id"], "result": result})
	rpc.mu.Lock()
	ch := rpc.waiters[string(m["id"])]
	rpc.mu.Unlock()
	if ch != nil {
		ch <- raw
	}
}
func officialResponsesQueue(rpc *officialRPC, events ...string) {
	rpc.mu.Lock()
	for _, event := range events {
		rpc.queue = append(rpc.queue, json.RawMessage(event))
		rpc.queued += len(event)
	}
	rpc.mu.Unlock()
	select {
	case rpc.wake <- struct{}{}:
	default:
	}
}
func officialResponsesRun(t *testing.T, thread *officialThread, delta []json.RawMessage) (json.RawMessage, []json.RawMessage, []map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events := []map[string]any{}
	thread.mu.Lock()
	result, output, err := thread.runResponse(ctx, officialResponseInput{Model: "gpt-6-astra"}, delta, func(event map[string]any) error {
		raw, _ := json.Marshal(event)
		var clone map[string]any
		_ = json.Unmarshal(raw, &clone)
		events = append(events, clone)
		return nil
	})
	return result, output, events, err
}
func TestOfficialResponsesRawFilteringStreamingAndUsage(t *testing.T) {
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(m["method"], &method)
		if method != "turn/start" {
			t.Fatalf("unexpected method %s", method)
		}
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn-1"}})
		officialResponsesQueue(rpc,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"old","item":{"type":"message","role":"assistant","id":"stale","content":[{"type":"output_text","text":"stale"}]}}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn-1","item":{"type":"message","role":"user","id":"u","content":[{"type":"input_text","text":"secret user"}]}}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn-1","item":{"type":"message","role":"developer","content":[{"type":"input_text","text":"harness context"}]}}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"old","output":"secret result"}}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn-1","item":{"type":"reasoning","id":"r","summary":[],"encrypted_content":"opaque-original"}}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn-1","itemId":"msg","delta":"Hel"}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn-1","itemId":"msg","delta":"lo"}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn-1","item":{"type":"message","id":"msg","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Hello","annotations":[]}]}}}`,
			`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn-1","responseId":"upstream","usage":{"inputTokens":9007199254740993,"outputTokens":2,"totalTokens":9007199254740995,"cachedInputTokens":1,"reasoningOutputTokens":1}}}`,
			`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn-1","status":"completed"}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
	result, output, events, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || !strings.Contains(string(output[0]), `"opaque-original"`) {
		t.Fatalf("bad output: %s", result)
	}
	if !strings.Contains(string(result), `"input_tokens":9007199254740993`) {
		t.Fatalf("usage precision lost: %s", result)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "harness context") || strings.Contains(string(raw), "stale") {
		t.Fatalf("input/old-turn leak: %s", raw)
	}
	if !strings.Contains(string(raw), "response.content_part.done") {
		t.Fatal("missing part completion")
	}
	if len(thread.history) != 3 || len(rpc.queue) != 0 || thread.awaiting {
		t.Fatal("history or terminal drain invalid")
	}
}

func TestOfficialResponsesNamespaceCustomPauseResume(t *testing.T) {
	tools, custom, err := officialTools([]json.RawMessage{json.RawMessage(`{"type":"namespace","name":"functions","description":"client tools","tools":[{"type":"custom","name":"apply_patch","description":"patch"},{"type":"function","name":"mcp__read","parameters":{"type":"object","properties":{}}}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	toolsJSON, _ := json.Marshal(tools)
	if strings.Contains(string(toolsJSON), `"name":"functions"`) || strings.Contains(string(toolsJSON), `"name":"mcp__read"`) {
		t.Fatalf("reserved runtime identifiers not aliased: %s", toolsJSON)
	}
	ns := officialToolAlias("namespace", "", "functions")
	patch := officialToolAlias("tool", "functions", "apply_patch")
	read := officialToolAlias("tool", "functions", "mcp__read")
	rawEvent := func(item any) string {
		v, _ := json.Marshal(map[string]any{"method": "rawResponseItem/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": item}})
		return string(v)
	}
	request := func(id, call, name string) string {
		v, _ := json.Marshal(map[string]any{"id": id, "method": "item/tool/call", "params": map[string]any{"threadId": "thread", "turnId": "turn", "callId": call, "namespace": ns, "tool": name, "arguments": map[string]string{"input": "patch\nline"}}})
		return string(v)
	}
	var rpc *officialRPC
	returned := map[string]json.RawMessage{}
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method, id string
		_ = json.Unmarshal(m["method"], &method)
		_ = json.Unmarshal(m["id"], &id)
		if method == "turn/start" {
			officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
			officialResponsesQueue(rpc,
				request("rpc-p", "call-p", patch),
				rawEvent(map[string]any{"type": "function_call", "id": "p", "call_id": "call-p", "namespace": ns, "name": patch, "arguments": "{\"input\":\"patch\\nline\"}"}),
				rawEvent(map[string]any{"type": "function_call", "id": "r", "call_id": "call-r", "namespace": ns, "name": read, "arguments": "{}"}),
				`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"up-1","usage":{"inputTokens":2,"outputTokens":2,"totalTokens":4}}}`,
				request("rpc-r", "call-r", read))
			return
		}
		returned[id] = m["result"]
		if len(returned) == 2 {
			officialResponsesQueue(rpc, rawEvent(map[string]any{"type": "function_call_output", "call_id": "call-r", "output": "tool-private"}), rawEvent(map[string]any{"type": "message", "id": "final", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "done"}}}), `{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"up-2","usage":{"inputTokens":4,"outputTokens":1,"totalTokens":5}}}`, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
		}
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: custom}
	_, output, _, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"use tools"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || !thread.awaiting || !strings.Contains(string(output[0]), `"type":"custom_tool_call"`) || !strings.Contains(string(output[0]), `"namespace":"functions"`) || !strings.Contains(string(output[1]), `"name":"mcp__read"`) {
		t.Fatalf("call not restored: %s", output)
	}
	injected, err := thread.injectItems(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(injected[0]), patch) || !strings.Contains(string(injected[0]), `"type":"function_call"`) || strings.Contains(string(injected[0]), `"type":"custom_tool_call"`) {
		t.Fatalf("replay did not restore wrapper: %s", injected[0])
	}
	_, output, events, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call-p","output":"patched"}`), json.RawMessage(`{"type":"function_call_output","call_id":"call-r","output":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"data:image/png;base64,x"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 || thread.awaiting || len(returned) != 2 {
		t.Fatalf("resume failed: output=%s returned=%v", output, returned)
	}
	if !strings.Contains(string(returned["rpc-p"]), `"text":"patched"`) || !strings.Contains(string(returned["rpc-r"]), `"imageUrl":"data:image/png;base64,x"`) {
		t.Fatalf("tool result altered: %v", returned)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "tool-private") {
		t.Fatal("tool output leaked into response")
	}
}
func TestOfficialResponsesToolOutputValidationBeforeSend(t *testing.T) {
	for name, delta := range map[string][]json.RawMessage{
		"unknown":    {json.RawMessage(`{"type":"function_call_output","call_id":"other","output":"x"}`)},
		"missing":    {},
		"duplicate":  {json.RawMessage(`{"type":"function_call_output","call_id":"call","output":"x"}`), json.RawMessage(`{"type":"function_call_output","call_id":"call","output":"x"}`)},
		"wrong type": {json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call","output":"x"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			writes := 0
			rpc := officialResponsesFixture(t, func(map[string]json.RawMessage) { writes++ })
			thread := &officialThread{rpc: rpc, id: "thread", turn: "turn", awaiting: true, expected: map[string]string{"call": "function_call"}, pending: map[string]json.RawMessage{"call": json.RawMessage(`1`)}}
			_, _, _, err := officialResponsesRun(t, thread, delta)
			if err == nil || writes != 0 {
				t.Fatalf("invalid output sent: writes=%d err=%v", writes, err)
			}
		})
	}
}
func TestOfficialResponsesPrefixPreservesContent(t *testing.T) {
	history := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"same"}]}`), json.RawMessage(`{"type":"function_call","call_id":"call","name":"read","arguments":"{\"id\":9007199254740993}"}`)}
	input := []json.RawMessage{json.RawMessage(`{"role":"user","content":"same"}`), json.RawMessage(`{"status":"completed","type":"function_call","call_id":"call","name":"read","arguments":"{\"id\":9007199254740993}"}`), json.RawMessage(`{"role":"user","content":"next"}`)}
	delta, ok := officialInputDelta(history, input)
	if !ok || len(delta) != 1 {
		t.Fatal("equivalent prefix lost")
	}
	input[1] = json.RawMessage(`{"type":"function_call","call_id":"call","name":"read","arguments":"{\"id\":9007199254740992}"}`)
	if _, ok = officialInputDelta(history, input); ok {
		t.Fatal("different history accepted")
	}
}
func TestOfficialResponsesCompactionUsesNewRolloutCheckpoint(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "rollout.jsonl")
	old := `{"type":"compacted","payload":{"compaction_response_id":"old","replacement_history":[{"type":"compaction","encrypted_content":"old-cipher"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(m["method"], &method)
		officialResponsesReply(rpc, m, map[string]any{})
		if method == "thread/inject_items" {
			return
		}
		if method != "thread/compact/start" {
			t.Fatalf("unexpected method %s", method)
		}
		fresh := `{"type":"compacted","payload":{"compaction_response_id":"new","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"retained"}]},{"type":"compaction","id":"cmp","encrypted_content":"opaque-new"}]}}` + "\n"
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.WriteString(f, fresh)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		officialResponsesQueue(rpc, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"stale","status":"completed"}}}`, `{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"compact","status":"inProgress"}}}`, `{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"compact","responseId":"new"}}`, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"compact","status":"completed"}}}`)
	})
	rpc.home = home
	thread := &officialThread{rpc: rpc, id: "thread", rollout: path, pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	thread.mu.Lock()
	output, err := thread.compactResponse(ctx, []json.RawMessage{json.RawMessage(`{"role":"user","content":"retained"}`)})
	thread.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || !strings.Contains(string(output[1]), "opaque-new") || len(thread.history) != 2 {
		t.Fatalf("wrong checkpoint: %s", output)
	}
}
func TestOfficialResponsesCompactionRejectsPlaintextAndWrongCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"compacted","payload":{"compaction_response_id":"new","replacement_history":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"plain summary"}]}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := officialReadCompaction(path, 0, "other"); err != nil || found {
		t.Fatalf("wrong response accepted: %v %v", found, err)
	}
	if _, _, err := officialReadCompaction(path, 0, "new"); err == nil {
		t.Fatal("plaintext compaction represented as encrypted")
	}
	if _, err := officialRolloutPath(filepath.Dir(path), filepath.Join(filepath.Dir(path), "..", "escape.jsonl")); err == nil {
		t.Fatal("rollout escape accepted")
	}
}

func TestOfficialResponsesRejectsContradictoryTextAndFailedTurn(t *testing.T) {
	for name, tail := range map[string][]string{
		"delta differs from raw": {
			`{"method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"msg","delta":"delivered"}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"different"}]}}}`,
		},
		"turn fails after upstream response": {
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}}`,
			`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"upstream"}}`,
			`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"failed","error":{"message":"fixture"}}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var rpc *officialRPC
			rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
				officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
				officialResponsesQueue(rpc, tail...)
			})
			thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
			_, _, _, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"test"}`)})
			if err == nil || rpc.alive() || len(thread.history) != 0 {
				t.Fatalf("failure committed: %v", err)
			}
		})
	}
}
func TestOfficialResponsesRejectsToolRequestIdentitySubstitution(t *testing.T) {
	thread := &officialThread{id: "thread", turn: "turn", custom: map[string]bool{"\x00read": false, "\x00write": false}, expected: map[string]string{"call": "function_call"}, expectedTools: map[string]string{"call": "\x00read"}, pending: map[string]json.RawMessage{}}
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": "item/tool/call", "params": map[string]any{"threadId": "thread", "turnId": "turn", "callId": "call", "tool": officialToolAlias("tool", "", "write")}})
	if err := thread.capturePending(raw); err == nil || len(thread.pending) != 0 {
		t.Fatal("raw call was rebound to another declared tool")
	}
}
func TestOfficialResponsesToolReplayAfterProcessEviction(t *testing.T) {
	var rpc *officialRPC
	var injected, turnInput json.RawMessage
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(m["method"], &method)
		var p map[string]json.RawMessage
		_ = json.Unmarshal(m["params"], &p)
		if method == "thread/inject_items" {
			injected = p["items"]
			officialResponsesReply(rpc, m, map[string]any{})
			return
		}
		if method != "turn/start" {
			t.Fatalf("unexpected method %s", method)
		}
		turnInput = p["input"]
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "new-turn"}})
		officialResponsesQueue(rpc, `{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"new-turn","item":{"type":"message","role":"assistant","id":"msg","content":[{"type":"output_text","text":"resumed"}]}}}`, `{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"new-turn","responseId":"upstream"}}`, `{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"new-turn","status":"completed"}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{"\x00patch": true}}
	delta := []json.RawMessage{json.RawMessage(`{"role":"user","content":"patch it"}`), json.RawMessage(`{"type":"custom_tool_call","call_id":"old","name":"patch","input":"exact\npatch"}`), json.RawMessage(`{"type":"custom_tool_call_output","call_id":"old","output":"applied"}`)}
	_, output, _, err := officialResponsesRun(t, thread, delta)
	if err != nil {
		t.Fatal(err)
	}
	if string(turnInput) != "[]" || len(output) != 1 || !strings.Contains(string(injected), `"type":"function_call_output"`) || strings.Contains(string(injected), `"type":"custom_tool_call"`) {
		t.Fatalf("invalid replay: input=%s injected=%s", turnInput, injected)
	}
}

func TestOfficialResponsesPrepareNativeToolsAndCompaction(t *testing.T) {
	input := officialResponseInput{Tools: []json.RawMessage{json.RawMessage(`{"type":"function","namespace":"functions","name":"read","parameters":{"type":"object","properties":{}}}`)}, Input: json.RawMessage(`[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","description":"local","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"patch","format":{"type":"text"}}]},{"type":"tool_search","execution":"client"}]},{"role":"user","content":"work"},{"type":"compaction_trigger"}]`)}
	history, err := officialPrepareInput(&input)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(input.Compact) == 0 || len(input.Tools) != 2 {
		t.Fatalf("controls/tools not extracted: history=%s tools=%s compact=%s", history, input.Tools, input.Compact)
	}
	defs := officialOriginalToolDefinitions(input.Tools)
	if len(defs) != 2 || defs["functions\x00read"] == nil || defs["functions\x00patch"] == nil {
		t.Fatalf("namespace merge failed: %v", defs)
	}
	for name, raw := range map[string]string{
		"schema conflict":     `[{"type":"additional_tools","tools":[{"type":"function","namespace":"functions","name":"read","parameters":{"type":"object","properties":{"other":{"type":"string"}}}}]}]`,
		"namespace conflict":  `[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","namespace":"other","name":"read"}]}]}]`,
		"unsupported builtin": `[{"type":"additional_tools","tools":[{"type":"computer_use_preview"}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			candidate := officialResponseInput{Tools: []json.RawMessage{json.RawMessage(`{"type":"function","namespace":"functions","name":"read","parameters":{"type":"object","properties":{}}}`)}, Input: json.RawMessage(raw)}
			if _, err := officialPrepareInput(&candidate); err == nil {
				t.Fatal("conflicting/unsupported native catalog accepted")
			}
		})
	}
}
func TestOfficialResponsesClientToolSearchWrapAndDiscoveryReplay(t *testing.T) {
	input := officialResponseInput{Input: json.RawMessage(`[{"type":"additional_tools","tools":[{"type":"tool_search","execution":"client"}]},{"role":"user","content":"find database tools"}]`)}
	history, err := officialPrepareInput(&input)
	if err != nil {
		t.Fatal(err)
	}
	_, custom, err := officialTools(input.Tools)
	if err != nil {
		t.Fatal(err)
	}
	searchName := officialToolAlias("tool", "", officialSearchToolName)
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
		request, _ := json.Marshal(map[string]any{"id": "search-request", "method": "item/tool/call", "params": map[string]any{"threadId": "thread", "turnId": "turn", "callId": "search-call", "tool": searchName, "arguments": map[string]string{"query": "database"}}})
		item, _ := json.Marshal(map[string]any{"method": "rawResponseItem/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"type": "function_call", "id": "search-item", "call_id": "search-call", "name": searchName, "arguments": "{\"query\":\"database\"}"}}})
		officialResponsesQueue(rpc, string(request), string(item), `{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"upstream"}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: custom, searchExecution: "client"}
	_, output, _, err := officialResponsesRun(t, thread, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 || !thread.awaiting || !strings.Contains(string(output[0]), `"type":"tool_search_call"`) || !strings.Contains(string(output[0]), `"arguments":{"query":"database"}`) {
		t.Fatalf("search not forwarded natively: %s", output)
	}
	searchOutput := json.RawMessage(`{"type":"tool_search_output","call_id":"search-call","execution":"client","tools":[{"type":"function","namespace":"db","name":"query","parameters":{"type":"object","properties":{"sql":{"type":"string"}}}}]}`)
	all := append(append(append([]json.RawMessage{}, history...), output...), searchOutput)
	allJSON, _ := json.Marshal(all)
	input.Input = allJSON
	replay, err := officialPrepareInput(&input)
	if err != nil {
		t.Fatal(err)
	}
	_, updated, err := officialTools(input.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := updated["db\x00query"]; !exists {
		t.Fatal("discovered tool missing from runtime catalog")
	}
	next := &officialThread{custom: updated, searchExecution: "client"}
	injected, err := next.injectItems(replay)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(injected[1]), `"type":"function_call"`) || !strings.Contains(string(injected[2]), `"type":"function_call_output"`) || !strings.Contains(string(injected[2]), `db`) {
		t.Fatalf("native search replay invalid: %s", injected)
	}
}
func TestOfficialResponsesServerDiscoveryRestoresExactCustomDefinition(t *testing.T) {
	input := officialResponseInput{Tools: []json.RawMessage{json.RawMessage(`{"type":"tool_search","execution":"server"}`), json.RawMessage(`{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"patch","defer_loading":true,"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]}`)}}
	_, custom, err := officialTools(input.Tools)
	if err != nil {
		t.Fatal(err)
	}
	thread := &officialThread{custom: custom, searchExecution: "server", definitions: officialOriginalToolDefinitions(input.Tools)}
	raw, _ := json.Marshal(map[string]any{"type": "tool_search_output", "call_id": "search", "execution": "client", "tools": []any{map[string]any{"type": "namespace", "name": officialToolAlias("namespace", "", "functions"), "tools": []any{map[string]any{"type": "function", "name": officialToolAlias("tool", "functions", "patch"), "parameters": map[string]any{"type": "object"}}}}}})
	restored, err := thread.restoreItem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), `"execution":"server"`) || !strings.Contains(string(restored), `"type":"custom"`) || !strings.Contains(string(restored), `"definition":"start: /.+/"`) || strings.Contains(string(restored), "defer_loading") {
		t.Fatalf("discovery altered original tool: %s", restored)
	}
	totals := map[string]json.RawMessage{}
	for i := 0; i < 2; i++ {
		if err := officialAccumulateUsage(totals, map[string]json.RawMessage{"inputTokens": json.RawMessage(`9007199254740993`), "outputTokens": json.RawMessage(`3`)}); err != nil {
			t.Fatal(err)
		}
	}
	if string(totals["inputTokens"]) != "18014398509481986" || string(totals["outputTokens"]) != "6" {
		t.Fatalf("multi-inference usage lost: %s", totals)
	}
}

func TestOfficialResponsesCatalogOverrideChangesOnlyToolCapabilities(t *testing.T) {
	raw := []byte(`{"etag":"official-v1","models":[{"slug":"gpt-6-astra","tool_mode":"code_mode_only","base_instructions":"exact official instructions\nwith code mode reference","context_window":9007199254740993,"supported_reasoning_levels":[{"effort":"xhigh","description":"full"}],"hidden_extension":{"raw":[1,2,3]}},{"slug":"gpt-5.5","base_instructions":"second","default_reasoning_level":"high"}]}`)
	catalog, err := officialClientToolCatalog(raw, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	var source, target struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	_ = json.Unmarshal(raw, &source)
	_ = json.Unmarshal(catalog, &target)
	if len(source.Models) != len(target.Models) {
		t.Fatal("model catalog truncated")
	}
	for i, model := range target.Models {
		if string(model["tool_mode"]) != `"direct"` {
			t.Fatal("tool transport override missing")
		}
		if string(model["apply_patch_tool_type"]) != "null" || string(model["multi_agent_version"]) != `"disabled"` {
			t.Fatal("model-driven runtime tool leaked into client-tool bridge")
		}
		for _, field := range officialClientToolModelFields {
			delete(model, field)
			delete(source.Models[i], field)
		}
		before, _ := json.Marshal(source.Models[i])
		after, _ := json.Marshal(model)
		if !officialJSONEqual(before, after) {
			t.Fatalf("model metadata altered: %s vs %s", before, after)
		}
	}
	home := t.TempDir()
	originalConfig := "model_provider = \"openai\"\n[history]\npersistence = \"none\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(originalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := officialWriteClientToolCatalog(home, raw, catalog); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(config), "model_catalog_json = ") || !strings.HasSuffix(string(config), originalConfig) {
		t.Fatalf("startup configuration altered: %s", config)
	}
	persisted, err := os.ReadFile(filepath.Join(home, "client_tools_model_catalog.json"))
	if err != nil || string(persisted) != string(catalog) {
		t.Fatal("catalog not persisted exactly")
	}
	audit, err := os.ReadFile(filepath.Join(home, "client_tools_model_catalog.audit.json"))
	if err != nil || !strings.Contains(string(audit), `"changed_model_fields":["tool_mode","apply_patch_tool_type","multi_agent_version","experimental_supported_tools"]`) || strings.Contains(string(audit), "exact official instructions") {
		t.Fatalf("invalid safe catalog audit: %s %v", audit, err)
	}
	if _, err := officialClientToolCatalog(raw, "absent"); err == nil {
		t.Fatal("unknown model silently accepted")
	}
	if _, err := officialClientToolCatalog([]byte(`{"models":[]}`), "gpt-6-astra"); err == nil {
		t.Fatal("empty model catalog accepted")
	}
}

func TestOfficialResponsesCredentialRefreshReusesThreadAndToolPause(t *testing.T) {
	account := Account{ID: "same-owner", AccountID: "account-1", AccessToken: "old-credential", CredentialVersion: 1, Identity: &managedAccountIdentity{UserID: "user-1"}}
	input := officialResponseInput{Model: "gpt-6-astra"}
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"remember"}`)}
	sigRaw, _ := json.Marshal([]any{input.Model, input.Instructions, input.Tools})
	sigHash := sha256.Sum256(sigRaw)
	requests := []map[string]json.RawMessage{}
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(m["method"], &method)
		if method != "account/login/start" {
			t.Fatalf("unexpected RPC %s", method)
		}
		var params map[string]json.RawMessage
		_ = json.Unmarshal(m["params"], &params)
		requests = append(requests, params)
		officialResponsesReply(rpc, m, map[string]any{"type": "chatgptAuthTokens"})
	})
	thread := &officialThread{rpc: rpc, id: "same-thread", signature: hex.EncodeToString(sigHash[:]), history: history, credentials: officialCredentialDigest(account), awaiting: true, expected: map[string]string{"call": "function_call"}, pending: map[string]json.RawMessage{"call": json.RawMessage(`42`)}}
	key := "key\x00" + gatewayAccountRef(account) + "\x00session"
	rt := &officialRuntime{threads: map[string]*officialThread{key: thread}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	check := func(wantCalls int) {
		t.Helper()
		returned, delta, err := rt.responseThread(ctx, "key", "session", account, nil, input, history)
		if err != nil {
			t.Fatal(err)
		}
		returned.mu.Unlock()
		if returned != thread || len(delta) != 0 || len(requests) != wantCalls || !returned.awaiting || string(returned.pending["call"]) != "42" || returned.credentials != officialCredentialDigest(account) {
			t.Fatal("credential update changed thread or pending state")
		}
	}
	check(0)
	account.AccessToken = "new-credential"
	check(1)
	if string(requests[0]["accessToken"]) != `"new-credential"` || string(requests[0]["chatgptAccountId"]) != `"account-1"` {
		t.Fatal("fresh credentials not forwarded")
	}
	check(1)
	account.CredentialVersion++
	check(2)
}
func TestOfficialResponsesCredentialRefreshFailureClosesAndEvicts(t *testing.T) {
	old := Account{ID: "same-owner", AccountID: "account", AccessToken: "old", CredentialVersion: 1, Identity: &managedAccountIdentity{UserID: "user-1"}}
	fresh := old
	fresh.AccessToken = "rejected"
	fresh.CredentialVersion = 2
	input := officialResponseInput{Model: "gpt-6-astra"}
	sigRaw, _ := json.Marshal([]any{input.Model, input.Instructions, input.Tools})
	sig := sha256.Sum256(sigRaw)
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		raw, _ := json.Marshal(map[string]any{"id": m["id"], "error": map[string]any{"code": -32000, "message": "fixture login rejection"}})
		rpc.mu.Lock()
		ch := rpc.waiters[string(m["id"])]
		rpc.mu.Unlock()
		ch <- raw
	})
	thread := &officialThread{rpc: rpc, id: "thread", signature: hex.EncodeToString(sig[:]), credentials: officialCredentialDigest(old)}
	key := "key\x00" + gatewayAccountRef(old) + "\x00session"
	rt := &officialRuntime{threads: map[string]*officialThread{key: thread}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	returned, _, err := rt.responseThread(ctx, "key", "session", fresh, nil, input, nil)
	if err == nil || returned != nil || rpc.alive() || rt.threads[key] != nil || thread.credentials != officialCredentialDigest(old) {
		t.Fatalf("failed refresh reused runtime: %v", err)
	}
	if !thread.mu.TryLock() {
		t.Fatal("failed refresh retained mutex")
	}
	thread.mu.Unlock()
}
