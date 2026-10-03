package server

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestManagedUsageClassRollupEligibility(t *testing.T) {
	for _, filter := range []string{"error_category=rate_limit", "error_severity=P1&status=failed", "error_category=timeout&from=1700000001&to=1700000100"} {
		q := managedQuery(t, filter)
		if _, _, eligible := managedUsageClassRollup(q); !eligible {
			t.Fatalf("single class rejected: %s", filter)
		}
		q.AsOf, q.SnapshotID = "usage_test", "usage_test"
		if _, _, eligible := managedUsageClassRollup(q); !eligible {
			t.Fatalf("resolved snapshot rejected: %s", filter)
		}
	}
	for _, filter := range []string{"error_category=rate_limit&error_severity=P1", "error_category=rate_limit&key_id=k", "error_category=rate_limit&model=m", "error_severity=P1&group_id=g", "error_category=rate_limit&as_of=usage_test", ""} {
		if _, _, eligible := managedUsageClassRollup(managedQuery(t, filter)); eligible {
			t.Fatalf("unproven intersection accepted: %s", filter)
		}
	}
}

func TestPGManagedUsageRawProjectionPreservesLegacyShapes(t *testing.T) {
	db := newMetricsTestDB(t)
	if _, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES('raw-legacy',to_timestamp(1790553600.5),'error') RETURNING event_id)
 INSERT INTO request_logs(id,event_id,created_at,status,model,account_name,key_name,record)
 SELECT 0,event_id,to_timestamp(1790553600.5),'error','model','account','key','{}'::jsonb FROM e`); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `true`, `7`, `"legacy"`, `{}`, `{"http_code":"429","error":"rate limit exceeded","groups":["one","one"],"input_tokens":17}`, `{"usage_known":null,"latency_ms":0,"cached_tokens_known":false,"unrelated_protocol_payload":"remove me"}`} {
		if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=$1::jsonb WHERE id=0`, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(t.Context(), `DELETE FROM request_log_analytics WHERE log_id=0`); err != nil {
			t.Fatal(err)
		}
		statement := `WITH raw_rows AS (SELECT id,created_at,status,model,account_name,key_name FROM request_logs WHERE id=0), ` + managedUsageRawRollupProjectionSQL() + ` compared AS (
 SELECT (SELECT to_jsonb(c) FROM retained_usage_counters(r.record,r.status)c) AS before,
        (SELECT to_jsonb(c) FROM retained_usage_counters(p.record,p.status)c) AS after,
        (SELECT jsonb_agg(to_jsonb(d) ORDER BY d.kind,d.resource_id,d.name) FROM retained_usage_dimensions(r.record,r.status,r.model,r.account_name,r.key_name)d) AS before_dimensions,
        (SELECT jsonb_agg(to_jsonb(d) ORDER BY d.kind,d.resource_id,d.name) FROM retained_usage_dimensions(p.record,p.status,p.model,p.account_name,p.key_name)d) AS after_dimensions
 FROM request_logs r CROSS JOIN raw_projected p WHERE r.id=0)
 SELECT before IS NOT DISTINCT FROM after AND before_dimensions IS NOT DISTINCT FROM after_dimensions FROM compared`
		var equal bool
		if err := db.Pool.QueryRow(t.Context(), statement).Scan(&equal); err != nil || !equal {
			t.Fatalf("compact raw projection changes legacy record %s: %v", raw, err)
		}
	}
}

func TestPGManagedUsageClassOptionsSnapshotParity(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	m, db := p.Metrics, p.Store.DB
	const base int64 = 1790553600
	for i, row := range []RequestRecord{
		{AccountID: "first", Account: "old", KeyID: "key-a", KeyName: "key old", Model: "model-a", Groups: []string{"group-a", "group-a"}, HTTPCode: 429, Error: "rate limit exceeded"},
		{AccountID: "second", Account: "renamed label", KeyID: "key-b", KeyName: "other", Model: "model-b", Groups: []string{"group-b"}, HTTPCode: 403},
		{AccountID: "mixed", Account: "mixed", KeyID: "key-c", KeyName: "mixed", Model: "model-c", Groups: []string{"group-c"}, HTTPCode: 429, Error: "rate limit exceeded"},
	} {
		row.EventID, row.Time, row.Status = fmt.Sprintf("class-options-%d", i), base+int64(i/2)*60+10, "error"
		m.Record(row)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-class-options',id FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	compare := func() {
		t.Helper()
		for _, filter := range []string{"error_category=rate_limit", "error_severity=P1"} {
			for _, snapshot := range []bool{false, true} {
				q := managedQuery(t, filter)
				if snapshot {
					q.AsOf, q.SnapshotID = "usage-class-options", "usage-class-options"
				}
				for _, field := range []string{"account", "group", "key", "model"} {
					for _, search := range []string{"", "mixed", "new"} {
						var got, want []managedUsageOption
						var err error
						raw := q
						raw.statsQueryer = db.Pool // Explicitly disable summaries for oracle.
						if field == "account" || field == "group" {
							got, err = m.managedUsageOptionsPGCurrent(t.Context(), q, field, search, 10)
							if err == nil {
								want, err = m.managedUsageOptionsPGCurrent(t.Context(), raw, field, search, 10)
							}
						} else {
							got, err = m.managedUsageOptions(t.Context(), q, field, search, 10, nil)
							if err == nil {
								want, err = m.managedUsageOptions(t.Context(), raw, field, search, 10, nil)
							}
						}
						if err != nil || !reflect.DeepEqual(got, want) {
							t.Fatalf("class options %s/%s/%s snapshot=%t: got=%+v raw=%+v err=%v", filter, field, search, snapshot, got, want, err)
						}
					}
				}
			}
		}
	}
	compare()
	m.Record(RequestRecord{EventID: "class-options-late", Time: base + 70, Status: "error", HTTPCode: 429, Error: "rate limit exceeded", AccountID: "new", Account: "new", KeyID: "new", KeyName: "new", Model: "new", Groups: []string{"new"}})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	compare()
}

func TestPGManagedUsageAdaptiveClassDenseMinuteUsesRawBranch(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db, m := p.Store.DB, p.Metrics
	if _, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'adaptive-'||i,to_timestamp(1790553610),'error' FROM generate_series(1,800)i RETURNING *
 ) INSERT INTO request_logs(event_id,created_at,status,model,account_name,key_name,record)
 SELECT event_id,created_at,status,'dense','account','key',jsonb_build_object('event_id',event_id,'time',1790553610,'status',status,'model','dense',
 'account_id','account','key_id','key','groups',jsonb_build_array('group'),'http_code',CASE WHEN substring(event_id from 10)::int%2=0 THEN 429 ELSE 403 END,
 'error',CASE WHEN substring(event_id from 10)::int%2=0 THEN 'rate limit exceeded' ELSE 'unauthorized' END) FROM e`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-adaptive',id FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []bool{false, true} {
		q := managedQuery(t, "error_category=rate_limit")
		if snapshot {
			q.AsOf, q.SnapshotID = "usage-adaptive", "usage-adaptive"
		}
		probe, args := managedUsageAdaptiveClassSQL(q, "SELECT 'fast'::text", nil, "SELECT 'raw'::text", nil)
		var branch string
		if err := db.Pool.QueryRow(t.Context(), probe, args...).Scan(&branch); err != nil || branch != "raw" {
			t.Fatalf("dense mixed minute should select one scalar decode branch: %s %v", branch, err)
		}
		got, err := m.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		raw := q
		raw.statsQueryer = db.Pool
		want, err := m.managedLogStats(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		got.CheckedAt, want.CheckedAt = 0, 0
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) || got.Requests != 400 {
			t.Fatalf("adaptive stats snapshot=%t got=%s raw=%s", snapshot, a, b)
		}
		for _, field := range []string{"account", "group", "model", "key"} {
			var gotOptions, wantOptions []managedUsageOption
			if field == "account" || field == "group" {
				gotOptions, err = m.managedUsageOptionsPGCurrent(t.Context(), q, field, "", 10)
				if err == nil {
					wantOptions, err = m.managedUsageOptionsPGCurrent(t.Context(), raw, field, "", 10)
				}
			} else {
				gotOptions, err = m.managedUsageOptions(t.Context(), q, field, "", 10, nil)
				if err == nil {
					wantOptions, err = m.managedUsageOptions(t.Context(), raw, field, "", 10, nil)
				}
			}
			if err != nil || !reflect.DeepEqual(gotOptions, wantOptions) {
				t.Fatalf("dense adaptive options field=%s snapshot=%t: got=%+v raw=%+v err=%v", field, snapshot, gotOptions, wantOptions, err)
			}
		}
	}
}

func TestPGManagedUsageClassMinuteRollupParity(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	db, m := p.Store.DB, p.Metrics
	const base int64 = 1790553600
	var lateID int64
	if err := db.Pool.QueryRow(t.Context(), "SELECT nextval(pg_get_serial_sequence('request_logs','id'))").Scan(&lateID); err != nil {
		t.Fatal(err)
	}
	// Two homogeneous minutes, one category-mixed minute whose severity is
	// homogeneous, one mixed severity minute, a success-only minute, an unsafe
	// minute, and observations in another day exercise independent dimensions.
	rows := []RequestRecord{}
	for i := range 8 {
		r := managedPerformanceRecord(i)
		r.EventID, r.Time, r.Status, r.HTTPCode, r.Error = fmt.Sprintf("class-minute-%d", i), base+int64(i)*60+10, "error", 429, "rate limit exceeded"
		if i == 1 || i == 3 {
			r.HTTPCode, r.Error = 403, "authentication failed"
		}
		if i == 4 {
			r.Status, r.HTTPCode, r.Error = "ok", 200, ""
		}
		if i == 5 {
			r.KeyName = strings.Repeat("wide", 500)
		}
		if i == 6 {
			r.Time = base + 86400 + 10
		}
		if i == 7 {
			r.Time = base + 86400 + 70
			r.ImageInputTokens = new(int64)
			*r.ImageInputTokens = math.MaxInt64
		}
		rows = append(rows, r)
	}
	rows = append(rows,
		RequestRecord{EventID: "class-minute-other-category", Time: base + 2*60 + 11, Status: "error", HTTPCode: 504, Error: "timeout", Model: "other", Groups: []string{"other"}, InputTokens: 993},
		RequestRecord{EventID: "class-minute-other-severity", Time: base + 3*60 + 11, Status: "error", HTTPCode: 429, Error: "rate limit exceeded", Model: "other", Groups: []string{"other"}, InputTokens: 997},
	)
	for _, row := range rows {
		m.Record(row)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||jsonb_build_object('unrelated_protocol_payload',repeat('unrelated payload ',2000))`); err != nil {
		t.Fatal(err)
	}
	classFilters := []string{"error_category=rate_limit", "error_category=authentication", "error_category=timeout", "error_category=unknown", "error_severity=P1", "error_severity=P3", "error_severity=unknown"}
	assertParity := func(stage string, snapshot bool) {
		t.Helper()
		for _, filter := range classFilters {
			for _, suffix := range []string{"&timezone=UTC", "&status=failed&timezone=Asia%2FShanghai", "&status=ok", fmt.Sprintf("&from=%d&to=%d&step=60", base+1, base+191), fmt.Sprintf("&from=%d&to=%d&step=60", base+1, base+31)} {
				q := managedQuery(t, filter+suffix)
				if snapshot {
					q.AsOf, q.SnapshotID = "usage-class-fixed", "usage-class-fixed"
				}
				got, err := m.managedLogStats(t.Context(), q)
				if err != nil {
					t.Fatalf("%s class summary %s%s: %v", stage, filter, suffix, err)
				}
				q.statsQueryer = db.Pool
				want, err := m.managedLogStats(t.Context(), q)
				if err != nil {
					t.Fatal(err)
				}
				got.CheckedAt, want.CheckedAt = 0, 0
				a, _ := json.Marshal(got)
				b, _ := json.Marshal(want)
				if string(a) != string(b) {
					t.Fatalf("%s filter=%s%s snapshot=%t\ngot=%s\nraw=%s", stage, filter, suffix, snapshot, a, b)
				}
			}
		}
	}
	assertParity("initial", false)
	q := managedQuery(t, "error_category=rate_limit&timezone=UTC")
	source, args := managedUsageRollupSourceWithMaterialization(q, false)
	var rawRows, compactRows int
	var compactBytes int
	if err := db.Pool.QueryRow(t.Context(), source+` SELECT (SELECT count(*) FROM raw_rows),count(*),COALESCE(max(octet_length(record::text)),0) FROM raw_projected`, args...).Scan(&rawRows, &compactRows, &compactBytes); err != nil || rawRows != 3 || compactRows != 3 || compactBytes >= 10000 {
		t.Fatalf("only mixed+unsafe matching rows should be parsed once: raw=%d compact=%d bytes=%d err=%v", rawRows, compactRows, compactBytes, err)
	}
	if _, err := db.Pool.Exec(t.Context(), `INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-class-fixed',id FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	// Capture and use the real public snapshot API for the same category.
	response := managedTestRequest(t, handler, "POST", "/api/logs/snapshot", map[string]any{"filters": map[string]string{"error_category": "rate_limit"}}, 200)
	var snapshot managedUsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, handler, "GET", "/api/logs/stats?error_category=rate_limit&as_of="+snapshot.ID, nil, 200)
	_, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES('class-late-low',to_timestamp($2::bigint),'error') RETURNING event_id
 ) INSERT INTO request_logs(id,event_id,created_at,status,model,record)
 SELECT $1,event_id,to_timestamp($2::bigint),'error','new',jsonb_build_object('event_id',event_id,'time',$2::bigint,'status','error','http_code',429,'error','rate limit exceeded','input_tokens',999999) FROM e`, lateID, base+11)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(RequestRecord{EventID: "class-new-minute", Time: base + 10*60 + 10, Status: "error", HTTPCode: 429, Error: "rate limit exceeded", InputTokens: 999999})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertParity("late low ID and continuous new minute", true)
	for _, statement := range []string{
		`DELETE FROM request_logs WHERE event_id='class-minute-6'`,
		`UPDATE request_logs SET record=record||'{"http_code":403,"error":"authentication failed","groups":["rewritten"]}'::jsonb WHERE event_id='class-minute-0'`,
		`UPDATE request_logs SET created_at=created_at+interval '0.5 second' WHERE event_id='class-minute-2'`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertParity("mutation", true)
	}
}
