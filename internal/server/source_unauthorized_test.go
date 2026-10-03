package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSourceGateway401RecoversOriginalAccount(t *testing.T) {
	const terminalJSON = `{"id":"recovered-response","output":[]}`
	const terminalSSE = "data: {\"type\":\"response.completed\",\"response\":" + terminalJSON + "}\n\n"
	for _, test := range []struct {
		name, method, path, body, result string
	}{
		{"stream", http.MethodPost, "/responses", `{"model":"test","stream":true,"instructions":"native instructions","input":[{"type":"reasoning","encrypted_content":"opaque-history"}]}`, terminalSSE},
		{"aggregate", http.MethodPost, "/responses", `{"model":"test","stream":false,"input":[]}`, terminalJSON},
		{"compact", http.MethodPost, "/responses/compact", `{"model":"test","input":[{"type":"reasoning","encrypted_content":"opaque-history"}]}`, `{"output":[{"type":"compaction","encrypted_content":"opaque-compacted"}]}`},
		{"models", http.MethodGet, "/models?client_version=0.154.0", "", `{"models":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var refreshes, requests atomic.Int32
			var initialBody, initialURL string
			var initialHeaders http.Header
			var captured sync.Mutex
			var p *Proxy
			p = proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if p.inFlight("test") != 1 {
					t.Errorf("account admission changed during auth recovery: %d", p.inFlight("test"))
				}
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"recovered-access","refresh_token":"recovered-refresh"}`, "test-account", "fixture-user"))
					return
				}
				attempt := requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if r.Header.Get("ChatGPT-Account-Id") != "test-account" {
					t.Errorf("recovery switched accounts: %q", r.Header.Get("ChatGPT-Account-Id"))
				}
				captured.Lock()
				defer captured.Unlock()
				if attempt == 1 {
					initialBody, initialURL, initialHeaders = string(body), r.URL.String(), r.Header.Clone()
					if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
						t.Error("first request did not use the original bearer")
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Request-Id", "rejected-request")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"expired upstream bearer"}}`)
					return
				}
				if attempt != 2 || r.Header.Get("Authorization") != "Bearer recovered-access" {
					t.Errorf("unexpected retry attempt=%d authorization=%q", attempt, r.Header.Get("Authorization"))
				}
				if string(body) != initialBody || r.URL.String() != initialURL {
					t.Errorf("recovery changed body or URL: URL=%s first=%s bodyEqual=%v", r.URL, initialURL, string(body) == initialBody)
				}
				for _, name := range []string{"Session-Id", "Thread-Id", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "User-Agent", "Originator", "Accept", "Content-Type"} {
					if r.Header.Get(name) != initialHeaders.Get(name) {
						t.Errorf("recovery changed native request header %s", name)
					}
				}
				w.Header().Set("X-Request-Id", "recovered-request")
				if strings.HasPrefix(test.name, "model") || test.name == "compact" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, test.result)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, terminalSSE)
				}
			})
			r := proxyRequest(test.method, "/v1"+test.path, test.body)
			r.Header.Set("Session-Id", "native-session")
			r.Header.Set("Thread-Id", "native-thread")
			r.Header.Set("X-Codex-Turn-State", "opaque-route")
			r.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn-1"}`)
			r.Header.Set("User-Agent", "codex_exec/0.154.0 (Windows 10; x86_64) unknown")
			r.Header.Set("Originator", "codex_exec")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != test.result || w.Header().Get("X-Request-Id") != "recovered-request" {
				t.Fatalf("recovered result: status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			if requests.Load() != 2 || refreshes.Load() != 1 || p.inFlight("test") != 0 {
				t.Fatalf("recovery lifecycle: attempts=%d refreshes=%d inflight=%d", requests.Load(), refreshes.Load(), p.inFlight("test"))
			}
			stored, err := p.Store.Get("test")
			if err != nil || stored.AccessToken != "recovered-access" || stored.RefreshToken != "recovered-refresh" {
				t.Fatalf("recovered credentials not persisted: %v", err)
			}
		})
	}
}

func TestSourceGateway401RetriesOnlyOnce(t *testing.T) {
	var refreshes, requests atomic.Int32
	const unauthorized = `{"error":{"message":"still unauthorized"}}`
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"still-rejected-access","refresh_token":"rotated-once","expires_in":3600}`, "test-account", "fixture-user"))
			return
		}
		requests.Add(1)
		w.Header().Set("X-Request-Id", "final-rejection")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, unauthorized)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
	if w.Code != http.StatusUnauthorized || w.Body.String() != unauthorized || requests.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("recovery was not bounded: status=%d attempts=%d refreshes=%d body=%s", w.Code, requests.Load(), refreshes.Load(), w.Body.String())
	}
}

func TestSourceGateway401WithoutRefreshKeepsOriginalFailure(t *testing.T) {
	for _, rejectRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "no refresh token", true: "refresh rejected"}[rejectRefresh], func(t *testing.T) {
			var refreshes, requests atomic.Int32
			const original = `{"error":{"message":"original unauthorized failure","code":"token_expired"}}`
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
					return
				}
				requests.Add(1)
				w.Header().Set("X-Request-Id", "original-rejection")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, original)
			})
			if !rejectRefresh {
				if err := p.Store.UpdateAccount("test", func(a *Account) error { a.RefreshToken = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
			wantRefreshes := int32(0)
			if rejectRefresh {
				wantRefreshes = 1
			}
			if w.Code != http.StatusUnauthorized || w.Body.String() != original || w.Header().Get("X-Request-Id") != "original-rejection" || requests.Load() != 1 || refreshes.Load() != wantRefreshes {
				t.Fatalf("original auth failure lost: status=%d attempts=%d refreshes=%d body=%s", w.Code, requests.Load(), refreshes.Load(), w.Body.String())
			}
		})
	}
}

func TestSourceGatewayDoesNotReplayOtherFailures(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var refreshes, requests atomic.Int32
			payload := `{"error":{"message":"preserve original failure"}}`
			if status == http.StatusOK {
				payload = "data: {\"type\":\"error\",\"status\":401,\"error\":{\"code\":\"401\",\"type\":\"authentication_error\",\"message\":\"stream already started\"}}\n\n"
			}
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				requests.Add(1)
				if status == http.StatusOK {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, payload)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
			if w.Code != status || w.Body.String() != payload || requests.Load() != 1 || refreshes.Load() != 0 {
				t.Fatalf("non-401 response was replayed: status=%d attempts=%d refreshes=%d body=%s", w.Code, requests.Load(), refreshes.Load(), w.Body.String())
			}
		})
	}
}
