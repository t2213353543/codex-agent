package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The same authenticated upstream catalog feeds two wire contracts. Query/path
// select the contract; a client User-Agent must not change an OpenAI list into a
// Codex manifest or leak manifest-only instructions into an ordinary list.
func TestBPSModelsEndpointWireContracts(t *testing.T) {
	const access = `{"allowed":true,"model_catalog":{"default_model":"gpt-6-astra","models":[{"id":"gpt-6-astra","label":"Fixture label","efforts":[{"value":"medium"}],"default_effort":"medium"}]}}`
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/responses/access" || r.URL.Query().Get("include_models") != "true" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, access)
	})
	for _, tc := range []struct {
		name, path, ua string
		codex          bool
	}{
		{"standard_sub2api", "/v1/models", "Go-http-client/1.1", false},
		{"codex_version", "/v1/models?client_version=0.155.0", "codex_cli_rs/0.155.0", true},
		{"standard_after_codex", "/v1/models", "OpenAI/Python", false},
		{"codex_native_path", "/models", "", true},
		{"codex_native_version", "/models?client_version=0.151.0", "Codex Desktop/0.151.0", true},
		{"standard_empty_version", "/v1/models?client_version=", "", false},
		{"standard_ua_does_not_negotiate", "/v1/models", "codex_cli_rs/0.155.0", false},
		{"standard_unknown_ua", "/v1/models", "not-codex-invented/1", false},
		{"codex_explicit_unknown_ua", "/v1/models?client_version=0.161.0", "fixture", true},
		{"standard_unrelated_query", "/v1/models?extra=1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := proxyRequest(http.MethodGet, tc.path, "")
			request.Header.Set("User-Agent", tc.ua)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, request)
			if w.Code != http.StatusOK {
				t.Fatalf("model discovery HTTP %d: %s", w.Code, w.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			field, id := "data", "id"
			if tc.codex {
				field, id = "models", "slug"
				if body["data"] != nil {
					t.Fatal("Codex manifest returned standard list")
				}
			} else if body["models"] != nil || string(body["object"]) != `"list"` {
				t.Fatal("standard model list returned Codex manifest")
			}
			var models []map[string]json.RawMessage
			if err := json.Unmarshal(body[field], &models); err != nil || len(models) != 1 || string(models[0][id]) != `"gpt-6-astra"` {
				t.Fatalf("wrong catalog membership: %s", w.Body.String())
			}
			if tc.codex {
				if string(models[0]["service_tiers"]) != "[]" || string(models[0]["additional_speed_tiers"]) != "[]" || models[0]["supported_reasoning_levels"] == nil {
					t.Fatal("Codex capability contract lost")
				}
			} else if models[0]["base_instructions"] != nil || models[0]["supported_reasoning_levels"] != nil || string(models[0]["object"]) != `"model"` {
				t.Fatal("standard entry exposed manifest-only fields")
			}
		})
	}
}

func TestBPSModelsEndpointInvalidCatalogStaysFailure(t *testing.T) {
	for _, access := range []string{`{"allowed":false}`, `{"allowed":true,"model_catalog":{"models":[]}}`, `{"allowed":true,"model_catalog":{"models":[{"id":"m"},{"id":"m"}]}}`} {
		p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, access)
		})
		for _, path := range []string{"/v1/models", "/v1/models?client_version=0.155.0", "/models"} {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodGet, path, ""))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("invalid upstream catalog became success: path=%s status=%d", path, w.Code)
			}
		}
	}
}
