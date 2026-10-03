package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func rotateRequestProxy(t *testing.T, p *Proxy, id, endpoint string) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Error(err)
		return
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Error(err)
		return
	}
	_, err = p.Proxies.UpdateVersion(id, 0, func(entry *ProxyEntry) error {
		entry.Host, entry.Port = u.Hostname(), port
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

func TestGatewayExpiryRefreshDoesNotSwitchProxyVersionMidRequest(t *testing.T) {
	for _, endpoint := range []string{"/responses", "/responses/compact", "/models"} {
		t.Run(endpoint, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.BaseURL, p.TokenURL = "http://route-upstream.invalid", "http://route-upstream.invalid/oauth/token"
			var refreshes, oldInference, newRequests atomic.Int32
			newProxy, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				newRequests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/token" || r.Header.Get("Authorization") != "Bearer updated-on-original-proxy" {
					t.Error("new admission lost the persisted OAuth refresh")
				}
				if r.URL.Path == "/responses" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"output":[],"models":[]}`)
				}
			})
			var proxyID string
			oldProxy, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path != "/oauth/token" {
					oldInference.Add(1)
					http.Error(w, "must not infer with a changed route", http.StatusInternalServerError)
					return
				}
				refreshes.Add(1)
				rotateRequestProxy(t, p, proxyID, newProxy.URL)
				_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"updated-on-original-proxy","refresh_token":"updated-refresh","expires_in":3600}`, "test-account", "fixture-user"))
			})
			entry := poolProxyEntry(t, p, oldProxy.URL)
			proxyID = entry.ID
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Proxies = []string{entry.ID}
				a.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			request := func() *httptest.ResponseRecorder {
				method, body := http.MethodPost, `{"model":"test","stream":true,"input":[]}`
				if endpoint == "/models" {
					method, body = http.MethodGet, ""
				}
				r := proxyRequest(method, endpoint, body)
				r.Header.Set("Session-Id", "version-pinned-session")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				return w
			}
			if w := request(); w.Code != http.StatusBadGateway || refreshes.Load() != 1 || oldInference.Load() != 0 || newRequests.Load() != 0 {
				t.Fatalf("changed route was used mid-request: HTTP=%d refresh=%d old=%d new=%d", w.Code, refreshes.Load(), oldInference.Load(), newRequests.Load())
			}
			if w := request(); w.Code != http.StatusOK || newRequests.Load() != 1 || refreshes.Load() != 1 {
				t.Fatalf("next admission did not adopt current route: HTTP=%d refresh=%d new=%d", w.Code, refreshes.Load(), newRequests.Load())
			}
		})
	}
}

func TestGatewayRequestPinnedClientRejectsVersionChangeAcrossModes(t *testing.T) {
	p := newTestProxy(t, 1)
	var hits atomic.Int32
	server, _ := poolProxyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "ok")
	})
	entry := poolProxyEntry(t, p, server.URL)
	ctx := withGatewayRequestRoute(context.Background(), "key", "session")
	if poolFetch(t, p.gatewayRequestClient(ctx, &entry, gatewayClientHTTP)) != "ok" {
		t.Fatal("initial request route was not used")
	}
	// Even an ABA edit back to identical endpoint/credentials is a new version.
	_, err := p.Proxies.UpdateVersion(entry.ID, 0, func(p *ProxyEntry) error { p.Password = "rotated"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Proxies.UpdateVersion(entry.ID, 0, func(p *ProxyEntry) error { p.Password = ""; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []gatewayClientMode{gatewayClientHTTP, gatewayClientCompact, gatewayClientWebSocket} {
		res, err := p.gatewayRequestClient(ctx, &entry, mode).Get("http://route-upstream.invalid/must-not-send")
		if res != nil {
			_ = res.Body.Close()
		}
		if !errors.Is(err, errGatewayProxyUnavailable) {
			t.Fatalf("mode %d adopted a changed request route: %v", mode, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("pinned request sent traffic after configuration changed")
	}
	if poolFetch(t, p.gatewayRequestClient(context.Background(), &entry, gatewayClientHTTP)) != "ok" || hits.Load() != 2 {
		t.Fatal("management client did not adopt the current configuration")
	}
}

func TestGatewayWebSocketRecoveryDoesNotSwitchProxyVersion(t *testing.T) {
	for _, rotateAt := range []string{"unauthorized-upgrade", "oauth-response"} {
		t.Run(rotateAt, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.BaseURL, p.TokenURL = "http://route-upstream.invalid", "http://route-upstream.invalid/oauth/token"
			enableGatewayWS(t, p, "")
			var oldUpgrades, refreshes, newUpgrades atomic.Int32
			newProxy, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				newUpgrades.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
			})
			var proxyID string
			oldProxy, _ := poolProxyServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					rotateRequestProxy(t, p, proxyID, newProxy.URL)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"ws-updated","refresh_token":"ws-refresh","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				oldUpgrades.Add(1)
				if rotateAt == "unauthorized-upgrade" {
					rotateRequestProxy(t, p, proxyID, newProxy.URL)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"original upgrade rejection"}}`)
			})
			entry := poolProxyEntry(t, p, oldProxy.URL)
			proxyID = entry.ID
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(p)
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dial := func() (*websocket.Conn, *http.Response, error) {
				return websocket.Dial(ctx, gateway.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"pinned-ws-session"}}})
			}
			conn, res, err := dial()
			if conn != nil {
				_ = conn.CloseNow()
			}
			wantStatus, wantRefreshes := http.StatusUnauthorized, int32(0)
			if rotateAt == "oauth-response" {
				wantStatus, wantRefreshes = http.StatusServiceUnavailable, 1
			}
			if res != nil && res.Body != nil {
				_ = res.Body.Close()
			}
			if err == nil || res == nil || res.StatusCode != wantStatus || oldUpgrades.Load() != 1 || refreshes.Load() != wantRefreshes || newUpgrades.Load() != 0 {
				t.Fatalf("upgrade changed route during recovery: response=%v err=%v old=%d refresh=%d new=%d", res, err, oldUpgrades.Load(), refreshes.Load(), newUpgrades.Load())
			}
			conn, res, err = dial()
			if err != nil {
				if res != nil && res.Body != nil {
					_ = res.Body.Close()
				}
				t.Fatalf("next admission could not use new route: %v", err)
			}
			_ = conn.CloseNow()
			if newUpgrades.Load() != 1 {
				t.Fatalf("next admission used %d new upgrades", newUpgrades.Load())
			}
		})
	}
}
