package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayPromptCacheObservationHTTPWire(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/stream=%t", native, stream), func(t *testing.T) {
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					if got := policyString(t, raw, "prompt_cache_key"); len(got) != 36 {
						t.Errorf("default cache key was not prepared: %q", got)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1000,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":256}}}}\n\n")
				})
				if native {
					realGatewayNativePool(t, p)
				}
				var err error
				p.PromptCacheObserver, err = newGatewayPromptCacheObserver()
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					raw := fmt.Sprintf(`{"model":"test","stream":%t,"instructions":"private instructions","prompt_cache_key":"conversation","client_metadata":{"session_id":"conversation","thread_id":"thread"},"input":[{"role":"user","content":"private first question"}%s]}`, stream, []string{"", `,{"role":"user","content":"followup"}`}[i])
					r := proxyRequest(http.MethodPost, "/v1/responses", raw)
					r.Header.Set("Session-Id", "conversation")
					r.Header.Set("Thread-Id", "thread")
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != 200 {
						t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
					}
					rec, ok := p.Metrics.LastFor("test")
					if !ok || rec.PromptCache == nil {
						t.Fatal("prepared request observation was not recorded")
					}
					if rec.InputTokens != 1000 || rec.CachedTokens != 256 || rec.OutputTokens != 5 {
						t.Fatal("diagnostics changed upstream accounting")
					}
					if rec.PromptCache.KeyMode != "codex_default" || !rec.PromptCache.CacheKeyPresent {
						t.Fatalf("wrong inbound key mode: %+v", rec.PromptCache)
					}
					if i == 1 && (rec.PromptCache.InputHistory != "extended" || rec.PromptCache.AccountChanged || rec.PromptCache.CacheKeyChanged) {
						t.Fatalf("wrong prepared comparison: %+v", rec.PromptCache)
					}
					encoded, _ := json.Marshal(rec.PromptCache)
					for _, secret := range []string{"private first question", "private instructions", `"conversation"`, `"thread"`, "test-upstream-token"} {
						if strings.Contains(string(encoded), secret) {
							t.Fatal("raw request leaked into diagnostics")
						}
					}
				}
			})
		}
	}
}

func TestGatewayPromptCacheObservationWebSocketWire(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"cache-wire-%d","status":"completed","output":[],"usage":{"input_tokens":1000,"output_tokens":1,"input_tokens_details":{"cached_tokens":128}}}}`, i)))
		}
		_, _, _ = conn.Read(r.Context())
	})
	var err error
	p.PromptCacheObserver, err = newGatewayPromptCacheObserver()
	if err != nil {
		t.Fatal(err)
	}
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "ws-observed")
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		raw := fmt.Sprintf(`{"type":"response.create","model":"test","prompt_cache_key":"ws-observed","client_metadata":{"session_id":"ws-observed","thread_id":"t"},"input":[{"role":"user","content":"stable"}%s]}`, []string{"", `,{"role":"user","content":"next"}`}[i])
		if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gatewayWSRead(t, conn)), "response.completed") {
			t.Fatal("missing completion")
		}
		// Receiving a frame can race the server Write return. Its request record
		// is finalized only after that return determines successful delivery.
		// Wait for this exact response, not merely an older record or free slot.
		var rec RequestRecord
		var ok bool
		for {
			rec, ok = p.Metrics.LastFor("test")
			if ok && rec.ResponseID == fmt.Sprintf("cache-wire-%d", i) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("WS response record was not finalized")
			case <-time.After(time.Millisecond):
			}
		}
		if !ok || rec.PromptCache == nil || rec.CachedTokens != 128 || rec.InputTokens != 1000 {
			t.Fatalf("missing WS diagnostics or changed usage: %+v", rec)
		}
		if i == 1 && rec.PromptCache.InputHistory != "extended" {
			t.Fatalf("WS frame not correlated: %+v", rec.PromptCache)
		}
	}
}

func TestPGGatewayPromptCacheObservationRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	observer, err := newGatewayPromptCacheObserver()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	request.Header.Set("Session-Id", "private-session")
	observation := observer.Observe(request, []byte(`{"model":"test","prompt_cache_key":"private-key","input":[{"role":"user","content":"private-content"}]}`), "private-tenant", Account{ID: "private-account"}, "explicit_override")
	record := RequestRecord{Time: time.Now().Unix(), Status: "ok", Model: "test", InputTokens: 1000, CachedTokens: 256, PromptCache: observation}
	m.Record(record)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.Pool.QueryRow(context.Background(), "SELECT record FROM request_logs ORDER BY id DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got RequestRecord
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptCache == nil || got.PromptCache.ScopeHash != observation.ScopeHash || got.CachedTokens != 256 || got.InputTokens != 1000 {
		t.Fatalf("record or accounting changed: %+v", got)
	}
	for _, secret := range []string{"private-session", "private-key", "private-tenant", "private-account", "private-content"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("raw identifier or prompt entered persistent observation")
		}
	}
}
