package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type importBulkModeTrace struct {
	mu     sync.Mutex
	probes map[string]pgx.QueryExecMode
}

func (tr *importBulkModeTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "display_name=ANY($1::text[])") || strings.Contains(data.SQL, "SELECT DISTINCT ON(token_fingerprint)") {
		var mode pgx.QueryExecMode
		if len(data.Args) != 0 {
			mode, _ = data.Args[0].(pgx.QueryExecMode)
		}
		tr.mu.Lock()
		tr.probes[data.SQL] = mode
		tr.mu.Unlock()
	}
	return ctx
}

func (*importBulkModeTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *importBulkModeTrace) take() map[string]pgx.QueryExecMode {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	probes := tr.probes
	tr.probes = make(map[string]pgx.QueryExecMode)
	return probes
}

func importSingleConnectionStoreForTest(t *testing.T, trace pgx.QueryTracer) Store {
	t.Helper()
	s := accountPGTestStore(t)
	config := s.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "auto"
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Pool.Close()
	s.DB.Pool = pool
	return s
}

type importCachedPlanCounts struct {
	Name    string
	Custom  int64
	Generic int64
}

func importCachedPlanCountsForTest(t *testing.T, q accountSQL, sql string) importCachedPlanCounts {
	t.Helper()
	var result importCachedPlanCounts
	if err := q.QueryRow(t.Context(), `SELECT name,custom_plans,generic_plans FROM pg_prepared_statements WHERE statement=$1`, sql).Scan(&result.Name, &result.Custom, &result.Generic); err != nil {
		t.Fatal("cached native statement is missing", err)
	}
	return result
}

// Reproduce the actual cache history, not a fresh EXPLAIN: at 100 rows prepare
// the real native statements and execute each eight times (past the five-custom
// threshold), then grow without ANALYZE and invoke real import transactions.
// The old cache counters must stay untouched. Timings are diagnostic only and
// are not HTTP, 10-million-account, or SLO acceptance evidence.
func TestPGImportBulkCachedPlanGrowthRegression(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in stale-plan 30k fixture regression")
	}
	trace := &importBulkModeTrace{probes: make(map[string]pgx.QueryExecMode)}
	s := importSingleConnectionStoreForTest(t, trace)
	ctx := t.Context()
	if _, err := s.DB.Pool.Exec(ctx, `ALTER TABLE accounts SET (autovacuum_enabled=false); ALTER TABLE account_credentials SET (autovacuum_enabled=false)`); err != nil {
		t.Fatal(err)
	}
	seed := bulkImportFixtureItems(100, "cache-seed")
	if report, err := s.ImportDetailedContext(ctx, seed, AccountDefaults{}, ImportOptions{}); err != nil || report.Added != 100 {
		t.Fatalf("seed: added=%d err=%v", report.Added, err)
	}
	if _, err := s.DB.Pool.Exec(ctx, `ANALYZE accounts; ANALYZE account_credentials`); err != nil {
		t.Fatal(err)
	}
	if report, err := s.ImportDetailedContext(ctx, seed, AccountDefaults{}, ImportOptions{}); err != nil || report.Duplicate != 100 {
		t.Fatalf("capture real native probes: duplicates=%d err=%v", report.Duplicate, err)
	}
	probes := trace.take()
	if len(probes) != 2 {
		t.Fatalf("expected two actual native probes, got %d", len(probes))
	}
	missingNames, missingTokens, knownTokens := make([]string, 100), make([][]byte, 100), make([][]byte, 100)
	for i := range seed {
		missingNames[i] = fmt.Sprintf("cache-missing-%d", i)
		missingTokens[i] = tokenFingerprint(missingNames[i])
		knownTokens[i] = tokenFingerprint("synthetic-" + seed[i].Name)
	}
	warm := make(map[string]importCachedPlanCounts)
	for sql := range probes {
		for i := 0; i < 8; i++ {
			if strings.Contains(sql, "display_name=ANY") {
				var overlap, populated bool
				if err := s.DB.Pool.QueryRow(ctx, sql, pgx.QueryExecModeCacheStatement, missingNames, missingTokens).Scan(&overlap, &populated); err != nil || overlap || !populated {
					t.Fatalf("warm overlap: overlap=%t populated=%t err=%v", overlap, populated, err)
				}
			} else {
				rows, err := s.DB.Pool.Query(ctx, sql, pgx.QueryExecModeCacheStatement, knownTokens)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for rows.Next() {
					count++
				}
				rows.Close()
				if rows.Err() != nil || count != 100 {
					t.Fatalf("warm duplicates: count=%d err=%v", count, rows.Err())
				}
			}
		}
		warm[sql] = importCachedPlanCountsForTest(t, s.DB.Pool, sql)
		if state := warm[sql]; state.Custom < 5 || state.Generic < 1 {
			t.Fatalf("fixture failed to cache the old generic plan: %+v", state)
		}
	}
	trace.take()
	var known []ImportItem
	var knownReport ImportReport
	for i := 0; i < 3; i++ {
		items := bulkImportFixtureItems(10000, fmt.Sprintf("cache-grow-%d", i))
		started := time.Now()
		report, err := s.ImportDetailedContext(ctx, items, AccountDefaults{}, ImportOptions{})
		if err != nil || report.Added != 10000 {
			t.Fatalf("grow %d: added=%d err=%v", i, report.Added, err)
		}
		t.Logf("IMPORT_UNCACHED_GROW accounts=%d elapsed_ms=%.3f slo_proven=false", 10100+10000*i, float64(time.Since(started).Microseconds())/1000)
		if i == 0 {
			known, knownReport = items, report
		}
	}
	preview := bulkImportFixtureItems(10000, "cache-preview")
	for _, test := range []struct {
		name             string
		input            []ImportItem
		added, duplicate int
	}{
		{"new_preview", preview, 10000, 0},
		{"duplicates", known, 0, 10000},
		{"mixed_preview", append(append([]ImportItem(nil), known[:5000]...), preview[:5000]...), 5000, 5000},
	} {
		started := time.Now()
		report, err := s.ImportDetailedContext(ctx, test.input, AccountDefaults{}, ImportOptions{DryRun: true})
		if err != nil || report.Added != test.added || report.Duplicate != test.duplicate || report.Failed != 0 {
			t.Fatalf("%s: added=%d duplicate=%d failed=%d err=%v", test.name, report.Added, report.Duplicate, report.Failed, err)
		}
		for i := 0; i < test.duplicate; i++ {
			if report.Results[i].ID != knownReport.Results[i].ID {
				t.Fatalf("%s fingerprint/input ordering changed at %d", test.name, i)
			}
		}
		t.Logf("IMPORT_UNCACHED_REGRESSION case=%s inputs=10000 accounts=30100 elapsed_ms=%.3f slo_proven=false", test.name, float64(time.Since(started).Microseconds())/1000)
	}
	var count int
	if err := s.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&count); err != nil || count != 30100 {
		t.Fatalf("preview persisted rows: count=%d err=%v", count, err)
	}
	for sql, before := range warm {
		if after := importCachedPlanCountsForTest(t, s.DB.Pool, sql); after != before {
			t.Fatalf("native import reused stale named plan: before=%+v after=%+v", before, after)
		}
	}
	for sql, mode := range trace.take() {
		if mode != pgx.QueryExecModeExec {
			t.Fatalf("native bulk probe was not query-local uncached: mode=%v sql=%s", mode, sql)
		}
	}
}

// OAuth calls this same transaction join point. A one-connection pool catches
// accidental pool reacquisition; rollback and a caller-owned setting/marker
// prove imports neither commit the parent nor change its planner policy.
func TestPGImportBulkExternalTransactionPlannerState(t *testing.T) {
	s := importSingleConnectionStoreForTest(t, nil)
	for _, mode := range []string{"auto", "force_generic_plan", "force_custom_plan"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := s.DB.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `SELECT set_config('plan_cache_mode',$1,true)`, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO settings(key,value,version) VALUES('outer_import_marker','true',1)`); err != nil {
				t.Fatal(err)
			}
			parsed, report, err := prepareImport(bulkImportFixtureItems(100, "outer-"+mode), AccountDefaults{})
			if err != nil {
				t.Fatal(err)
			}
			report, err = s.pgImportDetailedTx(ctx, tx, parsed, report, ImportOptions{})
			if err != nil || report.Added != 100 {
				t.Fatalf("external transaction import: added=%d err=%v", report.Added, err)
			}
			duplicate := ImportReport{Results: make([]ImportResult, len(parsed))}
			duplicate, err = s.pgImportDetailedTx(ctx, tx, parsed, duplicate, ImportOptions{})
			if err != nil || duplicate.Duplicate != 100 {
				t.Fatalf("external transaction duplicate: duplicate=%d err=%v", duplicate.Duplicate, err)
			}
			var actual string
			if err := tx.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&actual); err != nil || actual != mode {
				t.Fatalf("parent planner policy changed: got=%q want=%q err=%v", actual, mode, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var accounts, markers int
			if err := s.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM settings WHERE key='outer_import_marker')`).Scan(&accounts, &markers); err != nil || accounts != 0 || markers != 0 {
				t.Fatalf("import escaped outer rollback: accounts=%d markers=%d err=%v", accounts, markers, err)
			}
			if err := s.DB.Pool.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&actual); err != nil || actual != "auto" {
				t.Fatalf("planner state leaked across pool reuse: got=%q err=%v", actual, err)
			}
		})
	}
}

type importCancelAfterLockTx struct {
	pgx.Tx
	cancel context.CancelFunc
}

func (tx importCancelAfterLockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := tx.Tx.Exec(ctx, sql, args...)
	if err == nil && strings.Contains(sql, "pg_advisory_xact_lock") {
		tx.cancel()
	}
	return tag, err
}

func TestPGImportBulkExternalTransactionCancellationKeepsPlanner(t *testing.T) {
	s := importSingleConnectionStoreForTest(t, nil)
	tx, err := s.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
		t.Fatal(err)
	}
	parsed, report, err := prepareImport(bulkImportFixtureItems(100, "outer-cancel"), AccountDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	before := report
	before.Results = append([]ImportResult(nil), report.Results...)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, err := s.pgImportDetailedTx(ctx, importCancelAfterLockTx{tx, cancel}, parsed, report, ImportOptions{})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, before) {
		t.Fatalf("canceled import changed report: err=%v", err)
	}
	var mode string
	var count int
	if err := tx.QueryRow(t.Context(), `SELECT current_setting('plan_cache_mode'),(SELECT count(*) FROM accounts)`).Scan(&mode, &count); err != nil || mode != "force_generic_plan" || count != 0 {
		t.Fatalf("canceled import poisoned caller: mode=%q rows=%d err=%v", mode, count, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Pool.QueryRow(t.Context(), `SHOW plan_cache_mode`).Scan(&mode); err != nil || mode != "auto" {
		t.Fatalf("cancellation leaked planner state to pool: mode=%q err=%v", mode, err)
	}
}

// Exercise a server-canceled probe as well as cancellation before the probe.
// PostgreSQL requires the owner to roll back an aborted transaction/savepoint;
// the import does not try to repair or commit someone else's transaction.
func TestPGImportBulkQueryCancellationKeepsCallerPlanner(t *testing.T) {
	s := importSingleConnectionStoreForTest(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := pgx.ConnectConfig(ctx, s.DB.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	blockTx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blockTx.Rollback(context.Background())
	if _, err := blockTx.Exec(ctx, `LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	outer, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Rollback(context.Background())
	if _, err := outer.Exec(ctx, `SET LOCAL plan_cache_mode=force_custom_plan`); err != nil {
		t.Fatal(err)
	}
	nested, err := outer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nested.Exec(ctx, `SET LOCAL statement_timeout='50ms'`); err != nil {
		t.Fatal(err)
	}
	parsed, report, err := prepareImport(bulkImportFixtureItems(2, "blocked-probe"), AccountDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pgImportDetailedTx(ctx, nested, parsed, report, ImportOptions{})
	var canceled *pgconn.PgError
	if !errors.As(err, &canceled) || canceled.Code != "57014" {
		t.Fatalf("expected blocked-query cancellation, got %v", err)
	}
	if err := nested.Rollback(ctx); err != nil {
		t.Fatal("caller could not roll back canceled savepoint", err)
	}
	if err := blockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var mode string
	var count int
	if err := outer.QueryRow(ctx, `SELECT current_setting('plan_cache_mode'),(SELECT count(*) FROM accounts)`).Scan(&mode, &count); err != nil || mode != "force_custom_plan" || count != 0 {
		t.Fatalf("query cancellation changed caller policy/data: mode=%q count=%d err=%v", mode, count, err)
	}
	if err := outer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Pool.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&mode); err != nil || mode != "auto" {
		t.Fatalf("server cancellation leaked planner state: mode=%q err=%v", mode, err)
	}
}
