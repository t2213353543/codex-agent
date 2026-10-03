package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// These cases pin the classification contract independently of its SQL CASE.
// Classification must never substitute an inferred status into HTTPCode.
func managedSeverityContractRows() ([]RequestRecord, []string) {
	var records []RequestRecord
	var expected []string
	errors := []struct {
		message          string
		client, priority bool
	}{
		{"unclassified diagnostic", false, false},
		{"Unsupported parameter: temperature", true, false},
		{"system messages are not allowed", true, false},
		{"invalidated oauth", true, false},
		{"refresh_token_reused", true, false},
		{"model_not_found", true, false},
		{"invalid_encrypted_content", true, false},
		{"request exceeds 32 MiB", false, false},
		{"flagged for possible policy violation", false, false},
		{"Rate limit exceeded", false, true},
		{"Our servers are currently overloaded", false, true},
		{"An error occurred while processing the request", false, true},
		{"context canceled", false, false},
		{"deadline exceeded", false, true},
	}
	for _, status := range []string{"ok", "error", "aborted"} {
		for _, code := range []int{0, 99, 100, 101, 200, 299, 399, 400, 401, 403, 404, 413, 429, 499, 500, 503, 504, 599, 600} {
			for i, item := range errors {
				r := RequestRecord{Status: status, HTTPCode: code, Error: item.message, EventID: fmt.Sprintf("severity-%s-%d-%d", status, code, i)}
				want := "P3"
				category := managedFailureCategory(r)
				switch {
				case status == "ok":
					want = ""
				case code < 100 || code > 599:
					want = "unknown"
				case item.client || category == "authentication":
					want = "P3"
				case code >= 500 || code == 429:
					want = "P1"
				// Existing category precedence can select cancellation or payload
				// size before an error-message hint; keep that one actual category.
				case item.priority && (category == "rate_limit" || category == "overloaded" || category == "upstream_error" || category == "timeout"):
					want = "P1"
				case code >= 400:
					want = "P2"
				}
				records = append(records, r)
				expected = append(expected, want)
			}
		}
	}
	return records, expected
}

func TestManagedSeverityContractMatrix(t *testing.T) {
	records, expected := managedSeverityContractRows()
	for i, record := range records {
		if got := managedErrorSeverity(record); got != expected[i] {
			t.Fatalf("%s severity=%s want=%s category=%s", record.EventID, got, expected[i], managedFailureCategory(record))
		}
		got := managedRecord(record)
		if got.HTTPCode != record.HTTPCode || got.Status != record.Status || got.ErrorSeverity != expected[i] {
			t.Fatalf("management projection changed request facts: %+v", got)
		}
		if record.Status == "ok" && got.ErrorSeveritySource != "" || record.Status != "ok" && got.ErrorSeveritySource != "management_rules_v1" {
			t.Fatal("classification provenance missing or applied to success")
		}
	}
}

func TestPGManagedSeverityContractMatrix(t *testing.T) {
	db := newMetricsTestDB(t)
	records, expected := managedSeverityContractRows()
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	query := `WITH request_logs AS (SELECT value AS record, value->>'status' AS status, ordinality AS position FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY)
 SELECT position,` + managedErrorSeveritySQL() + ` FROM request_logs ORDER BY position`
	rows, err := db.Pool.Query(context.Background(), query, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var position int
		var severity string
		if err := rows.Scan(&position, &severity); err != nil {
			t.Fatal(err)
		}
		if position != count+1 || severity != expected[count] || severity != managedErrorSeverity(records[count]) {
			t.Fatalf("PG category contract row=%d severity=%s expected=%s record=%+v", position, severity, expected[count], records[count])
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(records) {
		t.Fatalf("incomplete matrix: %d/%d", count, len(records))
	}
}
