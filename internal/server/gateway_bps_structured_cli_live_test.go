package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// At most two official CLI tasks (one SSE, one WS), with a hard six-upstream-
// generation ceiling each. The random marker lives only in the actual file.
// These fixtures are isolated; no customer prompts or production state change.
func TestBPSLiveStructuredCLI(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("official structured CLI fixture unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil || !strings.Contains(string(version), "0.155.") {
		t.Fatal("official structured CLI version mismatch")
	}
	help, err := exec.CommandContext(ctx, binary, "exec", "--help").Output()
	if err != nil || !bytes.Contains(help, []byte("--output-schema")) {
		t.Fatal("official CLI does not advertise output-schema")
	}
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if err := bpsLiveWriteEvidence(path, "bps-structured-official-cli", !t.Failed(), evidence); err != nil {
				t.Error("structured CLI evidence write failed")
			}
		}
	}()
	account := fixture.Accounts[0]
	for _, websocket := range []bool{false, true} {
		name := "official_schema_sse_tool_workflow"
		if websocket {
			name = "official_schema_ws_tool_workflow"
		}
		if !t.Run(name, func(t *testing.T) {
			h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
			if websocket {
				configPath := filepath.Join(h.home, "config.toml")
				config, err := os.ReadFile(configPath)
				if err != nil || !bytes.Contains(config, []byte("supports_websockets=false")) {
					t.Fatal("structured WS config missing")
				}
				config = bytes.Replace(config, []byte("supports_websockets=false"), []byte("supports_websockets=true"), 1)
				if os.WriteFile(configPath, config, 0600) != nil {
					t.Fatal("structured WS config write failed")
				}
				h.recorder.requireWS = true
			}
			const schema = `{"type":"object","properties":{"sum":{"type":"integer"},"marker":{"type":"string"}},"required":["sum","marker"],"additionalProperties":false}`
			if os.WriteFile(filepath.Join(h.work, "schema.json"), []byte(schema), 0600) != nil {
				t.Fatal("structured CLI schema write failed")
			}
			marker := "cli-file-marker-" + bpsUUID()
			input, _ := json.Marshal(map[string]any{"left": 17, "right": 26, "marker": marker})
			if os.WriteFile(filepath.Join(h.work, "fixture.json"), input, 0600) != nil {
				t.Fatal("structured CLI source fixture write failed")
			}
			p := h.recorder.p
			p.BPS.SearchDisabled = true
			base := p.Client.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			var mu sync.Mutex
			posts, terminalCount := 0, 0
			stopped, protocolError, firstPromptLeakedMarker := false, false, false
			clientCalls, markerOutputs := make(map[string]bool), make(map[string]bool)
			formatterNames := make(map[string]bool)
			p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost {
					return base.RoundTrip(request)
				}
				mu.Lock()
				if stopped || posts >= 6 {
					mu.Unlock()
					return nil, errors.New("structured CLI generation boundary reached")
				}
				posts++
				turn := posts
				mu.Unlock()
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				_ = request.Body.Close()
				request.Body, request.GetBody = io.NopCloser(bytes.NewReader(body)), nil
				var payload struct {
					Input []json.RawMessage `json:"input"`
				}
				if json.Unmarshal(body, &payload) != nil {
					return nil, errors.New("invalid actual structured CLI upstream payload")
				}
				mu.Lock()
				if turn == 1 && bytes.Contains(body, []byte(marker)) {
					firstPromptLeakedMarker = true
				}
				for _, item := range payload.Input {
					var value struct {
						Type   string
						CallID string `json:"call_id"`
						Output json.RawMessage
						Tools  []json.RawMessage
					}
					_ = json.Unmarshal(item, &value)
					if (value.Type == "function_call_output" || value.Type == "custom_tool_call_output") && value.CallID != "" && bytes.Contains(value.Output, []byte(marker)) {
						markerOutputs[value.CallID] = true
					}
					for _, definition := range value.Tools {
						var tool struct {
							Name       string
							Parameters json.RawMessage
						}
						if json.Unmarshal(definition, &tool) == nil && strings.HasPrefix(tool.Name, "emit_structured_") {
							formatterNames[tool.Name] = true
						}
					}
				}
				mu.Unlock()
				response, err := base.RoundTrip(request)
				if err != nil {
					return nil, err
				}
				if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
					mu.Lock()
					stopped = true
					mu.Unlock()
				}
				if response.Body != nil {
					response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(wire []byte) {
						canonical, _, _, err := bpsStructuredLiveCanonicalUpstream(wire, strings.HasSuffix(request.URL.Path, "/responses/compact"))
						var terminal struct{ Output []json.RawMessage }
						mu.Lock()
						defer mu.Unlock()
						if err != nil || json.Unmarshal(canonical, &terminal) != nil {
							protocolError = true
							return
						}
						terminalCount++
						for _, item := range terminal.Output {
							var call struct {
								Type, Name string
								CallID     string `json:"call_id"`
							}
							if json.Unmarshal(item, &call) == nil && (call.Type == "function_call" || call.Type == "custom_tool_call") && !formatterNames[call.Name] && call.CallID != "" {
								clientCalls[call.CallID] = true
							}
						}
					}}
				}
				return response, nil
			})
			command := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--sandbox", "workspace-write", "--output-schema", "schema.json", "-"}
			result := h.run(name, "Use a real shell command to read only fixture.json in the current directory. Compute left + right from that file. Your final response must follow the requested JSON schema with numeric sum and the exact marker copied from the file. Never invent a marker. Do not create or edit files, do not use Office tools, and do not read parent directories or access the network.", command...)
			// The CLI can exit immediately after receiving completed, before the
			// producer closes its upstream body and persists the final record.
			deadline := time.Now().Add(3 * time.Second)
			for {
				mu.Lock()
				postCount, doneCount := posts, terminalCount
				mu.Unlock()
				recorded := 0
				for _, record := range p.Metrics.RecentForAccount("test", 50) {
					if record.Endpoint != "models" && (record.WebSocketWarmup == nil || !*record.WebSocketWarmup) {
						recorded++
					}
				}
				if (postCount > 0 && doneCount == postCount && recorded == postCount) || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			var final string
			for _, item := range result.Final {
				if json.Valid([]byte(item)) {
					final = item
				}
			}
			formatInput, _ := json.Marshal(map[string]any{"format": map[string]any{"type": "json_schema", "name": "cli_check", "strict": true, "schema": json.RawMessage(schema)}})
			format, formatErr := bps.ParseStructuredFormat(formatInput)
			validFormat := formatErr == nil && format != nil
			if validFormat {
				_, formatErr = format.ValidateArguments(final)
				validFormat = formatErr == nil
			}
			var actual struct {
				Sum    int
				Marker string
			}
			finalMatches := json.Unmarshal([]byte(final), &actual) == nil && actual.Sum == 43 && actual.Marker == marker
			commandExecuted := false
			for _, kind := range result.ItemTypes {
				commandExecuted = commandExecuted || kind == "command_execution"
			}
			mu.Lock()
			workflow := false
			for id := range markerOutputs {
				workflow = workflow || clientCalls[id]
			}
			upstreamPosts, completedTerminals, upstreamOK, markerLeaked := posts, terminalCount, !stopped && !protocolError, firstPromptLeakedMarker
			var privateNames []string
			for toolName := range formatterNames {
				privateNames = append(privateNames, toolName)
			}
			mu.Unlock()
			rolloutFiles, internalLeak := 0, false
			walkErr := filepath.WalkDir(filepath.Join(h.home, "sessions"), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
					return nil
				}
				rollout, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				rolloutFiles++
				for _, privateName := range privateNames {
					internalLeak = internalLeak || bytes.Contains(rollout, []byte(privateName))
				}
				return nil
			})
			for _, privateName := range privateNames {
				internalLeak = internalLeak || strings.Contains(result.Stderr, privateName)
				for _, output := range result.Final {
					internalLeak = internalLeak || strings.Contains(output, privateName)
				}
			}
			metricsOK, measured := true, 0
			var metrics []map[string]any
			for _, record := range p.Metrics.RecentForAccount("test", 50) {
				if record.Endpoint == "models" || (record.WebSocketWarmup != nil && *record.WebSocketWarmup) {
					continue
				}
				measured++
				wantType := "http_sse"
				if websocket {
					wantType = "websocket"
				}
				metricsOK = metricsOK && record.Status == "ok" && record.RequestType == wantType && record.SentModel == "gpt-6-astra" && record.SentReasoningEffort == "xhigh" && record.ReturnedModel == "gpt-6-astra" && record.UsageKnown != nil && *record.UsageKnown
				metrics = append(metrics, map[string]any{"request_type": record.RequestType, "status": record.Status, "http": record.HTTPCode, "sent_model": record.SentModel, "sent_effort": record.SentReasoningEffort, "returned_model": record.ReturnedModel, "input_tokens": record.InputTokens, "output_tokens": record.OutputTokens, "cache_read_tokens": record.CachedTokens, "cache_read_tokens_known": record.CachedTokensKnown, "cache_write_tokens": record.CacheWriteTokens})
			}
			upgrades := 0
			for _, record := range h.recorder.forCase(name) {
				if record.WebSocketUpgrade && record.Status == http.StatusSwitchingProtocols {
					upgrades++
				}
			}
			passed := result.Completed && validFormat && finalMatches && commandExecuted && workflow && upstreamOK && !markerLeaked && upstreamPosts >= 2 && completedTerminals == upstreamPosts && len(privateNames) == 1 && walkErr == nil && rolloutFiles > 0 && !internalLeak && metricsOK && measured >= 2 && (!websocket || upgrades > 0)
			row := map[string]any{"case": name, "passed": passed, "cli_completed": result.Completed, "cli_exit": result.ExitCode, "timed_out": result.TimedOut, "schema_valid": validFormat, "sum_and_marker_match": finalMatches, "actual_shell_execution": commandExecuted, "actual_tool_call_output_pair_with_marker": workflow, "marker_absent_from_first_prompt": !markerLeaked, "upstream_posts": upstreamPosts, "completed_upstream_terminals": completedTerminals, "formatter_declaration_count": len(privateNames), "cli_rollout_files_checked": rolloutFiles, "internal_formatter_leaked": internalLeak, "successful_ws_upgrades": upgrades, "http_generation_fallback_allowed": !websocket, "official_cli_version": "0.155", "output_schema_flag": true, "automatic_task_retries": 0, "gateway_metrics": metrics}
			if !passed {
				row["cli_errors"], row["cli_stderr"] = result.Errors, h.scrub(result.Stderr)
			}
			evidence = append(evidence, row)
			encoded, _ := json.Marshal(row)
			t.Log(string(encoded))
			if !passed {
				t.Fatal("official structured CLI file/tool/format workflow verification failed")
			}
		}) {
			return
		}
	}
}
