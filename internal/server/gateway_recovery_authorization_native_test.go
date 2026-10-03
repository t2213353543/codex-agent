package server

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayNativeActualRecoveryAuthorizationRevokedKey(t *testing.T) {
	for _, mode := range []string{"http", "websocket"} {
		t.Run(mode, func(t *testing.T) {
			p := newTestProxy(t, 1)
			key, account := gatewaySessionAuthorizationFixture(t, p)
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			var upstreamCalls, refreshes atomic.Int32
			cert, _, ca := nativeCaptureCertificate(t)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.TLS == nil {
					t.Error("actual native recovery fixture did not use TLS")
				}
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					if _, err := writer.Update(key.ID, func(current *APIKey) error {
						current.Enabled = false
						return nil
					}); err != nil {
						t.Error("cannot revoke synthetic key during native OAuth recovery")
						http.Error(w, "synthetic fixture mutation failed", http.StatusInternalServerError)
						return
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(gatewayRecoveryAuthorizationTokens, account.AccountID, account.Identity.UserID))
					return
				}
				upstreamCalls.Add(1)
				if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+account.AccessToken {
					t.Error("native recovery dispatched an unexpected route or retry credential")
				}
				if mode == "websocket" {
					if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "websocket" || r.ProtoMajor != 1 {
						t.Error("native WebSocket recovery did not use a TLS HTTP/1 upgrade")
					}
				} else if r.Method != http.MethodPost || r.ProtoMajor != 2 {
					t.Error("native HTTP recovery did not use TLS HTTP/2 Responses")
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic native recovery unauthorized"}}`)
			}))
			upstream.EnableHTTP2 = true
			upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			upstream.Config.ErrorLog = log.New(io.Discard, "", 0)
			upstream.StartTLS()
			defer upstream.Close()
			p.BaseURL, p.TokenURL, p.Client = upstream.URL, upstream.URL+"/oauth/token", upstream.Client()
			t.Setenv("CODEX_CA_CERTIFICATE", ca)
			realGatewayNativePool(t, p)
			p.NativeWebSocket = true
			guard := &nativeCLIFallbackGuard{}
			p.Client.Transport = guard
			enableGatewayWS(t, p, "")
			if mode == "http" {
				r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`)
				r.Header.Set("Authorization", "Bearer "+key.Key)
				r.Header.Set("Session-Id", "native-recovery-authorization-http")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Errorf("native HTTP revoked retry status=%d, want403", w.Code)
				}
				gatewayRecoveryAuthorizationAssertRecord(t, p, "disabled", "/responses")
			} else {
				server := httptest.NewServer(p)
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				headers := http.Header{"Authorization": {"Bearer " + key.Key}, "Session-Id": {"native-recovery-authorization-ws"}}
				conn, response, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
				if conn != nil {
					_ = conn.CloseNow()
				}
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				if err == nil || conn != nil || response == nil || response.StatusCode != http.StatusForbidden {
					t.Error("native WebSocket revoked retry was accepted or lost its403")
				}
				waitGatewayWSReleased(t, p)
				p.Metrics.mu.Lock()
				businessRecords := len(p.Metrics.accounts)
				p.Metrics.mu.Unlock()
				if businessRecords != 0 {
					t.Fatal("rejected native handshake created a business turn record")
				}
			}
			if upstreamCalls.Load() != 1 || refreshes.Load() != 1 || guard.calls.Load() != 0 {
				t.Errorf("native recovery retried or fell back to Go: upstream=%d refresh=%d go=%d", upstreamCalls.Load(), refreshes.Load(), guard.calls.Load())
			}
			gatewayRecoveryAuthorizationAssertStored(t, p, account)
			gatewayKeyAuthorizationAssertStaleIndex(t, p, key)
		})
	}
}
