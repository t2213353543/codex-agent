package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Real official App Server / exec -> actual Go gateway -> HTTP fixture. The
// only substituted boundary is the gateway's already-built upstream URL,
// after its fixed official host/path and selected credentials are checked.
// Native TLS/IPC has separate candidate-bound integration tests.
func TestAppServerDelegatedClientActualIntegration(t *testing.T) {
	binary := os.Getenv("CODEX_NATIVE_CAPTURE_BIN")
	driver := os.Getenv("CODEX_CLIENT_PROFILE_DRIVER")
	if runtime.GOOS != "linux" || binary == "" || driver == "" {
		t.Skip("set pinned CODEX_NATIVE_CAPTURE_BIN and CODEX_CLIENT_PROFILE_DRIVER inside an isolated Linux network namespace")
	}
	network, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.Readlink("/proc/1/ns/net")
	if err != nil || network == initial {
		t.Fatal("fresh network namespace required")
	}
	actual, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(actual)
	if hex.EncodeToString(digest[:]) != "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501" {
		t.Fatal("official CLI pin mismatch")
	}
	for _, route := range []string{"app", "exec"} {
		t.Run(route, func(t *testing.T) { appServerDelegatedClientActualCase(t, binary, driver, route) })
	}
}

func appServerDelegatedClientActualCase(t *testing.T, binary, driver, route string) {
	t.Helper()
	p := appServerIdentityBridgeFixture(t)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Concurrency = 50; a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	p.AppServerCLI = newAppServerCLI(p)
	t.Cleanup(p.AppServerCLI.Close)
	var mu sync.Mutex
	counts, incomingCounts := map[string]int{}, map[string]int{}
	modelCalls, toolCalls, imageCalls, historyCalls := 0, 0, 0, 0
	appResult, historyResult := false, false
	var otherAdded atomic.Bool
	jsonReply := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		counts[r.URL.Path]++
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("gateway leaked its key or moved delegated identity to another account")
			w.WriteHeader(403)
			return
		}
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1))
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer decoder.Close()
			reader = decoder
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/backend-api/codex/models":
			// A real parseable ModelInfo lets the official client select its
			// Lite writer and owned tool context, without a gateway rewrite.
			jsonReply(w, map[string]any{"models": []any{map[string]any{
				"slug": "mock-model", "display_name": "Synthetic gateway model", "description": "loopback only", "default_reasoning_level": "medium",
				"supported_reasoning_levels": []any{map[string]string{"effort": "medium", "description": "fixture"}},
				"shell_type":                 "disabled", "visibility": "list", "supported_in_api": true, "priority": 0,
				"model_messages":    map[string]any{"instructions_template": "Complete the synthetic fixture tool sequence."},
				"support_verbosity": true, "default_verbosity": "low", "default_reasoning_summary": "none",
				"truncation_policy": map[string]any{"mode": "bytes", "limit": 10000}, "experimental_supported_tools": []string{},
				"use_responses_lite": true, "tool_mode": "direct", "context_window": 200000,
			}}})
		case "/backend-api/wham/settings/user":
			jsonReply(w, map[string]any{})
		case "/backend-api/connectors/directory/list":
			jsonReply(w, map[string]any{"apps": []any{}, "next_token": nil})
		case "/backend-api/ps/mcp":
			if r.Header.Get("X-OpenAI-Product-Sku") != "codex" {
				t.Error("MCP product sku header changed")
			}
			var envelope struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(raw, &envelope) != nil {
				t.Error("invalid MCP JSON")
				w.WriteHeader(400)
				return
			}
			if len(envelope.ID) == 0 {
				w.WriteHeader(202)
				return
			}
			var result any = map[string]any{}
			switch envelope.Method {
			case "initialize":
				var params struct {
					Version string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(envelope.Params, &params)
				result = map[string]any{"protocolVersion": params.Version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "gateway-profile-upstream", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "echo_marker", "description": "Return a synthetic gateway marker.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}, "annotations": map[string]bool{"readOnlyHint": true, "openWorldHint": false}, "_meta": map[string]string{"connector_id": "connector_profile_fixture", "connector_name": "profile_fixture", "connector_description": "Synthetic gateway profile fixture"}}}}
			case "tools/call":
				toolCalls++
				result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "SYNTHETIC_APP_TOOL_RESULT"}}, "isError": false}
			}
			jsonReply(w, map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
		case "/backend-api/codex/images/generations":
			imageCalls++
			jsonReply(w, map[string]any{"created": 1, "background": "opaque", "data": []any{map[string]string{"b64_json": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==", "generation_id": "fixture-image"}}})
		case "/backend-api/codex/alpha/notes/v2/thread_hint":
			jsonReply(w, map[string]string{"text": ""})
		case "/backend-api/codex/alpha/history/v2/list_windows":
			historyCalls++
			jsonReply(w, map[string]any{"windows": []any{}, "fixture": "SYNTHETIC_HISTORY_RESULT"})
		case "/backend-api/codex/responses":
			modelCalls++
			appResult = appResult || bytes.Contains(raw, []byte("SYNTHETIC_APP_TOOL_RESULT"))
			historyResult = historyResult || bytes.Contains(raw, []byte("SYNTHETIC_HISTORY_RESULT"))
			var item any
			switch modelCalls {
			case 1:
				item = map[string]any{"type": "function_call", "id": "fc-profile", "call_id": "call-profile", "namespace": "image_gen", "name": "imagegen", "arguments": `{"prompt":"SYNTHETIC_CLIENT_PROFILE_IMAGE"}`}
			case 2:
				item = map[string]any{"type": "function_call", "id": "fc-app", "call_id": "call-app", "namespace": "mcp__codex_apps__profile_fixture", "name": "echo_marker", "arguments": "{}"}
			case 3:
				item = map[string]any{"type": "function_call", "id": "fc-history", "call_id": "call-history", "namespace": "history", "name": "list_windows", "arguments": "{}"}
			default:
				item = map[string]any{"type": "message", "id": "msg-profile", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "CLIENT_PROFILE_DONE"}}}
			}
			id := fmt.Sprintf("resp-gateway-profile-%d", modelCalls)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []any{map[string]any{"type": "response.created", "response": map[string]string{"id": id}}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}} {
				encoded, _ := json.Marshal(event)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
			}
		default:
			t.Errorf("unexpected actual upstream route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "chatgpt.com" || !strings.HasPrefix(r.URL.Path, "/backend-api/") {
			return nil, fmt.Errorf("nonofficial gateway target")
		}
		request := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		request.URL = &address
		request.Host = target.Host
		return transport.RoundTrip(request)
	})}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-client-key" {
			t.Error("official client sent a non-gateway credential")
		}
		mu.Lock()
		incomingCounts[r.URL.Path]++
		mu.Unlock()
		p.ServeHTTP(w, r)
		if r.URL.Path == appServerClientIdentityPath && otherAdded.CompareAndSwap(false, true) {
			// A later, higher-priority pool account must not replace the owner
			// whose metadata the official client already hydrated.
			raw, _ := json.Marshal(map[string]any{"access_token": "unrelated-upstream-token", "account_id": "unrelated-account", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("unrelated", raw, AccountDefaults{Concurrency: 50, Priority: 1000}); err != nil {
				t.Error(err)
			}
			p.invalidateAccounts()
		}
	}))
	defer gateway.Close()
	output := filepath.Join(t.TempDir(), "driver-output")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", "-B", driver, "--isolated-net", "--external-gateway", gateway.URL, "--codex", binary, "--output", output, "--route", route)
	// Explicitly select environment; never discover real credentials or a
	// workstation's ordinary Codex home when running the official process.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	log, err := command.CombinedOutput()
	if err != nil {
		for _, name := range []string{"summary.json", "stderr.log", "events.jsonl"} {
			if raw, readErr := os.ReadFile(filepath.Join(output, route, name)); readErr == nil {
				t.Logf("%s: %s", name, raw)
			}
		}
		t.Fatalf("official %s client through gateway failed: %v %s", route, err, log)
	}
	var result struct {
		Passed bool `json:"passed"`
	}
	if json.Unmarshal(log, &result) != nil || !result.Passed {
		t.Fatalf("client driver failed: %s", log)
	}
	mu.Lock()
	defer mu.Unlock()
	if modelCalls != 4 || toolCalls != 1 || imageCalls != 1 || historyCalls != 1 || !appResult || !historyResult {
		t.Fatalf("incomplete actual tool chain: models=%d mcp=%d images=%d history=%d app_result=%v history_result=%v", modelCalls, toolCalls, imageCalls, historyCalls, appResult, historyResult)
	}
	for _, path := range []string{appServerClientIdentityPath, "/backend-api/codex/models", "/backend-api/codex/responses", "/backend-api/codex/images/generations", "/backend-api/ps/mcp", "/backend-api/codex/alpha/history/v2/list_windows", "/backend-api/codex/alpha/notes/v2/thread_hint"} {
		if incomingCounts[path] == 0 {
			t.Errorf("official client did not reach gateway route %s", path)
		}
	}
	if !otherAdded.Load() {
		t.Fatal("owner pin was not exercised")
	}
	t.Logf("official %s client, complete tool sequence, fixed delegated owner: inbound=%v upstream=%v", route, incomingCounts, counts)
}
