package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The official SSE consumer ignores output_index for OutputItemDone. The
// gateway's JSON collector has a stricter index contract. A collector hardening
// must therefore never become a destructive SSE rewrite or a turn replay.
func cliContractWire(id string, items []map[string]any, invalid bool) []byte {
	var wire bytes.Buffer
	wire.WriteString(": raw contract <>& 你好\r\n\r\n")
	event := func(value any) {
		wire.WriteString("data: ")
		encoder := json.NewEncoder(&wire)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value)
		wire.WriteString("\r\n")
	}
	event(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
	wire.WriteString("event: future.contract\r\ndata: {\"type\":\"future.contract\",\"exact\":9007199254740993,\"opaque\":\"+/=<>&你好\"}\r\n\r\n")
	for _, item := range items {
		event(map[string]any{"type": "response.output_item.done", "response_id": id, "output_index": 0, "item": item})
	}
	if invalid {
		event(map[string]any{"type": "response.completed"})
	} else {
		// Omit terminal output deliberately: real CLI already consumed done.
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "usage": map[string]int{"input_tokens": 8, "output_tokens": 2, "total_tokens": 10}}})
	}
	return wire.Bytes()
}

func TestGatewayCLIContractAuditRawStreamAndCollectorBoundary(t *testing.T) {
	items := []map[string]any{
		{"type": "function_call", "id": "item_a", "call_id": "call_a", "name": "fixture", "arguments": `{"text":"<>&你好"}`},
		{"type": "custom_tool_call", "id": "item_b", "call_id": "call_b", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch\n"},
	}
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			wire := cliContractWire("contract-response", items, invalid)
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(wire)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"fixture","stream":true,"input":[]}`))
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), wire) || calls.Load() != 1 || p.inFlight("test") != 0 {
				t.Fatalf("stream changed/replayed: status=%d calls=%d raw_equal=%t", w.Code, calls.Load(), bytes.Equal(w.Body.Bytes(), wire))
			}
			record, exists := p.Metrics.LastFor("test")
			wantStatus := "ok"
			if invalid {
				wantStatus = "error"
			}
			if !exists || record.Status != wantStatus {
				t.Fatalf("invalid terminal classification: got %q want %q", record.Status, wantStatus)
			}
			result, err := collectResponse(bytes.NewReader(wire))
			if result != nil || err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("JSON collector accepted executable item collision: result=%s err=%v", result, err)
			}
		})
	}
}

type cliContractTapWriter struct {
	http.ResponseWriter
	bytes.Buffer
}

func (w *cliContractTapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *cliContractTapWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseWriter.Write(raw)
	_, _ = w.Buffer.Write(raw[:n])
	return n, err
}

// Opt-in real official CLI + real Rust native helper. Upstream model events,
// credentials and attestation are controlled fixtures, not live inference.
func TestGatewayNativeActualCLIContractAudit(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI/native helper paths on Linux for CLI contract audit")
	}
	cli, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal(err)
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("contract audit requires the source-pinned CLI")
	}
	helperRaw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash explicit native helper")
	}
	helperHash := sha256.Sum256(helperRaw)
	var scenarios []map[string]any
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid_terminal=%t", invalid), func(t *testing.T) {
			scenarios = append(scenarios, cliContractActualScenario(t, cli, invalid))
		})
	}
	report := map[string]any{"schema": "official-cli-contract-audit-v1", "passed": !t.Failed(), "cli": identity,
		"helper_sha256": hex.EncodeToString(helperHash[:]), "scenarios": scenarios,
		"execution_path":       "official CLI -> loopback HTTP gateway -> Rust UDS helper -> loopback TLS HTTP/2",
		"live_model_inference": false, "live_accounts": false, "desktop_auth_read": false,
		"limits": []string{"Conflicting output_index is an adversarial fixture accepted by CLI item parsing, not a claim that the upstream normally emits it.", "Raw response bytes are compared at gateway write boundary; this does not establish TLS ciphertext identity.", "Synthetic attestation verifies transparent forwarding only, not authenticity or server acceptance.", "API-key custom provider behavior does not prove built-in ChatGPT OAuth behavior; the separate compression fixture covers that runtime path."}}
	if file := os.Getenv("CODEX_NATIVE_CONTRACT_CLI_REPORT"); file != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(file, append(raw, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized CLI contract report")
		}
	}
}

func cliContractActualScenario(t *testing.T, cli string, invalid bool) map[string]any {
	t.Helper()
	home, workspace := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, 1)
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled, settings.CompatibilityNormalizationEnabled, settings.PromptCacheKeyEnabled = false, false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var inbound, outbound []http.Header
	var expected, received [][]byte
	requests, toolOutputs := 0, 0
	checks := map[string]bool{"native_tls13_h2": true, "native_header_identity": true, "account_credentials_selected": true,
		"opaque_attestation_preserved": true, "turn_state_echo": true, "api_key_provider_no_version": true}
	cert, _, ca := nativeCaptureCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		outbound = append(outbound, r.Header.Clone())
		checks["native_tls13_h2"] = checks["native_tls13_h2"] && r.TLS != nil && r.TLS.Version == tls.VersionTLS13 && r.ProtoMajor == 2
		checks["account_credentials_selected"] = checks["account_credentials_selected"] && r.Header.Get("Authorization") == "Bearer test-upstream-token" && r.Header.Get("ChatGPT-Account-Id") == "test-account"
		checks["opaque_attestation_preserved"] = checks["opaque_attestation_preserved"] && r.Header.Get("X-Oai-Attestation") == "synthetic-opaque-proof"
		checks["api_key_provider_no_version"] = checks["api_key_provider_no_version"] && r.Header.Get("Version") == ""
		if r.Method != http.MethodPost || r.URL.Path != "/responses" || requests > 2 || invalid && requests != 1 {
			t.Error("contract fixture received an unexpected route or replay")
			http.Error(w, "unexpected request", 400)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody+1)).Decode(&body); err != nil {
			t.Error("invalid actual CLI request JSON")
			http.Error(w, "invalid request", 400)
			return
		}
		var items []map[string]any
		if !invalid && requests == 1 {
			var shell nativeEditTool
			for _, name := range []string{"exec_command", "shell_command", "shell"} {
				if found, ok := nativeEditFindTool(realCLIArray(body["tools"]), name, ""); ok && found.Kind == "function" {
					shell = found
					break
				}
			}
			for _, name := range []string{"a", "b"} {
				item, err := shell.call("contract-"+name, "printf '%s\\n' 'CONTRACT_"+name+"_<>&你好' >> contract-"+name+".txt; cat contract-"+name+".txt")
				if err != nil {
					t.Error("real CLI tool schema unavailable")
					http.Error(w, "tool schema unavailable", 400)
					return
				}
				items = append(items, item)
			}
		} else if !invalid {
			checks["turn_state_echo"] = r.Header.Get("X-Codex-Turn-State") == "contract-route-state"
			seen := make(map[string]bool)
			for _, raw := range realCLIArray(body["input"]) {
				item, _ := raw.(map[string]any)
				if item["type"] != "function_call_output" {
					continue
				}
				id, _ := item["call_id"].(string)
				name := strings.TrimPrefix(id, "contract-")
				text := nativeEditOutputText(item["output"])
				code, parsed := nativeEditToolExit(text)
				if name != "a" && name != "b" || seen[id] || !parsed || code != 0 || !strings.Contains(text, "CONTRACT_"+name+"_<>&你好") {
					t.Error("actual CLI tool output was lost, duplicated or changed")
				} else {
					toolOutputs++
				}
				seen[id] = true
			}
			items = append(items, realCLIMessage("contract-final-item", "CONTROLLED_CONTRACT_COMPLETE"))
		}
		wire := cliContractWire(fmt.Sprintf("contract-response-%d", requests), items, invalid)
		expected = append(expected, append([]byte(nil), wire...))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "contract-route-state")
		_, _ = w.Write(wire)
	}))
	upstream.EnableHTTP2 = true
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	realGatewayNativePool(t, p)
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inbound = append(inbound, r.Header.Clone())
		mu.Unlock()
		tap := &cliContractTapWriter{ResponseWriter: w}
		p.ServeHTTP(tap, r)
		mu.Lock()
		received = append(received, append([]byte(nil), tap.Buffer.Bytes()...))
		mu.Unlock()
	}))
	t.Cleanup(gateway.Close)
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "contract_fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
model_auto_compact_token_limit = 1000000
[model_providers.contract_fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
http_headers = { "X-Oai-Attestation" = "synthetic-opaque-proof" }
`, gateway.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Run the controlled fixture tools and complete the contract check.")
	cmd.Dir = workspace
	for _, key := range []string{"PATH", "SHELL", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home, "CODEX_HOME="+filepath.Join(home, "codex"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"), "CODEX_GATEWAY_FIXTURE_KEY=test-client-key", "NO_PROXY=*", "no_proxy=*", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	mu.Lock()
	defer mu.Unlock()
	checks["cli_exit_matches_terminal"] = ctx.Err() == nil && (invalid && runErr != nil || !invalid && runErr == nil && bytes.Contains(stdout.Bytes(), []byte("CONTROLLED_CONTRACT_COMPLETE")))
	checks["exact_request_count_no_replay"] = invalid && requests == 1 && toolOutputs == 0 || !invalid && requests == 2 && toolOutputs == 2
	checks["raw_sse_bytes_unchanged"] = len(expected) == requests && len(received) == requests
	var hashes []string
	for i, raw := range expected {
		if i >= len(received) || !bytes.Equal(raw, received[i]) {
			checks["raw_sse_bytes_unchanged"] = false
		}
		sum := sha256.Sum256(raw)
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	checks["native_header_identity"] = len(inbound) == requests && len(outbound) == requests
	for i, header := range outbound {
		if i >= len(inbound) || !strings.HasPrefix(header.Get("User-Agent"), "codex_exec/"+clientVersion+" ") || header.Get("User-Agent") != inbound[i].Get("User-Agent") || header.Get("Originator") != inbound[i].Get("Originator") || header.Get("Originator") != "codex_exec" {
			checks["native_header_identity"] = false
		}
	}
	if !invalid {
		checks["two_actual_tools_executed_once"] = true
		for _, name := range []string{"a", "b"} {
			raw, err := os.ReadFile(filepath.Join(workspace, "contract-"+name+".txt"))
			checks["two_actual_tools_executed_once"] = checks["two_actual_tools_executed_once"] && err == nil && string(raw) == "CONTRACT_"+name+"_<>&你好\n"
		}
	}
	checks["no_go_fallback"] = guard.calls.Load() == 0
	checks["admission_released"] = p.inFlight("test") == 0
	page, err := p.Metrics.Logs(LogQuery{Size: 10})
	checks["terminal_metrics_correct"] = err == nil && len(page.Items) == requests
	for _, record := range page.Items {
		want := "ok"
		if invalid {
			want = "error"
		}
		checks["terminal_metrics_correct"] = checks["terminal_metrics_correct"] && record.Status == want && record.RequestType == "http_sse"
	}
	for name, pass := range checks {
		if !pass {
			t.Errorf("CLI contract audit failed: %s (requests=%d tool_outputs=%d stdout_bytes=%d stderr_bytes=%d)", name, requests, toolOutputs, stdout.Len(), stderr.Len())
		}
	}
	return map[string]any{"invalid_terminal": invalid, "passed": !t.Failed(), "requests": requests, "actual_tool_outputs": toolOutputs, "raw_response_sha256": hashes, "assertions": checks}
}
