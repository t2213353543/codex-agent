package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// One isolated DB/schema and fresh reader verify optional compatibility
// observations, without turning local failures or unknown usage into zeros.
func TestPGBPSRequestCompatibilityObservationRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	prepared, err := bps.Prepare([]byte(`{"model":"gpt-6-astra","input":[],"tools":[],"text":{"format":{"type":"json_object"}}}`), bps.Options{TaskID: "pg-compat-task", TurnID: "pg-compat-turn", StructuredOutput: true})
	if err != nil || prepared.Structured == nil {
		t.Fatal("validated structured fixture unavailable")
	}
	formatter := prepared.Structured.ToolName
	output := []any{
		map[string]any{"type": "function_call", "id": "fc_format_a", "call_id": "call_format_a", "name": formatter, "arguments": `{"value":"PRIVATE_FORMAT_A"}`},
		map[string]any{"type": "function_call", "id": "fc_format_b", "call_id": "call_format_b", "name": formatter, "arguments": `{"value":"PRIVATE_FORMAT_B"}`},
	}
	frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_pg_mixed", "status": "completed", "output": output}})
	_, mixedErr := bps.StreamStructured(context.Background(), io.Discard, strings.NewReader("data: "+string(frame)+"\n\n"), bps.StreamOptions{Unbounded: true}, prepared.Structured)
	mixed := bps.ObserveStructuredMixed(mixedErr)
	if mixed == nil || mixed.FormatterCount != 2 || mixed.ClientCallCount != 0 || mixed.OtherCallCount != 0 || mixed.HasText || mixed.HasRefusal {
		t.Fatal("actual mixed rejection did not produce finite expected metadata")
	}
	policy := bps.ObserveToolPolicy(prepared)
	rows := []RequestRecord{
		{EventID: "pg-compat-sse-rejected", Status: "error", HTTPCode: 400, Stream: true, RequestType: "http_sse", ErrorParam: bpsObservedErrorParameter("stream_options.reasoning_summary_delivery"), Error: "synthetic rejected parameter", ErrorSource: "client", ErrorPhase: "request_validation"},
		{EventID: "pg-compat-ws-mixed", Status: "error", HTTPCode: 502, Stream: true, RequestType: "websocket", StructuredMixed: mixed, ToolPolicy: policy, Error: mixedErr.Error(), ErrorSource: "upstream", ErrorPhase: "bps_protocol"},
		{EventID: "pg-compat-legacy", Status: "error", HTTPCode: 400, RequestType: "websocket", Error: "synthetic legacy websocket rejection", ErrorSource: "client", ErrorPhase: "request_validation"},
	}
	want := make(map[string]RequestRecord, len(rows))
	for i := range rows {
		rows[i].Time, rows[i].Model, rows[i].Endpoint = base+int64(i), "pg-request-compatibility", "responses"
		rows[i].ObservationSchemaVersion = 2
		want[rows[i].EventID] = rows[i]
		writer.Record(rows[i])
	}
	if err := writer.HealthError(); err != nil {
		t.Fatal(err)
	}
	reader := newTestPGMetrics(t, db)
	assertRecord := func(got, expected RequestRecord) {
		t.Helper()
		if got.ErrorParam != expected.ErrorParam || got.RequestType != expected.RequestType || got.Stream != expected.Stream || !reflect.DeepEqual(got.StructuredMixed, expected.StructuredMixed) || !reflect.DeepEqual(got.ToolPolicy, expected.ToolPolicy) {
			t.Fatal("compatibility observation changed across persistence/export")
		}
		if got.UsageKnown != nil || got.CachedTokensKnown != nil || got.CacheWriteTokens != nil || got.InputTokens != 0 || got.OutputTokens != 0 || got.CachedTokens != 0 || got.SentModel != "" || got.ReturnedModel != "" || len(got.Attempts) != 0 {
			t.Fatal("unobserved usage or dispatch was fabricated")
		}
	}
	assertJSON := func(raw []byte, expected RequestRecord) {
		t.Helper()
		var got RequestRecord
		if json.Unmarshal(raw, &got) != nil {
			t.Fatal("record JSON invalid")
		}
		assertRecord(got, expected)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if expected.ErrorParam == "" && fields["error_param"] != nil {
			t.Fatal("legacy missing error_param filled in")
		}
		if expected.StructuredMixed == nil {
			if fields["structured_mixed"] != nil {
				t.Fatal("unknown mixed metadata filled in")
			}
		} else {
			var value map[string]json.RawMessage
			if json.Unmarshal(fields["structured_mixed"], &value) != nil || len(value) != 5 || string(value["formatter_count"]) != "2" || string(value["client_call_count"]) != "0" || string(value["other_call_count"]) != "0" || string(value["has_text"]) != "false" || string(value["has_refusal"]) != "false" {
				t.Fatal("finite mixed shape or known zero/false lost")
			}
			var tool map[string]json.RawMessage
			if json.Unmarshal(fields["tool_policy"], &tool) != nil || string(tool["catalog_at_prepare_count"]) != "0" || string(tool["tool_search_at_prepare"]) != "false" || string(tool["structured"]) != "true" {
				t.Fatal("tool policy coexistence/zero/false lost")
			}
		}
		if bytes.Contains(raw, []byte("PRIVATE_FORMAT")) || bytes.Contains(raw, []byte(formatter)) {
			t.Fatal("private formatter identity/arguments leaked")
		}
	}
	for _, expected := range rows {
		var raw []byte
		if err := db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", expected.EventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		assertJSON(raw, expected)
		stored, err := reader.managedLogDetail(t.Context(), expected.EventID)
		if err != nil {
			t.Fatal(err)
		}
		assertRecord(stored, expected)
		for _, redacted := range []bool{false, true} {
			encoded, _ := json.Marshal(managedExportRecord(stored, redacted))
			assertJSON(encoded, expected)
		}
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&model=pg-request-compatibility&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", base, base+60))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 {
		t.Fatal("fresh PG compatibility query lost records")
	}
	for _, item := range page.Items {
		expected, ok := want[item.EventID]
		if !ok {
			t.Fatal("unexpected record")
		}
		raw, _ := json.Marshal(item)
		assertJSON(raw, expected)
	}
	stats, err := reader.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 3 || stats.Errors != 3 || stats.UsageKnownRequests != 0 || stats.UsageUnknownRequests != 3 || stats.CacheReadSamples != 0 || stats.CacheReadUnknownRequests != 3 || stats.CacheWriteSamples != 0 || stats.CacheWriteTokens != nil || stats.Input != 0 || stats.Output != 0 {
		t.Fatal("unknown compatibility usage became known zero/read/write")
	}
	for _, redacted := range []bool{false, true} {
		var output bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "jsonl", &output, redacted)
		if err != nil || count != 3 {
			t.Fatal("JSONL export failed")
		}
		lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatal("JSONL count changed")
		}
		for i, raw := range lines {
			assertJSON(raw, rows[i])
		}
	}
	var csvOutput bytes.Buffer
	count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "csv", &csvOutput, false)
	if err != nil || count != 3 || managedUsageExportSchema != "codex-usage-v4" {
		t.Fatal("CSV schema/count changed")
	}
	csvRows, err := csv.NewReader(&csvOutput).ReadAll()
	if err != nil || len(csvRows) != 4 || len(csvRows[0]) != 77 {
		t.Fatal("CSV compatibility columns changed")
	}
	columns := map[string]int{}
	for i, name := range csvRows[0] {
		columns[name] = i
		if name == "error_param" || name == "structured_mixed" || name == "tool_policy" {
			t.Fatal("passive metadata appended CSV column")
		}
	}
	for _, row := range csvRows[1:] {
		if len(row) != 77 {
			t.Fatal("CSV row width changed")
		}
		for _, name := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens"} {
			if row[columns[name]] != "" {
				t.Fatal("CSV unknown usage rendered as measured zero")
			}
		}
	}
	t.Log("isolated PostgreSQL: three compatibility records; exact early SSE/WS types, fixed parameter/legacy omit, actual mixed five fields and tool policy coexistence, fresh detail/query/private+redacted JSONL, unknown read/write retained, CSV v4/77 unchanged")
}
