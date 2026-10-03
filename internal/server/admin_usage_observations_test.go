package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func observationFixtureRecords() []RequestRecord {
	zero, ten, twenty, invalid := int64(0), int64(10), int64(20), int64(-1)
	yes, no := true, false
	status := 503
	base := RequestRecord{Time: 1700000000, Model: "observed-request", AccountID: "account-1", Account: "fixture", KeyID: "key-1", Groups: []string{"group-1"}, InputTokens: 100, OutputTokens: 10, CachedTokens: 20, UsageKnown: &yes, Status: "ok", Endpoint: "responses"}
	rows := []RequestRecord{base, base, base, base, base}
	for i := range rows {
		rows[i].EventID = "observation-" + strconv.Itoa(i)
		rows[i].Time += int64(i)
	}
	rows[0].SentModel = "observed-send"
	rows[0].ReturnedModel = "observed-return"
	rows[0].InboundPath = "/v1/responses"
	rows[0].UpstreamPath = "/responses"
	rows[0].RequestPreparationMs = &zero
	rows[0].AccountSelectionMs = &ten
	rows[0].ProxySelectionMs = &zero
	rows[0].ErrorTruncated = &no
	rows[0].AttemptsTruncated = &no
	rows[0].ObservationSchemaVersion = 1
	rows[0].UsageSchemaSource = "responses.usage"
	rows[0].Attempts = []GatewayAttemptSummary{{Attempt: 1, Phase: "request", Endpoint: "/responses", StatusCode: &status, DurationMs: &ten, HeaderMs: &zero, ErrorSource: "upstream", ErrorCode: "http_status"}}
	rows[1].SentModel = base.Model
	rows[1].ReturnedModel = base.Model
	rows[1].InboundPath = "/responses"
	rows[1].UpstreamPath = "/responses"
	rows[1].RequestPreparationMs = &twenty
	rows[1].AccountSelectionMs = &zero
	rows[1].ErrorTruncated = &yes
	rows[1].Status = "error"
	rows[1].HTTPCode = 200
	rows[1].Error = "synthetic truncated diagnostic"
	rows[2].RequestPreparationMs = &invalid
	rows[2].ProxySelectionMs = &invalid
	rows[3].SentModel = "observed-send"
	rows[3].RequestPreparationMs = &ten
	rows[4].ReturnedModel = "observed-return"
	rows[4].AccountSelectionMs = &twenty
	return rows
}
func TestManagedObservationQueryFile(t *testing.T) { exerciseManagedObservations(t, webTestSetup) }
func TestPGManagedObservationQuery(t *testing.T)   { exerciseManagedObservations(t, adminPGTestProxy) }
func exerciseManagedObservations(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	ctx := context.Background()
	records := observationFixtureRecords()
	for _, r := range records {
		p.Metrics.Record(r)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		query string
		count int64
	}{
		{"sent_model=observed-send", 2}, {"returned_model=observed-return", 2}, {"inbound_path=%2Fv1%2Fresponses", 1}, {"upstream_path=%2Fresponses", 2},
		{"sent_model_mismatch=true", 2}, {"sent_model_mismatch=false", 1}, {"returned_model_mismatch=true", 1}, {"returned_model_mismatch=false", 1}, {"error_truncated=true", 1}, {"error_truncated=false", 1},
		{"sent_model=unknown", 0}, {"sent_model=observed-send&returned_model=observed-return&error_truncated=false", 1},
	} {
		q := managedQuery(t, item.query+"&exact_total=true")
		page, err := p.Metrics.managedLogs(ctx, q)
		if err != nil || page.Total != item.count {
			t.Fatal(item.query, err, page.Total)
		}
		stats, err := p.Metrics.managedLogStats(ctx, q)
		if err != nil || stats.Requests != item.count {
			t.Fatal("filter stats", item.query, err, stats.Requests)
		}
		var output bytes.Buffer
		count, err := p.Metrics.exportManagedLogs(ctx, q, "jsonl", &output)
		if err != nil || count != item.count {
			t.Fatal("filter export", item.query, err, count)
		}
	}
	for _, query := range []string{"error_truncated=1", "sent_model_mismatch=null", "returned_model_mismatch=TRUE", "sent_model=x&sent_model=y"} {
		managedTestRequest(t, h, "GET", "/api/logs?"+query, nil, 400)
	}
	q := managedQuery(t, "model=observed-request")
	stats, err := p.Metrics.managedLogStats(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RequestPreparationSamples != 3 || stats.AverageRequestPreparation == nil || *stats.AverageRequestPreparation != 10 || stats.AccountSelectionSamples != 3 || stats.AverageAccountSelection == nil || *stats.AverageAccountSelection != 10 || stats.ProxySelectionSamples != 1 || stats.AverageProxySelection == nil || *stats.AverageProxySelection != 0 {
		t.Fatalf("stage sample semantics: %+v", stats.managedUsageTotals)
	}
	for name, rows := range map[string][]managedUsageDimension{"sent": stats.SentModels, "returned": stats.ReturnedModels, "inbound": stats.InboundPaths, "upstream": stats.UpstreamPaths} {
		var total int64
		for _, row := range rows {
			total += row.Requests
			if row.Tokens != row.Requests*110 {
				t.Fatal("new dimension usage missing", name, row)
			}
		}
		want := int64(3)
		if name == "inbound" || name == "upstream" {
			want = 2
		}
		if total != want {
			t.Fatal("dimension fabricated unknown", name, total)
		}
	}
	var output bytes.Buffer
	if _, err := p.Metrics.exportManagedLogsWithPrivacy(ctx, q, "csv", &output, true); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&output).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]int{}
	for i, key := range rows[0] {
		cols[key] = i
	}
	for _, row := range rows[1:] {
		switch row[cols["time"]] {
		case strconv.FormatInt(records[0].Time, 10):
			if row[cols["request_preparation_ms"]] != "0" || row[cols["error_truncated"]] != "false" || row[cols["attempts_truncated"]] != "false" || row[cols["sent_model"]] != "observed-send" || row[cols["observation_schema_version"]] != "1" || !strings.Contains(row[cols["attempts"]], `"status_code":503`) {
				t.Fatal("csv observation mismatch", row)
			}
		case strconv.FormatInt(records[2].Time, 10):
			if row[cols["error_truncated"]] != "" || row[cols["sent_model"]] != "" || row[cols["observation_schema_version"]] != "" {
				t.Fatal("historical unknown made explicit")
			}
		}
	}
	// New selectors are bound to cursor and snapshot signatures.
	first := managedQuery(t, "sent_model=observed-send&page_size=1&exact_total=true")
	page, err := p.Metrics.managedLogs(ctx, first)
	if err != nil || !page.HasMore || page.NextCursor == "" {
		t.Fatal("observation pagination", err)
	}
	first.Cursor = page.NextCursor
	next, err := p.Metrics.managedLogs(ctx, first)
	if err != nil || len(next.Items) != 1 || next.Items[0].EventID == page.Items[0].EventID {
		t.Fatal("observation cursor", err)
	}
	first.ReturnedModel = "observed-return"
	if _, err := p.Metrics.managedLogs(ctx, first); err == nil {
		t.Fatal("cursor ignores new filter")
	}
	response := managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": map[string]string{"sent_model": "observed-send"}}, 200)
	var snapshot struct {
		ID string `json:"as_of"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &snapshot)
	managedTestRequest(t, h, "GET", "/api/logs?sent_model=observed-send&as_of="+url.QueryEscape(snapshot.ID), nil, 200)
	managedTestRequest(t, h, "GET", "/api/logs?sent_model=different&as_of="+url.QueryEscape(snapshot.ID), nil, 409)
	managedTestRequest(t, h, "GET", "/api/logs/observation-0?sent_model=observed-send&as_of="+url.QueryEscape(snapshot.ID), nil, 200)
}
