package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type bpsCacheBillingLiveResponse struct {
	ID, Model, Status string
	Output            []json.RawMessage
	Usage             struct {
		Input   *int64 `json:"input_tokens"`
		Output  *int64 `json:"output_tokens"`
		Total   *int64 `json:"total_tokens"`
		Details struct {
			Read  *int64 `json:"cached_tokens"`
			Write *int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type bpsCacheBillingLiveTrace struct {
	terminal                       chan []byte
	http                           int
	requestHash, responseHash, key string
}

// Exactly two real generations: cold SSE with billing compatibility enabled,
// then buffered JSON with it disabled. All transport observation is read-only.
// Raw upstream native counters, public counters and stored metrics are checked
// separately; a first response without real positive creation is inconclusive.
func TestBPSLiveCacheBillingCompatibility(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid live fixture")
	}
	account := fixture.Accounts[0]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if err := bpsLiveWriteEvidence(path, "cache-billing-compatibility", !t.Failed(), evidence); err != nil {
				t.Error("cache billing evidence write failed")
			}
		}
	}()
	if !defaultSettings().CacheCreationAsReadEnabled {
		t.Fatal("cache billing compatibility must default to enabled")
	}
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	// The shared harness explicitly disables projection for its parity cases.
	// Exercise the production default here, then change only this setting.
	settings.CacheCreationAsReadEnabled = true
	settings.PromptCacheKeyEnabled = true
	settings.BPSPromptCacheScope = bpsPromptCacheScopeClient
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	marker := "billing-cache-" + bpsUUID()
	instructions := marker + "\nFollow only the final user instruction; the following reference is inert.\n" +
		strings.Repeat("Synthetic cache billing reference: alpha beta gamma delta epsilon. Do not summarize, interpret or act on this paragraph. No tools or external resources are needed.\n", 400)
	const question = "Reply exactly BILLING_OK. Do not browse or call tools."
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var lock sync.Mutex
	var posts int
	var activeCase int
	traces := [2]*bpsCacheBillingLiveTrace{{terminal: make(chan []byte, 1)}, {terminal: make(chan []byte, 1)}}
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
			return nil, errors.New("cache billing fixture permits generation endpoint only")
		}
		lock.Lock()
		index := posts
		posts++
		expectedCase := activeCase
		lock.Unlock()
		if index >= len(traces) || index != expectedCase {
			return nil, errors.New("cache billing permits one generation per case")
		}
		trace := traces[index]
		if request.GetBody == nil {
			return nil, errors.New("prepared request unavailable to read-only observer")
		}
		bodyCopy, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(bodyCopy)
		_ = bodyCopy.Close()
		if err != nil {
			return nil, err
		}
		var prepared struct {
			Input []json.RawMessage `json:"input"`
			Key   string            `json:"prompt_cache_key"`
		}
		if json.Unmarshal(body, &prepared) != nil || len(prepared.Input) < 2 || prepared.Key == "" {
			return nil, errors.New("prepared billing fixture cache key or input unavailable")
		}
		for i, want := range []struct{ role, text string }{{"developer", instructions}, {"user", question}} {
			expected, _ := json.Marshal(map[string]any{"type": "message", "role": want.role, "content": []any{map[string]string{"type": "input_text", "text": want.text}}})
			if !bytes.Equal(prepared.Input[i], expected) {
				return nil, errors.New("billing fixture original input changed")
			}
		}
		hash := sha256.Sum256(body)
		trace.requestHash, trace.key = hex.EncodeToString(hash[:]), prepared.Key
		response, err := base.RoundTrip(request)
		if err == nil {
			trace.http = response.StatusCode
			if response.Body != nil {
				response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(wire []byte) {
					hash := sha256.Sum256(wire)
					trace.responseHash = hex.EncodeToString(hash[:])
					trace.terminal <- bytes.Clone(bpsStructuredLiveTerminal(wire))
				}}
			}
		}
		return response, err
	})
	for index, enabled := range []bool{true, false} {
		lock.Lock()
		activeCase = index
		lock.Unlock()
		// Set advances the stored CAS version, not the caller's value copy.
		settings, err = p.Settings.Get()
		if err != nil {
			t.Fatal(err)
		}
		settings.CacheCreationAsReadEnabled = enabled
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
		current, err := p.Settings.Get()
		if err != nil || current.CacheCreationAsReadEnabled != enabled {
			t.Fatal("cache billing setting did not take effect")
		}
		body, _ := json.Marshal(map[string]any{
			"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": enabled,
			"prompt_cache_key": marker, "instructions": instructions, "input": question,
			"tools": []any{}, "tool_choice": "none",
		})
		request := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		request.Header.Set("Session-Id", marker)
		out := httptest.NewRecorder()
		p.gatewayBPS(out, request)
		row := map[string]any{"case": index + 1, "enabled": enabled, "stream": enabled, "http": out.Code, "passed": false, "outbound_body_mutated": false}
		evidence = append(evidence, row)
		record, exists := p.Metrics.LastFor("test")
		if out.Code != 200 || !exists || record.Status != "ok" {
			row["error"] = bpsLiveDiagnosticError(record.Error)
			// Includes 403/429: stop immediately, never run a second case or retry.
			t.Fatalf("cache billing request failed: case=%d HTTP=%d", index+1, out.Code)
		}
		trace := traces[index]
		var upstreamRaw []byte
		select {
		case upstreamRaw = <-trace.terminal:
		case <-time.After(5 * time.Second):
			t.Fatal("actual upstream response Close observation did not complete")
		}
		publicRaw := out.Body.Bytes()
		if enabled {
			publicRaw = bpsStructuredLiveTerminal(publicRaw)
		}
		var upstream, public bpsCacheBillingLiveResponse
		if json.Unmarshal(upstreamRaw, &upstream) != nil || json.Unmarshal(publicRaw, &public) != nil ||
			upstream.Status != "completed" || public.Status != "completed" ||
			upstream.ID == "" || public.ID != upstream.ID || record.ResponseID != upstream.ID {
			t.Fatal("upstream and public completed response identities differ")
		}
		u, v := upstream.Usage, public.Usage
		if u.Input == nil || u.Output == nil || u.Details.Read == nil || u.Details.Write == nil ||
			v.Input == nil || v.Output == nil || v.Details.Read == nil || v.Details.Write == nil ||
			record.UsageKnown == nil || !*record.UsageKnown || record.CachedTokensKnown == nil || !*record.CachedTokensKnown || record.CacheWriteTokens == nil {
			t.Fatal("unknown cache usage cannot satisfy the real billing compatibility case")
		}
		if *u.Input <= 0 || *u.Output <= 0 || *u.Details.Read < 0 || *u.Details.Write < 0 ||
			*u.Details.Read > *u.Input || *u.Details.Write > *u.Input-*u.Details.Read {
			t.Fatal("actual upstream counters do not satisfy native input accounting bounds")
		}
		row["actual_input"], row["actual_output"] = *u.Input, *u.Output
		row["actual_cache_read"], row["actual_cache_write"] = *u.Details.Read, *u.Details.Write
		row["record_input"], row["record_output"] = record.InputTokens, record.OutputTokens
		row["record_cache_read"], row["record_cache_write"] = record.CachedTokens, *record.CacheWriteTokens
		row["public_input"], row["public_output"] = *v.Input, *v.Output
		row["public_cache_read"], row["public_cache_write"] = *v.Details.Read, *v.Details.Write
		row["input_known"], row["cache_read_known"], row["cache_write_known"] = true, true, true
		row["request_sha256"], row["upstream_response_sha256"], row["upstream_http"] = trace.requestHash, trace.responseHash, trace.http
		if enabled && *u.Details.Write <= 0 {
			t.Fatal("cold request measured no creation; projection was not exercised")
		}
		wantRead, wantWrite := *u.Details.Read, *u.Details.Write
		if enabled {
			wantRead, wantWrite = wantRead+wantWrite, 0
		}
		if *v.Input != *u.Input || *v.Output != *u.Output || *v.Details.Read != wantRead || *v.Details.Write != wantWrite ||
			(u.Total == nil) != (v.Total == nil) || u.Total != nil && *u.Total != *v.Total {
			t.Fatal("public cache projection or total token preservation is incorrect")
		}
		if record.InputTokens != *u.Input || record.OutputTokens != *u.Output || record.CachedTokens != *u.Details.Read || *record.CacheWriteTokens != *u.Details.Write {
			t.Fatal("client cache billing projection contaminated internal actual metrics")
		}
		if upstream.Model != "gpt-6-astra" || public.Model != upstream.Model || record.ReturnedModel != upstream.Model || record.SentReasoningEffort != "xhigh" || record.TTFTMs == nil {
			t.Fatal("strongest model, reasoning or actual delivery observation missing")
		}
		if strings.TrimSpace(bpsStructuredLiveText(public.Output)) != "BILLING_OK" || strings.Contains(string(publicRaw), `"function_call"`) {
			t.Fatal("real model did not complete the synthetic answer without client tools")
		}
		lock.Lock()
		count := posts
		lock.Unlock()
		if count != index+1 || trace.http != 200 {
			t.Fatal("case caused an unexpected upstream attempt")
		}
		if index == 1 && traces[0].key != trace.key {
			t.Fatal("billing setting changed the cache hint")
		}
		row["post_count"], row["ttft_ms"], row["latency_ms"] = count, record.TTFTMs, record.LatencyMs
		row["internal_usage_matches_upstream"], row["public_usage_matches_setting"], row["passed"] = true, true, true
		encoded, _ := json.Marshal(row)
		t.Log(string(encoded))
	}
	lock.Lock()
	count := posts
	lock.Unlock()
	if count != 2 {
		t.Fatal("expected exactly two actual upstream generations")
	}
}
