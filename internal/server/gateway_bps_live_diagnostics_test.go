package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBPSLiveDiagnosticsIsolatesCaseAndOmitsPrivatePayload(t *testing.T) {
	p := newTestProxy(t, 1)
	p.Metrics.Record(RequestRecord{Time: time.Now().Unix(), Account: "test", Status: "error", Error: "old-case"})
	recorder := &bpsLiveRecorder{p: p}
	recorder.begin("fixture")
	h := &bpsLiveHarness{recorder: recorder}
	status := 429
	p.Metrics.Record(RequestRecord{Time: time.Now().Unix(), Account: "test", Status: "error", HTTPCode: 200,
		TerminalStatus: "failed", ErrorSource: "upstream", ErrorPhase: "response_event",
		Error:    "bps response terminated with error (status unreported) (code=rate_limit_exceeded)",
		Attempts: []GatewayAttemptSummary{{Attempt: 1, StatusCode: &status, ErrorCode: "rateLimitExceeded"}}})
	p.Metrics.Record(RequestRecord{Time: time.Now().Unix(), Account: "test", Status: "error",
		Error: "<html>Bearer sk-private-test-token user prompt private-prompt-text</html>"})
	rows := h.diagnosticMetrics()
	encoded, err := json.Marshal(rows)
	if err != nil || len(rows) != 2 || !strings.Contains(string(encoded), "rate_limit_exceeded") || !strings.Contains(string(encoded), "429") {
		t.Fatalf("request diagnostic lost observed failure: %s %v", encoded, err)
	}
	for _, private := range []string{"old-case", "<html>", "Bearer", "sk-private", "private-prompt-text"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private or stale diagnostic leaked: %s", private)
		}
	}
}
