package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This deliberately controls stale small-table statistics in a fresh synthetic
// schema. It is a planner diagnostic, not an HTTP or ingestion SLO benchmark.
// Statements are captured from actual native import calls, not SQL approximations.
func TestPerformanceAccountImportPlan(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated opt-in account import planner diagnostic")
	}
	s := accountPGTestStore(t)
	config := s.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "auto"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "60000"
	trace := new(overviewPlanTrace)
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Pool.Close()
	s.DB.Pool = pool
	if _, err := pool.Exec(t.Context(), "ALTER TABLE accounts SET (autovacuum_enabled=false); ALTER TABLE account_credentials SET (autovacuum_enabled=false)"); err != nil {
		t.Fatal(err)
	}
	items := func(start, count int) []ImportItem {
		out := make([]ImportItem, count)
		for i := range out {
			name := fmt.Sprintf("planner-account-%06d", start+i)
			raw, _ := json.Marshal(map[string]string{"access_token": "synthetic-" + name, "refresh_token": "refresh-" + name})
			out[i] = ImportItem{Name: name, Credentials: raw}
		}
		return out
	}
	initial := items(0, 100)
	report, err := s.ImportDetailedContext(t.Context(), initial, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Added != 100 {
		t.Fatal("seed", report, err)
	}
	if _, err := pool.Exec(t.Context(), "ANALYZE accounts; ANALYZE account_credentials"); err != nil {
		t.Fatal(err)
	}
	trace.mu.Lock()
	trace.statements = nil
	trace.mu.Unlock()
	report, err = s.ImportDetailedContext(t.Context(), initial, AccountDefaults{}, ImportOptions{})
	if err != nil || report.Duplicate != 100 {
		t.Fatal("capture native duplicate", report, err)
	}
	trace.mu.Lock()
	statements := append([]overviewPlanStatement(nil), trace.statements...)
	trace.mu.Unlock()
	var overlapSQL, duplicateSQL string
	for _, st := range statements {
		if strings.Contains(st.SQL, "display_name=ANY($1::text[])") && strings.Contains(st.SQL, "token_fingerprint=ANY($2::bytea[])") {
			overlapSQL = st.SQL
		}
		if strings.Contains(st.SQL, "SELECT DISTINCT ON(token_fingerprint)") && strings.Contains(st.SQL, "token_fingerprint=ANY($1::bytea[])") {
			duplicateSQL = st.SQL
		}
	}
	if overlapSQL == "" || duplicateSQL == "" {
		t.Fatal("native import statement capture failed; update diagnostic if query shape changed")
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{"native_import_overlap": overlapSQL, "native_import_duplicate": duplicateSQL} {
		if _, err := conn.Conn().Prepare(t.Context(), name, sql); err != nil {
			t.Fatal(err)
		}
	}
	missingArgs := func(count int) ([]string, [][]byte) {
		names := make([]string, count)
		tokens := make([][]byte, count)
		for i := range names {
			names[i] = fmt.Sprintf("absent-account-%06d", i)
			tokens[i] = tokenFingerprint("absent-token-" + names[i])
		}
		return names, tokens
	}
	knownFingerprints := func(count int) [][]byte {
		out := make([][]byte, count)
		for i := range out {
			out[i] = tokenFingerprint(fmt.Sprintf("synthetic-planner-account-%06d", i))
		}
		return out
	}
	missingNames, missingTokens := missingArgs(100)
	for i := 0; i < 8; i++ {
		var overlaps, populated bool
		if err := conn.QueryRow(t.Context(), "native_import_overlap", missingNames, missingTokens).Scan(&overlaps, &populated); err != nil || overlaps || !populated {
			t.Fatal("warm overlap", err)
		}
		rows, err := conn.Query(t.Context(), "native_import_duplicate", knownFingerprints(100))
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			count++
		}
		rows.Close()
		if rows.Err() != nil || count != 100 {
			t.Fatal("warm duplicates", count, rows.Err())
		}
	}
	conn.Release()
	for start := 100; start < 30100; start += 10000 {
		begun := time.Now()
		report, err := s.ImportDetailedContext(t.Context(), items(start, 10000), AccountDefaults{}, ImportOptions{})
		if err != nil || report.Added != 10000 {
			t.Fatal("grow controlled fixture", report, err)
		}
		performanceSLOEmit(t, "IMPORT_PLAN_SEED", map[string]any{"accounts": start + 10000, "elapsed_ms": float64(time.Since(begun).Microseconds()) / 1000, "slo_proven": false})
	}
	missingNames, missingTokens = missingArgs(10000)
	arguments := map[string][]any{"native_import_overlap": {missingNames, missingTokens}, "native_import_duplicate": {knownFingerprints(10000)}}
	sqls := map[string]string{"native_import_overlap": overlapSQL, "native_import_duplicate": duplicateSQL}
	for _, stage := range []string{"stale_100_row_statistics", "after_explicit_analyze"} {
		if stage == "after_explicit_analyze" {
			if _, err := pool.Exec(t.Context(), "ANALYZE accounts; ANALYZE account_credentials"); err != nil {
				t.Fatal(err)
			}
		}
		for _, mode := range []string{"auto_cached", "force_custom_plan", "force_generic_plan"} {
			usePool := pool
			if mode != "auto_cached" {
				modeConfig := config.Copy()
				modeConfig.ConnConfig.RuntimeParams["plan_cache_mode"] = mode
				usePool, err = pgxpool.NewWithConfig(t.Context(), modeConfig)
				if err != nil {
					t.Fatal(err)
				}
			}
			use, err := usePool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if mode != "auto_cached" {
				for name, sql := range sqls {
					if _, err := use.Conn().Prepare(t.Context(), name, sql); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, name := range []string{"native_import_overlap", "native_import_duplicate"} {
				before := importPreparedStateForTest(t, use, name)
				literals := make([]string, len(arguments[name]))
				for i, value := range arguments[name] {
					literals[i] = importPlanArrayLiteralForTest(t, value)
				}
				execute := "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) EXECUTE " + pgx.Identifier{name}.Sanitize() + "(" + strings.Join(literals, ",") + ")"
				var plan json.RawMessage
				started := time.Now()
				if err := use.QueryRow(t.Context(), execute, pgx.QueryExecModeSimpleProtocol).Scan(&plan); err != nil {
					t.Fatal("actual prepared plan", stage, mode, name, err)
				}
				after := importPreparedStateForTest(t, use, name)
				var stats json.RawMessage
				if err := use.QueryRow(t.Context(), `SELECT jsonb_agg(jsonb_build_object('table',c.relname,'reltuples',c.reltuples,'relpages',c.relpages,'n_live_tup',s.n_live_tup,'last_analyze',s.last_analyze,'last_autoanalyze',s.last_autoanalyze,'last_autovacuum',s.last_autovacuum)) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace LEFT JOIN pg_stat_user_tables s ON s.relid=c.oid WHERE n.nspname=current_schema() AND c.relname IN ('accounts','account_credentials')`).Scan(&stats); err != nil {
					t.Fatal(err)
				}
				performanceSLOEmit(t, "IMPORT_PREPARED_PLAN", map[string]any{"stage": stage, "mode": mode, "statement": name, "sql": sqls[name], "argument_elements": 10000, "actual_accounts": 30100, "prepared_before": before, "prepared_after": after, "statistics": stats, "plan": plan, "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000, "scope": "actual prepared native read SQL, no HTTP or Go credential decode; controlled stale stats; not SLO acceptance", "slo_proven": false})
			}
			use.Release()
			if mode != "auto_cached" {
				usePool.Close()
			}
		}
	}
}

func importPreparedStateForTest(t *testing.T, conn *pgxpool.Conn, name string) json.RawMessage {
	t.Helper()
	var result json.RawMessage
	if err := conn.QueryRow(t.Context(), "SELECT jsonb_build_object('name',name,'generic_plans',generic_plans,'custom_plans',custom_plans,'from_sql',from_sql,'parameter_types',parameter_types) FROM pg_prepared_statements WHERE name=$1", name).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
func importPlanArrayLiteralForTest(t *testing.T, value any) string {
	t.Helper()
	switch values := value.(type) {
	case []string:
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		return "ARRAY[" + strings.Join(out, ",") + "]::text[]"
	case [][]byte:
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = "decode('" + hex.EncodeToString(v) + "','hex')"
		}
		return "ARRAY[" + strings.Join(out, ",") + "]::bytea[]"
	default:
		t.Fatalf("unsupported import array %T", value)
		return ""
	}
}
