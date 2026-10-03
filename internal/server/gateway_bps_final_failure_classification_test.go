package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type finalErrorCase struct {
	Name, Code, Kind, Message, WantFinal, WantCategory string
	HTTP                                               int
}

var finalErrorCases = []finalErrorCase{
	{Name: "sse_type_only_auth", Kind: "authentication_error", Message: "synthetic authentication error", HTTP: 200, WantFinal: "unauthorized", WantCategory: "authentication"},
	{Name: "sse_model_code_rate_type", Code: "model_not_found", Kind: "rate_limit_error", Message: "synthetic conflicting error", HTTP: 200, WantFinal: "rateLimitExceeded", WantCategory: "rate_limit"},
	{Name: "sse_busy_code_auth_type", Code: "server_overloaded", Kind: "authentication_error", Message: "synthetic conflicting error", HTTP: 200, WantFinal: "unauthorized", WantCategory: "authentication"},
	{Name: "http_quota_auth_words", Message: "invalidated oauth in synthetic upstream message", HTTP: 429, WantFinal: "rateLimitExceeded", WantCategory: "rate_limit"},
	{Name: "sse_quota_control", Code: "rate_limit_exceeded", Message: "synthetic quota", HTTP: 200, WantFinal: "rateLimitExceeded", WantCategory: "rate_limit"},
}

func produceFinalErrorRecord(t *testing.T, tc finalErrorCase) RequestRecord {
	t.Helper()
	generations := 0
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("unexpected producer request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		generations++
		_, _ = io.Copy(io.Discard, r.Body)
		detail := map[string]any{"message": tc.Message}
		if tc.Code != "" {
			detail["code"] = tc.Code
		}
		if tc.Kind != "" {
			detail["type"] = tc.Kind
		}
		if tc.HTTP != 200 {
			w.WriteHeader(tc.HTTP)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": detail})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_" + tc.Name, "status": "failed", "model": "gpt-6-astra", "output": []any{}, "error": detail, "usage": map[string]any{"input_tokens": 37, "output_tokens": 0, "input_tokens_details": map[string]int{"cached_tokens": 0, "cache_write_tokens": 20}}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
	})
	settings, _ := p.Settings.Get()
	settings.Upstream401RecoveryEnabled = false
	settings.AutomaticTokenRefreshEnabled = false
	settings.RateLimitCooldownEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	session := "synthetic-final-" + tc.Name
	if _, err = p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(account)); err != nil {
		t.Fatal(err)
	}
	req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-state-no-real-upstream"}]}`)
	req.Header.Set("Session-Id", session)
	response := httptest.NewRecorder()
	p.gatewayBPS(response, req)
	if generations != 1 {
		t.Fatalf("fixture generation calls=%d", generations)
	}
	p.Metrics.mu.Lock()
	defer p.Metrics.mu.Unlock()
	if p.Metrics.recentAt != 1 {
		t.Fatal("logical request did not produce exactly one record")
	}
	record := p.Metrics.recent[0]
	if record.Status != "error" || record.FinalUpstreamErrorCode != tc.WantFinal || record.FinalUpstreamErrorSource != "upstream" || record.SentModel != "gpt-6-astra" {
		t.Fatalf("actual producer shape differs: %+v", record)
	}
	return record
}

func TestBPSFinalClassificationRealProducerManagement(t *testing.T) {
	exerciseFinalClassification(t, false)
}
func TestPGBPSFinalClassificationRealProducerManagement(t *testing.T) {
	exerciseFinalClassification(t, true)
}
func exerciseFinalClassification(t *testing.T, pg bool) {
	records := map[string]RequestRecord{}
	report := map[string]any{}
	defer func() {
		if folder := os.Getenv("FINAL_ERROR_REVIEW_OUTPUT"); folder != "" {
			suffix := "file"
			if pg {
				suffix = "pg"
			}
			for name, value := range map[string]any{"records-" + suffix: records, "management-" + suffix: report} {
				raw, _ := json.MarshalIndent(value, "", "  ")
				if err := os.WriteFile(filepath.Join(folder, name+".json"), raw, 0600); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	for _, tc := range finalErrorCases {
		t.Run(tc.Name, func(t *testing.T) {
			record := produceFinalErrorRecord(t, tc)
			records[tc.Name] = record
			var metrics *Metrics
			if pg {
				metrics = newTestPGMetrics(t, newMetricsTestDB(t))
			} else {
				var err error
				metrics, err = NewMetrics(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(metrics.Close)
			}
			copied := record
			copied.EventID = "classification-" + tc.Name
			copied.Time = time.Now().Unix()
			copied.ID = 0
			metrics.Record(copied)
			if metrics.HealthError() != nil {
				t.Fatal(metrics.HealthError())
			}
			page, err := metrics.managedLogs(context.Background(), managedQuery(t, "exact_total=true"))
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("list %v count%d", err, len(page.Items))
			}
			listed := page.Items[0]
			detail, err := metrics.managedLogDetail(t.Context(), copied.EventID)
			if err != nil {
				t.Fatal(err)
			}
			stats, err := metrics.managedLogStats(t.Context(), managedQuery(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			categoryQuery := managedQuery(t, "error_category="+url.QueryEscape(tc.WantCategory)+"&exact_total=true")
			filtered, err := metrics.managedLogs(t.Context(), categoryQuery)
			if err != nil {
				t.Fatal(err)
			}
			filteredStats, err := metrics.managedLogStats(t.Context(), categoryQuery)
			if err != nil {
				t.Fatal(err)
			}
			var exported bytes.Buffer
			_, err = metrics.exportManagedLogs(t.Context(), managedQuery(t, ""), "jsonl", &exported)
			if err != nil {
				t.Fatal(err)
			}
			var export map[string]json.RawMessage
			_ = json.Unmarshal(bytes.TrimSpace(exported.Bytes()), &export)
			var exportCategory string
			_ = json.Unmarshal(export["error_category"], &exportCategory)
			entry := map[string]any{"expected_category": tc.WantCategory, "actual_producer_final": record.FinalUpstreamErrorCode, "actual_http": record.HTTPCode, "go_category": managedFailureCategory(record), "list_category": listed.ErrorCategory, "detail_category": managedRecord(detail).ErrorCategory, "stats_categories": stats.ErrorCategories, "filtered_list_total": filtered.Total, "filtered_stats_requests": filteredStats.Requests, "export_category": exportCategory, "record_error": record.Error}
			report[tc.Name] = entry
			t.Logf("mode_pg=%v case=%s producer_final=%s HTTP=%d go/list/detail/export=%s/%s/%s/%s expected_filter_count=%d", pg, tc.Name, record.FinalUpstreamErrorCode, record.HTTPCode, managedFailureCategory(record), listed.ErrorCategory, managedRecord(detail).ErrorCategory, exportCategory, filtered.Total)
			if listed.ErrorCategory != tc.WantCategory || managedRecord(detail).ErrorCategory != tc.WantCategory || exportCategory != tc.WantCategory || filtered.Total != 1 || filteredStats.Requests != 1 {
				t.Error("actual produced final failure conflicts with management listing/filter/statistics/export")
			}
			if len(stats.ErrorCategories) != 1 || stats.ErrorCategories[0].Name != tc.WantCategory || stats.ErrorCategories[0].Requests != 1 {
				t.Error("summary classified actual final failure under wrong category")
			}
		})
	}
}

func TestBPSFinalClassificationFiniteProvenanceAndLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name, status, source, code, error string
		http                              int
		want                              string
	}{
		{"typed_limit", "error", "upstream", "usageLimitExceeded", "invalidated oauth", 200, "rate_limit"},
		{"typed_busy", "error", "upstream", "server_overloaded", "rate_limit_error", 200, "overloaded"},
		{"typed_account_rejection", "error", "upstream", "upstream_account_rejected", "policy violation", 403, "authentication"},
		{"legacy", "error", "", "", "invalidated oauth", 429, "invalid_oauth"},
		{"wrong_source", "error", "gateway", "rateLimitExceeded", "invalidated oauth", 200, "invalid_oauth"},
		{"unknown_code", "error", "upstream", "PRIVATE_UNKNOWN", "invalidated oauth", 200, "invalid_oauth"},
		{"success", "ok", "upstream", "rateLimitExceeded", "invalidated oauth", 200, ""},
		{"aborted", "aborted", "upstream", "unauthorized", "context canceled", 499, "aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := RequestRecord{Status: tc.status, FinalUpstreamErrorSource: tc.source, FinalUpstreamErrorCode: tc.code, Error: tc.error, HTTPCode: tc.http}
			if got := managedFailureCategory(record); got != tc.want {
				t.Errorf("category=%s want=%s", got, tc.want)
			}
		})
	}
}
