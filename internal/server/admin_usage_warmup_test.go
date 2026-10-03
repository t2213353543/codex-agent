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

func TestManagedUsageWebSocketWarmup(t *testing.T)   { exerciseManagedWarmup(t, webTestSetup) }
func TestPGManagedUsageWebSocketWarmup(t *testing.T) { exerciseManagedWarmup(t, adminPGTestProxy) }

func exerciseManagedWarmup(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	yes, no := true, false
	rows := []RequestRecord{
		{EventID: "ws-prewarm-1", Time: 1700000000, WebSocketWarmup: &yes},
		{EventID: "ws-prewarm-2", Time: 1700000001, WebSocketWarmup: &yes},
		{EventID: "ws-generation", Time: 1700000002, WebSocketWarmup: &no},
		{EventID: "ws-legacy", Time: 1700000003},
	}
	longID := strings.Repeat("r", 1024)
	for i, row := range rows {
		row.Status = "ok"
		row.RequestType = "websocket"
		if i == 0 {
			row.RequestID = longID
		}
		p.Metrics.Record(row)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter string
		want   int64
		value  *bool
	}{{"", 4, nil}, {"true", 2, &yes}, {"false", 1, &no}} {
		raw := "exact_total=true"
		if test.filter != "" {
			raw += "&websocket_warmup=" + test.filter
		}
		q := managedQuery(t, raw)
		page, err := p.Metrics.managedLogs(context.Background(), q)
		if err != nil || page.Total != test.want {
			t.Fatalf("list %s count=%d err=%v", test.filter, page.Total, err)
		}
		for _, row := range page.Items {
			if test.value != nil && (row.WebSocketWarmup == nil || *row.WebSocketWarmup != *test.value) {
				t.Fatal("unknown was treated as false")
			}
		}
		stats, err := p.Metrics.managedLogStats(context.Background(), q)
		if err != nil || stats.Requests != test.want {
			t.Fatal("stats", stats.Requests, err)
		}
		for _, format := range []string{"jsonl", "csv"} {
			var out bytes.Buffer
			count, err := p.Metrics.exportManagedLogsWithPrivacy(context.Background(), q, format, &out, true)
			if err != nil || count != test.want {
				t.Fatal("export", count, err)
			}
			if format == "csv" && test.filter == "" {
				parsed, err := csv.NewReader(&out).ReadAll()
				if err != nil {
					t.Fatal(err)
				}
				index := -1
				for i, column := range parsed[0] {
					if column == "websocket_warmup" {
						index = i
					}
				}
				if index < 0 {
					t.Fatal("warmup CSV column missing")
				}
				want := []string{"", "false", "true", "true"}
				for i, row := range parsed[1:] {
					if row[index] != want[i] {
						t.Fatal("CSV changed nullable bool", row[index], want[i])
					}
				}
			}
			if format == "jsonl" && test.filter != "" {
				var record map[string]any
				if err := json.NewDecoder(&out).Decode(&record); err != nil || record["websocket_warmup"] != *test.value {
					t.Fatal("JSONL changed bool", record, err)
				}
			}
		}
	}
	for _, raw := range []string{"1", "null", "FALSE"} {
		managedTestRequest(t, h, "GET", "/api/logs?websocket_warmup="+raw, nil, 400)
	}
	q := managedQuery(t, "request_id="+url.QueryEscape(longID))
	page, err := p.Metrics.managedLogs(context.Background(), q)
	if err != nil || len(page.Items) != 1 || page.Items[0].EventID != "ws-prewarm-1" {
		t.Fatal("long ID filtering", err)
	}
	q = managedQuery(t, "websocket_warmup=true&page_size=1")
	first, err := p.Metrics.managedLogs(context.Background(), q)
	if err != nil || !first.HasMore {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	q.WebSocketWarmup = &no
	if _, err := p.Metrics.managedLogs(context.Background(), q); err == nil {
		t.Fatal("cursor accepted changed warmup filter")
	}
	r := managedTestRequest(t, h, "POST", "/api/logs/snapshot", map[string]any{"filters": map[string]string{"websocket_warmup": "true"}}, 200)
	var snap struct {
		ID string `json:"as_of"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, h, "GET", "/api/logs/ws-prewarm-1?websocket_warmup=true&as_of="+url.QueryEscape(snap.ID), nil, 200)
	managedTestRequest(t, h, "GET", "/api/logs?websocket_warmup=false&as_of="+url.QueryEscape(snap.ID), nil, 409)
	if p.Store.DB != nil {
		response := managedTestRequest(t, h, "POST", "/api/logs/cleanup/preview", map[string]any{"filters": map[string]string{"websocket_warmup": "true", "from": "1699999999", "to": "1700000010"}}, 200)
		var preview struct {
			Total int64 `json:"total"`
			Count int64 `json:"count"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
			t.Fatal(err)
		}
		if preview.Total != 2 && preview.Count != 2 {
			t.Fatalf("cleanup filter mismatch: %s", response.Body.String())
		}
	}
}
