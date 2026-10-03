package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestManagementStateBatch(t *testing.T)   { exerciseManagementStateBatch(t, webTestSetup) }
func TestPGManagementStateBatch(t *testing.T) { exerciseManagementStateBatch(t, adminPGTestProxy) }

func exerciseManagementStateBatch(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, handler := setup(t)
	store := p.managementState()
	for _, key := range []string{"kept", "expired", "unrelated"} {
		if err := store.Update(t.Context(), "batch-test", key, func(entry *managementStateEntry) error {
			entry.Payload = json.RawMessage(`{"value":42}`)
			if key == "expired" {
				entry.ExpiresAt = time.Now().Unix() - 1
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var before int64
	if p.Store.DB != nil {
		before = p.Store.DB.Pool.Stat().AcquireCount()
	}
	entries, err := store.GetMany(t.Context(), "batch-test", []string{"kept", "kept", "expired", "missing"})
	var payload struct {
		Value int `json:"value"`
	}
	decodeErr := json.Unmarshal(entries["kept"].Payload, &payload)
	if err != nil || len(entries) != 1 || decodeErr != nil || payload.Value != 42 {
		t.Fatalf("batch lookup: %v %v", entries, err)
	}
	if p.Store.DB != nil && p.Store.DB.Pool.Stat().AcquireCount()-before != 1 {
		t.Fatal("batch lookup added per-document database requests")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.GetMany(ctx, "batch-test", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch: %v", err)
	}
	if _, err := store.GetMany(t.Context(), "invalid/namespace", nil); err == nil {
		t.Fatal("empty batch skipped namespace validation")
	}
	// A review for an unrelated retained record must not be loaded/decoded for
	// an ordinary page. Resolved-set filtering still needs the full annotation
	// set and therefore must report its corruption instead of fabricating it.
	p.Metrics.Record(RequestRecord{EventID: "batch-error", Time: time.Now().Unix(), Status: "error", HTTPCode: 500})
	if err := store.Update(t.Context(), "usage-review", "unrelated-corrupt", func(entry *managementStateEntry) error {
		entry.Payload = json.RawMessage(`{"review":{"record_ref":"mismatched"}}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/logs/errors?page_size=1", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("unrelated review blocked error page: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodGet, "/api/logs/errors?resolved=true", "", "Bearer "+webTestKey)
	if response.Code < 400 {
		t.Fatal("filtered review silently ignored corrupt resolved set")
	}
}
