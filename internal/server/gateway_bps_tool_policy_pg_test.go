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

	"codex-agent/internal/bps"
)

// A single isolated PostgreSQL round trip for the new passive struct. This is
// storage/export verification, not a second gateway or OAuth behavior test.
func TestPGBPSToolPolicyObservationRoundTrip(t *testing.T) {
	db := newMetricsTestDB(t)
	writer := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	yes, no := true, false
	zero, writeSeven := int64(0), int64(7)
	prepare := func(body string) *bps.Request {
		t.Helper()
		request, err := bps.Prepare([]byte(body), bps.Options{TaskID: "pg-policy-task", TurnID: "pg-policy-turn", StructuredOutput: true})
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	plain := prepare(`{"model":"gpt-6-astra","input":"PRIVATE_POLICY_PROMPT","tools":[],"text":{"format":{"type":"json_object"}}}`)
	knownZero := bps.ObserveToolPolicy(plain)
	rejected := prepare(`{"model":"gpt-6-astra","input":[{"type":"reasoning","encrypted_content":"PRIVATE_POLICY_CIPHER","summary":[]}],"tools":[{"type":"function","name":"PRIVATE_POLICY_DECLARED","namespace":"PRIVATE_POLICY_NAMESPACE","description":"PRIVATE_POLICY_SCHEMA","parameters":{"type":"object"}}]}`)
	failedPolicy := bps.ObserveToolPolicy(rejected)
	item := map[string]json.RawMessage{
		"type": json.RawMessage(`"function_call"`), "name": json.RawMessage(`"PRIVATE_POLICY_UNDECLARED"`),
		"call_id": json.RawMessage(`"call_pg_policy"`), "arguments": json.RawMessage(`"PRIVATE_POLICY_ARGUMENT"`),
	}
	before := bps.CaptureToolPolicyItem(item)
	if rejected.Tools.NormalizeItem(item) == nil {
		t.Fatal("synthetic undeclared tool did not fail the real normalizer")
	}
	failedPolicy.RecordRejection("normalize_item", before.Resolve(rejected).WithRequestDeclarationEvidence(
		rejected.Body, rejected.Body, strings.Repeat("a", 64), strings.Repeat("b", 64)))
	if failedPolicy.RejectedIdentity != "undeclared" || failedPolicy.RejectedMatchesExactDeclaration == nil || *failedPolicy.RejectedMatchesExactDeclaration {
		t.Fatal("producer did not create the expected finite rejection metadata")
	}
	if failedPolicy.InboundFamilyNameDeclared == nil || *failedPolicy.InboundFamilyNameDeclared ||
		failedPolicy.SerializedInitialFamilyNameDeclared == nil || *failedPolicy.SerializedInitialFamilyNameDeclared ||
		len(failedPolicy.RejectedNameSHA256) != 64 || failedPolicy.ResolvedToolsSHA256 != strings.Repeat("a", 64) {
		t.Fatal("bounded declaration evidence was not produced")
	}
	rows := []RequestRecord{
		{EventID: "pg-policy-known-zero", Status: "ok", HTTPCode: 200, InputTokens: 40, OutputTokens: 4, CachedTokensKnown: &yes, UsageKnown: &yes, CacheWriteTokens: &zero, ToolPolicy: knownZero},
		{EventID: "pg-policy-undeclared", Status: "error", HTTPCode: 502, InputTokens: 100, OutputTokens: 10, CachedTokens: 80, CachedTokensKnown: &yes, UsageKnown: &yes, CacheWriteTokens: &writeSeven, ToolPolicy: failedPolicy, ErrorSource: "upstream", ErrorPhase: "bps_protocol", Error: "synthetic tool policy rejection"},
		{EventID: "pg-policy-legacy-unobserved", Status: "error", HTTPCode: 503, UsageKnown: &no, Error: "synthetic unobserved legacy request"},
	}
	want := make(map[string]RequestRecord, len(rows))
	for index := range rows {
		rows[index].Time, rows[index].Model, rows[index].Endpoint = base+int64(index), "pg-tool-policy-fixture", "responses"
		rows[index].ObservationSchemaVersion = 2
		want[rows[index].EventID] = rows[index]
		writer.Record(rows[index])
	}
	if err := writer.HealthError(); err != nil {
		t.Fatal(err)
	}
	reader := newTestPGMetrics(t, db)
	assertRecord := func(got RequestRecord, expected RequestRecord) {
		t.Helper()
		if !reflect.DeepEqual(got.ToolPolicy, expected.ToolPolicy) || got.ObservationSchemaVersion != 2 || got.InputTokens != expected.InputTokens || got.OutputTokens != expected.OutputTokens || got.CachedTokens != expected.CachedTokens || !reflect.DeepEqual(got.CacheWriteTokens, expected.CacheWriteTokens) || !reflect.DeepEqual(got.UsageKnown, expected.UsageKnown) || !reflect.DeepEqual(got.CachedTokensKnown, expected.CachedTokensKnown) {
			t.Fatalf("tool policy or independent usage fields changed for %s", expected.EventID)
		}
	}
	assertJSON := func(raw []byte, expected RequestRecord) {
		t.Helper()
		var got RequestRecord
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		assertRecord(got, expected)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		policy, exists := fields["tool_policy"]
		if expected.ToolPolicy == nil {
			if exists {
				t.Fatal("unobserved legacy policy was filled in")
			}
			return
		}
		if !exists || bytes.Equal(policy, []byte("null")) {
			t.Fatal("known policy was dropped")
		}
		if bytes.Contains(policy, []byte("PRIVATE_POLICY_")) {
			t.Fatal("tool identity/arguments/schema/prompt/cipher leaked into policy metadata")
		}
		var observation map[string]json.RawMessage
		_ = json.Unmarshal(policy, &observation)
		if expected.ToolPolicy.CatalogAtPrepareCount == 0 {
			for _, field := range []string{"catalog_at_prepare_count", "named_namespaces_at_prepare_count"} {
				if string(observation[field]) != "0" {
					t.Fatal("known zero policy count was omitted")
				}
			}
		}
		if string(observation["tool_search_at_prepare"]) != "false" {
			t.Fatal("known false tool search flag was omitted")
		}
		if expected.ToolPolicy.RejectionPhase != "" {
			if string(observation["rejected_matches_exact_declaration"]) != "false" || string(observation["rejected_family"]) != `"function_call"` || string(observation["rejected_identity"]) != `"undeclared"` || string(observation["rejection_phase"]) != `"normalize_item"` {
				t.Fatal("finite rejection fields changed or false was omitted")
			}
			if string(observation["structured"]) != "false" {
				t.Fatal("known false structured flag was omitted")
			}
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
			out := managedExportRecord(stored, redacted)
			encoded, _ := json.Marshal(out)
			assertJSON(encoded, expected)
		}
	}
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&model=pg-tool-policy-fixture&exact_total=true&sort_by=time&sort_order=asc&page_size=10&timezone=UTC&step=60", base, base+60))
	page, err := reader.managedLogs(t.Context(), q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.HasMore {
		t.Fatalf("PG policy request cardinality: total=%d rows=%d err=%v", page.Total, len(page.Items), err)
	}
	for _, item := range page.Items {
		expected, exists := want[item.EventID]
		if !exists {
			t.Fatal("unexpected PG request")
		}
		encoded, _ := json.Marshal(item)
		assertJSON(encoded, expected)
	}
	stats, err := reader.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 3 || stats.Success != 1 || stats.Errors != 2 || stats.Input != 140 || stats.Output != 14 || stats.Tokens != 154 || stats.UsageKnownRequests != 2 || stats.UsageUnknownRequests != 1 || stats.CacheReadSamples != 2 || stats.CacheReadUnknownRequests != 1 || stats.CacheReadInputTokens != 140 || stats.CacheReadTokens != 80 || stats.CacheWriteSamples != 2 || stats.CacheWriteTokens == nil || *stats.CacheWriteTokens != 7 {
		t.Fatalf("policy records changed request/read/write/unknown aggregates: %+v err=%v", stats, err)
	}
	filtered := q
	filtered.HTTPCode = 502
	failurePage, err := reader.managedLogs(t.Context(), filtered)
	if err != nil || failurePage.Total != 1 || len(failurePage.Items) != 1 || failurePage.Items[0].EventID != "pg-policy-undeclared" {
		t.Fatalf("tool rejection query lost record: %+v err=%v", failurePage, err)
	}
	assertRecord(failurePage.Items[0].RequestRecord, rows[1])
	for _, redacted := range []bool{false, true} {
		var output bytes.Buffer
		count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "jsonl", &output, redacted)
		if err != nil || count != 3 {
			t.Fatalf("JSONL policy export count=%d err=%v", count, err)
		}
		lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatal("JSONL policy export changed record count")
		}
		for index, raw := range lines {
			assertJSON(raw, rows[index])
		}
	}
	if managedUsageExportSchema != "codex-usage-v4" {
		t.Fatal("passive tool policy changed CSV schema version")
	}
	var csvOutput bytes.Buffer
	count, err := reader.exportManagedLogsWithPrivacy(t.Context(), q, "csv", &csvOutput, false)
	if err != nil || count != 3 {
		t.Fatalf("CSV policy export count=%d err=%v", count, err)
	}
	csvRows, err := csv.NewReader(&csvOutput).ReadAll()
	if err != nil || len(csvRows) != 4 || len(csvRows[0]) != 77 {
		t.Fatalf("tool policy changed CSV rows/77 columns: rows=%d err=%v", len(csvRows), err)
	}
	for _, name := range csvRows[0] {
		if strings.Contains(name, "tool_policy") {
			t.Fatal("passive struct was appended to the CSV contract")
		}
	}
	for _, row := range csvRows[1:] {
		if len(row) != 77 {
			t.Fatal("CSV policy row width changed")
		}
	}
	t.Log("isolated PostgreSQL: 3 request records, finite tool policy/known zero/false/legacy nil, raw JSONB/detail/query/private+redacted JSONL, independent read80/write7/unknown1, CSV v4/77 preserved")
}
