package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagedObservationRuntimeToQuery(t *testing.T) {
	exerciseManagedObservationRuntime(t, webTestSetup)
}
func TestPGManagedObservationRuntimeToQuery(t *testing.T) {
	exerciseManagedObservationRuntime(t, adminPGTestProxy)
}
func exerciseManagedObservationRuntime(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	const wire = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"runtime-observed\",\"model\":\"actual-returned\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":2}}}\n\n"
	var requests, refreshes atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"synthetic-runtime-rotated","refresh_token":"synthetic-runtime-refresh"}`, "test-account", "fixture-user"))
			return
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, wire)
	})
	management, h := setup(t)
	p.Metrics = management.Metrics
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest("POST", "/v1/responses?private=DO_NOT_STORE", `{"model":"actual-sent","stream":true,"input":"DO_NOT_STORE"}`))
	if w.Code != 200 || w.Body.String() != wire || requests.Load() != 2 || refreshes.Load() != 1 {
		t.Fatal("runtime wire/dispatch changed")
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	response := managedTestRequest(t, h, "GET", "/api/logs?sent_model=actual-sent&returned_model=actual-returned&returned_model_mismatch=true&inbound_path=%2Fresponses&exact_total=true", nil, 200)
	var page managedLogPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatal("runtime fields absent in management query", err, response.Body.String())
	}
	r := page.Items[0]
	if len(r.Attempts) != 3 || r.Attempts[0].Phase != "request" || r.Attempts[1].Phase != "refresh" || r.Attempts[2].Phase != "request" || r.AttemptsTruncated == nil || *r.AttemptsTruncated || r.ObservationSchemaVersion != 1 || r.RequestPreparationMs == nil {
		t.Fatal("runtime observation contract lost")
	}
	identity := r.EventID
	if identity == "" {
		identity = strconv.FormatInt(r.ID, 10)
	}
	for _, path := range []string{"/api/logs/" + url.PathEscape(identity), "/api/logs/stats?sent_model=actual-sent", "/api/logs/export?format=jsonl&sent_model=actual-sent"} {
		result := managedTestRequest(t, h, "GET", path, nil, 200)
		if strings.Contains(result.Body.String(), "DO_NOT_STORE") || strings.Contains(result.Body.String(), "synthetic-runtime-refresh") {
			t.Fatal("runtime query persisted secret")
		}
		if !strings.Contains(result.Body.String(), "actual-returned") {
			t.Fatal("management lost returned model", path)
		}
	}
}
