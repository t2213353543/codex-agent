package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

// Synthetic Codex upstream reproduces the three observed compatibility errors.
// These tests never load production configuration or contact a real account.
func compatibilityRejection(raw []byte) string {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	for _, key := range []string{"max_output_tokens", "temperature"} {
		if _, exists := body[key]; exists {
			return "Unsupported parameter: " + key
		}
	}
	var input []struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	_ = json.Unmarshal(body["input"], &input)
	for _, item := range input {
		if (item.Type == "" || item.Type == "message") && item.Role == "system" {
			return "System messages are not allowed"
		}
	}
	return ""
}

func TestGatewayCompatibilityHTTPObservedErrors(t *testing.T) {
	for _, mode := range []struct{ name, path, stream string }{
		{"stream", "/v1/responses", "true"},
		{"json", "/v1/responses", "false"},
		{"compact", "/v1/responses/compact", "false"},
	} {
		for _, incompatible := range []struct{ name, fields string }{
			{"max_output_tokens", `"max_output_tokens":1024,"input":[]`},
			{"temperature", `"temperature":0.7,"input":[]`},
			{"system_role", `"input":[{"role":"system","content":"client instructions"},{"role":"user","content":"hello"}]`},
		} {
			t.Run(mode.name+"/"+incompatible.name, func(t *testing.T) {
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if message := compatibilityRejection(raw); message != "" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"detail": message})
						return
					}
					if r.URL.Path == "/responses/compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"object":"response.compaction","output":[]}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compatible\",\"status\":\"completed\",\"output\":[]}}\n\n")
				})
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(http.MethodPost, mode.path, `{"model":"test","stream":`+mode.stream+`,`+incompatible.fields+`}`))
				if w.Code != http.StatusOK {
					t.Fatalf("compatible request rejected: HTTP %d: %s", w.Code, w.Body.String())
				}
				if calls.Load() != 1 {
					t.Fatalf("request unexpectedly retried %d times", calls.Load())
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.Status != "ok" || rec.HTTPCode != 200 {
					t.Fatalf("successful request not recorded correctly: %+v", rec)
				}
			})
		}
	}
}

const compatibilityRequest = `{
  "model":"gpt-5.6",
  "instructions":"keep client instructions",
  "max_output_tokens":1024,"temperature":0.7,
  "input":[
    {"id":"m1","role":"system","content":"first system instruction","unknown":1234567890123456789},
    {"type":"reasoning","id":"reasoning1","encrypted_content":"opaque-encrypted-history","summary":[]},
    {"type":"function_call","call_id":"call1","name":"tool1","arguments":"{}"},
    {"type":"function_call_output","call_id":"call1","output":"{\"role\":\"system\",\"temperature\":0.1}"},
    {"type":"message","id":"m2","role":"system","content":[{"type":"input_text","text":"second system instruction"}]},
    {"type":"message","role":"developer","content":"existing developer instruction"},
    {"type":"message","role":"user","content":"hello"}
  ],
  "tools":[{"type":"function","name":"tool1","parameters":{"properties":{"temperature":{"type":"number"},"max_output_tokens":{"type":"integer"}}}}],
  "previous_response_id":"resp_previous","prompt_cache_key":"explicit-cache",
  "prompt_cache_options":{"mode":"implicit"},"generate":false,"stream_id":"lane",
  "include":["reasoning.encrypted_content"],"store":true,
  "text":{"format":{"type":"text"}},"reasoning":{"effort":"high"},
  "client_metadata":{"role":"system","temperature":0.8},"unknown":{"max_output_tokens":42}
}`

// The expected request explicitly preserves messages, opaque data, tool schemas,
// model identity and continuation fields, rather than merely checking deletion.
const compatibilityExpected = `{
  "model":"gpt-5.6",
  "instructions":"keep client instructions",
  "input":[
    {"id":"m1","role":"developer","content":"first system instruction","unknown":1234567890123456789},
    {"type":"reasoning","id":"reasoning1","encrypted_content":"opaque-encrypted-history","summary":[]},
    {"type":"function_call","call_id":"call1","name":"tool1","arguments":"{}"},
    {"type":"function_call_output","call_id":"call1","output":"{\"role\":\"system\",\"temperature\":0.1}"},
    {"type":"message","id":"m2","role":"developer","content":[{"type":"input_text","text":"second system instruction"}]},
    {"type":"message","role":"developer","content":"existing developer instruction"},
    {"type":"message","role":"user","content":"hello"}
  ],
  "tools":[{"type":"function","name":"tool1","parameters":{"properties":{"temperature":{"type":"number"},"max_output_tokens":{"type":"integer"}}}}],
  "previous_response_id":"resp_previous","prompt_cache_key":"explicit-cache",
  "prompt_cache_options":{"mode":"implicit"},"generate":false,"stream_id":"lane",
  "include":["reasoning.encrypted_content"],"store":true,
  "text":{"format":{"type":"text"}},"reasoning":{"effort":"high"},
  "client_metadata":{"role":"system","temperature":0.8},"unknown":{"max_output_tokens":42}
}`

func TestGatewayCompatibilityPreservesConversation(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("User-Agent", "codex_cli_rs/0.155.0 (Linux 6.8; x86_64) unknown")
	r.Header.Set("Originator", "codex_cli_rs")
	prepared, session, err := applyGatewayPolicy(r, []byte(compatibilityRequest), "key", settings)
	if err != nil || session != "explicit-cache" {
		t.Fatalf("policy: session=%q err=%v", session, err)
	}
	assertJSONEqual(t, prepared, []byte(compatibilityExpected))
	if !strings.Contains(string(prepared), "1234567890123456789") {
		t.Fatal("unknown numeric field lost precision")
	}
	twice, sessionAgain, err := applyGatewayPolicy(r, prepared, "key", settings)
	if err != nil || string(twice) != string(prepared) || sessionAgain != session {
		t.Fatal("compatibility normalization is not idempotent")
	}
}

func TestGatewayCompatibilityLeavesUnrelatedFieldsUntouched(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for _, raw := range []string{
		`{ "model":"test", "input":"system instructions as plain user text" }`,
		`{"model":"test","input":null}`,
		`{"model":"test","input":{"role":"system"}}`,
		`{"model":"test","input":[null,42,"system",{"role":null},{"role":12},{"role":"SYSTEM"},{"type":"future_item","role":"system","encrypted_content":"opaque"},{"type":null,"role":"system"}]}`,
		`{"model":"test","input":[{"role":"developer","content":"keep"}],"top_p":0.3,"max_completion_tokens":100,"context_management":[{"type":"compaction"}]}`,
	} {
		prepared, _, err := applyGatewayPolicy(r, []byte(raw), "key", settings)
		if err != nil || string(prepared) != raw {
			t.Errorf("unrelated request changed: %s; err=%v", prepared, err)
		}
	}
}

func TestGatewayCompatibilityWebSocketObservedErrors(t *testing.T) {
	captured := make(chan []byte, 1)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
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
		response := []byte(`{"type":"response.completed","response":{"id":"resp_ws_compatible","status":"completed","output":[]}}`)
		if message := compatibilityRejection(raw); message != "" {
			response, _ = json.Marshal(map[string]any{"type": "error", "error": map[string]string{"message": message}})
		}
		_ = conn.Write(r.Context(), websocket.MessageText, response)
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "compatibility-session")
	payload := strings.Replace(compatibilityRequest, "{", `{"type":"response.create",`, 1)
	if err := gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	response := gatewayWSRead(t, conn)
	if !strings.Contains(string(response), `"response.completed"`) {
		t.Fatalf("compatible WebSocket request rejected: %s", response)
	}
	want := strings.Replace(compatibilityExpected, "{", `{"type":"response.create",`, 1)
	want = strings.Replace(want, `"prompt_cache_key":"explicit-cache"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "explicit-cache")+`"`, 1)
	want = strings.Replace(want, `"prompt_cache_options":{"mode":"implicit"},`, "", 1)
	assertJSONEqual(t, <-captured, []byte(want))
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
}

func TestGatewayCompatibilityPreservesUpstreamRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"unsupported_model", 400, `{"detail":"The 'gpt-5.6' model is not supported when using Codex with a ChatGPT account."}`},
		{"invalid_encrypted_content", 400, `{"detail":"Encrypted content could not be decrypted or parsed."}`},
		{"upstream_refusal", 200, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"This content was flagged for possible cybersecurity risk.\"}}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if policyString(t, raw, "model") != "gpt-5.6" || !strings.Contains(string(raw), "opaque-encrypted-history") {
					t.Error("model or encrypted conversation was changed")
				}
				if tc.code == 200 {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})
			w := httptest.NewRecorder()
			payload := strings.Replace(compatibilityRequest, "{", `{"stream":true,`, 1)
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", payload))
			if w.Code != tc.code || w.Body.String() != tc.body || calls.Load() != 1 {
				t.Fatalf("upstream rejection changed/retried: HTTP=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != "error" || rec.HTTPCode != tc.code || rec.Error == "" {
				t.Fatalf("upstream rejection not recorded correctly: %+v", rec)
			}
		})
	}
}
