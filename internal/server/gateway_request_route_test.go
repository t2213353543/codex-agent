package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayExpiryRefreshUsesRequestProxy(t *testing.T) {
	for _, sticky := range []bool{true, false} {
		t.Run(strconv.FormatBool(sticky), func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.BaseURL, p.TokenURL = "http://unrouted.invalid", "http://unrouted.invalid/oauth/token"
			settings, _ := p.Settings.Get()
			settings.StickySessionsEnabled = sticky
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var paths [2][]string
			var proxyIDs []string
			for i := range 2 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					mu.Lock()
					paths[i] = append(paths[i], r.URL.Path)
					mu.Unlock()
					if r.URL.Path == "/oauth/token" {
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"routed-renewed","refresh_token":"routed-rotation","expires_in":3600}`, "test-account", "fixture-user"))
						return
					}
					if r.Header.Get("Authorization") != "Bearer routed-renewed" {
						t.Error("inference used the stale token")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"routed\",\"output\":[]}}\n\n")
				}))
				t.Cleanup(server.Close)
				u, _ := url.Parse(server.URL)
				port, _ := strconv.Atoi(u.Port())
				entry, err := p.Proxies.Create(ProxyEntry{Name: "route-" + strconv.Itoa(i), Type: "http", Host: u.Hostname(), Port: port})
				if err != nil {
					t.Fatal(err)
				}
				proxyIDs = append(proxyIDs, entry.ID)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.Proxies = proxyIDs
				a.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`)
			r.Header.Set("Session-Id", "routed-session")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("routed request HTTP %d: %s", w.Code, w.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if !((len(paths[0]) == 2 && len(paths[1]) == 0) || (len(paths[1]) == 2 && len(paths[0]) == 0)) {
				t.Fatalf("OAuth and inference used different proxies: %v", paths)
			}
			for _, seen := range paths {
				if len(seen) > 0 && strings.Join(seen, ",") != "/oauth/token,/responses" {
					t.Fatalf("unexpected route sequence: %v", seen)
				}
			}
		})
	}
}

func TestGatewayConfiguredUnavailableProxyNeverFallsBackDirect(t *testing.T) {
	for _, endpoint := range []string{"/responses", "/models"} {
		t.Run(endpoint, func(t *testing.T) {
			var requests atomic.Int32
			p := proxyFixture(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{"missing-proxy"}; return nil }); err != nil {
				t.Fatal(err)
			}
			method, body := http.MethodPost, `{"model":"test","stream":true}`
			if endpoint == "/models" {
				method, body = http.MethodGet, ""
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(method, endpoint, body))
			if w.Code != http.StatusServiceUnavailable || requests.Load() != 0 {
				t.Fatalf("configured proxy bypass: HTTP=%d requests=%d", w.Code, requests.Load())
			}
		})
	}
}

func TestGatewayWebSocketUpgradeRecovers401Once(t *testing.T) {
	for _, rejectedAgain := range []bool{false, true} {
		t.Run(strconv.FormatBool(rejectedAgain), func(t *testing.T) {
			var upgrades, refreshes, frames atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"websocket-renewed","refresh_token":"websocket-rotation","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				attempt := upgrades.Add(1)
				if attempt == 1 || rejectedAgain {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"upgrade rejected"}}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer websocket-renewed" || r.Header.Get("Session-Id") != "refresh-ws-session" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
					t.Error("WebSocket recovery changed identity or did not update bearer")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, err = conn.Read(r.Context())
				if err != nil {
					return
				}
				frames.Add(1)
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"ws-recovered","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, res, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"refresh-ws-session"}}})
			if rejectedAgain {
				if conn != nil {
					_ = conn.CloseNow()
				}
				if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
					t.Fatalf("second upgrade rejection not preserved: %v", err)
				}
				if res != nil && res.Body != nil {
					_ = res.Body.Close()
				}
			} else {
				if err != nil {
					t.Fatalf("recovered upgrade failed: %v", err)
				}
				defer conn.CloseNow()
				if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				if _, _, err = conn.Read(ctx); err != nil {
					t.Fatal(err)
				}
				if frames.Load() != 1 {
					t.Fatal("response.create was duplicated during handshake recovery")
				}
				_ = conn.CloseNow()
			}
			if upgrades.Load() != 2 || refreshes.Load() != 1 {
				t.Fatalf("unbounded upgrade recovery: upgrades=%d refreshes=%d", upgrades.Load(), refreshes.Load())
			}
		})
	}
}
