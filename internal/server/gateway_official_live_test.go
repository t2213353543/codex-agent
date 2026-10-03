package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs the real official executable and real account against an isolated Go
// gateway. No production settings, credentials or account rows are changed.
func TestOfficialLiveRuntime(t *testing.T) {
	credentialPath, binary, workRoot := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if credentialPath == "" || binary == "" || workRoot == "" {
		t.Skip("explicit live fixture required")
	}
	raw, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal("live credential fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid fixture")
	}
	var evidence []map[string]any
	account := fixture.Accounts[0]
	h := newBPSLiveHarness(t, binary, workRoot, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.RuntimeModes = true
	p.CodexTransport = &Proxy{Client: newHTTPClient(), Proxies: p.Proxies}
	if err := configureGatewayNativeHTTP(p.CodexTransport, os.Getenv); err != nil {
		t.Fatal("native transport fixture unavailable")
	}
	t.Cleanup(p.CodexTransport.closeGatewayClients)
	root := filepath.Join(workRoot, "official-runtime")
	p.Official, err = newOfficialRuntime(func(key string) string {
		if key == "CODEX_CLI_PATH" {
			return binary
		}
		if key == "CODEX_RUNTIME_DIR" {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Official.Close() })
	base := ""
	for _, line := range strings.Split(string(mustReadOfficialLive(t, filepath.Join(h.home, "config.toml"))), "\n") {
		if strings.HasPrefix(line, "base_url=") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "base_url=")), &base)
		}
	}
	if base == "" {
		t.Fatal("isolated server URL unavailable")
	}
	setMode := func(mode string) {
		s, e := p.Settings.Get()
		if e != nil {
			t.Fatal(e)
		}
		s.InferenceMode = mode
		if e = p.Settings.Set(s); e != nil {
			t.Fatal(e)
		}
	}
	call := func(path string, body any, session string) (int, []byte) {
		data, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		method := http.MethodPost
		if body == nil {
			method = http.MethodGet
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", session)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal("live request transport failed")
		}
		defer res.Body.Close()
		out, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if e != nil {
			t.Fatal("live response read failed")
		}
		if res.StatusCode >= 400 {
			t.Logf("status=%d body=%s", res.StatusCode, h.scrub(string(out)))
		}
		return res.StatusCode, out
	}
	response := func(out []byte) map[string]json.RawMessage {
		var obj map[string]json.RawMessage
		if json.Unmarshal(out, &obj) != nil {
			t.Fatal("invalid live JSON response")
		}
		return obj
	}
	answer := func(obj map[string]json.RawMessage) string {
		var items []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(obj["output"], &items)
		var text string
		for _, item := range items {
			if item.Type == "message" {
				for _, c := range item.Content {
					text += c.Text
				}
			}
		}
		return text
	}
	assertUsageRecord := func(t *testing.T, raw []byte) {
		t.Helper()
		var observed struct {
			ID, Model string
			Usage     struct {
				Input   int64 `json:"input_tokens"`
				Output  int64 `json:"output_tokens"`
				Details struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &observed) != nil || observed.Usage.Input <= 0 {
			t.Fatal("tool response omitted actual usage")
		}
		p.Metrics.mu.Lock()
		defer p.Metrics.mu.Unlock()
		for _, record := range p.Metrics.recent[:p.Metrics.recentAt] {
			if record.ResponseID != observed.ID {
				continue
			}
			if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != observed.Usage.Input || record.OutputTokens != observed.Usage.Output || record.CachedTokens != observed.Usage.Details.Cached || record.ReturnedModel != observed.Model || !strings.HasPrefix(record.UpstreamModelSource, "app_server_") {
				t.Fatalf("tool response usage/model does not match management record: input=%d output=%d cached=%d source=%s", record.InputTokens, record.OutputTokens, record.CachedTokens, record.UpstreamModelSource)
			}
			t.Logf("tool response accounting matched input=%d output=%d cached=%d model=%s", record.InputTokens, record.OutputTokens, record.CachedTokens, record.ReturnedModel)
			return
		}
		t.Fatal("tool response missing management record")
	}
	t.Run("official_cli_native_text", func(t *testing.T) {
		h.recorder.begin("official_cli_native_text")
		setMode(inferenceCodexCLI)
		code, out := call("/responses", map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "input": "Reply exactly OFFICIAL_CLI_OK without tools."}, "native-"+bpsUUID())
		if code != 200 || strings.TrimSpace(answer(response(out))) != "OFFICIAL_CLI_OK" {
			t.Fatal("official CLI native response failed")
		}
	})
	t.Run("app_server_text_resume_compact", func(t *testing.T) {
		h.recorder.begin("app_server_text_resume_compact")
		setMode(inferenceAppServer)
		session := "app-" + bpsUUID()
		marker := "MARKER_" + strings.ReplaceAll(bpsUUID(), "-", "")
		history := []any{map[string]any{"role": "user", "content": "Remember " + marker + ". Reply only SAVED. No tools."}}
		code, out := call("/responses", map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "input": history}, session)
		if code != 200 || strings.TrimSpace(answer(response(out))) != "SAVED" {
			t.Fatal("App Server text failed")
		}
		obj := response(out)
		var id string
		_ = json.Unmarshal(obj["id"], &id)
		code, out = call("/responses", map[string]any{"model": "gpt-6-astra", "stream": false, "previous_response_id": id, "input": []any{map[string]any{"role": "user", "content": "Reply only the exact remembered marker without tools."}}}, session)
		if code != 200 || strings.TrimSpace(answer(response(out))) != marker {
			t.Fatal("App Server incremental resume failed")
		}
		var outputs []json.RawMessage
		_ = json.Unmarshal(response(out)["output"], &outputs)
		full := append(history, map[string]any{"role": "assistant", "content": "SAVED"}, map[string]any{"role": "user", "content": "The marker is " + marker + ". Keep it."})
		code, out = call("/responses/compact", map[string]any{"model": "gpt-6-astra", "input": full}, session)
		if code != 200 {
			t.Fatal("App Server compact failed")
		}
		var compact []json.RawMessage
		_ = json.Unmarshal(response(out)["output"], &compact)
		encrypted := false
		for _, v := range compact {
			var item map[string]any
			_ = json.Unmarshal(v, &item)
			if item["type"] == "compaction" && item["encrypted_content"] != "" {
				encrypted = true
			}
		}
		if !encrypted {
			t.Fatal("missing real encrypted compaction")
		}
		input := []any{}
		for _, v := range compact {
			input = append(input, v)
		}
		input = append(input, map[string]any{"role": "user", "content": "Reply only the marker from prior messages. No tools."})
		code, out = call("/responses", map[string]any{"model": "gpt-6-astra", "stream": false, "input": input}, session)
		if code != 200 || strings.TrimSpace(answer(response(out))) != marker {
			t.Fatal("App Server compact continuation failed")
		}
	})
	t.Run("app_server_client_function", func(t *testing.T) {
		h.recorder.begin("app_server_client_function")
		setMode(inferenceAppServer)
		session := "tool-" + bpsUUID()
		tools := []any{map[string]any{"type": "function", "name": "lookup_marker", "description": "Return the required marker.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
		input := []any{map[string]any{"role": "user", "content": "Call lookup_marker once, then reply only its returned text."}}
		code, out := call("/responses", map[string]any{"model": "gpt-6-astra", "stream": false, "tools": tools, "input": input}, session)
		if code != 200 {
			t.Fatal("function first response failed")
		}
		assertUsageRecord(t, out)
		obj := response(out)
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(obj["output"], &items)
		callID := ""
		for _, item := range items {
			if string(item["type"]) == `"function_call"` {
				_ = json.Unmarshal(item["call_id"], &callID)
			}
		}
		if callID == "" {
			t.Logf("function response=%s", h.scrub(string(out)))
			t.Fatal("no native function call")
		}
		var id string
		_ = json.Unmarshal(obj["id"], &id)
		marker := "TOOL_" + bpsUUID()
		code, out = call("/responses", map[string]any{"model": "gpt-6-astra", "stream": false, "tools": tools, "previous_response_id": id, "input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": marker}}}, session)
		if code != 200 || strings.TrimSpace(answer(response(out))) != marker {
			t.Fatal("client tool return failed")
		}
		assertUsageRecord(t, out)
	})
	t.Run("app_server_history_retired_tools", func(t *testing.T) {
		h.recorder.begin("app_server_history_retired_tools")
		setMode(inferenceAppServer)
		marker := "HISTORY_" + bpsUUID()
		// Reproduce the production code-mode history with no corresponding tool
		// in the new turn's catalog. Native ResponseItem accepts this directly.
		history := []any{
			map[string]any{"role": "user", "content": "Get the marker using exec, then verify it using exec_command."},
			map[string]any{"type": "custom_tool_call", "call_id": "retired-exec", "namespace": "functions", "name": "exec", "input": "return tools.read_marker();"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "retired-exec", "output": marker},
			map[string]any{"type": "function_call", "call_id": "retired-command", "namespace": "functions", "name": "exec_command", "arguments": `{"cmd":"cat marker.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": "retired-command", "output": marker},
			map[string]any{"role": "assistant", "content": "I retrieved and verified the marker."},
			map[string]any{"role": "user", "content": "Reply only the exact marker returned by both old tools. Do not call any tools."},
		}
		code, out := call("/responses", map[string]any{"model": "gpt-5.6-sol", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "tools": []any{}, "input": history}, "history-"+bpsUUID())
		if code != 200 || strings.TrimSpace(answer(response(out))) != marker {
			t.Fatal("retired custom/function history replay failed")
		}
	})
	t.Run("app_server_encrypted_reasoning_missing_id", func(t *testing.T) {
		h.recorder.begin("app_server_encrypted_reasoning_missing_id")
		setMode(inferenceAppServer)
		session := "reasoning-identity-" + bpsUUID()
		prompt := "Compute the least positive integer n such that n is divisible by 37 and its decimal digit sum equals 37. Give only the integer. Do not use tools."
		history := []json.RawMessage{json.RawMessage(mustJSONUsage(map[string]string{"role": "user", "content": prompt}))}
		code, out := call("/responses", map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh", "summary": "detailed"}, "stream": false, "input": history}, session)
		if code != 200 {
			t.Fatal("reasoning source response failed")
		}
		var items []json.RawMessage
		if json.Unmarshal(response(out)["output"], &items) != nil {
			t.Fatal("missing reasoning source output")
		}
		changed := 0
		for _, raw := range items {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if string(fields["type"]) == `"reasoning"` && len(fields["encrypted_content"]) > 2 && string(fields["encrypted_content"]) != "null" {
				delete(fields, "id")
				raw, _ = json.Marshal(fields)
				changed++
			}
			history = append(history, raw)
		}
		if changed == 0 {
			t.Fatal("no encrypted reasoning source for real regression")
		}
		history = append(history, json.RawMessage(`{"role":"user","content":"Reply exactly CONTINUE_OK. Do not use tools."}`))
		// Explicit model change starts a fresh upstream thread and replays the
		// history, exercising the exact production missing-ID injection path.
		code, out = call("/responses", map[string]any{"model": "gpt-5.6-sol", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "input": history}, session)
		if code != 200 || strings.TrimSpace(answer(response(out))) != "CONTINUE_OK" {
			t.Fatal("encrypted reasoning without client item ID did not resume")
		}
		t.Logf("encrypted reasoning items replayed without client IDs: %d; resumed model=gpt-5.6-sol", changed)
	})
	t.Run("hosted_cli_file_and_resume", func(t *testing.T) {
		h.recorder.begin("hosted_cli_file_and_resume")
		session := "hosted-" + bpsUUID()
		marker := "HOSTED_" + bpsUUID()
		code, out := call("/codex/tasks", map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "prompt": "Create result.txt containing exactly " + marker + ". Read it back with a shell command, then say DONE."}, session)
		if code != 202 {
			t.Fatal("hosted CLI task create failed")
		}
		var id string
		_ = json.Unmarshal(response(out)["id"], &id)
		end := time.Now().Add(5 * time.Minute)
		completed := false
		for time.Now().Before(end) {
			code, out = call("/codex/tasks/"+id, nil, session)
			obj := response(out)
			if string(obj["status"]) == `"completed"` {
				completed = true
				break
			}
			if string(obj["status"]) != `"running"` {
				t.Logf("task status=%s", obj["status"])
				t.Fatal("hosted task failed")
			}
			time.Sleep(time.Second)
		}
		if !completed {
			t.Fatal("hosted task timeout")
		}
		code, out = call("/codex/tasks/"+id+"/files?path=result.txt", nil, session)
		if code != 200 || strings.TrimSpace(string(out)) != marker {
			t.Fatal("hosted file result mismatch")
		}
		code, out = call("/codex/tasks", map[string]any{"resume_task_id": id, "model": "gpt-6-astra", "reasoning_effort": "xhigh", "prompt": "Without using tools, recall the exact text written into result.txt. Reply only that text."}, session)
		if code != 202 {
			t.Fatal("hosted resume create failed")
		}
		var resumed string
		_ = json.Unmarshal(response(out)["id"], &resumed)
		code, out = call("/codex/tasks/"+resumed+"/events", nil, session)
		if code != 200 || !bytes.Contains(out, []byte(marker)) || !bytes.Contains(out, []byte(`"status":"completed"`)) {
			t.Fatal("hosted resume stream failed")
		}
	})
	for _, mode := range []string{inferenceCodexCLI, inferenceAppServer} {
		mode := mode
		t.Run(mode+"_real_cli_tools", func(t *testing.T) {
			setMode(mode)
			marker := "LOCAL_" + bpsUUID()
			result := h.run(mode+"_file", "Create local-result.txt with exactly "+marker+" using local tools, then read the file back independently to verify. Reply only DONE.", "exec", "--json", "--skip-git-repo-check", "-")
			data, err := os.ReadFile(filepath.Join(h.work, "local-result.txt"))
			passed := result.Completed && err == nil && strings.TrimSpace(string(data)) == marker
			h.log(mode+"_file", result, passed)
			if !passed {
				t.Fatal("real downstream CLI local-tool file task failed")
			}
			if mode == inferenceAppServer {
				if result.Thread == "" {
					t.Fatal("real CLI file task omitted thread identity required for resume")
				}
				resumed := h.run("app_server_cli_model_change", "Do not read files or call tools. Recall the exact text you wrote into local-result.txt and reply only that text.", "-c", `model="gpt-5.6-sol"`, "-c", "features.code_mode=false", "exec", "resume", result.Thread, "--json", "--skip-git-repo-check", "-")
				passed := resumed.Completed && bpsLiveNoTools(resumed) && bpsLiveHasText(resumed, marker)
				// h.log has a fixed model label; this transition deliberately uses
				// another explicit model so keep its identity separate in evidence.
				t.Logf("CLI resume model=gpt-5.6-sol effort=xhigh passed=%v result=%s", passed, h.scrub(strings.Join(resumed.Final, "\n")))
				if !passed {
					p.Metrics.mu.Lock()
					failures := []RequestRecord{}
					for _, record := range p.Metrics.recent[:p.Metrics.recentAt] {
						if record.Status == "error" || record.Status == "aborted" {
							failures = append(failures, record)
						}
					}
					p.Metrics.mu.Unlock()
					evidence, _ := json.Marshal(map[string]any{"cli": resumed, "http": h.recorder.forCase("app_server_cli_model_change"), "gateway_errors": failures, "model": "gpt-5.6-sol", "effort": "xhigh"})
					t.Logf("CLI resume failure=%s", h.scrub(string(evidence)))
					t.Fatal("real CLI model/tool-catalog transition failed")
				}
			}
		})
	}
	t.Run("app_server_native_websocket", func(t *testing.T) {
		setMode(inferenceAppServer)
		server := httptest.NewServer(p)
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		header := http.Header{"Authorization": []string{"Bearer test-client-key"}, "Session-Id": []string{"native-rpc-" + bpsUUID()}}
		conn, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/codex/app-server", &websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			if res != nil {
				t.Logf("status=%d", res.StatusCode)
			}
			t.Fatal("native RPC upgrade failed")
		}
		defer conn.CloseNow()
		request := func(id int, method string, params any) json.RawMessage {
			raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
			if conn.Write(ctx, websocket.MessageText, raw) != nil {
				t.Fatal("native RPC write failed")
			}
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					t.Fatal("native RPC read failed")
				}
				var message struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if json.Unmarshal(raw, &message) != nil {
					continue
				}
				if message.ID == id {
					if len(message.Error) > 0 {
						t.Logf("rpc error=%s", message.Error)
						t.Fatal("native RPC request failed")
					}
					return message.Result
				}
			}
		}
		initialized := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "gateway_live_test", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
		if !bytes.Contains(initialized, []byte("userAgent")) {
			t.Fatal("native initialization result missing")
		}
		started := request(2, "thread/start", map[string]any{"model": "gpt-6-astra", "approvalPolicy": "never", "sandbox": "read-only"})
		var thread struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(started, &thread)
		if thread.Thread.ID == "" {
			t.Fatal("native thread missing")
		}
		request(3, "turn/start", map[string]any{"threadId": thread.Thread.ID, "model": "gpt-6-astra", "effort": "xhigh", "input": []any{map[string]string{"type": "text", "text": "Reply exactly NATIVE_RPC_OK without tools."}}})
		text := ""
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatal("native turn stream failed")
			}
			var m struct {
				Method string `json:"method"`
				Params struct {
					Delta string `json:"delta"`
					Turn  struct {
						Status string `json:"status"`
					} `json:"turn"`
				} `json:"params"`
			}
			_ = json.Unmarshal(raw, &m)
			if m.Method == "item/agentMessage/delta" {
				text += m.Params.Delta
			}
			if m.Method == "turn/completed" {
				if m.Params.Turn.Status != "completed" || strings.TrimSpace(text) != "NATIVE_RPC_OK" {
					t.Fatal("native RPC output mismatch")
				}
				break
			}
		}
	})
	t.Logf("official live checks complete; isolated runtime=%s", fmt.Sprint(p.RuntimeModes))
}

func mustReadOfficialLive(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
