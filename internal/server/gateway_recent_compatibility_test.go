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
	"time"

	"github.com/coder/websocket"
)

const recentCompatibilityRequest = `{"model":"test","stream":true,"background":false,"user":"caller-identity",
"instructions":"keep <content>&","input":[
{"type":"message","role":"assistant","author":{"role":"assistant","name":"client"},"id":"msg_1","content":[{"type":"output_text","text":"unchanged","annotations":[]}],"unknown":1234567890123456789},
{"role":"user","author":"client","content":"question"},
{"type":"function_call_output","call_id":"c","output":"{\"author\":\"tool value\",\"user\":\"keep\"}"},
{"type":"reasoning","encrypted_content":"opaque","summary":[]}],
"tools":[{"type":"function","name":"f","parameters":{"properties":{"user":{"type":"string"},"author":{"type":"object"},"background":{"type":"boolean"}}}}],
"client_metadata":{"user":"keep","author":"keep","background":true},"previous_response_id":"resp_prev","prompt_cache_key":"explicit-cache"}`

func TestRecentCompatibilityIndividualRejections(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"background", `"background":false,"input":[]`},
		{"user", `"user":"client-identity","input":[]`},
		{"author", `"input":[{"role":"user","author":{"role":"user"},"content":"hello"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if message := recentCompatibilityRejection(raw); message != "" {
					http.Error(w, message, 400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"test","stream":true,`+tc.fields+`}`))
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
		})
	}
}

func recentCompatibilityRejection(raw []byte) string {
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	for _, key := range []string{"background", "user"} {
		if _, ok := body[key]; ok {
			return "Unsupported parameter: " + key
		}
	}
	var input []map[string]json.RawMessage
	_ = json.Unmarshal(body["input"], &input)
	for _, item := range input {
		var role string
		_ = json.Unmarshal(item["role"], &role)
		if _, ok := item["author"]; ok && role != "" {
			return "Unknown parameter: 'input[0].author'."
		}
	}
	return ""
}

func assertRecentCompatibilityPayload(t *testing.T, raw []byte) {
	t.Helper()
	var want, got map[string]json.RawMessage
	_ = json.Unmarshal([]byte(recentCompatibilityRequest), &want)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	delete(want, "background")
	delete(want, "user")
	want["prompt_cache_key"], _ = json.Marshal(gatewayFixtureScopedValue("prompt-cache", "explicit-cache"))
	var input []map[string]json.RawMessage
	_ = json.Unmarshal(want["input"], &input)
	delete(input[0], "author")
	delete(input[1], "author")
	want["input"], _ = json.Marshal(input)
	// Transport preparation owns stream/store/type; all prompt/history fields
	// must otherwise survive exactly, including opaque values and large numbers.
	for _, key := range []string{"stream", "store", "type"} {
		delete(want, key)
		delete(got, key)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	assertJSONEqual(t, b, a)
}

func TestRecentCompatibilityObservedHTTPFailures(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, endpoint := range []string{"sse", "json", "compact"} {
			name := endpoint
			if native {
				name += "/native"
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if message := recentCompatibilityRejection(raw); message != "" {
						http.Error(w, message, 400)
						return
					}
					assertRecentCompatibilityPayload(t, raw)
					if endpoint == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"object":"response.compaction","output":[]}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
				})
				if native {
					realGatewayNativePool(t, p)
				}
				settings, _ := p.Settings.Get()
				settings.BuiltinPromptEnabled = false
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
				raw, path := recentCompatibilityRequest, "/v1/responses"
				if endpoint != "sse" {
					raw = strings.Replace(raw, `"stream":true`, `"stream":false`, 1)
				}
				if endpoint == "compact" {
					path += "/compact"
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(http.MethodPost, path, raw))
				if w.Code != 200 || calls.Load() != 1 {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
				}
			})
		}
	}
}

func TestRecentCompatibilityWebSocket(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if message := recentCompatibilityRejection(raw); message != "" {
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","error":{"message":"unsupported compatibility field"}}`))
			return
		}
		assertRecentCompatibilityPayload(t, raw)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[]}}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	settings, _ := p.Settings.Get()
	settings.BuiltinPromptEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "compat-recent")
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw := strings.Replace(recentCompatibilityRequest, `{"model"`, `{"type":"response.create","model"`, 1)
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	_, result, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(result), `"response.completed"`) {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

func TestRecentCompatibilityBackgroundSemanticsAndScope(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for _, value := range []string{"true", `"false"`, "0", "{}"} {
		_, _, err := applyGatewayPolicy(r, []byte(`{"model":"test","background":`+value+`}`), "key", settings)
		if err == nil {
			t.Errorf("background=%s was silently forwarded or downgraded", value)
		}
	}
	for _, value := range []string{"false", "null"} {
		got, _, err := applyGatewayPolicy(r, []byte(`{"model":"test","background":`+value+`}`), "key", settings)
		if err != nil || strings.Contains(string(got), "background") {
			t.Errorf("background=%s got=%s err=%v", value, got, err)
		}
	}
	for _, raw := range []string{
		`{"input":[{"type":"future","role":"user","author":"keep"},{"type":"function_call","author":"keep"},{"author":{"role":"user"},"content":"not a canonical message"}]}`,
		`{"input":[{"type":"message","role":"future_role","author":"keep"}],"tools":[{"background":true,"user":"keep","author":"keep"}]}`,
	} {
		got, _, err := applyGatewayPolicy(r, []byte(raw), "key", settings)
		if err != nil || string(got) != raw {
			t.Fatalf("unrelated data changed: %s err=%v", got, err)
		}
	}
	settings.CompatibilityNormalizationEnabled = false
	got, _, err := applyGatewayPolicy(r, []byte(recentCompatibilityRequest), "key", settings)
	if err != nil || string(got) != recentCompatibilityRequest {
		t.Fatalf("disabled compatibility changed body: %s err=%v", got, err)
	}
}
