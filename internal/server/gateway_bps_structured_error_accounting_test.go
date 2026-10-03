package server

import (
	"context"
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
	"github.com/coder/websocket"
)

func TestBPSStructuredErrorAccounting(t *testing.T) {
	format, err := bps.ParseStructuredFormat(json.RawMessage(bpsStructuredTestText))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"schema_rejection", "projection_identity_rejection", "completion_authorization_rejection"} {
		for _, transport := range []string{"json", "sse", "websocket"} {
			t.Run(failure+"/"+transport, func(t *testing.T) {
				var calls atomic.Int32
				var proxy *Proxy
				proxy = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					arguments, itemID := `{"number":9007199254740993}`, "fc_delivery"
					if failure == "schema_rejection" {
						arguments = `{"number":2}`
					}
					if failure == "projection_identity_rejection" {
						itemID = ""
					}
					if failure == "completion_authorization_rejection" {
						if err := proxy.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
							t.Error(err)
						}
						proxy.invalidateAccounts()
					}
					item := map[string]any{"type": "function_call", "name": format.ToolName, "id": itemID, "call_id": "call_delivery", "status": "completed", "arguments": arguments}
					response := map[string]any{"id": "resp_delivery", "model": "gpt-6-astra", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": 80, "cache_write_tokens": 7}}}
					event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				})
				body := fmt.Sprintf(`{"model":"gpt-6-astra","input":"synthetic delivery","stream":%t,"text":%s}`, transport != "json", bpsStructuredTestText)
				wire, status, aborted := "", 0, false
				if transport != "websocket" {
					response := httptest.NewRecorder()
					func() {
						defer func() {
							if got := recover(); got != nil {
								if got == http.ErrAbortHandler {
									aborted = true
								} else {
									panic(got)
								}
							}
						}()
						proxy.ServeHTTP(response, proxyRequest(http.MethodPost, "/v1/responses", body))
					}()
					wire, status = response.Body.String(), response.Code
				} else {
					server := httptest.NewServer(proxy)
					defer server.Close()
					conn, _ := gatewayWSDial(t, server.URL, "structured-delivery-session")
					defer conn.CloseNow()
					request := `{"type":"response.create","stream_id":"delivery","model":"gpt-6-astra","input":"synthetic delivery","text":` + bpsStructuredTestText + `}`
					bpsGatewayWSSend(t, conn, request)
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					kind, raw, readErr := conn.Read(ctx)
					if readErr != nil {
						t.Fatalf("websocket returned no bounded error frame: %v", readErr)
					}
					if kind != websocket.MessageText {
						t.Fatal("non-text error")
					}
					wire = string(raw)
					var frame struct{ Status int }
					_ = json.Unmarshal(raw, &frame)
					status = frame.Status
				}
				record, known := proxy.Metrics.LastFor("test")
				// A real failed terminal can reach the client before the producer
				// unwinds and persists its one request record. Await that boundary.
				deadline := time.Now().Add(time.Second)
				for !known && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
					record, known = proxy.Metrics.LastFor("test")
				}
				t.Logf("transport=%s failure=%s status=%d aborted=%t bytes=%d upstream_calls=%d record_exists=%t input=%d cached=%d error=%q wire=%s", transport, failure, status, aborted, len(wire), calls.Load(), known, record.InputTokens, record.CachedTokens, record.Error, wire)
				if strings.Contains(wire, "response.completed") {
					t.Error("invalid final result became completed")
				}
				wantPhase := "response_validation"
				if failure == "completion_authorization_rejection" {
					wantPhase = "authorization"
				}
				if record.ErrorSource != "gateway" || record.ErrorPhase != wantPhase {
					t.Error("local validation failure mislabeled as an upstream failure")
				}
				if calls.Load() != 1 {
					t.Errorf("unexpected generation retry: %d", calls.Load())
				}
				if !known || record.Status != "error" || record.InputTokens != 100 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 7 {
					t.Error("completed upstream usage was not retained in the failed gateway request record")
				}
			})
		}
	}
}
