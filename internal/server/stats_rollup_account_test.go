package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManagedUsageAccountProjectionOrderedGuard(t *testing.T) {
	for _, filter := range []string{"", "status=ok", "status=failed", "from=1699999981&to=1700000002", "from=1700000100&to=1700000200&status=aborted"} {
		q := managedQuery(t, filter)
		q.accountsOnly, q.AccountIDs = true, []string{"a", "b"}
		statement, args, eligible := managedUsageAccountProjectionSQL(q, "SELECT $1::text", []any{"raw-sentinel"})
		if !eligible || len(args) < 2 || args[0] != "raw-sentinel" {
			t.Fatalf("projection argument prefix changed: %q", filter)
		}
		if !strings.HasPrefix(statement, "WITH account_identity_guard AS MATERIALIZED (SELECT NOT EXISTS(SELECT 1 FROM (SELECT created_at FROM request_logs WHERE account_ref IS DISTINCT FROM COALESCE(record->>'account_id','')") || !strings.Contains(statement, "ORDER BY created_at LIMIT 1 OFFSET 0) inconsistent) AS consistent)") {
			t.Fatal("identity consistency check lost its ordered bounded index probe")
		}
		if strings.Contains(statement, "plan_cache_mode") {
			t.Fatal("identity projection must not change connection planning state")
		}
	}
}

func TestPGManagedUsageAccountProjectionIdentityGuard(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for i := 0; i < 4; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("guard-account-%d", i), Time: 1700000000 + int64(i), AccountID: []string{"a", "b", "name:real", "outside"}[i], Account: fmt.Sprintf("Account %d", i), Status: "ok", InputTokens: int64(i + 10)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, filter := range []string{"", "status=ok", "status=failed", "from=1699999981&to=1700000002", "from=1700000100&to=1700000200"} {
			for _, ids := range [][]string{{"a", "b"}, {"name:real"}, {"legacy:legacy-name"}, {"outside"}, {"missing"}} {
				q := managedQuery(t, filter)
				q.accountsOnly, q.AccountIDs = true, ids
				got, err := m.managedLogStats(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				q.statsQueryer = db.Pool
				want, err := m.managedLogStats(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				a, _ := json.Marshal(got.Accounts)
				b, _ := json.Marshal(want.Accounts)
				if string(a) != string(b) {
					t.Fatalf("ids=%v filter=%q\ngot%s\nwant%s", ids, filter, a, b)
				}
			}
		}
	}
	check()
	// An outside account_ref masquerading as selected JSON identity must not
	// enter the selected projection. The reverse disagreement must not vanish.
	for _, statement := range []string{
		`UPDATE request_logs SET record=record||'{"account_id":"a"}'::jsonb WHERE event_id='guard-account-3'`,
		`UPDATE request_logs SET record=record||'{"account_id":"elsewhere"}'::jsonb WHERE event_id='guard-account-0'`,
		`UPDATE request_logs SET account_ref='legacy:legacy-name',record=record-'account_id' WHERE event_id='guard-account-1'`,
		`UPDATE request_logs SET account_ref='',record=record-'account_id' WHERE event_id='guard-account-2'`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		check()
	}
}
