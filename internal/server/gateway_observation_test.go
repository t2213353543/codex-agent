package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayObservationWaitsForActualOutput(t *testing.T) {
	started := time.Unix(1000, 0)
	rec := RequestRecord{}
	observer := gatewayObservation{record: &rec, started: started}
	observeGatewayHeaders(&rec, http.Header{"X-Request-Id": {"req-handshake"}, "Openai-Model": {"header-model"}})
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp-created","model":"actual-model","usage":{"output_tokens":999}}}`,
		`{"type":"response.in_progress"}`,
		`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"shell","arguments":""}}`,
		`{"type":"response.output_text.delta","delta":""}`,
		`{"type":"response.completed","response":{"output":[],"usage":{}}}`,
	} {
		observer.eventAt([]byte(event), started.Add(15*time.Millisecond))
		if rec.TTFTMs != nil || rec.UsageKnown != nil {
			t.Fatalf("metadata was counted as output or terminal usage before outcome processing: %s", event)
		}
	}
	observer.eventAt([]byte(`{"type":"response.function_call_arguments.delta","delta":"{"}`), started.Add(75*time.Millisecond))
	observer.eventAt([]byte(`{"type":"response.output_text.delta","delta":"later"}`), started.Add(150*time.Millisecond))
	if rec.TTFTMs == nil || *rec.TTFTMs != 75 || rec.RequestID != "req-handshake" || rec.ResponseID != "resp-created" || rec.UpstreamModel != "actual-model" {
		t.Fatalf("observations do not describe first actual output: %+v", rec)
	}
}

func TestGatewayObservationRecognizesOutputEventsWithoutRetainingContent(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","delta":"private-output"}`,
		`{"type":"response.reasoning_text.delta","delta":"private-output"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"private-output"}`,
		`{"type":"response.custom_tool_call_input.delta","delta":"private-output"}`,
		`{"type":"response.function_call_arguments.done","arguments":"private-output"}`,
		`{"type":"response.content_part.done","part":{"type":"output_text","text":"private-output"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","arguments":"private-output"}}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"private-output"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"private-output"}]}]}}`,
	} {
		var rec RequestRecord
		started := time.Now()
		(gatewayObservation{record: &rec, started: started}).eventAt([]byte(payload), started)
		if rec.TTFTMs == nil || *rec.TTFTMs != 0 {
			t.Fatalf("actual zero-ms output was not observed: %s", payload)
		}
		raw, err := json.Marshal(rec)
		if err != nil || strings.Contains(string(raw), "private-output") {
			t.Fatal("an observation retained response content")
		}
	}
}

func TestGatewayObservationUsageDistinguishesUnknownAndZero(t *testing.T) {
	for _, test := range []struct {
		name, response string
		known          bool
		reasoning      *int64
		cacheWrite     *int64
	}{
		{name: "missing", response: `{}`},
		{name: "null", response: `{"usage":null}`},
		{name: "array", response: `{"usage":[]}`},
		{name: "string", response: `{"usage":"unknown"}`},
		{name: "empty object", response: `{"usage":{}}`, known: true},
		{name: "explicit zero", response: `{"usage":{"output_tokens_details":{"reasoning_tokens":0},"input_tokens_details":{"cache_write_tokens":0}}}`, known: true, reasoning: new(int64), cacheWrite: new(int64)},
		{name: "native details", response: `{"usage":{"output_tokens_details":{"reasoning_tokens":7},"input_tokens_details":{"cache_write_tokens":11}}}`, known: true, reasoning: observedTestInt64(7), cacheWrite: observedTestInt64(11)},
		{name: "null details", response: `{"usage":{"output_tokens_details":{"reasoning_tokens":null},"input_tokens_details":{"cache_write_tokens":null}}}`, known: true},
		{name: "invalid details", response: `{"usage":{"output_tokens_details":{"reasoning_tokens":-1},"input_tokens_details":{"cache_write_tokens":"11"}}}`, known: true},
		{name: "nested absent usage", response: `{"usage":{"input_tokens":123},"response":{"id":"nested"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := RequestRecord{}
			observeGatewayResponse(&rec, []byte(test.response))
			if test.known != (rec.UsageKnown != nil && *rec.UsageKnown) || (!test.known && rec.UsageKnown != nil) {
				t.Fatalf("usage-known is not based on a real usage object: %+v", rec)
			}
			assertObservedCount(t, "reasoning", rec.ReasoningTokens, test.reasoning)
			assertObservedCount(t, "cache write", rec.CacheWriteTokens, test.cacheWrite)
		})
	}
}

func observedTestInt64(value int64) *int64 { return &value }

func assertObservedCount(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Fatalf("%s optional count mismatch: got=%v want=%v", name, got, want)
	}
}

func TestGatewayObservationHTTPFirstOutputFollowsHeadersAndCreated(t *testing.T) {
	const response = `{"id":"resp-observed","model":"actual-model","service_tier":"default","reasoning":{"effort":"medium"},"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"do-not-log-output"}]}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":12},"output_tokens_details":{"reasoning_tokens":7}}}`
	created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-observed\",\"model\":\"actual-model\"}}\n\n"
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"do-not-log-output\"}\n\n"
	completed := "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "req-observed")
				w.Header().Set("Openai-Model", "header-model")
				_, _ = io.WriteString(w, created)
				w.(http.Flusher).Flush()
				time.Sleep(90 * time.Millisecond)
				_, _ = io.WriteString(w, delta+completed)
			})
			body := fmt.Sprintf(`{"model":"requested-model","input":"do-not-log-prompt","service_tier":"priority","reasoning":{"effort":"high"},"stream":%t}`, stream)
			r := proxyRequest(http.MethodPost, "/v1/responses", body)
			r.Header.Set("User-Agent", "codex_cli_rs/test-observation")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			if stream && w.Body.String() != created+delta+completed {
				t.Fatal("passive observations changed streaming bytes")
			}
			if !stream && strings.TrimSpace(w.Body.String()) != response {
				t.Fatal("passive observations changed JSON aggregation")
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.TTFTMs == nil || *rec.TTFTMs < 75 {
				t.Fatalf("TTFT measured headers/created rather than delayed output: %+v", rec)
			}
			wantType := "http_json"
			if stream {
				wantType = "http_sse"
			}
			if rec.RequestType != wantType || rec.UserAgent != "codex_cli_rs/test-observation" || rec.RequestID != "req-observed" || rec.ResponseID != "resp-observed" || rec.Model != "requested-model" || rec.UpstreamModel != "actual-model" || rec.ServiceTier != "default" || rec.ReasoningEffort != "medium" {
				t.Fatalf("request/response metadata missing: %+v", rec)
			}
			if rec.UsageKnown == nil || !*rec.UsageKnown || rec.InputTokens != 100 || rec.OutputTokens != 20 || rec.CachedTokens != 80 {
				t.Fatalf("observed usage changed existing totals: %+v", rec)
			}
			assertObservedCount(t, "reasoning", rec.ReasoningTokens, observedTestInt64(7))
			assertObservedCount(t, "cache write", rec.CacheWriteTokens, observedTestInt64(12))
			encoded, _ := json.Marshal(rec)
			if strings.Contains(string(encoded), "do-not-log") {
				t.Fatal("request log captured prompt/output content")
			}
		})
	}
}

func TestGatewayObservationModelsAndCompactKeepUnknownTTFT(t *testing.T) {
	for _, test := range []struct {
		path, requestType string
		status            int
		body              string
		known             bool
	}{
		{"models", "models", 200, `{"models":[]}`, false},
		{"models", "models", 304, ``, false},
		{"models", "models", 429, `{"error":{"message":"try later"}}`, false},
		{"responses/compact", "compact", 200, `{"id":"compact-result","model":"actual-compact","output":[],"usage":{"input_tokens":10,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}}`, true},
		{"responses/compact", "compact", 503, `{"error":{"message":"unavailable"}}`, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", test.path, test.status), func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("X-Oai-Request-Id", "request-upstream")
				w.Header().Set("X-Openai-Model", "header-model")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			method, body := http.MethodPost, `{"model":"requested","input":[],"service_tier":"auto","reasoning":{"effort":"high"}}`
			if test.requestType == "models" {
				method, body = http.MethodGet, ""
			}
			r := proxyRequest(method, "/v1/"+test.path, body)
			r.Header.Set("User-Agent", "observation-models")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != test.status || strings.TrimSpace(w.Body.String()) != test.body {
				t.Fatalf("observation changed forwarded response: HTTP %d %s", w.Code, w.Body.String())
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.RequestType != test.requestType || rec.RequestID != "request-upstream" || rec.UserAgent != "observation-models" || rec.TTFTMs != nil || (rec.UsageKnown != nil && *rec.UsageKnown) != test.known {
				t.Fatalf("unexpected non-stream metadata: %+v", rec)
			}
			if test.known && (rec.ResponseID != "compact-result" || rec.UpstreamModel != "actual-compact" || rec.ReasoningTokens == nil || *rec.ReasoningTokens != 0) {
				t.Fatalf("compact metadata lost explicit measured zero: %+v", rec)
			}
		})
	}
}

func TestGatewayObservationFailedResponseWithoutOutputHasNoTTFT(t *testing.T) {
	wire := "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"failed-response\",\"status\":\"failed\",\"error\":{\"message\":\"rejected\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n"
	for _, stream := range []bool{true, false} {
		p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, wire) })
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"test","stream":%t}`, stream)))
		rec, ok := p.Metrics.LastFor("test")
		if !ok || rec.Status != "error" || rec.ResponseID != "failed-response" || rec.TTFTMs != nil || rec.UsageKnown == nil || !*rec.UsageKnown || rec.ReasoningTokens == nil || *rec.ReasoningTokens != 0 {
			t.Fatalf("failed response observations invented output or lost usage: %+v", rec)
		}
	}
}

func TestGatewayObservationWebSocketPerTurnMetadataAndTTFT(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "ws-handshake-request")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for turn := range 2 {
			if _, _, err := conn.Read(r.Context()); err != nil {
				t.Error(err)
				return
			}
			id := fmt.Sprintf("ws-observed-%d", turn)
			_ = gatewayWSWrite(r.Context(), conn, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)))
			if turn == 0 {
				time.Sleep(90 * time.Millisecond)
				_ = gatewayWSWrite(r.Context(), conn, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","response_id":%q,"delta":"do-not-log-ws"}`, id)))
			}
			_ = gatewayWSWrite(r.Context(), conn, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","model":"ws-actual","usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":1}}}}`, id)))
		}
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(upstream.Close)
	p := newTestProxy(t, 2)
	p.Client, p.BaseURL = upstream.Client(), upstream.URL
	t.Cleanup(p.closeGatewayClients)
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer test-client-key"}, "Session-Id": {"observation-ws-session"}, "User-Agent": {"ws-observed-client"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for turn := range 2 {
		if err := gatewayWSWrite(ctx, conn, websocket.MessageText, []byte(`{"type":"response.create","model":"requested","input":[],"service_tier":"priority","reasoning":{"effort":"high"}}`)); err != nil {
			t.Fatal(err)
		}
		for range map[bool]int{true: 3, false: 2}[turn == 0] {
			_ = gatewayWSRead(t, conn)
		}
		waitGatewayWSReleased(t, p)
		rec, ok := p.Metrics.LastFor("test")
		if !ok || rec.ResponseID != fmt.Sprintf("ws-observed-%d", turn) || rec.RequestID != "ws-handshake-request" || rec.UserAgent != "ws-observed-client" || rec.RequestType != "websocket" || rec.UpstreamModel != "ws-actual" || rec.ServiceTier != "priority" || rec.ReasoningEffort != "high" || rec.UsageKnown == nil || !*rec.UsageKnown {
			t.Fatalf("WebSocket turn metadata missing: %+v", rec)
		}
		if (turn == 0 && (rec.TTFTMs == nil || *rec.TTFTMs < 75)) || (turn == 1 && rec.TTFTMs != nil) {
			t.Fatalf("WebSocket TTFT included handshake or leaked across turns: %+v", rec)
		}
		assertObservedCount(t, "cache write", rec.CacheWriteTokens, new(int64))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}
