package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// These previously unmeasured routes use actual loopback TLS/HTTP, the admin
// router, synthetic PostgreSQL and isolated Redis. Setup and validation queries
// are excluded from endpoint timing. No upstream or background worker runs.
func TestPerformanceRemainingLocalWrites(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	if p.Store.DB.Redis == nil {
		t.Fatal("session timing requires isolated Redis")
	}
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client := server.Client()
	client.Timeout = 30 * time.Second
	defer client.CloseIdleConnections()
	client.Jar, _ = cookiejar.New(nil)
	request := func(name, method, path string, value any, bearer bool, expected int, emit bool) map[string]any {
		t.Helper()
		var raw []byte
		if value != nil {
			raw, _ = json.Marshal(value)
		}
		r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", server.URL)
		r.Header.Set("Content-Type", "application/json")
		if bearer {
			r.Header.Set("Authorization", "Bearer "+webTestKey)
		}
		acquires := p.Store.DB.Pool.Stat().AcquireCount()
		start := time.Now()
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		ms := float64(time.Since(start).Microseconds()) / 1000
		acquires = p.Store.DB.Pool.Stat().AcquireCount() - acquires
		result := map[string]any{}
		if readErr != nil || response.StatusCode != expected || json.Unmarshal(body, &result) != nil {
			t.Fatalf("%s HTTP%d expected%d read error%v", name, response.StatusCode, expected, readErr)
		}
		if emit {
			performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{
				"name": name, "method": method, "path": path,
				"sample":       map[string]any{"ms": ms, "status": response.StatusCode, "error": "", "db_acquires": acquires},
				"threshold_ms": 1000, "slo_pass": ms < 1000,
				"scope": "synthetic real loopback TLS HTTP plus actual PG/Redis; setup/validation outside timing; no provider",
			})
			if ms >= 1000 {
				t.Errorf("%s %.3fms exceeds1s", name, ms)
			}
		}
		return result
	}
	for sample := range envInt("PERFORMANCE_SLO_SAMPLES", 3, 1, 20) {
		login := request("session_login", http.MethodPost, "/api/session", map[string]string{"admin_key": webTestKey}, false, 200, true)
		if login["authenticated"] != true {
			t.Fatal("login did not authenticate")
		}
		if request("session_cookie_check", http.MethodGet, "/api/session", nil, false, 200, false)["authenticated"] != true {
			t.Fatal("secure session cookie not usable")
		}
		logout := request("session_logout", http.MethodDelete, "/api/session", nil, false, 200, true)
		if logout["authenticated"] != false || logout["ok"] != true {
			t.Fatal("logout contract changed")
		}
		request("session_revoked_check", http.MethodGet, "/api/session", nil, false, 401, false)
		var keys []string
		var cursor uint64
		for {
			found, next, err := p.Store.DB.Redis.Scan(t.Context(), cursor, p.Store.DB.Namespace+"*", 256).Result()
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, found...)
			cursor = next
			if cursor == 0 {
				break
			}
		}
		for _, key := range keys {
			if strings.Contains(key, ":session:") {
				t.Fatalf("sample%d retained a revoked session", sample)
			}
		}
	}

	account := managedTestAccount(t, p, "http-delete-10k-history", nil)
	const historyRows = 10000
	// Same deterministic high-entropy TOAST fixture as the existing deletion
	// regression, at10k rows. Bulk construction skips user maintenance triggers
	// inside one isolated transaction; all triggers are restored before HTTP.
	var metadata strings.Builder
	for i := range 256 {
		digest := sha256.Sum256([]byte(fmt.Sprintf("delete-wide-history-%d", i)))
		metadata.WriteString(base64.RawStdEncoding.EncodeToString(digest[:]))
	}
	seed, err := p.Store.DB.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(context.Background())
	if _, err := seed.Exec(t.Context(), "SET LOCAL statement_timeout='5min'; ALTER TABLE request_logs DISABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < historyRows; start += 1000 {
		_, err := seed.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,account_ref,account_name,status)
 SELECT 'http-delete-wide-'||g,to_timestamp(1800000000),$1,$2,'ok'
 FROM generate_series($4::integer,$4::integer+999)g
 RETURNING event_id,created_at,account_ref,account_name,status
) INSERT INTO request_logs(event_id,account_id,account_ref,account_name,created_at,status,model,key_name,record)
 SELECT event_id,$1,account_ref,account_name,created_at,status,'fixture','',
 jsonb_build_object('event_id',event_id,'time',1800000000,'account_id',$1::text,'account',$2::text,'model','fixture','status','ok','user_agent',$3::text)
 FROM events`, account.ID, account.Name, metadata.String(), start)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := seed.Exec(t.Context(), "ALTER TABLE request_logs ENABLE TRIGGER USER; ANALYZE request_logs(account_id)"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var relationBytes int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT pg_total_relation_size('request_logs')").Scan(&relationBytes); err != nil {
		t.Fatal(err)
	}
	deleted := request("account_delete_10k_wide_history", http.MethodDelete, fmt.Sprintf("/api/accounts/%s?version=%d", account.ID, account.Version), nil, true, 200, true)
	if deleted["ok"] != true {
		t.Fatal("native delete failed")
	}
	var rows, historical, durable, live, credentials, audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM request_logs),
 (SELECT count(*) FROM request_logs WHERE account_id=$1 AND account_ref=$1),
 (SELECT count(*) FROM usage_events WHERE account_ref=$1),
 (SELECT count(*) FROM accounts WHERE id=$1),
 (SELECT count(*) FROM account_credentials WHERE account_id=$1),
 (SELECT count(*) FROM audit_logs WHERE entity_id=$1 AND action='account.delete')`, account.ID).Scan(&rows, &historical, &durable, &live, &credentials, &audits); err != nil || rows != historyRows || historical != historyRows || durable != historyRows || live != 0 || credentials != 0 || audits != 1 {
		t.Fatalf("history/audit/cascade mismatch %d/%d/%d/%d/%d/%d %v", rows, historical, durable, live, credentials, audits, err)
	}
	performanceSLOEmit(t, "WRITE_FIXTURE", map[string]any{"name": "account_delete_10k_wide_history", "retained_history_rows": rows, "metadata_bytes_per_row": metadata.Len(), "history_relation_bytes": relationBytes, "scope": "fixture size and post-HTTP durability validation; not timed HTTP"})
	if deny.calls.Load() != 0 {
		t.Fatal("local write attempted upstream request")
	}
}
