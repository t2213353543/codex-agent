package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSUpstreamAuthenticationWebSocketKeepsConnection(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","type":"authentication_error","message":"synthetic expired oauth"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "ws_auth_recovered", "[]"))
	})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.Upstream401RecoveryEnabled, settings.AutomaticTokenRefreshEnabled = false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	conn, handshake := gatewayWSDial(t, server.URL, "upstream-auth-status")
	defer conn.CloseNow()
	if handshake.StatusCode != 101 {
		t.Fatal("fixture did not establish a real WebSocket")
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"auth_failure","model":"gpt-6-astra","input":"synthetic first request"}`)
	event := bpsGatewayWSUntilTerminal(t, conn, "auth_failure")
	var status int
	var detail struct{ Code, Type string }
	if bpsGatewayString(event, "type") != "error" || json.Unmarshal(event["status"], &status) != nil || status != 503 ||
		json.Unmarshal(event["error"], &detail) != nil || detail.Code != "upstream_authentication_unavailable" || detail.Type != "server_error" {
		t.Fatalf("upstream authentication escaped as a client credential error: %s", event)
	}
	var failed RequestRecord
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		if record, ok := p.Metrics.LastFor("test"); ok {
			failed = record
			break
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 || failed.HTTPCode != 503 || failed.ErrorSource != "upstream" || failed.RequestType != "websocket" ||
		len(failed.Attempts) != 1 || failed.Attempts[0].StatusCode == nil || *failed.Attempts[0].StatusCode != 401 ||
		failed.Attempts[0].ObservedHTTPStatusCode == nil || *failed.Attempts[0].ObservedHTTPStatusCode != 401 {
		t.Fatal("WebSocket public status replaced actual attempt evidence or retried")
	}
	// A new lane on the same socket must remain usable; no whole-connection
	// shutdown and no replay of the failed input is permitted.
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"after_auth_failure","model":"gpt-6-astra","input":"synthetic independent second request"}`)
	event = bpsGatewayWSUntilTerminal(t, conn, "after_auth_failure")
	if bpsGatewayString(event, "type") != "response.completed" || !strings.Contains(string(event["response"]), "ws_auth_recovered") || calls.Load() != 2 {
		t.Fatal("failed lane closed connection, retried or prevented the next independent request")
	}
}
