package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// These files are produced only by the two genuine CLI tools. Exclusive start
// creation rejects re-execution; the rendezvous and monotonic intervals prove
// overlap rather than merely accepting two outputs from a scripted server.
const nativeParallelWorker = `import json
import sys
import time
from pathlib import Path

name = sys.argv[1]
assert name in ("a", "b")
started = time.monotonic_ns()
with Path(name + ".start").open("x") as f:
    f.write(str(started))
peer = Path("b.done" if name == "a" else "a.start")
deadline = time.monotonic() + 4
while not peer.exists():
    if time.monotonic() >= deadline:
        raise RuntimeError("parallel rendezvous timed out")
    time.sleep(0.005)
time.sleep(0.08 if name == "a" else 0.12)
ended = time.monotonic_ns()
code = 0 if name == "a" else 7
payload = {"name": name, "start_ns": started, "end_ns": ended, "exit_code": code}
temporary = Path(name + ".tmp")
temporary.write_text(json.dumps(payload))
temporary.rename(name + ".done")
print("CONTROLLED_PARALLEL_" + name.upper())
sys.exit(code)
`

type nativeParallelExecution struct {
	Name     string `json:"name"`
	Start    int64  `json:"start_ns"`
	End      int64  `json:"end_ns"`
	ExitCode int    `json:"exit_code"`
}

func TestGatewayNativeActualCLIParallelTools(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI/native helper paths on Linux for parallel tool execution")
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("parallel fixture requires the source-pinned CLI")
	}
	var scenarios []map[string]any
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			scenarios = append(scenarios, nativeParallelScenario(t, cli, transport))
		})
	}
	report := map[string]any{"schema": "official-cli-native-parallel-tools-v1", "passed": !t.Failed(), "cli": identity,
		"scenarios": scenarios, "fresh_cli_home": true, "fresh_os_home": true, "desktop_auth_read": false,
		"live_accounts": false, "live_model_inference": false,
		"scope": "two genuine exec_command calls per transport, synthetic local Responses, not model reasoning or real cache performance"}
	if path := os.Getenv("CODEX_NATIVE_PARALLEL_CLI_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized parallel-tool report")
		}
	}
}

func nativeParallelScenario(t *testing.T, cli, transport string) map[string]any {
	t.Helper()
	isWS := transport == "websocket"
	home, workspace := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "parallel_worker.py"), []byte(nativeParallelWorker), 0600); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, 1)
	if isWS {
		enableGatewayWS(t, p, "Stable parallel tool fixture instructions.")
	}
	var mu sync.Mutex
	requests, warmups, connections, verifiedOutputs := 0, 0, 0, 0
	var firstInput []any
	var calls []any
	properties, cacheKey, latestResponse := "", "", ""
	checks := map[string]bool{}
	for _, name := range []string{"parallel_flag", "stable_tools_instructions", "stable_cache_key", "history_or_delta_preserved",
		"distinct_output_identity_and_exit", "native_tls", "native_application_identity"} {
		checks[name] = true
	}
	if !isWS {
		checks["exact_call_identity_and_arguments"] = true
	}
	// The server scripts model responses, never tool output or side-effect files.
	respond := func(body map[string]any) ([]map[string]any, bool) {
		input, ok := body["input"].([]any)
		if !ok {
			t.Error("parallel fixture did not receive an input array")
			return nil, false
		}
		warmup := body["generate"] == false
		if warmup {
			warmups++
			if !isWS || warmups != 1 || requests != 0 {
				t.Error("unexpected parallel fixture warmup")
				return nil, false
			}
		} else {
			requests++
		}
		if requests > 2 {
			t.Error("parallel workflow exceeded its two-request bound")
			return nil, false
		}
		if isWS {
			previous, _ := body["previous_response_id"].(string)
			if latestResponse != "" && previous != latestResponse || latestResponse == "" && previous != "" {
				checks["history_or_delta_preserved"] = false
				t.Error("WS continuation lost the exact latest response identity")
				return nil, false
			}
		}
		output := []any{}
		if !warmup {
			checks["parallel_flag"] = checks["parallel_flag"] && body["parallel_tool_calls"] == true
			currentProperties := realCLIJSON(map[string]any{"model": body["model"], "instructions": body["instructions"], "tools": body["tools"]})
			key, _ := body["prompt_cache_key"].(string)
			if requests == 1 {
				properties, cacheKey = currentProperties, key
				firstInput = append([]any(nil), input...)
				tool, found := nativeEditFindTool(realCLIArray(body["tools"]), "exec_command", "")
				if !found || tool.Kind != "function" {
					t.Error("official CLI did not expose exec_command")
					return nil, false
				}
				for _, name := range []string{"a", "b"} {
					item, err := tool.call("parallel-"+name, "python3 parallel_worker.py "+name)
					if err != nil {
						t.Error("cannot construct tool call from official schema")
						return nil, false
					}
					var args map[string]any
					if json.Unmarshal([]byte(item["arguments"].(string)), &args) != nil {
						t.Error("invalid fixture tool arguments")
						return nil, false
					}
					// Wait for both bounded workers instead of creating background
					// process sessions whose early outputs would not prove overlap.
					args["yield_time_ms"] = 10000
					item["arguments"] = realCLIJSON(args)
					calls = append(calls, item)
				}
				output = calls
			} else {
				checks["stable_tools_instructions"] = properties == currentProperties
				checks["stable_cache_key"] = cacheKey != "" && key == cacheKey
				if isWS {
					checks["history_or_delta_preserved"] = len(input) == 2
				} else {
					checks["history_or_delta_preserved"] = len(input) == len(firstInput)+4 && reflect.DeepEqual(input[:len(firstInput)], firstInput)
					for index, issued := range calls {
						if len(input) <= len(firstInput)+index {
							checks["exact_call_identity_and_arguments"] = false
							continue
						}
						actual, _ := input[len(firstInput)+index].(map[string]any)
						for _, field := range []string{"type", "call_id", "name", "namespace", "arguments"} {
							checks["exact_call_identity_and_arguments"] = checks["exact_call_identity_and_arguments"] && reflect.DeepEqual(actual[field], issued.(map[string]any)[field])
						}
					}
				}
				seen := map[string]bool{}
				for _, value := range input {
					item, _ := value.(map[string]any)
					if item["type"] != "function_call_output" {
						continue
					}
					id, _ := item["call_id"].(string)
					text := nativeEditOutputText(item["output"])
					code, parsed := nativeEditToolExit(text)
					valid := id == "parallel-a" && code == 0 && strings.Contains(text, "CONTROLLED_PARALLEL_A") && !strings.Contains(text, "CONTROLLED_PARALLEL_B") ||
						id == "parallel-b" && code == 7 && strings.Contains(text, "CONTROLLED_PARALLEL_B") && !strings.Contains(text, "CONTROLLED_PARALLEL_A")
					checks["distinct_output_identity_and_exit"] = checks["distinct_output_identity_and_exit"] && parsed && valid && !seen[id]
					seen[id] = true
					verifiedOutputs++
				}
				checks["distinct_output_identity_and_exit"] = checks["distinct_output_identity_and_exit"] && verifiedOutputs == 2 && seen["parallel-a"] && seen["parallel-b"]
				output = []any{realCLIMessage("parallel-final", "CONTROLLED_PARALLEL_COMPLETE")}
			}
		}
		id := fmt.Sprintf("parallel-response-%d-%d", warmups, requests)
		events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": id}}}
		for index, item := range output {
			events = append(events, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
		tokens := 40
		if warmup {
			tokens = 0
		}
		events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output,
			"usage": map[string]any{"input_tokens": tokens, "output_tokens": len(output), "total_tokens": tokens + len(output)}}})
		latestResponse = id
		return events, true
	}
	cert, _, ca := nativeCaptureCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		checks["native_tls"] = checks["native_tls"] && r.TLS != nil && r.TLS.Version == tls.VersionTLS13 && (isWS || r.ProtoMajor == 2)
		checks["native_application_identity"] = checks["native_application_identity"] && strings.HasPrefix(r.UserAgent(), "codex_exec/"+clientVersion+" ") && r.Header.Get("Originator") == "codex_exec"
		mu.Unlock()
		if r.URL.Path != "/responses" {
			t.Error("unexpected parallel fixture path")
			http.Error(w, "Responses required", 400)
			return
		}
		if isWS {
			if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				t.Error("unexpected HTTP fallback in WS parallel fixture")
				http.Error(w, "WebSocket required", 426)
				return
			}
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
			if err != nil {
				t.Error("cannot accept fixture WebSocket")
				return
			}
			defer conn.CloseNow()
			conn.SetReadLimit(maxBody)
			mu.Lock()
			connections++
			mu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					return
				}
				var body map[string]any
				if json.Unmarshal(raw, &body) != nil {
					t.Error("invalid WS parallel fixture request")
					return
				}
				mu.Lock()
				events, ok := respond(body)
				mu.Unlock()
				if !ok {
					return
				}
				for _, event := range events {
					if lane, found := body["stream_id"]; found {
						event["stream_id"] = lane
					}
					if err := conn.Write(ctx, websocket.MessageText, []byte(realCLIJSON(event))); err != nil {
						return
					}
				}
			}
		}
		if r.Method != http.MethodPost {
			t.Error("unexpected non-POST parallel request")
			http.Error(w, "POST required", 400)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		var body map[string]any
		if err != nil || len(raw) > maxBody || json.Unmarshal(raw, &body) != nil {
			t.Error("invalid HTTP parallel fixture request")
			http.Error(w, "invalid request", 400)
			return
		}
		mu.Lock()
		events, ok := respond(body)
		mu.Unlock()
		if !ok {
			http.Error(w, "fixture assertion failed", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], realCLIJSON(event))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	upstream.EnableHTTP2 = !isWS
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	realGatewayNativePool(t, p)
	p.NativeWebSocket = isWS
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "parallel_fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
model_auto_compact_token_limit = 1000000
[model_providers.parallel_fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = %t
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1", isWS)
	if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write isolated parallel configuration")
	}
	stdout := nativeEditRunCLI(t, cli, home, workspace, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Execute the two local fixture commands and report both results, including the expected failure.")
	if !bytes.Contains(stdout, []byte("CONTROLLED_PARALLEL_COMPLETE")) {
		t.Fatal("official CLI did not finish the parallel workflow")
	}
	// CLI exit precedes the gateway's asynchronous close and metric publication.
	deadline := time.Now().Add(3 * time.Second)
	for p.inFlight("test") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	checks["native_no_go_fallback"] = guard.calls.Load() == 0
	checks["exact_request_and_call_counts"] = requests == 2 && len(calls) == 2 && verifiedOutputs == 2 && (!isWS && warmups == 0 && connections == 0 || isWS && warmups == 1 && connections == 1)
	checks["gateway_admission_released"] = p.inFlight("test") == 0
	var executions [2]nativeParallelExecution
	checks["two_exclusive_side_effects"] = true
	for index, name := range []string{"a", "b"} {
		start, startErr := os.ReadFile(filepath.Join(workspace, name+".start"))
		raw, err := os.ReadFile(filepath.Join(workspace, name+".done"))
		if err != nil || json.Unmarshal(raw, &executions[index]) != nil {
			t.Error("actual CLI parallel execution record missing or invalid")
			checks["two_exclusive_side_effects"] = false
			continue
		}
		execution := executions[index]
		checks["two_exclusive_side_effects"] = checks["two_exclusive_side_effects"] && startErr == nil && string(start) == fmt.Sprint(execution.Start) && execution.Name == name && execution.Start > 0 && execution.End > execution.Start && execution.ExitCode == index*7
	}
	a, b := executions[0], executions[1]
	overlap := min(a.End, b.End) - max(a.Start, b.Start)
	checks["actual_process_intervals_overlap"] = overlap > 0
	checks["worker_completion_order_reversed"] = b.End < a.End
	page, err := p.Metrics.Logs(LogQuery{Size: 10})
	checks["two_successful_generated_records"] = err == nil && len(page.Items) == 2+warmups
	checks["warmup_classification_and_zero_usage"] = true
	warmupRecords, generatedRecords := 0, 0
	var inputTokens, outputTokens int64
	for _, record := range page.Items {
		checks["two_successful_generated_records"] = checks["two_successful_generated_records"] && record.Status == "ok" && record.UsageKnown != nil && *record.UsageKnown
		if isWS {
			checks["warmup_classification_and_zero_usage"] = checks["warmup_classification_and_zero_usage"] && record.RequestType == "websocket" && record.WebSocketWarmup != nil
		}
		if record.WebSocketWarmup != nil && *record.WebSocketWarmup {
			warmupRecords++
			checks["warmup_classification_and_zero_usage"] = checks["warmup_classification_and_zero_usage"] && record.InputTokens == 0 && record.OutputTokens == 0 && record.CachedTokens == 0
		} else {
			generatedRecords++
		}
		inputTokens += record.InputTokens
		outputTokens += record.OutputTokens
	}
	checks["two_successful_generated_records"] = checks["two_successful_generated_records"] && generatedRecords == 2
	checks["warmup_classification_and_zero_usage"] = checks["warmup_classification_and_zero_usage"] && warmupRecords == warmups
	checks["synthetic_usage_not_tool_count"] = inputTokens == 80 && outputTokens == 3
	for name, passed := range checks {
		if !passed {
			t.Errorf("actual parallel tool assertion failed: %s", name)
		}
	}
	t.Logf("actual CLI parallel tools: transport=%s model_requests=%d outputs=%d overlap_ns=%d worker_completion_order=b,a", transport, requests, verifiedOutputs, overlap)
	return map[string]any{"transport": transport, "passed": !t.Failed(), "assertions": checks, "generated_requests": requests,
		"warmups": warmups, "websocket_connections": connections, "issued_calls": len(calls), "verified_tool_outputs": verifiedOutputs,
		"overlap_ns": overlap, "worker_completion_order": []string{"b", "a"}, "tool_exit_codes_by_issue_order": []int{a.ExitCode, b.ExitCode},
		"warmup_records": warmupRecords, "generated_records": generatedRecords,
		"go_fallback_attempts": guard.calls.Load(), "tls": checks["native_tls"], "synthetic_input_tokens": inputTokens, "synthetic_output_tokens": outputTokens}
}
