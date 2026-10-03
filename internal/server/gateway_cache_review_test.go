package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGatewayCacheReviewWorkloadSkipsCompactBetweenResponseTurns(t *testing.T) {
	const responseOne = `{"model":"fixture-model","instructions":"stable","tools":[{"type":"function","name":"fixture"}],"prompt_cache_key":"fixture-cache","stream":true,"input":[{"type":"message","content":"first"}]}`
	const compactRequest = `{"model":"fixture-model","instructions":"compaction-only","prompt_cache_key":"fixture-cache","input":[{"type":"reasoning","encrypted_content":"opaque-compact-input"}]}`
	const responseTwo = `{"model":"fixture-model","instructions":"stable","tools":[{"type":"function","name":"fixture"}],"prompt_cache_key":"fixture-cache","stream":true,"input":[{"type":"message","content":"first"},{"type":"function_call_output","call_id":"fixture-call","output":"done"}]}`
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":80}}}}\n\n"
	const compactResponse = `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-compact-result"}],"usage":{"input_tokens":999,"output_tokens":222}}`
	steps := []struct{ path, request, response, contentType string }{
		{"/backend-api/codex/responses", responseOne, terminal, "text/event-stream"},
		{"/backend-api/codex/responses/compact", compactRequest, compactResponse, "application/json"},
		{"/backend-api/codex/responses", responseTwo, terminal, "text/event-stream"},
	}
	requests := 0
	diagnostics := cacheDiagnosticsForTest(t)
	transport := &codexWorkloadTransport{cache: diagnostics, statuses: make(map[int]int)}
	transport.base = sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		if requests >= len(steps) {
			t.Fatal("cache diagnostics replayed a request")
		}
		want := steps[requests]
		requests++
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || string(body) != want.request || r.Method != http.MethodPost || r.URL.Path != want.path || r.URL.RawQuery != "fixture=unchanged" || r.Header.Get("Authorization") != "Bearer synthetic-cache-review" || r.Header.Get("X-Codex-Turn-State") != "opaque-fixture-route" {
			t.Fatal("cache observer changed actual request body, route or headers")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {want.contentType}, "X-Request-Id": {"fixture-request"}}, Body: io.NopCloser(strings.NewReader(want.response)), Request: r}, nil
	})
	for _, step := range steps {
		r, err := http.NewRequest(http.MethodPost, "http://loopback-fixture.invalid"+step.path+"?fixture=unchanged", strings.NewReader(step.request))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer synthetic-cache-review")
		r.Header.Set("Session-Id", "fixture-session")
		r.Header.Set("Thread-Id", "fixture-thread")
		r.Header.Set("X-Codex-Turn-State", "opaque-fixture-route")
		response, err := transport.RoundTrip(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil || !bytes.Equal(body, []byte(step.response)) || response.StatusCode != 200 || response.Header.Get("X-Request-Id") != "fixture-request" || response.Header.Get("Content-Type") != step.contentType {
			t.Fatal("cache observer changed actual response bytes, headers or status")
		}
	}
	if requests != 3 || transport.statuses[200] != 3 || transport.errors != 0 {
		t.Fatal("transport counters lost independent compact request")
	}
	summary := diagnostics.Snapshot()
	if summary.Requests != 2 || summary.Responses != 2 || summary.ComparablePairs != 1 || summary.StableConfigurationPairs != 1 || summary.InputHistoryExtensionPairs != 1 || summary.PendingRequests != 0 || summary.UnfinishedResponses != 0 {
		t.Fatalf("compact JSON polluted Responses cache history: requests=%d responses=%d pairs=%d stable=%d extensions=%d unfinished=%d pending=%d", summary.Requests, summary.Responses, summary.ComparablePairs, summary.StableConfigurationPairs, summary.InputHistoryExtensionPairs, summary.UnfinishedResponses, summary.PendingRequests)
	}
	if summary.InputTokens != 200 || summary.OutputTokens != 2 || summary.CachedInputTokens != 160 || summary.CacheHitRatio == nil || *summary.CacheHitRatio != 0.8 || transport.diagnosticErrors != 0 {
		t.Fatal("compact response created a false cache statistic or diagnostic error")
	}
}

func TestGatewayCacheReviewWorkloadSkipsOAuthJSON(t *testing.T) {
	const request = `{"grant_type":"refresh_token","refresh_token":"synthetic-refresh","client_id":"synthetic-client"}`
	const response = `{"access_token":"synthetic-new","refresh_token":"synthetic-rotated","expires_in":3600}`
	diagnostics := cacheDiagnosticsForTest(t)
	requests := 0
	transport := &codexWorkloadTransport{cache: diagnostics, statuses: make(map[int]int), base: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || string(body) != request || r.URL.Path != "/oauth/token" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("diagnostics changed OAuth request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})}
	r, err := http.NewRequest(http.MethodPost, "http://oauth-fixture.invalid/oauth/token", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	closeErr := res.Body.Close()
	if err != nil || closeErr != nil || string(body) != response || requests != 1 || transport.statuses[200] != 1 {
		t.Fatal("observer changed OAuth response or request count")
	}
	s := diagnostics.Snapshot()
	if s.Requests != 0 || s.Responses != 0 || s.PendingRequests != 0 || s.UnfinishedResponses != 0 || s.CacheHitRatio != nil || transport.diagnosticErrors != 0 {
		t.Fatalf("JSON OAuth exchange was treated as a cache-bearing SSE request: requests=%d unfinished=%d errors=%d", s.Requests, s.UnfinishedResponses, transport.diagnosticErrors)
	}
}

func TestGatewayCacheReviewWorkloadResponseRouteAllowlist(t *testing.T) {
	const request = `{"model":"fixture","stream":true,"input":[]}`
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n\n"
	for _, test := range []struct {
		method, path string
		observed     bool
	}{
		{http.MethodPost, "/responses", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/backend-api/codex/responses", true},
		{http.MethodGet, "/responses", false},
		{http.MethodPost, "/unrelated/responses", false},
		{http.MethodPost, "/responses/extra", false},
		{http.MethodPost, "/responses/compact", false},
		{http.MethodPost, "/oauth/token", false},
		{http.MethodPost, "/models", false},
		{http.MethodPost, "/%72esponses", false},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			diagnostics := cacheDiagnosticsForTest(t)
			calls := 0
			transport := &codexWorkloadTransport{cache: diagnostics, statuses: make(map[int]int), base: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || string(body) != request || r.Method != test.method || r.URL.EscapedPath() != test.path {
					t.Fatal("route filtering changed the actual request")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(terminal)), Request: r}, nil
			})}
			r, err := http.NewRequest(test.method, "http://synthetic.invalid"+test.path, strings.NewReader(request))
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if err != nil || closeErr != nil || string(body) != terminal || calls != 1 || transport.statuses[200] != 1 || transport.errors != 0 || transport.diagnosticErrors != 0 {
				t.Fatal("cache route allowlist changed transport bytes or counters")
			}
			summary := diagnostics.Snapshot()
			want := int64(0)
			if test.observed {
				want = 1
			}
			if summary.Requests != want || summary.Responses != want || summary.CacheUsageResponses != want || summary.PendingRequests != 0 || summary.UnfinishedResponses != 0 {
				t.Fatal("cache diagnostics observed an unrelated route or omitted a supported Responses route")
			}
		})
	}
}
