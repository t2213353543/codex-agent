package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"testing"
)

func TestManagedUsageCacheKnowledgePersistenceAndExport(t *testing.T) {
	p, handler := webTestSetup(t)
	yes, no := true, false
	cases := []struct {
		id     string
		known  *bool
		cached int64
		csv    string
	}{
		{id: "legacy-zero", cached: 0, csv: ""},
		{id: "measured-zero", known: &yes, cached: 0, csv: "0"},
		{id: "measured-eight", known: &yes, cached: 8, csv: "8"},
		// A connection can contain a reported cached count and another turn
		// without cache usage. Keep that reported subtotal, but do not claim
		// the connection's complete cached count is known.
		{id: "partial-unknown", known: &no, cached: 7, csv: ""},
	}
	for i, item := range cases {
		p.Metrics.Record(RequestRecord{EventID: item.id, Time: 1700000000 + int64(i), Account: "cache-observation", Model: "cache-knowledge", Status: "ok", InputTokens: 20, OutputTokens: 1, CachedTokens: item.cached, CachedTokensKnown: item.known, UsageKnown: &yes})
	}
	assertRecord := func(raw []byte, item struct {
		id     string
		known  *bool
		cached int64
		csv    string
	}) {
		t.Helper()
		var decoded RequestRecord
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.CachedTokens != item.cached || (decoded.CachedTokensKnown == nil) != (item.known == nil) || item.known != nil && *decoded.CachedTokensKnown != *item.known {
			t.Fatalf("cache knowledge changed for %s: cached=%d known=%v", item.id, decoded.CachedTokens, decoded.CachedTokensKnown)
		}
	}
	for _, item := range cases {
		// managedLogDetail reads the persisted JSON log, not the recent ring.
		stored, err := p.Metrics.managedLogDetail(t.Context(), item.id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(stored)
		assertRecord(raw, item)
		projection, _ := json.Marshal(managedRecord(stored))
		assertRecord(projection, item)
	}
	last, ok := p.Metrics.LastFor("cache-observation")
	if !ok || last.CachedTokensKnown == nil || *last.CachedTokensKnown || last.CachedTokens != 7 {
		t.Fatal("account latest request lost unknown cache observation")
	}
	response := managedTestRequest(t, handler, "GET", "/api/logs/export?model=cache-knowledge&format=csv&sort_by=time&sort_order=asc", nil, 200)
	rows, err := csv.NewReader(bytes.NewReader(response.Body.Bytes())).ReadAll()
	if err != nil || len(rows) != len(cases)+1 {
		t.Fatalf("CSV export failed: rows=%d error=%v", len(rows), err)
	}
	columns := map[string]int{}
	for i, name := range rows[0] {
		columns[name] = i
	}
	for i, item := range cases {
		row := rows[i+1]
		if row[columns["cached_tokens"]] != item.csv || row[columns["input_tokens"]] != "20" || row[columns["output_tokens"]] != "1" {
			t.Fatalf("CSV confused missing cache with measured zero for %s", item.id)
		}
	}
	response = managedTestRequest(t, handler, "GET", "/api/logs/export?model=cache-knowledge&format=jsonl&sort_by=time&sort_order=asc", nil, 200)
	lines := bytes.Split(bytes.TrimSpace(response.Body.Bytes()), []byte{'\n'})
	if len(lines) != len(cases) {
		t.Fatal("JSONL export count changed")
	}
	for i, line := range lines {
		assertRecord(line, cases[i])
	}
	stats, err := p.Metrics.managedLogStats(t.Context(), managedQuery(t, "model=cache-knowledge"))
	if err != nil || stats.Cached != 15 {
		t.Fatalf("aggregate must retain reported subtotals only: cached=%d error=%v", stats.Cached, err)
	}
}
