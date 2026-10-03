package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Actual gateway production path writes to isolated PostgreSQL. A new Metrics
// instance verifies durable logical/transport status and unknown accounting.
func TestPGBPSUpstreamAuthUnavailablePersistence(t *testing.T) {
	db := newMetricsTestDB(t)
	var calls []string
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/responses" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		calls = append(calls, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"unauthorized","message":"synthetic invalidated oauth token"}}`)
	})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.Upstream401RecoveryEnabled = false
	settings.AutomaticTokenRefreshEnabled = false
	settings.RateLimitCooldownEnabled = false
	if err = p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	if err = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.Concurrency = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{"access_token": "next-token", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err = p.Store.ImportWith("next", credential, AccountDefaults{Concurrency: 2, Priority: 1, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	// Real file-backed authorization/selection, direct PG metrics producer.
	p.Metrics.Close()
	p.Metrics = newTestPGMetrics(t, db)
	t.Cleanup(p.Metrics.Close)
	request := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"synthetic plaintext"}]}`)
	request.Header.Set("Session-Id", "pg-upstream-auth-failure")
	out := httptest.NewRecorder()
	p.gatewayBPS(out, request)
	if out.Code != 503 || !reflect.DeepEqual(calls, []string{"Bearer test-upstream-token", "Bearer next-token"}) {
		t.Fatalf("want two401 attempts/public503: HTTP=%d calls=%v", out.Code, calls)
	}
	var public struct {
		Error struct{ Code, Type, Message string }
	}
	if json.Unmarshal(out.Body.Bytes(), &public) != nil || public.Error.Code != "upstream_authentication_unavailable" || public.Error.Type != "server_error" || out.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("upstream authentication exposed as client credentials failure")
	}
	if err = p.Metrics.Save(); err != nil || p.Metrics.HealthError() != nil {
		t.Fatalf("producer persistence failed: %v", err)
	}
	fresh := newTestPGMetrics(t, db)
	t.Cleanup(fresh.Close)
	page, err := fresh.managedLogs(t.Context(), managedQuery(t, "exact_total=true&http_code=503&error_category=authentication"))
	if err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("fresh PG query failed: err=%v count=%d/%d", err, page.Total, len(page.Items))
	}
	row := page.Items[0]
	assertPGAuthFailureRecord(t, row.RequestRecord)
	if row.UsageKnown != nil || row.ErrorCategory != "authentication" {
		t.Fatal("management invented known usage/category")
	}
	detail, err := fresh.managedLogDetail(t.Context(), row.EventID)
	if err != nil {
		t.Fatal(err)
	}
	assertPGAuthFailureRecord(t, detail)
	var persisted []byte
	if err = db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", row.EventID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	var durable RequestRecord
	if json.Unmarshal(persisted, &durable) != nil {
		t.Fatal("durable request invalid")
	}
	assertPGAuthFailureRecord(t, durable)
	var events int
	if err = db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM usage_events WHERE event_id=$1", row.EventID).Scan(&events); err != nil || events != 1 {
		t.Fatal("logical request did not persist exactly one event")
	}
	stats, err := fresh.managedLogStats(t.Context(), managedQuery(t, "http_code=503&error_category=authentication"))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.Errors != 1 || stats.Success != 0 || stats.UsageKnownRequests != 0 || stats.UsageUnknownRequests != 1 || stats.CacheReadSamples != 0 || stats.CacheReadUnknownRequests != 1 || stats.CacheReadRate != nil || stats.CacheWriteSamples != 0 || stats.CacheWriteTokens != nil || stats.TTFTSamples != 0 || stats.AverageTTFT != nil {
		t.Fatal("PG statistics invented known usage/cache/TTFT or success")
	}
	if len(stats.ErrorCategories) != 1 || stats.ErrorCategories[0].Name != "authentication" || stats.ErrorCategories[0].Requests != 1 {
		t.Fatal("PG category lost final authentication provenance")
	}
	var jsonl bytes.Buffer
	if n, err := fresh.exportManagedLogs(t.Context(), managedQuery(t, ""), "jsonl", &jsonl); err != nil || n != 1 {
		t.Fatal("JSONL export failed")
	}
	var exported map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(jsonl.Bytes()), &exported) != nil || string(exported["http_code"]) != "503" || string(exported["final_upstream_error_code"]) != `"unauthorized"` || string(exported["final_upstream_error_source"]) != `"upstream"` || string(exported["usage_known"]) != "null" {
		t.Fatal("JSONL logical503/final401/unknown usage changed")
	}
	for _, key := range []string{"cached_tokens_known", "cache_write_tokens", "ttft_ms"} {
		if v, ok := exported[key]; ok && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			t.Fatalf("JSONL invented %s", key)
		}
	}
	var exportedAttempts []GatewayAttemptSummary
	if json.Unmarshal(exported["attempts"], &exportedAttempts) != nil || !reflect.DeepEqual(exportedAttempts, detail.Attempts) {
		t.Fatal("JSONL changed actual attempt ledger")
	}
	var csvData bytes.Buffer
	if n, err := fresh.exportManagedLogs(t.Context(), managedQuery(t, ""), "csv", &csvData); err != nil || n != 1 {
		t.Fatal("CSV export failed")
	}
	csvRows, err := csv.NewReader(&csvData).ReadAll()
	if err != nil || len(csvRows) != 2 {
		t.Fatal("CSV shape invalid")
	}
	fields := map[string]string{}
	for i, name := range csvRows[0] {
		fields[name] = csvRows[1][i]
	}
	if fields["http_code"] != "503" || fields["final_upstream_error_code"] != "unauthorized" || fields["final_upstream_error_source"] != "upstream" || fields["error_category"] != "authentication" {
		t.Fatal("CSV public/final status attribution lost")
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens", "usage_known", "ttft_ms"} {
		if fields[name] != "" {
			t.Fatalf("CSV invented zero/known observation for %s", name)
		}
	}
	if json.Unmarshal([]byte(fields["attempts"]), &exportedAttempts) != nil || !reflect.DeepEqual(exportedAttempts, detail.Attempts) {
		t.Fatal("CSV changed actual attempts")
	}
	for _, name := range []string{"test", "next"} {
		account, err := p.Store.Get(name)
		if err != nil || account.Concurrency != 2 || p.inFlight(gatewayAdmissionKey(account)) != 0 {
			t.Fatal("concurrency configuration or slot leaked")
		}
	}
	t.Log("real producer two-account401 exhaustion: public503; attempts401/observed401; final unauthorized/upstream; PG freshread/stats/CSV/JSONL unknown usage preserved")
}

func assertPGAuthFailureRecord(t *testing.T, r RequestRecord) {
	t.Helper()
	if r.HTTPCode != 503 || r.Status != "error" || r.ErrorSource != "upstream" || r.FinalUpstreamErrorCode != "unauthorized" || r.FinalUpstreamErrorSource != "upstream" || !strings.Contains(r.Error, "invalidated oauth token") {
		t.Fatalf("record provenance changed: HTTP=%d status=%s final=%s/%s source=%s", r.HTTPCode, r.Status, r.FinalUpstreamErrorCode, r.FinalUpstreamErrorSource, r.ErrorSource)
	}
	if r.UsageKnown != nil || r.CachedTokensKnown != nil || r.CacheWriteTokens != nil || r.TTFTMs != nil || r.InputTokens != 0 || r.OutputTokens != 0 || r.CachedTokens != 0 || r.ReturnedModel != "" {
		t.Fatal("nonterminal401 acquired fabricated usage/cache/model/TTFT")
	}
	if len(r.Attempts) != 2 || r.AttemptsTruncated == nil || *r.AttemptsTruncated {
		t.Fatal("attempt ledger incomplete")
	}
	for i, attempt := range r.Attempts {
		if attempt.Attempt != i+1 || attempt.Phase != "request" || attempt.Endpoint != "/responses" || attempt.StatusCode == nil || *attempt.StatusCode != 401 || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != 401 || attempt.ErrorSource != "upstream" {
			t.Fatalf("attempt%d rewritten: %+v", i, attempt)
		}
	}
}
