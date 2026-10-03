package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func policyString(t *testing.T, body []byte, key string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	var value string
	if raw := fields[key]; raw != nil {
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
	}
	return value
}

func TestGatewayPolicyPreservesConversationAndExplicitCache(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("Session-Id", "root-session")
	r.Header.Set("User-Agent", "codex_cli_rs/0.155.0")
	settings := defaultSettings()
	settings.BuiltinPrompt = "Stable instructions."
	raw := []byte(`{"model":"test","instructions":"client instructions","input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call_output","call_id":"c","output":"result"}],"tools":[{"type":"function","name":"z"},{"type":"function","name":"a"}],"previous_response_id":"resp_1","prompt_cache_key":"client-key","prompt_cache_options":{"mode":"implicit"},"generate":false,"stream_id":"lane","client_metadata":{"turn_id":"turn"}}`)
	prepared, session, err := applyGatewayPolicy(r, raw, "key-a", settings)
	if err != nil || session != "root-session" {
		t.Fatalf("policy: %q %v", session, err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(prepared, &after)
	for key, value := range before {
		if key != "instructions" {
			assertJSONEqual(t, after[key], value)
		}
	}
	if got := policyString(t, prepared, "instructions"); got != "Stable instructions.\n\nclient instructions" {
		t.Fatal(got)
	}
	twice, _, err := applyGatewayPolicy(r, prepared, "key-a", settings)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, twice, prepared)
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	untouched, _, err := applyGatewayPolicy(r, raw, "key-a", settings)
	if err != nil || string(untouched) != string(raw) {
		t.Fatal("disabled policy modified request")
	}
}

func TestGatewayPolicyCacheIdentity(t *testing.T) {
	settings := defaultSettings()
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	body := []byte(`{"model":"test","input":"first"}`)
	first, session, err := applyGatewayPolicy(r, body, "key-a", settings)
	if err != nil || session == "" {
		t.Fatalf("content affinity missing: %q %v", session, err)
	}
	second, otherSession, err := applyGatewayPolicy(r, []byte(`{"model":"test","input":"different first user"}`), "key-a", settings)
	if err != nil {
		t.Fatal(err)
	}
	if otherSession == "" || otherSession == session {
		t.Fatal("different first users collapsed into the same local content route")
	}
	other, _, err := applyGatewayPolicy(r, body, "key-b", settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, prepared := range [][]byte{first, second, other} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(prepared, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["prompt_cache_key"]; exists {
			t.Fatal("local content affinity generated an upstream cache key")
		}
	}
	r.Header.Set("Session-Id", "root")
	first, session, err = applyGatewayPolicy(r, body, "key-a", settings)
	if err != nil || session != "root" {
		t.Fatal("session lost", err)
	}
	r.Header.Set("Thread-Id", "child")
	second, otherSession, err = applyGatewayPolicy(r, []byte(`{"model":"other-model","input":[],"instructions":"new instructions"}`), "key-a", settings)
	if err != nil || otherSession != "root" || policyString(t, first, "prompt_cache_key") != "" || policyString(t, second, "prompt_cache_key") != "" {
		t.Fatal("explicit root session was changed or promoted to an upstream cache key", err)
	}
	settings.StickySessionsEnabled = false
	r.Header.Del("Session-Id")
	r.Header.Del("Thread-Id")
	if _, localSession, err := applyGatewayPolicy(r, body, "key-a", settings); err != nil || localSession != "" {
		t.Fatalf("disabled stickiness retained inferred conversation identity: %q %v", localSession, err)
	}
}

func TestGatewayPolicyHTTPModesAndLiveToggle(t *testing.T) {
	var captured [][]byte
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, body)
		if r.URL.Path == "/responses/compact" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"response.compaction","output":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2000,\"input_tokens_details\":{\"cached_tokens\":1800},\"output_tokens\":10}}}\n\n")
	})
	for _, tc := range []struct{ path, body, contentType string }{
		{"/v1/responses", `{"model":"test","input":"one","stream":true}`, "text/event-stream"},
		{"/v1/responses", `{"model":"test","input":"two","stream":false}`, "application/json"},
		{"/v1/responses/compact", `{"model":"test","input":[]}`, "application/json"},
	} {
		w := httptest.NewRecorder()
		r := proxyRequest(http.MethodPost, tc.path, tc.body)
		r.Header.Set("Session-Id", "same-session")
		p.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Content-Type") != tc.contentType {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	for _, body := range captured {
		if policyString(t, body, "instructions") != defaultBuiltinPrompt {
			t.Fatal("default prompt missing")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["prompt_cache_key"]; exists {
			t.Fatal("missing key was generated for HTTP streaming, buffering or compact")
		}
	}
	settings, _ := p.Settings.Get()
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","instructions":"keep","input":[]}`))
	last := captured[len(captured)-1]
	if w.Code != 200 || policyString(t, last, "instructions") != "keep" || policyString(t, last, "prompt_cache_key") != "" {
		t.Fatal("toggle did not take effect immediately")
	}
}

func TestGatewayPolicyRejectsMalformedFields(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	for _, raw := range []string{`[]`, `null`, `{"instructions":{}}`, `{"prompt_cache_key":45}`} {
		if _, _, err := applyGatewayPolicy(r, []byte(raw), "key", defaultSettings()); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	settings := defaultSettings()
	settings.BuiltinPrompt = ""
	prepared, _, err := applyGatewayPolicy(r, []byte(`{"instructions":"keep"}`), "key", settings)
	if err != nil || strings.Contains(string(prepared), defaultBuiltinPrompt) || policyString(t, prepared, "instructions") != "keep" {
		t.Fatal("empty configured prompt was replaced")
	}
}
