package server

import (
	"context"
	"encoding/json"
	"errors"
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

const bpsRouteRateCode = "basispoints_route_rate_limit_exceeded"

func TestBPSRouteRateLimitExactBoundaries(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}`,
		`{"type":"error","error":{"code":"basispoints_route_rate_limit_exceeded"}}`,
		`{"type":"response.failed","response":{"id":"route","status":"failed","error":{"code":"basispoints_route_rate_limit_exceeded"}}}`,
	} {
		_, _, _, limited := gatewayEventRateLimit([]byte(raw))
		state := &officialFailoverState{}
		observeAppServerCLIFailoverEvent(context.WithValue(t.Context(), officialFailoverKey{}, state), []byte(raw))
		var protocol *officialProtocolError
		if !limited || !errors.As(state.failure, &protocol) || protocol.status != 429 || protocol.code != "rateLimitExceeded" || protocol.source != "upstream" {
			t.Fatalf("route rejection lacked canonical 429: %v", state.failure)
		}
		var client map[string]json.RawMessage
		if json.Unmarshal(bpsWebSocketFailurePayload([]byte(raw)), &client) != nil || string(client["status"]) != "429" || !strings.Contains(string(bpsWebSocketFailurePayload([]byte(raw))), bpsRouteRateCode) {
			t.Fatal("client WS projection lost explicit code or status")
		}
	}
	for _, raw := range []string{
		`{"type":"error","code":"basispoints_route_rate_limit_exceeded_typo"}`,
		`{"type":"error","error":{"message":"basispoints_route_rate_limit_exceeded"}}`,
		`{"type":"response.completed","error":{"code":"basispoints_route_rate_limit_exceeded"}}`,
	} {
		_, _, _, limited := gatewayEventRateLimit([]byte(raw))
		state := &officialFailoverState{}
		observeAppServerCLIFailoverEvent(context.WithValue(t.Context(), officialFailoverKey{}, state), []byte(raw))
		if limited || state.failure != nil || string(bpsWebSocketFailurePayload([]byte(raw))) != raw {
			t.Fatal("message, near-match or success created a quota failure")
		}
	}
	for _, raw := range []string{
		`{"type":"error","status":503,"code":"basispoints_route_rate_limit_exceeded"}`,
		`{"type":"error","status_code":401,"code":"basispoints_route_rate_limit_exceeded"}`,
		`{"type":"error","status_code":503,"error":{"code":"basispoints_route_rate_limit_exceeded"}}`,
		`{"type":"response.failed","status_code":503,"response":{"error":{"code":"basispoints_route_rate_limit_exceeded"}}}`,
	} {
		if string(bpsWebSocketFailurePayload([]byte(raw))) != raw {
			t.Fatal("route projection overwrote an explicit provider status")
		}
	}
}

func TestBPSRouteRateLimitClientTransportMatrix(t *testing.T) {
	for _, upstreamWS := range []bool{false, true} {
		for _, client := range []string{"json", "sse", "websocket"} {
			for _, envelope := range []string{"flat", "nested", "failed"} {
				t.Run(fmt.Sprintf("upstream_ws_%t/%s/%s", upstreamWS, client, envelope), func(t *testing.T) {
					var frames, posts, upgrades atomic.Int32
					raw := `{"type":"error","code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}`
					if envelope == "nested" {
						raw = `{"type":"error","error":{"code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}}`
					} else if envelope == "failed" {
						raw = `{"type":"response.failed","response":{"id":"route-limited","status":"failed","error":{"code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}}}`
					}
					p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPost {
							posts.Add(1)
							_, _ = io.Copy(io.Discard, r.Body)
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
							return
						}
						upgrades.Add(1)
						conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.CloseNow()
						_, request, err := conn.Read(r.Context())
						if err != nil {
							t.Error(err)
							return
						}
						if !strings.Contains(string(request), `"type":"response.create"`) {
							t.Error("missing generation frame")
							return
						}
						frames.Add(1)
						if err := conn.Write(r.Context(), websocket.MessageText, []byte(raw)); err != nil {
							t.Error(err)
							return
						}
						if _, extra, err := conn.Read(r.Context()); err == nil {
							t.Errorf("terminal caused another frame: %s", extra)
						}
					})
					settings, _ := p.Settings.Get()
					settings.PreferWebsocket, settings.RateLimitCooldownEnabled = upstreamWS, false
					if err := p.Settings.Set(settings); err != nil {
						t.Fatal(err)
					}
					before, _ := p.Store.Get("test")
					var result []byte
					wantHTTP := 200
					if client == "websocket" {
						server := httptest.NewServer(p)
						defer server.Close()
						conn, _ := gatewayWSDial(t, server.URL, "route-client")
						defer conn.CloseNow()
						if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":"fixture"}]}`)); err != nil {
							t.Fatal(err)
						}
						result = gatewayWSRead(t, conn)
						var event struct{ Status int }
						if json.Unmarshal(result, &event) != nil || event.Status != 429 {
							t.Fatalf("client WS lost 429: %s", result)
						}
					} else {
						out := teamCRateRequest(t, p, client == "sse", "route-client")
						result = out.Body.Bytes()
						if client == "json" {
							wantHTTP = 429
						}
						if out.Code != wantHTTP {
							t.Fatalf("public status=%d want=%d body=%s", out.Code, wantHTTP, result)
						}
					}
					if !strings.Contains(string(result), bpsRouteRateCode) {
						t.Fatal("original upstream error code lost")
					}
					var rec RequestRecord
					var ok bool
					deadline := time.Now().Add(2 * time.Second)
					for {
						rec, ok = p.Metrics.LastFor("test")
						if ok || time.Now().After(deadline) {
							break
						}
						time.Sleep(time.Millisecond)
					}
					if !ok || rec.Status != "error" || rec.FinalUpstreamErrorCode != "rateLimitExceeded" || rec.FinalUpstreamErrorSource != "upstream" || len(rec.Attempts) != 1 {
						t.Fatalf("final quota classification missing: %+v", rec)
					}
					if client != "websocket" && rec.HTTPCode != wantHTTP {
						t.Fatalf("persisted public status=%d want=%d", rec.HTTPCode, wantHTTP)
					}
					attempt := rec.Attempts[0]
					wantObserved := 200
					if upstreamWS {
						wantObserved = 101
					}
					if attempt.StatusCode == nil || *attempt.StatusCode != 429 || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != wantObserved || attempt.ErrorCode != "rateLimitExceeded" {
						t.Fatalf("logical vs actual transport mismatch: %+v", attempt)
					}
					if upstreamWS && (frames.Load() != 1 || posts.Load() != 0 || upgrades.Load() != 1) || !upstreamWS && (posts.Load() != 1 || frames.Load() != 0 || upgrades.Load() != 0) {
						t.Fatalf("generation replay: WS=%d POST=%d upgrade=%d", frames.Load(), posts.Load(), upgrades.Load())
					}
					after, _ := p.Store.Get("test")
					if before.Enabled != after.Enabled || before.CredentialStatus != after.CredentialStatus {
						t.Fatal("route quota mutated durable account eligibility")
					}
				})
			}
		}
	}
}

func TestBPSRouteRateLimitCooldownStaysOptionalAndModelScoped(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := cooldownNewTestProxy(t, 1)
		settings, _ := p.Settings.Get()
		settings.RateLimitCooldownEnabled = enabled
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
		account, _ := p.Store.Get("test")
		ctx := withGatewayCooldownModel(t.Context(), "limited")
		response := &http.Response{StatusCode: 200, Request: httptest.NewRequest(http.MethodPost, "/responses", nil)}
		p.observeGatewayCooldownResponse(ctx, account, response)
		p.observeGatewayCooldownEvent(response, []byte(`{"type":"error","code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}`), &gatewayHTTPCooldownIdentity{})
		var limited *gatewayCooldownError
		if got := p.checkGatewayCooldown(ctx, account); errors.As(got, &limited) != enabled {
			t.Fatalf("optional cooldown changed: enabled=%t err=%v", enabled, got)
		}
		if enabled && time.Until(limited.until) > gatewayCooldownDefault+time.Second {
			t.Fatal("route quota installed long account exclusion")
		}
		if err := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "unrelated"), account); err != nil {
			t.Fatal("route quota cooled unrelated model")
		}
	}
}

func TestBPSRouteRateLimitWebSocketKeepsGenerationReplayGuards(t *testing.T) {
	for _, mode := range []string{"opaque", "visible_output"} {
		t.Run(mode, func(t *testing.T) {
			var frames, posts atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(500)
					return
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"responses"}, InsecureSkipVerify: true})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err = conn.Read(r.Context()); err != nil {
					t.Error(err)
					return
				}
				frames.Add(1)
				if mode == "visible_output" {
					if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"VISIBLE"}`)); err != nil {
						t.Error(err)
						return
					}
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}`))
				_, _, _ = conn.Read(r.Context())
			})
			settings, _ := p.Settings.Get()
			settings.PreferWebsocket = true
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.ImportWith("replacement", []byte(`{"access_token":"route-replacement-token"}`), AccountDefaults{Concurrency: 10}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			input := `[{"role":"user","content":"fixture"}]`
			if mode == "opaque" {
				input = `[{"type":"reasoning","encrypted_content":"route-opaque"},{"role":"user","content":"fixture"}]`
			}
			req := proxyRequest(http.MethodPost, "/responses", `{"model":"gpt-6-astra","stream":true,"input":`+input+`}`)
			out := httptest.NewRecorder()
			p.gatewayBPS(out, req)
			if frames.Load() != 1 || posts.Load() != 0 {
				t.Fatalf("non-replayable WS generation replayed: WS=%d HTTP=%d", frames.Load(), posts.Load())
			}
			if !strings.Contains(out.Body.String(), bpsRouteRateCode) || mode == "visible_output" && !strings.Contains(out.Body.String(), "VISIBLE") {
				t.Fatal("original output or terminal was lost")
			}
		})
	}
}
