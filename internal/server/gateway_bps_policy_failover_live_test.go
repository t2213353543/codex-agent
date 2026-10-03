package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestBPSLiveRejectedAccountFailover(t *testing.T) {
	file, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || work == "" {
		t.Skip("explicit live two-account fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("account fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) != 2 {
		t.Fatal("two live accounts required")
	}
	// Probe 20260927T142733Z confirmed exported row 0 accepts the request and
	// row 1 rejects it. A caller can explicitly update this index after another
	// account-pair probe; an unexpected outcome fails rather than inventing it.
	badIndex := 1
	if value := os.Getenv("BPS_LIVE_BAD_ACCOUNT_INDEX"); value != "" {
		badIndex, err = strconv.Atoi(value)
		if err != nil || badIndex < 0 || badIndex > 1 {
			t.Fatal("invalid rejected account fixture index")
		}
	}
	good, bad := fixture.Accounts[1-badIndex], fixture.Accounts[badIndex]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if err := bpsLiveWriteEvidence(path, "bps-real-account-rejection-failover", !t.Failed(), evidence); err != nil {
				t.Error("evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, "unused", work, good, []string{good.AccessToken, good.AccountID, bad.AccessToken, bad.AccountID}, &evidence)
	p := h.recorder.p
	encoded, _ := json.Marshal(map[string]any{"access_token": bad.AccessToken, "account_id": bad.AccountID, "expires_at": time.Now().Add(2 * time.Hour).Unix()})
	if err := p.Store.ImportWith("rejected-first", encoded, AccountDefaults{Priority: 100, Weight: 1, Concurrency: 50}); err != nil {
		t.Fatal("isolated rejected fixture import failed")
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 0; a.Weight = 1; a.Concurrency = 50; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	server := httptest.NewServer(p)
	defer server.Close()
	payload := []byte(`{"model":"gpt-6-astra","stream":false,"reasoning":{"effort":"xhigh"},"input":"Reply exactly RELEASE_OK without tools."}`)
	req, _ := http.NewRequest("POST", server.URL+"/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "real-403-failover-"+bpsUUID())
	response, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		t.Fatal("real failover transport failed")
	}
	result, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal("real failover response interrupted")
	}
	var completed struct{ ID, Status, Model string }
	_ = json.Unmarshal(result, &completed)
	if response.StatusCode != 200 || completed.Status != "completed" || completed.Model != "gpt-6-astra" {
		t.Fatalf("real account failover status=%d response_bytes=%d", response.StatusCode, len(result))
	}
	var record RequestRecord
	deadline := time.Now().Add(time.Second)
	for {
		p.Metrics.mu.Lock()
		for _, r := range p.Metrics.recent[:p.Metrics.recentAt] {
			if r.ResponseID == completed.ID {
				record = r
			}
		}
		p.Metrics.mu.Unlock()
		if record.ResponseID != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(record.Attempts) != 2 || record.Attempts[0].StatusCode == nil || *record.Attempts[0].StatusCode != 403 || record.Attempts[0].ErrorCode != "upstream_account_rejected" || record.Attempts[1].StatusCode == nil || *record.Attempts[1].StatusCode != 200 || record.Account != "test" {
		t.Fatalf("expected observed 403 then200, attempts=%d selected=%s", len(record.Attempts), record.Account)
	}
	if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens <= 0 || record.OutputTokens <= 0 || record.TTFTMs == nil {
		t.Fatal("replacement usage/timing missing")
	}
	if p.inFlight("test") != 0 || p.inFlight("rejected-first") != 0 {
		t.Fatal("live failover retained concurrency")
	}
	row := map[string]any{"rejected_fixture_index": badIndex, "http": 200, "model": record.ReturnedModel, "attempts": record.Attempts, "input_tokens": record.InputTokens, "output_tokens": record.OutputTokens, "cached_tokens": record.CachedTokens, "ttft_ms": *record.TTFTMs, "latency_ms": record.LatencyMs, "production_account_changes": false, "production_usage_written": false}
	evidence = append(evidence, row)
	log, _ := json.Marshal(row)
	t.Log(string(log))
}
