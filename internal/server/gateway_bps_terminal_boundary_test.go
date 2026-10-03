package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Every terminal accepted by the BPS transformer must also retire its WebSocket
// lane. A second turn proves no synthetic interruption is left on the socket.
func TestBPSWebSocketTerminalAliasesAllowNextTurn(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
	}{
		{"response.done", "completed"},
		{"basispoints.response.cancelled", "cancelled"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					_, _ = io.WriteString(w, `data: {"type":"`+tc.kind+`","response":{"id":"resp_alias","status":"`+tc.status+`","output":[]}}`+"\n\n")
					return
				}
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_next", "[]"))
			})
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "bps-terminal-alias")
			defer conn.CloseNow()
			const request = `{"type":"response.create","stream_id":"reused","model":"gpt-6-astra","input":[]}`
			bpsGatewayWSSend(t, conn, request)
			terminal := bpsGatewayWSReadEvent(t, conn)
			if bpsGatewayString(terminal, "type") != tc.kind || bpsGatewayString(terminal, "stream_id") != "reused" || bpsGatewayWSResponseID(t, terminal) != "resp_alias" {
				t.Fatalf("upstream terminal changed: %s", terminal)
			}
			bpsGatewayWSSend(t, conn, request)
			next := bpsGatewayWSReadEvent(t, conn)
			if bpsGatewayString(next, "type") != "response.completed" || bpsGatewayString(next, "stream_id") != "reused" || bpsGatewayWSResponseID(t, next) != "resp_next" || calls.Load() != 2 {
				t.Fatalf("valid terminal polluted the next turn: event=%s upstream_calls=%d", next, calls.Load())
			}
		})
	}
}
