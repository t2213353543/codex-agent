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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This is an explicitly enabled capture driver, never a production endpoint.
// Its proxy records the actual official executable/native-helper egress. Only
// synthetic requests are used, and account credentials stay in the server's
// private fixture directory. HTTP errors are observations, not passing cases.
func TestAppServerParityCapture(t *testing.T) {
	proxyURL, ca := os.Getenv("APP_SERVER_PARITY_PROXY"), os.Getenv("APP_SERVER_PARITY_CA")
	destination, mode := os.Getenv("APP_SERVER_PARITY_OUTPUT"), os.Getenv("APP_SERVER_PARITY_ROUTE")
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if proxyURL == "" || ca == "" || destination == "" || file == "" || binary == "" || work == "" {
		t.Skip("explicit isolated outbound-capture environment required")
	}
	if mode != inferenceAppServer && mode != inferenceCodexCLI {
		t.Fatal("capture route must explicitly select a supported backend")
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		t.Fatal("capture proxy must be a local dedicated listener")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		t.Fatal("invalid capture listener")
	}
	if _, err := os.Stat(ca); err != nil {
		t.Fatal("capture CA missing")
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	t.Setenv("SSL_CERT_FILE", ca)
	credentials, err := os.ReadFile(file)
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if err != nil || json.Unmarshal(credentials, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("private account fixture unavailable")
	}
	account := fixture.Accounts[0]
	evidence := []map[string]any{}
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.RuntimeModes = true
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "isolated-outbound-capture", Type: "http", Host: "127.0.0.1", Port: port, Enabled: true})
	if err != nil {
		t.Fatal("capture proxy configuration failed")
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{proxy.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	p.CodexTransport = &Proxy{Client: newHTTPClient(), Proxies: p.Proxies}
	if err := configureGatewayNativeHTTP(p.CodexTransport, os.Getenv); err != nil {
		t.Fatal("native capture transport unavailable")
	}
	t.Cleanup(p.CodexTransport.closeGatewayClients)
	if os.Getenv("APP_SERVER_PARITY_LIGHTWEIGHT") == "1" {
		p.AppServerCLI = newAppServerCLI(p)
		t.Cleanup(p.AppServerCLI.Close)
		if err := p.AppServerCLI.configureIdentity(os.Getenv); err != nil {
			t.Fatal(err)
		}
	}
	p.Official, err = newOfficialRuntime(func(k string) string {
		if k == "CODEX_CLI_PATH" {
			return binary
		}
		if k == "CODEX_RUNTIME_DIR" {
			return filepath.Join(work, "capture-runtime")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Official.Close() })
	// A private launcher adds the test CA to officialEnvironment's whitelist.
	launcher := filepath.Join(destination, "codex-with-capture-ca")
	if strings.ContainsAny(binary+ca, "\n\r'") {
		t.Fatal("unsupported launcher path")
	}
	script := "#!/bin/sh\nexport CODEX_CA_CERTIFICATE='" + ca + "'\nexport SSL_CERT_FILE='" + ca + "'\nexec '" + binary + "' \"$@\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p.Official.bin = launcher
	settings, _ := p.Settings.Get()
	settings.InferenceMode = mode
	settings.BuiltinPromptEnabled = false
	settings.PromptCacheKeyEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	model := os.Getenv("APP_SERVER_PARITY_MODEL")
	if model == "" {
		model = "gpt-6-astra"
	}
	effort := os.Getenv("APP_SERVER_PARITY_EFFORT")
	if effort == "" {
		effort = "xhigh"
	}
	manifest := []map[string]any{}
	requestNumber := 0
	call := func(label, endpoint, session string, body map[string]any) map[string]json.RawMessage {
		t.Helper()
		requestNumber++
		requestBody, _ := json.Marshal(body)
		name := fmt.Sprintf("%02d-%s", requestNumber, label)
		if err := os.WriteFile(filepath.Join(destination, name+".input.json"), requestBody, 0600); err != nil {
			t.Fatal(err)
		}
		phase, _ := json.Marshal(map[string]any{"route": mode, "case": label, "request": requestNumber, "time": time.Now().UTC().Format(time.RFC3339Nano)})
		_ = os.WriteFile(filepath.Join(destination, "phase.json"), phase, 0600)
		method := http.MethodPost
		if body == nil {
			method = http.MethodGet
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, method, server.URL+"/v1"+endpoint, bytes.NewReader(requestBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Session-Id", session)
		started := time.Now()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("capture request transport failed")
		}
		raw, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		_ = os.WriteFile(filepath.Join(destination, name+".response"), raw, 0600)
		inputHash, responseHash := sha256.Sum256(requestBody), sha256.Sum256(raw)
		entry := map[string]any{"case": label, "route": mode, "endpoint": endpoint, "http": res.StatusCode, "elapsed_ms": time.Since(started).Milliseconds(), "input_sha256": hex.EncodeToString(inputHash[:]), "response_sha256": hex.EncodeToString(responseHash[:])}
		manifest = append(manifest, entry)
		encoded, _ := json.MarshalIndent(manifest, "", "  ")
		_ = os.WriteFile(filepath.Join(destination, "driver-summary.json"), encoded, 0600)
		t.Logf("route=%s case=%s endpoint=%s http=%d elapsed_ms=%d", mode, label, endpoint, res.StatusCode, time.Since(started).Milliseconds())
		if readErr != nil || res.StatusCode != 200 {
			t.Fatalf("capture case %s failed with HTTP %d", label, res.StatusCode)
		}
		var response map[string]json.RawMessage
		if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
			var observedOutput []json.RawMessage
			for _, line := range bytes.Split(raw, []byte("\n")) {
				if !bytes.HasPrefix(line, []byte("data: ")) {
					continue
				}
				var event struct {
					Type     string
					Response json.RawMessage
					Item     json.RawMessage
				}
				_ = json.Unmarshal(line[6:], &event)
				if event.Type == "error" || event.Type == "response.failed" {
					t.Fatalf("capture case %s has failed terminal", label)
				}
				if event.Type == "response.completed" {
					_ = json.Unmarshal(event.Response, &response)
				}
				if event.Type == "response.output_item.done" && len(event.Item) > 0 {
					observedOutput = append(observedOutput, append(json.RawMessage(nil), event.Item...))
				}
			}
			// Direct Responses streams may omit output in the final usage event.
			// The simulated client restores history from observed item.done frames;
			// the captured wire response remains unmodified on disk.
			var terminalOutput []json.RawMessage
			if response != nil && len(observedOutput) > 0 && (json.Unmarshal(response["output"], &terminalOutput) != nil || len(terminalOutput) == 0) {
				response["output"], _ = json.Marshal(observedOutput)
			}
		} else {
			_ = json.Unmarshal(raw, &response)
		}
		if response == nil {
			t.Fatalf("capture case %s omitted response", label)
		}
		return response
	}
	body := func(input []json.RawMessage) map[string]any {
		return map[string]any{"model": model, "reasoning": map[string]string{"effort": effort}, "instructions": "Complete only the supplied request. Use tools only when explicitly requested.", "input": input, "stream": true, "store": false, "include": []string{"reasoning.encrypted_content"}}
	}
	user := func(text string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"role": "user", "content": text})
		return raw
	}
	appendOutput := func(history []json.RawMessage, response map[string]json.RawMessage) []json.RawMessage {
		var output []json.RawMessage
		if json.Unmarshal(response["output"], &output) != nil {
			t.Fatal("missing capture output")
		}
		return append(history, output...)
	}
	call("models", "/models", "capture-models", nil)
	history := []json.RawMessage{user("Remember the marker PARITY_MARKER. Reply exactly SAVED without tools.")}
	response := call("text", "/responses", "capture-text", body(history))
	history = appendOutput(history, response)
	history = append(history, user("Reply exactly the remembered marker without tools."))
	response = call("resume", "/responses", "capture-text", body(history))
	history = appendOutput(history, response)
	if os.Getenv("APP_SERVER_PARITY_PILOT") == "1" {
		return
	}
	request := body(history)
	request["stream"] = false
	compacted := call("compact", "/responses/compact", "capture-text", request)
	var compressed []json.RawMessage
	if json.Unmarshal(compacted["output"], &compressed) != nil {
		t.Fatal("compact output missing")
	}
	call("compact-resume", "/responses", "capture-text", body(append(compressed, user("Reply exactly the remembered marker without tools."))))
	history = []json.RawMessage{user("Call lookup_marker once, then reply only with its returned text.")}
	tools := []any{map[string]any{"type": "function", "name": "lookup_marker", "description": "Retrieve the marker.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
	request = body(history)
	request["tools"] = tools
	response = call("function-call", "/responses", "capture-function", request)
	history = appendOutput(history, response)
	var calls []map[string]json.RawMessage
	_ = json.Unmarshal(response["output"], &calls)
	callID := ""
	for _, item := range calls {
		if string(item["type"]) == `"function_call"` {
			_ = json.Unmarshal(item["call_id"], &callID)
		}
	}
	if callID == "" {
		t.Fatal("required function was not called")
	}
	result, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": callID, "output": "PARITY_TOOL_MARKER"})
	history = append(history, result)
	request = body(history)
	request["tools"] = tools
	call("function-result", "/responses", "capture-function", request)
	request = body([]json.RawMessage{user("Search OpenAI official documentation for the Codex CLI installation command. Give the command and one source URL. Do not run shell commands.")})
	request["tools"] = []any{map[string]string{"type": "web_search"}}
	if os.Getenv("APP_SERVER_PARITY_LIGHTWEIGHT") != "1" {
		call("web-search", "/responses", "capture-search", request)
		return
	}
	searchRequest := request
	searchRequest["tool_choice"] = "required"
	request = body([]json.RawMessage{user("Return an object with marker equal to STRUCTURED_MARKER.")})
	request["stream"] = false
	request["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "client_marker", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"marker": map[string]string{"type": "string"}}, "required": []string{"marker"}, "additionalProperties": false}}}
	structured := call("structured-output", "/responses", "capture-structured", request)
	if !bytes.Contains(structured["output"], []byte("STRUCTURED_MARKER")) {
		t.Fatal("structured output marker missing")
	}
	request = body([]json.RawMessage{user("Call lookup_marker exactly once.")})
	request["tools"] = tools
	request["tool_choice"] = map[string]string{"type": "function", "name": "lookup_marker"}
	required := call("required-function", "/responses", "capture-required", request)
	if !bytes.Contains(required["output"], []byte(`"function_call"`)) {
		t.Fatal("explicit required function missing")
	}
	history = []json.RawMessage{user("Call emit_marker with EXACT_MARKER, then reply with the result of the tool.")}
	customTools := []any{map[string]any{"type": "custom", "name": "emit_marker", "description": "Emit the marker text.", "format": map[string]string{"type": "grammar", "syntax": "lark", "definition": "start: \"EXACT_MARKER\""}}}
	request = body(history)
	request["tools"] = customTools
	request["tool_choice"] = "required"
	custom := call("custom-tool", "/responses", "capture-custom", request)
	var customItems []map[string]json.RawMessage
	_ = json.Unmarshal(custom["output"], &customItems)
	customCall := ""
	for _, item := range customItems {
		if string(item["type"]) == `"custom_tool_call"` {
			_ = json.Unmarshal(item["call_id"], &customCall)
			var input string
			_ = json.Unmarshal(item["input"], &input)
			if input != "EXACT_MARKER" {
				t.Fatal("custom grammar input changed")
			}
		}
	}
	if customCall == "" {
		t.Fatal("custom tool not called")
	}
	history = appendOutput(history, custom)
	customResult, _ := json.Marshal(map[string]string{"type": "custom_tool_call_output", "call_id": customCall, "output": "CUSTOM_TOOL_RESULT"})
	history = append(history, customResult)
	request = body(history)
	request["tools"] = customTools
	customReply := call("custom-result", "/responses", "capture-custom", request)
	if !bytes.Contains(customReply["output"], []byte("CUSTOM_TOOL_RESULT")) {
		t.Fatal("custom result was not used")
	}
	request = body([]json.RawMessage{user("Call ops.lookup_marker once.")})
	request["tools"] = []any{map[string]any{"type": "namespace", "name": "ops", "description": "Marker operations", "tools": tools}}
	namespaced := call("namespace-function", "/responses", "capture-namespace", request)
	if !bytes.Contains(namespaced["output"], []byte(`"namespace":"ops"`)) {
		t.Fatal("explicit tool namespace lost")
	}
	p.Official.mu.Lock()
	hostedProcesses := len(p.Official.processes)
	p.Official.mu.Unlock()
	if hostedProcesses != 0 {
		t.Fatalf("lightweight profile launched %d official processes", hostedProcesses)
	}
	searchResponse := call("web-search", "/responses", "capture-search", searchRequest)
	if !bytes.Contains(searchResponse["output"], []byte(`"web_search_call"`)) {
		t.Fatal("no actual web search call observed")
	}
	alpha := call("native-web-search", "/alpha/search", "capture-native-search", map[string]any{
		"id": "capture-native-search", "model": model, "input": "Find the OpenAI Codex CLI installation command.",
		"commands": map[string]any{"search_query": []any{map[string]any{"q": "Codex CLI installation npm install OpenAI", "domains": []string{"developers.openai.com", "help.openai.com"}}}, "response_length": "short"},
	})
	var searchText string
	if json.Unmarshal(alpha["output"], &searchText) != nil || strings.TrimSpace(searchText) == "" {
		t.Fatal("native search returned no tool output")
	}
}
