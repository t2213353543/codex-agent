package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManagedUsageDecodedProjectionUsesExactScalarClasses(t *testing.T) {
	q := managedQuery(t, "error_category=rate_limit&error_severity=P1")
	prefix, source, where, args := managedUsageDecodedSource(q)
	if strings.Count(prefix, "jsonb_to_record(record)") != 2 || strings.Count(prefix, managedErrorCategoryProjection+"=$1") != 2 || strings.Count(prefix, managedErrorSeverityProjection+"=$2") != 2 || !strings.Contains(prefix, "UNION ALL SELECT created_at") {
		t.Fatalf("each disjoint branch must retain indexed predicates and decode each selected row once: %s", prefix)
	}
	if source != "decoded" || where != "TRUE" || strings.Contains(prefix, "decoded_error_lower") || len(args) != 2 {
		t.Fatalf("exact category must avoid redundant decoding/classification: source=%s where=%s args=%v", source, where, args)
	}
	category, severity, args := managedUsageSelectedClassExpressions(q, args)
	if category != "$3::text" || severity != "$4::text" || len(args) != 4 {
		t.Fatalf("known indexed classes must remain parameterized: %s %s %v", category, severity, args)
	}
	q.ErrorSeverity = ""
	_, severity, _ = managedUsageSelectedClassExpressions(q, nil)
	if strings.Contains(severity, "record->>'error'") || !strings.Contains(severity, "$1::text") {
		t.Fatalf("known category must derive severity without repeating category rules: %s", severity)
	}
	for _, expression := range []string{managedFailureSQL, managedErrorSeveritySQL(), cacheObservationEligibleSQL, cacheReadObservedSQL} {
		if actual := managedUsageDecodedExpression(expression); strings.Contains(actual, "record->") {
			t.Fatalf("repeated JSON extraction escaped typed projection: %s", actual)
		}
	}
}

func TestPGManagedDecodedStatsWideRecordParity(t *testing.T) {
	file, pg := managedUsageFixture(t, false), managedUsageFixture(t, true)
	for i := range 12 {
		r := managedPerformanceRecord(i)
		r.EventID, r.Model, r.Status, r.HTTPCode = fmt.Sprintf("decoded-wide-%d", i), "wide-model", "error", 429
		r.Error = "rate limit exceeded " + strings.Repeat("synthetic observation ", 800)
		if i%3 == 1 {
			r.Error, r.HTTPCode = "authentication failure", 403
		} else if i%3 == 2 {
			r.Error, r.HTTPCode = "unexpected response", 200
			r.FinalUpstreamErrorCode, r.FinalUpstreamErrorSource = "rateLimitExceeded", "upstream"
		}
		file.Record(r)
		pg.Record(r)
	}
	if err := pg.HealthError(); err != nil {
		t.Fatal(err)
	}
	// Unknown retained protocol payloads can be much wider than all management
	// counters combined. They must not affect totals or trigger repeated parses.
	if _, err := pg.db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||jsonb_build_object('unrelated_protocol_payload',repeat('large protocol payload ',8000)) WHERE model='wide-model'`); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"", "error_category=rate_limit", "error_severity=P1", "error_category=authentication&error_severity=P3", "model=wide-model&error_category=rate_limit", "error_category=rate_limit&stream=true"} {
		q := managedQuery(t, filter)
		want, err := file.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		q.statsQueryer = pg.db.Pool // Exercise decoded raw SQL for every variant.
		got, err := pg.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		want.CheckedAt, got.CheckedAt = 0, 0
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Fatalf("%s file=%s PG=%s", filter, a, b)
		}
	}
}
