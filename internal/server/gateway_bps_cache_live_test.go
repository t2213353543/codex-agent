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
	"sync"
	"testing"
	"time"
)

type bpsCacheLiveTransport func(*http.Request) (*http.Response, error)

func (transport bpsCacheLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

// Five synthetic same-prefix requests through the real BPS gateway. Real
// credentials are explicit, server-only test fixtures; statistics belong to
// newTestProxy's private store, never production usage. No tools are executed.
func TestBPSLiveStablePrefixCache(t *testing.T) {
	runBPSLiveStablePrefixCache(t, false)
}

func TestBPSLiveGeneratedDefaultCache(t *testing.T) {
	runBPSLiveStablePrefixCache(t, true)
}

func TestBPSLiveSharedAPIKeyCache(t *testing.T) {
	runBPSLiveStablePrefixCache(t, false, bpsPromptCacheScopeAPIKey)
}

type bpsCacheLiveTerminal struct {
	ID, Model string
	Usage     struct {
		Input   int64 `json:"input_tokens"`
		Output  int64 `json:"output_tokens"`
		Details struct {
			Cached *int64 `json:"cached_tokens"`
			Write  *int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type bpsCacheLiveResponseBody struct {
	io.ReadCloser
	wire     bytes.Buffer
	once     sync.Once
	complete func([]byte)
}

func (b *bpsCacheLiveResponseBody) Read(raw []byte) (int, error) {
	n, err := b.ReadCloser.Read(raw)
	_, _ = b.wire.Write(raw[:n])
	return n, err
}

func (b *bpsCacheLiveResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.complete(b.wire.Bytes()) })
	return err
}

func runBPSLiveStablePrefixCache(t *testing.T, defaultKey bool, scope ...string) {
	sharedAPIKey := len(scope) > 0 && scope[0] == bpsPromptCacheScopeAPIKey
	file, binary, work := os.Getenv("BPS_LIVE_ACCOUNTS_FILE"), os.Getenv("BPS_LIVE_CODEX_BINARY"), os.Getenv("BPS_LIVE_WORK_ROOT")
	if file == "" || binary == "" || work == "" {
		t.Skip("explicit live credential fixture and isolated work root required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("live credential fixture unavailable")
	}
	var fixture struct {
		Accounts []bpsLiveAccount `json:"accounts"`
	}
	if json.Unmarshal(raw, &fixture) != nil || len(fixture.Accounts) == 0 {
		t.Fatal("invalid live credential fixture")
	}
	account := fixture.Accounts[0]
	var evidence []map[string]any
	defer func() {
		if path := os.Getenv("BPS_LIVE_EVIDENCE_FILE"); path != "" {
			name := "bps-stable-prefix-cache"
			if sharedAPIKey {
				name = "bps-shared-api-key-cache"
			}
			if err := bpsLiveWriteEvidence(path, name, !t.Failed(), evidence); err != nil {
				t.Error("cache evidence write failed")
			}
		}
	}()
	h := newBPSLiveHarness(t, binary, work, account, []string{account.AccessToken, account.AccountID}, &evidence)
	p := h.recorder.p
	p.BPS.SearchDisabled = true
	if sharedAPIKey {
		// The generic live harness deliberately clears owner metadata. Shared
		// caching requires the same real owner identity as production imports;
		// initialize it from this fixture's token using the production parser.
		if err := p.Store.UpdateAccount("test", func(a *Account) error {
			identity, err := parseManagedIdentity(nil, nil, *a)
			if err != nil {
				return err
			}
			a.Identity = identity
			return nil
		}); err != nil {
			t.Fatal("shared cache fixture owner initialization failed")
		}
		current, err := p.Store.Get("test")
		if err != nil || !gatewayCompleteOwner(current) {
			t.Fatal("shared cache fixture has no complete token-derived owner")
		}
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.PromptCacheKeyEnabled = true
	settings.StickySessionsEnabled = true
	if sharedAPIKey {
		settings.BPSPromptCacheScope = bpsPromptCacheScopeAPIKey
	}
	if err = p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	base := p.Client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var lock sync.Mutex
	var prefixHashes []string
	var cacheKeys []string
	upstreamUsage := make(map[string]bpsCacheLiveTerminal)
	p.Client.Transport = bpsCacheLiveTransport(func(request *http.Request) (*http.Response, error) {
		inference := request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/responses")
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/responses") && request.GetBody != nil {
			body, e := request.GetBody()
			if e != nil {
				return nil, e
			}
			data, e := io.ReadAll(body)
			_ = body.Close()
			if e != nil {
				return nil, e
			}
			var content struct {
				Input    []json.RawMessage `json:"input"`
				CacheKey string            `json:"prompt_cache_key"`
			}
			if json.Unmarshal(data, &content) == nil && len(content.Input) >= 2 {
				// Instructions, converted tools and the unchanged long context
				// precede the final per-turn question and policy suffix.
				prefix := content.Input[:len(content.Input)-2]
				encoded, _ := json.Marshal(prefix)
				hash := sha256.Sum256(encoded)
				lock.Lock()
				prefixHashes = append(prefixHashes, hex.EncodeToString(hash[:]))
				cacheKeys = append(cacheKeys, content.CacheKey)
				lock.Unlock()
			}
		}
		response, err := base.RoundTrip(request)
		if sharedAPIKey && err == nil && inference && response.Body != nil {
			response.Body = &bpsCacheLiveResponseBody{ReadCloser: response.Body, complete: func(raw []byte) {
				for _, line := range bytes.Split(raw, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data:")) {
						continue
					}
					var event struct {
						Type     string          `json:"type"`
						Response json.RawMessage `json:"response"`
					}
					if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil || event.Type != "response.completed" {
						continue
					}
					var terminal bpsCacheLiveTerminal
					if json.Unmarshal(event.Response, &terminal) == nil && terminal.ID != "" {
						lock.Lock()
						upstreamUsage[terminal.ID] = terminal
						lock.Unlock()
					}
				}
			}}
		}
		return response, err
	})
	server := httptest.NewServer(p)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Minute}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models?client_version=0.155.0", nil)
	request.Header.Set("Authorization", "Bearer test-client-key")
	request.Header.Set("User-Agent", "codex_cli_rs/0.155.0")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("BPS models lookup transport failed")
	}
	catalog, e := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if e != nil || response.StatusCode != 200 {
		t.Fatalf("BPS models lookup status=%d", response.StatusCode)
	}
	var models struct {
		Models []struct {
			Slug    string `json:"slug"`
			Efforts []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if json.Unmarshal(catalog, &models) != nil {
		t.Fatal("BPS model catalog invalid")
	}
	available := false
	for _, model := range models.Models {
		if model.Slug == "gpt-6-astra" {
			for _, effort := range model.Efforts {
				available = available || effort.Effort == "xhigh"
			}
		}
	}
	if !available {
		t.Fatal("BPS catalog does not advertise gpt-6-astra/xhigh")
	}
	evidence = append(evidence, map[string]any{"kind": "catalog", "model": "gpt-6-astra", "effort": "xhigh", "advertised": true})
	long := strings.Repeat("Stable synthetic background reference: alpha beta gamma delta. These records are inert cache test data and require no action, interpretation, or tool calls.\n", 400)
	session := "bps-cache-" + bpsUUID()
	sharedMarker := "shared-api-key-run-" + bpsUUID()
	clientKeys := make(map[string]bool)
	clientSessions := make(map[string]bool)
	tools := []any{map[string]any{"type": "function", "name": "cache_fixture_unused", "description": "Declared only to verify stable tool-prefix serialization; never invoke.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
	var totalInput, totalCached, totalWrite, warmInput, warmCached int64
	allWritesKnown := true
	for turn := 0; turn < 5; turn++ {
		fields := map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": true, "tools": tools, "tool_choice": "none",
			"instructions": "Follow the final user instruction. Do not summarize the inert reference. Never use tools. Reply exactly CACHE_OK.",
			"input":        []any{map[string]string{"role": "user", "content": long}, map[string]string{"role": "user", "content": "Reply exactly CACHE_OK."}}}
		if sharedAPIKey {
			session = "shared-session-" + bpsUUID()
			clientKey := "shared-client-key-" + bpsUUID()
			fields["prompt_cache_key"] = clientKey
			fields["instructions"] = sharedMarker + ". " + fields["instructions"].(string)
			clientKeys[clientKey] = true
		} else if defaultKey {
			session = "generated-cache-" + bpsUUID()
		} else {
			fields["prompt_cache_key"] = session
		}
		clientSessions[session] = true
		payload, _ := json.Marshal(fields)
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", session)
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("BPS cache test transport failed")
		}
		data, e := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if e != nil || res.StatusCode != 200 {
			t.Fatalf("BPS cache turn=%d status=%d bytes=%d", turn, res.StatusCode, len(data))
		}
		var done bpsCacheLiveTerminal
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data: ")) {
				var event struct {
					Type     string          `json:"type"`
					Response json.RawMessage `json:"response"`
				}
				if json.Unmarshal(line[6:], &event) == nil && event.Type == "response.completed" {
					_ = json.Unmarshal(event.Response, &done)
					var envelope map[string]json.RawMessage
					_ = json.Unmarshal(event.Response, &envelope)
					var fields []string
					for field := range envelope {
						fields = append(fields, field)
					}
					t.Logf("turn=%d terminal_fields=%v usage_schema=%s usage_metadata_schema=%s", turn, fields, bpsCacheUsageShape(envelope["usage"]), bpsCacheUsageShape(envelope["usage_metadata"]))
				}
			}
		}
		if done.ID == "" || done.Usage.Input < 4000 || done.Usage.Output <= 0 || done.Usage.Details.Cached == nil {
			t.Fatalf("BPS cache turn=%d missing terminal or actual usage/cache counters", turn)
		}
		var record RequestRecord
		var originalUsage bpsCacheLiveTerminal
		deadline := time.Now().Add(time.Second)
		for {
			p.Metrics.mu.Lock()
			for _, r := range p.Metrics.recent[:p.Metrics.recentAt] {
				if r.ResponseID == done.ID {
					record = r
				}
			}
			p.Metrics.mu.Unlock()
			lock.Lock()
			originalUsage = upstreamUsage[done.ID]
			lock.Unlock()
			if (record.ResponseID != "" && (!sharedAPIKey || originalUsage.ID != "")) || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != done.Usage.Input || record.OutputTokens != done.Usage.Output || record.CachedTokens != *done.Usage.Details.Cached {
			t.Fatal("BPS streamed usage differs from isolated persisted record")
		}
		if sharedAPIKey {
			if originalUsage.ID == "" || originalUsage.Usage.Details.Cached == nil || originalUsage.Usage.Details.Write == nil || done.Usage.Details.Write == nil || record.CachedTokensKnown == nil || !*record.CachedTokensKnown || record.CacheWriteTokens == nil {
				t.Fatal("API-key cache scope requires actual upstream read/write counters including explicit zero")
			}
			if originalUsage.Usage.Input != record.InputTokens || originalUsage.Usage.Output != record.OutputTokens || *originalUsage.Usage.Details.Cached != record.CachedTokens || *originalUsage.Usage.Details.Write != *record.CacheWriteTokens || *done.Usage.Details.Write != *record.CacheWriteTokens {
				t.Fatal("upstream, downstream and recorded cache read/write counters differ")
			}
		}
		if record.ReturnedModel != done.Model || done.Model == "" || record.TTFTMs == nil {
			t.Fatal("BPS returned model or measured TTFT missing")
		}
		totalInput += record.InputTokens
		totalCached += record.CachedTokens
		if record.CacheWriteTokens != nil {
			totalWrite += *record.CacheWriteTokens
		} else {
			allWritesKnown = false
		}
		if turn > 0 {
			warmInput += record.InputTokens
			warmCached += record.CachedTokens
		}
		row := map[string]any{"kind": "turn", "turn": turn, "input_tokens": record.InputTokens, "output_tokens": record.OutputTokens, "cached_tokens": record.CachedTokens, "returned_model": record.ReturnedModel, "ttft_ms": *record.TTFTMs, "latency_ms": record.LatencyMs, "cache_percent": 100 * float64(record.CachedTokens) / float64(record.InputTokens)}
		row["cache_read_tokens"], row["cache_write_tokens"] = record.CachedTokens, record.CacheWriteTokens
		if sharedAPIKey {
			row["upstream_cache_read_tokens"], row["upstream_cache_write_tokens"] = originalUsage.Usage.Details.Cached, originalUsage.Usage.Details.Write
			row["cache_read_write_matches_upstream"], row["bps_prompt_cache_scope"] = true, bpsPromptCacheScopeAPIKey
			clientHash := sha256.Sum256([]byte(fields["prompt_cache_key"].(string)))
			sessionHash := sha256.Sum256([]byte(session))
			row["client_cache_key_sha256"], row["client_session_sha256"] = hex.EncodeToString(clientHash[:]), hex.EncodeToString(sessionHash[:])
		}
		evidence = append(evidence, row)
		encoded, _ := json.Marshal(row)
		t.Log(string(encoded))
	}
	lock.Lock()
	hashes := append([]string(nil), prefixHashes...)
	keys := append([]string(nil), cacheKeys...)
	lock.Unlock()
	stable := len(hashes) == 5
	for _, hash := range hashes {
		stable = stable && hash == hashes[0]
	}
	stableKeys := len(keys) == 5
	for _, key := range keys {
		stableKeys = stableKeys && key != "" && key == keys[0]
	}
	row := map[string]any{"kind": "summary", "turns": 5, "prefix_hashes": hashes, "model_prefix_stable": stable, "input_tokens": totalInput, "cached_tokens": totalCached, "cache_percent": 100 * float64(totalCached) / float64(totalInput), "warm_input_tokens": warmInput, "warm_cached_tokens": warmCached, "warm_cache_percent": 100 * float64(warmCached) / float64(warmInput), "target_percent": 70, "target_met": totalCached*100 >= totalInput*70}
	row["cache_read_tokens"], row["cache_write_tokens"] = totalCached, nil
	row["cache_write_tokens_known"] = allWritesKnown
	if allWritesKnown {
		row["cache_write_tokens"] = totalWrite
	}
	evidence = append(evidence, row)
	row["client_omitted_cache_key"], row["distinct_client_sessions"], row["upstream_cache_key_stable"] = defaultKey, len(clientSessions) == 5, stableKeys
	if sharedAPIKey {
		row["client_explicit_random_cache_keys"], row["bps_prompt_cache_scope"] = len(clientKeys) == 5, bpsPromptCacheScopeAPIKey
		keyHashes := make([]string, 0, len(keys))
		for _, key := range keys {
			sum := sha256.Sum256([]byte(key))
			keyHashes = append(keyHashes, hex.EncodeToString(sum[:]))
		}
		row["upstream_cache_key_sha256s"] = keyHashes
		if len(clientKeys) != 5 || len(clientSessions) != 5 {
			t.Error("shared API-key test did not vary every client key and session")
		}
	}
	encoded, _ := json.Marshal(row)
	t.Log(string(encoded))
	if !stable {
		t.Error("BPS model input prefix changed during identical requests")
	}
	if !stableKeys {
		t.Error("upstream cache key missing or changed")
	}
	if totalCached*100 < totalInput*70 {
		t.Error("actual BPS weighted cache ratio is below the 70 percent target")
	}
}

// Only types/field names and measured nonnegative numbers are emitted. Never
// log prompt/output, arbitrary usage strings, tokens or credential metadata.
func bpsCacheUsageShape(raw json.RawMessage) string {
	var object map[string]json.RawMessage
	if len(raw) == 0 {
		return "absent"
	}
	if json.Unmarshal(raw, &object) == nil && object != nil {
		out := map[string]any{}
		for key, value := range object {
			if len(key) > 100 {
				continue
			}
			if number := observedGatewayCount(value); number != nil {
				out[key] = *number
			} else {
				var nested map[string]json.RawMessage
				if json.Unmarshal(value, &nested) == nil && nested != nil {
					out[key] = json.RawMessage(bpsCacheUsageShape(value))
				} else {
					out[key] = "unreported_or_non_numeric"
				}
			}
		}
		encoded, _ := json.Marshal(out)
		return string(encoded)
	}
	return "null_or_non_object"
}
