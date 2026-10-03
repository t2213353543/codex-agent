package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

func officialRepairMergeCacheTestRecord(t *testing.T, raw, patch []byte) []byte {
	t.Helper()
	var record, fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(patch, &fields); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		record[name] = value
	}
	merged, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

func officialRepairLegacyCacheTestRecord(t *testing.T, raw []byte, entry officialUsageRepairEntry, known *bool) []byte {
	t.Helper()
	fields := map[string]any{
		"usage_known": true, "input_tokens": *entry.Usage.Input, "output_tokens": *entry.Usage.Output,
		"cached_tokens": *entry.Usage.Cached, "usage_total_tokens": *entry.Usage.Total,
		"reasoning_tokens": *entry.Usage.Reasoning, "cache_write_tokens": *entry.Usage.Write,
		"usage_schema_source": "official_rollout",
		"window_cost_usd":     0.125, "window_cost_price_version": "existing-price-version",
	}
	if known != nil {
		fields["cached_tokens_known"] = known
	}
	patch, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return officialRepairMergeCacheTestRecord(t, raw, patch)
}

func TestOfficialUsageRepairCacheKnownFreshEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached int64
		known  *bool
	}{
		{"zero_missing_flag", 0, nil},
		{"zero_explicit_false", 0, officialRepairPtr(false)},
		{"positive_missing_flag", 60, nil},
		{"positive_explicit_false", 60, officialRepairPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, entry, raw := officialRepairFixture(t)
			entry.Usage.Cached = officialRepairPtr(tc.cached)
			if tc.known != nil {
				raw = officialRepairMergeCacheTestRecord(t, raw, []byte(`{"cached_tokens_known":false}`))
			}
			patch, already, _, err := officialRepairRecordPatch(raw, entry)
			if err != nil || already {
				t.Fatalf("fresh evidence rejected: already=%v error=%v", already, err)
			}
			repaired := officialRepairMergeCacheTestRecord(t, raw, patch)
			var record RequestRecord
			if err := json.Unmarshal(repaired, &record); err != nil {
				t.Fatal(err)
			}
			if record.CachedTokensKnown == nil || !*record.CachedTokensKnown || record.CachedTokens != tc.cached || !cacheReadObserved(record) {
				t.Fatalf("proven cache read remained unknown: cached=%d known=%v", record.CachedTokens, record.CachedTokensKnown)
			}
			var totals managedCacheReadTotals
			totals.addCacheRead(record)
			if totals.CacheReadSamples != 1 || totals.CacheReadUnknownRequests != 0 || totals.CacheReadInputTokens != 100 || totals.CacheReadTokens != tc.cached || totals.CacheReadRate == nil || *totals.CacheReadRate != float64(tc.cached)/100 {
				t.Fatalf("repaired evidence missing from weighted cache statistics: %+v", totals)
			}
			if next, done, _, err := officialRepairRecordPatch(repaired, entry); err != nil || !done || len(next) != 0 {
				t.Fatalf("fresh repair is not idempotent: done=%v patch=%s error=%v", done, next, err)
			}
		})
	}
}

func TestOfficialUsageRepairCacheKnownLegacyBackfill(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached int64
		known  *bool
	}{
		{"zero_missing_flag", 0, nil},
		{"zero_explicit_false", 0, officialRepairPtr(false)},
		{"positive_explicit_false", 60, officialRepairPtr(false)},
		{"already_known", 60, officialRepairPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, entry, raw := officialRepairFixture(t)
			entry.Usage.Cached = officialRepairPtr(tc.cached)
			raw = officialRepairLegacyCacheTestRecord(t, raw, entry, tc.known)
			patch, already, costKnown, err := officialRepairRecordPatch(raw, entry)
			if err != nil || !already || !costKnown {
				t.Fatalf("verified existing usage was rejected: already=%v costKnown=%v error=%v", already, costKnown, err)
			}
			if tc.known != nil && *tc.known {
				if len(patch) != 0 {
					t.Fatal("complete known record was modified")
				}
				return
			}
			if !bytes.Equal(patch, []byte(`{"cached_tokens_known":true}`)) {
				t.Fatalf("legacy repair must only backfill cache knowledge: patch=%s", patch)
			}
			repaired := officialRepairMergeCacheTestRecord(t, raw, patch)
			var record RequestRecord
			if err := json.Unmarshal(repaired, &record); err != nil || !cacheReadObserved(record) {
				t.Fatal("backfilled cache usage still excluded from statistics", err)
			}
			if next, done, _, err := officialRepairRecordPatch(repaired, entry); err != nil || !done || len(next) != 0 {
				t.Fatalf("legacy backfill is not idempotent: done=%v patch=%s error=%v", done, next, err)
			}
		})
	}
}

func TestOfficialUsageRepairCacheKnownRejectsConflictingEvidence(t *testing.T) {
	_, _, entry, raw := officialRepairFixture(t)
	raw = officialRepairLegacyCacheTestRecord(t, raw, entry, officialRepairPtr(false))
	for _, patch := range []string{
		`{"input_tokens":101}`, `{"output_tokens":11}`, `{"cached_tokens":0}`,
		`{"usage_total_tokens":111}`, `{"reasoning_tokens":5}`, `{"cache_write_tokens":1}`,
		`{"usage_schema_source":"responses_usage"}`, `{"latency_ms":3001}`,
	} {
		t.Run(patch, func(t *testing.T) {
			changed := officialRepairMergeCacheTestRecord(t, raw, []byte(patch))
			out, _, _, err := officialRepairRecordPatch(changed, entry)
			if err == nil || len(out) != 0 {
				t.Fatal("knowledge backfill bypassed conflicting usage or identity")
			}
		})
	}
}

func TestOfficialUsageRepairCacheKnownPGLegacyDryRunCommitAndConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "matching_durable_usage"
		if conflict {
			name = "conflicting_durable_usage"
		}
		t.Run(name, func(t *testing.T) {
			db := newMetricsTestDB(t)
			root, source, entry, raw := officialRepairFixture(t)
			raw = officialRepairLegacyCacheTestRecord(t, raw, entry, officialRepairPtr(false))
			insertOfficialRepairFixture(t, db, entry, raw)
			cached := *entry.Usage.Cached
			if conflict {
				cached--
			}
			if _, err := db.Pool.Exec(t.Context(), `UPDATE usage_events SET input_tokens=$1,output_tokens=$2,cached_tokens=$3 WHERE event_id=$4`, *entry.Usage.Input, *entry.Usage.Output, cached, entry.EventID); err != nil {
				t.Fatal(err)
			}
			readState := func() (string, string) {
				t.Helper()
				var record, durable string
				if err := db.Pool.QueryRow(t.Context(), `SELECT l.record::text,to_jsonb(e)::text FROM request_logs l JOIN usage_events e USING(event_id) WHERE l.id=$1`, entry.RecordID).Scan(&record, &durable); err != nil {
					t.Fatal(err)
				}
				return record, durable
			}
			before, beforeDurable := readState()
			if conflict {
				if _, err := repairOfficialUsageWithRoot(t.Context(), db, source, root, false); err == nil {
					t.Fatal("conflicting durable usage was accepted")
				}
				after, afterDurable := readState()
				if after != before || afterDurable != beforeDurable {
					t.Fatal("conflict changed existing observations")
				}
				return
			}
			dry, err := repairOfficialUsageWithRoot(t.Context(), db, source, root, true)
			if err != nil || dry.Repairable != 1 || dry.Updated != 0 || dry.AlreadyRepaired != 0 {
				t.Fatalf("legacy backfill not visible in dry run: %+v error=%v", dry, err)
			}
			after, afterDurable := readState()
			if after != before || afterDurable != beforeDurable {
				t.Fatal("dry run modified observations")
			}
			result, err := repairOfficialUsageWithRoot(t.Context(), db, source, root, false)
			if err != nil || result.Updated != 1 || result.Repairable != 1 || result.RebuildRequired || result.InputTokens != 0 || result.OutputTokens != 0 || result.CachedTokens != 0 {
				t.Fatalf("knowledge-only repair changed token accounting: %+v error=%v", result, err)
			}
			after, afterDurable = readState()
			if afterDurable != beforeDurable {
				t.Fatal("knowledge backfill rewrote durable usage")
			}
			var record RequestRecord
			if err := json.Unmarshal([]byte(after), &record); err != nil || !cacheReadObserved(record) {
				t.Fatal("committed cache knowledge missing", err)
			}
			var beforeFields, afterFields map[string]json.RawMessage
			_ = json.Unmarshal([]byte(before), &beforeFields)
			_ = json.Unmarshal([]byte(after), &afterFields)
			delete(beforeFields, "cached_tokens_known")
			delete(afterFields, "cached_tokens_known")
			beforeJSON, _ := json.Marshal(beforeFields)
			afterJSON, _ := json.Marshal(afterFields)
			if !bytes.Equal(beforeJSON, afterJSON) {
				t.Fatal("backfill changed frozen cost, timing or opaque fields")
			}
			again, err := repairOfficialUsageWithRoot(t.Context(), db, source, root, false)
			if err != nil || again.Updated != 0 || again.AlreadyRepaired != 1 {
				t.Fatalf("metadata backfill did not become idempotent: %+v error=%v", again, err)
			}
		})
	}
}
