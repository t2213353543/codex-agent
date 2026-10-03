package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// This opt-in test uses the unmodified official CLI against a local genuine
// WebSocket upstream, including warmup, tool continuation, and process resume.
func TestRealCodexCLIWebSocket(t *testing.T) {
	bin := os.Getenv("CODEX_REAL_CLI_BIN")
	if bin == "" {
		t.Skip("set CODEX_REAL_CLI_BIN for official CLI WebSocket protocol integration")
	}
	runOfficialCLIWebSocket(t, bin, false)
}

func TestGatewayNativeActualWebSocketCLI(t *testing.T) {
	bin := os.Getenv("CODEX_NATIVE_CAPTURE_BIN")
	if bin == "" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI and native helper paths on Linux")
	}
	runOfficialCLIWebSocket(t, bin, true)
}

func runOfficialCLIWebSocket(t *testing.T, bin string, native bool) {
	t.Helper()
	var scenarios []map[string]any
	for _, source := range []string{"handshake", "response_metadata"} {
		t.Run(source, func(t *testing.T) {
			scenarios = append(scenarios, runOfficialCLIWebSocketScenario(t, bin, native, source))
		})
	}
	if native {
		if reportPath := os.Getenv("CODEX_NATIVE_WS_CLI_REPORT"); reportPath != "" {
			report := map[string]any{"schema": "official-cli-native-websocket-v2", "passed": !t.Failed(), "scenarios": scenarios,
				"live_model_inference": false, "upstream": "synthetic stateful loopback WebSocket", "tls_scope": "plaintext in this test; separate TLS and proxy tests"}
			connections, generated, warmups, continuations, toolOutputs, fallback := 0, 0, 0, 0, 0, int64(0)
			for _, scenario := range scenarios {
				connections += scenario["connections"].(int)
				generated += scenario["generated_responses"].(int)
				warmups += scenario["warmups"].(int)
				continuations += scenario["incremental_continuations"].(int)
				toolOutputs += scenario["tool_output_requests"].(int)
				fallback += int64(scenario["go_fallback_attempts"].(int32))
				report["cli_version"] = scenario["cli_version"]
			}
			report["connections"], report["generated_responses"], report["warmups"] = connections, generated, warmups
			report["incremental_continuations"], report["tool_output_requests"], report["go_fallback_attempts"] = continuations, toolOutputs, fallback
			report["actual_shell_tool"], report["resume_context_verified"] = !t.Failed(), !t.Failed()
			encoded, err := json.MarshalIndent(report, "", "  ")
			if err != nil || os.WriteFile(reportPath, append(encoded, '\n'), 0600) != nil {
				t.Fatal("cannot write sanitized native WS CLI report")
			}
		}
	}
}

func runOfficialCLIWebSocketScenario(t *testing.T, bin string, native bool, hintSource string) map[string]any {
	t.Helper()
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("CLI version: %v %s", err, version)
	}
	t.Logf("real client: %s; genuine local WebSocket upstream", strings.TrimSpace(string(version)))
	const toolMarker = "CODEX_GATEWAY_WS_TOOL_OK"
	const firstPrompt = "Run the harmless requested echo tool and finish the WebSocket test."
	const firstReply = "CODEX_GATEWAY_WS_FIRST_OK"
	const resumePrompt = "Continue the same session and recall the WebSocket tool output."
	const resumeReply = "CODEX_GATEWAY_WS_RESUME_OK"
	var mu sync.Mutex
	var requests []realCLIRequest
	type completedContext struct {
		input, output []any
		warmup        bool
	}
	connections, warmups, generated := 0, 0, 0
	previousLinks, emptyDeltas, warmupContinuations, emptyWarmupContinuations, exactToolDeltas, stateEchoes, absentStates := 0, 0, 0, 0, 0, 0, 0
	var finalLogicalInput []any
	var downstreamHintMu sync.Mutex
	var downstreamHandshakeHints []string
	p := newTestProxy(t, 1)
	enableGatewayWS(t, p, "Stable gateway WS instructions.")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Errorf("unexpected HTTP fallback: %s %s", r.Method, r.URL.Path)
			http.Error(w, "WebSocket required", 426)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("gateway upstream credentials missing")
		}
		mu.Lock()
		connections++
		connectionID := connections
		mu.Unlock()
		hint := fmt.Sprintf("ws-route-%s-%d", hintSource, connectionID)
		if hintSource == "handshake" {
			w.Header().Set("X-Codex-Turn-State", hint)
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxBody)
		completed := make(map[string]completedContext)
		lastCompletedID := ""
		metadataSent := false
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			requests = append(requests, realCLIRequest{r.URL.Path, r.Header.Clone(), body})
			isWarmup := body["generate"] == false
			if isWarmup {
				warmups++
			} else {
				generated++
			}
			turn := generated
			id := fmt.Sprintf("resp_ws_%d_%d", connectionID, len(requests))
			input, ok := body["input"].([]any)
			if !ok {
				t.Error("official CLI did not send an input array")
				mu.Unlock()
				return
			}
			previous, _ := body["previous_response_id"].(string)
			logicalInput := append([]any(nil), input...)
			if previous != "" {
				prior, found := completed[previous]
				if !found || previous != lastCompletedID {
					t.Error("incremental request did not reference the exact latest completed response on this connection")
					mu.Unlock()
					return
				}
				previousLinks++
				logicalInput = append(append(append([]any(nil), prior.input...), prior.output...), input...)
				if len(input) == 0 {
					emptyDeltas++
				}
				if prior.warmup && !isWarmup {
					warmupContinuations++
					if len(input) == 0 {
						emptyWarmupContinuations++
					}
				}
				for _, delta := range input {
					for _, priorItem := range append(append([]any(nil), prior.input...), prior.output...) {
						if reflect.DeepEqual(delta, priorItem) {
							t.Error("incremental delta repeated an already completed input/output item")
						}
					}
				}
			}
			metadata, _ := body["client_metadata"].(map[string]any)
			state, _ := metadata["x-codex-turn-state"].(string)
			if metadataSent {
				if state != hint {
					t.Error("CLI continuation lost current turn state or reused a previous connection's hint")
				} else {
					stateEchoes++
				}
			} else if state != "" {
				t.Error("CLI used a turn-state hint before receiving response.metadata")
			} else {
				// CLI 0.155 core connect_websocket passes turn_state=None to
				// its handshake API; only streamed response.metadata sets it.
				absentStates++
			}
			if !isWarmup && turn == 2 {
				if previous == "" || len(input) != 1 {
					t.Error("tool followup was not a single incremental tool result")
				} else if tool, ok := input[0].(map[string]any); !ok || tool["type"] != "function_call_output" || tool["call_id"] != "call_ws_echo" || !strings.Contains(realCLIJSON(tool["output"]), toolMarker) {
					t.Error("incremental tool result changed type, call identity or output")
				} else {
					exactToolDeltas++
				}
			}
			if !isWarmup && turn == 3 {
				finalLogicalInput = append([]any(nil), logicalInput...)
			}
			mu.Unlock()
			var item map[string]any
			if !isWarmup {
				switch turn {
				case 1:
					name, namespace := realCLIShellTool(realCLIArray(body["tools"]), "")
					if name == "" {
						t.Error("official CLI shell tool missing")
						return
					}
					args := map[string]any{"cmd": "echo " + toolMarker, "login": false, "max_output_tokens": 100}
					if name == "shell_command" {
						args = map[string]any{"command": "echo " + toolMarker, "login": false}
					} else if name == "shell" {
						args = map[string]any{"command": []string{"echo", toolMarker}}
					}
					item = map[string]any{"id": "fc_ws_fixture", "type": "function_call", "call_id": "call_ws_echo", "name": name, "arguments": realCLIJSON(args)}
					if namespace != "" {
						item["namespace"] = namespace
					}
				case 2:
					item = realCLIMessage("msg_ws_first", firstReply)
				case 3:
					item = realCLIMessage("msg_ws_resume", resumeReply)
				default:
					t.Errorf("unexpected generated turn %d", turn)
					return
				}
			}
			events := []map[string]any{}
			if hintSource == "response_metadata" {
				emittedHint := hint
				if metadataSent {
					emittedHint += "-later-ignored"
				}
				events = append(events, map[string]any{"type": "response.metadata", "response_id": id, "headers": map[string]any{"x-codex-turn-state": emittedHint}})
				metadataSent = true
			}
			events = append(events, map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
			output := []any{}
			if item != nil {
				output = append(output, item)
				events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			}
			usage := map[string]any{"input_tokens": 100, "output_tokens": 1, "total_tokens": 101, "input_tokens_details": map[string]any{"cached_tokens": 80}}
			if isWarmup {
				usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": 0}}
			}
			events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output, "usage": usage}})
			for _, event := range events {
				if lane, ok := body["stream_id"]; ok {
					event["stream_id"] = lane
				}
				if err := conn.Write(r.Context(), websocket.MessageText, []byte(realCLIJSON(event))); err != nil {
					return
				}
			}
			completed[id] = completedContext{input: logicalInput, output: output, warmup: isWarmup}
			lastCompletedID = id
		}
	}))
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	guard := &nativeCLIFallbackGuard{}
	if native {
		realGatewayNativePool(t, p)
		p.NativeWebSocket = true
		p.Client.Transport = guard
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r)
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			downstreamHintMu.Lock()
			downstreamHandshakeHints = append(downstreamHandshakeHints, w.Header().Get("X-Codex-Turn-State"))
			downstreamHintMu.Unlock()
		}
	}))
	t.Cleanup(gateway.Close)
	cliHome, workdir := t.TempDir(), t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
model_auto_compact_token_limit = 1000000
[model_providers.fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = true
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1")
	if err := os.WriteFile(filepath.Join(cliHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	first := realCLIRun(t, bin, cliHome, workdir, "exec", "--skip-git-repo-check", "--json", firstPrompt)
	if !strings.Contains(first, firstReply) || !strings.Contains(first, toolMarker) {
		t.Fatalf("CLI WebSocket tool flow failed: %s", first)
	}
	threadID := ""
	for _, line := range strings.Split(first, "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["type"] == "thread.started" {
			threadID, _ = event["thread_id"].(string)
		}
	}
	if threadID == "" {
		t.Fatal("CLI did not persist thread")
	}
	resumed := realCLIRun(t, bin, cliHome, workdir, "exec", "resume", "--skip-git-repo-check", "--json", threadID, resumePrompt)
	if !strings.Contains(resumed, resumeReply) {
		t.Fatalf("CLI WebSocket resume failed: %s", resumed)
	}
	// Both real CLI processes have exited; wait for gateway handlers to finish
	// forwarding their close and expose the actual downstream upgrade headers.
	deadline := time.Now().Add(3 * time.Second)
	for {
		downstreamHintMu.Lock()
		finished := len(downstreamHandshakeHints)
		downstreamHintMu.Unlock()
		mu.Lock()
		opened := connections
		mu.Unlock()
		if finished == opened && finished >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CLI gateway WebSocket handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if generated != 3 || connections < 2 {
		t.Fatalf("generated=%d connections=%d", generated, connections)
	}
	continuations, toolOutputs := 0, 0
	for _, request := range requests {
		if id, _ := request.body["previous_response_id"].(string); id != "" {
			continuations++
		}
		if strings.Contains(realCLIJSON(request.body["input"]), toolMarker) {
			toolOutputs++
		}
		instructions, _ := request.body["instructions"].(string)
		if !strings.HasPrefix(instructions, "Stable gateway WS instructions.") {
			t.Error("gateway prompt missing from real CLI WebSocket turn")
		}
		if request.header.Get("Thread-Id") != threadID {
			t.Error("thread identity changed across resume")
		}
	}
	stateValid := hintSource == "handshake" && absentStates == len(requests) || hintSource == "response_metadata" && stateEchoes >= 3 && absentStates == connections
	if continuations == 0 || toolOutputs == 0 || previousLinks != continuations || exactToolDeltas != 1 || warmupContinuations != 2 || !stateValid {
		t.Fatalf("incremental continuations=%d tool output requests=%d", continuations, toolOutputs)
	}
	if native && guard.calls.Load() != 0 {
		t.Error("native CLI WebSocket used Go outbound fallback")
	}
	downstreamHintMu.Lock()
	forwardedHints := append([]string(nil), downstreamHandshakeHints...)
	downstreamHintMu.Unlock()
	if hintSource == "handshake" {
		seen := map[string]bool{}
		for _, hint := range forwardedHints {
			seen[hint] = true
		}
		for connection := 1; connection <= connections; connection++ {
			if !seen[fmt.Sprintf("ws-route-%s-%d", hintSource, connection)] {
				t.Error("gateway lost the actual upstream handshake turn-state header")
			}
		}
	} else {
		for _, hint := range forwardedHints {
			if hint != "" {
				t.Error("metadata-only fixture invented a handshake turn-state header")
			}
		}
	}
	records := p.Metrics.RecentForAccount("test", 20)
	warmupRecords, generatedRecords := 0, 0
	warmupUsageZero := true
	for _, record := range records {
		if record.RequestType != "websocket" || record.WebSocketWarmup == nil {
			warmupUsageZero = false
			t.Error("real CLI WebSocket request lost passive warmup classification")
			continue
		}
		if *record.WebSocketWarmup {
			warmupRecords++
			if record.InputTokens != 0 || record.OutputTokens != 0 || record.CachedTokens != 0 {
				warmupUsageZero = false
				t.Error("zero-usage warmup fixture gained invented token usage")
			}
		} else {
			generatedRecords++
			if record.InputTokens != 100 || record.OutputTokens != 1 || record.CachedTokens != 80 {
				t.Error("generated response usage changed while adding passive warmup classification")
			}
		}
	}
	warmupUsageZero = warmupUsageZero && warmupRecords > 0 && warmupRecords == warmups
	if warmupRecords != warmups || generatedRecords != generated || len(records) != warmups+generated {
		t.Error("real CLI warmup/generated records were dropped or misclassified")
	}
	last := realCLIJSON(finalLogicalInput)
	for _, remembered := range []string{firstPrompt, firstReply, toolMarker, resumePrompt} {
		if !strings.Contains(last, remembered) {
			t.Errorf("resumed context lost %q", remembered)
		}
	}
	t.Logf("%s: connections=%d generated=%d warmups=%d exact continuations=%d warmup empty deltas=%d; real tool + reconstructed resume verified", hintSource, connections, generated, warmups, previousLinks, emptyWarmupContinuations)
	assertions := map[string]bool{"previous_response_id_same_connection_completed": previousLinks == continuations && continuations > 0,
		"tool_delta_not_repeated_history": exactToolDeltas == 1, "turn_state_exact_and_resume_isolated": !t.Failed(),
		"reconstructed_resume_context_verified": !t.Failed(), "warmup_usage_zero": warmupUsageZero}
	if hintSource == "response_metadata" {
		assertions["first_metadata_hint_retained"] = stateValid
	}
	return map[string]any{"passed": !t.Failed(), "cli_version": strings.TrimSpace(string(version)), "turn_state_source": hintSource,
		"connections": connections, "generated_responses": generated, "warmups": warmups, "incremental_continuations": continuations,
		"tool_output_requests": toolOutputs, "go_fallback_attempts": guard.calls.Load(), "exact_previous_response_links": previousLinks,
		"empty_input_deltas_observed": emptyDeltas, "warmup_continuations": warmupContinuations, "empty_warmup_continuations_observed": emptyWarmupContinuations,
		"exact_tool_output_only_deltas": exactToolDeltas, "current_turn_state_echoes": stateEchoes,
		"turn_state_absent_before_metadata":                absentStates,
		"handshake_hint_forwarded_but_not_consumed_by_cli": hintSource == "handshake" && stateValid,
		"warmup_records": warmupRecords, "generated_records": generatedRecords,
		"assertions": assertions}
}
