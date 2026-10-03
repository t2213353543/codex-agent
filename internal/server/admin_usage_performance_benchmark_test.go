package server

// This harness runs unchanged against the saved baseline source and the current
// tree. Data is synthetic and each PostgreSQL run owns a disposable schema.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func managedPerformanceRecord(i int) RequestRecord {
	value := int64(11)
	return RequestRecord{EventID: fmt.Sprintf("perf-%d", i), Time: 1700000000 + int64(i/3), AccountID: fmt.Sprintf("a%d", i%100), Account: fmt.Sprintf("Account %d", i%100), KeyID: fmt.Sprintf("k%d", i%10), KeyName: "key", Groups: []string{"g1", "g2"}, Model: fmt.Sprintf("m%d", i%8), UpstreamModel: "upstream", SentModel: "sent", ReturnedModel: "returned", InboundPath: "/v1/responses", UpstreamPath: "/responses", Endpoint: "responses", Status: "ok", HTTPCode: 200, InputTokens: int64(i%100 + 10), OutputTokens: 20, CachedTokens: 5, LatencyMs: int64(i % 1000), TTFTMs: &value, RequestPreparationMs: &value, AccountSelectionMs: &value, ProxySelectionMs: &value, AuthenticationMs: &value, ResponseTransferMs: &value, ImageInputTokens: &value, ImageOutputTokens: &value}
}

func managedPerformanceMetrics(b *testing.B, pg bool) *Metrics {
	b.Helper()
	if !pg {
		m, err := NewMetrics(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(m.Close)
		enc := json.NewEncoder(m.logFile)
		for i := 0; i < 10000; i++ {
			if err := enc.Encode(managedPerformanceRecord(i)); err != nil {
				b.Fatal(err)
			}
		}
		return m
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	bootstrap, err := pgx.Connect(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		b.Fatal(err)
	}
	schema := "usage_perf_" + hex.EncodeToString(nonce[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := bootstrap.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		b.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		b.Fatal(err)
	}
	config.MaxConns = 2
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { pool.Close(); bootstrap.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); bootstrap.Close(ctx) })
	db := &Database{Pool: pool}
	if err := db.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	raw, _ := json.Marshal(managedPerformanceRecord(0))
	_, err = pool.Exec(ctx, `WITH events AS (
        INSERT INTO usage_events(event_id,created_at,status)
        SELECT 'perf-'||i,to_timestamp(1700000000+i/3),'ok' FROM generate_series(1,30000) i
        RETURNING event_id,created_at,status
    ) INSERT INTO request_logs(event_id,created_at,account_ref,account_name,key_name,status,model,record)
    SELECT event_id,created_at,'a'||(substring(event_id from 6)::int%100),'Account '||(substring(event_id from 6)::int%100),'key',status,'m'||(substring(event_id from 6)::int%8),
        $1::jsonb || jsonb_build_object('event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'account_id','a'||(substring(event_id from 6)::int%100),'account','Account '||(substring(event_id from 6)::int%100),'model','m'||(substring(event_id from 6)::int%8),'input_tokens',substring(event_id from 6)::int%100+10,'latency_ms',substring(event_id from 6)::int%1000)
    FROM events`, string(raw))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE request_logs"); err != nil {
		b.Fatal(err)
	}
	m, err := NewPGMetrics(db)
	if err != nil {
		b.Fatal(err)
	}
	return m
}

func BenchmarkManagedUsageAggregationSamples(b *testing.B) {
	record := managedPerformanceRecord(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var totals managedUsageTotals
		for j := 0; j < 10000; j++ {
			totals.add(record)
		}
		if totals.Requests != 10000 {
			b.Fatal(totals.Requests)
		}
	}
}

func BenchmarkPGMetricsRecordHourlyRollup(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "baseline"
		if enabled {
			name = "hourly_rollup"
		}
		b.Run(name, func(b *testing.B) {
			m := managedPerformanceMetrics(b, true)
			ctx := context.Background()
			if !enabled {
				// Each case owns its disposable schema. The baseline disables only
				// the new derived summary so it measures this change's write cost.
				if _, err := m.db.Pool.Exec(ctx, "ALTER TABLE usage_minute DISABLE TRIGGER usage_minute_hourly_change"); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := managedPerformanceRecord(i)
				r.EventID, r.AccountID = fmt.Sprintf("write-hourly-%d", i), ""
				if _, err := m.persistRecord(ctx, r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkManagedUsageEndpoints(b *testing.B) {
	for _, pg := range []bool{false, true} {
		name := "file_10k"
		if pg {
			name = "pg_30k"
		}
		b.Run(name, func(b *testing.B) {
			m := managedPerformanceMetrics(b, pg)
			ctx := context.Background()
			b.Run("account_batch_10", func(b *testing.B) {
				p := &Proxy{Metrics: m}
				body := []byte(`{"ids":["a0","a1","a2","a3","a4","a5","a6","a7","a8","a9"],"from":1700000000,"to":1700086400}`)
				// Validate the actual filtered HTTP response before timing. In PG,
				// AccountIDs filters the stable account_ref column, not JSON data.
				check := httptest.NewRecorder()
				p.handleManagedAccountUsageBatch(check, httptest.NewRequest("POST", "/api/accounts/usage-batch", bytes.NewReader(body)))
				var result struct {
					Items []managedUsageResource `json:"items"`
				}
				if check.Code != 200 || json.Unmarshal(check.Body.Bytes(), &result) != nil || len(result.Items) != 10 {
					b.Fatalf("invalid populated fixture: status=%d body=%s", check.Code, check.Body.String())
				}
				wantRequests := int64(100)
				if pg {
					wantRequests = 300
				}
				for i, item := range result.Items {
					if item.ID != fmt.Sprintf("a%d", i) || item.Requests != wantRequests || item.Tokens != wantRequests*int64(i+30) || item.UsageKnownRequests != wantRequests {
						b.Fatalf("populated fixture returned incorrect account statistics: %+v; want requests=%d", item, wantRequests)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					req := httptest.NewRequest("POST", "/api/accounts/usage-batch", bytes.NewReader(body))
					w := httptest.NewRecorder()
					p.handleManagedAccountUsageBatch(w, req)
					if w.Code != 200 {
						b.Fatal(w.Code, w.Body.String())
					}
				}
			})
			if !pg {
				return
			}
			for _, id := range []string{"perf-15000", "15000"} {
				b.Run("detail_"+id, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if _, err := m.managedLogDetail(ctx, id); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			for _, spec := range []struct {
				page int
				sort string
			}{{1, "time"}, {20, "time"}, {100, "time"}, {20, "tokens"}} {
				b.Run(fmt.Sprintf("page_%d_%s", spec.page, spec.sort), func(b *testing.B) {
					q, err := parseManagedLogQuery(url.Values{"page": {fmt.Sprint(spec.page)}, "size": {"50"}, "sort_by": {spec.sort}, "timezone": {"UTC"}})
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						page, err := m.managedLogs(ctx, q)
						if err != nil || len(page.Items) != 50 {
							b.Fatal(len(page.Items), err)
						}
					}
				})
			}
		})
	}
}
