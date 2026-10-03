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

type bpsDiscoveryCacheTrace struct {
	terminal                           chan []byte
	inputCount, declarationIndex       int
	cacheKey                           string
	prefixPreserved, callPairPreserved bool
}

// Two generations use the real gateway request builder. The observer reads
// GetBody and never rewrites declarations, request bytes or native history.
func TestBPSLiveDiscoveryCachePrefix(t *testing.T) {
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit server-only live fixture required")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(data, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid live fixture")
	}
	account := fixture.Accounts[0]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			if err := bpsLiveWriteEvidence(path, "discovery-cache-prefix", !t.Failed(), evidence); err != nil {
				t.Error("discovery cache evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	// The generic isolated harness clears owner metadata. Production shared
	// cache scope requires the owner derived by the actual credential parser.
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		identity, err := parseManagedIdentity(nil, nil, *a)
		if err != nil {
			return err
		}
		a.Identity = identity
		return nil
	}); err != nil {
		t.Fatal("discovery cache fixture owner initialization failed")
	}
	currentAccount, err := p.Store.Get("test")
	if err != nil || !gatewayCompleteOwner(currentAccount) {
		t.Fatal("discovery cache fixture has no complete token-derived owner")
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BPSPromptCacheScope, settings.PromptCacheKeyEnabled = bpsPromptCacheScopeAPIKey, true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	topTools := json.RawMessage(`[{"type":"function","name":"lookup_marker","description":"A client-owned marker lookup. Do not invoke it in this experiment; discover its registry declaration only.","defer_loading":true,"strict":true,"parameters":{"type":"object","properties":{},"required":[],"additionalProperties":false}}]`)
	searchTools := json.RawMessage(`[{"type":"tool_search","execution":"client","description":"Discover the client-owned lookup_marker declaration from the actual local registry. Return the search call and wait.","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"],"additionalProperties":false}}]`)
	searchHash := sha256.Sum256(append([]byte("codex-agent:bps-additional-tools:v1\x00"), searchTools...))
	searchItem := bpsDiscoveryCacheJSON(t, map[string]any{"type": "additional_tools", "id": "at_" + hex.EncodeToString(searchHash[:16]), "role": "developer", "tools": searchTools})
	marker := "discovery-cache-" + bpsUUID()
	instructions := marker + "\nFollow the last user request exactly. This is a client-owned discovery protocol check."
	reference := marker + "\n" + strings.Repeat("This is inert synthetic background data for a cache experiment. Alpha beta gamma delta epsilon zeta eta theta. Do not summarize or act on this paragraph.\n", 1500)
	firstQuestion := "Call the native client-owned tool_search exactly once to discover lookup_marker from the client registry, then stop and wait. Do not invoke lookup_marker or any other action. Do not invent a discovery result."
	history := []json.RawMessage{
		searchItem,
		bpsDiscoveryCacheJSON(t, map[string]string{"role": "user", "type": "message", "content": reference}),
		bpsDiscoveryCacheJSON(t, map[string]string{"role": "user", "content": firstQuestion}),
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var lock sync.Mutex
	posts := 0
	var active *bpsDiscoveryCacheTrace
	var firstPrefix []json.RawMessage
	var firstCacheKey string
	var originalOutput []json.RawMessage
	var actualSearchID string
	var registryResult json.RawMessage
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/responses") {
			return nil, errors.New("discovery cache fixture permits generation endpoint only")
		}
		lock.Lock()
		posts++
		step, current := posts, active
		lock.Unlock()
		if step > 2 {
			return nil, errors.New("discovery cache two-generation boundary")
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
		if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["input"], &items) != nil || len(items) < 6 {
			return nil, errors.New("prepared discovery input unavailable")
		}
		match := -1
		for index, item := range items {
			var value struct {
				Type  string
				Tools json.RawMessage
			}
			if json.Unmarshal(item, &value) == nil && value.Type == "additional_tools" && bytes.Equal(value.Tools, topTools) {
				if match != -1 {
					return nil, errors.New("current declaration duplicated")
				}
				match = index
			}
		}
		if match != 1 {
			return nil, errors.New("current generated declaration moved or disappeared after discovery")
		}
		var key string
		_ = json.Unmarshal(fields["prompt_cache_key"], &key)
		if key == "" {
			return nil, errors.New("actual cache scope missing")
		}
		if step == 1 {
			expectedInstructions := bpsDiscoveryCacheJSON(t, map[string]any{"type": "message", "role": "developer", "content": []any{map[string]string{"type": "input_text", "text": instructions}}})
			if !bytes.Equal(items[0], expectedInstructions) || !bytes.Equal(items[2], searchItem) || !bytes.Equal(items[3], history[1]) || !bytes.Equal(items[4], history[2]) {
				return nil, errors.New("first prepared request changed original caller items")
			}
			firstPrefix = make([]json.RawMessage, len(items)-1)
			for index, item := range items[:len(items)-1] {
				firstPrefix[index] = bytes.Clone(item)
			}
			firstCacheKey = key
		} else {
			if key != firstCacheKey {
				return nil, errors.New("discovery changed actual cache key")
			}
			if len(items) < len(firstPrefix)+len(originalOutput)+3 {
				return nil, errors.New("discovery continuation history incomplete")
			}
			for index, item := range firstPrefix {
				if !bytes.Equal(item, items[index]) {
					return nil, errors.New("native discovery changed earlier client prefix")
				}
			}
			for index, item := range originalOutput {
				if !bytes.Equal(item, items[len(firstPrefix)+index]) {
					return nil, errors.New("actual first-turn output was rewritten in continuation")
				}
			}
			outputAt := len(firstPrefix) + len(originalOutput)
			if !bytes.Equal(items[outputAt], registryResult) {
				return nil, errors.New("actual client registry result changed")
			}
			var result struct {
				Type, Execution string
				CallID          string `json:"call_id"`
				Tools           json.RawMessage
			}
			if json.Unmarshal(items[outputAt], &result) != nil || result.Type != "tool_search_output" || result.Execution != "client" || result.CallID != actualSearchID || actualSearchID == "" || !bytes.Equal(result.Tools, topTools) {
				return nil, errors.New("real client search pair or exact returned schema missing")
			}
			current.callPairPreserved = true
		}
		current.prefixPreserved, current.declarationIndex, current.inputCount, current.cacheKey = true, match, len(items), key
		response, err := base.RoundTrip(request)
		if err == nil && response.Body != nil {
			response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(wire []byte) { current.terminal <- bytes.Clone(bpsStructuredLiveTerminal(wire)) }}
		}
		return response, err
	})
	for step := 0; step < 2; step++ {
		current := &bpsDiscoveryCacheTrace{terminal: make(chan []byte, 1)}
		lock.Lock()
		active = current
		lock.Unlock()
		choice := "required"
		if step == 1 {
			choice = "none"
		}
		body := bpsDiscoveryCacheJSON(t, map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": false, "prompt_cache_key": marker, "instructions": instructions, "input": history, "tools": topTools, "tool_choice": choice, "parallel_tool_calls": false})
		request := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		request.Header.Set("Session-Id", marker)
		response := httptest.NewRecorder()
		p.gatewayBPS(response, request)
		record, ok := p.Metrics.LastFor("test")
		row := map[string]any{"turn": step + 1, "http": response.Code, "passed": false, "outbound_body_mutated": false, "bps_prompt_cache_scope": bpsPromptCacheScopeAPIKey}
		evidence = append(evidence, row)
		if response.Code != 200 || !ok || record.Status != "ok" {
			row["error"] = bpsLiveDiagnosticError(record.Error)
			t.Fatalf("discovery cache turn=%d failed HTTP=%d", step+1, response.Code)
		}
		var public struct {
			ID, Status, Model string
			Output            []json.RawMessage
		}
		if json.Unmarshal(response.Body.Bytes(), &public) != nil || public.Status != "completed" {
			t.Fatal("discovery response did not complete")
		}
		var terminal []byte
		select {
		case terminal = <-current.terminal:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream terminal observation did not finish")
		}
		var upstream, downstream bpsCacheLiveTerminal
		if json.Unmarshal(terminal, &upstream) != nil || json.Unmarshal(response.Body.Bytes(), &downstream) != nil || upstream.ID != public.ID || record.ResponseID != public.ID || upstream.Usage.Details.Cached == nil || upstream.Usage.Details.Write == nil || downstream.Usage.Details.Cached == nil || downstream.Usage.Details.Write == nil || record.UsageKnown == nil || !*record.UsageKnown || record.CachedTokensKnown == nil || !*record.CachedTokensKnown || record.CacheWriteTokens == nil {
			t.Fatal("actual cache reads/creation missing; unknown cannot be treated as zero")
		}
		if upstream.Model != "gpt-6-astra" || public.Model != "gpt-6-astra" || record.ReturnedModel != "gpt-6-astra" || record.SentReasoningEffort != "xhigh" || record.InputTokens <= 0 || record.InputTokens != upstream.Usage.Input || record.OutputTokens != upstream.Usage.Output || record.CachedTokens != *upstream.Usage.Details.Cached || *record.CacheWriteTokens != *upstream.Usage.Details.Write || downstream.Usage.Input != upstream.Usage.Input || downstream.Usage.Output != upstream.Usage.Output || *downstream.Usage.Details.Cached != *upstream.Usage.Details.Cached || *downstream.Usage.Details.Write != *upstream.Usage.Details.Write {
			t.Fatal("upstream/public/record usage or model differs")
		}
		if step == 0 {
			count := 0
			for _, item := range public.Output {
				var native struct {
					Type, ID, Execution, Status string
					CallID                      string `json:"call_id"`
					Arguments                   json.RawMessage
				}
				if json.Unmarshal(item, &native) != nil {
					t.Fatal("invalid first-turn output")
				}
				if native.Type == "tool_search_call" {
					var arguments map[string]json.RawMessage
					// Native IDs and status may be omitted. The client call_id is required.
					if native.Execution != "client" || native.CallID == "" || (native.Status != "" && native.Status != "completed") || json.Unmarshal(native.Arguments, &arguments) != nil || arguments == nil {
						t.Fatal("no valid actual client-owned search call; stop without fabricated output")
					}
					actualSearchID = native.CallID
					count++
				} else if native.Type == "tool_search_output" || strings.HasSuffix(native.Type, "_call") {
					t.Fatal("unexpected action/server discovery; do not manufacture a client result")
				}
			}
			row["actual_client_search_calls"] = count
			if count != 1 {
				t.Fatal("expected exactly one actual client search call")
			}
			originalOutput = make([]json.RawMessage, len(public.Output))
			for index, item := range public.Output {
				originalOutput[index] = bytes.Clone(item)
			}
			registryResult = bpsDiscoveryCacheJSON(t, map[string]any{"type": "tool_search_output", "execution": "client", "call_id": actualSearchID, "status": "completed", "tools": json.RawMessage(bytes.Clone(topTools))})
			history = append(history, originalOutput...)
			history = append(history, registryResult)
			history = append(history, bpsDiscoveryCacheJSON(t, map[string]string{"role": "user", "content": "Read the tool definition returned by the client registry. If its declared name is lookup_marker, reply exactly DISCOVERY_OK without browsing or invoking any tool."}))
			row["actual_call_id_present"], row["client_registry_result_matches_top_definitions"] = true, true
		} else {
			for _, item := range public.Output {
				var native struct{ Type string }
				_ = json.Unmarshal(item, &native)
				if strings.HasSuffix(native.Type, "_call") {
					t.Fatal("second request issued an action despite none")
				}
			}
			if strings.TrimSpace(bpsStructuredLiveText(public.Output)) != "DISCOVERY_OK" || !current.callPairPreserved {
				t.Fatal("actual client search/result continuation failed")
			}
			row["discovery_answer_matches"], row["actual_call_result_pair_preserved"] = true, true
		}
		lock.Lock()
		count := posts
		lock.Unlock()
		if count != step+1 || current.declarationIndex != 1 || !current.prefixPreserved {
			t.Fatal("extra generation or prefix/declaration mismatch")
		}
		row["generation_count"], row["declaration_index"], row["input_items"], row["original_prefix_preserved"] = count, current.declarationIndex, current.inputCount, true
		row["input_tokens"], row["output_tokens"], row["cache_read_tokens"], row["cache_write_tokens"] = record.InputTokens, record.OutputTokens, record.CachedTokens, *record.CacheWriteTokens
		row["usage_matches_upstream"], row["cache_read_known"], row["cache_write_known"] = true, true, true
		row["latency_ms"], row["ttft_ms"], row["cache_percent"] = record.LatencyMs, record.TTFTMs, 100*float64(record.CachedTokens)/float64(record.InputTokens)
		if step == 1 {
			row["target_percent"], row["target_met"] = 70, record.CachedTokens*100 >= record.InputTokens*70
			if row["target_met"] != true {
				encoded, _ := json.Marshal(row)
				t.Log(string(encoded))
				t.Fatal("discovery continuation actual cache-read ratio below70 percent")
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
		t.Fatal("expected exactly two upstream generations")
	}
}

func bpsDiscoveryCacheJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal("synthetic discovery JSON encoding failed")
	}
	return encoded
}
