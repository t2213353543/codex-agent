package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCodexCLIIntegration is an opt-in live-model acceptance test. The CLI
// connects only to this test's local Proxy, whose stores are all temporary.
// Supply either an upstream gateway URL/key or an explicit account export file,
// plus a model. This test never discovers credentials or uses an existing DB.
// Run with -timeout 15m, optionally setting CODEX_CLI_BIN and
// CODEX_CLI_EVIDENCE_FILE for a CLI executable and a content-free JSON report.
func TestCodexCLIIntegration(t *testing.T) {
	runCodexCLIIntegration(t, true)
}

// TestCodexFingerprintCLIConversation isolates application identity acceptance
// from the separately required full CLI shell/sandbox acceptance above.
func TestCodexFingerprintCLIConversation(t *testing.T) {
	if os.Getenv("CODEX_CLI_FINGERPRINT_PROFILE") == "" {
		t.Skip("set CODEX_CLI_FINGERPRINT_PROFILE and live account/model variables")
	}
	runCodexCLIIntegration(t, false)
}

func runCodexCLIIntegration(t *testing.T, withTools bool) {
	t.Helper()
	upstreamURL := strings.TrimRight(strings.TrimSpace(os.Getenv("CODEX_CLI_UPSTREAM_URL")), "/")
	upstreamKey := os.Getenv("CODEX_CLI_UPSTREAM_KEY")
	model := strings.TrimSpace(os.Getenv("CODEX_CLI_MODEL"))
	accountFile := strings.TrimSpace(os.Getenv("CODEX_CLI_ACCOUNT_FILE"))
	if model == "" || (accountFile == "" && (upstreamURL == "" || strings.TrimSpace(upstreamKey) == "")) {
		t.Skip("set CODEX_CLI_MODEL and either CODEX_CLI_ACCOUNT_FILE or CODEX_CLI_UPSTREAM_URL/KEY for real CLI acceptance")
	}
	if accountFile != "" {
		if upstreamURL != "" || upstreamKey != "" {
			t.Fatal("choose an account export file or an upstream gateway; do not combine both")
		}
		upstreamURL = codexURL
	}
	parsedURL, err := url.Parse(upstreamURL)
	if err != nil || parsedURL.Hostname() == "" ||
		(parsedURL.Scheme != "http" && parsedURL.Scheme != "https") ||
		parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" ||
		strings.HasSuffix(parsedURL.Path, "/responses") {
		t.Fatal("CODEX_CLI_UPSTREAM_URL must be an HTTP(S) API base without userinfo, query, fragment, or /responses suffix")
	}
	if strings.ContainsAny(upstreamKey, "\r\n") {
		t.Fatal("CODEX_CLI_UPSTREAM_KEY must not contain newlines")
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("python is required for the opted-in CLI integration test")
	}
	script, err := filepath.Abs(filepath.Join(projectRoot(t), "tools", "codex_cli_smoke.py"))
	if err != nil {
		t.Fatal("cannot resolve the CLI acceptance script")
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatal("tools/codex_cli_smoke.py is required for CLI integration")
	}

	p := newTestProxy(t, 10)
	configureCodexCLIFingerprintTest(t, p)
	if accountFile != "" {
		codexCLIImportTestAccount(t, p, accountFile)
	} else if err := p.Store.UpdateAccount("test", func(account *Account) error {
		account.AccessToken = upstreamKey
		account.RefreshToken = ""
		account.AccountID = ""
		account.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal("cannot configure the temporary upstream account")
	}
	p.invalidateAccounts()
	p.BaseURL = upstreamURL
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)

	args := []string{script, "--base-url", server.URL + "/v1", "--model", model,
		"--api-key-env", "CODEX_CLI_LOCAL_KEY", "--timeout", "180"}
	if withTools {
		args = append(args, "--with-tools")
	}
	if bin := os.Getenv("CODEX_CLI_BIN"); bin != "" {
		args = append(args, "--codex", bin)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, python, args...)
	command.Env = codexCLIIntegrationEnvironment()
	command.WaitDelay = 5 * time.Second
	if runtime.GOOS == "windows" {
		// The script normally terminates its own CLI process tree on per-turn
		// timeouts. Also clean up descendants if the outer deadline expires.
		command.Cancel = func() error {
			killContext, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer killCancel()
			killer := exec.CommandContext(killContext, "taskkill", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
			if err := killer.Run(); err != nil {
				return command.Process.Kill()
			}
			return nil
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	runErr := command.Run()
	server.Close() // Wait for request accounting before reading local metrics.

	evidence := codexCLIIntegrationEvidence{
		SchemaVersion:    1,
		AcceptanceScope:  "conversation_only",
		ElapsedSeconds:   time.Since(started).Seconds(),
		ProcessExitCode:  -1,
		TimedOut:         ctx.Err() != nil,
		Checks:           []codexCLIIntegrationCheck{},
		HTTPStatusCounts: map[int]int{},
		Failures:         []string{},
		ImportedAccount:  accountFile != "",
	}
	if withTools {
		evidence.AcceptanceScope = "conversation_and_shell"
	}
	if command.ProcessState != nil {
		evidence.ProcessExitCode = command.ProcessState.ExitCode()
	}
	if runErr != nil {
		evidence.Failures = append(evidence.Failures, "cli_process_failed")
	}
	if os.Getenv("CODEX_CLI_DIAGNOSTICS") == "1" {
		var diagnostics struct {
			Checks []struct {
				ErrorMessages   []string        `json:"error_messages"`
				ItemTypes       map[string]int  `json:"completed_item_types"`
				ToolDiagnostics json.RawMessage `json:"tool_diagnostics"`
			} `json:"checks"`
		}
		if json.Unmarshal(stdout.Bytes(), &diagnostics) == nil {
			for _, check := range diagnostics.Checks {
				encoded, _ := json.Marshal(check.ItemTypes)
				codexCLILogDiagnostic(t, p, "completed item types: "+string(encoded))
				codexCLILogDiagnostic(t, p, "tool diagnostics: "+string(check.ToolDiagnostics))
				for _, message := range check.ErrorMessages {
					codexCLILogDiagnostic(t, p, message)
				}
			}
		}
	}
	var report struct {
		Passed     bool                       `json:"passed"`
		CLIVersion string                     `json:"cli_version"`
		Checks     []codexCLIIntegrationCheck `json:"checks"`
	}
	if json.Unmarshal(stdout.Bytes(), &report) != nil {
		evidence.Failures = append(evidence.Failures, "invalid_cli_report")
	} else {
		// Never persist stderr, model replies, error messages, URLs, credentials,
		// thread IDs, or arbitrary keys from the subprocess JSON.
		if regexp.MustCompile(`^codex-cli [0-9][0-9A-Za-z.+_-]{0,79}$`).MatchString(report.CLIVersion) {
			evidence.CLIVersion = report.CLIVersion
		}
		seen := map[string]bool{}
		for _, check := range report.Checks {
			switch check.Name {
			case "initial_session", "resume_memory", "shell_tool":
			default:
				continue
			}
			filtered := map[string]bool{}
			for _, name := range []string{
				"process_exited_successfully", "thread_started", "same_thread_resumed",
				"turn_started", "turn_completed", "no_error_events", "final_reply_matches",
				"shell_read_marker", "no_commands_for_memory_check", "marker_file_unchanged",
			} {
				if passed, exists := check.Assertions[name]; exists {
					filtered[name] = passed
				}
			}
			check.Assertions = filtered
			evidence.Checks = append(evidence.Checks, check)
			seen[check.Name] = check.Passed && !check.TimedOut && check.ReturnCode == 0
		}
		if !report.Passed || !seen["initial_session"] || !seen["resume_memory"] || (withTools && !seen["shell_tool"]) {
			evidence.Failures = append(evidence.Failures, "cli_conversation_checks_failed")
		}
	}

	for page := 1; ; page++ {
		logs, err := p.Metrics.Logs(LogQuery{Page: page, Size: 500})
		if err != nil {
			evidence.Failures = append(evidence.Failures, "local_metrics_read_failed")
			break
		}
		for _, record := range logs.Items {
			if record.Endpoint != "responses" && record.Endpoint != "/responses" {
				continue
			}
			evidence.Requests++
			evidence.HTTPStatusCounts[record.HTTPCode]++
			if record.Status != "ok" || record.Error != "" || record.HTTPCode < 200 || record.HTTPCode >= 300 {
				evidence.FailedRequests++
				if os.Getenv("CODEX_CLI_DIAGNOSTICS") == "1" {
					codexCLILogDiagnostic(t, p, record.Error)
				}
			}
			if record.InputTokens <= 0 || record.OutputTokens <= 0 || record.CachedTokens < 0 {
				evidence.RequestsWithoutUsage++
			}
			evidence.InputTokens += record.InputTokens
			evidence.OutputTokens += record.OutputTokens
			evidence.CachedTokens += record.CachedTokens
		}
		if !logs.HasMore {
			break
		}
	}
	if withTools && evidence.Requests < 3 {
		evidence.Failures = append(evidence.Failures, "fewer_than_three_inference_requests")
	} else if evidence.Requests < 2 {
		evidence.Failures = append(evidence.Failures, "fewer_than_two_inference_requests")
	}
	if evidence.FailedRequests != 0 {
		evidence.Failures = append(evidence.Failures, "local_inference_request_failed")
	}
	if evidence.HTTPStatusCounts[401] != 0 || evidence.HTTPStatusCounts[403] != 0 {
		evidence.Failures = append(evidence.Failures, "upstream_authentication_failed")
	}
	if evidence.RequestsWithoutUsage != 0 || evidence.InputTokens <= 0 || evidence.OutputTokens <= 0 {
		evidence.Failures = append(evidence.Failures, "missing_actual_inference_usage")
	}
	evidence.Passed = len(evidence.Failures) == 0
	rendered, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal("cannot encode CLI integration evidence")
	}
	if path := os.Getenv("CODEX_CLI_EVIDENCE_FILE"); path != "" {
		if err := os.WriteFile(path, append(rendered, '\n'), 0600); err != nil {
			t.Error("cannot write CODEX_CLI_EVIDENCE_FILE; ensure its parent directory exists")
		}
	}
	t.Logf("CLI integration evidence: %s", rendered)
	if !evidence.Passed {
		t.Fatal("real CLI acceptance failed; inspect the content-free evidence above")
	}
}

func codexCLILogDiagnostic(t *testing.T, p *Proxy, message string) {
	t.Helper()
	_, accounts, err := p.Store.List()
	if err != nil {
		return
	}
	for _, account := range accounts {
		for _, secret := range []string{account.AccessToken, account.RefreshToken, account.AccountID, account.ClientID, account.Name} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
	}
	message = redactMetricError(message)
	message = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`).ReplaceAllString(message, "[redacted-email]")
	if len(message) > 800 {
		message = message[:800]
	}
	t.Logf("redacted CLI diagnostic: %s", message)
}

type codexCLIIntegrationCheck struct {
	Name                  string          `json:"name"`
	Passed                bool            `json:"passed"`
	Assertions            map[string]bool `json:"assertions"`
	ReturnCode            int             `json:"returncode"`
	TimedOut              bool            `json:"timed_out"`
	CompletedCommandCount int             `json:"completed_command_count"`
}

type codexCLIIntegrationEvidence struct {
	SchemaVersion        int                        `json:"schema_version"`
	AcceptanceScope      string                     `json:"acceptance_scope"`
	ImportedAccount      bool                       `json:"imported_account"`
	Passed               bool                       `json:"passed"`
	CLIVersion           string                     `json:"cli_version,omitempty"`
	ElapsedSeconds       float64                    `json:"elapsed_seconds"`
	ProcessExitCode      int                        `json:"process_exit_code"`
	TimedOut             bool                       `json:"timed_out"`
	Checks               []codexCLIIntegrationCheck `json:"checks"`
	Requests             int                        `json:"inference_requests"`
	HTTPStatusCounts     map[int]int                `json:"http_status_counts"`
	FailedRequests       int                        `json:"failed_inference_requests"`
	RequestsWithoutUsage int                        `json:"requests_without_usage"`
	InputTokens          int64                      `json:"input_tokens"`
	OutputTokens         int64                      `json:"output_tokens"`
	CachedTokens         int64                      `json:"cached_tokens"`
	Failures             []string                   `json:"failures"`
}

func codexCLIIntegrationEnvironment() []string {
	allowed := map[string]bool{}
	for _, name := range []string{
		"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR",
		"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
		"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "LANG", "LC_ALL", "SHELL",
	} {
		allowed[name] = true
	}
	environment := []string{"CODEX_CLI_LOCAL_KEY=test-client-key", "NO_PROXY=*", "PYTHONUTF8=1"}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(name)] {
			environment = append(environment, entry)
		}
	}
	return environment
}
