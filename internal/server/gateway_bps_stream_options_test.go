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

func TestBPSGatewayStreamOptionsKeepsTerminalUsageWithoutForwarding(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, includeUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/include_usage=%t", streaming, includeUsage), func(t *testing.T) {
				calls := 0
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					var request map[string]json.RawMessage
					if json.Unmarshal(body, &request) != nil || request["stream_options"] != nil {
						t.Errorf("downstream option leaked to upstream: %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_usage", "[]"))
				})
				body := fmt.Sprintf(`{"model":"gpt-6-astra","input":"fixture","stream":%t,"stream_options":{"include_usage":%t}}`, streaming, includeUsage)
				w := httptest.NewRecorder()
				p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", body))
				if w.Code != http.StatusOK || calls != 1 {
					t.Fatalf("compatible request failed: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
				}
				result := w.Body.Bytes()
				if streaming {
					events := bpsGatewayEvents(t, string(result))
					if len(events) != 1 || bpsGatewayString(events[0], "type") != "response.completed" {
						t.Fatalf("terminal response missing: %s", result)
					}
					result = events[0]["response"]
				}
				var response struct {
					Usage struct {
						Input  int `json:"input_tokens"`
						Output int `json:"output_tokens"`
						Total  int `json:"total_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal(result, &response) != nil || response.Usage.Input != 12 || response.Usage.Output != 3 || response.Usage.Total != 15 {
					t.Fatalf("observed terminal usage changed: %s", result)
				}
			})
		}
	}
}

func TestBPSGatewayUnsupportedStreamOptionsAreTypedAndNeverSent(t *testing.T) {
	for _, tc := range []struct{ value, param string }{
		{`{"include_usage":"true"}`, "stream_options.include_usage"},
		{`{"include_usage":null}`, "stream_options.include_usage"},
		{`{"reasoning_summary_delivery":"concurrent"}`, "stream_options.reasoning_summary_delivery"},
		{`[]`, "stream_options"},
	} {
		t.Run(tc.param+tc.value, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
			w := httptest.NewRecorder()
			p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":[],"stream_options":`+tc.value+`}`))
			var response struct{ Error struct{ Code, Param string } }
			if w.Code != http.StatusBadRequest || calls != 0 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error.Code != "unsupported_parameter" || response.Error.Param != tc.param || strings.Contains(w.Body.String(), "PowerPoint") {
				t.Fatalf("invalid transport option was not rejected precisely: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
