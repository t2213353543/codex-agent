package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestManagedNativeLogFiltersAvoidJSONProjection(t *testing.T) {
	for _, filter := range []string{"status=error", "status=ok&model=m", "key_name=key&model=m&status=aborted"} {
		q := managedQuery(t, filter)
		where, _ := q.whereSQL()
		if strings.Contains(where, "record") || q.indexedFilterTags() != nil {
			t.Fatalf("native filter unnecessarily decodes JSON: %s", where)
		}
	}
	for _, filter := range []string{"status=error&http_code=429", "model=m&key_id=k", "status=ok&stream=false"} {
		where, _ := managedQuery(t, filter).whereSQL()
		if !strings.Contains(where, managedLogFilterProjection) {
			t.Fatalf("JSON filter lost indexed projection: %s", where)
		}
	}
}

func TestPGManagedFilterTagsPreservePredicates(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	yes, no := true, false
	for i, r := range []RequestRecord{
		{Model: "m", Endpoint: "responses", Status: "ok", KeyID: "k", KeyName: "key", Groups: []string{"g", "g"}, HTTPCode: 200, Stream: true, IP: "192.0.2.1", SentModel: "sent", ReturnedModel: "returned", UpstreamModel: "up", RequestType: "http", ServiceTier: "fast", ReasoningEffort: "high", ErrorPhase: "write", InboundPath: "/v1/responses", UpstreamPath: "/responses", WebSocketWarmup: &no, NativeCompaction: &yes, ErrorTruncated: &no},
		{Model: "m2", Status: "error", HTTPCode: 429, KeyID: "k2", Groups: []string{"g2"}, Stream: false, WebSocketWarmup: &yes},
		{Model: "", Status: "aborted"},
	} {
		r.EventID = fmt.Sprintf("filter-tags-%d", i)
		r.Time = 1800000000 + int64(i)
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"", "model=m", "model=missing", "model=m&endpoint=responses&key_id=k&group_id=g&stream=true", "status=failed", "status=error&http_code=429", "http_code=200", "stream=false", "websocket_warmup=false", "websocket_warmup=true", "native_compaction=true", "error_truncated=false", "ip=192.0.2.1", "sent_model=sent&returned_model=returned", "upstream_model=up&request_type=http&service_tier=fast&reasoning_effort=high", "inbound_path=%2Fv1%2Fresponses&upstream_path=%2Fresponses", "error_phase=write"} {
		q := managedQuery(t, filter)
		where, args := q.whereSQL()
		want := []int64{}
		got := []int64{}
		rows, err := db.Pool.Query(t.Context(), "SELECT id,record FROM request_logs ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r RequestRecord
			var raw []byte
			var id int64
			if err := rows.Scan(&id, &raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			r.ID = id
			if q.matches(r) {
				want = append(want, id)
			}
		}
		rows.Close()
		rows, err = db.Pool.Query(t.Context(), "SELECT id FROM request_logs WHERE "+where+" ORDER BY id", args...)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("filter=%s got%v want%v err=%v", filter, got, want, err)
		}
	}
	// SQL's ->> predicates accept scalar JSON textual representations. The
	// acceleration payload must match those even for historical numeric data.
	for _, raw := range []string{`{"key_id":123,"groups":["g"],"stream":"true"}`, `{"groups":{"g":true},"native_compaction":"false"}`, `{"groups":"g","http_code":"429"}`, `{"groups":null}`, `{}`} {
		var tags []byte
		if err := db.Pool.QueryRow(t.Context(), "SELECT request_log_filter_tags($1::jsonb,'m','key','ok')", raw).Scan(&tags); err != nil {
			t.Fatal(err)
		}
		for _, group := range []string{"g", "missing"} {
			var want, got bool
			if err := db.Pool.QueryRow(t.Context(), `SELECT COALESCE($1::jsonb->'groups','[]'::jsonb) ? $2,request_log_filter_tags($1::jsonb,'m','key','ok') @> jsonb_build_object('groups',jsonb_build_array($2::text))`, raw, group).Scan(&want, &got); err != nil || want != got {
				t.Fatalf("historical groups=%s group=%s want=%v got=%v err=%v", raw, group, want, got, err)
			}
		}
	}
}

func TestPGManagedModelSortLongValues(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	models := []string{"a", "b", "", strings.Repeat("a", 50000), strings.Repeat("z", 4000), "名字", strings.Repeat("<", 8192), strings.Repeat("界", 1800)}
	for i := 0; i < 35; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("sort-model-%d", i), Time: 1800000000 + int64(i%4), Status: "ok", Model: models[i%len(models)]})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"asc", "desc"} {
		want := []int64{}
		rows, err := db.Pool.Query(t.Context(), `SELECT id FROM request_logs ORDER BY model COLLATE "C" `+direction+",created_at "+direction+",id "+direction)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			want = append(want, id)
		}
		rows.Close()
		q := managedQuery(t, "sort_by=model&sort_order="+direction+"&page_size=3")
		got := []int64{}
		for {
			page, err := m.managedLogs(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range page.Items {
				got = append(got, r.ID)
			}
			if !page.HasMore {
				break
			}
			if len(page.NextCursor) > 1024 {
				t.Fatal("self-generated cursor exceeds URL budget")
			}
			q.Cursor = page.NextCursor
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cursor %s got%v want%v", direction, got, want)
		}
		q.Cursor = ""
		q.Page = 4
		page, err := m.managedLogs(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range page.Items {
			if r.ID != want[9+i] {
				t.Fatal("numbered model page differs")
			}
		}
	}
}
