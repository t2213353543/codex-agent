package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBPSHistoryInferenceModeIsolationAndLegacyCompatibility(t *testing.T) {
	p := &Proxy{BPS: &bpsRuntime{}}
	ctx := context.Background()
	for _, mode := range []string{inferencePowerPoint, inferenceAppServer} {
		modeCtx := withInferenceMode(ctx, mode)
		if err := p.bpsSaveHistory(modeCtx, "key", mode, bpsHistoryRecord{Owner: "owner", Session: "session", Request: json.RawMessage(`{"model":"m","input":[]}`)}); err != nil {
			t.Fatal(err)
		}
		if record, err := p.bpsLoadHistory(modeCtx, "key", mode); err != nil || record.Mode != mode {
			t.Fatalf("same mode history unavailable: %+v %v", record, err)
		}
		for _, other := range []string{inferencePowerPoint, inferenceAppServer, inferenceCodexCLI} {
			if other == mode {
				continue
			}
			if _, err := p.bpsLoadHistory(withInferenceMode(ctx, other), "key", mode); !errors.Is(err, errBPSHistoryMissing) {
				t.Fatalf("%s history crossed into %s: %v", mode, other, err)
			}
		}
	}
	key := bpsHistoryKey("key", inferencePowerPoint)
	p.BPS.history.mu.Lock()
	var old map[string]json.RawMessage
	_ = json.Unmarshal(p.BPS.history.items[key], &old)
	delete(old, "mode")
	p.BPS.history.items[key], _ = json.Marshal(old)
	p.BPS.history.mu.Unlock()
	if _, err := p.bpsLoadHistory(ctx, "key", inferencePowerPoint); err != nil {
		t.Fatalf("legacy BPS history broken: %v", err)
	}
	if _, err := p.bpsLoadHistory(withInferenceMode(ctx, inferenceAppServer), "key", inferencePowerPoint); !errors.Is(err, errBPSHistoryMissing) {
		t.Fatalf("legacy BPS history crossed into App Server: %v", err)
	}
}

func TestBPSGatewayReturnsStructuredUnsupportedParametersBeforeUpstream(t *testing.T) {
	var upstream atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { upstream.Add(1); w.WriteHeader(500) })
	for _, tc := range []struct{ field, param string }{
		{`"service_tier":"priority"`, "service_tier"},
		{`"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}`, "text.format.name"},
	} {
		r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":[],`+tc.field+`}`)
		w := httptest.NewRecorder()
		p.gatewayBPS(w, r)
		var got struct {
			Error struct{ Code, Param, Type, Message string }
		}
		if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error.Code != "unsupported_parameter" || got.Error.Param != tc.param || got.Error.Type != "invalid_request_error" || !strings.Contains(got.Error.Message, "/models") || strings.Contains(got.Error.Message, "PowerPoint") {
			t.Fatalf("unexpected parameter rejection for %s: %d %s", tc.field, w.Code, w.Body.String())
		}
	}
	if upstream.Load() != 0 {
		t.Fatalf("invalid BPS parameters reached upstream %d times", upstream.Load())
	}
}

func TestBPSGatewayInferenceSessionConflictRejectedBeforeAccountSelection(t *testing.T) {
	var upstream atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { upstream.Add(1); w.WriteHeader(500) })
	p.RuntimeModes = true
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":[{"type":"reasoning","encrypted_content":"opaque-other-backend-state"}]}`)
	r.Header.Set("Session-Id", "same-client-conversation")
	keyID, _, _, err := p.resolveGatewayKeyContext(r.Context(), bearerToken(r))
	if err != nil || keyID == "" {
		t.Fatalf("fixture key unavailable: %v", err)
	}
	if err := p.bindInferenceSession(withInferenceMode(r.Context(), inferenceAppServer), keyID, "same-client-conversation"); err != nil {
		t.Fatal(err)
	}
	r = r.WithContext(withInferenceMode(r.Context(), inferencePowerPoint))
	w := httptest.NewRecorder()
	p.gatewayBPS(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "previous backend") || upstream.Load() != 0 {
		t.Fatalf("mode conflict not rejected before upstream: %d %s attempts=%d", w.Code, w.Body.String(), upstream.Load())
	}
}

func TestBPSGatewayCapabilityRejectionUsesActualAccountCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort, wantCode string
		status, wantHTTP              int
	}{
		{"unknown-model-fixed-route-preserves-real-denial", "unknown-model-fixture", "xhigh", "original_failure", 403, 403},
		{"upstream-validation-preserved", "gpt-6-astra", "high", "original_failure", 422, 422},
		{"other-validation-failure", "gpt-6-astra", "xhigh", "original_failure", 422, 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var inferenceCalls, catalogCalls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/responses/access" {
					catalogCalls.Add(1)
					if r.Method != http.MethodGet || r.URL.Query().Get("include_models") != "true" {
						t.Errorf("incorrect catalog request: %s %s", r.Method, r.URL)
					}
					_, _ = fmt.Fprint(w, `{"allowed":true,"model_catalog":{"default_model":"gpt-6-astra","models":[{"id":"gpt-6-astra","label":"GPT-6 Astra","efforts":[{"value":"xhigh"}],"default_effort":"xhigh"}]}}`)
					return
				}
				inferenceCalls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, `{"error":{"code":"original_failure","message":"Original upstream validation failure"}}`)
			})
			r := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"input":[]}`, tc.model, tc.effort))
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			var got struct {
				Error struct {
					Code            string
					AvailableModels []struct{ ID string } `json:"available_models"`
				}
			}
			if w.Code != tc.wantHTTP || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Error.Code != tc.wantCode {
				t.Fatalf("incorrect capability classification: %d %s", w.Code, w.Body.String())
			}
			if inferenceCalls.Load() != 1 || catalogCalls.Load() != 0 {
				t.Fatalf("generation retried or catalog not consulted: inference=%d catalog=%d", inferenceCalls.Load(), catalogCalls.Load())
			}
			if tc.wantHTTP == 400 && (len(got.Error.AvailableModels) != 1 || got.Error.AvailableModels[0].ID != "gpt-6-astra") {
				t.Fatal("diagnostic catalog unavailable")
			}
		})
	}
}
