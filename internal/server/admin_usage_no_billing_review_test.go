package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestPGManagedUsageAggregationParityReview(t *testing.T) {
	file, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(file.Close)
	pg := newTestPGMetrics(t, newMetricsTestDB(t))
	rows := []RequestRecord{
		{EventID: "parity-1", Time: 1700000000, AccountID: "same-account", Account: "Zulu", KeyID: "same-key", KeyName: "Zulu", Groups: []string{"group-a", "group-a", ""}, Model: "synthetic", Status: "ok", InputTokens: 10, OutputTokens: 2},
		{EventID: "parity-2", Time: 1700000001, AccountID: "same-account", Account: "\u00e1lpha", KeyID: "same-key", KeyName: "\u00e1lpha", Groups: []string{"group-a", "group-b"}, Model: "synthetic", Status: "error"},
		{EventID: "parity-3", Time: 1700000002, AccountID: "other-account", Account: "\u00e1lpha", KeyID: "other-key", KeyName: "\u00e1lpha", Model: "synthetic", Status: "ok"},
		{EventID: "parity-4", Time: 1700000003, Account: "legacy", KeyName: "legacy", Model: "synthetic", Status: "aborted"},
	}
	for _, row := range rows {
		file.Record(row)
		pg.Record(row)
	}
	if err := pg.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"from=1699999980&to=1700000040&step=60", "account_id=same-account", "group_id=group-a", "model=missing&from=1699999980&to=1700000100&step=60"} {
		t.Run(query, func(t *testing.T) {
			q := managedQuery(t, query)
			a, err := file.managedLogStats(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			b, err := pg.managedLogStats(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			a.CheckedAt, b.CheckedAt = 0, 0
			rawA, _ := json.Marshal(a)
			rawB, _ := json.Marshal(b)
			var viewA, viewB map[string]any
			if json.Unmarshal(rawA, &viewA) != nil || json.Unmarshal(rawB, &viewB) != nil {
				t.Fatal("invalid stats JSON")
			}
			if !reflect.DeepEqual(viewA, viewB) {
				for key, value := range viewA {
					if !reflect.DeepEqual(value, viewB[key]) {
						t.Errorf("file/PG differs at %s: file=%v PG=%v", key, value, viewB[key])
					}
				}
				t.Fatal("file/PG aggregation differs")
			}
			for _, group := range b.Groups {
				if group.ID == "" {
					t.Fatal("empty group counted")
				}
			}
			if query == "account_id=same-account" && (b.Requests != 2 || len(b.Accounts) != 1 || b.Accounts[0].Name != "\u00e1lpha" || len(b.Keys) != 1 || b.Keys[0].Name != "\u00e1lpha") {
				t.Fatal("stable identity changed or names depend on database locale")
			}
			if q.Model == "missing" && (b.Requests != 0 || len(b.Accounts) != 0 || len(b.Keys) != 0 || len(b.Groups) != 0 || len(b.Series) != 2) {
				t.Fatal("empty range is not empty or missing zero buckets")
			}
		})
	}
}
