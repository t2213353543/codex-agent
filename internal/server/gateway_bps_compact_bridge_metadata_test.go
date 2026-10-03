package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSCompactBridgeRejectsMissingStateWithoutLosingUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			posts := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				posts++
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"comp_missing_state","object":"response.compaction","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}}`)
			})
			body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"role":"user","content":"synthetic"},{"type":"compaction_trigger"}]}`, stream)
			out := httptest.NewRecorder()
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				p.ServeHTTP(out, proxyRequest(http.MethodPost, "/v1/responses", body))
			}()
			if panicked != nil && panicked != http.ErrAbortHandler {
				t.Fatalf("unexpected panic: %v", panicked)
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != "error" || posts != 1 || strings.Contains(out.Body.String(), "response.completed") {
				t.Fatalf("invalid compacted state escaped completion barrier: status=%s posts=%d body=%s", rec.Status, posts, out.Body)
			}
			if !stream && out.Code != 502 {
				t.Errorf("buffered JSON changed local error status: %d", out.Code)
			}
			if rec.InputTokens != 100 || rec.OutputTokens != 10 || rec.CachedTokens != 80 || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 7 || rec.ReturnedModel != "gpt-6-astra" || rec.TTFTMs != nil {
				t.Fatalf("known measured usage lost or false TTFT: %+v", rec)
			}
			if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "comp_missing_state"); err == nil {
				t.Fatal("unusable compaction saved as continuable history")
			}
			t.Logf("stream=%t error=true actual_generation_posts=%d returned_model=%s input=%d read=%d write=%d no_history=true no_completed=true", stream, posts, rec.ReturnedModel, rec.InputTokens, rec.CachedTokens, *rec.CacheWriteTokens)
		})
	}
}
