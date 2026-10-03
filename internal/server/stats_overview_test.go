package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func checkOverviewBreakdown(t *testing.T, m *Metrics) {
	t.Helper()
	base := time.Now().Unix() / 60 * 60
	for i, rec := range []RequestRecord{
		{Time: base - 1, Model: "outside", Status: "ok", InputTokens: 900},
		{Time: base, Model: "gpt-6-astra", Status: "ok", InputTokens: 20, OutputTokens: 5},
		{Time: base + 1, Model: "gpt-6-astra", Status: "error", HTTPCode: 429},
		{Time: base + 2, Model: "gpt-5.6-sol", Status: "error", HTTPCode: 503},
		{Time: base + 3, Model: "gpt-5.6-sol", Status: "aborted", HTTPCode: 499},
		{Time: base + 60, Model: "outside", Status: "error", HTTPCode: 401},
	} {
		rec.EventID = "overview-" + itoa(int64(i))
		m.Record(rec)
	}
	got, err := m.OverviewBreakdown(context.Background(), base, base+60)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "retained_request_details" || got.Requests != 4 || got.Errors != 3 || len(got.Models) != 2 || len(got.ErrorCategories) != 3 {
		t.Fatalf("incorrect scope/counts: %+v", got)
	}
	if got.Models[0].Name != "gpt-5.6-sol" || got.Models[0].Errors != 2 || got.Models[1].Tokens != 25 || got.Models[0].UsageKnown != 0 || got.Models[1].UsageKnown != 1 {
		t.Fatalf("incorrect model aggregation/order: %+v", got.Models)
	}
	for i, expected := range []string{"aborted", "rate_limit", "server_error"} {
		if got.ErrorCategories[i].Name != expected || got.ErrorCategories[i].Requests != 1 {
			t.Fatalf("incorrect error categories: %+v", got.ErrorCategories)
		}
	}
	empty, err := m.OverviewBreakdown(context.Background(), base+61, base+62)
	if err != nil || empty.Requests != 0 || empty.Models == nil || empty.ErrorCategories == nil {
		t.Fatalf("empty breakdown must retain zero and arrays: %+v, %v", empty, err)
	}
}

func TestOverviewBreakdownFile(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	checkOverviewBreakdown(t, m)
}

func TestOverviewBreakdownPG(t *testing.T) {
	checkOverviewBreakdown(t, newTestPGMetrics(t, newMetricsTestDB(t)))
}

func TestOverviewBreakdownOptInAndLiveAdmission(t *testing.T) {
	p, handler := webTestSetup(t)
	active := int64(3)
	p.admissionMu.Lock()
	p.admission = map[string]*int64{"id:a": &active}
	p.admissionMu.Unlock()
	for _, query := range []string{"", "?include_breakdown=true"} {
		response := webTestRequest(handler, http.MethodGet, "/api/overview"+query, "", "Bearer "+webTestKey)
		var data struct {
			Cards     map[string]any     `json:"cards"`
			Breakdown *overviewBreakdown `json:"traffic_breakdown"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &data) != nil {
			t.Fatalf("overview failed: %d %s", response.Code, response.Body.String())
		}
		if data.Cards["in_flight"] != float64(3) || (data.Breakdown != nil) != (query != "") {
			t.Fatalf("wrong live admission or opt-in: %+v", data)
		}
	}
}
