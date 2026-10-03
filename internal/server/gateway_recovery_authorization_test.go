package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayRecoveryAuthorizationHTTPRejectsRevocationDuringRefresh(t *testing.T) {
	for _, route := range []struct{ name, method, path, body, response string }{
		{"json", http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovery-control\",\"output\":[]}}\n\n"},
		{"sse", http.MethodPost, "/responses", `{"model":"fixture","stream":true,"input":[]}`, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"recovery-control\",\"output\":[]}}\n\n"},
		{"compact", http.MethodPost, "/responses/compact", `{"model":"fixture","input":[]}`, `{"output":[]}`},
		{"models", http.MethodGet, "/models", "", `{"models":[]}`},
	} {
		for _, mutation := range []string{"disabled", "rotated", "groups", "store_error", "unchanged"} {
			t.Run(route.name+"/"+mutation, func(t *testing.T) {
				var requests, refreshes atomic.Int32
				var writer *KeyStore
				var key APIKey
				var firstBody string
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path == "/oauth/token" {
						refreshes.Add(1)
						if mutation != "unchanged" {
							gatewayKeyAuthorizationMutate(t, writer, key, mutation)
						}
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(gatewayRecoveryAuthorizationTokens, "test-account", "fixture-user"))
						return
					}
					if requests.Add(1) == 1 {
						firstBody = string(body)
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"error":{"message":"synthetic initial unauthorized"}}`)
						return
					}
					if string(body) != firstBody || r.Header.Get("Authorization") != "Bearer synthetic-recovery-access" {
						t.Error("allowed recovery changed request bytes or failed to use current credentials")
					}
					_, _ = io.WriteString(w, route.response)
				})
				key, account := gatewaySessionAuthorizationFixture(t, p)
				writer = gatewayKeyAuthorizationWriter(t, p, key)
				r := proxyRequest(route.method, route.path, route.body)
				r.Header.Set("Authorization", "Bearer "+key.Key)
				r.Header.Set("Session-Id", "recovery-authorization-http")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				wantRequests := int32(1)
				if mutation == "unchanged" {
					wantRequests = 2
					if w.Code != http.StatusOK {
						t.Errorf("unchanged authorization rejected the permitted recovery: status=%d", w.Code)
					}
				} else if w.Code != gatewayRecoveryAuthorizationStatus(mutation) {
					t.Errorf("revoked recovery returned the wrong local status: %d", w.Code)
				}
				if requests.Load() != wantRequests || refreshes.Load() != 1 {
					t.Errorf("OAuth recovery dispatch count mismatch: requests=%d want=%d refreshes=%d", requests.Load(), wantRequests, refreshes.Load())
				}
				gatewayRecoveryAuthorizationAssertStored(t, p, account)
				gatewayRecoveryAuthorizationAssertRecord(t, p, mutation, route.path)
			})
		}
	}
}

func TestGatewayRecoveryAuthorizationWebSocketRejectsRevocationDuringRefresh(t *testing.T) {
	for _, mutation := range []string{"disabled", "rotated", "groups", "store_error", "unchanged"} {
		t.Run(mutation, func(t *testing.T) {
			var handshakes, refreshes, frames atomic.Int32
			var writer *KeyStore
			var key APIKey
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if mutation != "unchanged" {
						gatewayKeyAuthorizationMutate(t, writer, key, mutation)
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(gatewayRecoveryAuthorizationTokens, "test-account", "fixture-user"))
					return
				}
				if handshakes.Add(1) == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic handshake unauthorized"}}`)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					frames.Add(1)
				}
			})
			key, account := gatewaySessionAuthorizationFixture(t, p)
			writer = gatewayKeyAuthorizationWriter(t, p, key)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": {"Bearer " + key.Key}, "Session-Id": {"recovery-authorization-ws"}}
			conn, response, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if conn != nil {
				_ = conn.CloseNow()
			}
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			wantHandshakes := int32(1)
			if mutation == "unchanged" {
				wantHandshakes = 2
				if err != nil || conn == nil || response == nil || response.StatusCode != http.StatusSwitchingProtocols {
					t.Error("unchanged authorization rejected the permitted WebSocket recovery")
				}
			} else if err == nil || conn != nil || response == nil || response.StatusCode != gatewayRecoveryAuthorizationStatus(mutation) {
				t.Error("revoked key completed a recovered downstream WebSocket upgrade")
			}
			if handshakes.Load() != wantHandshakes || refreshes.Load() != 1 || frames.Load() != 0 {
				t.Errorf("WebSocket recovery dispatch count mismatch: handshakes=%d want=%d refreshes=%d frames=%d", handshakes.Load(), wantHandshakes, refreshes.Load(), frames.Load())
			}
			waitGatewayWSReleased(t, p)
			gatewayRecoveryAuthorizationAssertStored(t, p, account)
			p.Metrics.mu.Lock()
			businessRecords := len(p.Metrics.accounts)
			p.Metrics.mu.Unlock()
			if businessRecords != 0 {
				t.Fatal("WebSocket handshake without an admitted turn created business records")
			}
		})
	}
}

const gatewayRecoveryAuthorizationTokens = `{"access_token":"synthetic-recovery-access","refresh_token":"synthetic-recovery-refresh","expires_in":3600}`

func gatewayRecoveryAuthorizationStatus(mutation string) int {
	if mutation == "store_error" {
		return http.StatusServiceUnavailable
	}
	return http.StatusForbidden
}

func gatewayRecoveryAuthorizationAssertRecord(t *testing.T, p *Proxy, mutation, endpoint string) {
	t.Helper()
	rec, found := p.Metrics.LastFor("test")
	if !found {
		t.Fatal("HTTP recovery omitted its existing business record")
	}
	wantAttempts := 2
	wantError := redactMetricError((&gatewayRecoveryAuthorizationError{status: gatewayRecoveryAuthorizationStatus(mutation)}).Error())
	if mutation == "unchanged" {
		wantAttempts = 3
	} else if rec.HTTPCode != gatewayRecoveryAuthorizationStatus(mutation) || rec.ErrorSource != "gateway" || rec.ErrorPhase != "authorization" || rec.Error != wantError {
		t.Errorf("post-refresh authorization record mismatch: status=%d source=%q phase=%q fixed_error_match=%t", rec.HTTPCode, rec.ErrorSource, rec.ErrorPhase, rec.Error == wantError)
	}
	if len(rec.Attempts) != wantAttempts || rec.AttemptsTruncated == nil || *rec.AttemptsTruncated {
		t.Fatal("recovery observation fabricated a retry or lost an actual request")
	}
	for i, attempt := range rec.Attempts {
		phase, path, status := "request", endpoint, http.StatusOK
		if i == 0 {
			status = http.StatusUnauthorized
		} else if i == 1 {
			phase, path = "refresh", "/oauth/token"
		}
		if attempt.Attempt != i+1 || attempt.Phase != phase || attempt.Endpoint != path || attempt.StatusCode == nil || *attempt.StatusCode != status {
			t.Errorf("recovery attempt %d did not preserve its actual phase/endpoint/status", i+1)
		}
	}
}

func gatewayRecoveryAuthorizationAssertStored(t *testing.T, p *Proxy, account Account) {
	t.Helper()
	stored, err := p.Store.Get(account.ID)
	if err != nil || stored.AccessToken != "synthetic-recovery-access" || stored.RefreshToken != "synthetic-recovery-refresh" || stored.LastError != account.LastError {
		t.Fatal("authorization rejection discarded successful credential rotation or marked it as an OAuth failure")
	}
	if p.inFlight(account.ID) != 0 || p.inFlight("alternate") != 0 {
		t.Fatal("recovery authorization retained account admission")
	}
}
