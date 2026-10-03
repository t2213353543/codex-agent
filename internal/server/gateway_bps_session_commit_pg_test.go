package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

// One isolated database fixture stores two observations produced by the real
// commit barrier/stream writer. No provider, OAuth or network call is needed.
func TestPGBPSSessionCommitObservationRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	var rows []RequestRecord
	for _, rejected := range []bool{true, false} {
		started := time.Now()
		record := RequestRecord{Status: "ok", RequestType: "http_sse", Stream: true, Model: "pg-session-commit-fixture", Endpoint: "responses"}
		cause := errors.New("session account changed during authentication failover")
		state := &officialFailoverState{started: started, commit: func() error {
			if rejected {
				return cause
			}
			return nil
		}}
		visible := httptest.NewRecorder()
		barrier := &officialFailoverWriter{target: visible, header: make(http.Header), state: state}
		barrier.Header().Set("Content-Type", "text/event-stream")
		streamWriter := &gatewayResponseWriter{ResponseWriter: barrier, record: &record}
		observation := gatewayObservation{record: &record, started: started}
		const frames = "data: " + `{"type":"response.created","response":{"id":"session_commit_fixture","model":"gpt-6-astra","status":"in_progress","output":[]}}` + "\n\n" +
			"data: " + `{"type":"response.output_text.delta","response_id":"session_commit_fixture","delta":"PRIVATE_RESPONSE_TEXT"}` + "\n\n" +
			"data: " + `{"type":"response.completed","response":{"id":"session_commit_fixture","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PRIVATE_RESPONSE_TEXT"}]}],"usage":{"input_tokens":100,"output_tokens":7,"total_tokens":107,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}}}` + "\n\n"
		result, err := bps.Stream(context.Background(), streamWriter, strings.NewReader(frames), bps.StreamOptions{Unbounded: true, OnEvent: func(event []byte) error { observation.eventAt(event, started.Add(7*time.Millisecond)); return nil }})
		if len(result.Response) > 0 {
			recordBPSTerminalUsage(&record, result.Response)
		}
		if rejected {
			if !errors.Is(err, cause) || !observeGatewaySessionCommitError(&record, err) || visible.Body.Len() != 0 || barrier.delivered || state.committed || result.Completed || len(result.Usage) != 0 {
				t.Fatal("failed commit did not stop before downstream/terminal")
			}
			record.EventID = "pg-session-commit-rejected"
			if record.Status != "error" || record.HTTPCode != 200 || record.ErrorSource != "gateway" || record.ErrorPhase != "session_binding" || record.TTFTMs != nil || record.FirstOutputKind != "" || record.UsageKnown != nil {
				t.Fatal("actual commit error lost its no-delivery observation")
			}
		} else {
			if err != nil || !result.Completed || !state.committed || !barrier.delivered || !strings.Contains(visible.Body.String(), "PRIVATE_RESPONSE_TEXT") {
				t.Fatal("successful commit did not deliver model output")
			}
			record.EventID = "pg-session-commit-success"
			if record.Status != "ok" || record.HTTPCode != 200 || record.TTFTMs == nil || *record.TTFTMs != 7 || record.UsageKnown == nil || !*record.UsageKnown || record.InputTokens != 100 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 20 {
				t.Fatal("successful measured output or usage changed")
			}
		}
		record.Time = base + int64(len(rows))
		rows = append(rows, record)
		writer.Record(record)
	}
	if err := writer.HealthError(); err != nil {
		t.Fatal(err)
	}
	reader := newTestPGMetrics(t, db)
	assertRecord := func(got, expected RequestRecord) {
		t.Helper()
		if got.Status != expected.Status || got.HTTPCode != expected.HTTPCode || got.ErrorSource != expected.ErrorSource || got.ErrorPhase != expected.ErrorPhase || got.FirstOutputKind != expected.FirstOutputKind || !reflect.DeepEqual(got.TTFTMs, expected.TTFTMs) || !reflect.DeepEqual(got.UsageKnown, expected.UsageKnown) || !reflect.DeepEqual(got.CachedTokensKnown, expected.CachedTokensKnown) || !reflect.DeepEqual(got.CacheWriteTokens, expected.CacheWriteTokens) || got.InputTokens != expected.InputTokens || got.OutputTokens != expected.OutputTokens || got.CachedTokens != expected.CachedTokens {
			t.Fatal("session commit or delivered timing/usage changed during persistence")
		}
	}
	assertJSON := func(raw []byte, expected RequestRecord) {
		t.Helper()
		var got RequestRecord
		if json.Unmarshal(raw, &got) != nil {
			t.Fatal("stored request JSON invalid")
		}
		assertRecord(got, expected)
		if bytes.Contains(raw, []byte("PRIVATE_RESPONSE_TEXT")) {
			t.Fatal("model output entered persisted observation")
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if expected.TTFTMs == nil && fields["ttft_ms"] != nil {
			t.Fatal("undelivered first output became measured TTFT")
		}
		if expected.CacheWriteTokens == nil && fields["cache_write_tokens"] != nil {
			t.Fatal("unknown cache creation became zero")
		}
	}
	for _, expected := range rows {
		var raw []byte
		if err := db.Pool.QueryRow(t.Context(), "SELECT record FROM request_logs WHERE event_id=$1", expected.EventID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		assertJSON(raw, expected)
		got, err := reader.managedLogDetail(t.Context(), expected.EventID)
		if err != nil {
			t.Fatal(err)
		}
		assertRecord(got, expected)
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&model=pg-session-commit-fixture&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", base, base+60))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatal("fresh reader request count changed")
	}
	for i, item := range page.Items {
		encoded, _ := json.Marshal(item)
		assertJSON(encoded, rows[i])
	}
	stats, err := reader.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 2 || stats.Success != 1 || stats.Errors != 1 || stats.Aborted != 0 || stats.Input != 100 || stats.Output != 7 || stats.TTFTSamples != 1 || stats.AverageTTFT == nil || *stats.AverageTTFT != 7 || stats.UsageKnownRequests != 1 || stats.UsageUnknownRequests != 1 || stats.CacheReadSamples != 1 || stats.CacheReadUnknownRequests != 1 || stats.CacheReadInputTokens != 100 || stats.CacheReadTokens != 80 || stats.CacheWriteSamples != 1 || stats.CacheWriteTokens == nil || *stats.CacheWriteTokens != 20 {
		t.Fatal("HTTP200 binding failure was counted as success or unknown timing/usage entered denominator")
	}
	failed := q
	failed.Status = "failed"
	failurePage, err := reader.managedLogs(t.Context(), failed)
	if err != nil || failurePage.Total != 1 || len(failurePage.Items) != 1 || failurePage.Items[0].EventID != rows[0].EventID || failurePage.Items[0].HTTPCode != 200 {
		t.Fatal("HTTP200 error missing from failure query")
	}
	for _, redacted := range []bool{false, true} {
		var exported bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "jsonl", &exported, redacted)
		if err != nil || count != 2 {
			t.Fatal("JSONL export failed")
		}
		lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte{'\n'})
		if len(lines) != 2 {
			t.Fatal("JSONL count changed")
		}
		for i, raw := range lines {
			assertJSON(raw, rows[i])
		}
	}
	var exported bytes.Buffer
	count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "csv", &exported, true)
	if err != nil || count != 2 || managedUsageExportSchema != "codex-usage-v4" {
		t.Fatal("CSV export schema changed")
	}
	csvRows, err := csv.NewReader(&exported).ReadAll()
	if err != nil || len(csvRows) != 3 || len(csvRows[0]) != 77 {
		t.Fatal("CSV v4/77 contract changed")
	}
	columns := map[string]int{}
	for i, name := range csvRows[0] {
		columns[name] = i
	}
	for _, name := range []string{"ttft_ms", "input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens"} {
		if csvRows[1][columns[name]] != "" {
			t.Fatal("CSV undelivered/unknown field emitted measured zero")
		}
	}
	for name, want := range map[string]string{"status": "error", "http_code": "200", "error_source": "gateway", "error_phase": "session_binding"} {
		if csvRows[1][columns[name]] != want {
			t.Fatal("CSV binding failure provenance changed")
		}
	}
	for name, want := range map[string]string{"status": "ok", "http_code": "200", "ttft_ms": "7", "input_tokens": "100", "output_tokens": "7", "cached_tokens": "80", "cache_write_tokens": "20"} {
		if csvRows[2][columns[name]] != want {
			t.Fatal("CSV delivered timing/usage changed")
		}
	}
	t.Log("isolated PostgreSQL: typed failed session commit HTTP200/error/no delivered TTFT versus real success TTFT7/input100/read80/write20; fresh raw/detail/query/JSONL/CSV preserve unknown fields and failure counts")
}
