package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSTerminalUsagePersistsForStreamAndJSON(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"OK"}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"bps-real-usage-fixture","model":"returned-model","status":"completed","output":[],"usage":{"input_tokens":8192,"output_tokens":17,"input_tokens_details":{"cached_tokens":7168},"output_tokens_details":{"reasoning_tokens":10},"total_tokens":8209}}}`+"\n\n")
			})
			out := httptest.NewRecorder()
			p.ServeHTTP(out, proxyRequest("POST", "/v1/responses", fmt.Sprintf(`{"model":"m","stream":%v,"input":"hello"}`, stream)))
			if out.Code != 200 {
				t.Fatalf("status=%d", out.Code)
			}
			record, ok := p.Metrics.LastFor("test")
			if !ok || record.InputTokens != 8192 || record.OutputTokens != 17 || record.CachedTokens != 7168 || record.UsageKnown == nil || !*record.UsageKnown {
				t.Fatalf("measured BPS usage lost: input=%d output=%d cache=%d known=%v", record.InputTokens, record.OutputTokens, record.CachedTokens, record.UsageKnown)
			}
			if record.ReturnedModel != "returned-model" || record.TTFTMs == nil || record.ReasoningTokens == nil || *record.ReasoningTokens != 10 {
				t.Fatal("BPS terminal model/timing/details missing")
			}
			if record.CachedTokensKnown == nil || !*record.CachedTokensKnown {
				t.Fatal("measured cache usage was left unknown")
			}
		})
	}
}

func TestBPSTerminalCacheMissingDiffersFromMeasuredZero(t *testing.T) {
	for _, test := range []struct {
		usage  string
		known  bool
		cached int64
	}{
		{`{"input_tokens":10,"output_tokens":1}`, false, 0},
		{`{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":8}}`, true, 0},
		{`{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":9}}`, true, 9},
		{`{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":null}}`, false, 0},
	} {
		var record RequestRecord
		recordBPSTerminalUsage(&record, []byte(`{"id":"fixture","status":"completed","output":[],"usage":`+test.usage+`}`))
		if record.CachedTokensKnown == nil || *record.CachedTokensKnown != test.known || record.CachedTokens != test.cached {
			t.Fatalf("cache knowledge wrong: %s known=%v cached=%d", test.usage, record.CachedTokensKnown, record.CachedTokens)
		}
		if strings.Contains(test.usage, "cache_write_tokens") && (record.CacheWriteTokens == nil || *record.CacheWriteTokens != 8) {
			t.Fatal("actual cache write tokens lost")
		}
	}
}
