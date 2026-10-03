package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Real gateway preparation inserts an empty instructions field. Verify its
// provenance across policy/body preparation/catalog/writer/HTTP dispatch.
func TestAppServerCLIInstructionOriginThroughGateway(t *testing.T) {
	for _, scenario := range []string{"omitted", "explicit-empty", "explicit-text", "policy-prompt"} {
		t.Run(scenario, func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			settings, _ := p.Settings.Get()
			settings.InferenceMode = inferenceCodexCLI
			settings.BuiltinPromptEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			settings.Upstream401RecoveryEnabled = false
			if scenario == "policy-prompt" {
				settings.BuiltinPromptEnabled = true
				settings.BuiltinPrompt = "POLICY INSTRUCTIONS"
			}
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			var observed []byte
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"models":[` + appServerCLITestModel + `]}`)), Request: r}, nil
				}
				observed, _ = io.ReadAll(r.Body)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")), Request: r}, nil
			})}
			p.AppServerCLI = newAppServerCLI(p)
			defer p.AppServerCLI.Close()
			body := map[string]any{"model": "fixture", "input": "hello", "stream": false, "tools": []any{map[string]string{"type": "web_search"}}}
			if scenario == "explicit-empty" {
				body["instructions"] = ""
			}
			if scenario == "explicit-text" {
				body["instructions"] = "CALLER INSTRUCTIONS"
			}
			raw, _ := json.Marshal(body)
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer test-client-key")
			request.Header.Set("Session-Id", "instructions-fixture")
			response := httptest.NewRecorder()
			p.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("gateway failed: %d %s", response.Code, response.Body.String())
			}
			var actual map[string]json.RawMessage
			if err := json.Unmarshal(observed, &actual); err != nil {
				t.Fatal(err)
			}
			var instruction string
			_ = json.Unmarshal(actual["instructions"], &instruction)
			want := map[string]string{"omitted": "CATALOG INSTRUCTIONS", "explicit-empty": "", "explicit-text": "CALLER INSTRUCTIONS", "policy-prompt": "POLICY INSTRUCTIONS"}[scenario]
			if instruction != want {
				t.Fatalf("instruction origin lost: got %q want %q", instruction, want)
			}
			if scenario == "explicit-empty" {
				if _, exists := actual["instructions"]; exists {
					t.Fatal("writer serialized explicit empty instructions instead of omitting")
				}
			}
		})
	}
}
