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
)

// RemoteCompactionV2 is a provider capability in CLI 0.155, not a live feature
// toggle. Its request is a streaming /responses call containing a trigger.
func TestGatewayNativeActualCLIRemoteCompactionV2AndResume(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI/native helper paths on Linux for actual V2 compaction")
	}
	cli, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal("cannot resolve official CLI")
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("compaction fixture requires the source-pinned official CLI")
	}
	helperBytes, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal("cannot hash native helper")
	}
	helperHash := sha256.Sum256(helperBytes)
	home, workspace := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal("cannot create fresh CLI home")
	}
	const firstMarker = "CONTROLLED_PRE_COMPACTION_TOOL"
	const secondMarker = "CONTROLLED_POST_COMPACTION_TOOL"
	const firstReply = "CONTROLLED_COMPACTION_FIRST_COMPLETE"
	const resumeReply = "CONTROLLED_COMPACTION_RESUME_COMPLETE"
	const resumePrompt = "Continue the compacted thread and finish the controlled resume check without using tools."
	const opaque = "SYNTHETIC_V2_COMPACTION_OPAQUE+/=not-a-real-ciphertext"
	opaqueHash := sha256.Sum256([]byte(opaque))
	checks := map[string]bool{}
	for _, name := range []string{"responses_only_no_legacy_compact", "exact_one_compaction_trigger", "first_tool_output_before_compaction", "post_compaction_tool_output",
		"opaque_compaction_item_preserved", "opaque_preserved_after_resume", "compaction_metadata_matches_trigger", "cache_key_stable",
		"instructions_stable", "native_tls_h2", "request_identity_stable", "no_go_fallback", "fresh_cli_home_and_resume"} {
		checks[name] = true
	}
	var mu sync.Mutex
	requests, compactions, toolOutputs, opaqueRequests := 0, 0, 0, 0
	var shell nativeEditTool
	cacheKey, instructions, sessionHeader, threadHeader := "", "", "", ""
	p := newTestProxy(t, 1)
	cert, _, ca := nativeCaptureCertificate(t)
	fail := func(w http.ResponseWriter, key, message string) {
		checks[key] = false
		t.Error(message)
		http.Error(w, "synthetic compaction assertion failed", http.StatusBadRequest)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			fail(w, "responses_only_no_legacy_compact", "CLI V2 used a non-Responses route or legacy compact endpoint")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		var body map[string]any
		if err != nil || len(raw) > maxBody || json.Unmarshal(raw, &body) != nil {
			fail(w, "responses_only_no_legacy_compact", "invalid controlled compaction request")
			return
		}
		requests++
		step := requests
		if step > 5 {
			fail(w, "exact_one_compaction_trigger", "CLI exceeded the bounded compaction workflow request count")
			return
		}
		checks["native_tls_h2"] = checks["native_tls_h2"] && r.TLS != nil && r.TLS.Version == tls.VersionTLS13 && r.ProtoMajor == 2
		key, _ := body["prompt_cache_key"].(string)
		text, _ := body["instructions"].(string)
		if step == 1 {
			cacheKey, instructions = key, text
			sessionHeader, threadHeader = r.Header.Get("Session-Id"), r.Header.Get("Thread-Id")
			for _, name := range []string{"exec_command", "shell_command", "shell"} {
				if candidate, ok := nativeEditFindTool(realCLIArray(body["tools"]), name, ""); ok && candidate.Kind == "function" {
					shell = candidate
					break
				}
			}
			if shell.Name == "" {
				fail(w, "first_tool_output_before_compaction", "real CLI shell schema unavailable")
				return
			}
		}
		checks["cache_key_stable"] = checks["cache_key_stable"] && key != "" && key == cacheKey
		checks["instructions_stable"] = checks["instructions_stable"] && text != "" && text == instructions
		checks["request_identity_stable"] = checks["request_identity_stable"] && sessionHeader != "" && threadHeader != "" && r.Header.Get("Session-Id") == sessionHeader && r.Header.Get("Thread-Id") == threadHeader
		input := realCLIArray(body["input"])
		triggers, opaqueItems := 0, 0
		outputs := make(map[string]map[string]any)
		for index, value := range input {
			item, ok := value.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "compaction_trigger":
				triggers++
				if index != len(input)-1 {
					fail(w, "exact_one_compaction_trigger", "V2 trigger was not the final input control")
					return
				}
			case "compaction":
				opaqueItems++
				if item["encrypted_content"] != opaque {
					fail(w, "opaque_compaction_item_preserved", "gateway or CLI changed the synthetic opaque compaction item")
					return
				}
			case "function_call_output":
				id, _ := item["call_id"].(string)
				if _, repeated := outputs[id]; repeated {
					fail(w, "first_tool_output_before_compaction", "history repeated a tool output call identity")
					return
				}
				outputs[id] = item
			}
		}
		var metadata map[string]any
		_ = json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &metadata)
		kind, _ := metadata["request_kind"].(string)
		if step == 2 {
			compactions++
			if triggers != 1 || kind != "compaction" {
				fail(w, "compaction_metadata_matches_trigger", "automatic V2 request did not expose its real trigger and compaction metadata")
				return
			}
		} else if triggers != 0 || kind == "compaction" {
			fail(w, "exact_one_compaction_trigger", "ordinary post-compaction request retained a durable trigger")
			return
		}
		if step >= 3 {
			if opaqueItems != 1 {
				fail(w, "opaque_compaction_item_preserved", "post-compaction request lost or duplicated its opaque summary")
				return
			}
			opaqueRequests++
		}
		verifyTool := func(id, marker, check string) bool {
			item, found := outputs[id]
			if !found {
				fail(w, check, "expected real tool result did not reach the compaction workflow")
				return false
			}
			text := nativeEditOutputText(item["output"])
			code, ok := nativeEditToolExit(text)
			if !ok || code != 0 || !strings.Contains(text, marker) {
				fail(w, check, "actual CLI tool result lost its success status or marker")
				return false
			}
			toolOutputs++
			return true
		}
		var item map[string]any
		tokens := 100
		switch step {
		case 1:
			item, err = shell.call("before-compact-tool", "printf '%s\\n' '"+firstMarker+"'")
			tokens = 50000
		case 2:
			if !verifyTool("before-compact-tool", firstMarker, "first_tool_output_before_compaction") {
				return
			}
			item = map[string]any{"id": "controlled-compaction-item", "type": "compaction", "encrypted_content": opaque}
		case 3:
			current, ok := nativeEditFindTool(realCLIArray(body["tools"]), shell.Name, "")
			if !ok || current.Kind != shell.Kind || current.Namespace != shell.Namespace {
				fail(w, "post_compaction_tool_output", "post-compaction real tool definition disappeared")
				return
			}
			item, err = current.call("after-compact-tool", "printf '%s\\n' '"+secondMarker+"'")
		case 4:
			if !verifyTool("after-compact-tool", secondMarker, "post_compaction_tool_output") {
				return
			}
			item = realCLIMessage("controlled-post-compact-final", firstReply)
		case 5:
			if !strings.Contains(realCLIJSON(input), firstReply) || !strings.Contains(realCLIJSON(input), resumePrompt) || outputs["after-compact-tool"] == nil {
				fail(w, "opaque_preserved_after_resume", "resumed compacted history lost post-compaction conversation")
				return
			}
			item = realCLIMessage("controlled-resume-final", resumeReply)
		}
		if err != nil {
			fail(w, "post_compaction_tool_output", "actual tool schema did not accept the controlled command")
			return
		}
		realCLIWriteSSE(w, fmt.Sprintf("controlled-v2-response-%d", step), item, tokens)
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
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "compact_fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
model_auto_compact_token_limit = 25000
[model_providers.compact_fixture]
name = "OpenAI"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write isolated compaction configuration")
	}
	first := nativeEditRunCLI(t, cli, home, workspace, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Run the requested local tool steps and complete the controlled compaction workflow.")
	if !bytes.Contains(first, []byte(firstReply)) {
		t.Fatal("official CLI did not complete V2 compaction and its follow-up")
	}
	thread := ""
	for _, line := range bytes.Split(first, []byte{'\n'}) {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "thread.started" {
			thread = event.ThreadID
		}
	}
	if thread == "" {
		t.Fatal("actual CLI compaction did not persist a resumable thread")
	}
	resumed := nativeEditRunCLI(t, cli, home, workspace, "exec", "resume", "--skip-git-repo-check", "--json", thread, resumePrompt)
	if !bytes.Contains(resumed, []byte(resumeReply)) {
		t.Fatal("actual CLI could not resume the opaque compacted history")
	}
	mu.Lock()
	defer mu.Unlock()
	checks["exact_five_requests_two_tools_one_compaction"] = requests == 5 && toolOutputs == 2 && compactions == 1 && opaqueRequests == 3
	checks["no_go_fallback"] = guard.calls.Load() == 0
	checks["gateway_admission_released"] = p.inFlight("test") == 0
	page, err := p.Metrics.Logs(LogQuery{Size: 20})
	checks["five_usage_records"] = err == nil && len(page.Items) == 5
	compactionRecords, normalRecords := 0, 0
	var inputTotal, outputTotal int64
	checks["passive_compaction_classification"] = len(page.Items) == 5
	for _, record := range page.Items {
		inputTotal += record.InputTokens
		outputTotal += record.OutputTokens
		if record.NativeCompaction == nil || record.RequestType != "http_sse" || record.Status != "ok" {
			checks["passive_compaction_classification"] = false
			continue
		}
		if *record.NativeCompaction {
			compactionRecords++
			checks["passive_compaction_classification"] = checks["passive_compaction_classification"] && record.ResponseID == "controlled-v2-response-2"
		} else {
			normalRecords++
			checks["passive_compaction_classification"] = checks["passive_compaction_classification"] && record.ResponseID != "controlled-v2-response-2"
		}
	}
	checks["passive_compaction_classification"] = checks["passive_compaction_classification"] && compactionRecords == 1 && normalRecords == 4
	checks["synthetic_usage_not_invented_or_dropped"] = inputTotal == 50400 && outputTotal == 5
	for name, passed := range checks {
		if !passed {
			t.Errorf("actual CLI compaction assertion failed: %s", name)
		}
	}
	report := map[string]any{"schema": "official-cli-native-compaction-v2-v1", "passed": !t.Failed(), "cli": identity,
		"helper_sha256": hex.EncodeToString(helperHash[:]), "requests": requests, "compaction_requests": compactions, "actual_tool_outputs": toolOutputs,
		"post_compaction_opaque_requests": opaqueRequests, "opaque_sha256": hex.EncodeToString(opaqueHash[:]), "opaque_is_synthetic_not_real_ciphertext": true,
		"compaction_endpoint": "/responses", "legacy_compact_requests": 0, "removed_feature_flag_overridden": false,
		"configured_auto_compact_token_limit": 25000, "initial_synthetic_input_usage": 50000, "compaction_records": compactionRecords,
		"normal_records": normalRecords, "assertions": checks, "cli_processes": 2, "fresh_os_and_cli_home": true,
		"desktop_auth_read": false, "live_accounts": false, "live_model_inference": false,
		"execution_path": "official CLI -> Go native HTTP -> Rust UDS -> loopback TLS HTTP/2 Responses",
		"limits":         []string{"Synthetic token usage triggers actual CLI compaction, not real model inference or cache measurement.", "The opaque compaction item is a fixture string preserved transparently, not decrypted or cryptographically authenticated by the gateway."}}
	if path := os.Getenv("CODEX_NATIVE_COMPACT_CLI_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized compaction report")
		}
	}
	t.Logf("actual CLI V2 compaction: requests=%d compactions=%d tool_outputs=%d opaque_followups=%d", requests, compactions, toolOutputs, opaqueRequests)
}
