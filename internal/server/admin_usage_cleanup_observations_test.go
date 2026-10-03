package server

import (
	"context"
	"encoding/json"
	"testing"
)

func TestManagedCleanupObservationFilters(t *testing.T) {
	for key, value := range map[string]string{"sent_model": "observed-send", "returned_model": "observed-return", "inbound_path": "/v1/responses", "upstream_path": "/responses", "sent_model_mismatch": "true", "returned_model_mismatch": "true", "error_truncated": "false", "websocket_warmup": "true"} {
		filters := map[string]string{"from": "1699999999", "to": "1700000010", key: value}
		if _, err := managedCleanupQuery(filters); err != nil {
			t.Fatalf("supported query field %s rejected for cleanup: %v", key, err)
		}
	}
	if _, err := managedCleanupQuery(map[string]string{"from": "1699999999", "to": "1700000010", "invented": "true"}); err == nil {
		t.Fatal("unsupported cleanup silently broadened range")
	}
}

func TestPGManagedCleanupObservationFilters(t *testing.T) {
	p, h := adminPGTestProxy(t)
	for _, row := range observationFixtureRecords() {
		p.Metrics.Record(row)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	filters := map[string]string{"from": "1699999999", "to": "1700000010", "sent_model": "observed-send", "returned_model": "observed-return", "inbound_path": "/v1/responses", "upstream_path": "/responses", "sent_model_mismatch": "true", "returned_model_mismatch": "true", "error_truncated": "false"}
	response := managedTestRequest(t, h, "POST", "/api/logs/cleanup/preview", map[string]any{"filters": filters}, 200)
	var preview managedCleanupPreview
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil || preview.Total != 1 {
		t.Fatal("wrong observation preview", preview, err)
	}
	response = managedTestRequest(t, h, "POST", "/api/logs/cleanup/jobs", map[string]string{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": "observation-cleanup-stable"}, 200)
	var result struct {
		Job managedCleanupJob `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if err := p.runManagedCleanupBatch(context.Background(), result.Job.ID); err != nil {
		t.Fatal(err)
	}
	var removed, remaining, events int
	if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FILTER(WHERE event_id='observation-0'),count(*) FROM request_logs").Scan(&removed, &remaining); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM usage_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if removed != 0 || remaining != 4 || events != 5 {
		t.Fatalf("cleanup widened filters or removed totals: removedPresent=%d remaining=%d durable=%d", removed, remaining, events)
	}
}
