package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// This synthetic sequence deliberately repeats an old terminal before the
// next response has an ID. It does not imply that normal CLI traffic does so.
func TestGatewayNativeActualStaleResponseIdentity(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "" || runtime.GOOS != "linux" {
		t.Skip("set CODEX_NATIVE_HTTP_TEST_BIN on Linux for actual stale-response identity integration")
	}
	const first = `{"type":"response.completed","stream_id":"stale-fixture-stream","response":{"id":"stale-fixture-r1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1}}}`
	const second = `{"type":"response.completed","stream_id":"stale-fixture-stream","response":{"id":"stale-fixture-r2","status":"completed","output":[],"usage":{"input_tokens":20,"output_tokens":2}}}`
	const failed = `{"type":"response.failed","stream_id":"stale-fixture-stream","headers":{"Retry-After":"30"},"response":{"id":"stale-fixture-r1","status":"failed","error":{"code":"rate_limit_exceeded","message":"synthetic stale failure"},"usage":{"input_tokens":999,"output_tokens":999}}}`
	for _, scenario := range []struct{ name, stale string }{{"duplicate_completed", first}, {"stale_failed_429", failed}} {
		t.Run(scenario.name, func(t *testing.T) {
			var handshakes, creates atomic.Int32
			release := make(chan struct{})
			var releaseOnce sync.Once
			allowCompletion := func() { releaseOnce.Do(func() { close(release) }) }
			p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.ProtoMajor != 1 {
					t.Error("native stale-event fixture did not use TLS WebSocket")
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for turn := range 2 {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					creates.Add(1)
					var create struct {
						Type     string `json:"type"`
						Model    string `json:"model"`
						StreamID string `json:"stream_id"`
					}
					wantModel := "stale-model-a"
					if turn == 1 {
						wantModel = "stale-model-b"
					}
					if json.Unmarshal(raw, &create) != nil || create.Type != "response.create" || create.Model != wantModel || create.StreamID != "stale-fixture-stream" {
						t.Error("native stale-event fixture changed request identity or order")
						return
					}
					if turn == 0 {
						if conn.Write(r.Context(), websocket.MessageText, []byte(first)) != nil {
							return
						}
						continue
					}
					if conn.Write(r.Context(), websocket.MessageText, []byte(scenario.stale)) != nil {
						return
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if conn.Write(r.Context(), websocket.MessageText, []byte(second)) != nil {
						return
					}
				}
				_, _, _ = conn.Read(r.Context())
			})
			guard := &nativeCLIFallbackGuard{}
			p.Client.Transport = guard
			gateway := httptest.NewServer(p)
			t.Cleanup(gateway.Close)
			t.Cleanup(allowCompletion)
			conn, _ := gatewayWSDial(t, gateway.URL, "native-stale-response-session")
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			write := func(model string) {
				t.Helper()
				raw, _ := json.Marshal(map[string]any{"type": "response.create", "stream_id": "stale-fixture-stream", "model": model, "input": []any{}})
				if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
					t.Fatal(err)
				}
			}
			readExact := func(expected string) {
				t.Helper()
				typ, raw, err := conn.Read(ctx)
				if err != nil || typ != websocket.MessageText || !bytes.Equal(raw, []byte(expected)) {
					t.Fatalf("native bookkeeping altered or dropped upstream frame: type=%d bytes=%d err=%v", typ, len(raw), err)
				}
			}
			write("stale-model-a")
			readExact(first)
			waitGatewayWSReleased(t, p)
			if records := p.Metrics.RecentForAccount("test", 10); len(records) != 1 || records[0].ResponseID != "stale-fixture-r1" || records[0].tokens() != 11 {
				t.Fatal("first completed response did not create exactly one usage record")
			}
			write("stale-model-b")
			readExact(scenario.stale)
			// The read follows bookkeeping in the forwarding pump. The fixture
			// cannot send r2 until these assertions inspect B while it is active.
			if p.inFlight("test") != 1 || len(p.Metrics.RecentForAccount("test", 10)) != 1 {
				t.Fatal("old response ID completed or recorded the still-active second turn")
			}
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			for _, model := range []string{"", "stale-model-a", "stale-model-b"} {
				cooldownCtx := ctx
				if model != "" {
					cooldownCtx = withGatewayCooldownModel(ctx, model)
				}
				if err := p.checkGatewayCooldown(cooldownCtx, account); err != nil {
					t.Fatal("stale response ID installed an account or model cooldown")
				}
			}
			allowCompletion()
			readExact(second)
			waitGatewayWSReleased(t, p)
			records := p.Metrics.RecentForAccount("test", 10)
			if len(records) != 2 || handshakes.Load() != 1 || creates.Load() != 2 || guard.calls.Load() != 0 {
				t.Fatal("stale response sequence changed record count, retried, or used Go fallback")
			}
			seen := make(map[string]bool)
			for _, record := range records {
				if seen[record.ResponseID] || record.Status != "ok" {
					t.Fatal("stale terminal duplicated an ID or changed successful response outcome")
				}
				seen[record.ResponseID] = true
				switch record.ResponseID {
				case "stale-fixture-r1":
					if record.Model != "stale-model-a" || record.InputTokens != 10 || record.OutputTokens != 1 {
						t.Fatal("first response usage changed after stale terminal")
					}
				case "stale-fixture-r2":
					if record.Model != "stale-model-b" || record.InputTokens != 20 || record.OutputTokens != 2 {
						t.Fatal("second response consumed stale terminal usage or model")
					}
				default:
					t.Fatal("unexpected response identity entered usage records")
				}
			}
		})
	}
}
