package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPerformanceComplexFilters(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("explicit synthetic SLO fixture required")
	}
	ctx := context.Background()
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.User == nil || u.User.Username() != "codex_agent_test" || u.Path != "/codex_agent_integration" || u.Port() != "25432" || !stringIn(u.Hostname(), "127.0.0.1", "localhost", "::1") || !regexp.MustCompile(`^perf_slo_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("isolated burst schema required")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)), "slo-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows, anchor int64
	var ready bool
	if err = db.Pool.QueryRow(ctx, "SELECT rows,anchor,ready FROM performance_slo_fixture").Scan(&rows, &anchor, &ready); err != nil || !ready {
		t.Fatal("fixture not ready", err)
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Keys: keys, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey}
	server := httptest.NewServer(p.adminRoutes())
	defer server.Close()
	client := &http.Client{Timeout: 40 * time.Second}
	defer client.CloseIdleConnections()
	interval := fmt.Sprintf("from=%d&to=%d&step=86400&timezone=UTC", anchor-30*86400, anchor+60)
	type spec struct {
		name, path, method, kind string
		body                     any
		want                     int64
	}
	cases := []spec{
		{"stats_model_all", "/api/logs/stats?" + interval + "&model=synthetic-model", "GET", "stats", nil, rows},
		{"stats_model_missing", "/api/logs/stats?" + interval + "&model=missing-synthetic", "GET", "stats", nil, 0},
		{"stats_account", "/api/logs/stats?" + interval + "&account_id=slo_a_1", "GET", "stats", nil, rows / 1000},
		{"stats_key", "/api/logs/stats?" + interval + "&key_id=slo_key", "GET", "stats", nil, rows},
		{"stats_group_missing", "/api/logs/stats?" + interval + "&group_id=g_1", "GET", "stats", nil, 0},
		{"stats_error_model", "/api/logs/stats?" + interval + "&status=error&model=synthetic-model", "GET", "stats", nil, rows / 10},
		{"stats_model_key_all", "/api/logs/stats?" + interval + "&model=synthetic-model&key_id=slo_key", "GET", "stats", nil, rows},
		{"stats_endpoint_missing", "/api/logs/stats?" + interval + "&endpoint=responses", "GET", "stats", nil, 0},
		{"logs_model_all", "/api/logs?size=50&model=synthetic-model", "GET", "items", nil, 50},
		{"logs_model_missing", "/api/logs?size=50&model=missing-synthetic", "GET", "items", nil, 0},
		{"logs_ip_missing", "/api/logs?size=50&ip=192.0.2.123", "GET", "items", nil, 0},
		{"logs_endpoint_missing", "/api/logs?size=50&endpoint=responses", "GET", "items", nil, 0},
		{"logs_key_missing", "/api/logs?size=50&key_id=missing-synthetic", "GET", "items", nil, 0},
		{"logs_model_error_tokens", "/api/logs?size=50&model=synthetic-model&status=error&sort_by=tokens", "GET", "items", nil, 50},
		{"logs_sort_model", "/api/logs?size=50&sort_by=model", "GET", "items", nil, 50},
		{"logs_sort_input", "/api/logs?size=50&sort_by=input_tokens", "GET", "items", nil, 50},
		{"logs_sort_output", "/api/logs?size=50&sort_by=output_tokens", "GET", "items", nil, 50},
		{"logs_sort_cached", "/api/logs?size=50&sort_by=cached_tokens", "GET", "items", nil, 50},
	}
	for _, count := range []int{10, 100} {
		ids := make([]string, count)
		for i := range ids {
			ids[i] = fmt.Sprintf("slo_a_%d", i+1)
		}
		cases = append(cases, spec{fmt.Sprintf("usage_batch_%d", count), "/api/accounts/usage-batch", "POST", "batch", map[string]any{"ids": ids, "from": anchor - 30*86400, "to": anchor + 60}, int64(count)})
	}
	selected := os.Getenv("PERFORMANCE_SLO_CASES")
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 2, 1, 20)
	threshold := 1000.0
	if rows >= 10000000 {
		threshold = 3000
	}
	performanceSLOEmit(t, "FILTER_FIXTURE", map[string]any{"rows": rows, "schema": schema, "source_distribution": "burst fixture; model/key common, groups/IP/endpoint absent from historical records", "sampling": "serial only; stop case on first failed/slow sample"})
	for _, c := range cases {
		if selected != "" && !strings.Contains(","+selected+",", ","+c.name+",") {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			for index := 0; index < samples; index++ {
				body, _ := json.Marshal(c.body)
				request, _ := http.NewRequest(c.method, server.URL+c.path, bytes.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+webTestKey)
				request.Header.Set("Content-Type", "application/json")
				started := time.Now()
				response, err := client.Do(request)
				status, size := 0, 0
				failure := ""
				var data map[string]any
				if err != nil {
					failure = err.Error()
				} else {
					status = response.StatusCode
					raw, e := io.ReadAll(response.Body)
					response.Body.Close()
					size = len(raw)
					if e != nil || status != 200 || json.Unmarshal(raw, &data) != nil {
						failure = "response_failure"
					}
				}
				ms := float64(time.Since(started).Microseconds()) / 1000
				if failure == "" {
					switch c.kind {
					case "stats":
						if data["total_requests"] != float64(c.want) {
							failure = "incorrect_total"
						}
					case "items", "batch":
						items, _ := data["items"].([]any)
						if len(items) != int(c.want) {
							failure = "incorrect_items"
						}
						if c.kind == "batch" {
							for _, item := range items {
								row, _ := item.(map[string]any)
								if row["total_requests"] != float64(rows/1000) {
									failure = "incorrect_batch_account_total"
								}
							}
						}
					}
				}
				performanceSLOEmit(t, "FILTER_SAMPLE", map[string]any{"name": c.name, "method": c.method, "path": c.path, "index": index, "ms": ms, "status": status, "bytes": size, "error": failure, "threshold_ms": threshold, "slo_pass": failure == "" && ms < threshold})
				if failure != "" || ms >= threshold {
					t.Errorf("sample%d status%d latency%.3fms %s", index, status, ms, failure)
					return
				}
			}
		})
	}
}
