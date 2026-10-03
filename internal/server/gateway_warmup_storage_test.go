package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func gatewayWarmupStorageRecords() []RequestRecord {
	now := time.Now().Unix()
	records := []RequestRecord{
		{EventID: "warmup-known-true", WebSocketWarmup: boolPointerWarmup(true), InputTokens: 7},
		{EventID: "warmup-known-false", WebSocketWarmup: boolPointerWarmup(false), InputTokens: 8, OutputTokens: 2},
		{EventID: "warmup-legacy-unknown"},
	}
	for index := range records {
		records[index].Time = now
		records[index].Account = "warmup-storage"
		records[index].KeyID = "warmup-key"
		records[index].Status = "ok"
		records[index].RequestType = "websocket"
		records[index].Endpoint = "responses"
	}
	return records
}

func assertGatewayWarmupStored(t *testing.T, metrics *Metrics) {
	t.Helper()
	page, err := metrics.Logs(LogQuery{Account: "warmup-storage", Size: 10})
	if err != nil || len(page.Items) != 3 {
		t.Fatal("warmup provenance changed persisted request cardinality")
	}
	seen := map[string]bool{}
	for _, record := range page.Items {
		seen[record.EventID] = true
		switch record.EventID {
		case "warmup-known-true":
			if record.WebSocketWarmup == nil || !*record.WebSocketWarmup || record.InputTokens != 7 {
				t.Fatal("warmup lost true provenance or suppressed reported usage")
			}
		case "warmup-known-false":
			if record.WebSocketWarmup == nil || *record.WebSocketWarmup {
				t.Fatal("ordinary generation became unknown after persistence")
			}
		case "warmup-legacy-unknown":
			if record.WebSocketWarmup != nil || record.UsageKnown != nil {
				t.Fatal("old record acquired invented warmup or usage metadata")
			}
		default:
			t.Fatal("unexpected record in isolated warmup storage fixture")
		}
		if record.RequestType != "websocket" || record.Endpoint != "responses" {
			t.Fatal("warmup classification changed transport or endpoint")
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(encoded, &object) != nil {
			t.Fatal("invalid persisted record")
		}
		if _, present := object["websocket_warmup"]; present != (record.WebSocketWarmup != nil) {
			t.Fatal("serialized record lost known false or invented old metadata")
		}
	}
	if len(seen) != 3 {
		t.Fatal("warmup persistence duplicated a record")
	}
	if totals := metrics.GrandTotals(); totals.Requests != 3 || totals.Tokens != 17 {
		t.Fatal("passive warmup field changed aggregate request or token counts")
	}
	if usage := metrics.KeyUsage("warmup-key"); usage.Requests != 3 || usage.Tokens != 17 {
		t.Fatal("passive warmup field changed key request or token counts")
	}
}

func TestGatewayWarmupStorageFileRestart(t *testing.T) {
	directory := t.TempDir()
	metrics, err := NewMetrics(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(metrics.Close)
	for _, record := range gatewayWarmupStorageRecords() {
		metrics.Record(record)
	}
	assertGatewayWarmupStored(t, metrics)
	metrics.Close()
	// No aggregate snapshot: restart rebuilds from the durable JSONL records.
	reopened, err := NewMetrics(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	assertGatewayWarmupStored(t, reopened)
}

func TestPGGatewayWarmupStorageRestartAndRebuild(t *testing.T) {
	db := newMetricsTestDB(t)
	metrics := newTestPGMetrics(t, db)
	for _, record := range gatewayWarmupStorageRecords() {
		metrics.Record(record)
	}
	if err := metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertGatewayWarmupStored(t, metrics)
	reopened := newTestPGMetrics(t, db)
	assertGatewayWarmupStored(t, reopened)
	if err := reopened.RebuildAggregates(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertGatewayWarmupStored(t, reopened)
}
