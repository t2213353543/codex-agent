package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBPSAStructuredFailedDiagnosticHTTPBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var posts atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				body, _ := io.ReadAll(r.Body)
				var request struct {
					Input []struct{ Tools []struct{ Name string } }
				}
				if json.Unmarshal(body, &request) != nil {
					t.Error("invalid synthetic request")
					return
				}
				name := ""
				for _, item := range request.Input {
					for _, tool := range item.Tools {
						if strings.HasPrefix(tool.Name, "emit_structured_") {
							name = tool.Name
						}
					}
				}
				if name == "" {
					t.Error("formatter was not actually prepared")
					return
				}
				response := map[string]any{"id": "resp_a_structured_fail", "status": "failed", "model": "gpt-6-astra", "output": []any{}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 7}}, "message": "failed " + name, "detail": "details " + name}
				terminal, _ := json.Marshal(map[string]any{"type": "error", "code": "server_error", "message": "failed " + name, "param": name, "response": response, "ordinal": json.RawMessage(`9007199254740993`)})
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
			})
			out := httptest.NewRecorder()
			p.gatewayBPS(out, proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic","text":%s}`, stream, bpsStructuredTestText)))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || posts.Load() != 1 || rec.Status != "error" || len(rec.Attempts) != 1 || rec.InputTokens != 100 || rec.OutputTokens != 10 || rec.CachedTokens != 80 || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 7 || rec.UsageKnown == nil || !*rec.UsageKnown || rec.TTFTMs != nil {
				t.Fatalf("real failed accounting changed: status=%s calls=%d in=%d out=%d read=%d", rec.Status, posts.Load(), rec.InputTokens, rec.OutputTokens, rec.CachedTokens)
			}
			if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "resp_a_structured_fail"); err == nil {
				t.Fatal("failed result saved history")
			}
			var detail map[string]json.RawMessage
			if stream {
				events := bpsGatewayEvents(t, out.Body.String())
				if out.Code != 200 || len(events) != 1 || bpsGatewayString(events[0], "type") != "response.failed" || string(events[0]["ordinal"]) != "9007199254740993" {
					t.Fatal("SSE failed terminal or precise extension changed")
				}
				var response map[string]json.RawMessage
				_ = json.Unmarshal(events[0]["response"], &response)
				_ = json.Unmarshal(response["error"], &detail)
			} else {
				var body map[string]json.RawMessage
				if out.Code != 502 || json.Unmarshal(out.Body.Bytes(), &body) != nil {
					t.Fatal("JSON failure changed")
				}
				_ = json.Unmarshal(body["error"], &detail)
			}
			if bpsGatewayString(detail, "code") != "server_error" || bpsGatewayString(detail, "message") != "failed structured output" || bpsGatewayString(detail, "param") != "structured output" {
				t.Fatal("existing nested error contract changed")
			}
			if bytes.Contains(out.Body.Bytes(), []byte("emit_structured_")) {
				t.Fatal("full gateway disclosed internal formatter in failed diagnostic")
			}
			t.Logf("stream=%t http=%d posts=%d usage=100/10/80/7 nohistory=true", stream, out.Code, posts.Load())
		})
	}
}
