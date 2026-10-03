package server

// Opt-in live verification. No credential fixture is embedded, discovered or
// refreshed. This test only runs when the operator explicitly supplies both
// credential-file and official Codex-binary paths. Normal go test skips it.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

type bpsLiveAccount struct {
	Alias       string `json:"alias"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
}

func bpsLiveWriteEvidence(path, transport string, passed bool, results []map[string]any) error {
	suitePassed := passed
	all := map[string]any{"passed": passed, "suites": []any{}}
	if data, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(data, &all); err != nil {
			return err
		}
		if previous, ok := all["passed"].(bool); !ok || !previous {
			passed = false
		}
	}
	suites, _ := all["suites"].([]any)
	suites = append(suites, map[string]any{"transport": transport, "passed": suitePassed, "results": results})
	all["suites"] = suites
	all["passed"] = passed
	encoded, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0600)
}

type bpsLiveHTTPRecord struct {
	Case                   string   `json:"case"`
	Path                   string   `json:"path"`
	Status                 int      `json:"status"`
	InputTypes             []string `json:"input_types,omitempty"`
	ToolResults            int      `json:"tool_results"`
	ToolCalls              []string `json:"tool_calls,omitempty"`
	HasEncryptedCompaction bool     `json:"has_encrypted_compaction"`
	PatchCall              bool     `json:"patch_call"`
	Completed              bool     `json:"completed"`
	WebSocketUpgrade       bool     `json:"websocket_upgrade,omitempty"`
}

type bpsLiveRecorder struct {
	mu            sync.Mutex
	caseName      string
	posts         int
	records       []bpsLiveHTTPRecord
	p             *Proxy
	requireWS     bool
	upstreamPosts int
	metricAfterID int64
}

type bpsLiveWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *bpsLiveWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *bpsLiveWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(data) <= 8<<20 {
		_, _ = w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}
func (w *bpsLiveWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *bpsLiveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *bpsLiveWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (r *bpsLiveRecorder) begin(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caseName = name
	r.posts = 0
	r.upstreamPosts = 0
	if r.p != nil && r.p.Metrics != nil {
		if last, ok := r.p.Metrics.LastFor("test"); ok {
			r.metricAfterID = last.ID
		}
	}
}
func (r *bpsLiveRecorder) forCase(name string) []bpsLiveHTTPRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []bpsLiveHTTPRecord
	for _, record := range r.records {
		if record.Case == name {
			out = append(out, record)
		}
	}
	return out
}
func (r *bpsLiveRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	name := r.caseName
	if req.Method == http.MethodPost {
		r.posts++
	}
	exceeded := r.posts > 10
	r.mu.Unlock()
	if exceeded {
		http.Error(w, "live_request_limit", 429)
		return
	}
	if r.requireWS && req.Method == http.MethodPost && req.URL.Path == "/v1/responses" {
		http.Error(w, "live_ws_fallback_forbidden", http.StatusConflict)
		return
	}
	if req.URL.Path != "/v1/responses" && req.URL.Path != "/v1/responses/compact" && req.URL.Path != "/v1/models" && !(r.p.RuntimeModes && strings.HasPrefix(req.URL.Path, "/v1/codex/")) {
		http.Error(w, "live_route_not_allowed", 404)
		return
	}
	upgrade := req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
	record := bpsLiveHTTPRecord{Case: name, Path: req.URL.Path, WebSocketUpgrade: upgrade}
	if req.Body != nil && !upgrade {
		body, e := io.ReadAll(io.LimitReader(req.Body, (16<<20)+1))
		_ = req.Body.Close()
		if e != nil || len(body) > 16<<20 {
			http.Error(w, "live_body_limit", 413)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Input []struct {
				Type string `json:"type"`
			}
		}
		if json.Unmarshal(body, &request) == nil {
			for _, item := range request.Input {
				record.InputTypes = append(record.InputTypes, item.Type)
				if item.Type == "function_call_output" || item.Type == "custom_tool_call_output" {
					record.ToolResults++
				}
			}
		}
	}
	writer := &bpsLiveWriter{ResponseWriter: w}
	defer func() {
		record.Status = writer.status
		bpsLiveInspectResponse(writer.body.Bytes(), &record)
		r.mu.Lock()
		r.records = append(r.records, record)
		r.mu.Unlock()
	}()
	r.p.ServeHTTP(writer, req)
}

func bpsLiveInspectResponse(body []byte, record *bpsLiveHTTPRecord) {
	var inspectItem func(json.RawMessage)
	inspectItem = func(raw json.RawMessage) {
		var item struct{ Type, Name, Namespace, Arguments, Input, EncryptedContent string }
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return
		}
		_ = json.Unmarshal(fields["type"], &item.Type)
		_ = json.Unmarshal(fields["name"], &item.Name)
		_ = json.Unmarshal(fields["namespace"], &item.Namespace)
		_ = json.Unmarshal(fields["arguments"], &item.Arguments)
		_ = json.Unmarshal(fields["input"], &item.Input)
		_ = json.Unmarshal(fields["encrypted_content"], &item.EncryptedContent)
		if item.Type == "compaction" && item.EncryptedContent != "" {
			record.HasEncryptedCompaction = true
		}
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			name := item.Namespace + "/" + item.Name
			found := false
			for _, previous := range record.ToolCalls {
				if previous == name {
					found = true
				}
			}
			if !found {
				record.ToolCalls = append(record.ToolCalls, name)
			}
			if item.Name == "apply_patch" || strings.Contains(item.Input, "tools.apply_patch(") || strings.Contains(item.Arguments, "tools.apply_patch(") {
				record.PatchCall = true
			}
		}
		if item.Type == "web_search_call" {
			found := false
			for _, previous := range record.ToolCalls {
				if previous == "native/web_search" {
					found = true
				}
			}
			if !found {
				record.ToolCalls = append(record.ToolCalls, "native/web_search")
			}
		}
	}
	inspectResponse := func(raw json.RawMessage) {
		var value struct {
			Output []json.RawMessage `json:"output"`
			Status string            `json:"status"`
		}
		if json.Unmarshal(raw, &value) == nil {
			for _, item := range value.Output {
				inspectItem(item)
			}
			if value.Status == "completed" {
				record.Completed = true
			}
		}
	}
	if json.Valid(body) {
		inspectResponse(body)
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type           string `json:"type"`
			Item, Response json.RawMessage
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		if len(event.Item) > 0 {
			inspectItem(event.Item)
		}
		if event.Type == "response.completed" {
			record.Completed = true
			inspectResponse(event.Response)
		}
	}
}

type bpsLiveCLIResult struct {
	Completed bool     `json:"completed"`
	TimedOut  bool     `json:"timed_out"`
	ExitCode  int      `json:"exit_code"`
	Final     []string `json:"final"`
	ItemTypes []string `json:"item_types"`
	Errors    []string `json:"errors,omitempty"`
	Thread    string   `json:"-"`
	Stderr    string   `json:"stderr,omitempty"`
}

type bpsLiveHarness struct {
	t                  *testing.T
	binary, work, home string
	env                []string
	recorder           *bpsLiveRecorder
	secrets            []string
	evidence           *[]map[string]any
}

var bpsLiveJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var bpsLiveEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func (h *bpsLiveHarness) scrub(s string) string {
	for _, secret := range h.secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	s = bpsLiveJWT.ReplaceAllString(s, "[redacted-jwt]")
	return bpsLiveEmail.ReplaceAllString(s, "[redacted-email]")
}
func (h *bpsLiveHarness) log(name string, result bpsLiveCLIResult, passed bool) {
	for i := range result.Final {
		result.Final[i] = h.scrub(result.Final[i])
	}
	result.Stderr = h.scrub(result.Stderr)
	value := map[string]any{"case": name, "passed": passed, "cli": result, "http": h.recorder.forCase(name), "gateway_metrics": h.diagnosticMetrics(), "model": "gpt-6-astra", "effort": "xhigh", "credential_in_cli": false}
	*h.evidence = append(*h.evidence, value)
	encoded, _ := json.Marshal(value)
	h.t.Log(string(encoded))
}

var bpsLiveSafeTerminalError = regexp.MustCompile(`^bps response terminated with (error|response\.(failed|incomplete|cancelled)|basispoints\.response\.cancelled) \(status (unreported|failed|incomplete|cancelled)\)( \(code=[A-Za-z0-9_]+\))?( \(error_type=(server_error|invalid_request_error|rate_limit_error|authentication_error)\))?$`)

func bpsLiveDiagnosticError(value string) string {
	if value == "" || bpsLiveSafeTerminalError.MatchString(value) {
		return value
	}
	for _, prefix := range []string{
		"bps stream ended without a complete terminal event", "bps stream contains an invalid event",
		"bps response identity changed during the stream", "bps stream event exceeds the configured limit",
		"context canceled", "context deadline exceeded", "parallel tool calls are disabled",
	} {
		if strings.HasPrefix(value, prefix) {
			return prefix
		}
	}
	return "private upstream diagnostic omitted"
}

func (h *bpsLiveHarness) diagnosticMetrics() []map[string]any {
	h.recorder.mu.Lock()
	after := h.recorder.metricAfterID
	h.recorder.mu.Unlock()
	var records []map[string]any
	for _, record := range h.recorder.p.Metrics.RecentForAccount("test", 500) {
		if record.ID <= after {
			continue
		}
		digest := sha256.Sum256([]byte(record.Error))
		records = append(records, map[string]any{
			"request_type": record.RequestType, "status": record.Status, "http_code": record.HTTPCode,
			"terminal_status": record.TerminalStatus, "error_source": record.ErrorSource, "error_phase": record.ErrorPhase,
			"error": bpsLiveDiagnosticError(record.Error), "error_sha256": hex.EncodeToString(digest[:]),
			"attempts": record.Attempts, "latency_ms": record.LatencyMs,
		})
	}
	return records
}
func (h *bpsLiveHarness) run(name, prompt string, args ...string) bpsLiveCLIResult {
	h.recorder.begin(name)
	ctx, cancel := context.WithTimeout(context.Background(), 590*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.binary, args...)
	command.Dir = h.work
	command.Env = h.env
	command.Stdin = strings.NewReader(prompt)
	command.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := bpsLiveCLIResult{ExitCode: 0, TimedOut: ctx.Err() != nil}
	if err != nil {
		result.ExitCode = -1
		if exit, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exit.ExitCode()
		}
	}
	result.Stderr = stderr.String()
	if len(result.Stderr) > 3000 {
		result.Stderr = result.Stderr[len(result.Stderr)-3000:]
	}
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	failed := false
	for scanner.Scan() {
		var event struct {
			Type, ThreadID string
			Item           struct{ Type, Text string }
		}
		var value map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		_ = json.Unmarshal(value["type"], &event.Type)
		_ = json.Unmarshal(value["thread_id"], &event.ThreadID)
		_ = json.Unmarshal(value["item"], &event.Item)
		if event.Type == "thread.started" {
			result.Thread = event.ThreadID
		}
		if event.Type == "turn.completed" {
			result.Completed = true
		}
		if event.Type == "turn.failed" || event.Type == "error" {
			failed = true
			var message string
			_ = json.Unmarshal(value["message"], &message)
			if message == "" {
				var detail struct{ Message string }
				_ = json.Unmarshal(value["error"], &detail)
				message = detail.Message
			}
			if message != "" && len(result.Errors) < 8 {
				if len(message) > 3000 {
					message = message[:3000]
				}
				result.Errors = append(result.Errors, h.scrub(message))
			}
		}
		if event.Type == "item.completed" {
			result.ItemTypes = append(result.ItemTypes, event.Item.Type)
			if event.Item.Type == "agent_message" {
				result.Final = append(result.Final, event.Item.Text)
			}
		}
	}
	result.Completed = result.Completed && !failed && result.ExitCode == 0 && !result.TimedOut
	return result
}

func bpsLiveNoTools(result bpsLiveCLIResult) bool {
	for _, kind := range result.ItemTypes {
		if kind == "command_execution" || kind == "tool_call" || kind == "mcp_tool_call" || kind == "file_change" || kind == "web_search" {
			return false
		}
	}
	return true
}
func bpsLiveHasText(result bpsLiveCLIResult, text string) bool {
	for _, answer := range result.Final {
		if strings.TrimSpace(answer) == text {
			return true
		}
	}
	return false
}

const bpsLiveCLIConfig = `model="gpt-6-astra"
model_provider="powerpoint_live"
model_reasoning_effort="xhigh"
model_context_window=300000
model_auto_compact_token_limit=200000
approval_policy="never"
sandbox_mode="workspace-write"
web_search="disabled"
check_for_update_on_startup=false
suppress_unstable_features_warning=true
project_doc_max_bytes=0
[analytics]
enabled=false
[feedback]
enabled=false
[skills]
include_instructions=false
[skills.bundled]
enabled=false
[features]
code_mode=false
js_repl=false
multi_agent=false
multi_agent_v2=false
multi_agent_mode=false
apps=false
enable_mcp_apps=false
hooks=false
plugin_hooks=false
plugins=false
remote_plugin=false
skill_search=false
[sandbox_workspace_write]
network_access=false
[model_providers.powerpoint_live]
name="OpenAI"
base_url=%q
env_key="BPS_LIVE_LOCAL_KEY"
wire_api="responses"
supports_websockets=false
request_max_retries=0
stream_max_retries=0
stream_idle_timeout_ms=300000
`

func newBPSLiveHarness(t *testing.T, binary, workRoot string, account bpsLiveAccount, secrets []string, evidence *[]map[string]any) *bpsLiveHarness {
	t.Helper()
	p := newTestProxy(t, 4)
	p.BPS = &bpsRuntime{ToolsVersion: bps.DefaultToolsVersion}
	p.BaseURL = bps.BaseURL
	// No refresh token and no live refresh/401-recovery path is available.
	if p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccessToken = account.AccessToken
		a.RefreshToken = ""
		a.AccountID = account.AccountID
		a.Identity = nil
		a.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
		a.Enabled = true
		return nil
	}) != nil {
		t.Fatal("live_fixture_store_failed")
	}
	settings, e := p.Settings.Get()
	if e != nil {
		t.Fatal("live_fixture_settings_failed")
	}
	settings.AdminAPI = true
	settings.WebsocketEnabled = true
	settings.BuiltinPromptEnabled = false
	// These acceptance cases validate actual accounting parity. The separate
	// billing compatibility case verifies the enabled client projection.
	settings.CacheCreationAsReadEnabled = false
	settings.StickySessionsEnabled = true
	settings.AutomaticTokenRefreshEnabled = false
	settings.Upstream401RecoveryEnabled = false
	settings.HTTPStreamEnabled = true
	settings.HTTPJSONEnabled = true
	settings.CompactEnabled = true
	settings.ModelsEnabled = true
	settings.UpstreamStreamIdleTimeoutSeconds = 300
	settings.UpstreamResponseHeaderTimeoutSeconds = 180
	settings.UpstreamCompactTimeoutSeconds = 540
	if p.Settings.Set(settings) != nil {
		t.Fatal("live_fixture_settings_failed")
	}
	recorder := &bpsLiveRecorder{p: p}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	directory, e := os.MkdirTemp(workRoot, "bps-cli-live-")
	if e != nil {
		t.Fatal("live_workspace_create_failed")
	}
	resolved, e := filepath.EvalSymlinks(directory)
	if e != nil || !strings.HasPrefix(resolved, workRoot+string(filepath.Separator)) {
		t.Fatal("live_workspace_boundary_failed")
	}
	t.Cleanup(func() { _ = os.RemoveAll(resolved) })
	work := filepath.Join(resolved, "workspace")
	home := filepath.Join(resolved, "codex-home")
	if os.Mkdir(work, 0700) != nil || os.Mkdir(home, 0700) != nil {
		t.Fatal("live_workspace_create_failed")
	}
	if os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(bpsLiveCLIConfig, server.URL+"/v1")), 0600) != nil {
		t.Fatal("live_config_write_failed")
	}
	var environment []string
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	environment = append(environment, "CODEX_HOME="+home, "BPS_LIVE_LOCAL_KEY=test-client-key", "NO_PROXY=127.0.0.1,localhost", "NO_COLOR=1", "RUST_LOG=off")
	return &bpsLiveHarness{t: t, binary: binary, work: work, home: home, env: environment, recorder: recorder, secrets: secrets, evidence: evidence}
}

const bpsLiveCandyPrompt = `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。参赛者需要在活动前决定摸出的糖果数目，那么最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？圆形苹果味匹配五角星桃子味，或者圆形桃子味匹配五角星苹果味，都满足要求。
数量：圆形 苹果味7 桃子味9 西瓜味8；五角星形 苹果味7 桃子味6 西瓜味4。
请直接在对话中给出答案与推导，不调用任何工具。最后独立一行写 FINAL_ANSWER=整数。`

func TestBPSLiveCodexIntegration(t *testing.T) {
	credentialsPath, binary := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY")
	if credentialsPath == "" || binary == "" {
		t.Skip("explicit BPS_LIVE_ACCOUNTS_FILE and BPS_LIVE_CODEX_BINARY are required")
	}
	workRoot := os.Getenv("BPS_LIVE_WORK_ROOT")
	if workRoot == "" {
		t.Fatal("live_work_root_required")
	}
	workRoot, e := filepath.Abs(workRoot)
	if e != nil {
		t.Fatal("live_work_root_invalid")
	}
	workRoot, e = filepath.EvalSymlinks(workRoot)
	if e != nil {
		t.Fatal("live_work_root_invalid")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("live_binary_must_be_absolute")
	}
	bytes, e := os.ReadFile(credentialsPath)
	if e != nil || len(bytes) > 1<<20 {
		t.Fatal("live_credentials_read_failed")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(bytes, &fixture) != nil || len(fixture.Accounts) < 2 {
		t.Fatal("live_two_accounts_required")
	}
	secrets := []string{}
	for _, account := range fixture.Accounts {
		if account.AccessToken == "" || account.AccountID == "" {
			t.Fatal("live_credentials_invalid")
		}
		secrets = append(secrets, account.AccessToken, account.AccountID, account.Alias)
	}
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if bpsLiveWriteEvidence(path, "http", !t.Failed(), evidence) != nil {
				t.Error("live_evidence_write_failed")
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, e := exec.CommandContext(ctx, binary, "--version").Output()
	if e != nil || !strings.Contains(string(version), "0.155.") {
		t.Fatal("live_cli_version_mismatch")
	}
	command := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--sandbox", "workspace-write", "-"}
	h := newBPSLiveHarness(t, binary, workRoot, fixture.Accounts[0], secrets, &evidence)
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		t.Fatal("live_nonce_failed")
	}
	nonce := "LIVE_" + hex.EncodeToString(random)
	input, _ := json.Marshal(map[string]any{"left": 17, "right": 26, "marker": nonce})
	if os.WriteFile(filepath.Join(h.work, "fixture.json"), input, 0600) != nil {
		t.Fatal("live_fixture_write_failed")
	}
	var thread string
	fileOK := t.Run("file", func(t *testing.T) {
		h.t = t
		result := h.run("file", "Read only fixture.json in the current directory. Compute left + right. Create result.json containing exactly two keys: sum with the numeric result and marker copied from the input. Then use a separate shell command to verify the saved file by reading it back. Do not access parent directories or the network. Finally state the sum and marker.", command...)
		data, e := os.ReadFile(filepath.Join(h.work, "result.json"))
		var actual map[string]json.RawMessage
		valid := e == nil && json.Unmarshal(data, &actual) == nil && len(actual) == 2
		var sum int
		var marker string
		_ = json.Unmarshal(actual["sum"], &sum)
		_ = json.Unmarshal(actual["marker"], &marker)
		passed := valid && sum == 43 && marker == nonce && result.Completed && result.Thread != ""
		h.log("file", result, passed)
		if !passed {
			t.Fatal("live_file_verification_failed")
		}
		thread = result.Thread
	})
	if fileOK {
		t.Run("resume", func(t *testing.T) {
			h.t = t
			result := h.run("resume", "Without reading files or using tools, recall the marker from the file task. Reply with that marker only.", "exec", "resume", thread, "--json", "--skip-git-repo-check", "-")
			passed := result.Completed && bpsLiveNoTools(result) && bpsLiveHasText(result, nonce)
			h.log("resume", result, passed)
			if !passed {
				t.Fatal("live_resume_verification_failed")
			}
		})
		t.Run("compact", func(t *testing.T) {
			h.t = t
			result := h.run("compact", "Without accessing files or using tools, recall the marker from the earlier file task. Reply with that marker only.", "-c", "model_auto_compact_token_limit=1000", "exec", "resume", thread, "--json", "--skip-git-repo-check", "-")
			encrypted, carried := false, false
			for _, record := range h.recorder.forCase("compact") {
				encrypted = encrypted || record.HasEncryptedCompaction
				for _, kind := range record.InputTypes {
					if kind == "compaction" {
						carried = true
					}
				}
			}
			passed := result.Completed && bpsLiveNoTools(result) && bpsLiveHasText(result, nonce) && encrypted && carried
			h.log("compact", result, passed)
			if !passed {
				t.Fatal("live_native_compaction_verification_failed")
			}
		})
	}
	t.Run("apply_patch", func(t *testing.T) {
		h.t = t
		if os.WriteFile(filepath.Join(h.work, "calc.py"), []byte("def add(a, b):\n    return a - b\n"), 0600) != nil {
			t.Fatal("live_patch_fixture_failed")
		}
		result := h.run("apply_patch", "Use the apply_patch tool to fix calc.py so add(a,b) returns the sum, then use the shell to import it and assert add(17,26)==43. Do not access parent directories or network. Finally report the result.", command...)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		check := exec.CommandContext(ctx, "python3", "-c", "from calc import add; assert add(17,26)==43")
		check.Dir = h.work
		check.Env = h.env
		checkErr := check.Run()
		patch := false
		for _, record := range h.recorder.forCase("apply_patch") {
			patch = patch || record.PatchCall
		}
		passed := result.Completed && checkErr == nil && patch
		h.log("apply_patch", result, passed)
		if !passed {
			t.Fatal("live_apply_patch_verification_failed")
		}
	})
	t.Run("search", func(t *testing.T) {
		h.t = t
		searchArgs := append([]string{"-c", `web_search="live"`}, command...)
		result := h.run("search", "请实际联网搜索 OpenAI 官方 Codex CLI 的安装命令，给出命令和来源链接。不要读写本地文件，不要操作办公文档。", searchArgs...)
		searched := false
		for _, record := range h.recorder.forCase("search") {
			for _, name := range record.ToolCalls {
				searched = searched || name == "native/web_search"
			}
		}
		passed := result.Completed && searched && strings.Contains(strings.Join(result.Final, "\n"), "https://")
		h.log("search", result, passed)
		if !passed {
			t.Fatal("live_search_verification_failed")
		}
	})
	t.Run("mcp_parallel", func(t *testing.T) {
		mcp := newBPSLiveHarness(t, binary, workRoot, fixture.Accounts[0], secrets, &evidence)
		marker := "MCP_" + nonce
		fixturePath := filepath.Join(mcp.home, "fixture_mcp.py")
		if os.WriteFile(fixturePath, []byte(bpsLiveMCPScript), 0600) != nil || os.WriteFile(filepath.Join(mcp.home, "mcp-marker.txt"), []byte(marker), 0600) != nil {
			t.Fatal("live_mcp_fixture_failed")
		}
		config, err := os.OpenFile(filepath.Join(mcp.home, "config.toml"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("live_mcp_config_failed")
		}
		arguments, _ := json.Marshal([]string{"-u", fixturePath})
		_, err = fmt.Fprintf(config, "\n[mcp_servers.fixture]\ncommand=\"python3\"\nargs=%s\nrequired=true\ndefault_tools_approval_mode=\"approve\"\nstartup_timeout_sec=15\ntool_timeout_sec=30\n", arguments)
		_ = config.Close()
		if err != nil {
			t.Fatal("live_mcp_config_failed")
		}
		result := mcp.run("mcp_parallel", "Use only the fixture MCP add_and_stamp tool twice with (a=17,b=26) and (a=11,b=19). Dispatch these independent calls in parallel in one response or Promise.allSettled. Do not use shell, read files, or access network. Finally report both sums and the marker returned by the MCP server.", command...)
		callsData, readErr := os.ReadFile(filepath.Join(mcp.home, "mcp-calls.jsonl"))
		var calls []struct {
			Sum        int `json:"sum"`
			Start, End float64
			Marker     string
		}
		for _, line := range bytesSplitLines(callsData) {
			var call struct {
				Sum        int `json:"sum"`
				Start, End float64
				Marker     string
			}
			if json.Unmarshal(line, &call) == nil {
				calls = append(calls, call)
			}
		}
		verified := readErr == nil && len(calls) == 2
		if verified {
			verified = calls[0].Marker == marker && calls[1].Marker == marker && calls[0].Sum+calls[1].Sum == 73 && (calls[0].Sum == 30 || calls[0].Sum == 43) && calls[0].Start < calls[1].End && calls[1].Start < calls[0].End
		}
		answer := strings.Join(result.Final, "\n")
		passed := result.Completed && verified && strings.Contains(answer, marker) && strings.Contains(answer, "43") && strings.Contains(answer, "30")
		mcp.log("mcp_parallel", result, passed)
		if !passed {
			t.Fatal("live_mcp_parallel_verification_failed")
		}
	})
	for i := 0; i < 2; i++ {
		i := i
		t.Run(fmt.Sprintf("candy_account_%d", i+1), func(t *testing.T) {
			candy := newBPSLiveHarness(t, binary, workRoot, fixture.Accounts[i], secrets, &evidence)
			name := fmt.Sprintf("candy_account_%d", i+1)
			result := candy.run(name, bpsLiveCandyPrompt, command...)
			answer := strings.Join(result.Final, "\n")
			passed := result.Completed && bpsLiveNoTools(result) && regexp.MustCompile(`(?m)^FINAL_ANSWER\s*=\s*21\s*$`).MatchString(answer)
			candy.log(name, result, passed)
			if !passed {
				t.Fatal("live_candy_verification_failed")
			}
		})
	}
}

func bytesSplitLines(data []byte) [][]byte { return bytes.Split(bytes.TrimSpace(data), []byte("\n")) }

// bpsLiveBoundedTransport applies the same ten-generation bound to WebSocket
// frames after the real gateway translates them into native BPS HTTP requests.
// It does not inspect/log headers or request bodies and never fabricates replies.
type bpsLiveBoundedTransport struct {
	next     http.RoundTripper
	recorder *bpsLiveRecorder
}

func (transport bpsLiveBoundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost {
		transport.recorder.mu.Lock()
		transport.recorder.upstreamPosts++
		count := transport.recorder.upstreamPosts
		transport.recorder.mu.Unlock()
		if count > 10 {
			return nil, fmt.Errorf("live_upstream_request_limit")
		}
	}
	return transport.next.RoundTrip(request)
}

// TestBPSLiveCodexWebSocket is intentionally separate so operators can verify
// the newly built WS path without repeating the full eight-case HTTP suite.
// Plain HTTP generation is rejected by its fixture: fallback cannot pass it.
func TestBPSLiveCodexWebSocket(t *testing.T) {
	credentialsPath, binary := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY")
	if credentialsPath == "" || binary == "" {
		t.Skip("explicit BPS_LIVE_ACCOUNTS_FILE and BPS_LIVE_CODEX_BINARY are required")
	}
	workRoot, e := filepath.Abs(os.Getenv("BPS_LIVE_WORK_ROOT"))
	if e != nil || os.Getenv("BPS_LIVE_WORK_ROOT") == "" {
		t.Fatal("live_work_root_required")
	}
	workRoot, e = filepath.EvalSymlinks(workRoot)
	if e != nil {
		t.Fatal("live_work_root_invalid")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("live_binary_must_be_absolute")
	}
	data, e := os.ReadFile(credentialsPath)
	if e != nil || len(data) > 1<<20 {
		t.Fatal("live_credentials_read_failed")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Accounts) < 1 {
		t.Fatal("live_account_required")
	}
	account := fixture.Accounts[0]
	if account.AccessToken == "" || account.AccountID == "" {
		t.Fatal("live_credentials_invalid")
	}
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if bpsLiveWriteEvidence(path, "websocket", !t.Failed(), evidence) != nil {
				t.Error("live_evidence_write_failed")
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, e := exec.CommandContext(ctx, binary, "--version").Output()
	if e != nil || !strings.Contains(string(version), "0.155.") {
		t.Fatal("live_cli_version_mismatch")
	}
	h := newBPSLiveHarness(t, binary, workRoot, account, []string{account.AccessToken, account.AccountID, account.Alias}, &evidence)
	configPath := filepath.Join(h.home, "config.toml")
	config, e := os.ReadFile(configPath)
	if e != nil {
		t.Fatal("live_config_read_failed")
	}
	config = bytes.Replace(config, []byte("supports_websockets=false"), []byte("supports_websockets=true"), 1)
	if os.WriteFile(configPath, config, 0600) != nil {
		t.Fatal("live_config_write_failed")
	}
	h.recorder.requireWS = true
	next := h.recorder.p.Client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	h.recorder.p.Client.Transport = bpsLiveBoundedTransport{next: next, recorder: h.recorder}
	var random [12]byte
	if _, e = rand.Read(random[:]); e != nil {
		t.Fatal("live_nonce_failed")
	}
	nonce := "WS_" + hex.EncodeToString(random[:])
	input, _ := json.Marshal(map[string]any{"left": 17, "right": 26, "marker": nonce})
	if os.WriteFile(filepath.Join(h.work, "fixture.json"), input, 0600) != nil {
		t.Fatal("live_fixture_write_failed")
	}
	command := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--sandbox", "workspace-write", "-"}
	metricBoundary := func() int64 {
		var id int64
		for _, record := range h.recorder.p.Metrics.RecentForAccount("test", 500) {
			if record.ID > id {
				id = record.ID
			}
		}
		return id
	}
	verifyMetrics := func(before int64, needCompact bool) (bool, []map[string]any) {
		var records []map[string]any
		ws, compacted := false, false
		for _, record := range h.recorder.p.Metrics.RecentForAccount("test", 500) {
			if record.ID <= before {
				continue
			}
			compact := record.NativeCompaction != nil && *record.NativeCompaction
			records = append(records, map[string]any{"request_type": record.RequestType, "status": record.Status, "http_code": record.HTTPCode, "native_compaction": compact, "terminal_status": record.TerminalStatus, "error": h.scrub(record.Error)})
			if record.RequestType == "websocket" && record.Status == "ok" {
				ws = true
			}
			if compact && record.Status == "ok" {
				compacted = true
			}
		}
		return ws && (!needCompact || compacted), records
	}
	log := func(name string, result bpsLiveCLIResult, passed bool, before int64, needCompact bool) bool {
		metricOK, records := verifyMetrics(before, needCompact)
		upgrades := 0
		for _, record := range h.recorder.forCase(name) {
			if record.WebSocketUpgrade && record.Status == http.StatusSwitchingProtocols {
				upgrades++
			}
		}
		passed = passed && metricOK && upgrades > 0
		h.log(name, result, passed)
		entry := evidence[len(evidence)-1]
		entry["gateway_metrics"] = records
		entry["successful_ws_upgrades"] = upgrades
		entry["http_generation_fallback_allowed"] = false
		encoded, _ := json.Marshal(map[string]any{"case": name, "gateway_metrics": records, "successful_ws_upgrades": upgrades, "passed": passed})
		h.t.Log(string(encoded))
		return passed
	}
	var thread string
	fileOK := t.Run("file", func(t *testing.T) {
		h.t = t
		before := metricBoundary()
		result := h.run("ws_file", "Read only fixture.json. Calculate left + right and create result.json with exactly numeric sum and the copied marker. Verify the saved file by reading it back. Do not read parent directories or access network. Finally state sum and marker.", command...)
		saved, e := os.ReadFile(filepath.Join(h.work, "result.json"))
		var actual struct {
			Sum    int    `json:"sum"`
			Marker string `json:"marker"`
		}
		valid := e == nil && json.Unmarshal(saved, &actual) == nil && actual.Sum == 43 && actual.Marker == nonce
		passed := log("ws_file", result, result.Completed && result.Thread != "" && valid, before, false)
		if !passed {
			t.Fatal("live_ws_file_verification_failed")
		}
		thread = result.Thread
	})
	if !fileOK {
		return
	}
	t.Run("resume", func(t *testing.T) {
		h.t = t
		before := metricBoundary()
		result := h.run("ws_resume", "Without reading files or using tools, recall the marker from the file task. Reply with that marker only.", "exec", "resume", thread, "--json", "--skip-git-repo-check", "-")
		if !log("ws_resume", result, result.Completed && bpsLiveNoTools(result) && bpsLiveHasText(result, nonce), before, false) {
			t.Fatal("live_ws_resume_verification_failed")
		}
	})
	t.Run("compact", func(t *testing.T) {
		h.t = t
		before := metricBoundary()
		result := h.run("ws_compact", "Without accessing files or using tools, recall the marker from the earlier file task. Reply with that marker only.", "-c", "model_auto_compact_token_limit=1000", "exec", "resume", thread, "--json", "--skip-git-repo-check", "-")
		if !log("ws_compact", result, result.Completed && bpsLiveNoTools(result) && bpsLiveHasText(result, nonce), before, true) {
			t.Fatal("live_ws_compaction_verification_failed")
		}
	})
}

// MCP returns only pure sums and a test nonce. The private fixture log proves
// both real calls and overlapping execution; it is never part of the prompt.
const bpsLiveMCPScript = `import json,sys,time,threading
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
root=Path(__file__).resolve().parent
lock=threading.Lock()
pool=ThreadPoolExecutor(max_workers=2)
def reply(req,result=None,error=None):
    value={'jsonrpc':'2.0','id':req['id']}
    value['error' if error else 'result']=error if error else result
    with lock: print(json.dumps(value),flush=True)
def call(req):
    try:
        p=req.get('params',{});a=p['arguments']['a'];b=p['arguments']['b']
        assert p['name']=='add_and_stamp' and type(a) is int and type(b) is int
        started=time.monotonic();time.sleep(0.4)
        value={'sum':a+b,'marker':(root/'mcp-marker.txt').read_text()}
        record={**value,'start':started,'end':time.monotonic()}
        with lock:
            with (root/'mcp-calls.jsonl').open('a') as f:f.write(json.dumps(record)+'\n')
        reply(req,{'content':[{'type':'text','text':json.dumps(value)}]})
    except Exception:reply(req,error={'code':-32602,'message':'Invalid fixture request'})
for line in sys.stdin:
    try:
        req=json.loads(line)
        if 'id' not in req:continue
        method=req.get('method');p=req.get('params',{})
        if method=='initialize':reply(req,{'protocolVersion':p.get('protocolVersion','2024-11-05'),'capabilities':{'tools':{}},'serverInfo':{'name':'fixture','version':'1'}})
        elif method=='tools/list':reply(req,{'tools':[{'name':'add_and_stamp','description':'Add two integers and return a test marker. Safe to call concurrently.','annotations':{'readOnlyHint':True,'destructiveHint':False,'openWorldHint':False},'inputSchema':{'type':'object','properties':{'a':{'type':'integer'},'b':{'type':'integer'}},'required':['a','b'],'additionalProperties':False}}]})
        elif method=='tools/call':pool.submit(call,req)
        elif method=='ping':reply(req,{})
        else:reply(req,error={'code':-32601,'message':'Method not found'})
    except Exception:pass
pool.shutdown(wait=True)
`
