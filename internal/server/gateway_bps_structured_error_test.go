package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSStructuredInvalidResultReturnsFailedTerminal(t *testing.T) {
	for _, transport := range []string{"sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				bpsStructuredUpstreamReply(t, w, r, "resp_invalid_json", `{"number":2}`)
			})
			var event map[string]json.RawMessage
			if transport == "sse" {
				r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"answer","text":`+bpsStructuredTestText+`}`)
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				events := bpsGatewayEvents(t, w.Body.String())
				if len(events) != 1 || strings.Contains(w.Body.String(), "response.completed") {
					t.Fatal("validation failure did not emit one explicit terminal")
				}
				event = events[0]
			} else {
				s := httptest.NewServer(p)
				defer s.Close()
				conn, _ := gatewayWSDial(t, s.URL, "invalid-schema-session")
				defer conn.CloseNow()
				bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"invalid-json","model":"gpt-6-astra","input":"answer","text":`+bpsStructuredTestText+`}`)
				event = bpsGatewayWSReadEvent(t, conn)
			}
			if bpsGatewayString(event, "type") != "response.failed" {
				t.Fatal("client received no failed terminal")
			}
			var response struct {
				ID, Status string
				Output     []json.RawMessage
				Error      struct{ Code string }
				Usage      struct {
					Input int `json:"input_tokens"`
				}
			}
			_ = json.Unmarshal(event["response"], &response)
			if response.ID != "resp_invalid_json" || response.Status != "failed" || len(response.Output) != 0 || response.Error.Code != "response_validation_failed" || response.Usage.Input != 100 {
				t.Fatal("failed terminal lost real identity/usage or exposed invalid output")
			}
			wire, _ := json.Marshal(event)
			if bytes.Contains(wire, []byte("emit_structured_")) || bytes.Contains(wire, []byte(`"number":2`)) {
				t.Fatal("invalid private output leaked")
			}
		})
	}
}
