package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndependentTerminalErrorTypeGatewayKeepsStatusPayloadAndFailover(t *testing.T) {
	for _, typ := range []string{"server_error", "invalid_request_error", "rate_limit_error", "authentication_error"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", typ, stream), func(t *testing.T) {
				calls := 0
				frame := fmt.Sprintf(`{"type":"response.failed","response":{"id":"error_type_fixture","status":"failed","model":"gpt-6-astra","output":[],"error":{"code":null,"type":%q,"message":"PowerPoint synthetic error"},"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}}`, typ)
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+frame+"\n\n")
				})
				w := httptest.NewRecorder()
				p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[]}`, stream)))
				wantHTTP := 502
				if stream {
					wantHTTP = 200
				}
				if typ == "authentication_error" {
					wantHTTP = http.StatusServiceUnavailable
				}
				if w.Code != wantHTTP || calls != 1 {
					t.Fatalf("transport status or sends changed: %d/%d", w.Code, calls)
				}
				var object map[string]json.RawMessage
				if stream && typ != "authentication_error" {
					events := bpsGatewayEvents(t, w.Body.String())
					if len(events) != 1 || bpsGatewayString(events[0], "type") != "response.failed" {
						t.Fatal("SSE terminal changed")
					}
					_ = json.Unmarshal(events[0]["response"], &object)
				} else {
					_ = json.Unmarshal(w.Body.Bytes(), &object)
				}
				var detail map[string]json.RawMessage
				_ = json.Unmarshal(object["error"], &detail)
				if typ == "authentication_error" {
					if string(detail["code"]) != `"upstream_authentication_unavailable"` || string(detail["type"]) != `"server_error"` {
						t.Fatal("public upstream authentication unavailable classification missing")
					}
				} else if string(detail["code"]) != "null" || string(detail["type"]) != fmt.Sprintf("%q", typ) || string(detail["message"]) != `"upstream synthetic error"` || strings.Contains(w.Body.String(), "error_type=") {
					t.Fatal("observed type changed client error/code")
				}
				r, ok := p.Metrics.LastFor("test")
				if !ok || r.Status != "error" || r.InputTokens != 100 || r.CachedTokens != 80 || r.CacheWriteTokens == nil || *r.CacheWriteTokens != 20 {
					t.Fatal("failure usage changed")
				}
				if !strings.Contains(r.Error, "(error_type="+typ+")") {
					t.Fatal("record still has an undifferentiated no-code terminal")
				}
				wantCode := ""
				if typ == "rate_limit_error" {
					wantCode = "rateLimitExceeded"
				}
				if typ == "authentication_error" {
					wantCode = "unauthorized"
				}
				if r.FinalUpstreamErrorCode != wantCode {
					t.Fatalf("existing typed event failover classification changed: got %q want %q", r.FinalUpstreamErrorCode, wantCode)
				}
			})
		}
	}
}

func TestIndependentTerminalErrorTypeLiveDiagnosticFilter(t *testing.T) {
	for _, typ := range []string{"server_error", "invalid_request_error", "rate_limit_error", "authentication_error"} {
		for _, code := range []string{"", " (code=context_length_exceeded)"} {
			value := "bps response terminated with response.failed (status failed)" + code + " (error_type=" + typ + ")"
			if bpsLiveDiagnosticError(value) != value {
				t.Fatal("safe type diagnostic was discarded by official live report")
			}
		}
	}
	for _, suffix := range []string{"PRIVATE_SECRET", "server_error PRIVATE_MESSAGE", "server_error) extra", "Server_Error"} {
		value := "bps response terminated with response.failed (status failed) (error_type=" + suffix + ")"
		if bpsLiveDiagnosticError(value) == value {
			t.Fatal("live diagnostic filter accepted untrusted error type")
		}
	}
}
