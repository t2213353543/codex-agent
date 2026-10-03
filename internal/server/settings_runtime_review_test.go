package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func setRuntimeReviewSettings(t *testing.T, p *Proxy, change func(*Settings)) {
	t.Helper()
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	change(&settings)
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRuntimeAutomaticRefreshExpiryPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, expiry := range []struct {
			name  string
			delta time.Duration
		}{{"unknown", 0}, {"fresh", time.Hour}, {"near_expiry", 30 * time.Second}, {"expired", -time.Hour}} {
			t.Run(fmt.Sprintf("enabled_%v/%s", enabled, expiry.name), func(t *testing.T) {
				var refreshes, requests atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/oauth/token" {
						refreshes.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"review-renewed","refresh_token":"review-rotated","expires_in":3600}`, "test-account", "fixture-user"))
						return
					}
					requests.Add(1)
					wantBearer := "Bearer test-upstream-token"
					if enabled && (expiry.name == "near_expiry" || expiry.name == "expired") {
						wantBearer = "Bearer review-renewed"
					}
					if r.Header.Get("Authorization") != wantBearer {
						t.Error("expiry selection used an unexpected credential snapshot")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"review\",\"output\":[]}}\n\n")
				})
				setRuntimeReviewSettings(t, p, func(s *Settings) {
					s.AutomaticTokenRefreshEnabled = enabled
					s.Upstream401RecoveryEnabled = false
				})
				if err := p.Store.UpdateAccount("test", func(a *Account) error {
					a.ExpiresAt = 0
					if expiry.delta != 0 {
						a.ExpiresAt = time.Now().Add(expiry.delta).Unix()
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false,"input":[]}`))
				wantStatus, wantRequests, wantRefreshes := http.StatusOK, int32(1), int32(0)
				if enabled && (expiry.name == "near_expiry" || expiry.name == "expired") {
					wantRefreshes = 1
				}
				if !enabled && expiry.name == "expired" {
					wantStatus, wantRequests = http.StatusServiceUnavailable, 0
					stored, err := p.Store.Get("test")
					if err != nil || !strings.Contains(stored.LastError, "automatic refresh is disabled") {
						t.Fatalf("expired account did not expose its disabled-refresh reason: %v", err)
					}
				}
				if w.Code != wantStatus || refreshes.Load() != wantRefreshes || requests.Load() != wantRequests || p.inFlight("test") != 0 {
					t.Fatalf("expiry policy: status=%d refreshes=%d requests=%d inflight=%d", w.Code, refreshes.Load(), requests.Load(), p.inFlight("test"))
				}
			})
		}
	}
}

func TestSettingsRuntimeManualRefreshRemainsAvailable(t *testing.T) {
	var refreshes atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Error("manual refresh called an inference endpoint")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		refreshes.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"review-manual-renewed","refresh_token":"review-manual-rotated","expires_in":3600}`, "test-account", "fixture-user"))
	})
	setRuntimeReviewSettings(t, p, func(s *Settings) {
		s.AutomaticTokenRefreshEnabled, s.Upstream401RecoveryEnabled = false, false
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	w := webTestRequest(p.adminRoutes(), http.MethodPost, "/api/accounts/refresh", `{"name":"test"}`, "Bearer test-client-key")
	stored, err := p.Store.Get("test")
	if w.Code != http.StatusOK || err != nil || refreshes.Load() != 1 || stored.AccessToken != "review-manual-renewed" || stored.RefreshToken != "review-manual-rotated" || stored.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("manual refresh was blocked or not persisted: status=%d refreshes=%d err=%v", w.Code, refreshes.Load(), err)
	}
	if strings.Contains(w.Body.String(), "review-manual-") {
		t.Fatal("manual refresh response leaked credentials")
	}
}

func TestSettingsRuntimeHTTP401RecoverySwitch(t *testing.T) {
	const original = `{"error":{"message":"original authentication failure","code":"token_expired"}}`
	for _, enabled := range []bool{false, true} {
		for _, endpoint := range []struct{ name, method, path, body string }{
			{"stream", http.MethodPost, "/responses", `{"model":"test","stream":true}`},
			{"json", http.MethodPost, "/responses", `{"model":"test","stream":false}`},
			{"compact", http.MethodPost, "/responses/compact", `{"model":"test","input":[]}`},
			{"models", http.MethodGet, "/models", ""},
		} {
			t.Run(fmt.Sprintf("enabled_%v/%s", enabled, endpoint.name), func(t *testing.T) {
				var refreshes, attempts atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if r.URL.Path == "/oauth/token" {
						refreshes.Add(1)
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"review-recovered","refresh_token":"review-recovery-rotated","expires_in":3600}`, "test-account", "fixture-user"))
						return
					}
					attempt := attempts.Add(1)
					if attempt == 1 {
						w.Header().Set("X-Request-Id", "review-original-rejection")
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, original)
						return
					}
					if r.Header.Get("Authorization") != "Bearer review-recovered" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
						t.Error("401 recovery changed account or retained rejected access token")
					}
					if endpoint.name == "models" || endpoint.name == "compact" {
						_, _ = io.WriteString(w, `{"output":[]}`)
					} else {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"review\",\"output\":[]}}\n\n")
					}
				})
				setRuntimeReviewSettings(t, p, func(s *Settings) {
					// The recovery switch is independent of proactive expiry refresh.
					s.AutomaticTokenRefreshEnabled = false
					s.Upstream401RecoveryEnabled = enabled
				})
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(endpoint.method, endpoint.path, endpoint.body))
				wantStatus, wantAttempts, wantRefreshes := http.StatusUnauthorized, int32(1), int32(0)
				if enabled {
					wantStatus, wantAttempts, wantRefreshes = http.StatusOK, 2, 1
				} else if w.Body.String() != original || w.Header().Get("X-Request-Id") != "review-original-rejection" {
					t.Fatal("disabled recovery did not preserve the original upstream error")
				}
				if w.Code != wantStatus || attempts.Load() != wantAttempts || refreshes.Load() != wantRefreshes || p.inFlight("test") != 0 {
					t.Fatalf("recovery switch: status=%d attempts=%d refreshes=%d inflight=%d", w.Code, attempts.Load(), refreshes.Load(), p.inFlight("test"))
				}
			})
		}
	}
}

func TestSettingsRuntimeWebSocket401RecoverySwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, headerSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled_%v/header_session_%v", enabled, headerSession), func(t *testing.T) {
				var refreshes, upgrades, frames atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/oauth/token" {
						refreshes.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"review-ws-renewed","refresh_token":"review-ws-rotated","expires_in":3600}`, "test-account", "fixture-user"))
						return
					}
					if upgrades.Add(1) == 1 {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"error":{"message":"review websocket unauthorized"}}`)
						return
					}
					if r.Header.Get("Authorization") != "Bearer review-ws-renewed" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
						t.Error("WebSocket recovery did not preserve the selected account")
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					if _, _, err = conn.Read(r.Context()); err != nil {
						return
					}
					frames.Add(1)
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"review-ws","output":[]}}`))
					_, _, _ = conn.Read(r.Context())
				})
				setRuntimeReviewSettings(t, p, func(s *Settings) {
					s.WebsocketEnabled = true
					s.AutomaticTokenRefreshEnabled = false
					s.Upstream401RecoveryEnabled = enabled
					// Native WebSocket must remain independent of both HTTP gates.
					s.HTTPStreamEnabled, s.HTTPJSONEnabled = false, false
				})
				server := httptest.NewServer(p)
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				headers := http.Header{"Authorization": {"Bearer test-client-key"}}
				if headerSession {
					headers.Set("Session-Id", "review-ws-session")
				}
				conn, res, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
				if !enabled && headerSession {
					if conn != nil {
						_ = conn.CloseNow()
					}
					if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
						t.Fatalf("disabled recovery did not preserve upgrade rejection: %v", err)
					}
					if res.Body != nil {
						_ = res.Body.Close()
					}
				} else {
					if err != nil {
						t.Fatalf("WebSocket connection: %v", err)
					}
					defer conn.CloseNow()
					if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","prompt_cache_key":"review-ws-session","input":[]}`)); err != nil {
						t.Fatal(err)
					}
					_, data, err := conn.Read(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var event struct {
						Type   string `json:"type"`
						Status int    `json:"status"`
					}
					if err := json.Unmarshal(data, &event); err != nil {
						t.Fatal(err)
					}
					if enabled && event.Type != "response.completed" || !enabled && (event.Type != "error" || event.Status != http.StatusUnauthorized) {
						t.Fatalf("incorrect recovery event: %s", data)
					}
					_ = conn.CloseNow()
				}
				wantUpgrades, wantRefreshes, wantFrames := int32(1), int32(0), int32(0)
				if enabled {
					wantUpgrades, wantRefreshes, wantFrames = 2, 1, 1
				}
				if upgrades.Load() != wantUpgrades || refreshes.Load() != wantRefreshes || frames.Load() != wantFrames {
					t.Fatalf("WebSocket recovery: upgrades=%d refreshes=%d frames=%d", upgrades.Load(), refreshes.Load(), frames.Load())
				}
				waitGatewayWSReleased(t, p)
			})
		}
	}
}
