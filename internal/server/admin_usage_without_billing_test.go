package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestManagedUsageWithoutBilling(t *testing.T)   { exerciseUsageWithoutBilling(t, webTestSetup) }
func TestPGManagedUsageWithoutBilling(t *testing.T) { exerciseUsageWithoutBilling(t, adminPGTestProxy) }
func exerciseUsageWithoutBilling(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	before := map[string]managementStateEntry{}
	for _, ns := range []string{"usage-pricing", "usage-procurement", "usage-sale", "usage-cost-policy"} {
		if err := p.managementState().Update(ctx, ns, "catalog", func(e *managementStateEntry) error {
			e.Payload = json.RawMessage(`{"retired":"unparseable_as_a_price_list","input_per_million":"42"}`)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		entry, err := p.managementState().Get(ctx, ns, "catalog")
		if err != nil {
			t.Fatal(err)
		}
		before[ns] = entry
	}
	known := true
	zero := int64(0)
	for _, record := range []RequestRecord{{EventID: "usage-only-a", Time: 1700000000, AccountID: "a", Account: "before", KeyID: "k", Model: "test-model", SentModel: "test-model", ReturnedModel: "actual-model", Status: "ok", InputTokens: 20, OutputTokens: 4, CachedTokens: 10, UsageKnown: &known, RequestPreparationMs: &zero, UsageAmount: "999.99"}, {EventID: "usage-only-b", Time: 1700000001, AccountID: "a", Account: "after", KeyID: "k", Model: "test-model", Status: "error", HTTPCode: 400, Error: "synthetic failure"}} {
		p.Metrics.Record(record)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertNoBilling := func(raw []byte) {
		t.Helper()
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		var walk func(any)
		walk = func(v any) {
			switch item := v.(type) {
			case map[string]any:
				for key, entry := range item {
					for _, fragment := range []string{"cost", "price", "pricing", "currency", "multiplier", "usage_amount", "rollout_budget"} {
						if strings.Contains(key, fragment) {
							t.Fatalf("billing field %s in usage response", key)
						}
					}
					walk(entry)
				}
			case []any:
				for _, entry := range item {
					walk(entry)
				}
			}
		}
		walk(value)
	}
	for _, path := range []string{"/api/logs?model=test-model&exact_total=true", "/api/logs/stats?model=test-model", "/api/logs/usage-only-a", "/api/logs/errors?model=test-model"} {
		response := managedTestRequest(t, h, "GET", path, nil, 200)
		assertNoBilling(response.Body.Bytes())
	}
	statsResponse := managedTestRequest(t, h, "GET", "/api/logs/stats?model=test-model", nil, 200)
	var stats managedUsageStats
	_ = json.Unmarshal(statsResponse.Body.Bytes(), &stats)
	if stats.Requests != 2 || stats.Tokens != 24 || stats.Success != 1 || stats.Errors != 1 || stats.RequestPreparationSamples != 1 || stats.AverageRequestPreparation == nil || *stats.AverageRequestPreparation != 0 {
		t.Fatal("nonbilling statistics regressed", stats)
	}
	response := managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": map[string]string{"model": "test-model"}}, 200)
	var snapshot struct {
		ID string `json:"as_of"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &snapshot)
	entry, err := p.managementState().Get(ctx, "usage-snapshot", snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoBilling(entry.Payload)
	for _, format := range []string{"csv", "jsonl"} {
		response = managedTestRequest(t, h, "GET", "/api/logs/export?model=test-model&format="+format+"&as_of="+url.QueryEscape(snapshot.ID), nil, 200)
		if response.Header().Get("X-Export-Schema") != "codex-usage-v4" {
			t.Fatal("schema version")
		}
		if format == "csv" {
			rows, err := csv.NewReader(bytes.NewReader(response.Body.Bytes())).ReadAll()
			if err != nil || len(rows) != 3 {
				t.Fatal("csv", err)
			}
			for _, name := range rows[0] {
				if strings.Contains(name, "cost") || strings.Contains(name, "pric") || strings.Contains(name, "amount") || strings.Contains(name, "budget") {
					t.Fatal("money CSV column", name)
				}
			}
		} else {
			for _, line := range bytes.Split(bytes.TrimSpace(response.Body.Bytes()), []byte("\n")) {
				assertNoBilling(line)
			}
		}
	}
	for ns, old := range before {
		current, err := p.managementState().Get(ctx, ns, "catalog")
		if err != nil || current.Version != old.Version || !bytes.Equal(current.Payload, old.Payload) {
			t.Fatal("retired data was changed", ns, err)
		}
	}
}
