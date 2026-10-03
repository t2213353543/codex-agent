package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestManagedPassiveUsageDetails(t *testing.T) {
	exerciseManagedPassiveUsageDetails(t, webTestSetup)
}
func TestPGManagedPassiveUsageDetails(t *testing.T) {
	exerciseManagedPassiveUsageDetails(t, adminPGTestProxy)
}
func exerciseManagedPassiveUsageDetails(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	rows := []RequestRecord{
		{EventID: "passive-details-one", Time: 1700000000, Model: "passive-details", AccountID: "account-passive", Account: "synthetic", KeyID: "key-passive", Groups: []string{"group-passive"}, Status: "ok", HTTPCode: 200, InputTokens: 100, OutputTokens: 20, ImageInputTokens: observedTestInt64(0), ImageOutputTokens: observedTestInt64(7), ImageInputTokensSource: "usage.input_tokens_details.image_tokens", ImageOutputTokensSource: "usage.output_tokens_details.image_tokens", AuthenticationMs: observedTestInt64(0), ResponseTransferMs: observedTestInt64(12), TimingSource: "gateway_handler_v1", ClientRequestID: "synthetic-client-request-1", ClientRequestIDSource: "request_header", ClientRequestIDScope: "request"},
		{EventID: "passive-details-two", Time: 1700000001, Model: "passive-details", AccountID: "account-passive", Account: "synthetic", KeyID: "key-passive", Groups: []string{"group-passive"}, Status: "error", HTTPCode: 503, ImageInputTokens: observedTestInt64(5), ImageOutputTokens: observedTestInt64(0), ImageInputTokensSource: "tool_usage.image_gen.input_tokens_details.image_tokens", ImageOutputTokensSource: "tool_usage.image_gen.output_tokens_details.image_tokens", AuthenticationMs: observedTestInt64(8), ResponseTransferMs: observedTestInt64(0), TimingSource: "gateway_handler_v1", ClientRequestID: "synthetic-client-request-2", ClientRequestIDSource: "request_header", ClientRequestIDScope: "connection"},
		{EventID: "passive-details-missing", Time: 1700000002, Model: "passive-details", AccountID: "account-passive", Account: "synthetic", KeyID: "key-passive", Groups: []string{"group-passive"}, Status: "ok", HTTPCode: 200},
	}
	for _, row := range rows {
		p.Metrics.Record(row)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	response := managedTestRequest(t, h, "GET", "/api/logs?model=passive-details&exact_total=true&sort_order=asc", nil, 200)
	var page managedLogPage
	if json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Total != 3 {
		t.Fatal("passive list")
	}
	for i, record := range page.Items {
		assertObservedCount(t, "record image input", record.ImageInputTokens, rows[i].ImageInputTokens)
		assertObservedCount(t, "record image output", record.ImageOutputTokens, rows[i].ImageOutputTokens)
		assertObservedCount(t, "record authentication", record.AuthenticationMs, rows[i].AuthenticationMs)
		assertObservedCount(t, "record response transfer", record.ResponseTransferMs, rows[i].ResponseTransferMs)
		if record.ClientRequestID != rows[i].ClientRequestID || record.ClientRequestIDScope != rows[i].ClientRequestIDScope || record.TimingSource != rows[i].TimingSource {
			t.Fatal("passive provenance missing")
		}
	}
	checkTotals := func(totals managedUsageTotals) {
		t.Helper()
		if totals.Requests != 3 || totals.Tokens != 120 || totals.ImageInputSamples != 2 || totals.ImageOutputSamples != 2 || totals.AuthenticationSamples != 2 || totals.ResponseTransferSamples != 2 {
			t.Fatal("coverage or base tokens changed", totals)
		}
		assertObservedCount(t, "image input sum", totals.ImageInputTokens, observedTestInt64(5))
		assertObservedCount(t, "image output sum", totals.ImageOutputTokens, observedTestInt64(7))
		if totals.AverageAuthentication == nil || *totals.AverageAuthentication != 4 || totals.AverageResponseTransfer == nil || *totals.AverageResponseTransfer != 6 {
			t.Fatal("timing average skipped measured zero")
		}
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?model=passive-details", nil, 200)
	var stats managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &stats) != nil {
		t.Fatal("stats JSON")
	}
	checkTotals(stats.managedUsageTotals)
	for _, available := range []string{"image_input_tokens", "image_output_tokens", "authentication_ms", "response_transfer_ms"} {
		if !stats.Available[available] {
			t.Fatal("coverage flag absent", available)
		}
	}
	if len(stats.Models) != 1 || len(stats.Accounts) != 1 || len(stats.Keys) != 1 || len(stats.Groups) != 1 || len(stats.Series) != 1 {
		t.Fatal("dimension mismatch")
	}
	for _, totals := range []managedUsageTotals{stats.Models[0].managedUsageTotals, stats.Accounts[0].managedUsageTotals, stats.Keys[0].managedUsageTotals, stats.Groups[0].managedUsageTotals, stats.Series[0].managedUsageTotals} {
		checkTotals(totals)
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?request_id=passive-details-missing", nil, 200)
	var missing managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &missing) != nil || missing.ImageInputTokens != nil || missing.ImageOutputTokens != nil || missing.AverageAuthentication != nil || missing.AverageResponseTransfer != nil || missing.ImageInputSamples != 0 || missing.AuthenticationSamples != 0 {
		t.Fatal("missing observations became zero")
	}
	response = managedTestRequest(t, h, "GET", "/api/logs/passive-details-one", nil, 200)
	var detail struct {
		Record managedLogRecord `json:"record"`
	}
	if json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Record.ImageInputTokens == nil || *detail.Record.ImageInputTokens != 0 {
		t.Fatal("detail zero lost")
	}
	for _, redacted := range []string{"false", "true"} {
		response = managedTestRequest(t, h, "GET", "/api/logs/export?model=passive-details&sort_order=asc&format=csv&redacted="+redacted, nil, 200)
		parsed, err := csv.NewReader(bytes.NewReader(response.Body.Bytes())).ReadAll()
		if err != nil || len(parsed) != 4 || len(parsed[0]) != 77 || parsed[0][71] != "model_consistency" || parsed[0][72] != "official_runtime_ms" || parsed[0][73] != "official_inference_ms" || parsed[0][74] != "sent_reasoning_effort" || parsed[0][75] != "final_upstream_error_code" || parsed[0][76] != "final_upstream_error_source" {
			t.Fatal("CSV75 with model consistency, official runtime timings and sent effort", err)
		}
		header := map[string]int{}
		for i, name := range parsed[0] {
			header[name] = i
		}
		for _, name := range []string{"client_request_id", "client_request_id_source", "client_request_id_scope", "authentication_ms", "response_transfer_ms", "timing_source", "image_input_tokens", "image_output_tokens", "image_input_tokens_source", "image_output_tokens_source"} {
			if _, ok := header[name]; !ok {
				t.Fatal("CSV field absent", name)
			}
		}
		if parsed[1][header["image_input_tokens"]] != "0" || parsed[2][header["image_output_tokens"]] != "0" || parsed[3][header["image_input_tokens"]] != "" || parsed[1][header["authentication_ms"]] != "0" || parsed[3][header["response_transfer_ms"]] != "" {
			t.Fatal("CSV nullable observations changed")
		}
		if redacted == "true" {
			if strings.Contains(response.Body.String(), "synthetic-client-request") {
				t.Fatal("client request identity not pseudonymized")
			}
		} else if parsed[1][header["client_request_id"]] != "synthetic-client-request-1" {
			t.Fatal("client request id lost")
		}
		response = managedTestRequest(t, h, "GET", "/api/logs/export?model=passive-details&format=jsonl&redacted="+redacted, nil, 200)
		if !strings.Contains(response.Body.String(), `"image_input_tokens":0`) || !strings.Contains(response.Body.String(), `"authentication_ms":0`) {
			t.Fatal("JSONL missing measured zero")
		}
	}
	filters := map[string]string{"model": "passive-details"}
	response = managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": filters}, 200)
	var snapshot managedUsageSnapshot
	if json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Total != 3 {
		t.Fatal("snapshot")
	}
	p.Metrics.Record(RequestRecord{EventID: "passive-details-late", Time: 1700000003, Model: "passive-details", Status: "ok", ImageInputTokens: observedTestInt64(999), AuthenticationMs: observedTestInt64(999)})
	response = managedTestRequest(t, h, "GET", "/api/logs/stats?model=passive-details&as_of="+url.QueryEscape(snapshot.ID), nil, 200)
	var frozen managedUsageStats
	if json.Unmarshal(response.Body.Bytes(), &frozen) != nil {
		t.Fatal("snapshot stats")
	}
	checkTotals(frozen.managedUsageTotals)
	if p.Store.DB != nil {
		var observed int
		if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs WHERE record->>'event_id'='passive-details-one' AND record->>'image_input_tokens'='0' AND record->>'authentication_ms'='0'").Scan(&observed); err != nil || observed != 1 {
			t.Fatal("durable nullable observations missing", err)
		}
	}
}

func TestManagedPassiveMetadataPrivacy(t *testing.T) {
	r := RequestRecord{Status: "ok", ClientRequestID: "Bearer PRIVATE_CLIENT_SECRET", ClientRequestIDSource: "access_token=PRIVATE_SOURCE_SECRET", ImageInputTokensSource: "Bearer PRIVATE_IMAGE_SECRET", ImageOutputTokensSource: "https://name:PRIVATE_PROXY_SECRET@proxy.invalid", TimingSource: "refresh_token=PRIVATE_TIMING_SECRET"}
	for _, redacted := range []bool{false, true} {
		out := managedExportRecord(r, redacted)
		raw, _ := json.Marshal(out)
		for _, secret := range []string{"PRIVATE_CLIENT_SECRET", "PRIVATE_SOURCE_SECRET", "PRIVATE_IMAGE_SECRET", "PRIVATE_PROXY_SECRET", "PRIVATE_TIMING_SECRET"} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("unsafe imported passive metadata retained", secret)
			}
		}
	}
}

func TestManagedPassiveImageOverflow(t *testing.T)   { exerciseManagedPassiveImageOverflow(t, false) }
func TestPGManagedPassiveImageOverflow(t *testing.T) { exerciseManagedPassiveImageOverflow(t, true) }
func exerciseManagedPassiveImageOverflow(t *testing.T, pg bool) {
	var m *Metrics
	if pg {
		m = newTestPGMetrics(t, newMetricsTestDB(t))
	} else {
		var err error
		m, err = NewMetrics(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
	}
	for i, n := range []int64{math.MaxInt64, 1, 0} {
		m.Record(RequestRecord{EventID: fmt.Sprintf("image-overflow-%d", i), Time: 1700000000 + int64(i), Model: "overflow-fixture", Status: "ok", InputTokens: 1, ImageInputTokens: observedTestInt64(n), ImageOutputTokens: observedTestInt64(n)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	stats, err := m.managedLogStats(context.Background(), managedQuery(t, "model=overflow-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(v managedUsageTotals) {
		t.Helper()
		if v.Requests != 3 || v.Input != 3 || v.ImageInputSamples != 3 || v.ImageOutputSamples != 3 || v.ImageInputTokens != nil || v.ImageOutputTokens != nil || !v.ImageInputTotalOverflow || !v.ImageOutputTotalOverflow {
			t.Fatal("image overflow fabricated a sum or damaged usage", v)
		}
	}
	check(stats.managedUsageTotals)
	check(stats.Models[0].managedUsageTotals)
	check(stats.Series[0].managedUsageTotals)
	one, err := m.managedLogStats(context.Background(), managedQuery(t, "request_id=image-overflow-0"))
	if err != nil || one.ImageInputTotalOverflow || one.ImageOutputTotalOverflow || one.ImageInputTokens == nil || *one.ImageInputTokens != math.MaxInt64 {
		t.Fatal("valid boundary became overflow", err)
	}
}
