package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeFallbackResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *nativeFallbackResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *nativeFallbackResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *nativeFallbackResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// CLI owns the 426 protocol fallback. Both gateway upstream transports remain
// native; no helper/Go adapter silently retries an accepted WS message as HTTP.
func TestGatewayNativeActualCLIWebSocket426FallbackAndResume(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI/native helper paths on Linux for actual WS 426 fallback")
	}
	cli, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal("cannot resolve official CLI")
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("fallback fixture requires the source-pinned official CLI")
	}
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash native helper")
	}
	helperHash := sha256.Sum256(helperBytes)
	home, workspace := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal("cannot create isolated CLI home")
	}
	const toolMarker = "CONTROLLED_NATIVE_FALLBACK_TOOL"
	const firstPrompt = "Execute the requested harmless local tool and finish the controlled fallback turn."
	const firstReply = "CONTROLLED_NATIVE_FALLBACK_FIRST_COMPLETE"
	const resumePrompt = "Resume the same thread and recall the previous tool result without running it again."
	const finalReply = "CONTROLLED_NATIVE_FALLBACK_RESUME_COMPLETE"
	const toolCallID = "native-fallback-tool"
	checks := map[string]bool{}
	for _, name := range []string{"same_upstream_credentials", "same_session_and_thread", "rejected_ws_empty_body", "rejected_ws_no_post_header_data",
		"native_ws_tls_http1", "native_http_tls_h2", "client_received_426_headers", "responses_after_rejection", "cache_key_stable",
		"exact_tool_call_output", "tool_result_stable_on_resume", "resume_context_preserved", "no_go_upstream_fallback"} {
		checks[name] = true
	}
	var mu sync.Mutex
	wsAttempts, httpRequests, rejectedBodyBytes, rejectedDataBytes, rejectionEOFs := 0, 0, 0, 0, 0
	clientRejections, verifiedToolOutputs := 0, 0
	preferredAccountIntroduced, preferredAccountWouldWin := false, false
	session, thread, cacheKey, firstToolOutput := "", "", "", ""
	var requestOrder []string
	rejectionDone := make(chan struct{}, 4)
	fail := func(w http.ResponseWriter, key, message string) {
		checks[key] = false
		t.Error(message)
		http.Error(w, "synthetic fallback assertion failed", http.StatusBadRequest)
	}
	p := newTestProxy(t, 1)
	enableGatewayWS(t, p, "")
	cert, _, ca := nativeCaptureCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		checks["same_upstream_credentials"] = checks["same_upstream_credentials"] && r.Header.Get("Authorization") == "Bearer test-upstream-token" && r.Header.Get("ChatGPT-Account-Id") == "test-account"
		if session == "" {
			session, thread = r.Header.Get("Session-Id"), r.Header.Get("Thread-Id")
		}
		checks["same_session_and_thread"] = checks["same_session_and_thread"] && session != "" && thread != "" && r.Header.Get("Session-Id") == session && r.Header.Get("Thread-Id") == thread
		if r.URL.Path != "/responses" {
			fail(w, "responses_after_rejection", "fallback attempted an unexpected upstream route")
			mu.Unlock()
			return
		}
		if r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts++
			attempt := wsAttempts
			requestOrder = append(requestOrder, "websocket_rejected")
			checks["native_ws_tls_http1"] = checks["native_ws_tls_http1"] && r.TLS != nil && r.ProtoMajor == 1
			checks["responses_after_rejection"] = checks["responses_after_rejection"] && (attempt == 1 && httpRequests == 0 || attempt == 2 && httpRequests == 2)
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 2))
			rejectedBodyBytes += len(body)
			checks["rejected_ws_empty_body"] = checks["rejected_ws_empty_body"] && readErr == nil && len(body) == 0 && r.ContentLength == 0 && len(r.TransferEncoding) == 0
			if attempt == 1 {
				// The first WS admission already installed its sticky binding.
				// A better independent candidate now makes a switch observable.
				if err := p.Store.ImportWith("fallback-preferred", []byte(`{"access_token":"synthetic-preferred-token","chatgpt_account_id":"synthetic-preferred-workspace","expires_at":4102444800}`), AccountDefaults{Concurrency: 2, Priority: 100}); err != nil {
					fail(w, "same_upstream_credentials", "cannot install synthetic competing account after initial binding")
					mu.Unlock()
					return
				}
				p.invalidateAccounts()
				preferredAccountIntroduced = true
				selected, release, _, _, err := p.pickAccount(r.Context(), "test-client-key")
				if err == nil {
					preferredAccountWouldWin = selected.Name == "fallback-preferred"
					release()
				}
				if err != nil || !preferredAccountWouldWin {
					fail(w, "same_upstream_credentials", "unbound selection did not prefer the synthetic competing account")
					mu.Unlock()
					return
				}
			}
			mu.Unlock()
			defer func() { rejectionDone <- struct{}{} }()
			connection, buffered, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error("cannot observe rejected native WS connection tail")
				return
			}
			defer connection.Close()
			response := &http.Response{StatusCode: http.StatusUpgradeRequired, ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: http.NoBody, ContentLength: 0, Close: true}
			response.Header.Set("X-Codex-Turn-State", fmt.Sprintf("rejected-native-route-%d", attempt))
			response.Header.Set("X-Request-Id", fmt.Sprintf("rejected-native-request-%d", attempt))
			response.Header.Set("Content-Type", "application/json")
			if response.Write(buffered) != nil || buffered.Flush() != nil {
				t.Error("cannot write controlled native WS rejection")
				return
			}
			_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, tailErr := buffered.ReadByte()
			mu.Lock()
			if tailErr == nil {
				rejectedDataBytes++
			}
			if tailErr == io.EOF {
				rejectionEOFs++
			}
			checks["rejected_ws_no_post_header_data"] = checks["rejected_ws_no_post_header_data"] && tailErr == io.EOF
			mu.Unlock()
			return
		}
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Upgrade") != "" {
			fail(w, "responses_after_rejection", "CLI fallback did not issue an ordinary Responses POST")
			return
		}
		httpRequests++
		step := httpRequests
		requestOrder = append(requestOrder, "http_response")
		if step > 3 {
			fail(w, "responses_after_rejection", "fallback exceeded the exact three-request bound")
			return
		}
		checks["responses_after_rejection"] = checks["responses_after_rejection"] && (step <= 2 && wsAttempts == 1 || step == 3 && wsAttempts == 2)
		checks["native_http_tls_h2"] = checks["native_http_tls_h2"] && r.TLS != nil && r.TLS.Version == tls.VersionTLS13 && r.ProtoMajor == 2
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		var body map[string]any
		if readErr != nil || len(raw) > maxBody || json.Unmarshal(raw, &body) != nil {
			fail(w, "responses_after_rejection", "invalid native HTTP fallback request")
			return
		}
		key, _ := body["prompt_cache_key"].(string)
		if step == 1 {
			cacheKey = key
		}
		checks["cache_key_stable"] = checks["cache_key_stable"] && key != "" && key == cacheKey
		if body["previous_response_id"] != nil || body["generate"] != nil || body["type"] == "response.create" {
			fail(w, "responses_after_rejection", "rejected WS state leaked into the HTTP request envelope")
			return
		}
		input := realCLIArray(body["input"])
		var item map[string]any
		if step == 1 {
			var shell nativeEditTool
			for _, name := range []string{"exec_command", "shell_command", "shell"} {
				if candidate, ok := nativeEditFindTool(realCLIArray(body["tools"]), name, ""); ok && candidate.Kind == "function" {
					shell = candidate
					break
				}
			}
			if shell.Name == "" {
				fail(w, "exact_tool_call_output", "actual CLI did not offer a supported shell tool")
				return
			}
			item, err = shell.call(toolCallID, "printf '%s\\n' '"+toolMarker+"'")
			if err != nil {
				fail(w, "exact_tool_call_output", "cannot encode actual CLI shell schema")
				return
			}
		} else {
			count := 0
			for _, value := range input {
				candidate, _ := value.(map[string]any)
				if candidate["type"] != "function_call_output" {
					continue
				}
				if candidate["call_id"] != toolCallID {
					fail(w, "exact_tool_call_output", "fallback history contained an unissued tool result")
					return
				}
				count++
				text := nativeEditOutputText(candidate["output"])
				code, parsed := nativeEditToolExit(text)
				if !parsed || code != 0 || !strings.Contains(text, toolMarker) {
					fail(w, "exact_tool_call_output", "actual fallback tool output lost call identity, exit code or marker")
					return
				}
				encoded := realCLIJSON(candidate["output"])
				if step == 2 {
					firstToolOutput = encoded
					verifiedToolOutputs++
				} else {
					checks["tool_result_stable_on_resume"] = checks["tool_result_stable_on_resume"] && encoded == firstToolOutput
				}
			}
			if count != 1 {
				fail(w, "exact_tool_call_output", "fallback tool output was missing or duplicated")
				return
			}
			if step == 2 {
				item = realCLIMessage("native-fallback-first-message", firstReply)
			} else {
				for _, remembered := range []string{firstPrompt, firstReply, toolMarker, resumePrompt} {
					checks["resume_context_preserved"] = checks["resume_context_preserved"] && strings.Contains(realCLIJSON(input), remembered)
				}
				item = realCLIMessage("native-fallback-resume-message", finalReply)
			}
		}
		realCLIWriteSSE(w, fmt.Sprintf("native-fallback-response-%d", step), item, 30)
	}))
	upstream.EnableHTTP2 = true
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	realGatewayNativePool(t, p)
	p.NativeWebSocket = true
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			observed := &nativeFallbackResponseWriter{ResponseWriter: w}
			p.ServeHTTP(observed, r)
			mu.Lock()
			clientRejections++
			hint := fmt.Sprintf("rejected-native-route-%d", clientRejections)
			requestID := fmt.Sprintf("rejected-native-request-%d", clientRejections)
			checks["client_received_426_headers"] = checks["client_received_426_headers"] && observed.status == http.StatusUpgradeRequired && observed.Header().Get("X-Codex-Turn-State") == hint && observed.Header().Get("X-Request-Id") == requestID
			mu.Unlock()
			return
		}
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "native_fallback_fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
model_auto_compact_token_limit = 1000000
[model_providers.native_fallback_fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = true
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write isolated fallback configuration")
	}
	first := nativeEditRunCLI(t, cli, home, workspace, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", firstPrompt)
	if !bytes.Contains(first, []byte(firstReply)) {
		t.Fatal("actual CLI did not complete its native WS-to-HTTP tool fallback")
	}
	threadID := ""
	for _, line := range bytes.Split(first, []byte{'\n'}) {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "thread.started" {
			threadID = event.ThreadID
		}
	}
	if threadID == "" {
		t.Fatal("actual fallback CLI did not persist its thread")
	}
	resumed := nativeEditRunCLI(t, cli, home, workspace, "exec", "resume", "--skip-git-repo-check", "--json", threadID, resumePrompt)
	if !bytes.Contains(resumed, []byte(finalReply)) {
		t.Fatal("actual CLI did not complete resumed native protocol fallback")
	}
	for range 2 {
		select {
		case <-rejectionDone:
		case <-time.After(3 * time.Second):
			t.Fatal("rejected WS tail observation did not terminate")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	checks["exact_two_ws_rejections_three_http_requests"] = wsAttempts == 2 && httpRequests == 3 && clientRejections == 2 && rejectionEOFs == 2
	checks["one_tool_result_observed_no_extra_response_requests"] = verifiedToolOutputs == 1 && httpRequests == 3
	checks["native_modes_remain_enabled"] = p.NativeHTTP != nil && p.NativeWebSocket
	checks["account_affinity_survived_preferred_candidate"] = preferredAccountIntroduced && preferredAccountWouldWin && checks["same_upstream_credentials"]
	checks["no_go_upstream_fallback"] = guard.calls.Load() == 0
	checks["gateway_admission_released"] = p.inFlight("test") == 0
	page, err := p.Metrics.Logs(LogQuery{Size: 10})
	checks["three_http_usage_records"] = err == nil && len(page.Items) == 3
	var inputTokens, outputTokens int64
	for _, record := range page.Items {
		inputTokens += record.InputTokens
		outputTokens += record.OutputTokens
		checks["three_http_usage_records"] = checks["three_http_usage_records"] && record.RequestType == "http_sse" && record.Status == "ok"
	}
	checks["synthetic_usage_totals_preserved"] = inputTokens == 90 && outputTokens == 3
	for name, passed := range checks {
		if !passed {
			t.Errorf("actual native fallback assertion failed: %s", name)
		}
	}
	report := map[string]any{"schema": "official-cli-native-ws-426-http-fallback-v1", "passed": !t.Failed(), "cli": identity,
		"helper_sha256": hex.EncodeToString(helperHash[:]), "websocket_rejected_handshakes": wsAttempts, "http_responses_requests": httpRequests,
		"client_426_responses": clientRejections, "rejected_ws_request_body_bytes": rejectedBodyBytes, "rejected_ws_post_header_data_bytes": rejectedDataBytes,
		"rejected_connection_eofs": rejectionEOFs, "verified_tool_outputs": verifiedToolOutputs, "cli_processes": 2, "request_order": requestOrder,
		"synthetic_competing_account_introduced": preferredAccountIntroduced, "unbound_control_preferred_competing_account": preferredAccountWouldWin,
		"go_upstream_fallback_attempts": guard.calls.Load(), "assertions": checks, "fresh_os_and_cli_home": true,
		"desktop_auth_read": false, "live_accounts": false, "live_model_inference": false,
		"execution_path": "official CLI native WS -> rejected Rust TLS WS handshake 426 -> CLI HTTP choice -> Rust UDS TLS/H2 Responses",
		"limits": []string{"Only explicit 426 fallback is exercised; 401 and other failures follow separate source/recovery paths.",
			"One issued tool result and exact request counts are not an OS-wide exactly-once execution guarantee.",
			"Fresh synthetic authentication and loopback replies do not verify live account, cache-hit, geographic or production behavior."}}
	if path := os.Getenv("CODEX_NATIVE_FALLBACK_CLI_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized native fallback report")
		}
	}
	t.Logf("actual CLI native 426 fallback: websocket_rejections=%d HTTP_requests=%d tool_outputs=%d Go_fallback=%d", wsAttempts, httpRequests, verifiedToolOutputs, guard.calls.Load())
}
