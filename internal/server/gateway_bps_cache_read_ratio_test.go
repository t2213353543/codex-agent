package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBPSCacheReadRatioBillingProjectionKeepsActualAccounting(t *testing.T) {
	for _, mode := range []string{"json", "sse", "compact_json", "compact_sse", "structured_json", "structured_sse"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", mode, enabled), func(t *testing.T) {
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(mode, "structured") {
						fixture := httptest.NewRecorder()
						bpsStructuredUpstreamReply(t, fixture, r, "billing_response", `{"number":9007199254740993}`)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write(bytes.ReplaceAll(fixture.Body.Bytes(), []byte(`"cache_write_tokens":0`), []byte(`"cache_write_tokens":7`)))
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					output := `[{"type":"message","id":"msg_billing","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]`
					if strings.HasPrefix(mode, "compact") {
						output = `[{"type":"compaction","encrypted_content":"ACTUAL_OPAQUE_STATE"}]`
					}
					response := `{"id":"billing_response","model":"gpt-6-astra","status":"completed","output":` + output + `,"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}}`
					if strings.HasPrefix(mode, "compact") {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				})
				settings, _ := p.Settings.Get()
				settings.CacheCreationAsReadEnabled = true
				settings.CacheReadRatioEnabled = enabled
				settings.CacheReadRatioPercent = 25
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				stream := strings.HasSuffix(mode, "sse")
				body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic"}`, stream)
				path := "/v1/responses"
				if mode == "compact_json" {
					path += "/compact"
				}
				if mode == "compact_sse" {
					body = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"compaction_trigger"}]}`
				}
				if strings.HasPrefix(mode, "structured") {
					body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic","text":%s}`, stream, bpsStructuredTestText)
				}
				out := httptest.NewRecorder()
				p.gatewayBPS(out, proxyRequest("POST", path, body))
				if out.Code != 200 {
					t.Fatalf("HTTP%d %s", out.Code, out.Body.String())
				}
				wire := out.Body.Bytes()
				if stream {
					events := bpsGatewayEvents(t, out.Body.String())
					wire = events[len(events)-1]["response"]
				}
				var got bpsCacheLiveTerminal
				if json.Unmarshal(wire, &got) != nil || got.Usage.Details.Cached == nil || got.Usage.Details.Write == nil {
					t.Fatal("client accounting absent")
				}
				wantRead, wantWrite := int64(87), int64(0)
				if enabled {
					wantRead, wantWrite = 25, 0
				}
				if got.Usage.Input != 100 || got.Usage.Output != 10 || *got.Usage.Details.Cached != wantRead || *got.Usage.Details.Write != wantWrite {
					t.Fatalf("client usage=%+v", got.Usage)
				}
				record, ok := p.Metrics.LastFor("test")
				if !ok || record.Status != "ok" || record.InputTokens != 100 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 7 {
					t.Fatal("client billing projection contaminated actual metrics")
				}
				if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "billing_response"); err != nil {
					t.Fatal("successful history missing")
				}
			})
		}
	}
}

func TestBPSCacheReadRatioBillingWebSocketAndFailedTerminals(t *testing.T) {
	for _, mode := range []string{"websocket", "failed_sse", "structured_failed_sse"} {
		t.Run(mode, func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "structured_failed_sse" {
					fixture := httptest.NewRecorder()
					bpsStructuredUpstreamReply(t, fixture, r, "billing_error", `{"number":2}`)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write(bytes.ReplaceAll(fixture.Body.Bytes(), []byte(`"cache_write_tokens":0`), []byte(`"cache_write_tokens":7`)))
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				status := "completed"
				if mode == "failed_sse" {
					status = "failed"
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, `data: {"type":"response.%s","response":{"id":"billing_ws","model":"gpt-6-astra","status":%q,"output":[],"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}}}`+"\n\n", status, status)
			})
			s, _ := p.Settings.Get()
			s.CacheCreationAsReadEnabled = true
			s.CacheReadRatioEnabled = true
			s.CacheReadRatioPercent = 25
			if err := p.Settings.Set(s); err != nil {
				t.Fatal(err)
			}
			var event map[string]json.RawMessage
			if mode == "websocket" {
				server := httptest.NewServer(p)
				defer server.Close()
				conn, _ := gatewayWSDial(t, server.URL, "billing-websocket")
				defer conn.CloseNow()
				bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"billing","model":"gpt-6-astra","input":"synthetic"}`)
				event = bpsGatewayWSUntilTerminal(t, conn, "billing")
			} else {
				body := `{"model":"gpt-6-astra","stream":true,"input":"synthetic"}`
				if mode == "structured_failed_sse" {
					body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"input":"synthetic","text":%s}`, bpsStructuredTestText)
				}
				out := httptest.NewRecorder()
				p.gatewayBPS(out, proxyRequest("POST", "/v1/responses", body))
				events := bpsGatewayEvents(t, out.Body.String())
				event = events[len(events)-1]
			}
			var got bpsCacheLiveTerminal
			if json.Unmarshal(event["response"], &got) != nil || got.Usage.Details.Cached == nil || got.Usage.Details.Write == nil || *got.Usage.Details.Cached != 25 || *got.Usage.Details.Write != 0 {
				t.Fatalf("public projection missing: %s", event["response"])
			}
			var record RequestRecord
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				var ok bool
				record, ok = p.Metrics.LastFor("test")
				if ok {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 7 {
				t.Fatal("public billing changed upstream record")
			}
			if mode != "websocket" && (bpsGatewayString(event, "type") != "response.failed" || record.Status != "error") {
				t.Fatal("billing projection changed failure status")
			}
		})
	}
}
