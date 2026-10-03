package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A single isolated PostgreSQL round trip for bounded selection facts and
// actual attachment-attempt metadata. This adds no inference or OAuth traffic.
func TestPGBPSBoundAndAttachmentObservationRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	zero := int64(0)
	blocked := &officialFailoverState{boundReplayClass: "previous_response", replayBlocked: true}
	blocked.rememberBoundSelection(&GatewayBoundSelectionObservation{BoundSnapshotState: "exact_disabled", EligibleCandidateCount: 0, SnapshotOrigin: "fresh_read", SnapshotAgeMS: &zero})
	blockedRecord := RequestRecord{EventID: "pg-bound-zero", Status: "error", HTTPCode: 503, ErrorSource: "gateway", ErrorPhase: "account_selection", Error: "synthetic unavailable bound account"}
	blocked.finishBoundSelection(t.Context(), &blockedRecord, false)
	recovered := &officialFailoverState{boundReplayClass: "full_plaintext", retrying: true, committed: true}
	recovered.rememberBoundSelection(&GatewayBoundSelectionObservation{BoundSnapshotState: "missing_or_replaced", EligibleCandidateCount: 2, SnapshotOrigin: "cache_hit"})
	recoveredRecord := RequestRecord{EventID: "pg-bound-recovered", Status: "ok", HTTPCode: 200}
	recovered.finishBoundSelection(t.Context(), &recoveredRecord, false)
	if blockedRecord.BoundSelection.RecoveryOutcome != "blocked_input" || recoveredRecord.BoundSelection.RecoveryOutcome != "recovered" {
		t.Fatal("bounded observation producer did not retain actual disposition")
	}
	_, attempt := withGatewayAttemptObservation(t.Context())
	started := time.Now()
	handle := attempt.Begin("request", "https://PRIVATE_ATTACHMENT_HOST.invalid/basispoints/api/attachments?secret=PRIVATE_QUERY", started)
	handle.Headers(429, started.Add(time.Millisecond))
	handle.Finish("upstream", "http_status", started.Add(2*time.Millisecond))
	uploadRecord := RequestRecord{EventID: "pg-attachment-429", Status: "error", HTTPCode: 429, ErrorSource: "upstream", ErrorPhase: "attachment", Error: "upstream image upload returned HTTP 429"}
	applyGatewayAttemptSnapshot(&uploadRecord, attempt)
	// Actual upload provenance must not claim a dispatched generation.
	if len(uploadRecord.Attempts) != 1 || uploadRecord.Attempts[0].Endpoint != "/attachments" || uploadRecord.Attempts[0].ObservedHTTPStatusCode == nil || *uploadRecord.Attempts[0].ObservedHTTPStatusCode != 429 {
		t.Fatal("fixed attachment route/status observation missing")
	}
	rows := []RequestRecord{blockedRecord, recoveredRecord, uploadRecord, {EventID: "pg-bound-legacy", Status: "error", HTTPCode: 400, Error: "synthetic legacy request"}}
	want := make(map[string]RequestRecord, len(rows))
	for i := range rows {
		rows[i].Time, rows[i].Model, rows[i].Endpoint = base+int64(i), "pg-bound-attachment-fixture", "responses"
		rows[i].RequestType = "http_json"
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
		if !reflect.DeepEqual(got.BoundSelection, expected.BoundSelection) || !reflect.DeepEqual(got.Attempts, expected.Attempts) || got.Status != expected.Status || got.HTTPCode != expected.HTTPCode {
			t.Fatal("bound/attachment observations changed after persistence")
		}
		if got.UsageKnown != nil || got.CachedTokensKnown != nil || got.CacheWriteTokens != nil || got.InputTokens != 0 || got.OutputTokens != 0 || got.CachedTokens != 0 || got.SentModel != "" || got.ReturnedModel != "" || got.FinalUpstreamErrorCode != "" || got.FinalUpstreamErrorSource != "" {
			t.Fatal("unobserved generation usage/final error metadata invented")
		}
		if got.Status == "ok" && (managedFailureCategory(got) != "" || managedErrorSeverity(got) != "") {
			t.Fatal("recovered diagnostic converted a success to failure")
		}
	}
	assertJSON := func(raw []byte, expected RequestRecord) {
		t.Helper()
		var got RequestRecord
		if json.Unmarshal(raw, &got) != nil {
			t.Fatal("invalid persisted observation JSON")
		}
		assertRecord(got, expected)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if expected.BoundSelection == nil {
			if fields["bound_selection"] != nil {
				t.Fatal("legacy or upload-only record acquired missing owner facts")
			}
		} else {
			var obs map[string]json.RawMessage
			_ = json.Unmarshal(fields["bound_selection"], &obs)
			allowed := map[string]bool{"bound_snapshot_state": true, "eligible_candidate_count": true, "snapshot_origin": true, "snapshot_age_ms": true, "replay_class": true, "recovery_outcome": true}
			for key := range obs {
				if !allowed[key] {
					t.Fatal("bound observation contains extra/private fields")
				}
			}
			if string(obs["eligible_candidate_count"]) != fmt.Sprint(expected.BoundSelection.EligibleCandidateCount) {
				t.Fatal("known candidate count lost")
			}
			if expected.BoundSelection.SnapshotAgeMS == nil {
				if obs["snapshot_age_ms"] != nil {
					t.Fatal("unknown snapshot age became zero")
				}
			} else if string(obs["snapshot_age_ms"]) != "0" || len(obs) != 6 {
				t.Fatal("known zero snapshot age omitted or six-scalar shape lost")
			}
		}
		if bytes.Contains(raw, []byte("PRIVATE_")) {
			t.Fatal("attachment URL/query escaped metadata filter")
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
		for _, redacted := range []bool{false, true} {
			encoded, _ := json.Marshal(managedExportRecord(got, redacted))
			assertJSON(encoded, expected)
		}
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&model=pg-bound-attachment-fixture&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", base, base+60))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 4 || len(page.Items) != 4 {
		t.Fatal("fresh DB observation query count changed")
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
	if err != nil || stats.Requests != 4 || stats.Success != 1 || stats.Errors != 3 || stats.UsageKnownRequests != 0 || stats.UsageUnknownRequests != 4 || stats.CacheReadSamples != 0 || stats.CacheReadUnknownRequests != 4 || stats.CacheWriteSamples != 0 || stats.CacheWriteTokens != nil {
		t.Fatal("diagnostics changed success count or unknown read/write denominator")
	}
	filtered := q
	filtered.Status = "failed"
	errorsPage, err := reader.managedLogs(t.Context(), filtered)
	if err != nil || errorsPage.Total != 3 {
		t.Fatal("recovered diagnostic counted as failed query")
	}
	for _, r := range errorsPage.Items {
		if r.EventID == recoveredRecord.EventID {
			t.Fatal("success with recovered diagnostics entered error result")
		}
	}
	for _, redacted := range []bool{false, true} {
		var output bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "jsonl", &output, redacted)
		if err != nil || count != 4 {
			t.Fatal("JSONL observation export failed")
		}
		lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
		if len(lines) != 4 {
			t.Fatal("JSONL record count changed")
		}
		for i, raw := range lines {
			assertJSON(raw, rows[i])
		}
	}
	var output bytes.Buffer
	count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "csv", &output, true)
	if err != nil || count != 4 || managedUsageExportSchema != "codex-usage-v4" {
		t.Fatal("CSV observation export failed")
	}
	csvRows, err := csv.NewReader(&output).ReadAll()
	if err != nil || len(csvRows) != 5 || len(csvRows[0]) != 77 {
		t.Fatal("CSV v4 77-column contract changed")
	}
	columns := map[string]int{}
	for i, name := range csvRows[0] {
		columns[name] = i
		if strings.Contains(name, "bound_selection") {
			t.Fatal("passive struct changed CSV columns")
		}
	}
	for index, row := range csvRows[1:] {
		if len(row) != 77 {
			t.Fatal("CSV observation row width changed")
		}
		for _, field := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_write_tokens", "final_upstream_error_code", "final_upstream_error_source"} {
			if row[columns[field]] != "" {
				t.Fatal("CSV invented known usage or generation error")
			}
		}
		var attempts []GatewayAttemptSummary
		if raw := row[columns["attempts"]]; raw != "" {
			if json.Unmarshal([]byte(raw), &attempts) != nil {
				t.Fatal("CSV attempt JSON corrupted")
			}
		}
		if !reflect.DeepEqual(attempts, rows[index].Attempts) {
			t.Fatal("CSV lost /attachments attempt status/endpoint")
		}
	}
	t.Log("isolated PostgreSQL: four observations, bounded six scalar fields/zero/unknown age/legacy nil, success recovered remains success, actual attachment429 path/status without generation usage, fresh JSONB/detail/query/redacted JSONL, CSV v4/77 and embedded attempts retained")
}
