package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBPSLiveAccountPolicyBoundary(t *testing.T) {
	file, settingsFile, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_SETTINGS_FILE"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || settingsFile == "" || work == "" {
		t.Skip("explicit isolated live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("accounts fixture read failed")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) != 2 {
		t.Fatal("two account fixture required")
	}
	raw, err = os.ReadFile(settingsFile)
	if err != nil {
		t.Fatal("settings fixture read failed")
	}
	var production Settings
	if json.Unmarshal(raw, &production) != nil {
		t.Fatal("settings fixture invalid")
	}
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			_ = bpsLiveWriteEvidence(path, "bps-account-policy-boundary", !t.Failed(), evidence)
		}
	}()
	promptHash := sha256.Sum256([]byte(production.BuiltinPrompt))
	evidence = append(evidence, map[string]any{"kind": "production_prompt", "enabled": production.BuiltinPromptEnabled, "mode": production.BuiltinPromptMode, "bytes": len(production.BuiltinPrompt), "sha256": hex.EncodeToString(promptHash[:])})
	for _, a := range fixture.Accounts {
		for _, builtin := range []bool{false, true} {
			h := newBPSLiveHarness(t, "unused", work, a, []string{a.AccessToken, a.AccountID}, &evidence)
			p := h.recorder.p
			settings, _ := p.Settings.Get()
			settings.BuiltinPromptEnabled = builtin && production.BuiltinPromptEnabled
			settings.BuiltinPrompt = production.BuiltinPrompt
			settings.BuiltinPromptMode = production.BuiltinPromptMode
			settings.CompatibilityNormalizationEnabled = production.CompatibilityNormalizationEnabled
			settings.PromptCacheKeyEnabled = production.PromptCacheKeyEnabled
			if p.Settings.Set(settings) != nil {
				t.Fatal("settings update failed")
			}
			// Production PowerPoint uses the Go HTTP client. Residual Codex
			// native-helper environment settings do not configure this route.
			server := httptest.NewServer(p)
			body := []byte(`{"model":"gpt-6-astra","stream":false,"reasoning":{"effort":"xhigh"},"input":"Reply exactly RELEASE_OK without tools."}`)
			req, _ := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-client-key")
			req.Header.Set("Session-Id", "account-policy-"+bpsUUID())
			req.Header.Set("User-Agent", "gateway-release-validation/0.4.5")
			started := time.Now()
			res, err := (&http.Client{Timeout: 180 * time.Second}).Do(req)
			if err != nil {
				server.Close()
				t.Fatal("isolated policy request transport failed")
			}
			data, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			server.Close()
			if err != nil {
				t.Fatal("policy response read failed")
			}
			var object map[string]json.RawMessage
			_ = json.Unmarshal(data, &object)
			row := map[string]any{"kind": "request", "account_alias": a.Alias, "builtin_prompt_enabled": settings.BuiltinPromptEnabled, "http": res.StatusCode, "elapsed_ms": time.Since(started).Milliseconds(), "bytes": len(data)}
			hash := sha256.Sum256(data)
			row["response_sha256"] = hex.EncodeToString(hash[:])
			if res.StatusCode >= 400 {
				var detail map[string]json.RawMessage
				_ = json.Unmarshal(object["error"], &detail)
				for _, field := range []string{"type", "code", "param"} {
					var value string
					if json.Unmarshal(detail[field], &value) == nil && len(value) < 100 && !strings.ContainsAny(value, " \r\n") {
						row["error_"+field] = value
					} else {
						row["error_"+field+"_json_type"] = "null_or_other"
					}
				}
				message := errorFromPayload(data)
				row["usage_policy_message"] = message == "403: This request was blocked by our usage policy." || message == "This request was blocked by our usage policy."
				row["error_message_sha256"] = func() string { x := sha256.Sum256([]byte(message)); return hex.EncodeToString(x[:]) }()
			} else {
				var model string
				_ = json.Unmarshal(object["model"], &model)
				row["returned_model"] = model
				row["usage_schema"] = bpsCacheUsageShape(object["usage"])
				row["completed"] = string(object["status"]) == `"completed"`
			}
			evidence = append(evidence, row)
			encoded, _ := json.Marshal(row)
			t.Log(string(encoded))
		}
	}
}
