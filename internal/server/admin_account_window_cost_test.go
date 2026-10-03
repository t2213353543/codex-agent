package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestManagedAccountRequestUSD(t *testing.T) {
	known, unknown := true, false
	tests := []struct {
		name   string
		record RequestRecord
		want   float64
		known  bool
	}{
		{"cached input counted once", RequestRecord{Model: "gpt-6-astra", InputTokens: 100000, CachedTokens: 80000, OutputTokens: 1000}, .33, true},
		{"fast selected once", RequestRecord{Model: "gpt-6-astra", ServiceTier: "fast", InputTokens: 100000, CachedTokens: 80000, OutputTokens: 1000}, .66, true},
		{"long context", RequestRecord{Model: "gpt-6-astra", InputTokens: 300000, CachedTokens: 200000, OutputTokens: 1000}, 2.475, true},
		{"exact threshold stays short", RequestRecord{Model: "gpt-6-astra", InputTokens: 272000}, 2.72, true},
		{"known zero", RequestRecord{Model: "gpt-6-astra", UsageKnown: &known}, 0, true},
		{"missing usage", RequestRecord{Model: "gpt-6-astra"}, 0, false},
		{"explicit unknown", RequestRecord{Model: "gpt-6-astra", InputTokens: 10, UsageKnown: &unknown}, 0, false},
		{"account cost uses sent model", RequestRecord{Model: "gpt-6-astra", SentModel: "gpt-6-astra", ReturnedModel: "gpt-6-sol", InputTokens: 100000}, 1, true},
		{"dated returned model", RequestRecord{ReturnedModel: "gpt-6-astra-2026-09-01", InputTokens: 100000}, 1, true},
		{"unknown response does not reprice upstream", RequestRecord{Model: "gpt-6-astra", ReturnedModel: "unknown-model", InputTokens: 100000}, 1, true},
		{"invalid cache", RequestRecord{Model: "gpt-6-astra", InputTokens: 1, CachedTokens: 2}, 0, false},
		{"no invented model price", RequestRecord{Model: "gpt-6-terra", InputTokens: 100000}, 0, false},
		{"sub2api source sol rates", RequestRecord{Model: "gpt-5.6-sol", InputTokens: 100000, OutputTokens: 1000}, .53, true},
		{"unpriced unsupported sol alias", RequestRecord{Model: "gpt-6-sol", InputTokens: 100000}, 0, false},
		{"flex without separate catalog fields", RequestRecord{Model: "gpt-6-astra", ServiceTier: "flex", InputTokens: 100000, CachedTokens: 80000, OutputTokens: 1000}, .165, true},
		{"GPT 5.5 fast override exactly once", RequestRecord{Model: "gpt-5.5", ServiceTier: "fast", InputTokens: 100000}, 1.25, true},
		{"GPT reasoning max no invented multiplier", RequestRecord{Model: "gpt-6-astra", ReasoningEffort: "max", InputTokens: 100000}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := managedAccountRequestUSD(tt.record)
			if ok != tt.known || math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("got %g, %v; want %g, %v", got, ok, tt.want, tt.known)
			}
		})
	}
}

func TestManagedAccountWindowCosts(t *testing.T)   { runManagedAccountWindowCosts(t, webTestSetup) }
func TestPGManagedAccountWindowCosts(t *testing.T) { runManagedAccountWindowCosts(t, adminPGTestProxy) }

func runManagedAccountWindowCosts(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	account := managedTestAccount(t, p, "window-cost", nil)
	now := time.Now().Unix()
	reset, seconds := now+3600, int64(7200)
	start := reset - seconds
	for i, record := range []RequestRecord{
		{AccountID: account.ID, Time: start, Model: "gpt-6-astra", InputTokens: 100000, CachedTokens: 80000, OutputTokens: 1000, Status: "ok"},
		{AccountID: account.ID, Time: now - 1, Model: "unpriced", InputTokens: 10, Status: "ok"},
		{AccountID: account.ID, Time: start - 1, Model: "gpt-6-astra", InputTokens: 100000, Status: "ok"},
		{AccountID: "other-account", Time: now - 1, Model: "gpt-6-astra", InputTokens: 100000, Status: "ok"},
		{AccountID: account.ID, Time: reset, Model: "gpt-6-astra", InputTokens: 100000, Status: "ok"},
	} {
		record.ID = int64(i + 1)
		p.Metrics.Record(record)
	}
	if err := p.Metrics.Save(); err != nil {
		t.Fatal(err)
	}
	snapshot := managedUpstreamSnapshot{ID: account.ID, Kind: "usage", OK: true, CheckedAt: now, Data: managedUsage{PlanType: "team", RateLimit: &managedRateLimit{PrimaryWindow: &managedRateWindow{ResetAt: &reset, LimitWindowSeconds: &seconds}}}}
	p.enrichManagedWindowCosts(context.Background(), []*managedUpstreamSnapshot{&snapshot})
	cost := snapshot.LocalWindowCosts["primary_window"]
	if cost == nil || cost.USD == nil || math.Abs(*cost.USD-.33) > 1e-9 || cost.From != start || cost.Requests != 2 || cost.EstimatedRequests != 1 || cost.UnknownRequests != 1 {
		t.Fatalf("wrong identity/window/coverage: %+v", cost)
	}
	// Derived costs must not mutate the official provider data.
	raw, _ := json.Marshal(snapshot.Data)
	var official map[string]any
	json.Unmarshal(raw, &official)
	if _, ok := official["local_window_costs"]; ok {
		t.Fatal("local dollars leaked into official response")
	}
	// Cache reuse must see newly persisted local requests.
	p.Metrics.Record(RequestRecord{AccountID: account.ID, Time: now - 1, Model: "gpt-6-astra", InputTokens: 1000, Status: "ok"})
	if err := p.Metrics.Save(); err != nil {
		t.Fatal(err)
	}
	p.enrichManagedWindowCosts(context.Background(), []*managedUpstreamSnapshot{&snapshot})
	if cost = snapshot.LocalWindowCosts["primary_window"]; cost == nil || cost.USD == nil || math.Abs(*cost.USD-.34) > 1e-9 || cost.Requests != 3 {
		t.Fatalf("cached official snapshot froze local costs: %+v", cost)
	}
	reset = now - 1
	beforeFallback := time.Now().Unix() - seconds
	p.enrichManagedWindowCosts(context.Background(), []*managedUpstreamSnapshot{&snapshot})
	afterFallback := time.Now().Unix() - seconds
	if cost := snapshot.LocalWindowCosts["primary_window"]; cost == nil || cost.From < beforeFallback || cost.From > afterFallback {
		t.Fatalf("Sub2API rolling fallback missing: %+v", cost)
	}
}
