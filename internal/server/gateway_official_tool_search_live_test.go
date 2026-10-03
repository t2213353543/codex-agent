package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfficialLiveClientToolDiscovery(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit isolated live environment required")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	raw, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("live account fixture unavailable")
	}
	rt, err := newOfficialRuntime(func(k string) string {
		if k == "CODEX_CLI_PATH" {
			return binary
		}
		if k == "CODEX_RUNTIME_DIR" {
			return filepath.Join(work, "client-discovery")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	a := fixture.Accounts[0]
	account := Account{ID: "discovery-test", AccessToken: a.AccessToken, AccountID: a.AccountID, Enabled: true}
	input := officialResponseInput{Model: "gpt-6-astra", Instructions: "Use the provided client tools. First call __gateway_client_tool_search to discover a marker retrieval tool. Then call the discovered lookup_marker function exactly once. Reply only the marker returned by lookup_marker. Do not invent the marker or use other tools.", Tools: []json.RawMessage{json.RawMessage(`{"type":"tool_search","execution":"client","description":"Discover the lookup_marker tool for the client. Search by query."}`)}}
	input.Reasoning.Effort = "xhigh"
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"Find the marker tool, invoke it, and return the marker."}`)}
	marker := "DISCOVERY_" + strings.ReplaceAll(bpsUUID(), "-", "")
	session := "discovery-" + bpsUUID()
	for step := 0; step < 3; step++ {
		input.Input, _ = json.Marshal(history)
		items, err := officialPrepareInput(&input)
		if err != nil {
			t.Fatal(err)
		}
		thread, delta, err := rt.responseThread(ctx, "key", session, account, nil, input, items)
		if err != nil {
			t.Fatal(err)
		}
		result, output, err := thread.runResponse(ctx, input, delta, func(map[string]any) error { return nil })
		if err != nil {
			t.Fatalf("discovery step %d: %v", step, err)
		}
		history = append(history, output...)
		var response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(result, &response)
		if len(response.Usage) == 0 {
			t.Fatal("discovery response has no real usage")
		}
		t.Logf("discovery step=%d output_items=%d usage_known=true", step, len(output))
		call, kind, text := "", "", ""
		for _, raw := range output {
			var item struct {
				Type, Name, ID string
				CallID         string `json:"call_id"`
				Content        []struct{ Type, Text string }
			}
			_ = json.Unmarshal(raw, &item)
			if item.Type == "tool_search_call" || item.Type == "function_call" {
				call, kind = item.CallID, item.Type
			}
			if step == 0 && item.Type == "tool_search_call" && !strings.HasPrefix(item.ID, "tsc_") {
				t.Fatal("search output has a mismatched typed ID")
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
				}
			}
		}
		switch step {
		case 0:
			if kind != "tool_search_call" || call == "" {
				t.Fatal("runtime did not request client-owned discovery")
			}
			toolOutput, _ := json.Marshal(map[string]any{"type": "tool_search_output", "execution": "client", "call_id": call, "tools": []any{map[string]any{"type": "function", "name": "lookup_marker", "description": "Return the private marker. Must invoke this function to obtain it.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}})
			history = append(history, toolOutput)
		case 1:
			if kind != "function_call" || call == "" {
				t.Fatal("discovered function was not called")
			}
			toolOutput, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": call, "output": marker})
			history = append(history, toolOutput)
		case 2:
			if strings.TrimSpace(text) != marker {
				t.Fatal("discovery final marker differs from actual client tool result")
			}
		}
	}
}

func TestOfficialLiveImplicitToolDiscovery(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit isolated live environment required")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	raw, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("live account fixture unavailable")
	}
	rt, err := newOfficialRuntime(func(k string) string {
		if k == "CODEX_CLI_PATH" {
			return binary
		}
		if k == "CODEX_RUNTIME_DIR" {
			return filepath.Join(work, "implicit-discovery")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	a := fixture.Accounts[0]
	account := Account{ID: "implicit-discovery-test", AccessToken: a.AccessToken, AccountID: a.AccountID, Enabled: true}
	input := officialResponseInput{Model: "gpt-6-astra", Instructions: "Use the provided tools to discover and invoke lookup_marker exactly once. Reply only its returned marker. Never invent the marker. When asked to repeat the marker already returned, reply that same marker without calling any tools.", Tools: []json.RawMessage{json.RawMessage(`{"type":"namespace","name":"marker_ns","description":"Marker tools","tools":[{"type":"function","name":"lookup_marker","description":"Return the private marker. Invoke to obtain it.","defer_loading":true,"parameters":{"type":"object","properties":{},"additionalProperties":false}}]}`)}}
	input.Reasoning.Effort = "xhigh"
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"Discover lookup_marker with tool search, invoke it, and return its result."}`)}
	marker := "IMPLICIT_" + strings.ReplaceAll(bpsUUID(), "-", "")
	session := "implicit-" + bpsUUID()
	for step := 0; step < 3; step++ {
		input.Input, _ = json.Marshal(history)
		items, err := officialPrepareInput(&input)
		if err != nil {
			t.Fatalf("prepare implicit step %d: %v", step, err)
		}
		thread, delta, err := rt.responseThread(ctx, "key", session, account, nil, input, items)
		if err != nil {
			t.Fatal(err)
		}
		result, output, err := thread.runResponse(ctx, input, delta, func(map[string]any) error { return nil })
		if err != nil {
			t.Fatalf("implicit discovery step %d: %v", step, err)
		}
		history = append(history, output...)
		var response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(result, &response)
		if len(response.Usage) == 0 {
			t.Fatal("implicit discovery has no real usage")
		}
		var call, text string
		searchCall, searchOutput := false, false
		for _, raw := range output {
			var item struct {
				Type, Name, Namespace, Execution string
				CallID                           string `json:"call_id"`
				Content                          []struct{ Type, Text string }
			}
			_ = json.Unmarshal(raw, &item)
			if item.Type == "tool_search_call" {
				searchCall = item.Execution == "server"
			}
			if item.Type == "tool_search_output" {
				searchOutput = item.Execution == "server"
			}
			if item.Type == "function_call" {
				if item.Name != "lookup_marker" || item.Namespace != "marker_ns" {
					t.Fatal("discovery changed function identity")
				}
				call = item.CallID
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
				}
			}
		}
		t.Logf("implicit step=%d output_items=%d search_call=%v search_output=%v usage_known=true", step, len(output), searchCall, searchOutput)
		if step == 0 {
			if !searchCall || !searchOutput || call == "" {
				t.Fatal("implicit runtime discovery or tool invocation missing")
			}
			toolOutput, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": call, "output": marker})
			history = append(history, toolOutput)
		} else {
			if strings.TrimSpace(text) != marker || call != "" {
				t.Fatal("implicit discovered tool result did not survive continuation")
			}
			if step == 1 {
				history = append(history, json.RawMessage(`{"role":"user","content":"Repeat the marker already returned, with no tool calls."}`))
			}
		}
	}
}
