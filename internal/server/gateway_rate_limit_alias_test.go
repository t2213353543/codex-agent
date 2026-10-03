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

func TestGatewayRateLimitCamelAliasesStructuredEvidence(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		for _, raw := range []string{
			fmt.Sprintf(`{"type":"error","code":%q}`, code),
			fmt.Sprintf(`{"type":"error","error":{"code":%q}}`, code),
			fmt.Sprintf(`{"type":"response.failed","response":{"id":"alias-failed","status":"failed","error":{"code":%q}}}`, code),
		} {
			_, _, _, limited := gatewayEventRateLimit([]byte(raw))
			if !limited {
				t.Errorf("structured camel quota not classified: %s", raw)
			}
			state := &officialFailoverState{}
			ctx := context.WithValue(t.Context(), officialFailoverKey{}, state)
			observeAppServerCLIFailoverEvent(ctx, []byte(raw))
			var protocol *officialProtocolError
			if !errors.As(state.failure, &protocol) || protocol.status != 429 || protocol.source != "upstream" || protocol.code != code || !officialFailoverEligible(state.failure) {
				t.Errorf("typed final quota missing: code=%s failure=%v", code, state.failure)
			}
		}
	}
	for _, raw := range []string{`{"type":"response.failed","response":{"error":{"code":"rateLimitExceededTypo"}}}`, `{"type":"response.failed","error":{"message":"usageLimitExceeded"}}`, `{"type":"response.completed","error":{"code":"rateLimitExceeded"}}`} {
		_, _, _, limited := gatewayEventRateLimit([]byte(raw))
		state := &officialFailoverState{}
		ctx := context.WithValue(t.Context(), officialFailoverKey{}, state)
		observeAppServerCLIFailoverEvent(ctx, []byte(raw))
		if limited || state.failure != nil {
			t.Fatalf("unknown/message/success created quota retry: %s", raw)
		}
	}
}

func TestBPSRateLimitCamelAliasesHTTPAndJSON(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		for _, stream := range []bool{false, true} {
			for _, cooldown := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream_%t/cooldown_%t", code, stream, cooldown), func(t *testing.T) {
					var calls atomic.Int32
					p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"headers\":{\"Retry-After\":\"30\"},\"response\":{\"id\":\"alias-failed\",\"status\":\"failed\",\"error\":{\"code\":%q}}}\n\n", code)
					})
					settings, _ := p.Settings.Get()
					settings.RateLimitCooldownEnabled = cooldown
					if err := p.Settings.Set(settings); err != nil {
						t.Fatal(err)
					}
					out := teamCRateRequest(t, p, stream, "camel-first")
					wantStatus := 502
					if stream {
						wantStatus = 200
					}
					if out.Code != wantStatus || !strings.Contains(out.Body.String(), code) {
						t.Fatalf("terminal payload/status changed: %d %s", out.Code, out.Body.String())
					}
					rec, ok := p.Metrics.LastFor("test")
					if !ok || rec.FinalUpstreamErrorCode != code || rec.FinalUpstreamErrorSource != "upstream" || rec.Status != "error" {
						t.Fatalf("typed final alias absent: %+v", rec)
					}
					account, _ := p.Store.Get("test")
					check := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), teamCLimitedModel), account)
					var blocked *gatewayCooldownError
					if errors.As(check, &blocked) != cooldown {
						t.Fatalf("cooldown toggle ignored: %v", check)
					}
					_ = teamCRateRequest(t, p, stream, "camel-next")
					wantCalls := int32(2)
					if cooldown {
						wantCalls = 1
					}
					if calls.Load() != wantCalls {
						t.Fatalf("quota redispatch count=%d want=%d", calls.Load(), wantCalls)
					}
				})
			}
		}
	}
}

func TestGatewayRateLimitCamelAliasesRespectResponseIdentity(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		p := cooldownNewTestProxy(t, 1)
		account, _ := p.Store.Get("test")
		ctx := withGatewayCooldownModel(t.Context(), "identity-model")
		response := &http.Response{StatusCode: 200, Request: httptest.NewRequest(http.MethodGet, "/", nil)}
		p.observeGatewayCooldownResponse(ctx, account, response)
		identity := &gatewayHTTPCooldownIdentity{}
		p.observeGatewayCooldownEvent(response, []byte(`{"type":"response.created","response":{"id":"actual-response"}}`), identity)
		p.observeGatewayCooldownEvent(response, []byte(fmt.Sprintf(`{"type":"response.failed","response":{"id":"foreign-response","error":{"code":%q}}}`, code)), identity)
		if err := p.checkGatewayCooldown(ctx, account); err != nil {
			t.Fatal("foreign response installed cooldown:", err)
		}
		p.observeGatewayCooldownEvent(response, []byte(fmt.Sprintf(`{"type":"response.failed","response":{"id":"actual-response","error":{"code":%q}}}`, code)), identity)
		if err := p.checkGatewayCooldown(ctx, account); err == nil {
			t.Fatal("accepted response alias did not install cooldown")
		}
	}
}

func TestBPSRateLimitCamelAliasesKeepReplayGuards(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		for _, mode := range []string{"plaintext", "after_output", "opaque"} {
			t.Run(code+"/"+mode, func(t *testing.T) {
				var original, replacement atomic.Int32
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
						replacement.Add(1)
						_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "alias-replacement", "[]"))
						return
					}
					original.Add(1)
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"alias-original\",\"status\":\"in_progress\"}}\n\n")
					if mode == "after_output" {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"VISIBLE\"}\n\n")
					}
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"alias-original\",\"status\":\"failed\",\"error\":{\"code\":%q}}}\n\n", code)
				})
				if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
					t.Fatal(err)
				}
				if err := p.Store.ImportWith("replacement", []byte(`{"access_token":"alias-replacement-token"}`), AccountDefaults{Concurrency: 10}); err != nil {
					t.Fatal(err)
				}
				p.invalidateAccounts()
				input := `[{"role":"user","content":"synthetic alias replay"}]`
				if mode == "opaque" {
					input = `[{"type":"reasoning","encrypted_content":"synthetic-opaque"},{"role":"user","content":"synthetic alias replay"}]`
				}
				req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":`+input+`}`)
				out := httptest.NewRecorder()
				p.gatewayBPS(out, req)
				wantReplacement := int32(0)
				if mode == "plaintext" {
					wantReplacement = 1
				}
				if original.Load() != 1 || replacement.Load() != wantReplacement {
					t.Fatalf("quota alias bypassed replay guard: original=%d replacement=%d mode=%s", original.Load(), replacement.Load(), mode)
				}
				if mode == "plaintext" && (!strings.Contains(out.Body.String(), "alias-replacement") || strings.Contains(out.Body.String(), "alias-original")) {
					t.Fatal("pre-output failure leaked instead of safe fallback")
				}
				if mode == "after_output" && !strings.Contains(out.Body.String(), "VISIBLE") {
					t.Fatal("already visible output disappeared")
				}
				if mode != "plaintext" && !strings.Contains(out.Body.String(), code) {
					t.Fatal("non-replayable upstream terminal lost")
				}
			})
		}
	}
}

func TestGatewayRateLimitCamelAliasesWebSocket(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		t.Run(code, func(t *testing.T) {
			var calls atomic.Int32
			p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					calls.Add(1)
					var request struct {
						Model  string `json:"model"`
						Stream string `json:"stream_id"`
					}
					if json.Unmarshal(raw, &request) != nil {
						return
					}
					event := fmt.Sprintf(`{"type":"response.failed","stream_id":%q,"headers":{"Retry-After":"30"},"response":{"id":%q,"status":"failed","error":{"code":%q}}}`, request.Stream, request.Stream, code)
					if request.Model == "other" {
						event = fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":%q,"status":"completed"}}`, request.Stream, request.Stream)
					}
					if conn.Write(r.Context(), websocket.MessageText, []byte(event)) != nil {
						return
					}
				}
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "camel-websocket")
			defer conn.CloseNow()
			for i, model := range []string{"limited", "limited", "other"} {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"stream_id":"alias-%d","input":[]}`, model, i)))
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				raw := gatewayWSRead(t, conn)
				if i == 1 {
					var blocked struct {
						Status int `json:"status"`
					}
					if json.Unmarshal(raw, &blocked) != nil || blocked.Status != 429 {
						t.Fatalf("WS camel cooldown not enforced: %s", raw)
					}
				}
				if i == 2 && !strings.Contains(string(raw), "response.completed") {
					t.Fatal("unrelated model cooled")
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("limited WS turn reached upstream: calls=%d", calls.Load())
			}
		})
	}
}
