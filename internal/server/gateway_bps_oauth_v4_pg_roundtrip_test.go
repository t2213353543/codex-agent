package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// One real PostgreSQL fixture, isolated by newMetricsTestDB's random schema.
// No upstream, account credentials, cooldown settings, or production data.
func TestPGBPSOAuthActualAttemptsRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	logical429, http200, http429, http400, logical503 := 429, 200, 429, 400, 503
	header, duration := int64(2), int64(5)
	yes, no := true, false
	rows := []RequestRecord{
		{EventID: "v4-pg-sse-limit", HTTPCode: 200, InputTokens: 100, OutputTokens: 10, UsageKnown: &yes,
			FinalUpstreamErrorCode: "rateLimitExceeded", FinalUpstreamErrorSource: "upstream",
			Attempts: []GatewayAttemptSummary{{Attempt: 1, Phase: "request", Endpoint: "/responses", StatusCode: &logical429, ObservedHTTPStatusCode: &http200, HeaderMs: &header, DurationMs: &duration, ErrorCode: "rateLimitExceeded", ErrorSource: "upstream"}}},
		{EventID: "v4-pg-prior-limit-refresh", HTTPCode: 429, InputTokens: 20, UsageKnown: &yes,
			FinalUpstreamErrorCode: "rateLimitExceeded", FinalUpstreamErrorSource: "upstream",
			Attempts: []GatewayAttemptSummary{
				{Attempt: 1, Phase: "request", Endpoint: "/responses", StatusCode: &logical429, ObservedHTTPStatusCode: &http429, HeaderMs: &header, DurationMs: &duration, ErrorCode: "rateLimitExceeded", ErrorSource: "upstream"},
				{Attempt: 2, Phase: "refresh", Endpoint: "/oauth/token", StatusCode: &http400, ObservedHTTPStatusCode: &http400, HeaderMs: &header, DurationMs: &duration, ErrorCode: "http_status", ErrorSource: "upstream"},
			}},
		{EventID: "v4-pg-headers-unknown", HTTPCode: 503, UsageKnown: &no,
			Attempts: []GatewayAttemptSummary{{Attempt: 1, Phase: "request", Endpoint: "/responses", StatusCode: &logical503, DurationMs: &duration, ErrorCode: "transport", ErrorSource: "transport"}}},
	}
	want := make(map[string]RequestRecord, len(rows))
	for index, row := range rows {
		row.Time, row.Model, row.Endpoint, row.Status = base+int64(index), "v4-pg-fixture", "responses", "error"
		row.ObservationSchemaVersion, row.AttemptsTruncated = 2, &no
		row.Error = "synthetic bounded observation"
		want[row.EventID] = row
		writer.Record(row)
	}
	if err := writer.HealthError(); err != nil {
		t.Fatal(err)
	}
	// All reads use a fresh Metrics instance and real PostgreSQL queries, not
	// the writer's recent ring; raw JSONB is checked separately below.
	reader := newTestPGMetrics(t, db)
	assertRecord := func(got RequestRecord) {
		t.Helper()
		expected, exists := want[got.EventID]
		if !exists {
			t.Fatalf("unexpected event: %q", got.EventID)
		}
		if got.ObservationSchemaVersion != 2 || got.FinalUpstreamErrorCode != expected.FinalUpstreamErrorCode || got.FinalUpstreamErrorSource != expected.FinalUpstreamErrorSource || !reflect.DeepEqual(got.Attempts, expected.Attempts) || !reflect.DeepEqual(got.AttemptsTruncated, expected.AttemptsTruncated) {
			t.Fatalf("PG logical/observed/final provenance changed for %s: got=%+v want=%+v", got.EventID, got, expected)
		}
	}
	assertJSON := func(raw []byte) {
		t.Helper()
		var got RequestRecord
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		assertRecord(got)
		if got.EventID == "v4-pg-headers-unknown" && (bytes.Contains(raw, []byte("observed_http_status_code")) || bytes.Contains(raw, []byte("final_upstream_error_code")) || bytes.Contains(raw, []byte("final_upstream_error_source"))) {
			t.Fatalf("unknown headers/final generation invented JSON fields: %s", raw)
		}
	}
	for _, expected := range rows {
		var raw []byte
		if err := db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", expected.EventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		assertJSON(raw)
		stored, err := reader.managedLogDetail(t.Context(), expected.EventID)
		if err != nil {
			t.Fatal(err)
		}
		assertRecord(stored)
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&model=v4-pg-fixture&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", base, base+60))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.HasMore {
		t.Fatalf("PG query cardinality: total=%d rows=%d err=%v", page.Total, len(page.Items), err)
	}
	for _, item := range page.Items {
		encoded, _ := json.Marshal(item)
		assertJSON(encoded)
	}
	stats, err := reader.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 3 || stats.Errors != 3 || stats.Input != 120 || stats.Output != 10 || stats.Tokens != 130 || stats.UsageKnownRequests != 2 || stats.UsageUnknownRequests != 1 {
		t.Fatalf("network ledger changed request/usage aggregates: %+v err=%v", stats, err)
	}
	filtered := q
	filtered.HTTPCode = 429
	filteredPage, err := reader.managedLogs(t.Context(), filtered)
	if err != nil || filteredPage.Total != 1 || len(filteredPage.Items) != 1 || filteredPage.Items[0].EventID != "v4-pg-prior-limit-refresh" {
		t.Fatalf("request HTTP filter confused nested observed status: %+v err=%v", filteredPage, err)
	}
	assertRecord(filteredPage.Items[0].RequestRecord)
	filteredStats, err := reader.managedLogStats(t.Context(), filtered)
	if err != nil || filteredStats.Requests != 1 || filteredStats.Errors != 1 || filteredStats.Input != 20 {
		t.Fatalf("filtered stats changed request boundary: %+v err=%v", filteredStats, err)
	}
	for _, format := range []string{"jsonl", "csv"} {
		var output bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, format, &output, false)
		if err != nil || count != 3 {
			t.Fatalf("PG export %s: count=%d err=%v", format, count, err)
		}
		if format == "jsonl" {
			lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
			if len(lines) != 3 {
				t.Fatal("JSONL rows changed")
			}
			for _, raw := range lines {
				assertJSON(raw)
			}
			continue
		}
		csvRows, err := csv.NewReader(&output).ReadAll()
		if err != nil || len(csvRows) != 4 {
			t.Fatalf("CSV rows changed: rows=%d err=%v", len(csvRows), err)
		}
		columns := map[string]int{}
		for i, name := range csvRows[0] {
			columns[name] = i
		}
		for _, name := range []string{"event_id", "attempts", "attempts_truncated", "observation_schema_version", "final_upstream_error_code", "final_upstream_error_source"} {
			if _, exists := columns[name]; !exists {
				t.Fatalf("missing CSV column %s", name)
			}
		}
		for _, values := range csvRows[1:] {
			got := RequestRecord{EventID: values[columns["event_id"]], FinalUpstreamErrorCode: values[columns["final_upstream_error_code"]], FinalUpstreamErrorSource: values[columns["final_upstream_error_source"]]}
			got.ObservationSchemaVersion, err = strconv.Atoi(values[columns["observation_schema_version"]])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(values[columns["attempts"]]), &got.Attempts); err != nil {
				t.Fatal(err)
			}
			truncated, err := strconv.ParseBool(values[columns["attempts_truncated"]])
			if err != nil {
				t.Fatal(err)
			}
			got.AttemptsTruncated = &truncated
			assertRecord(got)
		}
	}
	t.Log("isolated PostgreSQL: 3 request records, 4 network attempts, schema2, logical429/observed200, prior429+refresh400, unknown headers nil, raw JSONB/detail/query/stats/JSONL/CSV round-trip PASS")
}
