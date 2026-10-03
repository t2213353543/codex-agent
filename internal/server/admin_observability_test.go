package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdminObservabilityAuthorizationAndReadOnly(t *testing.T) {
	_, handler := webTestSetup(t)
	for _, path := range []string{"/api/diagnostics", "/api/audit-logs"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			response := webTestRequest(handler, method, path, "", "")
			if response.Code != http.StatusUnauthorized || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s auth=%d cache=%q", method, path, response.Code, response.Header().Get("Cache-Control"))
			}
			response = webTestRequest(handler, method, path, "", "Bearer "+webTestKey)
			wantStatus := http.StatusMethodNotAllowed
			if method == http.MethodGet {
				wantStatus = http.StatusOK
			}
			if response.Code != wantStatus || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s status=%d want=%d", method, path, response.Code, wantStatus)
			}
		}
	}
}

func TestAdminDiagnosticsFileModeAndUptime(t *testing.T) {
	p, handler := webTestSetup(t)
	p.startedAt = time.Now().Add(-75 * time.Second)
	response := webTestRequest(handler, http.MethodGet, "/api/diagnostics", "", "Bearer "+webTestKey)
	var data adminDiagnostics
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil {
		t.Fatalf("diagnostics: %d %s", response.Code, response.Body.String())
	}
	if data.Version != version || data.StorageMode != "file" || data.Status != "ok" || data.UptimeSeconds < 75 || data.UptimeSeconds > 78 || data.CheckedAt < time.Now().Unix()-2 || data.Capabilities["audit_logs"] {
		t.Fatalf("incorrect runtime state: %+v", data)
	}
	for _, name := range []string{"postgres", "redis"} {
		dependency := data.Dependencies[name]
		if dependency.Configured || dependency.Status != "not_configured" || dependency.ErrorCode != "" {
			t.Fatalf("unconfigured %s: %+v", name, dependency)
		}
	}
	p.startedAt = time.Now().Add(time.Hour)
	response = webTestRequest(handler, http.MethodGet, "/api/diagnostics", "", "Bearer "+webTestKey)
	if json.Unmarshal(response.Body.Bytes(), &data) != nil || data.UptimeSeconds != 0 {
		t.Fatal("future start produced negative uptime")
	}
}

func TestAdminDiagnosticsDependencyOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, status, code string
		configured         bool
		ping               func(context.Context) error
	}{
		{name: "not configured", status: "not_configured"},
		{name: "success", status: "ok", configured: true, ping: func(context.Context) error { return nil }},
		{name: "missing pool", status: "unavailable", code: "dependency_unavailable", configured: true},
		{name: "private error", status: "unavailable", code: "dependency_unavailable", configured: true, ping: func(context.Context) error {
			return errors.New("postgres://private-user:private-password@private-host:5432/private-db")
		}},
		{name: "timeout", status: "timeout", code: "dependency_timeout", configured: true, ping: func(context.Context) error { return context.DeadlineExceeded }},
		{name: "cancel", status: "unavailable", code: "check_canceled", configured: true, ping: func(context.Context) error { return context.Canceled }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := adminCheckDependency(context.Background(), test.configured, test.ping)
			if result.Configured != test.configured || result.Status != test.status || result.ErrorCode != test.code || result.LatencyMS < 0 {
				t.Fatalf("outcome: %+v", result)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "private-") {
				t.Fatal("driver details leaked")
			}
		})
	}
	// A driver's disabled context socket timeout must not extend the HTTP budget.
	finish := make(chan struct{})
	defer close(finish)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := adminCheckDependency(ctx, true, func(context.Context) error { <-finish; return nil })
	if result.Status != "timeout" || time.Since(start) > time.Second {
		t.Fatalf("unbounded dependency check: %+v", result)
	}
}

func TestAdminDiagnosticsConfiguredMissingStorage(t *testing.T) {
	p := &Proxy{Store: Store{DB: &Database{}}}
	response := httptest.NewRecorder()
	p.handleDiagnostics(response, httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil))
	var data adminDiagnostics
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Status != "degraded" || data.StorageMode != "postgres" || !data.Capabilities["audit_logs"] {
		t.Fatalf("missing storage state: %d %s", response.Code, response.Body.String())
	}
	if data.Dependencies["postgres"].Status != "unavailable" || data.Dependencies["redis"].Status != "not_configured" {
		t.Fatalf("missing dependencies: %+v", data.Dependencies)
	}
}

func TestAdminAuditFileModeAndValidation(t *testing.T) {
	_, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodGet, "/api/audit-logs?limit=25", "", "Bearer "+webTestKey)
	var page adminAuditPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Supported || page.StorageMode != "file" || page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" || page.HasMore || page.Limit != 25 {
		t.Fatalf("file mode implied durable auditing: %d %s", response.Code, response.Body.String())
	}
	for _, query := range []string{
		"limit=0", "limit=-1", "limit=201", "limit=1.5", "limit=abc", "limit=50&limit=60", "unknown=1",
		"from=bad", "from=0", "from=-1", "to=253402300800", "from=2&to=2", "from=3&to=2",
		"action=" + strings.Repeat("a", 101), "entity_id=" + strings.Repeat("x", 257), "action=line%0Abreak", "entity_id=bad%00id",
		"cursor=bad", "cursor=" + strings.Repeat("a", 769),
	} {
		response = webTestRequest(handler, http.MethodGet, "/api/audit-logs?"+query, "", "Bearer "+webTestKey)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("invalid query accepted %q: %d %s", query, response.Code, response.Body.String())
		}
	}
}

func TestAdminAuditCursorValidation(t *testing.T) {
	o := adminAuditOptions{Limit: 50, Action: "account.update", EntityID: "a_123", From: 1700000000, To: 1900000000}
	valid := adminAuditCursor{Version: 1, Time: "2026-09-17T12:00:00.000123Z", ID: "9007199254740993", Filter: o.filterHash()}
	encode := func(cursor adminAuditCursor) string {
		raw, _ := json.Marshal(cursor)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	query := url.Values{"action": {o.Action}, "entity_id": {o.EntityID}, "from": {strconv.FormatInt(o.From, 10)}, "to": {strconv.FormatInt(o.To, 10)}, "cursor": {encode(valid)}}
	parsed, err := parseAdminAuditOptions(query)
	if err != nil || parsed.Cursor == nil || parsed.Cursor.ID != valid.ID || parsed.Cursor.Time != valid.Time {
		t.Fatalf("valid precise cursor rejected: %+v %v", parsed, err)
	}
	for _, mutate := range []func(*adminAuditCursor){
		func(c *adminAuditCursor) { c.Version = 2 }, func(c *adminAuditCursor) { c.ID = "0" }, func(c *adminAuditCursor) { c.ID = "9223372036854775808" },
		func(c *adminAuditCursor) { c.Time = "2026-09-17" }, func(c *adminAuditCursor) { c.Time = "2026-09-17T12:00:00.000000001Z" },
		func(c *adminAuditCursor) { c.Filter = "changed" },
	} {
		invalid := valid
		mutate(&invalid)
		query.Set("cursor", encode(invalid))
		if _, err := parseAdminAuditOptions(query); err == nil {
			t.Fatalf("invalid cursor accepted: %+v", invalid)
		}
	}
	query.Set("cursor", encode(valid))
	query.Set("action", "account.delete")
	if _, err := parseAdminAuditOptions(query); err == nil {
		t.Fatal("cursor accepted after filter change")
	}
}

func TestPGAdminObservabilityAuditPagination(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	var ids []string
	for _, nanos := range []int{1000, 1000, 2000, 3000, 3000} {
		var id int64
		err := p.Store.DB.Pool.QueryRow(ctx, `INSERT INTO audit_logs(created_at,action,entity_id) VALUES($1,'account.update','audit-fixture') RETURNING id`, base.Add(time.Duration(nanos))).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	for _, row := range []struct {
		action, entity string
		stamp          time.Time
	}{
		{"account.delete", "audit-fixture", base}, {"account.update", "other", base},
		{"account.update", "audit-fixture", base.Add(-time.Second)}, {"account.update", "audit-fixture", base.Add(time.Second)},
	} {
		if _, err := p.Store.DB.Pool.Exec(ctx, `INSERT INTO audit_logs(created_at,action,entity_id) VALUES($1,$2,$3)`, row.stamp, row.action, row.entity); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/api/audit-logs?limit=2&action=account.update&entity_id=audit-fixture&from=%d&to=%d", base.Unix(), base.Unix()+1)
	var gotIDs []string
	cursor := ""
	for pageIndex := range 3 {
		response := webTestRequest(handler, http.MethodGet, path+"&cursor="+url.QueryEscape(cursor), "", "Bearer "+webTestKey)
		var page adminAuditPage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || !page.Supported || page.StorageMode != "postgres" {
			t.Fatalf("audit page %d: %d %s", pageIndex, response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			if item.CreatedAt != base.Unix() || item.Actor != "admin" || item.Action != "account.update" || item.EntityID != "audit-fixture" || item.Result != "ok" {
				t.Fatalf("filter mismatch: %+v", item)
			}
			gotIDs = append(gotIDs, item.ID)
		}
		if page.HasMore != (pageIndex < 2) || (page.NextCursor != "") != page.HasMore {
			t.Fatalf("incorrect continuation: %+v", page)
		}
		cursor = page.NextCursor
		if pageIndex == 0 {
			// An insert newer than the current cursor must not shift later pages.
			if _, err := p.Store.DB.Pool.Exec(ctx, `INSERT INTO audit_logs(created_at,action,entity_id) VALUES($1,'account.update','audit-fixture')`, base.Add(4000*time.Nanosecond)); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantIDs := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("cursor dropped or repeated records: got=%v want=%v", gotIDs, wantIDs)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/audit-logs?entity_id="+url.QueryEscape("audit-fixture' OR true --"), "", "Bearer "+webTestKey)
	var page adminAuditPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
		t.Fatal("filter was not parameterized")
	}
	var before int64
	if err := p.Store.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_ = webTestRequest(handler, http.MethodGet, "/api/audit-logs", "", "Bearer "+webTestKey)
	var after int64
	if err := p.Store.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&after); err != nil || before != after {
		t.Fatal("read endpoint mutated audit storage")
	}
}

func TestPGAdminObservabilityDiagnosticsAndStorageFailure(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	// The shared admin fixture may configure Redis for admission tests. This
	// first branch explicitly exercises PostgreSQL without a Redis dependency.
	configuredRedis := p.Store.DB.Redis
	p.Store.DB.Redis = nil
	defer func() { p.Store.DB.Redis = configuredRedis }()
	response := webTestRequest(handler, http.MethodGet, "/api/diagnostics", "", "Bearer "+webTestKey)
	var data adminDiagnostics
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Dependencies["postgres"].Status != "ok" || data.Status != "degraded" || data.Dependencies["redis"].Configured || data.Dependencies["redis"].Status != "not_configured" {
		t.Fatalf("PG diagnostic: %d %s", response.Code, response.Body.String())
	}
	p.Store.DB.Redis = configuredRedis
	if raw := os.Getenv("TEST_REDIS_URL"); raw != "" {
		if configuredRedis == nil {
			t.Fatal("admin fixture did not configure the isolated Redis dependency")
		}
		response = webTestRequest(handler, http.MethodGet, "/api/diagnostics", "", "Bearer "+webTestKey)
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil || data.Status != "ok" || data.Dependencies["redis"].Status != "ok" {
			t.Fatalf("Redis diagnostic: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), raw) {
			t.Fatal("diagnostic exposed connection configuration")
		}
	}
	// The isolated schema can lose its audit table without leaking SQL or DSNs.
	if _, err := p.Store.DB.Pool.Exec(context.Background(), `DROP TABLE audit_logs`); err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodGet, "/api/audit-logs", "", "Bearer "+webTestKey)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"audit_unavailable"`) || strings.Contains(response.Body.String(), "SQLSTATE") || strings.Contains(response.Body.String(), "does not exist") {
		t.Fatalf("unsafe audit failure: %d %s", response.Code, response.Body.String())
	}
}
