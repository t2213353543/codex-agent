package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfficialErrorWriterCapturesOnlyCompleteHTTPError(t *testing.T) {
	for _, status := range []int{409, 503} {
		record := RequestRecord{}
		writer := &officialErrorResponseWriter{gatewayResponseWriter: &gatewayResponseWriter{ResponseWriter: httptest.NewRecorder(), record: &record}, phase: "account_selection"}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"error":{"message":"account `))
		_, _ = writer.Write([]byte(`binding unavailable"}}`))
		writer.observe()
		if record.Error != "account binding unavailable" || record.Status != "error" || record.ErrorPhase != "account_selection" || record.HTTPCode != status {
			t.Fatalf("lost early error: %+v", record)
		}
	}
	for _, test := range []struct {
		status      int
		body        string
		contentType string
	}{{200, `{"error":{"message":"this is model text"}}`, "application/json"}, {200, "data: {\"error\":{\"message\":\"event\"}}\n\n", "text/event-stream"}, {503, strings.Repeat("x", 20000), "application/json"}} {
		record := RequestRecord{}
		writer := &officialErrorResponseWriter{gatewayResponseWriter: &gatewayResponseWriter{ResponseWriter: httptest.NewRecorder(), record: &record}, phase: "request"}
		writer.Header().Set("Content-Type", test.contentType)
		writer.WriteHeader(test.status)
		_, _ = writer.Write([]byte(test.body))
		writer.observe()
		if test.status == 200 && record.Error != "" {
			t.Fatal("ordinary model output treated as gateway error")
		}
		if writer.body.Len() > 16<<10 || test.status == 503 && record.Error != "official gateway returned HTTP 503" {
			t.Fatal("error capture not bounded")
		}
	}
}

func TestOfficialStructuredErrorsClassifyWithoutTextGuessing(t *testing.T) {
	for _, test := range []struct {
		raw    string
		status int
		code   string
	}{
		{`{"message":"full context","codexErrorInfo":"contextWindowExceeded"}`, 400, "context_length_exceeded"},
		{`{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_json_schema\",\"message\":\"required risk_level missing\"}}","codexErrorInfo":"other"}`, 400, "invalid_json_schema"},
		{`{"message":"arbitrary text says invalid_json_schema contextWindowExceeded 400","codexErrorInfo":"other"}`, 502, "other"},
		{`{"message":"limited","codexErrorInfo":"rateLimitExceeded"}`, 429, "rateLimitExceeded"},
		{`{"message":"Error running remote compact task: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\",\"message\":\"checkpoint identity rejected\"}}","codexErrorInfo":"other"}`, 400, "invalid_encrypted_content"},
		{`{"message":"unrecognized prefix: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_encrypted_content\"}}","codexErrorInfo":"other"}`, 502, "other"},
	} {
		err := officialResponseFailure("official failure", json.RawMessage(test.raw))
		status, code, _ := officialFailureHTTP(err, 502)
		if status != test.status || code != test.code {
			t.Fatalf("wrong structured classification: %d %s %v", status, code, err)
		}
	}
	raw := json.RawMessage(`{"code":-32602,"message":"invalid thread parameters Authorization: Bearer sk-secret-test","data":{"prompt":"not-for-logs"}}`)
	err := officialRPCFailure("thread/start", raw)
	status, _, _ := officialFailureHTTP(fmt.Errorf("initialization: %w", err), 502)
	if status != 400 || !strings.Contains(err.Error(), "rpc_code=-32602") || !strings.Contains(err.Error(), "invalid thread parameters") || strings.Contains(err.Error(), "sk-secret-test") || strings.Contains(err.Error(), "not-for-logs") {
		t.Fatalf("RPC detail lost or leaked: %v", err)
	}
}

func TestOfficialCancellationRemainsAbortedAndPreservesMeasuredUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	known := true
	record := RequestRecord{HTTPCode: 200, UsageKnown: &known, InputTokens: 10, CachedTokens: 5}
	status := recordOfficialFailure(&record, ctx, fmt.Errorf("wrapped: %w", context.Canceled), "response_event", 502)
	if status != 499 || record.Status != "aborted" || record.ErrorSource != "client" || record.HTTPCode != 200 || record.InputTokens != 10 || record.CachedTokens != 5 {
		t.Fatalf("client cancellation became upstream error or lost usage: %+v", record)
	}
	internal := RequestRecord{}
	if status := recordOfficialFailure(&internal, context.Background(), context.Canceled, "response_event", 502); status != 503 || internal.ErrorSource != "gateway" || internal.Status != "error" {
		t.Fatalf("internal cancellation mislabeled client: %+v", internal)
	}
}

func TestOfficialRPCCallPreservesStructuredRejectedParams(t *testing.T) {
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(request map[string]json.RawMessage) {
		raw, _ := json.Marshal(map[string]any{"id": request["id"], "error": map[string]any{"code": -32602, "message": "missing model"}})
		rpc.mu.Lock()
		waiter := rpc.waiters[string(request["id"])]
		rpc.mu.Unlock()
		waiter <- raw
	})
	_, err := rpc.call(t.Context(), "thread/start", map[string]any{})
	var protocol *officialProtocolError
	if !errors.As(err, &protocol) || protocol.status != 400 || !strings.Contains(err.Error(), "missing model") {
		t.Fatalf("raw RPC failure erased: %v", err)
	}
}

func TestOfficialGatewayLogsPreRuntimeAndAuthenticationFailures(t *testing.T) {
	fixture, _ := officialManagedFixture(t, false)
	p := newTestProxy(t, 1)
	p.RuntimeModes = true
	p.Official = fixture.Official
	p.BPS = &bpsRuntime{}
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceAppServer
	_ = p.Settings.Set(settings)
	for _, test := range []struct {
		key    string
		status int
		body   string
	}{{"invalid-key", 401, `{"model":"m","input":"x"}`}, {"test-client-key", 400, `{"model":"m","input":"x","previous_response_id":"not-stored"}`}} {
		request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+test.key)
		response := httptest.NewRecorder()
		p.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("unexpected handler status %d", response.Code)
		}
		p.Metrics.mu.Lock()
		record := p.Metrics.recent[p.Metrics.recentAt-1]
		p.Metrics.mu.Unlock()
		if record.Error == "" || record.ErrorSource == "" || record.ErrorPhase == "" || record.Status != "error" {
			t.Fatalf("unobserved early error: %+v", record)
		}
	}
}

type officialCanceledBody struct{ cancel context.CancelFunc }

func (r officialCanceledBody) Read([]byte) (int, error) { r.cancel(); return 0, context.Canceled }
func (officialCanceledBody) Close() error               { return nil }

func TestOfficialGatewayCanceledBodyIsAborted(t *testing.T) {
	fixture, _ := officialManagedFixture(t, false)
	p := newTestProxy(t, 1)
	p.RuntimeModes = true
	p.Official = fixture.Official
	p.BPS = &bpsRuntime{}
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceAppServer
	_ = p.Settings.Set(settings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
	request.Body = io.ReadCloser(officialCanceledBody{cancel: cancel})
	request.Header.Set("Authorization", "Bearer test-client-key")
	response := httptest.NewRecorder()
	p.ServeHTTP(response, request)
	p.Metrics.mu.Lock()
	record := p.Metrics.recent[p.Metrics.recentAt-1]
	p.Metrics.mu.Unlock()
	if record.Status != "aborted" || record.ErrorSource != "client" || record.HTTPCode == 502 || strings.Contains(response.Body.String(), "invalid request") {
		t.Fatalf("body cancellation became proxy error: %+v %s", record, response.Body.String())
	}
}
