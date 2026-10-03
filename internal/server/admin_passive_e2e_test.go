package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedPassiveRuntimeToQuery(t *testing.T) { exercisePassiveRuntimeToQuery(t, webTestSetup) }
func TestPGManagedPassiveRuntimeToQuery(t *testing.T) {
	exercisePassiveRuntimeToQuery(t, adminPGTestProxy)
}

func exercisePassiveRuntimeToQuery(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	management, h := setup(t)
	if err := management.startGatewayIngressEvents(); err != nil {
		t.Fatal(err)
	}
	defer management.IngressEvents.Close(context.Background())
	const response = `{"id":"passive-runtime-response","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"image_tokens":0},"output_tokens_details":{"image_tokens":1}}}`
	const wire = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	var sends atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "passive-runtime-upstream")
		_, _ = io.WriteString(w, wire)
	})
	p.Metrics, p.IngressEvents = management.Metrics, management.IngressEvents
	r := proxyRequest("POST", "/v1/responses?PRIVATE_QUERY=secret", `{"model":"passive-runtime-model","input":"PRIVATE_PROMPT","stream":true}`)
	r.Header.Set("X-Client-Request-ID", "runtime-client-1")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != wire || sends.Load() != 1 {
		t.Fatal("wire/dispatch changed")
	}
	bad := proxyRequest("POST", "/v1/responses?PRIVATE_QUERY=secret", "PRIVATE_BODY")
	bad.Header.Set("Authorization", "Bearer PRIVATE_INVALID_KEY")
	bad.Header.Set("X-Client-Request-ID", "runtime-rejected-1")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, bad)
	if w.Code != 401 || sends.Load() != 1 {
		t.Fatal("reject made upstream request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := management.IngressEvents.Close(ctx); err != nil {
		t.Fatal(err)
	}
	result := managedTestRequest(t, h, "GET", "/api/logs?client_request_id=runtime-client-1&exact_total=true", nil, 200)
	var page managedLogPage
	if err := json.Unmarshal(result.Body.Bytes(), &page); err != nil || page.Total != 1 {
		t.Fatalf("persisted client correlation missing: %s %v", result.Body.String(), err)
	}
	rec := page.Items[0]
	if rec.RequestID != "passive-runtime-upstream" || rec.AuthenticationMs == nil || rec.ResponseTransferMs == nil || rec.ImageInputTokens == nil || *rec.ImageInputTokens != 0 || rec.ImageOutputTokens == nil || *rec.ImageOutputTokens != 1 || rec.InputTokens+rec.OutputTokens != 10 {
		t.Fatal("actual passive fields absent after persistence")
	}
	identity := rec.EventID
	if identity == "" {
		identity = strconv.FormatInt(rec.ID, 10)
	}
	for _, path := range []string{"/api/logs/stats?client_request_id=runtime-client-1", "/api/logs/export?format=jsonl&client_request_id=runtime-client-1", "/api/logs/" + identity} {
		result := managedTestRequest(t, h, "GET", path, nil, 200)
		if strings.Contains(result.Body.String(), "PRIVATE_") || !strings.Contains(result.Body.String(), "image_input") {
			t.Fatal("passive query privacy/metadata contract", path)
		}
	}
	result = managedTestRequest(t, h, "GET", "/api/ingress-events?client_request_id=runtime-rejected-1", nil, 200)
	var ingress struct {
		Items []gatewayIngressEvent `json:"items"`
		Total int                   `json:"total"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &ingress); err != nil || ingress.Total != 1 || ingress.Items[0].HTTPCode != 401 {
		t.Fatalf("actual rejected request not persisted: %s %v", result.Body.String(), err)
	}
	if strings.Contains(result.Body.String(), "PRIVATE_") {
		t.Fatal("ingress private content retained")
	}
	if management.Store.DB != nil {
		var details, events int
		if err := management.Store.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM request_logs),(SELECT count(*) FROM usage_events)").Scan(&details, &events); err != nil || details != 1 || events != 1 {
			t.Fatal("pre-admission changed business totals", details, events, err)
		}
	}
}
