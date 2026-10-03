package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func severityUsageRows() ([]RequestRecord, map[string]string) {
	rows := []RequestRecord{
		{EventID: "severity-overloaded", Status: "error", HTTPCode: 200, Error: "Our servers are currently overloaded"},
		{EventID: "severity-rate", Status: "error", HTTPCode: 200, Error: "rate limit exceeded"},
		{EventID: "severity-server", Status: "error", HTTPCode: 502, Error: "synthetic backend failure"},
		{EventID: "severity-size", Status: "error", HTTPCode: 413, Error: "synthetic too large"},
		{EventID: "severity-bad", Status: "error", HTTPCode: 400, Error: "synthetic invalid body"},
		{EventID: "severity-parameter", Status: "error", HTTPCode: 503, Error: "Unsupported parameter: fixture"},
		{EventID: "severity-business", Status: "error", HTTPCode: 200, Error: "synthetic business failure"},
		{EventID: "severity-auth", Status: "error", HTTPCode: 401, Error: "synthetic unauthorized"},
		{EventID: "severity-missing-http", Status: "error", HTTPCode: 0, Error: "rate limit exceeded"},
		{EventID: "severity-invalid-http", Status: "aborted", HTTPCode: 999, Error: "synthetic invalid historical status"},
		{EventID: "severity-success", Status: "ok", HTTPCode: 200},
	}
	levels := []string{"P1", "P1", "P1", "P2", "P2", "P3", "P3", "P3", "unknown", "unknown", ""}
	want := map[string]string{}
	for i := range rows {
		rows[i].Time = 1700000000 + int64(i)
		rows[i].Model = "severity-fixture"
		rows[i].InputTokens = 10
		rows[i].OutputTokens = 2
		want[rows[i].EventID] = levels[i]
	}
	return rows, want
}

func TestManagedUsageSeverity(t *testing.T)   { exerciseManagedUsageSeverity(t, webTestSetup) }
func TestPGManagedUsageSeverity(t *testing.T) { exerciseManagedUsageSeverity(t, adminPGTestProxy) }

func exerciseManagedUsageSeverity(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	rows, want := severityUsageRows()
	for _, record := range rows {
		p.Metrics.Record(record)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	var originalFile []byte
	if p.Store.DB == nil {
		var err error
		originalFile, err = os.ReadFile(p.Metrics.logPath())
		if err != nil {
			t.Fatal(err)
		}
	}
	checkRecord := func(record managedLogRecord) {
		t.Helper()
		expected, exists := want[record.EventID]
		if !exists || record.ErrorSeverity != expected {
			t.Fatal("severity mismatch", record.EventID, record.ErrorSeverity, expected)
		}
		if expected == "" {
			if record.ErrorSeveritySource != "" {
				t.Fatal("success has severity source")
			}
		} else if record.ErrorSeveritySource != managedErrorSeveritySource {
			t.Fatal("missing management provenance")
		}
	}
	decodePage := func(raw []byte) managedLogPage {
		t.Helper()
		var page managedLogPage
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	response := managedTestRequest(t, h, "GET", "/api/logs?model=severity-fixture&exact_total=true", nil, 200)
	all := decodePage(response.Body.Bytes())
	if all.Total != int64(len(rows)) {
		t.Fatal("all count", all.Total)
	}
	for _, record := range all.Items {
		checkRecord(record)
		if record.EventID == "severity-overloaded" && record.HTTPCode != 200 {
			t.Fatal("severity changed actual HTTP status")
		}
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/severity-success", nil, 200)
	if strings.Contains(response.Body.String(), "error_severity") {
		t.Fatal("success detail contains severity")
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?model=severity-fixture", nil, 200)
	var allStats managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &allStats) != nil || allStats.ErrorSeveritySource != managedErrorSeveritySource || allStats.Requests != 11 || allStats.Errors != 10 {
		t.Fatal("severity stats totals")
	}
	names := []string{}
	counts := []int64{}
	for _, dimension := range allStats.ErrorSeverities {
		names = append(names, dimension.Name)
		counts = append(counts, dimension.Requests)
		if dimension.Success != 0 {
			t.Fatal("success entered failure dimension")
		}
	}
	if !reflect.DeepEqual(names, []string{"P1", "P2", "P3", "unknown"}) || !reflect.DeepEqual(counts, []int64{3, 2, 3, 2}) {
		t.Fatal("severity distribution", names, counts)
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?model=severity-fixture&status=ok", nil, 200)
	var successStats managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &successStats) != nil || len(successStats.ErrorSeverities) != 0 || successStats.ErrorSeveritySource != managedErrorSeveritySource {
		t.Fatal("success-only severity contract")
	}
	for _, level := range []string{"P1", "P2", "P3", "unknown"} {
		t.Run(level, func(t *testing.T) {
			values := url.Values{"model": {"severity-fixture"}, "error_severity": {level}, "page_size": {"1"}, "exact_total": {"true"}, "sort_order": {"asc"}}
			var found []string
			cursor := ""
			for {
				if cursor != "" {
					values.Set("cursor", cursor)
				}
				page := decodePage(managedTestRequest(t, h, "GET", "/api/logs?"+values.Encode(), nil, 200).Body.Bytes())
				for _, record := range page.Items {
					checkRecord(record)
					if record.ErrorSeverity != level {
						t.Fatal("filter broadened")
					}
					found = append(found, record.EventID)
				}
				if !page.HasMore {
					break
				}
				cursor = page.NextCursor
			}
			count := int64(0)
			for _, expected := range want {
				if expected == level {
					count++
				}
			}
			if int64(len(found)) != count {
				t.Fatal("severity pagination count")
			}
			if cursor != "" {
				values.Set("cursor", cursor)
				values.Set("error_severity", "P1")
				if level == "P1" {
					values.Set("error_severity", "P2")
				}
				managedTestRequest(t, h, "GET", "/api/logs?"+values.Encode(), nil, 400)
			}
			query := "model=severity-fixture&error_severity=" + level
			r := managedTestRequest(t, h, "GET", "/api/logs/stats?"+query, nil, 200)
			var stats managedUsageStats
			if json.Unmarshal(r.Body.Bytes(), &stats) != nil || stats.Requests != count || len(stats.ErrorSeverities) != 1 || stats.ErrorSeverities[0].Name != level {
				t.Fatal("filtered stats disagree")
			}
			r = managedTestRequest(t, h, "GET", "/api/logs/errors?"+query, nil, 200)
			var errorsView struct {
				Total int64 `json:"total"`
				Items []struct {
					Record managedLogRecord `json:"record"`
				} `json:"items"`
			}
			if json.Unmarshal(r.Body.Bytes(), &errorsView) != nil || errorsView.Total != count {
				t.Fatal("error review severity filter")
			}
			for _, item := range errorsView.Items {
				checkRecord(item.Record)
			}
			r = managedTestRequest(t, h, "GET", "/api/logs/export?format=jsonl&redacted=false&"+query, nil, 200)
			lines := bytes.Split(bytes.TrimSpace(r.Body.Bytes()), []byte("\n"))
			if int64(len(lines)) != count {
				t.Fatal("JSONL severity count")
			}
			for _, line := range lines {
				var record managedLogRecord
				if json.Unmarshal(line, &record) != nil {
					t.Fatal("invalid JSONL")
				}
				checkRecord(record)
			}
		})
	}
	for _, bad := range []string{"p1", "P0", "P4", "UNKNOWN", "null", "P1%27%20OR%201=1", "%00", "P1&error_severity=P2"} {
		managedTestRequest(t, h, "GET", "/api/logs?error_severity="+bad, nil, 400)
	}
	managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": map[string]string{"error_severity": "p1"}}, 400)
	if _, err := managedCleanupQuery(map[string]string{"from": "1699999999", "to": "1700000100", "error_severity": "p1"}); err == nil {
		t.Fatal("cleanup accepted invalid severity")
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/export?model=severity-fixture&format=csv&redacted=false&sort_order=asc", nil, 200)
	values, err := csv.NewReader(bytes.NewReader(response.Body.Bytes())).ReadAll()
	if err != nil || len(values) != 12 || len(values[0]) != 77 || values[0][59] != "error_severity" || values[0][60] != "error_severity_source" || values[0][71] != "model_consistency" || values[0][72] != "official_runtime_ms" || values[0][73] != "official_inference_ms" || values[0][74] != "sent_reasoning_effort" || values[0][75] != "final_upstream_error_code" || values[0][76] != "final_upstream_error_source" || response.Header().Get("X-Export-Schema") != "codex-usage-v4" {
		t.Fatal("CSV schema75 retains severity column positions", err)
	}
	for _, record := range values[1:] {
		level := want[record[1]]
		if record[59] != level || level != "" && record[60] != managedErrorSeveritySource || level == "" && record[60] != "" {
			t.Fatal("CSV severity mismatch")
		}
	}
	managedTestRequest(t, h, "PUT", "/api/logs/severity-overloaded/review", map[string]any{"version": 0, "resolved": true, "note": "synthetic reviewed failure"}, 200)
	response = managedTestRequest(t, h, "GET", "/api/logs/errors?model=severity-fixture&error_severity=P1&resolved=true", nil, 200)
	var reviewed struct {
		Total int64 `json:"total"`
	}
	if json.Unmarshal(response.Body.Bytes(), &reviewed) != nil || reviewed.Total != 1 {
		t.Fatal("severity review-state intersection")
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/errors?model=severity-fixture&error_severity=P1&page_size=1", nil, 200)
	var reviewPage struct {
		Cursor string `json:"next_cursor"`
	}
	if json.Unmarshal(response.Body.Bytes(), &reviewPage) != nil || reviewPage.Cursor == "" {
		t.Fatal("review cursor missing")
	}
	managedTestRequest(t, h, "GET", "/api/logs/errors?model=severity-fixture&error_severity=P2&page_size=1&cursor="+url.QueryEscape(reviewPage.Cursor), nil, 409)
	if p.Store.DB == nil {
		raw, _ := os.ReadFile(p.Metrics.logPath())
		if !bytes.Equal(raw, originalFile) {
			t.Fatal("derived severity mutated request log")
		}
	} else {
		var derived int
		err := p.Store.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs WHERE record ? 'error_severity' OR record ? 'error_severity_source'").Scan(&derived)
		if err != nil || derived != 0 {
			t.Fatal("severity stored in RequestRecord", err)
		}
	}
	filters := map[string]string{"model": "severity-fixture", "error_severity": "P1", "from": "1699999999", "to": "1700000100"}
	response = managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Total != 3 {
		t.Fatal("severity snapshot capture")
	}
	p.Metrics.Record(RequestRecord{EventID: "severity-after-snapshot", Time: 1700000020, Model: "severity-fixture", Status: "error", HTTPCode: 503})
	filterValues := url.Values{}
	for k, v := range filters {
		filterValues.Set(k, v)
	}
	filterValues.Set("as_of", snapshot.ID)
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?"+filterValues.Encode(), nil, 200)
	var frozen managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &frozen) != nil || frozen.Requests != 3 || frozen.ErrorSeverities[0].Requests != 3 {
		t.Fatal("severity snapshot grew")
	}
	filterValues.Set("format", "jsonl")
	response = managedTestRequest(t, h, "GET", "/api/logs/export?"+filterValues.Encode(), nil, 200)
	if response.Header().Get("X-Export-Count") != "3" || strings.Contains(response.Body.String(), "severity-after-snapshot") {
		t.Fatal("snapshot export broadened")
	}
	filterValues.Set("error_severity", "P2")
	managedTestRequest(t, h, "GET", "/api/logs?"+filterValues.Encode(), nil, 409)
	if p.Store.DB != nil {
		filters["as_of"] = snapshot.ID
		response = managedTestRequest(t, h, "POST", "/api/logs/cleanup/preview", map[string]any{"filters": filters}, 200)
		var preview managedCleanupPreview
		if json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.Total != 3 || preview.Filters["error_severity"] != "P1" {
			t.Fatal("cleanup severity snapshot")
		}
		response = managedTestRequest(t, h, "POST", "/api/logs/cleanup/jobs", map[string]any{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": "severity-cleanup-proof"}, 200)
		var created struct {
			Job managedCleanupJob `json:"job"`
		}
		if json.Unmarshal(response.Body.Bytes(), &created) != nil {
			t.Fatal("cleanup job")
		}
		if err := p.runManagedCleanupBatch(ctx, created.Job.ID); err != nil {
			t.Fatal(err)
		}
		var remaining, events int64
		if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM request_logs WHERE model='severity-fixture'),(SELECT count(*) FROM usage_events)").Scan(&remaining, &events); err != nil || remaining != 9 || events != 12 {
			t.Fatal("cleanup deleted wrong scope or durable usage", remaining, events, err)
		}
		if _, err := p.Metrics.managedLogDetail(ctx, "severity-after-snapshot"); err != nil {
			t.Fatal("later failure deleted")
		}
	}
}
