package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func managedUsageFixture(t *testing.T, pg bool) *Metrics {
	t.Helper()
	var m *Metrics
	if pg {
		m = newTestPGMetrics(t, newMetricsTestDB(t))
	} else {
		var err error
		m, err = NewMetrics(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
	}
	rows := []RequestRecord{
		{EventID: "event-a", Time: 1700000000, AccountID: "acc-1", Account: "Alpha", KeyID: "key-1", KeyName: "Primary", Groups: []string{"group-a"}, Model: "codex-a", Endpoint: "responses", Status: "ok", HTTPCode: 200, Stream: true, InputTokens: 100, OutputTokens: 20, CachedTokens: 50, LatencyMs: 500},
		{EventID: "event-b", Time: 1700000001, AccountID: "acc-1", Account: "Alpha", KeyID: "key-1", KeyName: "Primary", Groups: []string{"group-a"}, Model: "codex-a", Endpoint: "responses", Status: "error", HTTPCode: 200, Stream: true, Error: "Our servers are currently overloaded. Please try again later.", LatencyMs: 100},
		{EventID: "event-c", Time: 1700000001, AccountID: "acc-2", Account: "=HYPERLINK(\"bad\")", KeyID: "key-2", KeyName: "Secondary", Groups: []string{"group-b"}, Model: "codex-b", Endpoint: "compact", Status: "ok", HTTPCode: 200, InputTokens: 40, OutputTokens: 10, LatencyMs: 200},
		{EventID: "event-d", Time: 1700000003, AccountID: "acc-1", Account: "Alpha", KeyID: "key-1", KeyName: "Primary", Groups: []string{"group-a"}, Model: "codex-a", Endpoint: "responses", Status: "error", HTTPCode: 429, Stream: true, Error: "Rate limit exceeded Bearer sensitive-token-here", LatencyMs: 100},
	}
	for _, r := range rows {
		m.Record(r)
	}
	if m.HealthError() != nil {
		t.Fatal(m.HealthError())
	}
	return m
}
func managedQuery(t *testing.T, raw string) managedLogQuery {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseManagedLogQuery(v)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func exerciseManagedUsage(t *testing.T, pg bool) {
	m := managedUsageFixture(t, pg)
	ctx := context.Background()
	q := managedQuery(t, "group_id=group-a&key_id=key-1&endpoint=responses&stream=true&exact_total=true&page_size=1&sort_by=latency&sort_order=asc")
	first, err := m.managedLogs(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Items) != 1 || first.Items[0].EventID != "event-b" || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	// The snapshot excludes records appended after the first page.
	m.Record(RequestRecord{EventID: "new-after-page", Time: 1700000002, Account: "Alpha", KeyID: "key-1", Groups: []string{"group-a"}, Endpoint: "responses", Stream: true, Status: "ok", LatencyMs: 150})
	q.Cursor = first.NextCursor
	second, err := m.managedLogs(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || second.Items[0].EventID != "event-d" || !second.HasMore {
		t.Fatalf("second page: %+v", second)
	}
	q.Cursor = second.NextCursor
	last, err := m.managedLogs(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if last.Items[0].EventID != "event-a" || last.HasMore {
		t.Fatalf("last page: %+v", last)
	}
	q.Model = "different"
	if _, err = m.managedLogs(ctx, q); err == nil {
		t.Fatal("cursor reused across different filters")
	}
	q = managedQuery(t, "account_id=acc-1&key_id=key-1&group_id=group-a&status=failed&http_code=200&error_category=overloaded&stream=true")
	page, err := m.managedLogs(ctx, q)
	if err != nil || len(page.Items) != 1 || page.Items[0].EventID != "event-b" {
		t.Fatalf("failure filters %+v %v", page, err)
	}
	stats, err := m.managedLogStats(ctx, q)
	if err != nil || stats.Requests != 1 || stats.Errors != 1 || stats.Success != 0 || stats.Tokens != 0 || (stats.AverageLatency == nil || *stats.AverageLatency != 100) {
		t.Fatalf("same-filter stats %+v %v", stats, err)
	}
	var export bytes.Buffer
	n, err := m.exportManagedLogs(ctx, q, "jsonl", &export)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var row map[string]any
	if json.Unmarshal(bytes.TrimSpace(export.Bytes()), &row) != nil || row["input_tokens"] != float64(0) || row["status"] != "error" {
		t.Fatalf("zero/status fidelity: %s", export.String())
	}
	detail, err := m.managedLogDetail(ctx, "event-b")
	if err != nil || detail.Status != "error" {
		t.Fatal(detail, err)
	}
	q = managedQuery(t, "model=codex-b")
	export.Reset()
	n, err = m.exportManagedLogs(ctx, q, "csv", &export)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	values, err := csv.NewReader(&export).ReadAll()
	if err != nil || len(values) != 2 || !strings.HasPrefix(values[1][4], "'=") {
		t.Fatalf("CSV injection: %v %v", values, err)
	}
	q = managedQuery(t, "http_code=429")
	export.Reset()
	_, err = m.exportManagedLogs(ctx, q, "jsonl", &export)
	if err != nil || strings.Contains(export.String(), "sensitive-token-here") {
		t.Fatal("export leaked credentials", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.managedLogs(ctx, managedQuery(t, "")); err == nil {
		t.Fatal("canceled query succeeded")
	}
}
func TestManagedUsageFileQueryStatsExport(t *testing.T) { exerciseManagedUsage(t, false) }
func TestPGManagedUsageQueryStatsExport(t *testing.T)   { exerciseManagedUsage(t, true) }

func TestManagedUsageQueryValidation(t *testing.T) {
	for _, raw := range []string{"from=-1", "from=200&to=100", "stream=maybe", "sort_by=created_at%3BDROP%20TABLE%20request_logs", "sort_order=sideways", "page_size=501", "http_code=700", "exact_total=perhaps", "error_category=not-real", "cursor=garbage", "page=101"} {
		v, _ := url.ParseQuery(raw)
		if _, err := parseManagedLogQuery(v); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	q := managedQuery(t, "model=quote%27OR%201%3D1&error_category=rate_limit")
	sql, args := q.whereSQL()
	if strings.Contains(sql, "quote'OR") || strings.Contains(sql, "%!") {
		t.Fatal("unsafe or corrupt query", sql)
	}
	if len(args) != 2 || args[0] != "quote'OR 1=1" || args[1] != "rate_limit" {
		t.Fatal(args)
	}
	if strings.Contains(sql, managedLogFilterProjection) {
		t.Fatal("native model predicate should not decode unrelated JSON fields")
	}
	if !strings.Contains(sql, managedErrorCategoryProjection+"=$2") {
		t.Fatal("classification must remain an exact parameterized scalar predicate", sql)
	}
}

func TestManagedUsageFileScansBeyondLegacySuffix(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	encoder := json.NewEncoder(m.logFile)
	for i := 0; i < logsScanCap+3; i++ {
		model := "common"
		if i == 0 {
			model = "first-record"
		}
		if err = encoder.Encode(RequestRecord{Time: int64(1700000000 + i), EventID: "synthetic", Model: model, Status: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	q := managedQuery(t, "model=first-record&exact_total=true")
	page, err := m.managedLogs(context.Background(), q)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != 1 {
		t.Fatalf("silently truncated oldest rows %+v %v", page, err)
	}
}

func TestManagedUsageHandlersAuthenticationAndExport(t *testing.T) {
	p, handler := webTestSetup(t)
	p.Metrics.Record(RequestRecord{Time: time.Now().Unix(), EventID: "handler-event", Status: "error", HTTPCode: 200, Error: "Our servers are currently overloaded."})
	for _, path := range []string{"/api/logs?exact_total=true", "/api/logs/stats", "/api/logs/handler-event", "/api/logs/export?format=csv&redacted=false"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
		r = httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+webTestKey)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("authenticated %s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(path, "export") && (w.Header().Get("X-Export-Count") != "1" || !strings.Contains(w.Body.String(), "overloaded")) {
			t.Fatal("bad download", w.Header(), w.Body.String())
		}
	}
}
