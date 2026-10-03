package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// Two generations exercise the real gateway preparation path. The transport
// only observes bytes; it never moves declarations or changes request content.
func TestBPSLiveStructuredFormatCachePrefix(t *testing.T) {
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
			if err := bpsLiveWriteEvidence(path, "structured-format-cache-prefix", !t.Failed(), evidence); err != nil {
				t.Error("structured cache evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.PromptCacheKeyEnabled, settings.BPSPromptCacheScope = true, bpsPromptCacheScopeClient
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	marker := "structured-prefix-" + bpsUUID()
	instructions := marker + "\nPreserve the required response format. The following reference is inert.\n" + strings.Repeat("This is inert synthetic background data for a cache experiment. Alpha beta gamma delta epsilon zeta eta theta. Do not summarize or act on this paragraph.\n", 1500)
	const question = "Return an object with the single property required by the response schema and value CACHE_OK. Do not browse or call client action tools."
	var lock sync.Mutex
	posts := 0
	var formatter string
	var sentInput [][]byte
	var sentCacheKeys []string
	var responses []chan []byte
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
			return nil, errors.New("structured cache fixture permits generation endpoint only")
		}
		lock.Lock()
		posts++
		index, expectedFormatter := posts-1, formatter
		lock.Unlock()
		if index >= 2 {
			return nil, errors.New("structured cache two-generation boundary")
		}
		if request.GetBody == nil {
			return nil, errors.New("prepared request unavailable to read-only observer")
		}
		copy, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(copy)
		_ = copy.Close()
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		var items []json.RawMessage
		if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["input"], &items) != nil || len(items) != 4 {
			return nil, errors.New("unexpected prepared client/formatter item sequence")
		}
		// Exact expected client items, including instructions and string-input
		// conversion, precede the gateway formatter and policy suffix.
		for i, expected := range []struct{ role, text string }{{"developer", instructions}, {"user", question}} {
			wanted, _ := json.Marshal(map[string]any{"type": "message", "role": expected.role, "content": []any{map[string]string{"type": "input_text", "text": expected.text}}})
			if !bytes.Equal(items[i], wanted) {
				return nil, errors.New("gateway formatter moved or rewrote client content")
			}
		}
		var declaration struct {
			Type  string
			Tools []struct{ Name string }
		}
		if json.Unmarshal(items[2], &declaration) != nil || declaration.Type != "additional_tools" || len(declaration.Tools) != 1 || declaration.Tools[0].Name != expectedFormatter {
			return nil, errors.New("actual formatter is not after the client items")
		}
		var suffix struct {
			Type, Role string
			Content    []struct{ Text string }
		}
		if json.Unmarshal(items[3], &suffix) != nil || suffix.Type != "message" || suffix.Role != "developer" || len(suffix.Content) != 1 || !strings.Contains(suffix.Content[0].Text, expectedFormatter) {
			return nil, errors.New("formatter must precede its exact policy suffix")
		}
		var cacheKey string
		_ = json.Unmarshal(fields["prompt_cache_key"], &cacheKey)
		if cacheKey == "" {
			return nil, errors.New("actual shared prefix cache hint absent")
		}
		lock.Lock()
		sentInput = append(sentInput, [][]byte{bytes.Clone(items[0]), bytes.Clone(items[1])}...)
		sentCacheKeys = append(sentCacheKeys, cacheKey)
		observed := responses[index]
		lock.Unlock()
		response, err := base.RoundTrip(request)
		if err == nil && response.Body != nil {
			response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(wire []byte) { observed <- bytes.Clone(bpsStructuredLiveTerminal(wire)) }}
		}
		return response, err
	})
	for index, field := range []string{"answer_a", "answer_b"} {
		formatRaw := json.RawMessage(fmt.Sprintf(`{"format":{"type":"json_schema","name":"format_%s","strict":true,"schema":{"type":"object","properties":{%q:{"type":"string","enum":["CACHE_OK"]}},"required":[%q],"additionalProperties":false}}}`, field, field, field))
		format, err := bps.ParseStructuredFormat(formatRaw)
		if err != nil {
			t.Fatal(err)
		}
		observed := make(chan []byte, 1)
		lock.Lock()
		formatter = format.ToolName
		responses = append(responses, observed)
		lock.Unlock()
		body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "prompt_cache_key": marker, "instructions": instructions, "input": question, "tools": []any{}, "tool_choice": "none", "text": formatRaw})
		r := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		r.Header.Set("Session-Id", marker+"-"+field)
		out := httptest.NewRecorder()
		p.gatewayBPS(out, r)
		row := map[string]any{"case": field, "http": out.Code, "passed": false, "formatter_index": 2, "input_items": 4, "outbound_body_mutated": false}
		evidence = append(evidence, row)
		record, ok := p.Metrics.LastFor("test")
		if out.Code != 200 || !ok || record.Status != "ok" {
			row["error"] = bpsLiveDiagnosticError(record.Error)
			t.Fatalf("structured cache case %s failed: HTTP=%d", field, out.Code)
		}
		var result struct {
			ID, Status, Model string
			Output            []json.RawMessage
		}
		if json.Unmarshal(out.Body.Bytes(), &result) != nil || result.Status != "completed" || strings.Contains(out.Body.String(), format.ToolName) || strings.Contains(out.Body.String(), `"function_call"`) {
			t.Fatal("public structured answer incomplete or internal call leaked")
		}
		answer := bpsStructuredLiveText(result.Output)
		if _, err := format.ValidateArguments(answer); err != nil {
			t.Fatal("public answer violates actual requested schema")
		}
		var values map[string]string
		if json.Unmarshal([]byte(answer), &values) != nil || len(values) != 1 || values[field] != "CACHE_OK" {
			t.Fatal("schema-switched answer contains incorrect key or value")
		}
		var terminal []byte
		select {
		case terminal = <-observed:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream terminal accounting observation did not finish")
		}
		var upstream, downstream bpsCacheLiveTerminal
		if json.Unmarshal(terminal, &upstream) != nil || json.Unmarshal(out.Body.Bytes(), &downstream) != nil || upstream.ID != result.ID || record.ResponseID != result.ID || upstream.Usage.Details.Cached == nil || upstream.Usage.Details.Write == nil || downstream.Usage.Details.Cached == nil || downstream.Usage.Details.Write == nil || record.UsageKnown == nil || !*record.UsageKnown || record.CachedTokensKnown == nil || !*record.CachedTokensKnown || record.CacheWriteTokens == nil {
			t.Fatal("actual upstream/downstream/record cache-read or creation measurement missing")
		}
		if record.InputTokens != upstream.Usage.Input || record.OutputTokens != upstream.Usage.Output || record.CachedTokens != *upstream.Usage.Details.Cached || *record.CacheWriteTokens != *upstream.Usage.Details.Write || downstream.Usage.Input != upstream.Usage.Input || downstream.Usage.Output != upstream.Usage.Output || *downstream.Usage.Details.Cached != *upstream.Usage.Details.Cached || *downstream.Usage.Details.Write != *upstream.Usage.Details.Write || record.ReturnedModel != "gpt-6-astra" || result.Model != "gpt-6-astra" || upstream.Model != "gpt-6-astra" || record.SentReasoningEffort != "xhigh" || record.InputTokens <= 0 || record.TTFTMs == nil {
			t.Fatal("upstream/downstream/record usage or strongest model changed")
		}
		lock.Lock()
		count := posts
		keys := append([]string(nil), sentCacheKeys...)
		prefix := append([][]byte(nil), sentInput...)
		lock.Unlock()
		if count != index+1 {
			t.Fatal("case caused unexpected generation retry")
		}
		if index == 1 && (len(keys) != 2 || keys[0] != keys[1] || len(prefix) != 4 || !bytes.Equal(prefix[0], prefix[2]) || !bytes.Equal(prefix[1], prefix[3])) {
			t.Fatal("schema switch changed sent cache key or client prefix")
		}
		row["input_tokens"], row["output_tokens"], row["cache_read_tokens"], row["cache_write_tokens"] = record.InputTokens, record.OutputTokens, record.CachedTokens, *record.CacheWriteTokens
		row["cache_read_known"], row["cache_write_known"], row["schema_validated"], row["usage_matches_upstream"], row["original_client_items_preserved"] = true, true, true, true, true
		row["post_count"], row["ttft_ms"], row["latency_ms"], row["cache_percent"] = count, record.TTFTMs, record.LatencyMs, 100*float64(record.CachedTokens)/float64(record.InputTokens)
		if index == 1 {
			row["target_percent"] = 70
			row["target_met"] = record.CachedTokens*100 >= record.InputTokens*70
			if row["target_met"] != true {
				encoded, _ := json.Marshal(row)
				t.Log(string(encoded))
				t.Fatal("schema change did not retain at least 70 percent actual cache reads")
			}
		}
		row["passed"] = true
		encoded, _ := json.Marshal(row)
		t.Log(string(encoded))
	}
	lock.Lock()
	count := posts
	lock.Unlock()
	if count != 2 {
		t.Fatal("expected exactly two actual generations")
	}
}
