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

func TestOfficialLiveUsageAndCache(t *testing.T) {
	runOfficialLiveUsageAndCache(t, false)
}

// Run against real upstream usage, with a fresh official process/thread each
// time. Synthetic input is isolated from production statistics; no cache
// response, usage counter, or production request is manufactured by this test.
func TestOfficialLiveCacheAcrossThreadRebuilds(t *testing.T) {
	runOfficialLiveUsageAndCache(t, true)
}

func runOfficialLiveUsageAndCache(t *testing.T, rebuild bool) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit live environment required")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("credential fixture missing")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid fixture")
	}
	a := fixture.Accounts[0]
	evidence := []map[string]any{}
	h := newBPSLiveHarness(t, binary, work, a, []string{a.AccessToken, a.AccountID}, &evidence)
	p := h.recorder.p
	p.RuntimeModes = true
	p.Official, err = newOfficialRuntime(func(k string) string {
		if k == "CODEX_CLI_PATH" {
			return binary
		}
		if k == "CODEX_RUNTIME_DIR" {
			return filepath.Join(work, "usage-runtime")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Official.Close() })
	s, _ := p.Settings.Get()
	s.InferenceMode = inferenceAppServer
	s.BuiltinPromptEnabled = false
	_ = p.Settings.Set(s)
	server := httptest.NewServer(p)
	defer server.Close()
	long := strings.Repeat("Stable cache fixture reference paragraph. The reference is background data; do not summarize or use tools. ", 220)
	input := []json.RawMessage{json.RawMessage(mustJSONUsage(map[string]any{"role": "user", "content": long + "\nReply exactly CACHE_OK."}))}
	session := "usage-" + bpsUUID()
	cached := false
	turns := 3
	if rebuild {
		turns = 5
	}
	var inputTotal, cachedTotal, warmInput, warmCached int64
	for turn := 0; turn < turns; turn++ {
		if rebuild {
			session = "rebuild-" + bpsUUID()
		}
		if turn > 0 {
			input = append(input, json.RawMessage(mustJSONUsage(map[string]any{"role": "user", "content": "Again reply exactly CACHE_OK without tools."})))
		}
		body := mustJSONUsage(map[string]any{"model": "gpt-5.6-sol", "reasoning": map[string]string{"effort": "xhigh"}, "stream": true, "input": input})
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Session-Id", session)
		client := &http.Client{Timeout: 3 * time.Minute}
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("usage live transport failed")
		}
		raw, e := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		_ = res.Body.Close()
		if e != nil || res.StatusCode != 200 {
			t.Fatalf("usage response status %d", res.StatusCode)
		}
		var completed struct {
			ID, Model string
			Output    []json.RawMessage `json:"output"`
			Usage     struct {
				Input   int64 `json:"input_tokens"`
				Output  int64 `json:"output_tokens"`
				Details struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		}
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			_ = json.Unmarshal(line[6:], &event)
			if event.Type == "response.completed" {
				_ = json.Unmarshal(event.Response, &completed)
			}
		}
		if completed.ID == "" || completed.Usage.Input < 1000 || completed.Usage.Output <= 0 {
			t.Fatal("missing actual upstream token usage")
		}
		p.Metrics.mu.Lock()
		records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
		p.Metrics.mu.Unlock()
		var record RequestRecord
		for _, v := range records {
			if v.ResponseID == completed.ID {
				record = v
			}
		}
		if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != completed.Usage.Input || record.OutputTokens != completed.Usage.Output || record.CachedTokens != completed.Usage.Details.Cached {
			t.Fatalf("stream vs persisted usage mismatch: input=%d/%d cached=%d/%d", record.InputTokens, completed.Usage.Input, record.CachedTokens, completed.Usage.Details.Cached)
		}
		if record.ReturnedModel != completed.Model || !strings.HasPrefix(record.UpstreamModelSource, "app_server_") || record.TTFTMs == nil || record.OfficialRuntimeMs == nil || record.OfficialInferenceMs == nil {
			t.Fatal("model provenance or timing missing")
		}
		if record.CachedTokens > 0 {
			cached = true
		}
		inputTotal += record.InputTokens
		cachedTotal += record.CachedTokens
		if turn > 0 {
			warmInput += record.InputTokens
			warmCached += record.CachedTokens
		}
		t.Logf("turn=%d input=%d output=%d cached=%d model=%s model_source=%s ttft_ms=%d runtime_ms=%d inference_ms=%d latency_ms=%d", turn, record.InputTokens, record.OutputTokens, record.CachedTokens, record.ReturnedModel, record.UpstreamModelSource, *record.TTFTMs, *record.OfficialRuntimeMs, *record.OfficialInferenceMs, record.LatencyMs)
		input = append(input, completed.Output...)
	}
	if !cached {
		t.Fatal("no real upstream cache hit observed in three stable-context turns")
	}
	if rebuild {
		t.Logf("isolated_rebuild_workload input=%d cached=%d cache_percent=%.2f warm_input=%d warm_cached=%d warm_cache_percent=%.2f", inputTotal, cachedTotal, 100*float64(cachedTotal)/float64(inputTotal), warmInput, warmCached, 100*float64(warmCached)/float64(warmInput))
		if cachedTotal*100 < inputTotal*70 {
			t.Fatal("actual upstream cache ratio below 70% for the isolated stable-prefix rebuild workload")
		}
	}
}
func mustJSONUsage(value any) []byte { data, _ := json.Marshal(value); return data }
