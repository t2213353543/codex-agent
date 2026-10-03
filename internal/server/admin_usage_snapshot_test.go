package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestManagedUsageSnapshot(t *testing.T)   { exerciseManagedUsageSnapshot(t, webTestSetup) }
func TestPGManagedUsageSnapshot(t *testing.T) { exerciseManagedUsageSnapshot(t, adminPGTestProxy) }
func exerciseManagedUsageSnapshot(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	var commit func() error
	var insertLate func() error
	if p.Store.DB != nil {
		tx, err := p.Store.DB.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Reserve a lower identity before the committed request, but insert its
		// detail only afterwards. Inserting first would hold the same-minute
		// summary row lock while synchronously waiting for the other writer.
		// The smaller-ID, same-timestamp, later-commit snapshot case is unchanged.
		var lateID int64
		if err = tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('request_logs','id'))").Scan(&lateID); err != nil {
			t.Fatal(err)
		}
		insertLate = func() error {
			_, err := tx.Exec(ctx, `WITH event AS (INSERT INTO usage_events(event_id,created_at,status,input_tokens) VALUES('late-commit',to_timestamp(1700000000),'ok',999) RETURNING event_id) INSERT INTO request_logs(id,event_id,created_at,status,model,record) SELECT $1,event_id,to_timestamp(1700000000),'ok','snapshot-model','{"event_id":"late-commit","time":1700000000,"status":"ok","model":"snapshot-model","input_tokens":999}' FROM event`, lateID)
			return err
		}
		commit = func() error { return tx.Commit(ctx) }
	}
	p.Metrics.Record(RequestRecord{EventID: "snapshot-before", Time: 1700000000, Model: "snapshot-model", Status: "ok", InputTokens: 10})
	if insertLate != nil {
		if err := insertLate(); err != nil {
			t.Fatal(err)
		}
	}
	filters := map[string]string{"model": "snapshot-model", "from": "1699999999", "to": "1700000001"}
	response := managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Total != 1 {
		t.Fatal("snapshot capture", err, response.Body.String())
	}
	if commit != nil {
		if err := commit(); err != nil {
			t.Fatal(err)
		}
	}
	p.Metrics.Record(RequestRecord{EventID: "snapshot-after", Time: 1700000000, Model: "snapshot-model", Status: "ok", InputTokens: 888})
	if err := p.managementState().Update(ctx, "usage-pricing", "catalog", func(entry *managementStateEntry) error {
		entry.Payload = json.RawMessage(`{"retired_data":"must_not_be_read"}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	values := url.Values{}
	for k, v := range filters {
		values.Set(k, v)
	}
	values.Set("as_of", snapshot.ID)
	values.Set("exact_total", "true")
	response = managedTestRequest(t, h, "GET", "/api/logs?"+values.Encode(), nil, 200)
	var page managedLogPage
	if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].EventID != "snapshot-before" {
		t.Fatal("late rows entered snapshot", response.Body.String())
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?"+values.Encode(), nil, 200)
	var stats managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &stats) != nil || stats.Requests != 1 || stats.Input != 10 || strings.Contains(response.Body.String(), "cost") {
		t.Fatal("stats disagreed with snapshot", response.Body.String())
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/export?format=jsonl&redacted=false&"+values.Encode(), nil, 200)
	if response.Header().Get("X-Export-Count") != "1" || strings.Contains(response.Body.String(), "snapshot-after") || strings.Contains(response.Body.String(), "late-commit") {
		t.Fatal("export disagreed with snapshot")
	}
	values.Set("model", "changed-filter")
	managedTestRequest(t, h, "GET", "/api/logs?"+values.Encode(), nil, 409)
	values.Set("model", "snapshot-model")
	if p.Store.DB != nil {
		filters["as_of"] = snapshot.ID
		response = managedTestRequest(t, h, "POST", "/api/logs/cleanup/preview", map[string]any{"filters": filters}, 200)
		var preview managedCleanupPreview
		if json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.Total != 1 {
			t.Fatal("cleanup enlarged fixed snapshot")
		}
	}
	err := p.managementState().Update(ctx, "usage-snapshot", snapshot.ID, func(entry *managementStateEntry) error {
		entry.ExpiresAt = time.Now().Add(-time.Second).Unix()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "GET", "/api/logs?"+values.Encode(), nil, 409)
}
