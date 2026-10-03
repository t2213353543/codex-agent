package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

func bpsGatewayFixture(t *testing.T, handler http.HandlerFunc) *Proxy {
	t.Helper()
	p := proxyFixture(t, handler)
	p.BPS = &bpsRuntime{ToolsVersion: bps.DefaultToolsVersion}
	enableGatewayWS(t, p, "")
	// Protocol/accounting fixtures compare original upstream usage. Billing
	// projection is exercised independently with the setting explicitly on.
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.CacheCreationAsReadEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	return p
}

func bpsGatewayFrame(kind, id, output string) string {
	return `data: {"type":"` + kind + `","response":{"id":"` + id + `","status":"completed","output":` + output + `,"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}}` + "\n\n"
}

func bpsGatewayEvents(t *testing.T, wire string) []map[string]json.RawMessage {
	t.Helper()
	var events []map[string]json.RawMessage
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func bpsGatewayString(fields map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(fields[name], &value)
	return value
}

func TestBPSGatewayHTTPProtocolAndNamespacePreserveRawNumbers(t *testing.T) {
	const tool = `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","arguments":"{\"integer\":9007199254740993}"}`
	captured := make(chan []byte, 1)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		if r.URL.Path != "/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL.Path)
		}
		for name, want := range map[string]string{
			"Authorization": "Bearer test-upstream-token", "X-Basispoints-Auth-Mode": "chatgpt",
			"X-Openai-Internal-Basispoints-Client-Agent-Profile": "powerpoint",
			"X-Openai-Internal-Basispoints-Client-Product":       "basispoints-powerpoint-plugin",
			"ChatGPT-Account-ID":                                 "test-account",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("upstream %s = %q; want %q", name, got, want)
			}
		}
		for _, name := range []string{"Cookie", "Proxy-Authorization", "X-Client-Private"} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("client-only header leaked: %s", name)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"inspect","status":"in_progress","arguments":""}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"integer\":9007199254740993}"}`+"\n\n")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_wire", "["+tool+"]"))
	})
	body := `{"model":"gpt-6-astra","stream":true,"store":false,"reasoning":{"effort":"xhigh","summary":"auto"},"instructions":"preserve this instruction","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"integer":{"const":9007199254740993},"decimal":{"const":1.2500e+02}}}}]}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]}],"parallel_tool_calls":false,"include":["reasoning.encrypted_content"]}`
	req := proxyRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Cookie", "client-secret=1")
	req.Header.Set("Proxy-Authorization", "private-proxy")
	req.Header.Set("X-Client-Private", "private")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	upstream := <-captured
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"model": true, "input": true, "stream": true, "store": true, "metadata": true, "reasoning_effort": true, "model_selection": true, "context_management": true, "prompt_cache_key": true}
	for name := range fields {
		if !allowed[name] {
			t.Errorf("unsupported Codex field forwarded to BPS: %s", name)
		}
	}
	if cache := bpsGatewayString(fields, "prompt_cache_key"); cache == "" || strings.Contains(cache, "test-client-key") {
		t.Fatal("default BPS cache identity missing or exposes the client key")
	}
	if bpsGatewayString(fields, "reasoning_effort") != "xhigh" || !bytes.Contains(upstream, []byte("9007199254740993")) || !bytes.Contains(upstream, []byte("1.2500e+02")) || !bytes.Contains(upstream, []byte(`"type":"additional_tools"`)) {
		t.Fatalf("request semantics lost: %s", upstream)
	}
	events := bpsGatewayEvents(t, w.Body.String())
	if len(events) != 3 || bpsGatewayString(events[1], "namespace") != "functions" || bpsGatewayString(events[1], "call_id") != "call_1" {
		t.Fatalf("stream tool identity lost: %s", w.Body.String())
	}
	var response struct{ Output []map[string]json.RawMessage }
	if json.Unmarshal(events[2]["response"], &response) != nil || len(response.Output) != 1 || bpsGatewayString(response.Output[0], "namespace") != "functions" || bpsGatewayString(response.Output[0], "arguments") != `{"integer":9007199254740993}` {
		t.Fatalf("terminal tool identity/number lost: %s", w.Body.String())
	}
}

func TestBPSGatewayHTTPFirstDeltaArrivesBeforeUpstreamTerminal(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","item_id":"msg_first","delta":"FIRST_DELTA"}`+"\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_first", "[]"))
		case <-r.Context().Done():
		}
	})
	server := httptest.NewServer(p)
	defer server.Close()
	client := &http.Client{Timeout: 4 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "FIRST_DELTA") {
			break
		}
	}
}

func TestBPSGatewayCompactionTriggerAndLegacyJSON(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			const compact = `{"id":"comp_test","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-state"}],"usage":{"input_tokens":9007199254740993}}`
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/responses/compact" || bytes.Contains(raw, []byte(`"compaction_trigger"`)) {
					t.Errorf("compact route/control translation failed: %s %s", r.URL.Path, raw)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, compact)
			})
			path, body := "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[{"role":"user","content":"remember"},{"type":"compaction_trigger"}]}`
			if legacy {
				path, body = "/v1/responses/compact", `{"model":"gpt-6-astra","input":[{"role":"user","content":"remember"}]}`
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, path, body))
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			if legacy {
				if w.Body.String() != compact || w.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("legacy compact JSON changed: %s", w.Body.String())
				}
			} else {
				events := bpsGatewayEvents(t, w.Body.String())
				if len(events) < 2 || bpsGatewayString(events[len(events)-1], "type") != "response.completed" || !strings.Contains(w.Body.String(), "opaque-state") {
					t.Fatalf("compact SSE incomplete: %s", w.Body.String())
				}
			}
		})
	}
}

func TestBPSGatewayJSONHistoryIsKeyAndOwnerBound(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_owner", `[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"private-answer"}]}]`))
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Groups = []string{"bps-test"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := p.Keys.Create("first", account.Groups)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := p.Keys.Create("second", account.Groups)
	if err != nil {
		t.Fatal(err)
	}
	send := func(key, body string) *httptest.ResponseRecorder {
		request := proxyRequest(http.MethodPost, "/v1/responses", body)
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Session-Id", "owner-bound")
		writer := httptest.NewRecorder()
		p.ServeHTTP(writer, request)
		return writer
	}
	first := send(firstKey.Key, `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"private-question"}]}`)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "private-answer") || calls.Load() != 1 {
		t.Fatalf("initial JSON request failed: %d %s", first.Code, first.Body.String())
	}
	const next = `{"model":"gpt-6-astra","stream":false,"previous_response_id":"resp_owner","input":[{"role":"user","content":"next"}]}`
	otherKey := send(secondKey.Key, next)
	if otherKey.Code != http.StatusBadRequest || !strings.Contains(otherKey.Body.String(), "previous_response_not_found") || calls.Load() != 1 || strings.Contains(otherKey.Body.String(), "private-answer") {
		t.Fatalf("cross-key history leaked: %d %s", otherKey.Code, otherKey.Body.String())
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "replacement-user", WorkspaceID: a.AccountID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	otherOwner := send(firstKey.Key, next)
	if otherOwner.Code < 400 || calls.Load() != 1 {
		t.Fatalf("cross-owner history reached inference: %d %s", otherOwner.Code, otherOwner.Body.String())
	}
}

func TestBPSGatewayRejectsInvalidKeyBeforeInference(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":[]}`)
	req.Header.Set("Authorization", "Bearer invalid")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("unauthorized inference: HTTP %d, calls %d", w.Code, calls.Load())
	}
}

func TestBPSGatewayLargeJSONResponseRetainsExactContent(t *testing.T) {
	large := strings.Repeat("糖果🍬", 20000)
	encoded, _ := json.Marshal(large)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, encoded) {
			t.Error("large input changed before reaching BPS")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		output := `[{"id":"msg_large","type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]`
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_large", output))
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":`+string(encoded)+`}`))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), encoded) || w.Body.Len() < 64<<10 {
		t.Fatalf("large response truncated: HTTP %d, bytes=%d", w.Code, w.Body.Len())
	}
}

func TestBPSGatewayInterruptedStreamNeverCompletesOrRetries(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"PARTIAL_ONLY"}`+"\n\n")
		_ = http.NewResponseController(w).Flush()
		// Return an ordinary HTTP EOF without a Responses terminal.
	})
	server := httptest.NewServer(p)
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":[]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	client := &http.Client{Timeout: 4 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(res.Body)
	if readErr == nil || !bytes.Contains(body, []byte("PARTIAL_ONLY")) || bytes.Contains(body, []byte("response.completed")) || calls.Load() != 1 {
		t.Fatalf("truncation was accepted/replayed: read=%v, calls=%d, body=%s", readErr, calls.Load(), body)
	}
}
