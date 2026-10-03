package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSSEScannerExtractsUsage(t *testing.T) {
	scanner := sseScanner{rec: &RequestRecord{}}
	chunks := []string{
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":80},\"output_tokens\":45}}}\n\n",
		"data: [DONE]\n\n",
	}
	for _, chunk := range chunks {
		if _, err := scanner.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	rec := scanner.rec
	if !scanner.finished || rec.InputTokens != 120 || rec.CachedTokens != 80 || rec.OutputTokens != 45 {
		t.Fatalf("usage not extracted: %+v", rec)
	}
}

func TestSSEScannerErrorEvent(t *testing.T) {
	scanner := sseScanner{rec: &RequestRecord{}}
	if _, err := scanner.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}}\n\n")); err != nil {
		t.Fatal(err)
	}
	if scanner.finished || scanner.rec.Status != "" {
		t.Fatal("HTTP failed event finalized before EOF")
	}
	scanner.Finish()
	rec := scanner.rec
	if !scanner.finished || rec.Status != "error" || !strings.Contains(rec.Error, "slow down") {
		t.Fatalf("failed event not captured: %+v", rec)
	}
}

func TestUsageFromResponse(t *testing.T) {
	payload := `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}},"model":"gpt-4o"}`
	input, output, cached, ok := usageFromResponse([]byte(payload))
	if !ok || input != 10 || output != 5 || cached != 4 {
		t.Fatalf("wrong usage: %d %d %d ok=%v", input, output, cached, ok)
	}
	if _, _, _, ok := usageFromResponse([]byte(`{}`)); ok {
		t.Fatal("payload without usage reported success")
	}
}

func TestErrorFromPayload(t *testing.T) {
	if err := errorFromPayload([]byte(`{"error":{"message":"boom","code":"x"}}`)); err == "" || !strings.Contains(err, "boom") {
		t.Fatalf("error not extracted: %q", err)
	}
	if errorFromPayload([]byte(`{"ok":true}`)) != "" {
		t.Fatal("clean payload reported an error")
	}
}

func TestMetricsBucketsAndSeries(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	base := time.Now().Unix()
	base = base / 60 * 60
	m.Record(RequestRecord{Time: base, Account: "a", KeyID: "k1", Status: "ok", InputTokens: 100, OutputTokens: 20, CachedTokens: 30, LatencyMs: 200})
	m.Record(RequestRecord{Time: base + 30, Account: "a", KeyID: "k1", Status: "ok", InputTokens: 10, OutputTokens: 5, CachedTokens: 0, LatencyMs: 400})
	m.Record(RequestRecord{Time: base + 90, Account: "a", KeyID: "k2", Status: "error", Error: "boom"})
	totals := m.Totals(base, base+120)
	if totals.Requests != 3 || totals.OK != 2 || totals.Errors != 1 || totals.Tokens != 135 || totals.Input != 110 || totals.Output != 25 || totals.Cached != 30 {
		t.Fatalf("wrong totals: %+v", totals)
	}
	if totals.LatencyN != 2 || totals.Latency != 600 {
		t.Fatalf("wrong latency aggregate: %+v", totals)
	}
	window := m.AccountWindow("a", base-86400)
	if window.Requests != 3 || window.Tokens != 135 {
		t.Fatalf("wrong account window: %+v", window)
	}
	usage := m.KeyUsage("k1")
	if usage.Requests != 2 || usage.Tokens != 135 {
		t.Fatalf("wrong key usage: %+v", usage)
	}
	recent := m.RecentForAccount("a", 10)
	if len(recent) != 3 {
		t.Fatalf("wrong recent ring size: %d", len(recent))
	}
	series := m.Series(base, base+120, 60)
	if len(series) != 3 || series[0].Requests != 2 || series[1].Requests != 1 || series[2].Requests != 0 {
		t.Fatalf("wrong series: %+v", series)
	}
	grand := m.GrandTotals()
	if grand.Requests != 3 {
		t.Fatalf("wrong grand totals: %+v", grand)
	}
}

func TestReadLinesBackward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.jsonl")
	var builder strings.Builder
	for i := 0; i < 5000; i++ {
		builder.WriteString("line\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0600); err != nil {
		t.Fatal(err)
	}
	lines, err := readLinesBackward(path, 100)
	if err != nil || len(lines) != 100 {
		t.Fatalf("expected 100 lines, got %d (%v)", len(lines), err)
	}
	lines, err = readLinesBackward(path, 10000)
	if err != nil || len(lines) < 5000 || len(lines) > 5001 {
		t.Fatalf("expected ~5000 lines, got %d (%v)", len(lines), err)
	}
	lines, err = readLinesBackward(filepath.Join(t.TempDir(), "missing"), 10)
	if err != nil || lines != nil {
		t.Fatalf("missing file should be empty: %v %v", lines, err)
	}
}
