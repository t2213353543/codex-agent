package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// One explicitly authorized live generation. Credentials are supplied only by
// the server-side runner; evidence contains protocol/counts and no account data.
func TestBPSLiveUpstreamWebSocketPreference(t *testing.T) {
	path := os.Getenv("BPS_LIVE_ACCOUNTS_FILE")
	if path == "" || os.Getenv("BPS_LIVE_WORK_ROOT") == "" {
		t.Skip("explicit server-side live credential fixture required")
	}
	raw, err := os.ReadFile(path)
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) != 1 {
		t.Fatal("one live account fixture required")
	}
	p := newTestProxy(t, 1)
	p.BPS, p.BaseURL = &bpsRuntime{ToolsVersion: bps.DefaultToolsVersion, SearchDisabled: true}, bps.BaseURL
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccessToken, a.AccountID = fixture.Accounts[0].AccessToken, fixture.Accounts[0].AccountID
		a.RefreshToken, a.Identity = "", nil
		a.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal("live fixture update failed")
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal("live fixture settings unavailable")
	}
	settings.PreferWebsocket, settings.WebsocketEnabled = true, true
	settings.AutomaticTokenRefreshEnabled, settings.Upstream401RecoveryEnabled = false, false
	settings.BuiltinPromptEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal("live fixture settings failed")
	}
	if err := configureGatewayNativeHTTP(p, os.Getenv); err != nil || !p.NativeWebSocket || p.NativeHTTP == nil {
		t.Fatal("explicit native WS helper fixture required")
	}
	t.Cleanup(p.closeGatewayClients)
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"gpt-6-astra","input":"Reply with exactly WS_OK. Do not use any tools.","stream":false,"reasoning":{"effort":"low"}}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	start := time.Now()
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal("live WS request transport failed")
	}
	defer res.Body.Close()
	result, readErr := io.ReadAll(res.Body)
	var response struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	decoded := json.Unmarshal(result, &response) == nil
	text := ""
	for _, item := range response.Output {
		if item.Type == "message" {
			for _, content := range item.Content {
				text += content.Text
			}
		}
	}
	records := p.Metrics.RecentForAccount("test", 2)
	wsAttempts, upgrades := 0, 0
	for _, record := range records {
		for _, attempt := range record.Attempts {
			if attempt.Phase == "websocket" {
				wsAttempts++
				if attempt.ObservedHTTPStatusCode != nil && *attempt.ObservedHTTPStatusCode == http.StatusSwitchingProtocols {
					upgrades++
				}
			}
		}
	}
	passed := readErr == nil && res.StatusCode == http.StatusOK && decoded && response.Status == "completed" && strings.TrimSpace(text) == "WS_OK" && len(records) == 1 && wsAttempts == 1 && upgrades == 1
	evidence := map[string]any{"case": "bps_upstream_ws_preference", "http_status": res.StatusCode, "completed": decoded && response.Status == "completed", "expected_text": strings.TrimSpace(text) == "WS_OK", "websocket_attempts": wsAttempts, "observed_101": upgrades, "elapsed_ms": time.Since(start).Milliseconds(), "generation_requests": 1, "passed": passed}
	if file := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); file != "" {
		if err := bpsLiveWriteEvidence(file, "bps_upstream_websocket", passed, []map[string]any{evidence}); err != nil {
			t.Fatal("live evidence write failed")
		}
	}
	encoded, _ := json.Marshal(evidence)
	t.Log(string(encoded))
	if !passed {
		t.Fatal("live BPS WS end-to-end verification failed")
	}
}
