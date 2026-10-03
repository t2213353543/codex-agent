package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPHopViewPreservesOpaqueMetadataAndSource(t *testing.T) {
	for _, h := range []http.Header{nil, {}, {
		"connection":  {"\tRetry-After, x-codex-private-hop \t"},
		"Connection":  {"X-Request-Id"},
		"RETRY-AFTER": {"0"}, "Retry-After": {"3600"},
		"X-Request-Id": {"hop-id"}, "x-codex-private-hop": {"hop-state"},
		"Proxy-Connection": {"keep-alive"}, "Keep-Alive": {"timeout=20"},
		"Proxy-Authenticate": {"private"}, "Proxy-Authorization": {"private"},
		"Te": {"trailers"}, "Trailer": {"X-Trailer"}, "Transfer-Encoding": {"chunked"}, "Upgrade": {"websocket"},
		"X-Codex-Turn-State": {"opaque\tstate==", "second"},
		"X-Models-Etag":      {`"catalog-one"`}, "X-Codex-Future-List": {"one", "two"},
		"X-Unknown-End-To-End": {"must-not-use-an-allowlist"},
	}} {
		t.Run(fmt.Sprintf("fields=%d", len(h)), func(t *testing.T) {
			original := h.Clone()
			body := io.NopCloser(strings.NewReader("body"))
			request, _ := http.NewRequest(http.MethodGet, "http://localhost/", nil)
			response := &http.Response{Header: h, Body: body, StatusCode: 429, ContentLength: 4, Request: request, ProtoMajor: 2}
			view := gatewayHTTPResponseView(response)
			if view == response || view.Body != body || view.Request != request || view.StatusCode != 429 || view.ContentLength != 4 || view.ProtoMajor != 2 {
				t.Fatal("view changed response framing, identity, or body")
			}
			for key := range view.Header {
				if strings.EqualFold(key, "retry-after") || strings.EqualFold(key, "x-request-id") || strings.EqualFold(key, "x-codex-private-hop") || !strings.HasPrefix(key, "X-") {
					t.Fatalf("hop-only header retained: %s", key)
				}
			}
			for _, key := range []string{"X-Codex-Turn-State", "X-Models-Etag", "X-Codex-Future-List", "X-Unknown-End-To-End"} {
				if !reflect.DeepEqual(view.Header[key], original[key]) {
					t.Fatalf("opaque header values/order changed: %s", key)
				}
			}
			if values := view.Header["X-Codex-Turn-State"]; len(values) != 0 {
				values[0] = "changed-view"
			}
			if !reflect.DeepEqual(h, original) {
				t.Fatal("view mutated source header map or slices")
			}
		})
	}
	if gatewayHTTPResponseView(nil) != nil {
		t.Fatal("nil response changed")
	}
}

func TestHTTPHopAuthRetryUsesOnlyActualFinalAttemptMetadata(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, nominated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nominated=%v", transport, nominated), func(t *testing.T) {
				var requests, grants atomic.Int32
				var firstBody string
				const requestBody = `{"model":"same-model","input":[],"stream":true}`
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path == "/oauth/token" {
						grants.Add(1)
						w.Header().Set("X-Codex-Turn-State", "grant-only-state")
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"recovered-access","refresh_token":"recovered-refresh"}`, "test-account", "fixture-user"))
						return
					}
					attempt := requests.Add(1)
					if attempt == 1 {
						firstBody = string(body)
					}
					if string(body) != firstBody || r.Header.Get("X-Codex-Turn-State") != "client-state" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
						t.Error("retry altered original body, opaque caller state, or account")
					}
					if attempt == 1 {
						if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
							t.Error("wrong original bearer")
						}
						w.Header().Set("X-Codex-Turn-State", "rejected-state")
						w.Header().Set("X-Request-Id", "rejected-request")
						w.Header().Set("Openai-Model", "rejected-model")
						w.WriteHeader(401)
						_, _ = io.WriteString(w, `{"error":{"message":"synthetic old bearer"}}`)
						return
					}
					if attempt != 2 || r.Header.Get("Authorization") != "Bearer recovered-access" {
						t.Error("unexpected retry/bearer")
					}
					if nominated {
						w.Header().Add("Connection", "X-Request-Id, Openai-Model, Retry-After")
					}
					w.Header().Set("Retry-After", "3600")
					w.Header().Set("X-Request-Id", "final-request")
					w.Header().Set("Openai-Model", "final-model")
					w.Header().Set("X-Codex-Turn-State", "final-state")
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"same-owner-response\",\"output\":[]}}\n\n")
				})
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				done := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); p.ServeHTTP(w, r) }))
				defer server.Close()
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/responses", strings.NewReader(requestBody))
				request.Header.Set("Authorization", "Bearer test-client-key")
				request.Header.Set("X-Codex-Turn-State", "client-state")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(response.Body)
				response.Body.Close()
				<-done
				if err != nil || response.StatusCode != 200 || !strings.Contains(string(raw), "same-owner-response") || response.Header.Get("X-Codex-Turn-State") != "final-state" || requests.Load() != 2 || grants.Load() != 1 {
					t.Fatalf("retry lifecycle changed: %v", err)
				}
				record, _ := p.Metrics.LastFor("test")
				wantID, wantModel := "final-request", "final-model"
				if nominated {
					wantID, wantModel = "", ""
				}
				if record.RequestID != wantID || record.UpstreamModel != wantModel || response.Header.Get("X-Request-Id") != wantID {
					t.Fatalf("wrong final-attempt metadata request=%q model=%q", record.RequestID, record.UpstreamModel)
				}
				account, _ := p.Store.Get("test")
				if account.AccessToken != "recovered-access" || record.AccountCredentialVersion != account.CredentialVersion {
					t.Fatal("record not bound to actual retried credentials")
				}
			})
		}
	}
}

// A hop-only Retry-After must neither lengthen nor disable the actual 429's
// ordinary account cooldown. The forwarded header was already filtered; this
// verifies the internal state subsequently exposed by an independent request.
func TestHTTPHopResponseCannotSetAccountCooldownOrMetadata(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, scenario := range []struct {
			name, retry      string
			nominated        bool
			minimum, maximum int
		}{
			{"hop_long_hint", "3600", true, 1, 4},
			{"hop_zero_hint", "0", true, 1, 4},
			{"end_to_end_control", "60", false, 59, 60},
		} {
			t.Run(transport+"/"+scenario.name, func(t *testing.T) {
				var calls atomic.Int32
				p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if scenario.nominated {
						w.Header().Add("Connection", "keep-alive, retry-after, X-Request-Id")
						w.Header().Add("Connection", "OPENAI-MODEL, X-Codex-Private-Hop")
					}
					w.Header().Set("Retry-After", scenario.retry)
					w.Header().Set("X-Request-Id", "hop-local-request")
					w.Header().Set("OpenAI-Model", "hop-local-model")
					w.Header().Set("X-Codex-Private-Hop", "hop-local-state")
					w.Header().Set("X-Codex-Turn-State", "end-to-end-state")
					w.Header().Add("X-Codex-Future-List", "first")
					w.Header().Add("X-Codex-Future-List", "second")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic upstream rejection"}}`)
				})
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				done := make(chan struct{}, 2)
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer func() { done <- struct{}{} }(); p.ServeHTTP(w, r) }))
				defer gateway.Close()
				send := func() *http.Response {
					t.Helper()
					// Each independent request needs its own fixture deadline:
					// the existing default cooldown (4s) plus its jitter (up to
					// 1.5s) already exceeds the former shared 5s deadline. This
					// allowance changes no runtime cooldown or waiting policy.
					const networkMargin = 2500 * time.Millisecond
					ctx, cancel := context.WithTimeout(t.Context(), gatewayCooldownDefault+gatewayCooldownAwaitJitter+networkMargin)
					defer cancel()
					req, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/responses", strings.NewReader(`{"model":"same-model","input":[],"stream":true}`))
					req.Header.Set("Authorization", "Bearer test-client-key")
					req.Header.Set("Session-Id", "hop-bound-session")
					res, err := gateway.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_, err = io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					<-done
					return res
				}
				first := send()
				if first.StatusCode != 429 || first.Header.Get("X-Codex-Turn-State") != "end-to-end-state" || !reflect.DeepEqual(first.Header.Values("X-Codex-Future-List"), []string{"first", "second"}) {
					t.Fatal("ordinary response status/opaque metadata changed")
				}
				record, _ := p.Metrics.LastFor("test")
				if scenario.nominated {
					if first.Header.Get("Retry-After") != "" || first.Header.Get("X-Request-Id") != "" || first.Header.Get("X-Codex-Private-Hop") != "" {
						t.Fatal("hop-only response header reached client")
					}
					if record.RequestID != "" || record.UpstreamModel != "" {
						t.Errorf("hop-only header contaminated durable request metadata: request=%q model=%q", record.RequestID, record.UpstreamModel)
					}
				} else if first.Header.Get("Retry-After") != "60" || record.RequestID != "hop-local-request" {
					t.Fatal("legitimate end-to-end hint lost")
				}
				second := send()
				if scenario.nominated {
					// The bounded default cooldown is now waited out server-side
					// (session D, 2026-09-30): the request replays once the
					// account uncools and surfaces the real upstream 429, while
					// the hop-nominated Retry-After stays stripped from clients
					// and from the cooldown duration.
					if second.StatusCode != 429 || calls.Load() != 2 || second.Header.Get("Retry-After") != "" {
						t.Fatalf("awaited replay lost its shielded 429: status=%d retry=%q upstream_calls=%d", second.StatusCode, second.Header.Get("Retry-After"), calls.Load())
					}
					return
				}
				seconds, err := strconv.Atoi(second.Header.Get("Retry-After"))
				if second.StatusCode != 429 || err != nil || seconds < scenario.minimum || seconds > scenario.maximum || calls.Load() != 1 {
					t.Fatalf("hop hint changed subsequent scheduling: status=%d retry=%q upstream_calls=%d want=%d..%d", second.StatusCode, second.Header.Get("Retry-After"), calls.Load(), scenario.minimum, scenario.maximum)
				}
			})
		}
	}
}

func TestHTTPHopOAuthHintDoesNotOverrideBoundedFailureCooldown(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, nominated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nominated=%v", transport, nominated), func(t *testing.T) {
				var grants atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					grants.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if nominated {
						w.Header().Set("Connection", "retry-after")
					}
					w.Header().Set("Retry-After", "1800")
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
				})
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				account, _ := p.Store.Get("test")
				_, err := refreshAccountWithSettings(t.Context(), p.clientFor(nil), p.TokenURL, account, defaultSettings())
				failure, ok := err.(*gatewayOAuthRefreshError)
				if !ok || grants.Load() != 1 || failure.StatusCode != 503 || failure.Permanent {
					t.Fatalf("synthetic grant boundary changed: %v", err)
				}
				if nominated && (failure.RetryAfterPresent || failure.RetryAfter != 0) {
					t.Fatalf("hop-only OAuth retry hint retained: present=%v duration=%v", failure.RetryAfterPresent, failure.RetryAfter)
				}
				if !nominated && (!failure.RetryAfterPresent || failure.RetryAfter != gatewayRefreshFailureMax) {
					t.Fatal("ordinary OAuth retry-after lost")
				}
			})
		}
	}
}

func TestHTTPHopFilteringDoesNotSuppressAcceptedBodyRateLimit(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Connection", "Retry-After")
				w.Header().Set("Retry-After", "0")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"body-rate\",\"error\":{\"code\":\"rate_limit_exceeded\"}},\"headers\":{\"Retry-After\":30}}\n\n")
			})
			if transport == "native" {
				realGatewayNativePool(t, p)
			}
			first := cooldownRequest(p, "model", "body-bound")
			if first.Code != 200 || first.Header().Get("Retry-After") != "" || !strings.Contains(first.Body.String(), "body-rate") {
				t.Fatal("accepted body was rewritten or hop-only hint forwarded")
			}
			assertCooldownResponse(t, cooldownRequest(p, "model", "body-bound"), 29, 30)
			if calls.Load() != 1 {
				t.Fatal("body rate limit was bypassed or request replayed")
			}
		})
	}
}

func TestHTTPHopViewPreservesActualHTTP2Metadata(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.ProtoMajor != 2 {
			t.Error("fixture did not use actual HTTP/2")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", "opaque-h2-state==")
		w.Header().Add("X-Codex-Future-List", "first")
		w.Header().Add("X-Codex-Future-List", "second")
		w.Header().Set("X-Models-Etag", `"h2-catalog"`)
		w.Header().Set("X-Request-Id", "h2-final")
		w.Header().Set("Openai-Model", "h2-model")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"h2-body\",\"output\":[]}}\n\n")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p := newTestProxy(t, 1)
	p.Client, p.BaseURL = upstream.Client(), upstream.URL
	w := cooldownRequest(p, "h2-model", "h2-session")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "h2-body") || calls.Load() != 1 {
		t.Fatal("HTTP/2 response changed")
	}
	if w.Header().Get("X-Codex-Turn-State") != "opaque-h2-state==" || w.Header().Get("X-Models-Etag") != `"h2-catalog"` || !reflect.DeepEqual(w.Header().Values("X-Codex-Future-List"), []string{"first", "second"}) {
		t.Fatal("HTTP/2 end-to-end opaque values/order changed")
	}
	record, _ := p.Metrics.LastFor("test")
	if record.RequestID != "h2-final" || record.UpstreamModel != "h2-model" {
		t.Fatal("HTTP/2 observation lost end-to-end metadata")
	}
}
