package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfficialGatewayStreamingAndJSONAccountSameUsage(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "json"}[stream], func(t *testing.T) {
			fixture, account := officialManagedFixture(t, false)
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			p.Official = fixture.Official
			p.BPS = &bpsRuntime{}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.AccessToken = account.AccessToken
				a.RefreshToken = ""
				a.AccountID = account.AccountID
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s, _ := p.Settings.Get()
			s.InferenceMode = inferenceAppServer
			s.BuiltinPromptEnabled = false
			s.AutomaticTokenRefreshEnabled = false
			s.Upstream401RecoveryEnabled = false
			_ = p.Settings.Set(s)
			raw, _ := json.Marshal(map[string]any{"model": "model-a", "stream": stream, "input": "Reply OK", "reasoning": map[string]string{"effort": "xhigh"}})
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer test-client-key")
			req.Header.Set("X-Client-Request-ID", "accounting-fixture")
			out := httptest.NewRecorder()
			p.ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatalf("handler: %d %s", out.Code, out.Body.String())
			}
			p.Metrics.mu.Lock()
			records := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
			p.Metrics.mu.Unlock()
			if len(records) != 1 {
				t.Fatalf("records=%d", len(records))
			}
			r := records[0]
			if r.UsageKnown == nil || !*r.UsageKnown || r.InputTokens != 17 || r.OutputTokens != 2 {
				t.Fatalf("lost %v usage: %+v", stream, r)
			}
			if r.ReturnedModel != "model-a" || r.UpstreamModelSource == "response" || r.ClientRequestID != "accounting-fixture" || r.AuthenticationMs == nil || r.OfficialRuntimeMs == nil || r.OfficialInferenceMs == nil || r.TTFTMs == nil {
				t.Fatalf("missing runtime observations: %+v", r)
			}
			managed, _ := json.Marshal(managedRecord(r))
			var displayed map[string]any
			_ = json.Unmarshal(managed, &displayed)
			if displayed["cached_tokens"] != float64(0) || displayed["input_tokens"] != float64(17) || displayed["returned_model"] != "model-a" {
				t.Fatal("management view lost observed values")
			}
		})
	}
}

func TestOfficialResponseAccountingPreservesActualCacheAndUnknown(t *testing.T) {
	r := RequestRecord{Status: "ok", HTTPCode: http.StatusOK}
	observed := []byte(`{"id":"r","status":"completed","model":"observed-model","usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":960}},"metadata":{"app_server_model_source":"app_server_thread"}}`)
	recordOfficialResponseOutcome(&r, observed)
	if r.InputTokens != 1000 || r.CachedTokens != 960 || r.OutputTokens != 20 || r.UsageKnown == nil || !*r.UsageKnown || r.UpstreamModelSource != "app_server_thread" {
		t.Fatalf("wrong accounting: %+v", r)
	}
	unknown := RequestRecord{}
	recordOfficialResponseOutcome(&unknown, []byte(`{"id":"u","status":"completed","model":"m","usage":null}`))
	if unknown.UsageKnown != nil {
		t.Fatal("missing usage was fabricated")
	}
}

func TestOfficialGatewayUnavailableModelIsClientError(t *testing.T) {
	fixture, account := officialManagedFixture(t, false)
	p := newTestProxy(t, 1)
	p.RuntimeModes, p.Official, p.BPS = true, fixture.Official, &bpsRuntime{}
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccessToken, a.RefreshToken, a.AccountID = account.AccessToken, "", account.AccountID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceAppServer
	settings.BuiltinPromptEnabled, settings.AutomaticTokenRefreshEnabled = false, false
	_ = p.Settings.Set(settings)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"absent-model","stream":true,"input":"Reply OK"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest || !strings.Contains(out.Body.String(), "official runtime catalog") {
		t.Fatalf("unsupported model became gateway failure: %d %s", out.Code, out.Body.String())
	}
}
