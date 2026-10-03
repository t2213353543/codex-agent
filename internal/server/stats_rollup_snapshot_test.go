package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPGManagedSnapshotRollupMinuteBoundaryParity(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	const base int64 = 1790553600 // Aligned UTC day/minute boundaries.
	var lateID int64
	if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT nextval(pg_get_serial_sequence('request_logs','id'))").Scan(&lateID); err != nil {
		t.Fatal(err)
	}
	for i := range 48 {
		r := managedPerformanceRecord(i)
		r.EventID, r.Time, r.Model = fmt.Sprintf("snapshot-rollup-%d", i), base+int64(i)*3600+10, "selected-model"
		if i%3 == 0 {
			r.Status, r.HTTPCode, r.Error = "error", 429, "rate limit exceeded"
		}
		p.Metrics.Record(r)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	filters := map[string]string{"from": strconv.FormatInt(base, 10), "to": strconv.FormatInt(base+3*86400, 10), "timezone": "UTC", "step": "86400"}
	response := managedTestRequest(t, handler, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Total != 48 {
		t.Fatalf("capture snapshot: %s %v", response.Body.String(), err)
	}
	values := url.Values{}
	for name, value := range filters {
		values.Set(name, value)
	}
	values.Set("as_of", snapshot.ID)
	q := managedQuery(t, values.Encode())
	q.SnapshotID = snapshot.ID
	if !managedUsageRollupEligible(q) {
		t.Fatal("resolved fixed-ID snapshot should use guarded rollups")
	}
	assertParity := func(stage string) {
		t.Helper()
		for _, variant := range []struct {
			zone, status, model string
			from, to            int64
		}{
			{"UTC", "", "", base, base + 3*86400}, {"Asia/Shanghai", "", "", base, base + 3*86400},
			{"UTC", "failed", "", base, base + 3*86400}, {"UTC", "", "selected-model", base, base + 3*86400},
			{"UTC", "", "", base + 1, base + 3600 + 31}, {"UTC", "", "", base + 1, base + 31},
		} {
			selected := q
			selected.Timezone, selected.Status, selected.Model = variant.zone, variant.status, variant.model
			selected.From, selected.To = variant.from, variant.to
			got, err := p.Metrics.managedLogStats(t.Context(), selected)
			if err != nil {
				t.Fatalf("%s optimized %v: %v", stage, variant, err)
			}
			selected.statsQueryer = p.Store.DB.Pool
			want, err := p.Metrics.managedLogStats(t.Context(), selected)
			if err != nil {
				t.Fatal(err)
			}
			got.CheckedAt, want.CheckedAt = 0, 0
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("%s %v optimized=%s raw=%s", stage, variant, a, b)
			}
		}
	}
	rawRows := func() int {
		t.Helper()
		source, args := managedUsageRollupSourceWithMaterialization(q, false)
		var count int
		if err := p.Store.DB.Pool.QueryRow(t.Context(), source+" SELECT count(*) FROM raw_rows", args...).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	assertParity("captured")
	if count := rawRows(); count != 0 {
		t.Fatalf("unchanged snapshot read %d raw records instead of summaries", count)
	}
	// The actual UI creates a snapshot before requesting stats. This must use
	// the same optimized path, not just a Metrics-only test configuration.
	managedTestRequest(t, handler, "GET", "/api/logs/stats?"+values.Encode(), nil, 200)
	_, err := p.Store.DB.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES('snapshot-late-low-id',to_timestamp($2::bigint),'ok') RETURNING event_id
 ) INSERT INTO request_logs(id,event_id,created_at,status,model,record)
 SELECT $1,event_id,to_timestamp($2::bigint),'ok','selected-model',jsonb_build_object('event_id',event_id,'time',$2::bigint,'status','ok','model','selected-model','input_tokens',999999) FROM e`, lateID, base+11)
	if err != nil {
		t.Fatal(err)
	}
	p.Metrics.Record(RequestRecord{EventID: "snapshot-new-minute", Time: base + 120, Model: "selected-model", Status: "ok", InputTokens: 999999})
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertParity("late-low-ID and new-minute commits")
	if count := rawRows(); count != 1 {
		t.Fatalf("only one selected row shares a dirty minute; raw scan got %d", count)
	}
	for _, statement := range []string{
		`DELETE FROM request_logs WHERE event_id='snapshot-rollup-47'`,
		`UPDATE request_logs SET record=record||'{"groups":["renamed-group","renamed-group"],"account":"updated label"}'::jsonb,account_name='updated label' WHERE event_id='snapshot-rollup-3'`,
	} {
		if _, err := p.Store.DB.Pool.Exec(t.Context(), statement, pgx.QueryExecModeExec); err != nil {
			t.Fatal(err)
		}
		assertParity("concurrent snapshot member mutation")
	}
}
