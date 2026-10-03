package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

type researchRoutingCase struct{ name, controls, hint string }

func researchRoutingCases() []researchRoutingCase {
	return []researchRoutingCase{
		{"canonical", `"model":"gpt-5.6-sol","service_tier":"priority"`, "model=gpt-5.6-sol;tier=priority"},
		{"unknown_model_string", `"model":"gpt-5.6-sol","Model":"shadow-model"`, "model=gpt-5.6-sol"},
		{"unknown_model_object", `"model":"gpt-5.6-sol","Model":{"opaque":true}`, "model=gpt-5.6-sol"},
		{"unknown_tier_string", `"model":"gpt-5.6-sol","Service_Tier":"priority"`, "model=gpt-5.6-sol"},
		{"unknown_tier_object", `"model":"gpt-5.6-sol","Service_Tier":{"opaque":true}`, "model=gpt-5.6-sol"},
		{"canonical_tier_shadow", `"model":"gpt-5.6-sol","service_tier":"priority","Service_Tier":"flex"`, "model=gpt-5.6-sol;tier=priority"},
		{"canonical_tier_bad_shadow", `"model":"gpt-5.6-sol","service_tier":"priority","Service_Tier":123`, "model=gpt-5.6-sol;tier=priority"},
		{"missing_tier", `"model":"gpt-5.6-sol"`, "model=gpt-5.6-sol"},
		{"null_tier", `"model":"gpt-5.6-sol","service_tier":null,"Service_Tier":"flex"`, "model=gpt-5.6-sol"},
		{"empty_tier", `"model":"gpt-5.6-sol","service_tier":"","Service_Tier":"priority"`, ""},
		{"bad_tier", `"model":"gpt-5.6-sol","service_tier":123,"Service_Tier":"priority"`, ""},
	}
}

func TestResearchRoutingControlsHTTPUsesOnlyExactBodyMembers(t *testing.T) {
	for _, scenario := range researchRoutingCases() {
		t.Run(scenario.name, func(t *testing.T) {
			raw := `{` + scenario.controls + `,"input":[],"stream":true,"store":false,"reasoning":{"effort":"medium"},"unknown":{"value":9007199254740993}}`
			var calls atomic.Int32
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)
				}
				q01AssertQualityFields(t, body, []byte(raw))
				if got := request.Header.Get(gatewayRoutingHintHeader); got != scenario.hint {
					t.Errorf("actual body retains canonical controls but hint=%q, want=%q", got, scenario.hint)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"routing\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", raw))
			if w.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestResearchRoutingControlsWSLazyHintMatchesFirstExactFrame(t *testing.T) {
	for _, scenario := range researchRoutingCases() {
		t.Run(scenario.name, func(t *testing.T) {
			first := `{"type":"response.create",` + scenario.controls + `,"input":[],"generate":false,"prompt_cache_key":"routing-research"}`
			const second = `{"type":"response.create","model":"second-model","input":[],"previous_response_id":"routing_0","prompt_cache_key":"routing-research"}`
			var connections atomic.Int32
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, request *http.Request) {
				connections.Add(1)
				if got := request.Header.Get(gatewayRoutingHintHeader); got != scenario.hint {
					t.Errorf("first-frame exact model/tier diverged from hint=%q, want=%q", got, scenario.hint)
				}
				conn, err := websocket.Accept(w, request, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for index, expected := range []string{first, second} {
					_, actual, err := conn.Read(request.Context())
					if err != nil {
						return
					}
					if string(actual) != expected {
						t.Error("routing hint construction changed raw frame")
					}
					response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"routing_%d","status":"completed","output":[]}}`, index)
					if err := conn.Write(request.Context(), websocket.MessageText, []byte(response)); err != nil {
						return
					}
				}
				_, _, _ = conn.Read(request.Context())
			})
			enableGatewayWS(t, p, "")
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.PromptCacheKeyEnabled, settings.CompatibilityNormalizationEnabled = false, false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			client, _ := gatewayWSDial(t, server.URL, "")
			defer client.CloseNow()
			for _, body := range []string{first, second} {
				if err := client.Write(t.Context(), websocket.MessageText, []byte(body)); err != nil {
					t.Fatal(err)
				}
				if got := gatewayWSRead(t, client); !strings.Contains(string(got), `"type":"response.completed"`) {
					t.Fatalf("unexpected WS result: %s", got)
				}
			}
			if connections.Load() != 1 {
				t.Fatal("model change regenerated an established handshake")
			}
		})
	}
}

func TestResearchRoutingControlsMissingModelCannotUseUnknownAlias(t *testing.T) {
	for _, controls := range []string{`"Model":"shadow"`, `"model":null,"Model":"shadow"`, `"model":"","Model":"shadow"`} {
		raw := `{` + controls + `,"input":[]}`
		var calls atomic.Int32
		p := q01RoutingHintProxy(t, func(w http.ResponseWriter, request *http.Request) { calls.Add(1); w.WriteHeader(200) })
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", raw))
		if w.Code != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("invalid exact model: status=%d calls=%d", w.Code, calls.Load())
		}
		header := make(http.Header)
		outcome := applyGatewayRoutingHint(header, codexURL, "/responses", Account{AccessToken: "synthetic", AccountID: "synthetic"}, []byte(raw))
		if outcome != "unrepresentable_controls" || header.Get(gatewayRoutingHintHeader) != "" {
			t.Errorf("hint read unknown Model as valid: outcome=%s hint=%q", outcome, header.Get(gatewayRoutingHintHeader))
		}
	}
}

func TestResearchRoutingControlsExplicitHintsAndBodyRemainUnchanged(t *testing.T) {
	for _, explicit := range []string{"model=gpt-5.6-sol;future=retained", "model=other;tier=flex;future=retained"} {
		const raw = `{"model":"gpt-5.6-sol","Model":{"opaque":true},"Service_Tier":"priority","input":[]}`
		p := q01RoutingHintProxy(t, func(w http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
			}
			q01AssertQualityFields(t, body, []byte(raw))
			if request.Header.Get(gatewayRoutingHintHeader) != explicit {
				t.Error("explicit hint changed")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"explicit\",\"status\":\"completed\",\"output\":[]}}\n\n")
		})
		request := proxyRequest(http.MethodPost, "/responses", raw)
		request.Header.Set(gatewayRoutingHintHeader, explicit)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("explicit hint request status=%d", w.Code)
		}
	}
}

func TestResearchRoutingControlsWSExplicitHintRemainsCallerOwned(t *testing.T) {
	for _, early := range []bool{false, true} {
		for _, explicit := range []string{"model=gpt-5.6-sol;future=retained", "model=other;tier=flex;future=retained"} {
			t.Run(fmt.Sprintf("early_%t/%s", early, explicit), func(t *testing.T) {
				const raw = `{"type":"response.create","model":"gpt-5.6-sol","Model":{"opaque":true},"Service_Tier":"priority","input":[],"prompt_cache_key":"routing-explicit"}`
				p := q01RoutingHintProxy(t, func(w http.ResponseWriter, request *http.Request) {
					if got := request.Header.Get(gatewayRoutingHintHeader); got != explicit {
						t.Errorf("explicit WS hint=%q, want=%q", got, explicit)
					}
					conn, err := websocket.Accept(w, request, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					_, body, err := conn.Read(request.Context())
					if err != nil {
						return
					}
					expected := strings.Replace(raw, `"prompt_cache_key":"routing-explicit"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "routing-explicit")+`"`, 1)
					q01AssertQualityFields(t, body, []byte(expected))
					_ = conn.Write(request.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"explicit","status":"completed","output":[]}}`))
					_, _, _ = conn.Read(request.Context())
				})
				enableGatewayWS(t, p, "")
				server := httptest.NewServer(p)
				defer server.Close()
				headers := http.Header{"Authorization": {"Bearer test-client-key"}, gatewayRoutingHintHeader: {explicit}}
				if early {
					headers.Set("Session-Id", "routing-explicit")
				}
				client, _, err := websocket.Dial(t.Context(), server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseNow()
				if err := client.Write(t.Context(), websocket.MessageText, []byte(raw)); err != nil {
					t.Fatal(err)
				}
				if got := gatewayWSRead(t, client); !strings.Contains(string(got), `"type":"response.completed"`) {
					t.Fatalf("explicit WS rejected: %s", got)
				}
			})
		}
	}
}
