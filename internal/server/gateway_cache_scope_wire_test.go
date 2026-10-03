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

func TestGatewaySub2CacheHTTPWireHeadersAndCompact(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%t", compact), func(t *testing.T) {
			var captured []byte
			var headers http.Header
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				headers = r.Header.Clone()
				if r.URL.Path != "/backend-api/codex/responses" {
					t.Errorf("wrong backend path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				output := `[]`
				if compact {
					output = `[{"type":"compaction","encrypted_content":"opaque-compacted-history"}]`
				}
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"scope-response\",\"status\":\"completed\",\"output\":%s}}\n\n", output)
			})
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			route := "/responses"
			if compact {
				route += "/compact"
			}
			r := proxyRequest(http.MethodPost, route, `{"model":"test","instructions":"stable","prompt_cache_key":"root","client_metadata":{"session_id":"root","thread_id":"child"},"input":[],"stream":false}`)
			r.Header.Set("Session-Id", "root")
			r.Header.Set("Thread-Id", "child")
			r.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","turn_id":"turn","n":9007199254740993}`)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
			}
			session := gatewayScopeCacheIdentityValue(account, "admin", "session", "root")
			thread := gatewayScopeCacheIdentityValue(account, "admin", "thread", "child")
			if gatewayPromptCacheKeyValue(captured) != session || headers.Get("Session-Id") != session || headers.Get("Thread-Id") != thread {
				t.Fatal("header/body namespace differs on actual HTTP wire")
			}
			if headers.Get("session_id") != gatewayUpstreamCacheSession("admin", account, "root") || headers.Get("conversation_id") != headers.Get("session_id") {
				t.Fatal("actual HTTP wire derived underscore headers from already-scoped body")
			}
			if headers.Get("X-Codex-Turn-Metadata") != fmt.Sprintf(`{"session_id":%q,"thread_id":%q,"turn_id":"turn","n":9007199254740993}`, session, thread) {
				t.Fatal("embedded header lost scope or opaque fields")
			}
			if compact && !strings.Contains(string(captured), `"type":"compaction_trigger"`) {
				t.Fatal("compact fixture did not exercise streaming Responses adapter")
			}
		})
	}
}

func TestGatewaySub2CacheWSWireHeadersLazyAndPreconnect(t *testing.T) {
	for _, earlySession := range []bool{false, true} {
		t.Run(fmt.Sprintf("preconnect=%t", earlySession), func(t *testing.T) {
			type request struct {
				header http.Header
				body   []byte
			}
			captured := make(chan request, 1)
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				captured <- request{r.Header.Clone(), raw}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"scope-ws","status":"completed","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer "+p.AdminKey)
			// No routing alias for the lazy case: its first frame must supply
			// the original cache key before the upstream upgrade is made.
			if earlySession {
				headers.Set("Session-Id", "root")
				headers.Set("Thread-Id", "child")
			}
			headers.Set("X-Codex-Turn-Metadata", `{"session_id":"root","thread_id":"child","turn_id":"turn"}`)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","prompt_cache_key":"root","client_metadata":{"session_id":"root","thread_id":"child"},"input":[]}`)); err != nil {
				t.Fatal(err)
			}
			if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), "response.completed") {
				t.Fatalf("WS rejected %s", reply)
			}
			got := <-captured
			session := gatewayScopeCacheIdentityValue(account, "admin", "session", "root")
			thread := gatewayScopeCacheIdentityValue(account, "admin", "thread", "child")
			if gatewayPromptCacheKeyValue(got.body) != session || got.header.Get("session_id") != gatewayUpstreamCacheSession("admin", account, "root") {
				t.Fatal("WS header/body did not scope original cache identity once")
			}
			if earlySession && (got.header.Get("Session-Id") != session || got.header.Get("Thread-Id") != thread) {
				t.Fatal("WS preconnect identity headers did not receive body-consistent scope")
			}
			if !earlySession && (got.header.Get("Session-Id") != "" || got.header.Get("Thread-Id") != "") {
				t.Fatal("lazy handshake invented absent hyphen identity")
			}
			var metadata map[string]string
			if json.Unmarshal([]byte(got.header.Get("X-Codex-Turn-Metadata")), &metadata) != nil || metadata["session_id"] != session || metadata["thread_id"] != thread || metadata["turn_id"] != "turn" {
				t.Fatal("WS embedded header changed or failed to scope")
			}
		})
	}
}
