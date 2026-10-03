package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeEditTool struct {
	Name, Namespace, Kind string
	Definition            map[string]any
}

func nativeEditFindTool(tools []any, wanted, namespace string) (nativeEditTool, bool) {
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		kind, _ := tool["type"].(string)
		if kind == "namespace" {
			if found, ok := nativeEditFindTool(realCLIArray(tool["tools"]), wanted, name); ok {
				return found, true
			}
		}
		if name == wanted {
			return nativeEditTool{Name: name, Namespace: namespace, Kind: kind, Definition: tool}, true
		}
	}
	return nativeEditTool{}, false
}

func (tool nativeEditTool) call(callID, input string) (map[string]any, error) {
	item := map[string]any{"id": "item_" + callID, "call_id": callID, "name": tool.Name}
	if tool.Namespace != "" {
		item["namespace"] = tool.Namespace
	}
	if tool.Kind == "custom" {
		item["type"], item["input"] = "custom_tool_call", input
		return item, nil
	}
	if tool.Kind != "function" {
		return nil, errors.New("unsupported fixture tool kind")
	}
	parameters, _ := tool.Definition["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	arguments := map[string]any{}
	if tool.Name == "apply_patch" {
		for _, field := range []string{"patch", "input"} {
			if _, found := properties[field]; found {
				arguments[field] = input
				break
			}
		}
	} else {
		if _, found := properties["cmd"]; found {
			arguments["cmd"] = input
		} else if command, ok := properties["command"].(map[string]any); ok {
			if command["type"] == "array" {
				arguments["command"] = []string{"/bin/sh", "-c", input}
			} else {
				arguments["command"] = input
			}
		}
		if _, found := properties["login"]; found {
			arguments["login"] = false
		}
		if _, found := properties["max_output_tokens"]; found {
			arguments["max_output_tokens"] = 512
		}
		if _, found := properties["yield_time_ms"]; found {
			arguments["yield_time_ms"] = 1000
		}
	}
	if len(arguments) == 0 {
		return nil, errors.New("fixture tool schema lacks a supported input parameter")
	}
	item["type"], item["arguments"] = "function_call", realCLIJSON(arguments)
	return item, nil
}

func nativeEditOutputText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if content, ok := value.([]any); ok {
		var parts []string
		for _, raw := range content {
			if item, ok := raw.(map[string]any); ok {
				if text, ok := item["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

var nativeEditExitPattern = regexp.MustCompile(`(?m)^(?:Process exited with code |Exit code: )(-?\d+)\r?$`)

func nativeEditToolExit(text string) (int, bool) {
	var structured struct {
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(text), &structured) == nil && structured.Metadata.ExitCode != nil {
		return *structured.Metadata.ExitCode, true
	}
	match := nativeEditExitPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0, false
	}
	value, err := strconv.Atoi(match[1])
	return value, err == nil
}

func nativeEditRunCLI(t *testing.T, binary, home, workspace string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = workspace
	for _, key := range []string{"PATH", "SHELL", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Env = append(command.Env, "HOME="+home, "USERPROFILE="+home, "CODEX_HOME="+filepath.Join(home, "codex"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"CODEX_GATEWAY_FIXTURE_KEY=test-client-key", "NO_PROXY=*", "no_proxy=*", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		// CLI output includes synthetic file/tool content and paths; preserve
		// only counts here rather than leaking it into a reusable report/log.
		t.Fatalf("official CLI edit fixture failed: stdout_bytes=%d stderr_bytes=%d deadline=%t", stdout.Len(), stderr.Len(), ctx.Err() != nil)
	}
	return stdout.Bytes()
}

// Real tool execution with deterministic local model responses. This is a
// sequential file-edit/failure/correction/resume workflow, not model reasoning.
func TestGatewayNativeActualCLIFileEditFailureAndResume(t *testing.T) {
	cli, helper := os.Getenv("CODEX_NATIVE_CAPTURE_BIN"), os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN")
	if cli == "" || helper == "" || runtime.GOOS != "linux" {
		t.Skip("set official CLI/native helper paths on Linux for real file-edit tool execution")
	}
	cli, err := filepath.Abs(cli)
	if err != nil {
		t.Fatal("cannot resolve official CLI")
	}
	identity := nativeCaptureIdentity(t, cli, "official_npm_release")
	if identity.Version != "codex-cli "+clientVersion {
		t.Fatal("file-edit fixture requires the source-pinned CLI")
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
	const filename = "native_edit_fixture.txt"
	const initialContent = "native fixture version=1\nstatus=needs-fix\n"
	const finalContent = "native fixture version=2\nstatus=verified\n"
	const failedMarker = "CONTROLLED_EDIT_EXPECTED_FAILURE"
	const verifiedMarker = "CONTROLLED_EDIT_VERIFIED"
	const resumeMarker = "CONTROLLED_EDIT_RESUME_READ"
	const firstReply = "CONTROLLED_EDIT_FIRST_COMPLETE"
	const finalReply = "CONTROLLED_EDIT_RESUME_COMPLETE"
	file := filepath.Join(workspace, filename)
	fileHash := sha256.Sum256([]byte(finalContent))
	expectedFileHash := hex.EncodeToString(fileHash[:])
	checks := map[string]bool{}
	for _, key := range []string{"actual_patch_create", "expected_nonzero_exit_observed", "correction_after_failure", "actual_patch_update", "shell_verified_final_file", "resume_read_verified",
		"call_ids_unique_and_preserved", "tool_outputs_not_replayed", "input_prefix_stable", "instructions_tools_stable_within_process", "cache_key_stable", "native_tls_h2", "native_application_identity", "no_go_fallback"} {
		checks[key] = true
	}
	var mu sync.Mutex
	requestCount, customCalls, functionCalls := 0, 0, 0
	var patchTool, shellTool nativeEditTool
	var priorInput []any
	var processProperties string
	cacheKey := ""
	issued := map[string]string{}
	observedOutputs := map[string]string{}
	verifiedOutputs := map[string]bool{}
	shellExits := []int{}
	p := newTestProxy(t, 1)
	cert, _, ca := nativeCaptureCertificate(t)
	fail := func(w http.ResponseWriter, key, message string) {
		checks[key] = false
		t.Error(message)
		http.Error(w, "synthetic workflow assertion failed", http.StatusBadRequest)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Error("unexpected non-Responses request in file-edit fixture")
			http.Error(w, "synthetic Responses only", http.StatusBadRequest)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		var body map[string]any
		if err != nil || len(raw) > maxBody || json.Unmarshal(raw, &body) != nil {
			t.Error("invalid synthetic file-edit request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		requestCount++
		step := requestCount
		if step > 7 {
			fail(w, "tool_outputs_not_replayed", "file-edit workflow exceeded its exact seven-request bound")
			return
		}
		checks["native_tls_h2"] = checks["native_tls_h2"] && r.TLS != nil && r.TLS.Version == tls.VersionTLS13 && r.ProtoMajor == 2
		checks["native_application_identity"] = checks["native_application_identity"] && strings.HasPrefix(r.UserAgent(), "codex_exec/"+clientVersion+" ") && r.Header.Get("Originator") == "codex_exec"
		input, ok := body["input"].([]any)
		if !ok {
			fail(w, "input_prefix_stable", "CLI did not send full HTTP input array")
			return
		}
		properties := realCLIJSON(map[string]any{"model": body["model"], "instructions": body["instructions"], "tools": body["tools"]})
		if step == 1 || step == 6 {
			processProperties, priorInput = properties, nil
		} else if properties != processProperties {
			fail(w, "instructions_tools_stable_within_process", "request instructions or tool schema changed within one CLI execution")
			return
		}
		if len(input) < len(priorInput) || !reflect.DeepEqual(input[:len(priorInput)], priorInput) && len(priorInput) > 0 {
			fail(w, "input_prefix_stable", "HTTP continuation changed already-sent input prefix")
			return
		}
		priorInput = append([]any(nil), input...)
		key, _ := body["prompt_cache_key"].(string)
		if step == 1 {
			cacheKey = key
		}
		if key == "" || key != cacheKey {
			fail(w, "cache_key_stable", "same CLI thread changed its cache identity")
			return
		}
		if step == 1 {
			patchTool, ok = nativeEditFindTool(realCLIArray(body["tools"]), "apply_patch", "")
			if !ok || patchTool.Kind != "custom" && patchTool.Kind != "function" {
				fail(w, "actual_patch_create", "actual CLI tools did not offer an apply_patch variant")
				return
			}
			for _, name := range []string{"exec_command", "shell_command", "shell"} {
				if selected, found := nativeEditFindTool(realCLIArray(body["tools"]), name, ""); found && selected.Kind == "function" {
					shellTool = selected
					break
				}
			}
			if shellTool.Name == "" {
				fail(w, "shell_verified_final_file", "actual CLI tools did not offer a supported shell function")
				return
			}
		}
		outputs := map[string]map[string]any{}
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok || item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
				continue
			}
			id, _ := item["call_id"].(string)
			if _, duplicate := outputs[id]; duplicate {
				fail(w, "call_ids_unique_and_preserved", "HTTP history duplicated a tool-output call identity")
				return
			}
			expected, exists := issued[id]
			if !exists || item["type"] != expected {
				fail(w, "call_ids_unique_and_preserved", "tool output lost its issued call identity or type")
				return
			}
			encoded := realCLIJSON(item["output"])
			if prior, exists := observedOutputs[id]; exists && prior != encoded {
				fail(w, "tool_outputs_not_replayed", "previous tool result changed during continuation or resume")
				return
			}
			observedOutputs[id], outputs[id] = encoded, item
		}
		verifyOutput := func(id, marker string, exit int, check string) bool {
			item, ok := outputs[id]
			if !ok || verifiedOutputs[id] {
				fail(w, "call_ids_unique_and_preserved", "required newly executed tool result was missing or reprocessed")
				return false
			}
			text := nativeEditOutputText(item["output"])
			code, parsed := nativeEditToolExit(text)
			if !parsed || code != exit || marker != "" && !strings.Contains(text, marker) {
				fail(w, check, "tool result did not retain its expected actual exit status and marker")
				return false
			}
			verifiedOutputs[id] = true
			if item["type"] == "function_call_output" && id != "patch-create" && id != "patch-fix" {
				shellExits = append(shellExits, code)
			}
			return true
		}
		verifyDisk := func(expected, check string) bool {
			current, err := os.ReadFile(file)
			if err != nil || string(current) != expected {
				fail(w, check, "actual workspace file did not match the required workflow stage")
				return false
			}
			return true
		}
		var item map[string]any
		var callTool nativeEditTool
		var callID, toolInput string
		switch step {
		case 1:
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				fail(w, "actual_patch_create", "edit fixture file existed before CLI apply_patch")
				return
			}
			callTool, callID = patchTool, "patch-create"
			toolInput = "*** Begin Patch\n*** Add File: " + filename + "\n+native fixture version=1\n+status=needs-fix\n*** End Patch\n"
		case 2:
			if !verifyOutput("patch-create", "", 0, "actual_patch_create") || !verifyDisk(initialContent, "actual_patch_create") {
				return
			}
			callTool, callID = shellTool, "verify-expected-failure"
			toolInput = "python3 -c 'from pathlib import Path; import sys; data=Path(\"" + filename + "\").read_text(); ok=data==\"native fixture version=2\\nstatus=verified\\n\"; print(\"" + failedMarker + "\" if not ok else \"UNEXPECTED_SUCCESS\"); sys.exit(7 if not ok else 0)'"
		case 3:
			if !verifyOutput("verify-expected-failure", failedMarker, 7, "expected_nonzero_exit_observed") || !verifyDisk(initialContent, "correction_after_failure") {
				return
			}
			callTool, callID = patchTool, "patch-fix"
			toolInput = "*** Begin Patch\n*** Update File: " + filename + "\n@@\n-native fixture version=1\n-status=needs-fix\n+native fixture version=2\n+status=verified\n*** End Patch\n"
		case 4:
			if !verifyOutput("patch-fix", "", 0, "actual_patch_update") || !verifyDisk(finalContent, "actual_patch_update") {
				return
			}
			callTool, callID = shellTool, "verify-final"
			toolInput = "python3 -c 'from pathlib import Path; import hashlib; data=Path(\"" + filename + "\").read_bytes(); assert data==b\"native fixture version=2\\nstatus=verified\\n\"; print(\"" + verifiedMarker + " \"+hashlib.sha256(data).hexdigest())'"
		case 5:
			if !verifyOutput("verify-final", verifiedMarker+" "+expectedFileHash, 0, "shell_verified_final_file") || !verifyDisk(finalContent, "shell_verified_final_file") {
				return
			}
			item = realCLIMessage("edit-first-final", firstReply)
		case 6:
			if !verifyDisk(finalContent, "resume_read_verified") || outputs["verify-final"] == nil || outputs["verify-expected-failure"] == nil || !strings.Contains(realCLIJSON(input), firstReply) {
				fail(w, "resume_read_verified", "resumed CLI did not retain the prior file-edit conversation")
				return
			}
			callTool, callID = shellTool, "resume-read"
			toolInput = "python3 -c 'from pathlib import Path; import hashlib; data=Path(\"" + filename + "\").read_bytes(); assert data==b\"native fixture version=2\\nstatus=verified\\n\"; print(\"" + resumeMarker + " \"+hashlib.sha256(data).hexdigest())'"
		case 7:
			if !verifyOutput("resume-read", resumeMarker+" "+expectedFileHash, 0, "resume_read_verified") || !verifyDisk(finalContent, "resume_read_verified") {
				return
			}
			item = realCLIMessage("edit-resume-final", finalReply)
		}
		if item == nil {
			currentTool, available := nativeEditFindTool(realCLIArray(body["tools"]), callTool.Name, "")
			if !available || currentTool.Kind != callTool.Kind || currentTool.Namespace != callTool.Namespace || !reflect.DeepEqual(currentTool.Definition, callTool.Definition) {
				fail(w, "instructions_tools_stable_within_process", "issued tool no longer matched the current real CLI tool schema")
				return
			}
			if _, duplicate := issued[callID]; duplicate {
				fail(w, "tool_outputs_not_replayed", "fixture attempted to issue an already accepted tool call")
				return
			}
			item, err = callTool.call(callID, toolInput)
			if err != nil {
				fail(w, "call_ids_unique_and_preserved", "actual tool schema did not support the fixture command")
				return
			}
			if item["type"] == "custom_tool_call" {
				issued[callID] = "custom_tool_call_output"
				customCalls++
			} else {
				issued[callID] = "function_call_output"
				functionCalls++
			}
		}
		realCLIWriteSSE(w, fmt.Sprintf("edit-response-%d", step), item, 40)
	}))
	upstream.EnableHTTP2 = true
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p.BaseURL = upstream.URL
	p.Client = upstream.Client()
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	realGatewayNativePool(t, p)
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	config := fmt.Sprintf(`model = "gpt-5.4"
model_provider = "edit_fixture"
approval_policy = "never"
sandbox_mode = "danger-full-access"
web_search = "disabled"
model_auto_compact_token_limit = 1000000
[model_providers.edit_fixture]
name = "Controlled file edit"
base_url = %q
env_key = "CODEX_GATEWAY_FIXTURE_KEY"
wire_api = "responses"
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
`, gateway.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal("cannot write isolated edit fixture configuration")
	}
	first := nativeEditRunCLI(t, cli, home, workspace, "exec", "--skip-git-repo-check", "--ignore-rules", "--json", "Create the requested local fixture with apply_patch, run its validation, correct the expected failure, and verify the file.")
	if !bytes.Contains(first, []byte(firstReply)) {
		t.Fatal("official CLI did not finish the scripted edit/correction turn")
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
		t.Fatal("official CLI did not retain a thread for file-edit resume")
	}
	resumed := nativeEditRunCLI(t, cli, home, workspace, "exec", "resume", "--skip-git-repo-check", "--json", threadID, "Read the existing fixture from disk, validate its final bytes and hash, and finish without changing it.")
	if !bytes.Contains(resumed, []byte(finalReply)) {
		t.Fatal("official CLI did not complete resumed file verification")
	}
	mu.Lock()
	defer mu.Unlock()
	checks["no_go_fallback"] = guard.calls.Load() == 0
	checks["exact_request_and_tool_counts"] = requestCount == 7 && len(issued) == 5 && len(verifiedOutputs) == 5
	checks["expected_shell_exit_sequence"] = reflect.DeepEqual(shellExits, []int{7, 0, 0})
	checks["gateway_admission_released"] = p.inFlight("test") == 0
	final, err := os.ReadFile(file)
	checks["final_file_hash_matches"] = err == nil && sha256.Sum256(final) == fileHash
	page, err := p.Metrics.Logs(LogQuery{Size: 20})
	checks["gateway_seven_usage_records"] = err == nil && len(page.Items) == 7
	var totalInput, totalOutput int64
	checks["gateway_successful_synthetic_usage"] = len(page.Items) == 7
	for _, record := range page.Items {
		totalInput += record.InputTokens
		totalOutput += record.OutputTokens
		checks["gateway_successful_synthetic_usage"] = checks["gateway_successful_synthetic_usage"] && record.Status == "ok" && record.UsageKnown != nil && *record.UsageKnown
	}
	checks["synthetic_usage_totals_preserved"] = totalInput == 280 && totalOutput == 7
	for key, passed := range checks {
		if !passed {
			t.Errorf("real CLI edit fixture assertion failed: %s", key)
		}
	}
	report := map[string]any{"schema": "official-cli-native-file-edit-v1", "passed": !t.Failed(), "cli": identity,
		"helper_sha256": hex.EncodeToString(helperHash[:]), "requests": requestCount, "unique_tool_calls": len(issued), "verified_tool_outputs": len(verifiedOutputs),
		"custom_tool_calls": customCalls, "function_tool_calls": functionCalls, "shell_exit_codes": shellExits,
		"apply_patch_wire_kind": patchTool.Kind, "apply_patch_namespace_present": patchTool.Namespace != "", "shell_tool_name": shellTool.Name,
		"file_sha256": expectedFileHash, "file_bytes": len(final), "assertions": checks, "fresh_cli_home": true,
		"fresh_os_home": true, "resume_processes": 2, "desktop_auth_read": false, "live_accounts": false, "live_model_inference": false,
		"execution_path": "official CLI -> Go gateway -> Rust UDS helper -> loopback TLS HTTP/2 scripted Responses",
		"scope":          "sequential real apply_patch and shell execution with expected failure/correction; no parallel-tool or model-reasoning claim"}
	if path := os.Getenv("CODEX_NATIVE_EDIT_CLI_REPORT"); path != "" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil || os.WriteFile(path, append(encoded, '\n'), 0600) != nil {
			t.Fatal("cannot write sanitized real-edit report")
		}
	}
	t.Logf("actual CLI file edit: requests=%d custom_calls=%d function_calls=%d verified_outputs=%d expected_failure_count=1 resume_processes=2", requestCount, customCalls, functionCalls, len(verifiedOutputs))
}
