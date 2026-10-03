package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A synthetic multi-turn request guards the controls that could affect output
// quality. It includes opaque history and numbers outside float64 precision;
// compare canonical JSON with UseNumber rather than rounding through float64.
const q01QualityRequest = `{"model":"quality-synthetic-model","instructions":"Preserve these exact client instructions.","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"Exact context."}],"phase":"commentary"},{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-reasoning"},{"type":"compaction","encrypted_content":"opaque-compaction"},{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}","encrypted_function_args":["opaque-args"]},{"type":"function_call_output","call_id":"call_1","output":"original-result"},{"type":"message","role":"user","content":[{"type":"input_text","text":"Unmodified question."}]}],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"number":{"const":9007199254740993}}}}],"reasoning":{"effort":"xhigh","summary":"detailed","context":"all_turns"},"service_tier":"priority","text":{"verbosity":"high","format":{"type":"json_schema","name":"quality","strict":true,"schema":{"type":"object","properties":{"n":{"const":9007199254740995}}}}},"include":["reasoning.encrypted_content"],"previous_response_id":"resp_existing","prompt_cache_key":"quality-thread","prompt_cache_options":{"mode":"implicit"},"client_metadata":{"opaque":"retain","turn_id":"quality-turn"},"access_programs":["synthetic"],"parallel_tool_calls":false,"tool_choice":"auto","stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},"store":false,"stream":true}`

func q01AssertQualityFields(t *testing.T, got, want []byte, excluded ...string) {
	t.Helper()
	var actual, expected map[string]json.RawMessage
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	for _, key := range excluded {
		delete(expected, key)
	}
	for key, value := range expected {
		expectedValue, err := canonicalGatewayCacheJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		actualValue, err := canonicalGatewayCacheJSON(actual[key])
		if err != nil || string(actualValue) != string(expectedValue) {
			t.Errorf("quality field %s changed: got %s, want %s (error %v)", key, actual[key], value, err)
		}
	}
}

func TestQualityQ01HTTPWirePreservesReasoningAndHistory(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		name := "native_identity"
		if fingerprint {
			name = "profile_override"
		}
		t.Run(name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				captured <- raw
				if got := r.Header.Get("X-Codex-Routing-Hint"); got != "model=quality-synthetic-model;tier=priority" {
					t.Errorf("routing hint changed: %q", got)
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("ingress cookie crossed account replacement")
				}
				if got := r.Header.Get("X-Codex-Turn-State"); got != "opaque-turn-state" {
					t.Errorf("turn state changed: %q", got)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"quality-http\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			settings := defaultSettings()
			settings.BuiltinPromptEnabled = false
			settings.CodexFingerprintEnabled = fingerprint
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			r := proxyRequest(http.MethodPost, "/v1/responses", q01QualityRequest)
			r.Header.Set("User-Agent", "codex_cli_rs/0.155.0 (Linux 6.8; x86_64) unknown")
			r.Header.Set("Originator", "codex_cli_rs")
			r.Header.Set("Session-Id", "quality-thread")
			r.Header.Set("X-Codex-Routing-Hint", "model=quality-synthetic-model;tier=priority")
			r.Header.Set("X-Codex-Turn-State", "opaque-turn-state")
			r.Header.Set("Cookie", "caller-cookie=must-not-cross")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			select {
			case raw := <-captured:
				expected := strings.Replace(q01QualityRequest, `"prompt_cache_key":"quality-thread"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "quality-thread")+`"`, 1)
				q01AssertQualityFields(t, raw, []byte(expected))
			default:
				t.Fatal("request did not reach upstream")
			}
		})
	}
}

func TestQualityQ01WebSocketWirePreservesReasoningAndHistory(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		name := "native_identity"
		if fingerprint {
			name = "profile_override"
		}
		t.Run(name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				captured <- raw
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"quality-ws","status":"completed","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			}))
			defer upstream.Close()
			p := newTestProxy(t, 2)
			p.BaseURL, p.Client = upstream.URL, upstream.Client()
			enableGatewayWS(t, p, "")
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.CodexFingerprintEnabled = fingerprint
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(p)
			defer server.Close()
			headers := http.Header{
				"Authorization": {"Bearer test-client-key"},
				"Session-Id":    {"quality-thread"},
				"User-Agent":    {"codex_cli_rs/0.155.0 (Linux 6.8; x86_64) unknown"},
				"Originator":    {"codex_cli_rs"},
			}
			conn, _, err := websocket.Dial(t.Context(), server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			payload := []byte(strings.Replace(q01QualityRequest, `"stream":true`, `"type":"response.create"`, 1))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
				t.Fatal(err)
			}
			_ = gatewayWSRead(t, conn)
			select {
			case raw := <-captured:
				expected := strings.Replace(string(payload), `"prompt_cache_key":"quality-thread"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "quality-thread")+`"`, 1)
				q01AssertQualityFields(t, raw, []byte(expected))
			default:
				t.Fatal("request did not reach upstream")
			}
		})
	}
}

func TestQualityQ01EffortAndTierAreNeverInvented(t *testing.T) {
	for _, controls := range []string{``, `,"reasoning":null,"service_tier":null`, `,"reasoning":{"effort":"none"},"service_tier":"flex"`, `,"reasoning":{"effort":"low"},"service_tier":"default"`, `,"reasoning":{"effort":"xhigh"},"service_tier":"priority"`} {
		raw := []byte(`{"model":"quality-synthetic-model","instructions":"original","input":[]` + controls + `}`)
		r := httptest.NewRequest(http.MethodPost, "/responses", nil)
		for _, policy := range []Settings{defaultSettings(), {}} {
			prepared, _, err := applyGatewayPolicy(r, raw, "quality-key", policy)
			if err != nil {
				t.Fatal(err)
			}
			prepared, _, err = prepareBody(prepared, false)
			if err != nil {
				t.Fatal(err)
			}
			q01AssertQualityFields(t, prepared, raw, "instructions")
			if controls == "" && (strings.Contains(string(prepared), `"reasoning"`) || strings.Contains(string(prepared), `"service_tier"`)) {
				t.Fatalf("missing controls were invented: %s", prepared)
			}
		}
	}
}
