package server

// Opt-in integration SLO evidence. No request in this test contacts an upstream.
// The fixture is retained in its uniquely named synthetic schema for source-
// matched reruns; the Python driver records that schema and owns cleanup.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func performanceSLOEmit(t *testing.T, kind string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PERFORMANCE_SLO_%s %s", kind, raw)
}

func TestPerformanceSLOScale(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("run tools/performance_slo.py for isolated synthetic SLO evidence")
	}
	ctx := context.Background()
	dsn, schema := os.Getenv("TEST_DATABASE_URL"), os.Getenv("PERFORMANCE_SLO_SCHEMA")
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/codex_agent_integration" || u.User == nil || u.User.Username() != "codex_agent_test" || u.Port() != "25432" || !stringIn(u.Hostname(), "localhost", "127.0.0.1", "::1") || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) {
		t.Fatal("dedicated integration database and perf_slo schema required")
	}
	rows := envInt("PERFORMANCE_SLO_ROWS", 10000000, 1000, 10000000)
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 20, 1, 200)
	phase := os.Getenv("PERFORMANCE_SLO_PHASE")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var present bool
	if err = admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		if phase == "measure" {
			t.Fatal("fixture schema does not exist")
		}
		if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)), "slo-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Only fixture construction/migration uses an extended statement deadline.
	seedConn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seedConn.Exec(ctx, "SET statement_timeout='30min'"); err != nil {
		t.Fatal(err)
	}
	if _, err = seedConn.Exec(ctx, "SET maintenance_work_mem='512MB'"); err != nil {
		t.Fatal(err)
	}
	migrationStarted := time.Now()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	performanceSLOEmit(t, "MIGRATION", map[string]any{"elapsed_s": time.Since(migrationStarted).Seconds(), "existing_fixture": present, "schema": schema})
	// Pin every fixture statement to the one session holding the extended
	// statement deadline; pooled 15s interactive conns must never serve them.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := seedConn.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// A schema may hold a partially seeded fixture (interrupted run); every
	// seeding statement below is idempotent so the same schema simply resumes.
	// The stored anchor keeps resumed batches in the original time distribution.
	anchor := time.Now().UTC().Truncate(24 * time.Hour).Unix()
	var existingAnchor *int64
	if err := db.Pool.QueryRow(ctx, "SELECT anchor FROM performance_slo_fixture LIMIT 1").Scan(&existingAnchor); err == nil && existingAnchor != nil {
		anchor = *existingAnchor
	}
	var fixtureReady bool
	_ = db.Pool.QueryRow(ctx, "SELECT ready FROM performance_slo_fixture LIMIT 1").Scan(&fixtureReady)
	if !fixtureReady {
		start := time.Now()
		exec("CREATE TABLE IF NOT EXISTS performance_slo_fixture(rows bigint NOT NULL,anchor bigint NOT NULL,ready boolean NOT NULL DEFAULT false)")
		exec("INSERT INTO performance_slo_fixture(rows,anchor) SELECT $1,$2 WHERE NOT EXISTS(SELECT 1 FROM performance_slo_fixture)", rows, anchor)
		exec("INSERT INTO groups(id,name) SELECT 'g_'||i,'Synthetic group '||i FROM generate_series(1,10)i ON CONFLICT (id) DO NOTHING")
		exec("INSERT INTO proxies(id,name,type,host,port,enabled) SELECT 'p_'||i,'Synthetic proxy '||i,'http','192.0.2.1',9,false FROM generate_series(1,10)i ON CONFLICT (id) DO NOTHING")
		exec("INSERT INTO accounts(id,display_name,enabled,created_at,updated_at,window_start) SELECT 'slo_a_'||i,'Synthetic account '||i,false,$1::bigint,$1::bigint,$1::bigint-30*86400 FROM generate_series(1,1000)i ON CONFLICT (id) DO NOTHING", anchor)
		encrypted, encryptErr := db.Encrypt([]byte(`{"access_token":"synthetic-unusable"}`))
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		exec("INSERT INTO account_credentials(account_id,credentials_cipher,token_fingerprint,encryption_key_id,updated_at) SELECT id,$1,$2,'slo-synthetic',$3 FROM accounts ON CONFLICT (account_id) DO NOTHING", encrypted, []byte("synthetic"), anchor)
		exec("INSERT INTO account_groups(account_id,group_id) SELECT id,'g_'||((substring(id from 7)::int-1)%10+1) FROM accounts ON CONFLICT (account_id,group_id) DO NOTHING")
		exec("INSERT INTO account_proxies(account_id,proxy_id) SELECT id,'p_'||((substring(id from 7)::int-1)%10+1) FROM accounts ON CONFLICT (account_id,proxy_id) DO NOTHING")
		// Statement triggers classify every inserted row through three retained
		// rollup families and re-upsert the same hot minute rows per batch, and
		// the secondary indexes (two GINs evaluate per-row jsonb projections)
		// pay per-row maintenance. At eight-digit row counts both dominate wall
		// time. Insert with triggers disabled and non-constraint indexes
		// dropped, then rebuild every family and index once from the committed
		// details — identical end state, one write per summary and index entry.
		indexes, indexErr := seedConn.Query(ctx, `SELECT i.indexname,i.indexdef FROM pg_indexes i
		 JOIN pg_class ci ON ci.relname=i.indexname JOIN pg_namespace n ON n.oid=ci.relnamespace AND n.nspname=i.schemaname
		 WHERE i.schemaname=current_schema() AND i.tablename='request_logs'
		 AND NOT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.conrelid='request_logs'::regclass AND c.conindid=ci.oid)`)
		if indexErr != nil {
			t.Fatal(indexErr)
		}
		dropped := [][2]string{}
		for indexes.Next() {
			var name, definition string
			if err = indexes.Scan(&name, &definition); err != nil {
				indexes.Close()
				t.Fatal(err)
			}
			dropped = append(dropped, [2]string{name, definition})
		}
		if err = indexes.Err(); err != nil {
			indexes.Close()
			t.Fatal(err)
		}
		indexes.Close()
		exec("SET synchronous_commit=off")
		exec("ALTER TABLE request_logs DISABLE TRIGGER USER")
		for _, index := range dropped {
			exec("DROP INDEX " + index[0])
		}
		// Re-running a partially seeded fixture must neither duplicate rows nor
		// repeat finished batches: usage_events' unique event_id gates the
		// insert, RETURNING only feeds new rows onward, and the loop resumes
		// after the highest synthetic event already committed.
		resume := 0
		if err := seedConn.QueryRow(ctx, "SELECT COALESCE(max(substring(event_id from 7)::bigint),0) FROM usage_events WHERE event_id LIKE 'slo\\_e\\_%'").Scan(&resume); err != nil {
			t.Fatal(err)
		}
		for first := resume + 1; first <= rows; first += 500000 {
			last := min(first+499999, rows)
			exec(`WITH events AS (INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
			 SELECT 'slo_e_'||i,to_timestamp($3::bigint-((i/1000)%30)*86400-(i%60)),
			 'slo_a_'||(i%1000+1),'Synthetic account '||(i%1000+1),'slo_key',
			 CASE WHEN i%10=0 THEN 'error' ELSE 'ok' END,1000+i%400,100+i%200,800,50+i%1000,true
			 FROM generate_series($1::bigint,$2::bigint)i ON CONFLICT (event_id) DO NOTHING RETURNING *)
			 INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
			 SELECT event_id,account_ref,account_ref,account_name,created_at,status,'synthetic-model','Synthetic key',
			 jsonb_build_object('event_id',event_id,'account_id',account_ref,'account',account_name,'time',extract(epoch FROM created_at)::bigint,
			 'status',status,'model','synthetic-model','key_id',key_id,'key_name','Synthetic key','input_tokens',input_tokens,
			 'output_tokens',output_tokens,'cached_tokens',cached_tokens,'latency_ms',latency_ms,'usage_known',true,
			 'http_code',CASE WHEN status='ok' THEN 200 ELSE 429 END,'error',CASE WHEN status='ok' THEN '' ELSE 'Synthetic rate limit' END)
			 FROM events`, first, last, anchor)
			performanceSLOEmit(t, "SEED", map[string]any{"rows": last, "elapsed_s": time.Since(start).Seconds(), "schema": schema})
		}
		exec("SET synchronous_commit=on")
		for _, index := range dropped {
			exec(index[1])
		}
		performanceSLOEmit(t, "SEED_INDEXES", map[string]any{"recreated": len(dropped), "elapsed_s": time.Since(start).Seconds(), "schema": schema})
		exec("ALTER TABLE request_logs ENABLE TRIGGER USER")
		metrics, metricErr := NewPGMetrics(db)
		if metricErr != nil {
			t.Fatal(metricErr)
		}
		if err = metrics.RebuildRetainedRollups(ctx); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "RETAINED_REBUILD", map[string]any{"elapsed_s": time.Since(start).Seconds(), "schema": schema})
		if err = metrics.RebuildAggregates(ctx); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record)
		 SELECT a.id,a.display_name,l.created_at,l.id,l.record FROM accounts a CROSS JOIN LATERAL
		 (SELECT created_at,id,record FROM request_logs WHERE account_ref=a.id ORDER BY created_at DESC,id DESC LIMIT 1)l
		 ON CONFLICT (account_ref) DO UPDATE SET account_name=EXCLUDED.account_name,last_at=EXCLUDED.last_at,last_log_id=EXCLUDED.last_log_id,record=EXCLUDED.record`)
		exec("ANALYZE accounts,account_credentials,account_groups,account_proxies,account_request_state,groups,proxies,request_logs,usage_events,usage_minute,usage_daily,usage_totals,retained_log_minute,retained_usage_minute,retained_usage_daily,retained_usage_model_minute,retained_usage_model_daily,retained_operations")
		exec("UPDATE performance_slo_fixture SET ready=true")
		performanceSLOEmit(t, "SEED_COMPLETE", map[string]any{"rows": rows, "elapsed_s": time.Since(start).Seconds(), "schema": schema})
	}
	seedConn.Release()
	var fixtureRows int64
	var ready bool
	if err = db.Pool.QueryRow(ctx, "SELECT rows,anchor,ready FROM performance_slo_fixture").Scan(&fixtureRows, &anchor, &ready); err != nil || !ready || fixtureRows != int64(rows) {
		t.Fatalf("fixture is not ready or row count differs: %v", err)
	}
	counts := map[string]int64{}
	for _, table := range []string{"accounts", "request_logs", "usage_events", "usage_minute", "usage_daily"} {
		var n int64
		if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	if counts["request_logs"] != int64(rows) || counts["usage_events"] != int64(rows) {
		t.Fatal("fixture row count mismatch")
	}
	performanceSLOEmit(t, "FIXTURE", map[string]any{"schema": schema, "rows": counts, "anchor": anchor, "distribution": "deterministic bursts across 30 days and 1000 accounts; 10% errors; no upstream calls"})
	relations, relationErr := db.Pool.Query(ctx, `SELECT c.relname,pg_total_relation_size(c.oid),pg_relation_size(c.oid),c.reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind IN ('r','m') ORDER BY c.relname`)
	if relationErr != nil {
		t.Fatal(relationErr)
	}
	storage := []map[string]any{}
	for relations.Next() {
		var name string
		var total, heap, estimated int64
		if err = relations.Scan(&name, &total, &heap, &estimated); err != nil {
			relations.Close()
			t.Fatal(err)
		}
		storage = append(storage, map[string]any{"table": name, "total_bytes": total, "heap_bytes": heap, "estimated_rows": estimated})
	}
	if err = relations.Err(); err != nil {
		relations.Close()
		t.Fatal(err)
	}
	relations.Close()
	performanceSLOEmit(t, "STORAGE", map[string]any{"relations": storage})
	if phase == "seed" {
		return
	}
	// Discard every maintenance connection; new sessions inherit the real
	// application's 15s database deadline, never the fixture's longer deadline.
	db.Pool.Reset()
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.initializeAdminKey(ctx, webTestKey); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Proxies: proxies, Keys: keys, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey}
	p.startedAt = time.Now().Add(-time.Minute)
	for range 10 {
		metrics.realtime.add(time.Now().Unix(), true, nil)
	}
	// Small management fixtures are separate from the immutable 10M log/event
	// population. They make auxiliary endpoints exercise populated state.
	if err = keys.store.Modify(func(state *keyState) error {
		if len(state.Keys) > 0 {
			return nil
		}
		for i := 0; i < 100; i++ {
			id := fmt.Sprintf("slo_key_%d", i)
			if i == 0 {
				id = "slo_key"
			}
			state.Keys = append(state.Keys, APIKey{ID: id, Name: fmt.Sprintf("Synthetic key %d", i), Key: fmt.Sprintf("synthetic-never-use-%d", i), Version: 1, Enabled: false, Groups: []string{"g_1"}})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := p.managementState()
	if err = state.Update(ctx, "ingress-events", "recent", func(entry *managementStateEntry) error {
		entry.Payload, _ = json.Marshal(gatewayIngressDocument{Epoch: "synthetic-slo"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sink := &gatewayIngressEventSink{store: state}
	events := make([]gatewayIngressEvent, 100)
	for i := range events {
		events[i] = gatewayIngressEvent{ID: fmt.Sprintf("slo_ingress_%d", i), TimeMs: time.Now().UnixMilli(), HTTPCode: 401, Reason: "invalid_api_key", Endpoint: "/v1/responses", ClientRequestID: "slo-synthetic"}
	}
	if err = sink.appendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("slo_cleanup_%d", i)
		job := managedCleanupJob{ID: id, Status: "completed", CreatedAt: anchor + int64(i), FinishedAt: anchor + int64(i), Scope: managedCleanupScope}
		if err = state.Update(ctx, "cleanup-jobs", id, func(entry *managementStateEntry) error { entry.Payload, _ = json.Marshal(job); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	keyRows, err := keys.List()
	if err != nil {
		t.Fatal(err)
	}
	performanceSLOEmit(t, "AUXILIARY_FIXTURE", map[string]any{"keys": len(keyRows), "groups": 10, "proxies": 10, "ingress_events": 100, "completed_cleanup_jobs": 10, "realtime_scope": "process-local ring, not historical event table"})
	server := httptest.NewServer(p.adminRoutes())
	defer server.Close()
	client := &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	defer client.CloseIdleConnections()
	rangeQuery := fmt.Sprintf("from=%d&to=%d&step=86400&timezone=UTC", anchor-30*86400, anchor+60)
	type endpoint struct {
		name, path, kind string
		size             int
	}
	cases := []endpoint{
		{"accounts_50", "/api/accounts/manage?page=1&page_size=50", "accounts", 50},
		{"accounts_500", "/api/accounts/manage?page=1&page_size=500", "accounts", 500},
		{"accounts_last_used", "/api/accounts/manage?page=1&page_size=50&sort_by=last_used_at&sort_order=desc", "accounts", 50},
		{"groups", "/api/groups", "groups", 10}, {"proxies", "/api/proxies", "proxies", 10}, {"keys", "/api/keys", "keys", len(keyRows)},
		{"settings", "/api/settings", "json", 0}, {"audit_logs", "/api/audit-logs", "json", 0},
		{"session", "/api/session", "session", 0},
		{"diagnostics", "/api/diagnostics", "diagnostics", 0},
		{"realtime", "/api/overview/realtime", "realtime", 0},
		{"ingress_events", "/api/ingress-events?limit=50", "items", 50},
		{"account_runtime", "/api/accounts/slo_a_1/runtime", "account", 0},
		{"account_metadata", "/api/accounts/slo_a_1/metadata", "account", 0},
		{"account_requests", "/api/accounts/slo_a_1/requests?limit=50", "items", min(50, rows/1000)},
		{"logs_coverage", "/api/logs/coverage", "coverage", rows},
		{"logs_options_model", "/api/logs/options?field=model", "items", 1},
		{"logs_options_account", "/api/logs/options?field=account", "items", 50},
		{"log_review", "/api/logs/slo_e_500/review", "review", 0},
		{"cleanup_jobs", "/api/logs/cleanup/jobs", "items", 10},
		{"logs_50", "/api/logs?size=50", "items", 50}, {"logs_500", "/api/logs?size=500", "items", 500},
		{"logs_page100", "/api/logs?size=50&page=100", "items", 50},
		{"logs_errors", "/api/logs?size=50&status=error", "items", 50},
		{"logs_account", "/api/logs?size=50&account_id=slo_a_1", "items", min(50, rows/1000)},
		{"logs_request_id", "/api/logs?size=50&request_id=slo_e_500", "items", 1},
		{"logs_request_missing", "/api/logs?size=50&request_id=missing-slo-request", "items", 0},
		{"logs_client_request_missing", "/api/logs?size=50&client_request_id=missing-slo-client", "items", 0},
		{"logs_tokens", "/api/logs?size=50&sort_by=tokens", "items", 50},
		{"logs_latency", "/api/logs?size=50&sort_by=latency", "items", 50},
		{"logs_exact_total", "/api/logs?size=50&exact_total=true", "total", rows},
		{"log_detail", "/api/logs/slo_e_500", "json", 0},
		{"log_stats_30d", "/api/logs/stats?" + rangeQuery, "stats", rows},
		{"overview_30d", "/api/overview?" + rangeQuery, "overview", rows},
		{"operations_24h", "/api/operations?range=24h", "json", 0},
	}
	threshold := 1000.0
	if rows >= 10000000 {
		threshold = 3000
	}
	selected := os.Getenv("PERFORMANCE_SLO_CASES")
	if selected != "" {
		known := map[string]bool{}
		for _, c := range cases {
			known[c.name] = true
		}
		for _, name := range strings.Split(selected, ",") {
			if !known[name] {
				t.Fatalf("unknown SLO case %q", name)
			}
		}
	}
	type sample struct {
		MS        float64 `json:"ms"`
		Status    int     `json:"status"`
		Bytes     int     `json:"bytes"`
		Error     string  `json:"error,omitempty"`
		ErrorCode string  `json:"error_code,omitempty"`
	}
	measure := func(c endpoint) sample {
		start := time.Now()
		request, _ := http.NewRequest(http.MethodGet, server.URL+c.path, nil)
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		response, err := client.Do(request)
		if err != nil {
			return sample{MS: float64(time.Since(start).Microseconds()) / 1000, Error: err.Error()}
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		result := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode, Bytes: len(body)}
		if readErr != nil {
			result.Error = readErr.Error()
			return result
		}
		if response.StatusCode != 200 {
			result.Error = "unexpected_http_status"
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
				Code string `json:"code"`
			}
			if json.Unmarshal(body, &failure) == nil {
				result.ErrorCode = failure.Error.Code
				if result.ErrorCode == "" {
					result.ErrorCode = failure.Code
				}
			}
			return result
		}
		var value any
		if json.Unmarshal(body, &value) != nil {
			result.Error = "invalid_json"
			return result
		}
		object, _ := value.(map[string]any)
		switch c.kind {
		case "items", "total", "accounts", "keys", "groups", "proxies":
			field := "items"
			if stringIn(c.kind, "accounts", "keys", "groups", "proxies") {
				field = c.kind
			}
			items, _ := object[field].([]any)
			want := c.size
			if c.kind == "total" {
				want = 50
				if object["total"] != float64(c.size) {
					result.Error = "incorrect_exact_total"
				}
			}
			if len(items) != want {
				result.Error = fmt.Sprintf("items=%d want=%d", len(items), want)
			}
		case "overview":
			cards, _ := object["cards"].(map[string]any)
			if cards["requests"] != float64(c.size) {
				result.Error = "incorrect_overview_total"
			}
			if cards["errors"] != float64(c.size/10) || cards["accounts"] != float64(1000) {
				result.Error = "incorrect_overview_fixture_counts"
			}
		case "stats":
			if object["total_requests"] != float64(c.size) {
				result.Error = "incorrect_stats_total"
			}
			if object["errors"] != float64(c.size/10) || object["success"] != float64(c.size-c.size/10) {
				result.Error = "incorrect_stats_status_counts"
			}
		case "account":
			if object["id"] != "slo_a_1" {
				result.Error = "incorrect_account_identity"
			}
		case "coverage":
			if object["detail_count"] != float64(c.size) || object["storage"] != "postgres" {
				result.Error = "incorrect_retained_coverage"
			}
		case "session":
			if object["authenticated"] != true {
				result.Error = "unauthenticated_session"
			}
		case "diagnostics":
			if object["storage_mode"] != "postgres" {
				result.Error = "incorrect_diagnostics_storage"
			}
		case "realtime":
			if object["scope"] != "current_process" || object["window_seconds"] != float64(60) {
				result.Error = "incorrect_realtime_scope"
			}
		case "review":
			if object["scope"] != "management_review" || object["record"] == nil {
				result.Error = "incorrect_review_record"
			}
		}
		if c.name == "logs_request_id" {
			items, _ := object["items"].([]any)
			if len(items) == 1 {
				row, _ := items[0].(map[string]any)
				if row["event_id"] != "slo_e_500" {
					result.Error = "incorrect_request_id_match"
				}
			}
		}
		if c.name == "logs_account" || c.name == "logs_errors" {
			items, _ := object["items"].([]any)
			for _, item := range items {
				row, _ := item.(map[string]any)
				if c.name == "logs_account" && row["account_id"] != "slo_a_1" || c.name == "logs_errors" && row["status"] != "error" {
					result.Error = "incorrect_filter_match"
				}
			}
		}
		return result
	}
	for _, c := range cases {
		if selected != "" && !strings.Contains(","+selected+",", ","+c.name+",") {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			cold := measure(c)
			performanceSLOEmit(t, "FIRST", map[string]any{"name": c.name, "path": c.path, "phase": "first request in fresh test process; database/OS caches uncontrolled", "sample": cold, "threshold_ms": threshold, "slo_pass": cold.Error == "" && cold.MS < threshold})
			if cold.Error != "" {
				t.Errorf("first request failed: %s status=%d latency=%.2fms", cold.Error, cold.Status, cold.MS)
				return
			}
			if cold.MS >= threshold {
				t.Errorf("first request %.2fms >= %.0fms SLO", cold.MS, threshold)
				return
			}
			for _, concurrency := range []int{1, 4} {
				measured := make([]sample, samples)
				jobs := make(chan int)
				var wg sync.WaitGroup
				before := db.Pool.Stat().AcquireCount()
				started := time.Now()
				for range concurrency {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := range jobs {
							measured[i] = measure(c)
						}
					}()
				}
				for i := range samples {
					jobs <- i
				}
				close(jobs)
				wg.Wait()
				ordered := make([]float64, samples)
				failed, over := 0, 0
				statuses := map[int]int{}
				for i, s := range measured {
					ordered[i] = s.MS
					statuses[s.Status]++
					if s.Error != "" {
						failed++
					}
					if s.MS >= threshold {
						over++
					}
				}
				sort.Float64s(ordered)
				percentile := func(p int) float64 { return ordered[(len(ordered)*p+99)/100-1] }
				performanceSLOEmit(t, "RESULT", map[string]any{"name": c.name, "path": c.path, "concurrency": concurrency, "samples": measured, "failed": failed, "over_slo": over, "statuses": statuses, "threshold_ms": threshold, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": ordered[len(ordered)-1], "elapsed_s": time.Since(started).Seconds(), "db_acquires_per_request": float64(db.Pool.Stat().AcquireCount()-before) / float64(samples)})
				if failed > 0 || over > 0 {
					t.Errorf("concurrency=%d failed=%d over_slo=%d/%d p99=%.2fms", concurrency, failed, over, samples, percentile(99))
					return
				}
			}
		})
	}
}
