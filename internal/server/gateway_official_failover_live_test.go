package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfficialLiveAccountFailover(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit live environment required")
	}
	raw, err := os.ReadFile(file)
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if err != nil || json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("credential fixture unavailable")
	}
	account := fixture.Accounts[0]
	evidence := []map[string]any{}
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.RuntimeModes = true
	p.Official, err = newOfficialRuntime(func(k string) string {
		if k == "CODEX_CLI_PATH" {
			return binary
		}
		if k == "CODEX_RUNTIME_DIR" {
			return filepath.Join(work, "failover-runtime")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Official.Close() })
	// Corrupt only a local copy's signature. Production credentials and refresh
	// tokens remain untouched; the high-priority account gets a real 401.
	parts := strings.Split(account.AccessToken, ".")
	if len(parts) != 3 || len(parts[2]) < 2 {
		t.Fatal("expected signed account token")
	}
	letter := byte('A')
	if parts[2][0] == letter {
		letter = 'B'
	}
	parts[2] = string(letter) + parts[2][1:]
	bad, _ := json.Marshal(map[string]any{"access_token": strings.Join(parts, "."), "account_id": account.AccountID, "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("unavailable-priority", bad, AccountDefaults{Concurrency: 4, Priority: 100, Weight: 100}); err != nil {
		t.Fatal("failed to build isolated priority account")
	}
	p.invalidateAccounts()
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceAppServer
	settings.Upstream401RecoveryEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	body := mustJSONUsage(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": true, "input": "Reply exactly FAILOVER_OK. Do not use tools."})
	req, _ := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "live-failover-"+bpsUUID())
	client := &http.Client{Timeout: 2 * time.Minute}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal("live failover request failed")
	}
	defer res.Body.Close()
	wire, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("live failover status %d", res.StatusCode)
	}
	created, completed := 0, 0
	var text strings.Builder
	for _, line := range bytes.Split(wire, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var event struct{ Type, Delta string }
		if json.Unmarshal(line[6:], &event) != nil {
			continue
		}
		switch event.Type {
		case "error", "response.failed":
			t.Fatal("intermediate account failure leaked to client")
		case "response.created":
			created++
		case "response.completed":
			completed++
		case "response.output_text.delta":
			text.WriteString(event.Delta)
		}
	}
	if created != 1 || completed != 1 || strings.TrimSpace(text.String()) != "FAILOVER_OK" {
		t.Fatal("final failover response missing or duplicated")
	}
	p.Metrics.mu.Lock()
	records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
	p.Metrics.mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("one request produced %d usage records", len(records))
	}
	r := records[0]
	if r.Status != "ok" || r.Account != "test" || r.UsageKnown == nil || !*r.UsageKnown || r.InputTokens <= 0 || len(r.Attempts) != 2 || r.Attempts[0].StatusCode == nil || *r.Attempts[0].StatusCode != 401 || r.TTFTMs == nil {
		t.Fatalf("failover accounting missing: status=%s account=%s attempts=%d", r.Status, r.Account, len(r.Attempts))
	}
	t.Logf("real expired-signature 401 -> healthy account: attempts=%d input=%d output=%d cached=%d ttft_ms=%d total_ms=%d", len(r.Attempts), r.InputTokens, r.OutputTokens, r.CachedTokens, *r.TTFTMs, r.LatencyMs)
}
