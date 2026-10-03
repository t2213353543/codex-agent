package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
)

// TEST_ACCOUNT_LAST_USED_EVENTS=10000000 exercises ten million durable events
// in an isolated schema. Both read plans return the same account metadata page.
func BenchmarkSystemPGAccountLastUsedScale(b *testing.B) {
	p, _ := accountPerformanceFixture(b)
	ctx := context.Background()
	eventCount := 100000
	if raw := os.Getenv("TEST_ACCOUNT_LAST_USED_EVENTS"); raw != "" {
		var err error
		eventCount, err = strconv.Atoi(raw)
		if err != nil || eventCount < 1 {
			b.Fatal("TEST_ACCOUNT_LAST_USED_EVENTS must be positive")
		}
	}
	if _, err := p.Store.DB.Pool.Exec(ctx, `INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms)
		SELECT 'sort-event-'||i,to_timestamp(1700000000+i),'perf-account-'||((i-1)%500+1),'synthetic','','ok',10,5,0,1 FROM generate_series(1,$1::bigint)i`, eventCount); err != nil {
		b.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(ctx, `INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record)
		SELECT account_ref,min(account_name),max(created_at),1,'{}'::jsonb FROM usage_events GROUP BY account_ref
		ON CONFLICT(account_ref) DO UPDATE SET last_at=EXCLUDED.last_at`); err != nil {
		b.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(ctx, "ANALYZE"); err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name, join, order string
	}{
		{"event_aggregate", "LEFT JOIN (SELECT account_ref,max(created_at) AS at FROM usage_events WHERE created_at>to_timestamp(0) GROUP BY account_ref) last_usage ON last_usage.account_ref=a.id", "last_usage.at"},
		{"durable_snapshot", "LEFT JOIN account_request_state last_usage ON last_usage.account_ref=a.id AND last_usage.last_at>to_timestamp(0)", "last_usage.last_at"},
	} {
		b.Run(fmt.Sprintf("events_%d/%s", eventCount, tc.name), func(b *testing.B) {
			suffix := tc.join + " ORDER BY " + tc.order + ` DESC NULLS LAST,a.id COLLATE "C" DESC LIMIT 50`
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				accounts, err := p.Store.queryAccounts(ctx, p.Store.DB.Pool, suffix, nil, false)
				if err != nil || len(accounts) != 50 {
					b.Fatalf("last-used page: %d %v", len(accounts), err)
				}
			}
		})
	}
	b.Run(fmt.Sprintf("events_%d/managed_endpoint", eventCount), func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			request := httptest.NewRequest(http.MethodGet, "/api/accounts/manage?sort_by=last_used_at&page_size=50", nil)
			response := httptest.NewRecorder()
			p.handleManagedAccounts(response, request)
			if response.Code != http.StatusOK {
				b.Fatalf("last-used endpoint: %d %s", response.Code, response.Body.String())
			}
		}
	})
}
