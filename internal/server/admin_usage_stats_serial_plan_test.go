package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retain only the explicitly named isolated performance schema. A failed plan
// can then be investigated with --reuse instead of recreating hundreds of
// thousands of rows. The companion tool performs checked explicit cleanup.
func classStatsRetainedFixture(t *testing.T, count int) (*Database, bool) {
	t.Helper()
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	if !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) {
		t.Fatal("retained fixture requires an isolated performance schema")
	}
	bootstrap, err := pgx.Connect(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer bootstrap.Close(t.Context())
	var database string
	if err := bootstrap.QueryRow(t.Context(), "SELECT current_database()").Scan(&database); err != nil || database != "codex_agent_integration" {
		t.Fatal("retained fixture requires the isolated integration database", err)
	}
	var exists bool
	if err := bootstrap.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := bootstrap.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 3
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["jit"] = "off"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	db := &Database{Pool: pool}
	if !exists {
		if err := db.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `CREATE TABLE class_serial_fixture_meta(version text PRIMARY KEY,rows bigint NOT NULL,complete boolean NOT NULL);
 INSERT INTO class_serial_fixture_meta VALUES('13class-v1',$1,false)`, pgx.QueryExecModeSimpleProtocol, count); err != nil {
			t.Fatal(err)
		}
		return db, false
	}
	var version string
	var rows int
	var complete bool
	if err := pool.QueryRow(t.Context(), "SELECT version,rows,complete FROM class_serial_fixture_meta").Scan(&version, &rows, &complete); err != nil || version != "13class-v1" || rows != count || !complete {
		t.Fatalf("retained fixture is incomplete/different; explicit cleanup required: version=%s rows=%d complete=%t err=%v", version, rows, complete, err)
	}
	return db, true
}

// This opt-in static read fixture copies one real trigger-built minute. Its
// dimensional summaries and compact projection are copied exactly, while only
// event identity/time change. It is never an ingestion-throughput benchmark.
func TestPerformanceClassStatsSerialDensePlan(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated dense-class execution policy probe")
	}
	count := envInt("PERFORMANCE_SLO_ROWS", 700000, 1000, 1000000)
	if count%1000 != 0 {
		t.Fatal("dense fixture rows must be divisible by 1000")
	}
	db, reused := classStatsRetainedFixture(t, count)
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var fixtureSchema string
	if err := conn.QueryRow(t.Context(), "SELECT current_schema()").Scan(&fixtureSchema); err != nil {
		t.Fatal(err)
	}
	performanceSLOEmit(t, "CLASS_SERIAL_SCHEMA", map[string]any{"schema": fixtureSchema, "retained": true, "reused": reused})
	if _, err := conn.Exec(t.Context(), "SET statement_timeout='120s'; SET jit=off; SET work_mem='4MB'; SET max_parallel_workers_per_gather=0"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if !reused {
		_, err = conn.Exec(t.Context(), `WITH input AS MATERIALIZED (
 SELECT i,'serial-base-'||i AS event_id,to_timestamp(1790553600+i%50) AS created_at,
 'error'::text AS status,'model-'||(i%2) AS model FROM generate_series(0,999)i
 ), e AS (INSERT INTO usage_events(event_id,created_at,status) SELECT event_id,created_at,status FROM input RETURNING event_id),
 payload AS (SELECT repeat(string_agg(md5(j::text),''),8) AS value FROM generate_series(1,48)j)
 INSERT INTO request_logs(event_id,created_at,status,model,account_name,key_name,record)
 SELECT input.event_id,created_at,status,model,'account-'||(i%10),'key-'||(i%5),
 jsonb_build_object('event_id',input.event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model',model,
 'account_id','a'||(i%10),'key_id','k'||(i%5),'groups',jsonb_build_array('g'||(i%3),'g'||(i%3)),'endpoint','responses',
 'input_tokens',101,'output_tokens',13,'cached_tokens',37,'usage_known',true,'latency_ms',21,'latency_known',true,'ttft_ms',3,
 'upstream_model','upstream-model-fixture','sent_model','sent-model-fixture','returned_model','returned-model-fixture',
 'inbound_path','/v1/responses','upstream_path','/backend-api/codex/responses','response_transfer_ms',13,'authentication_ms',1,
 'http_code',CASE WHEN i%5<>4 THEN 429 WHEN (i/5)%12=11 THEN 403 ELSE 400 END,
 'error',CASE WHEN i%5<>4 THEN 'rate limit exceeded' ELSE
 (ARRAY['unsupported parameter','system messages are not allowed','invalidated oauth','model is not supported','invalid encrypted content','exceeds 32 mib',
 'flagged for possible','no enabled account','overloaded','an error occurred while processing','timeout','authentication failed'])[(i/5)%12+1] END,
 'unrelated_protocol_payload',payload.value) FROM input JOIN e USING(event_id) CROSS JOIN payload;
 CREATE TEMP TABLE serial_templates AS SELECT l.*,a.record AS compact FROM request_logs l JOIN request_log_analytics a ON a.log_id=l.id;
 CREATE TEMP TABLE serial_minute AS SELECT * FROM retained_usage_minute;
 ALTER TABLE request_logs DISABLE TRIGGER USER; ALTER TABLE usage_events DISABLE TRIGGER USER`)
		if err != nil {
			t.Fatal(err)
		}
		var templateClasses, templateMatches int
		if err := conn.QueryRow(t.Context(), `SELECT count(DISTINCT request_log_error_classes(record,status)->>'category'),
 count(*) FILTER(WHERE request_log_error_classes(record,status)->>'category'='rate_limit') FROM request_logs`).Scan(&templateClasses, &templateMatches); err != nil || templateClasses != 13 || templateMatches != 800 {
			t.Fatalf("small template classification is not representative: classes=%d rate_limit=%d err=%v", templateClasses, templateMatches, err)
		}
		for first := 1; first < count/1000; first += 10 {
			last := min(first+9, count/1000-1)
			_, err = conn.Exec(t.Context(), `WITH input AS MATERIALIZED (
 SELECT 'serial-'||m||'-'||t.id AS event_id,t.created_at+m*interval '1 minute' AS created_at,t.status,t.model,t.account_name,t.key_name,t.compact,
 t.record||jsonb_build_object('event_id','serial-'||m||'-'||t.id,'time',extract(epoch FROM t.created_at+m*interval '1 minute')::bigint) AS record
 FROM serial_templates t CROSS JOIN generate_series($1::int,$2::int)m
 ), e AS (INSERT INTO usage_events(event_id,created_at,status) SELECT event_id,created_at,status FROM input RETURNING event_id),
 logs AS (INSERT INTO request_logs(event_id,created_at,status,model,account_name,key_name,record)
 SELECT i.event_id,i.created_at,i.status,i.model,i.account_name,i.key_name,i.record FROM input i JOIN e USING(event_id) RETURNING id,event_id)
 INSERT INTO request_log_analytics(log_id,record) SELECT l.id,i.compact FROM logs l JOIN input i USING(event_id)`, first, last)
			if err != nil {
				t.Fatal(err)
			}
			if last%100 == 0 || last == count/1000-1 {
				performanceSLOEmit(t, "CLASS_SERIAL_SEED", map[string]any{"rows": (last + 1) * 1000, "elapsed_s": time.Since(start).Seconds()})
			}
		}
		// Copy every counter/dimension from the complete template minute. The
		// retained_usage_minute trigger maintains its corresponding daily rows.
		columns := retainedUsageInsertColumns("bucket", "status", "kind", "resource_id", "name")
		_, err = conn.Exec(t.Context(), `INSERT INTO retained_usage_minute(`+columns+`)
 SELECT bucket+m*60,status,kind,resource_id,name,`+managedUsageRollupColumns+` FROM serial_minute CROSS JOIN generate_series(1,$1::int-1)m;
 ALTER TABLE request_logs ENABLE TRIGGER USER; ALTER TABLE usage_events ENABLE TRIGGER USER`, pgx.QueryExecModeSimpleProtocol, count/1000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), `INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-serial-plan',id FROM request_logs;
 ANALYZE request_logs; ANALYZE request_log_analytics; ANALYZE management_cleanup_targets; ANALYZE retained_usage_minute; ANALYZE retained_usage_unsafe_minute`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), "UPDATE class_serial_fixture_meta SET complete=true"); err != nil {
			t.Fatal(err)
		}
	}
	var rows, matching, summaryTotal, summaryClass, classCount int64
	var originalBytes, thinBytes float64
	if err := conn.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE (record->>'http_code')='429'),avg(pg_column_size(record)) FROM request_logs`).Scan(&rows, &matching, &originalBytes); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT avg(pg_column_size(record)) FROM request_log_analytics`).Scan(&thinBytes); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT sum(requests) FILTER(WHERE kind='total'),sum(requests) FILTER(WHERE kind='category' AND name='rate_limit') FROM retained_usage_minute`).Scan(&summaryTotal, &summaryClass); err != nil || rows != int64(count) || matching != int64(count)*4/5 || summaryTotal != rows || summaryClass != matching {
		t.Fatalf("fixture source/summary mismatch rows=%d class=%d total=%d summary_class=%d err=%v", rows, matching, summaryTotal, summaryClass, err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count(DISTINCT name) FROM retained_usage_minute WHERE kind='category'`).Scan(&classCount); err != nil || classCount != 13 {
		t.Fatalf("fixture must reproduce generic-plan class underestimation: classes=%d err=%v", classCount, err)
	}
	performanceSLOEmit(t, "CLASS_SERIAL_FIXTURE", map[string]any{"rows": rows, "matching": matching, "distinct_classes": classCount, "original_avg_bytes": originalBytes, "thin_avg_bytes": thinBytes, "seed_s": time.Since(start).Seconds(), "scope": "static disposable schema; 1000 real-trigger template rows with exact copied projections/minute summaries; unused permanent-event/model aggregates are not benchmarked; no production access"})
	if _, err := conn.Exec(t.Context(), "SET statement_timeout='15s'; SET plan_cache_mode=force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PERFORMANCE_SLO_CASES") == "union" {
		exerciseAnalyticsUnionPlans(t, db, conn, matching)
		return
	}
	for _, snapshot := range []bool{false, true} {
		q := managedQuery(t, "error_category=rate_limit&timezone=Asia%2FShanghai")
		if snapshot {
			q.AsOf, q.SnapshotID = "usage-serial-plan", "usage-serial-plan"
		}
		capture := &analyticsNarrowCapture{pool: db.Pool}
		raw := q
		raw.statsQueryer = capture
		out, err := (&Metrics{db: db}).managedLogStats(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		fast, fastArgs := managedUsageRollupQuery(q, out)
		statement, args := managedUsageAdaptiveClassSQL(q, fast, fastArgs, capture.sql, capture.args)
		branchSQL, branchArgs := managedUsageAdaptiveClassSQL(q, "SELECT 'fast'", nil, "SELECT 'raw'", nil)
		var branch string
		if err := conn.QueryRow(t.Context(), branchSQL, branchArgs...).Scan(&branch); err != nil || branch != "raw" {
			t.Fatalf("dense exact fixture did not select raw: %s %v", branch, err)
		}
		var expectedSignature string
		for _, mode := range []string{"narrow_serial", "narrow_parallel_private_hash", "narrow_parallel", "wide_parallel"} {
			sql, workers := statement, 2
			if mode == "narrow_serial" {
				workers = 0
			}
			if mode == "wide_parallel" {
				sql = analyticsWideBaselineSQL(statement)
			}
			parallelHash := "on"
			if mode == "narrow_parallel_private_hash" {
				parallelHash = "off"
			}
			if _, err := conn.Exec(t.Context(), fmt.Sprintf("SET max_parallel_workers_per_gather=%d; SET enable_parallel_hash=%s", workers, parallelHash)); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			result, queryErr := conn.Query(t.Context(), sql, args...)
			var encoded []string
			var total int64
			if queryErr == nil {
				for result.Next() {
					values, err := result.Values()
					if err != nil {
						queryErr = err
						break
					}
					if values[0] == "total" {
						switch value := values[4].(type) {
						case int64:
							total = value
						case pgtype.Numeric:
							integer, err := value.Int64Value()
							if err != nil || !integer.Valid {
								t.Fatalf("invalid total counter: %v", err)
							}
							total = integer.Int64
						default:
							t.Fatalf("unexpected total type %T", values[4])
						}
					}
					value, err := json.Marshal(values)
					if err != nil {
						t.Fatal(err)
					}
					encoded = append(encoded, string(value))
				}
				if queryErr == nil {
					queryErr = result.Err()
				}
				result.Close()
			}
			sort.Strings(encoded)
			digest := sha256.Sum256([]byte(strings.Join(encoded, "\n")))
			signature := hex.EncodeToString(digest[:])
			entry := map[string]any{"case": fmt.Sprintf("snapshot_%t", snapshot), "mode": mode, "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000, "returned_rows": len(encoded), "signature": signature, "total_requests": total, "sql": sql, "args": args, "http_slo_proven": false}
			if queryErr != nil {
				entry["query_error"] = queryErr.Error()
			} else {
				if mode == "narrow_serial" {
					expectedSignature = signature
				}
				if total != matching || expectedSignature != signature {
					t.Fatalf("execution policy changed results: %v", entry)
				}
			}
			// Preserve a failure as evidence rather than retrying the same costly
			// query. Successful modes get one actual plan at the same 15s budget.
			if queryErr == nil {
				var plan json.RawMessage
				if err := conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
					entry["plan_error"] = err.Error()
				} else {
					entry["plan"] = plan
				}
			}
			performanceSLOEmit(t, "CLASS_SERIAL_RESULT", entry)
			if mode == "narrow_serial" && queryErr != nil {
				t.Fatalf("serial query still exceeds bounded execution or fails: %v", queryErr)
			}
		}
	}
	// The wrapper is the runtime guarantee. The preceding direct-session probes
	// compare only plans; this check runs the same scope on the acquired pool.
	rowsScoped, err := (managedLogQuery{ErrorCategory: "rate_limit"}).managedStatsQueryer(&Metrics{db: db}).Query(t.Context(), "SELECT current_setting('max_parallel_workers_per_gather')")
	if err != nil || !rowsScoped.Next() {
		t.Fatal("scoped serial query failed", err)
	}
	var workers string
	if err := rowsScoped.Scan(&workers); err != nil || workers != "2" {
		t.Fatalf("runtime policy unexpectedly disabled scan parallelism: workers=%s err=%v", workers, err)
	}
	rowsScoped.Close()
}
