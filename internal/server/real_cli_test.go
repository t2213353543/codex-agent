package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRealCodexCLI runs an explicitly selected, unmodified official CLI binary.
// The upstream is a deterministic local protocol fixture, NOT a live model.
// Set CODEX_REAL_CLI_BIN to opt in; no global installation or auth is changed.
func TestRealCodexCLI(t *testing.T) {
	bin := os.Getenv("CODEX_REAL_CLI_BIN")
	if bin == "" {
		t.Skip("set CODEX_REAL_CLI_BIN to the official Codex CLI executable to run protocol integration")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("official CLI version: %v: %s", err, version)
	}
	t.Logf("real client: %s; upstream: local deterministic fixture", strings.TrimSpace(string(version)))
	for _, tc := range []struct {
		name       string
		websockets bool
		compact    bool
	}{
		{name: "http_tool_and_resume"},
		{name: "websocket_fallback_tool_and_resume", websockets: true},
		{name: "auto_compact_and_resume", compact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runRealCodexConversation(t, bin, tc.websockets, tc.compact)
		})
	}
}

type realCLIRequest struct {
	path   string
	header http.Header
	body   map[string]any
}

func runRealCodexConversation(t *testing.T, bin string, websockets, compact bool) {
	t.Helper()
	runRealCodexConversationWithSandbox(t, bin, websockets, compact, false)
}

func runRealCodexConversationWithSandbox(t *testing.T, bin string, websockets, compact, readOnlyWindows bool) {
	t.Helper()
	const toolMarker = "CODEX_GATEWAY_REAL_TOOL_OK"
	firstPrompt := "Gateway integration: execute the requested harmless echo tool, then finish."
	toolCommand := "echo " + toolMarker
	if readOnlyWindows {
		firstPrompt = "Gateway integration: read only gateway-readonly-marker.txt in the current directory with the requested shell tool, then finish."
		toolCommand = "Get-Content -LiteralPath .\\gateway-readonly-marker.txt"
	}
	const firstReply = "CODEX_GATEWAY_FIRST_TURN_OK"
	const resumePrompt = "Continue the same session and remember the previous tool result."
	const resumeReply = "CODEX_GATEWAY_RESUME_OK"
	const summary = "CODEX_GATEWAY_COMPACTED_CONTEXT"
	var mu sync.Mutex
	var requests []realCLIRequest
	responseCount, upgradeCount := 0, 0
	p := newTestProxy(t, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			http.Error(w, "cannot read request", http.StatusBadRequest)
			return
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("decode upstream %s: %v (encoding %q)", r.URL.Path, err, r.Header.Get("Content-Encoding"))
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		requests = append(requests, realCLIRequest{r.URL.Path, r.Header.Clone(), parsed})
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("gateway did not replace client credential with fixture upstream credential")
		}
		switch r.URL.Path {
		case "/responses/compact":
			// Like the official source fixture, retain user/developer messages
			// and return one opaque compaction item; the client must replay it.
			output := []any{}
			for _, value := range realCLIArray(parsed["input"]) {
				item, _ := value.(map[string]any)
				if item["role"] == "user" || item["role"] == "developer" {
					output = append(output, item)
				}
			}
			output = append(output, map[string]any{"type": "compaction", "encrypted_content": summary})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "compact_fixture", "object": "response.compaction", "output": output})
		case "/responses":
			if realCLIHasCompactionTrigger(parsed) {
				if !compact {
					t.Error("CLI requested compaction outside the compact scenario")
				}
				triggers := 0
				for _, value := range realCLIArray(parsed["input"]) {
					if item, ok := value.(map[string]any); ok && item["type"] == "compaction_trigger" {
						triggers++
					}
				}
				if triggers != 1 {
					t.Errorf("V2 compaction request has %d trigger items, want one", triggers)
				}
				realCLIWriteSSE(w, "resp_compact_fixture_v2", map[string]any{"type": "compaction", "encrypted_content": summary}, 10)
				return
			}
			responseCount++
			id := fmt.Sprintf("resp_real_cli_%d", responseCount)
			var item map[string]any
			tokens := 10
			switch responseCount {
			case 1:
				name, namespace := realCLIShellTool(realCLIArray(parsed["tools"]), "")
				if name == "" {
					t.Errorf("real CLI did not advertise a supported shell tool: %s", realCLIJSON(parsed["tools"]))
					http.Error(w, "missing shell tool", http.StatusBadRequest)
					return
				}
				args := map[string]any{"cmd": toolCommand, "login": false, "max_output_tokens": 100}
				if name == "shell_command" {
					args = map[string]any{"command": toolCommand, "login": false}
				} else if name == "shell" {
					args = map[string]any{"command": []string{"echo", toolMarker}}
					if readOnlyWindows {
						args = map[string]any{"command": []string{"powershell.exe", "-NoProfile", "-Command", toolCommand}}
					}
				}
				item = map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture_echo", "name": name, "arguments": realCLIJSON(args)}
				if namespace != "" {
					item["namespace"] = namespace
				}
				if compact {
					tokens = 100000000 // Force the actual CLI's automatic compact path.
				}
			case 2:
				item = realCLIMessage("msg_first", firstReply)
			case 3:
				item = realCLIMessage("msg_resume", resumeReply)
			default:
				t.Errorf("unexpected extra response request %d", responseCount)
				http.Error(w, "unexpected response", http.StatusBadRequest)
				return
			}
			realCLIWriteSSE(w, id, item, tokens)
		default:
			t.Errorf("unexpected upstream route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			mu.Lock()
			upgradeCount++
			mu.Unlock()
		}
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	cliHome, workdir := t.TempDir(), t.TempDir()
	markerFile := filepath.Join(workdir, "gateway-readonly-marker.txt")
	if readOnlyWindows {
		if err := os.WriteFile(markerFile, []byte(toolMarker+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A custom provider reaches only the local gateway. Its OpenAI name is
	// intentional: the official client uses it to enable remote compaction.
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
supports_websockets = %t
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1", websockets)
	if readOnlyWindows {
		config = strings.Replace(config, `sandbox_mode = "danger-full-access"`, `sandbox_mode = "read-only"`, 1)
		config += "\n[windows]\nsandbox = \"unelevated\"\n"
	}
	if err := os.WriteFile(filepath.Join(cliHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	first := realCLIRun(t, bin, cliHome, workdir, "exec", "--skip-git-repo-check", "--json", firstPrompt)
	if !strings.Contains(first, firstReply) || !strings.Contains(first, toolMarker) {
		t.Fatalf("CLI did not complete the tool turn: %s", first)
	}
	threadID := ""
	toolExecuted := false
	for _, line := range strings.Split(first, "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event["type"] == "thread.started" {
			threadID, _ = event["thread_id"].(string)
		}
		if item, ok := event["item"].(map[string]any); ok && event["type"] == "item.completed" && item["type"] == "command_execution" {
			output, _ := item["aggregated_output"].(string)
			toolExecuted = item["exit_code"] == float64(0) && strings.Contains(output, toolMarker)
		}
	}
	if !toolExecuted {
		t.Fatalf("CLI did not report successful real shell execution: %s", first)
	}
	if threadID == "" {
		t.Fatalf("CLI did not emit a persisted thread ID: %s", first)
	}
	resumed := realCLIRun(t, bin, cliHome, workdir, "exec", "resume", "--skip-git-repo-check", "--json", threadID, resumePrompt)
	if !strings.Contains(resumed, resumeReply) || !strings.Contains(resumed, threadID) {
		t.Fatalf("CLI did not resume the same session: %s", resumed)
	}
	mu.Lock()
	defer mu.Unlock()
	if responseCount != 3 {
		t.Errorf("response requests = %d, want 3", responseCount)
	}
	if websockets && upgradeCount == 0 {
		t.Error("websocket-enabled CLI did not exercise the gateway's HTTP fallback")
	}
	legacyCompactCount, v2CompactCount, toolOutputCount := 0, 0, 0
	for i, request := range requests {
		if request.header.Get("Thread-Id") != threadID {
			t.Errorf("request %d thread-id = %q, want %q", i, request.header.Get("Thread-Id"), threadID)
		}
		for _, value := range realCLIArray(request.body["input"]) {
			item, _ := value.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == "call_fixture_echo" && strings.Contains(realCLIJSON(item["output"]), toolMarker) {
				toolOutputCount++
			}
		}
		if request.path == "/responses/compact" {
			legacyCompactCount++
			for _, forbidden := range []string{"stream", "store"} {
				if _, found := request.body[forbidden]; found {
					t.Errorf("compact request has responses-only field %s", forbidden)
				}
			}
		}
		if request.path == "/responses" && realCLIHasCompactionTrigger(request.body) {
			v2CompactCount++
		}
	}
	compactCount := legacyCompactCount + v2CompactCount
	if toolOutputCount == 0 {
		t.Error("real CLI tool output never reached the upstream through the gateway")
	}
	if compact && compactCount != 1 || !compact && compactCount != 0 {
		t.Errorf("compact requests = %d, compact enabled = %t", compactCount, compact)
	}
	last := realCLIJSON(requests[len(requests)-1].body["input"])
	for _, remembered := range []string{firstPrompt, firstReply, resumePrompt} {
		if !strings.Contains(last, remembered) {
			t.Errorf("resumed request lost previous conversation item %q", remembered)
		}
	}
	if compact && !strings.Contains(last, summary) {
		t.Error("resumed CLI did not retain the compacted context")
	}
	if !compact && !strings.Contains(last, toolMarker) {
		t.Error("resumed CLI did not retain the shell output")
	}
	if readOnlyWindows {
		contents, err := os.ReadFile(markerFile)
		if err != nil || string(contents) != toolMarker+"\n" {
			t.Fatalf("read-only marker was changed: read error=%v", err)
		}
		t.Log("Windows read-only/never with unelevated backend: workspace marker read, exit 0, marker unchanged, and resume verified")
	}
	t.Logf("thread %s: %d responses, %d compact requests (legacy=%d V2=%d), %d websocket attempts; tool execution and resume verified", threadID, responseCount, compactCount, legacyCompactCount, v2CompactCount, upgradeCount)
}

func realCLIRun(t *testing.T, bin, cliHome, workdir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	// Keep process-launch essentials, excluding the host's auth, app-server,
	// proxy and OpenAI/Codex configuration. CODEX_HOME is child-local only.
	for _, key := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "USERPROFILE", "HOME", "SHELL", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+cliHome, "CODEX_GATEWAY_FIXTURE_KEY=test-client-key", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("official CLI %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, &stdout, &stderr)
	}
	return stdout.String()
}

func realCLIArray(value any) []any {
	items, _ := value.([]any)
	return items
}

func realCLIHasCompactionTrigger(body map[string]any) bool {
	input := realCLIArray(body["input"])
	if len(input) == 0 {
		return false
	}
	last, _ := input[len(input)-1].(map[string]any)
	return last["type"] == "compaction_trigger"
}

func realCLIJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func realCLIShellTool(tools []any, namespace string) (string, string) {
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		name, _ := tool["name"].(string)
		if tool["type"] == "namespace" {
			if child, ns := realCLIShellTool(realCLIArray(tool["tools"]), name); child != "" {
				return child, ns
			}
		}
		if name == "exec_command" || name == "shell_command" || name == "shell" {
			return name, namespace
		}
	}
	return "", ""
}

func realCLIMessage(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func realCLIWriteSSE(w http.ResponseWriter, id string, item map[string]any, tokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id}},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": tokens, "output_tokens": 1, "total_tokens": tokens + 1}}},
	} {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], realCLIJSON(event))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}
