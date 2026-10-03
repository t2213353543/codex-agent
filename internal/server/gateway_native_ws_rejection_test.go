package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayWebSocketRejectedStatusesPreserveRoutingBoundary(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUpgradeRequired, http.StatusProxyAuthRequired} {
		t.Run(fmt.Sprint(status), func(t *testing.T) { exerciseGatewayWSRejectedStatus(t, status, false) })
	}
}

func TestGatewayNativeActualWebSocketRejectedStatuses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUpgradeRequired, http.StatusProxyAuthRequired} {
		t.Run(fmt.Sprint(status), func(t *testing.T) { exerciseGatewayWSRejectedStatus(t, status, true) })
	}
}

// These are target HTTP responses, not a CONNECT proxy. Proxy rejection has a
// separate typed transport path and existing native SOCKS-failure acceptance.
func exerciseGatewayWSRejectedStatus(t *testing.T, upstreamStatus int, native bool) {
	t.Helper()
	var handshakes, refreshes, posts atomic.Int32
	cert, _, ca := nativeCaptureCertificate(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			http.Error(w, "fixture must not refresh", 500)
			return
		}
		if r.Method != http.MethodGet {
			posts.Add(1)
			http.Error(w, "fixture must not send a model turn", 500)
			return
		}
		handshakes.Add(1)
		w.Header().Set("X-Request-Id", "rejected-fixture-id")
		w.Header().Set("X-Codex-Turn-State", "rejected-fixture-state")
		w.Header().Set("Retry-After", "3")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamStatus)
		_, _ = io.WriteString(w, `{"error":{"message":"synthetic rejected upgrade"}}`)
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
	upstream.StartTLS()
	defer upstream.Close()
	p := newTestProxy(t, 1)
	p.BaseURL, p.TokenURL, p.Client = upstream.URL, upstream.URL+"/oauth/token", upstream.Client()
	t.Cleanup(p.closeGatewayClients)
	guard := &nativeCLIFallbackGuard{}
	if native {
		t.Setenv("CODEX_CA_CERTIFICATE", ca)
		realGatewayNativePool(t, p)
		p.NativeWebSocket = true
		p.Client.Transport = guard
	}
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	want := upstreamStatus
	if upstreamStatus == http.StatusOK {
		want = http.StatusServiceUnavailable
	}
	for mode := range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		headers := http.Header{"Authorization": {"Bearer test-client-key"}}
		if mode == 0 {
			headers.Set("Session-Id", "header-rejected-fixture")
		}
		conn, response, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
		if mode == 0 {
			if conn != nil {
				_ = conn.CloseNow()
			}
			if err == nil || conn != nil || response == nil || response.StatusCode != want {
				cancel()
				t.Fatalf("header-routed rejection became an accepted upgrade or wrong status: upstream=%d want=%d", upstreamStatus, want)
			}
			if response.Header.Get("X-Request-Id") != "rejected-fixture-id" || response.Header.Get("X-Codex-Turn-State") != "rejected-fixture-state" || response.Header.Get("Retry-After") != "3" {
				cancel()
				t.Fatal("rejected handshake lost allowed response metadata")
			}
			if response.Body != nil {
				_ = response.Body.Close()
			}
		} else {
			if err != nil || conn == nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
				cancel()
				t.Fatal("body-routed fixture did not preserve independent downstream acceptance")
			}
			payload := []byte(`{"type":"response.create","stream_id":"rejected-stream","model":"fixture","prompt_cache_key":"body-rejected-fixture","input":[]}`)
			if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
				_ = conn.CloseNow()
				cancel()
				t.Fatal(err)
			}
			typ, raw, readErr := conn.Read(ctx)
			var failure struct {
				Type     string            `json:"type"`
				Status   int               `json:"status"`
				StreamID string            `json:"stream_id"`
				Headers  map[string]string `json:"headers"`
			}
			if readErr != nil || typ != websocket.MessageText || json.Unmarshal(raw, &failure) != nil || failure.Type != "error" || failure.Status != want || failure.StreamID != "rejected-stream" || failure.Headers["Retry-After"] != "3" {
				_ = conn.CloseNow()
				cancel()
				t.Fatal("body-routed rejection lost explicit error-frame status or stream identity")
			}
			_ = conn.CloseNow()
		}
		cancel()
		waitGatewayWSReleased(t, p)
		if handshakes.Load() != int32(mode+1) || refreshes.Load() != 0 || posts.Load() != 0 || guard.calls.Load() != 0 {
			t.Fatal("rejected upgrade refreshed credentials, sent a model request, retried or fell back to Go")
		}
	}
}
