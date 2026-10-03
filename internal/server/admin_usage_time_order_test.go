package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestManagedUsageTimeOrderSQL(t *testing.T) {
	q := managedQuery(t, "")
	if q.sortSQL() != "created_at" {
		t.Fatalf("time ordering cannot use the existing timestamp index: %s", q.sortSQL())
	}
}

func TestManagedUsageTimeOrder(t *testing.T)   { exerciseManagedTimeOrder(t, false) }
func TestPGManagedUsageTimeOrder(t *testing.T) { exerciseManagedTimeOrder(t, true) }

func exerciseManagedTimeOrder(t *testing.T, pg bool) {
	m := managedUsageFixture(t, pg)
	ctx := context.Background()
	for i, stamp := range []int64{1700000000, 1700000003, 1699999999, 1700000001, 1700000003} {
		m.Record(RequestRecord{EventID: fmt.Sprintf("time-order-%d", i), Time: stamp, Model: "codex-a", Status: "ok", InputTokens: int64(i % 3), LatencyMs: int64(i % 2)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	var records []RequestRecord
	if err := m.walkManagedLogs(ctx, managedQuery(t, ""), func(r RequestRecord) error { records = append(records, r); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"time", "model", "tokens", "input_tokens", "output_tokens", "cached_tokens", "latency"} {
		for _, direction := range []string{"asc", "desc"} {
			t.Run(field+"_"+direction, func(t *testing.T) {
				q := managedQuery(t, "page_size=2&exact_total=true&sort_by="+field+"&sort_order="+direction)
				want := append([]RequestRecord(nil), records...)
				sort.Slice(want, func(i, j int) bool { return q.before(want[i], want[j]) })
				var got []RequestRecord
				for pageNumber := 0; pageNumber <= len(want); pageNumber++ {
					page, err := m.managedLogs(ctx, q)
					if err != nil || page.Total != int64(len(want)) {
						t.Fatalf("page %d: total=%d error=%v", pageNumber, page.Total, err)
					}
					for _, item := range page.Items {
						got = append(got, item.RequestRecord)
					}
					if !page.HasMore {
						break
					}
					q.Cursor = page.NextCursor
				}
				ids := func(rows []RequestRecord) []string {
					out := make([]string, len(rows))
					for i, r := range rows {
						out[i] = r.EventID
					}
					return out
				}
				if !reflect.DeepEqual(ids(got), ids(want)) {
					t.Fatalf("cursor order got=%v want=%v", ids(got), ids(want))
				}
				q.Cursor, q.Page = "", 3
				third, err := m.managedLogs(ctx, q)
				if err != nil || len(third.Items) != 2 || third.Items[0].EventID != want[4].EventID || third.Items[1].EventID != want[5].EventID {
					t.Fatalf("page jump: %+v %v", third, err)
				}
				q.Page = 1
				var output bytes.Buffer
				count, err := m.exportManagedLogsWithPrivacy(ctx, q, "jsonl", &output, false)
				if err != nil || count != int64(len(want)) {
					t.Fatalf("export: %d %v", count, err)
				}
				decoder := json.NewDecoder(&output)
				for _, expected := range want {
					var item RequestRecord
					if err := decoder.Decode(&item); err != nil || item.EventID != expected.EventID {
						t.Fatalf("export order: %+v want=%s error=%v", item, expected.EventID, err)
					}
				}
			})
		}
	}
	if pg {
		q := managedQuery(t, "page_size=2")
		first, err := m.managedLogs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		last := first.Items[len(first.Items)-1]
		legacy, _ := json.Marshal(managedLogCursor{Time: last.Time, ID: last.ID})
		q.Cursor = base64.RawURLEncoding.EncodeToString(legacy)
		oldPage, err := m.managedLogs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		q.Cursor = first.NextCursor
		current, err := m.managedLogs(ctx, q)
		if err != nil || !reflect.DeepEqual(oldPage.Items, current.Items) {
			t.Fatalf("legacy cursor changed: %v", err)
		}
	}
}

func TestPGManagedUsageTimeIndexPlan(t *testing.T) {
	db := newMetricsTestDB(t)
	ctx := context.Background()
	seed, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	// Fixture construction maintains all synchronous retained summaries. Give
	// only that transaction a bulk-load deadline; tested reads retain the pool's
	// normal statement timeout and their original index-plan assertions.
	if _, err = seed.Exec(ctx, "SET LOCAL statement_timeout='60s'"); err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(ctx, `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status)
 SELECT 'time-plan-'||i, to_timestamp(1700000000+i/3), 'ok' FROM generate_series(1,50000) i
 RETURNING event_id,created_at,status
) INSERT INTO request_logs(event_id,created_at,status,model,record)
 SELECT event_id,created_at,status,'synthetic',jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model','synthetic') FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "ANALYZE request_logs"); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"asc", "desc"} {
		q := managedQuery(t, "sort_order="+direction)
		orders := map[string]string{"previous": "extract(epoch FROM created_at)::bigint " + direction + ",created_at " + direction + ",id " + direction, "current": q.orderSQL()}
		for _, name := range []string{"previous", "current"} {
			var raw []byte
			statement := "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id,record FROM request_logs WHERE id<=50000 ORDER BY " + orders[name] + " LIMIT 51"
			if err := db.Pool.QueryRow(ctx, statement).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct {
				Plan      map[string]any `json:"Plan"`
				Execution float64        `json:"Execution Time"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatal("invalid plan", err)
			}
			var nodes, indexes []string
			var visit func(map[string]any)
			visit = func(plan map[string]any) {
				if node, ok := plan["Node Type"].(string); ok {
					nodes = append(nodes, node)
				}
				if index, ok := plan["Index Name"].(string); ok {
					indexes = append(indexes, index)
				}
				if children, ok := plan["Plans"].([]any); ok {
					for _, child := range children {
						visit(child.(map[string]any))
					}
				}
			}
			visit(plans[0].Plan)
			t.Logf("%s %s nodes=%v indexes=%v execution_ms=%.3f buffers=%v", name, direction, nodes, indexes, plans[0].Execution, plans[0].Plan["Shared Hit Blocks"])
			if name == "current" && (strings.Contains(strings.Join(nodes, ","), "Sort") || !stringIn("request_logs_time_idx", indexes...)) {
				t.Fatalf("timestamp index not used: %s", raw)
			}
		}
	}
}
