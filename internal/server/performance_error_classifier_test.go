package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exercise the actual proposed migration body under an isolated name. The
// deployed function and indexes remain untouched by this cost-only probe.
func classifierExperimentSQL(t *testing.T) string {
	t.Helper()
	raw, err := migrationFiles.ReadFile("025_request_log_error_classes_plpgsql.sql")
	if err != nil {
		t.Fatal(err)
	}
	const from = "CREATE OR REPLACE FUNCTION request_log_error_classes("
	if strings.Count(string(raw), from) != 1 {
		t.Fatal("migration signature changed")
	}
	return strings.Replace(string(raw), from, "CREATE FUNCTION classifier_experiment(", 1)
}

func TestPerformanceClassifierWholeStats(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated retained-fixture comparison required")
	}
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	if schema != "perf_slo_20260929t230847_15601a" {
		t.Skip("use reviewed retained100k fixture via --reuse")
	}
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	config.MinConns = 0
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["jit"] = "off"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var database, marker string
	var rowsCount int
	var complete bool
	if err := conn.QueryRow(t.Context(), `SELECT current_database(),version,rows,complete FROM class_serial_fixture_meta`).Scan(&database, &marker, &rowsCount, &complete); err != nil || database != "codex_agent_integration" || marker != "13class-v1" || rowsCount != 100000 || !complete {
		t.Fatal("retained fixture guard", err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "SET LOCAL statement_timeout='15s';SET LOCAL lock_timeout='1s';SET LOCAL plan_cache_mode=force_generic_plan;SET LOCAL enable_parallel_hash=off"); err != nil {
		t.Fatal(err)
	}
	old, _ := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	next, _ := migrationFiles.ReadFile("025_request_log_error_classes_plpgsql.sql")
	for _, snapshot := range []bool{false, true} {
		q := managedQuery(t, "error_category=rate_limit&timezone=Asia%2FShanghai&step=2592000")
		if snapshot {
			q.AsOf, q.SnapshotID = "usage-serial-plan", "usage-serial-plan"
		}
		capture := new(managedClassDiagnosticQueryer)
		rawQ := q
		rawQ.statsQueryer = capture
		_, _ = (&Metrics{db: &Database{}}).managedLogStats(t.Context(), rawQ)
		fast, fastArgs := managedUsageRollupQuery(q, managedUsageStats{Step: 2592000})
		statement, args := managedUsageAdaptiveClassSQL(q, fast, fastArgs, capture.sql, capture.args)
		var expected []string
		for sample, order := range [][]string{{"old", "new"}, {"new", "old"}} {
			for _, mode := range order {
				definition := old
				if mode == "new" {
					definition = next
				}
				if _, err := tx.Exec(t.Context(), string(definition)); err != nil {
					t.Fatal(err)
				}
				name := fmt.Sprintf("classifier_stats_%t_%d_%s", snapshot, sample, mode)
				if _, err := conn.Conn().Prepare(t.Context(), name, statement); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				rows, err := tx.Query(t.Context(), name, args...)
				if err != nil {
					t.Fatal(err)
				}
				values := []string{}
				for rows.Next() {
					row, err := rows.Values()
					if err != nil {
						t.Fatal(err)
					}
					data, _ := json.Marshal(row)
					values = append(values, string(data))
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				elapsed := float64(time.Since(start).Microseconds()) / 1000
				sort.Strings(values)
				if expected == nil {
					expected = values
				} else if strings.Join(values, "\n") != strings.Join(expected, "\n") {
					t.Fatal("whole statistics differ")
				}
				var generic, custom int
				if err := tx.QueryRow(t.Context(), "SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1", name).Scan(&generic, &custom); err != nil || generic != 1 || custom != 0 {
					t.Fatal("not exact preparedgenericplan", err)
				}
				performanceSLOEmit(t, "CLASSIFIER_STATS", map[string]any{"snapshot": snapshot, "sample": sample, "version": mode, "elapsed_ms": elapsed, "aggregate_rows": len(values), "fixture_rows": rowsCount, "generic_plans": generic, "custom_plans": custom, "scope": "complete actual aggregation on retained100k; function replacement is transaction-local and rolled back; no HTTP/production mutation"})
			}
		}
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPGErrorClassifierExecutionParity(t *testing.T) {
	db := newMetricsTestDB(t)
	// Even after025 is embedded, keep this probe's baseline explicitly022.
	oldDefinition, err := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), string(oldDefinition)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), classifierExperimentSQL(t)); err != nil {
		t.Fatal(err)
	}
	records, _ := managedSeverityContractRows()
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded", "unauthorized", "authentication_error", "serverOverloaded", "unmatched"} {
		for _, source := range []string{"", "gateway", "upstream"} {
			for _, status := range []string{"ok", "error", "aborted"} {
				records = append(records, RequestRecord{Status: status, HTTPCode: 200, Error: "unsupported parameter lower-priority hint", ErrorSource: source, FinalUpstreamErrorSource: "upstream", FinalUpstreamErrorCode: code})
			}
		}
	}
	encoded, _ := json.Marshal(records)
	var mismatch int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM jsonb_array_elements($1::jsonb) r(record)
 WHERE request_log_error_classes(record,record->>'status') IS DISTINCT FROM classifier_experiment(record,record->>'status')`, encoded).Scan(&mismatch); err != nil || mismatch != 0 {
		t.Fatalf("indexed values changed: %d %v", mismatch, err)
	}
	for _, raw := range []any{nil, `{}`, `null`, `[]`, `42`, `true`, `"rate limit"`, `{"http_code":null,"error":null}`, `{"http_code":"429","error":["rate limit"]}`, `{"error":{"message":"unsupported parameter"}}`, `{"http_code":"00429","error":false}`, `{"http_code":"+429","error":"unknown"}`, `{"http_code":"-1"}`} {
		for _, status := range []any{"ok", "error", "aborted", "", nil} {
			var equal bool
			if err := db.Pool.QueryRow(t.Context(), `SELECT request_log_error_classes($1::jsonb,$2::text) IS NOT DISTINCT FROM classifier_experiment($1::jsonb,$2::text)`, raw, status).Scan(&equal); err != nil || !equal {
				t.Fatalf("legacy shape changed: %v %v %v", raw, status, err)
			}
		}
	}
	// Compare exception SQLSTATE too, preserving lazy casts on success.
	if _, err := db.Pool.Exec(t.Context(), `CREATE FUNCTION classifier_exception(newer boolean,r jsonb,s text) RETURNS text LANGUAGE plpgsql AS $$
 BEGIN IF newer THEN PERFORM classifier_experiment(r,s); ELSE PERFORM request_log_error_classes(r,s); END IF; RETURN 'ok';
 EXCEPTION WHEN OTHERS THEN RETURN SQLSTATE; END $$`); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"invalid", "1.2", "99999999999999999999999999999"} {
		for _, status := range []string{"ok", "error", "aborted"} {
			raw, _ := json.Marshal(map[string]string{"http_code": code})
			var old, new string
			if err := db.Pool.QueryRow(t.Context(), `SELECT classifier_exception(false,$1::jsonb,$2),classifier_exception(true,$1::jsonb,$2)`, raw, status).Scan(&old, &new); err != nil || old != new {
				t.Fatalf("exception semantics changed: %s %s %v", old, new, err)
			}
		}
	}
	t.Logf("Preserved%d indexed classification vectors,65legacy/null shapes and9exception cases", len(records))
}

func TestPerformanceErrorClassifierExecution(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic function-cost probe")
	}
	db := newMetricsTestDB(t)
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	oldDefinition, err := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), string(oldDefinition)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), classifierExperimentSQL(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), `SET statement_timeout='15s';SET jit=off;SET max_parallel_workers_per_gather=0;
 CREATE TEMP TABLE classifier_input(shape text,id bigint,status text,record jsonb);
 INSERT INTO classifier_input SELECT 'thin',i,'error',jsonb_build_object('http_code',CASE WHEN i%5=0 THEN 401 ELSE 429 END,'error',CASE WHEN i%5=0 THEN 'authentication failed' ELSE 'rate limit exceeded' END) FROM generate_series(1,10000)i;
 INSERT INTO classifier_input SELECT 'wide',id,status,record||jsonb_build_object('unused_payload',repeat((SELECT string_agg(md5(j::text),'') FROM generate_series(1,64)j),8)) FROM classifier_input WHERE shape='thin';
 ANALYZE classifier_input`); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"thin", "wide"} {
		for sample, order := range [][]string{{"request_log_error_classes", "classifier_experiment"}, {"classifier_experiment", "request_log_error_classes"}} {
			for _, name := range order {
				start := time.Now()
				var count int
				if err := conn.QueryRow(t.Context(), "SELECT count(*) FILTER(WHERE "+name+"(record,status)->>'category'='rate_limit') FROM classifier_input WHERE shape=$1", shape).Scan(&count); err != nil || count != 8000 {
					t.Fatal("classifier query failed", count, err)
				}
				performanceSLOEmit(t, "CLASSIFIER_COST", map[string]any{"shape": shape, "function": name, "sample": sample, "records": 10000, "matched": count, "elapsed_ms": float64(time.Since(start).Microseconds()) / 1000, "scope": "isolated function aggregate only; no HTTP or production mutation"})
			}
		}
	}
	var body string
	if err := conn.QueryRow(t.Context(), `SELECT pg_get_functiondef('classifier_experiment(jsonb,text)'::regprocedure)`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "CLASSIFIER_EXPERIMENT_DEFINITION "+body)
}

func TestPGErrorClassifierMigrationPreservesStoredIndexes(t *testing.T) {
	db := newMetricsTestDB(t)
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	old, err := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	if err != nil {
		t.Fatal(err)
	}
	next, err := migrationFiles.ReadFile("025_request_log_error_classes_plpgsql.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), string(old)); err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(old), "CREATE OR REPLACE FUNCTION request_log_error_classes(", "CREATE FUNCTION classifier_before(", 1)
	if _, err := conn.Exec(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	records, _ := managedSeverityContractRows()
	raw, _ := json.Marshal(records)
	if _, err := conn.Exec(t.Context(), `CREATE TEMP TABLE classifier_index_rows(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,status text,record jsonb);
 INSERT INTO classifier_index_rows(status,record) SELECT record->>'status',record FROM jsonb_array_elements($1::jsonb) r(record)`, pgx.QueryExecModeSimpleProtocol, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `CREATE INDEX classifier_category_index ON classifier_index_rows((request_log_error_classes(record,status)->>'category'));
 CREATE INDEX classifier_severity_index ON classifier_index_rows((request_log_error_classes(record,status)->>'severity'));
 CREATE INDEX classifier_tags_index ON classifier_index_rows USING gin(request_log_error_classes(record,status) jsonb_path_ops);
 ANALYZE classifier_index_rows; SET enable_seqscan=off`); err != nil {
		t.Fatal(err)
	}
	var beforeOID, afterOID uint32
	if err := conn.QueryRow(t.Context(), `SELECT 'request_log_error_classes(jsonb,text)'::regprocedure::oid`).Scan(&beforeOID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), "BEGIN;"+string(next)+"ROLLBACK;"); err != nil {
		t.Fatal(err)
	}
	var language string
	if err := conn.QueryRow(t.Context(), `SELECT l.lanname FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang WHERE p.oid='request_log_error_classes(jsonb,text)'::regprocedure`).Scan(&language); err != nil || language != "sql" {
		t.Fatal("rolled-back025changedfunction", language, err)
	}
	if _, err := conn.Exec(t.Context(), string(next)); err != nil {
		t.Fatal(err)
	}
	var immutable, parallel string
	if err := conn.QueryRow(t.Context(), `SELECT oid,provolatile::text,proparallel::text FROM pg_proc WHERE oid='request_log_error_classes(jsonb,text)'::regprocedure`).Scan(&afterOID, &immutable, &parallel); err != nil || beforeOID != afterOID || immutable != "i" || parallel != "s" {
		t.Fatal("function identity/contract changed", err)
	}
	check := func() {
		t.Helper()
		for _, dim := range []string{"category", "severity"} {
			for _, key := range []string{"rate_limit", "authentication", "unknown", "P1", "P2", "P3", ""} {
				var equal bool
				query := fmt.Sprintf(`SELECT (SELECT coalesce(array_agg(id ORDER BY id),'{}'::bigint[]) FROM classifier_index_rows WHERE request_log_error_classes(record,status)->>'%s'=$1) IS NOT DISTINCT FROM (SELECT coalesce(array_agg(id ORDER BY id),'{}'::bigint[]) FROM classifier_index_rows WHERE classifier_before(record,status)->>'%s'=$1)`, dim, dim)
				if err := conn.QueryRow(t.Context(), query, key).Scan(&equal); err != nil || !equal {
					t.Fatal("preexisting index disagrees after migration", dim, key, err)
				}
			}
		}
		var plan []byte
		if err := conn.QueryRow(t.Context(), `EXPLAIN(FORMAT JSON) SELECT id FROM classifier_index_rows WHERE request_log_error_classes(record,status)->>'category'='rate_limit'`).Scan(&plan); err != nil || !strings.Contains(string(plan), "classifier_category_index") {
			t.Fatal("scalar stored index not used", err)
		}
		var equal bool
		if err := conn.QueryRow(t.Context(), `SELECT (SELECT coalesce(array_agg(id ORDER BY id),'{}'::bigint[]) FROM classifier_index_rows WHERE request_log_error_classes(record,status) @> '{"category":"rate_limit"}'::jsonb)=(SELECT coalesce(array_agg(id ORDER BY id),'{}'::bigint[]) FROM classifier_index_rows WHERE classifier_before(record,status) @> '{"category":"rate_limit"}'::jsonb)`).Scan(&equal); err != nil || !equal {
			t.Fatal("existing GIN index differs", err)
		}
	}
	check()
	if _, err := conn.Exec(t.Context(), `INSERT INTO classifier_index_rows(status,record) VALUES('error','{"http_code":429}'),('error','{"http_code":403}');
 UPDATE classifier_index_rows SET record=record||'{"http_code":504,"error":"timeout"}'::jsonb WHERE id<=3;
 DELETE FROM classifier_index_rows WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	check()
	// Rolling back a transaction that reinstalls022 restores025 and keeps
	// the already-stored expression indexes usable.
	if _, err := conn.Exec(t.Context(), "BEGIN;"+string(old)+"ROLLBACK;"); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestPGErrorClassifierExplicitCollationParity(t *testing.T) {
	db := newMetricsTestDB(t)
	old, err := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(old), "CREATE OR REPLACE FUNCTION request_log_error_classes(", "CREATE FUNCTION classifier_before(", 1)
	if _, err := db.Pool.Exec(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	next, err := migrationFiles.ReadFile("025_request_log_error_classes_plpgsql.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), string(next)); err != nil {
		t.Fatal(err)
	}
	collations := []string{`"C"`}
	var icu bool
	if err := db.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_collation WHERE collprovider='i')`).Scan(&icu); err != nil {
		t.Fatal(err)
	}
	if icu {
		if _, err := db.Pool.Exec(t.Context(), `CREATE COLLATION classifier_case_insensitive(provider=icu,locale='und-u-ks-level2',deterministic=false)`); err != nil {
			t.Fatal(err)
		}
		collations = append(collations, "classifier_case_insensitive")
	}
	for _, collation := range collations {
		for _, status := range []string{"ok", "OK", "error", "ERROR", "aborted"} {
			for _, code := range []string{"rateLimitExceeded", "RATELIMITEXCEEDED", "unauthorized", "UNAUTHORIZED", "serverOverloaded", "SERVEROVERLOADED", "rateLimitExceeded "} {
				raw, _ := json.Marshal(map[string]any{"http_code": 200, "error": "unmatched diagnostic", "error_source": "upstream", "final_upstream_error_source": "upstream", "final_upstream_error_code": code})
				var equal bool
				query := `SELECT classifier_before($1::jsonb,$2::text COLLATE ` + collation + `) IS NOT DISTINCT FROM request_log_error_classes($1::jsonb,$2::text COLLATE ` + collation + `)`
				if err := db.Pool.QueryRow(t.Context(), query, raw, status).Scan(&equal); err != nil || !equal {
					t.Fatalf("explicit collation changed code/source matching: %s/%s/%s %v", collation, status, code, err)
				}
			}
		}
	}
	t.Logf("Explicit collation parity: %v", collations)
}

func TestPerformanceClassifierDurableIngestion(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated durable ingestion comparison")
	}
	db := newMetricsTestDB(t)
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := migrationFiles.ReadFile("022_request_log_error_classes_decode.sql")
	next, _ := migrationFiles.ReadFile("025_request_log_error_classes_plpgsql.sql")
	for sample, order := range [][]string{{"old", "new"}, {"new", "old"}} {
		for _, mode := range order {
			definition := old
			if mode == "new" {
				definition = next
			}
			if _, err := db.Pool.Exec(t.Context(), string(definition)); err != nil {
				t.Fatal(err)
			}
			records := make([]RequestRecord, 1000)
			for i := range records {
				records[i] = RequestRecord{EventID: fmt.Sprintf("classifier_ingest_%d_%s_%d", sample, mode, i), Time: 1790553600 + int64(sample*3600+i%60), Status: "error", HTTPCode: 429, Error: "rate limit exceeded", Model: "synthetic", InputTokens: 100, OutputTokens: 1, UsageKnown: boolPointerWarmup(true), LatencyMs: 25}
			}
			start := time.Now()
			inserted, duplicates, err := metrics.ImportRecords(t.Context(), records)
			elapsed := float64(time.Since(start).Microseconds()) / 1000
			if err != nil || inserted != 1000 || duplicates != 0 {
				t.Fatal("durable ingress failed", inserted, duplicates, err)
			}
			var persisted int
			if err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM request_logs WHERE event_id LIKE $1`, fmt.Sprintf("classifier_ingest_%d_%s_%%", sample, mode)).Scan(&persisted); err != nil || persisted != 1000 {
				t.Fatal("durable request count mismatch", persisted, err)
			}
			performanceSLOEmit(t, "CLASSIFIER_INGEST", map[string]any{"sample": sample, "version": mode, "records": 1000, "elapsed_ms": elapsed, "scope": "actual durable metrics transaction including request indexes and retained summaries; not HTTP/provider throughput"})
		}
	}
}
