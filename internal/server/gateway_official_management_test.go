package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A subprocess exercises the real official RPC transport, initialization,
// authentication and cleanup without relying on account credentials or network.
func TestOfficialManagementRPCFixture(t *testing.T) {
	fixture, broken, refreshBeforeModels, catalogUnauthorized := false, false, false, false
	for _, arg := range os.Args {
		if arg == "--management-rpc-fixture" {
			fixture = true
		}
		if arg == "--broken-stream" {
			broken = true
		}
		if arg == "--refresh-before-models" {
			refreshBeforeModels = true
		}
		if arg == "--catalog-unauthorized" {
			catalogUnauthorized = true
		}
	}
	if !fixture {
		t.Skip("subprocess fixture")
	}
	enc := json.NewEncoder(os.Stdout)
	notify := func(method string, params map[string]any) {
		_ = enc.Encode(map[string]any{"method": method, "params": params})
	}
	model, logged := "", false
	loginCount := 0
	rollout := filepath.Join(os.Getenv("CODEX_HOME"), "fixture-rollout.jsonl")
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if len(req.ID) == 0 {
			continue
		}
		reply := func(v any) { _ = enc.Encode(map[string]any{"id": req.ID, "result": v}) }
		switch req.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fixture"})
		case "account/login/start":
			if string(req.Params["accessToken"]) != `"SENSITIVE_FIXTURE_TOKEN"` && string(req.Params["accessToken"]) != `"SENSITIVE_FIXTURE_TOKEN_REFRESHED"` && string(req.Params["accessToken"]) != `"SENSITIVE_FIXTURE_TOKEN_RESTART"` || string(req.Params["chatgptAccountId"]) != `"fixture-account"` {
				os.Exit(3)
			}
			if logged && string(req.Params["accessToken"]) == `"SENSITIVE_FIXTURE_TOKEN_RESTART"` {
				_ = enc.Encode(map[string]any{"id": req.ID, "error": map[string]any{"message": "fixture reauthentication rejected"}})
				continue
			}
			logged = true
			loginCount++
			reply(map[string]any{})
		case "fixture/login_count":
			reply(map[string]any{"count": loginCount})
		case "model/list":
			if !logged || string(req.Params["includeHidden"]) != "true" || string(req.Params["limit"]) != "100" {
				os.Exit(4)
			}
			if catalogUnauthorized && loginCount < 2 {
				fmt.Fprintln(os.Stderr, "ERROR codex_models_manager::manager: failed to refresh available models: unexpected status 401 Unauthorized")
				reply(map[string]any{"data": []any{}, "nextCursor": nil})
				continue
			}
			if refreshBeforeModels {
				refreshBeforeModels = false
				_ = enc.Encode(map[string]any{"id": "refresh-fixture", "method": "account/chatgptAuthTokens/refresh", "params": map[string]string{"reason": "unauthorized", "previousAccountId": "fixture-account"}})
				if !scan.Scan() {
					os.Exit(8)
				}
				var refreshed struct {
					ID     string `json:"id"`
					Result struct {
						AccessToken string `json:"accessToken"`
						AccountID   string `json:"chatgptAccountId"`
					} `json:"result"`
				}
				if json.Unmarshal(scan.Bytes(), &refreshed) != nil || refreshed.ID != "refresh-fixture" || refreshed.Result.AccessToken != "SENSITIVE_FIXTURE_TOKEN_REFRESHED" || refreshed.Result.AccountID != "fixture-account" || strings.Contains(scan.Text(), "refreshToken") {
					os.Exit(9)
				}
			}
			_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "models_cache.json"), []byte(`{"models":[{"slug":"model-a","tool_mode":"code_mode_only","base_instructions":"fixture model instructions","context_window":8192},{"slug":"model-b","tool_mode":"direct"}]}`), 0600)
			if string(req.Params["cursor"]) == `"page-2"` {
				reply(map[string]any{"data": []any{map[string]any{"id": "model-b", "displayName": "Model B"}}, "nextCursor": nil})
			} else {
				reply(map[string]any{"data": []any{map[string]any{"id": "internal-id", "model": "model-a", "displayName": "Model A", "contextWindow": 8192}}, "nextCursor": "page-2"})
			}
		case "thread/start":
			if !logged {
				os.Exit(5)
			}
			_ = json.Unmarshal(req.Params["model"], &model)
			reply(map[string]any{"thread": map[string]any{"id": "thread-managed", "path": rollout}, "model": model})
		case "thread/inject_items":
			reply(map[string]any{})
		case "turn/start":
			if string(req.Params["effort"]) != `"xhigh"` {
				os.Exit(6)
			}
			reply(map[string]any{"turn": map[string]any{"id": "turn-managed"}})
			notify("item/agentMessage/delta", map[string]any{"threadId": "thread-managed", "turnId": "turn-managed", "itemId": "msg-managed", "delta": "OK"})
			notify("rawResponseItem/completed", map[string]any{"threadId": "thread-managed", "turnId": "turn-managed", "item": map[string]any{"id": "msg-managed", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "OK"}}}})
			if !broken {
				notify("rawResponse/completed", map[string]any{"threadId": "thread-managed", "turnId": "turn-managed", "responseId": "response-managed", "usage": map[string]any{"inputTokens": 17, "outputTokens": 2, "totalTokens": 19}})
			}
			notify("turn/completed", map[string]any{"threadId": "thread-managed", "turn": map[string]any{"id": "turn-managed", "status": "completed"}})
		case "thread/compact/start":
			reply(map[string]any{})
			checkpoint := `{"type":"compacted","payload":{"compaction_response_id":"compact-managed","replacement_history":[{"type":"compaction","encrypted_content":"PRIVATE_COMPACT_CIPHER"}]}}` + "\n"
			if os.WriteFile(rollout, []byte(checkpoint), 0600) != nil {
				os.Exit(7)
			}
			notify("turn/started", map[string]any{"threadId": "thread-managed", "turn": map[string]any{"id": "turn-compact"}})
			notify("rawResponse/completed", map[string]any{"threadId": "thread-managed", "turnId": "turn-compact", "responseId": "compact-managed"})
			notify("turn/completed", map[string]any{"threadId": "thread-managed", "turn": map[string]any{"id": "turn-compact", "status": "completed"}})
		default:
			_ = enc.Encode(map[string]any{"id": req.ID, "error": map[string]any{"message": "unexpected method"}})
		}
	}
	os.Exit(0)
}

func officialManagedFixture(t *testing.T, broken bool) (*Proxy, Account) {
	t.Helper()
	rt, err := newOfficialRuntime(func(key string) string {
		if key == "CODEX_RUNTIME_DIR" {
			return t.TempDir()
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"-test.run=^TestOfficialManagementRPCFixture$", "--", "--management-rpc-fixture"}
		if broken {
			args = append(args, "--broken-stream")
		}
		return exec.CommandContext(ctx, executable, args...)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return &Proxy{Official: rt}, Account{ID: "fixture-id", Name: "fixture", Version: 3, CredentialVersion: 2, AccessToken: "SENSITIVE_FIXTURE_TOKEN", AccountID: "fixture-account"}
}

func assertOfficialManagedClean(t *testing.T, p *Proxy) {
	t.Helper()
	p.Official.mu.Lock()
	threads, processes := len(p.Official.threads), len(p.Official.processes)
	p.Official.mu.Unlock()
	entries, err := os.ReadDir(p.Official.root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if threads != 0 || processes != 0 || len(entries) != 0 {
		t.Fatalf("management RPC leaked: threads=%d processes=%d directories=%d", threads, processes, len(entries))
	}
}

func TestOfficialManagedModelsStreamCompactAndCleanup(t *testing.T) {
	for _, kind := range []string{"models", "stream", "compact"} {
		t.Run(kind, func(t *testing.T) {
			p, account := officialManagedFixture(t, false)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var phases, deltas []string
			got := p.checkOfficialManaged(ctx, account, kind, "model-a", "Reply OK", func(v string) error { phases = append(phases, v); return nil }, func(v string) error {
				if len(phases) == 0 {
					t.Error("delta preceded phase")
				}
				deltas = append(deltas, v)
				return nil
			})
			if !got.OK || got.HTTPCode != 200 || got.Source != "official_app_server_"+kind || got.ID != account.ID || got.Version != 3 || got.CredentialVersion != 2 || got.CheckedAt == 0 || got.LatencyMs < 0 {
				t.Fatalf("invalid snapshot: %+v", got)
			}
			raw, _ := json.Marshal(got)
			for _, secret := range []string{account.AccessToken, "PRIVATE_COMPACT_CIPHER", p.Official.root} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("snapshot leaked secret/runtime state: %s", raw)
				}
			}
			data, ok := got.Data.(map[string]any)
			if !ok {
				t.Fatalf("unexpected data %T", got.Data)
			}
			switch kind {
			case "models":
				models := data["models"].([]managedModel)
				if len(models) != 2 || models[0].ID != "model-a" || models[0].DisplayName != "Model A" || models[0].ContextWindow == nil || *models[0].ContextWindow != 8192 || models[1].ID != "model-b" || models[1].ContextWindow != nil {
					t.Fatalf("catalog was invented/truncated: %+v", models)
				}
			case "stream":
				if data["completed"] != true || data["text"] != "OK" || strings.Join(deltas, "") != "OK" || fmt.Sprint(phases) != "[streaming]" {
					t.Fatalf("stream summary/delta mismatch: %+v %v %v", data, phases, deltas)
				}
				if !strings.Contains(string(raw), `"input_tokens":17`) || !strings.Contains(string(raw), `"total_tokens":19`) {
					t.Fatalf("stream usage lost: %s", raw)
				}
			case "compact":
				if data["compaction_found"] != true {
					t.Fatalf("missing compact check: %+v", data)
				}
			}
			assertOfficialManagedClean(t, p)
		})
	}
}

func TestOfficialManagedFailedTerminalAndDisconnectedClientCleanUp(t *testing.T) {
	for _, scenario := range []string{"incomplete", "phase-error", "delta-error"} {
		t.Run(scenario, func(t *testing.T) {
			p, a := officialManagedFixture(t, scenario == "incomplete")
			phase := func(string) error {
				if scenario == "phase-error" {
					return errors.New("private callback failure")
				}
				return nil
			}
			delta := func(string) error {
				if scenario == "delta-error" {
					return errors.New("private callback failure")
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got := p.checkOfficialManaged(ctx, a, "stream", "model-a", "Reply OK", phase, delta)
			if got.OK || got.Error == nil || strings.Contains(got.Error.Message, "private callback") {
				t.Fatalf("failed stream returned success/secret: %+v", got)
			}
			assertOfficialManagedClean(t, p)
		})
	}
}
