package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// Real native compact rejection records cross the existing isolated PG store.
// The only upstream is httptest; no record or usage outcome is hand-constructed.
func TestPGBPSCompactFailureUsageRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	since := time.Now().Add(-time.Minute).Unix()
	type expectation struct {
		record RequestRecord
		read   int64
		write  *int64
		known  bool
	}
	var expected []expectation
	for _, fixture := range []struct {
		name        string
		details     json.RawMessage
		read, write int64
		known       bool
	}{
		{"positive", json.RawMessage(`{"cached_tokens":750,"cache_write_tokens":250}`), 750, 250, true},
		{"zero", json.RawMessage(`{"cached_tokens":0,"cache_write_tokens":0}`), 0, 0, true},
		{"missing", nil, 0, 0, false},
	} {
		responseID := "compact_pg_" + fixture.name
		usage := map[string]any{"input_tokens": 1000, "output_tokens": 11, "total_tokens": 1011}
		if fixture.details != nil {
			usage["input_tokens_details"] = fixture.details
		}
		raw, err := json.Marshal(map[string]any{"id": responseID, "model": "gpt-6-astra", "status": "failed", "error": map[string]any{"type": "server_error", "code": nil, "message": "synthetic native compaction failure"}, "output": []any{}, "usage": usage})
		if err != nil {
			t.Fatal(err)
		}
		posts := 0
		p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
			posts++
			if r.Method != http.MethodPost || r.URL.Path != "/responses/compact" {
				t.Error("unexpected compact fixture route")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		})
		out := httptest.NewRecorder()
		p.gatewayBPS(out, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":[]}`))
		record, ok := p.Metrics.LastFor("test")
		if !ok || posts != 1 || out.Code != http.StatusBadGateway || record.Status != "error" || record.HTTPCode != http.StatusBadGateway || record.TerminalStatus != "failed" || record.ErrorSource != "upstream" {
			t.Fatalf("native compact failure did not preserve one rejected request: case=%s http=%d posts=%d", fixture.name, out.Code, posts)
		}
		if record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != 1000 || record.OutputTokens != 11 || record.CachedTokens != fixture.read || record.CachedTokensKnown == nil || *record.CachedTokensKnown != fixture.known || record.ReturnedModel != "gpt-6-astra" || record.ResponseID != responseID || record.TTFTMs != nil {
			t.Fatalf("real compact accounting or response provenance missing for %s", fixture.name)
		}
		var wantWrite *int64
		if fixture.known {
			value := fixture.write
			wantWrite = &value
			if record.CacheWriteTokens == nil || *record.CacheWriteTokens != value {
				t.Fatal("explicit compact cache creation lost")
			}
		} else if record.CacheWriteTokens != nil {
			t.Fatal("unreported cache creation was invented as zero")
		}
		if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", responseID); err == nil {
			t.Fatal("failed compact response became resumable history")
		}
		// prepareMetricRecord supplies only storage identity/redaction to the actual
		// gateway record, using the same preparation as the PG writer.
		record = prepareMetricRecord(record)
		writer.Record(record)
		expected = append(expected, expectation{record: record, read: fixture.read, write: wantWrite, known: fixture.known})
	}
	if err := writer.HealthError(); err != nil {
		t.Fatal(err)
	}
	reader := newTestPGMetrics(t, db)
	want := map[string]expectation{}
	for _, value := range expected {
		want[value.record.EventID] = value
	}
	assertRecord := func(got RequestRecord, value expectation) {
		t.Helper()
		if got.Status != "error" || got.HTTPCode != 502 || got.TerminalStatus != "failed" || got.ErrorSource != "upstream" || got.ErrorPhase != value.record.ErrorPhase || got.RequestType != value.record.RequestType || got.ResponseID != value.record.ResponseID || got.ReturnedModel != "gpt-6-astra" || got.TTFTMs != nil {
			t.Fatal("PG reload changed compact failure/provenance/delivery boundary")
		}
		if got.UsageKnown == nil || !*got.UsageKnown || got.InputTokens != 1000 || got.OutputTokens != 11 || got.CachedTokens != value.read || got.CachedTokensKnown == nil || *got.CachedTokensKnown != value.known || !reflect.DeepEqual(got.CacheWriteTokens, value.write) {
			t.Fatal("PG reload conflated cache reads/creation/unknown fields")
		}
	}
	assertJSON := func(raw []byte, value expectation) {
		t.Helper()
		var got RequestRecord
		if json.Unmarshal(raw, &got) != nil {
			t.Fatal("compact persisted/exported JSON invalid")
		}
		assertRecord(got, value)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if value.write == nil && fields["cache_write_tokens"] != nil {
			t.Fatal("unknown creation appeared in JSON")
		}
		if value.write != nil && *value.write == 0 && string(fields["cache_write_tokens"]) != "0" {
			t.Fatal("known zero creation omitted")
		}
	}
	for _, value := range expected {
		var raw []byte
		if err := db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", value.record.EventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		assertJSON(raw, value)
		stored, err := reader.managedLogDetail(t.Context(), value.record.EventID)
		if err != nil {
			t.Fatal(err)
		}
		assertRecord(stored, value)
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&endpoint=responses%%2Fcompact&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", since, time.Now().Add(time.Minute).Unix()))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.HasMore {
		t.Fatal("fresh PG compact failure query cardinality changed")
	}
	for _, item := range page.Items {
		value, exists := want[item.EventID]
		if !exists {
			t.Fatal("unexpected persisted compact record")
		}
		encoded, _ := json.Marshal(item)
		assertJSON(encoded, value)
	}
	stats, err := reader.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 3 || stats.Success != 0 || stats.Errors != 3 || stats.Aborted != 0 || stats.Input != 3000 || stats.Output != 33 || stats.Tokens != 3033 || stats.Cached != 750 || stats.UsageKnownRequests != 3 || stats.UsageUnknownRequests != 0 || stats.TTFTSamples != 0 {
		t.Fatal("real failed compaction usage became success/unknown or double counted")
	}
	if stats.CacheReadSamples != 2 || stats.CacheReadUnknownRequests != 1 || stats.CacheReadInputTokens != 2000 || stats.CacheReadTokens != 750 || stats.CacheReadRate == nil || *stats.CacheReadRate != 0.375 || stats.CacheWriteSamples != 2 || stats.Requests-stats.CacheWriteSamples != 1 || stats.CacheWriteTokens == nil || *stats.CacheWriteTokens != 250 {
		t.Fatal("cache ratio must be 750/2000=37.5%, with independent creation250 and one unknown")
	}
	for _, filter := range []struct {
		status     string
		http, want int
	}{
		{"failed", 502, 3}, {"error", 502, 3}, {"ok", 0, 0},
	} {
		filtered := q
		filtered.Status, filtered.HTTPCode = filter.status, filter.http
		result, err := reader.managedLogs(t.Context(), filtered)
		if err != nil || result.Total != int64(filter.want) || len(result.Items) != filter.want {
			t.Fatal("failed compaction status/http filter changed")
		}
	}
	for _, redacted := range []bool{false, true} {
		var output bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "jsonl", &output, redacted)
		if err != nil || count != 3 {
			t.Fatal("compact JSONL export failed")
		}
		lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatal("compact JSONL row count changed")
		}
		for index, raw := range lines {
			value := expected[index]
			if redacted {
				originalID := value.record.ResponseID
				value.record.ResponseID = managedExportPseudonym("response", originalID)
				if bytes.Contains(raw, []byte(originalID)) {
					t.Fatal("redacted export leaked original response ID")
				}
			}
			assertJSON(raw, value)
		}
	}
	var csvOutput bytes.Buffer
	count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "csv", &csvOutput, false)
	if err != nil || count != 3 || managedUsageExportSchema != "codex-usage-v4" {
		t.Fatal("compact CSV schema/export changed")
	}
	csvRows, err := csv.NewReader(&csvOutput).ReadAll()
	if err != nil || len(csvRows) != 4 || len(csvRows[0]) != 77 {
		t.Fatal("compact CSV v4 77-column contract changed")
	}
	columns := map[string]int{}
	for index, name := range csvRows[0] {
		columns[name] = index
	}
	for _, row := range csvRows[1:] {
		value, exists := want[row[columns["event_id"]]]
		if !exists || len(row) != 77 {
			t.Fatal("compact CSV identity or width changed")
		}
		if row[columns["status"]] != "error" || row[columns["http_code"]] != "502" || row[columns["input_tokens"]] != "1000" || row[columns["output_tokens"]] != "11" || row[columns["ttft_ms"]] != "" || row[columns["returned_model"]] != "gpt-6-astra" {
			t.Fatal("CSV changed real accounting or delivered timing")
		}
		readValue, writeValue := "", ""
		if value.known {
			readValue = fmt.Sprint(value.read)
			writeValue = fmt.Sprint(*value.write)
		}
		if row[columns["cached_tokens"]] != readValue || row[columns["cache_write_tokens"]] != writeValue {
			t.Fatal("CSV unknown cache must be blank; explicit zero must remain 0")
		}
	}
	t.Log("isolated PostgreSQL: three real native compact failed records; all3error/0success, input3000/output33, read750/knowninput2000=37.5%, write250 with two known/one unknown, no history/TTFT, returned model and JSONL/CSV zero-versus-blank preserved")
}
