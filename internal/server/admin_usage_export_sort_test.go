package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestManagedUsageExternalSort(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	enc := json.NewEncoder(m.logFile)
	for i := 0; i < 1541; i++ {
		if err = enc.Encode(RequestRecord{Time: 1700000000 + int64(i%7), EventID: fmt.Sprintf("record-%d", i), Model: fmt.Sprintf("model-%d", i%3), OutputTokens: int64(i % 13)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"sort_by=tokens&sort_order=asc", "sort_by=model&sort_order=desc", "sort_by=time&sort_order=asc"} {
		q := managedQuery(t, raw)
		var previous *RequestRecord
		seen := map[string]bool{}
		err = m.walkManagedLogsOrdered(context.Background(), q, func(r RequestRecord) error {
			if previous != nil && q.before(r, *previous) {
				t.Fatal("merge order invalid")
			}
			if seen[r.EventID] {
				t.Fatal("duplicate export row")
			}
			seen[r.EventID] = true
			previous = &r
			return nil
		})
		if err != nil || len(seen) != 1541 {
			t.Fatalf("external sort: %v %d", err, len(seen))
		}
	}
}
