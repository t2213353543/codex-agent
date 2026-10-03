package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayPassiveImageCounts(t *testing.T) {
	for _, test := range []struct {
		name, payload             string
		input, output             *int64
		inputSource, outputSource string
	}{
		{"missing", `{"usage":{"input_tokens":4},"input":[{"type":"input_image","image_url":"https://private.invalid/image"}],"output":[{"type":"image_generation_call","result":"PRIVATE_BASE64"}]}`, nil, nil, "", ""},
		{"direct", `{"usage":{"input_tokens_details":{"image_tokens":12},"output_tokens_details":{"image_tokens":0}}}`, observedTestInt64(12), observedTestInt64(0), "usage.input_tokens_details.image_tokens", "usage.output_tokens_details.image_tokens"},
		{"nested", `{"response":{"usage":{"input_tokens_details":{"image_tokens":0},"output_tokens_details":{"image_tokens":3}}}}`, observedTestInt64(0), observedTestInt64(3), "usage.input_tokens_details.image_tokens", "usage.output_tokens_details.image_tokens"},
		{"compatible", `{"usage":{"prompt_tokens_details":{"image_tokens":2},"completion_tokens_details":{"image_tokens":4}}}`, observedTestInt64(2), observedTestInt64(4), "usage.prompt_tokens_details.image_tokens", "usage.completion_tokens_details.image_tokens"},
		{"tool_only", `{"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":8},"output_tokens_details":{"image_tokens":0},"url":"https://private.invalid/image","b64_json":"PRIVATE_BASE64"}}}`, observedTestInt64(8), observedTestInt64(0), "tool_usage.image_gen.input_tokens_details.image_tokens", "tool_usage.image_gen.output_tokens_details.image_tokens"},
		{"zero_priority", `{"usage":{"input_tokens_details":{"image_tokens":0},"prompt_tokens_details":{"image_tokens":5},"output_tokens_details":{"image_tokens":0}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":9},"output_tokens_details":{"image_tokens":20}}}}`, observedTestInt64(0), observedTestInt64(0), "usage.input_tokens_details.image_tokens", "usage.output_tokens_details.image_tokens"},
		{"invalid_primary_valid_fallback", `{"usage":{"input_tokens_details":{"image_tokens":null},"prompt_tokens_details":{"image_tokens":7},"output_tokens_details":{"image_tokens":-1}},"tool_usage":{"image_gen":{"output_tokens_details":{"image_tokens":5}}}}`, observedTestInt64(7), observedTestInt64(5), "usage.prompt_tokens_details.image_tokens", "tool_usage.image_gen.output_tokens_details.image_tokens"},
		{"max_int64", `{"usage":{"input_tokens_details":{"image_tokens":9223372036854775807}}}`, observedTestInt64(9223372036854775807), nil, "usage.input_tokens_details.image_tokens", ""},
		{"malformed_objects", `{"usage":{"input_tokens_details":[{"image_tokens":4}],"output_tokens_details":7},"tool_usage":{"image_gen":[]}}`, nil, nil, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var record RequestRecord
			observeGatewayResponse(&record, []byte(test.payload))
			assertObservedCount(t, "input image", record.ImageInputTokens, test.input)
			assertObservedCount(t, "output image", record.ImageOutputTokens, test.output)
			if record.ImageInputTokensSource != test.inputSource || record.ImageOutputTokensSource != test.outputSource {
				t.Fatal("image source mismatch", record.ImageInputTokensSource, record.ImageOutputTokensSource)
			}
			if record.InputTokens != 0 || record.OutputTokens != 0 || record.CachedTokens != 0 {
				t.Fatal("image observer changed base counters")
			}
			if test.name == "tool_only" && record.UsageKnown != nil {
				t.Fatal("tool-only observation fabricated base usage")
			}
			raw, _ := json.Marshal(record)
			if strings.Contains(string(raw), "PRIVATE_BASE64") || strings.Contains(string(raw), "private.invalid") {
				t.Fatal("image content retained")
			}
		})
	}
	for _, value := range []string{"null", "-1", "1.5", "1e3", "\"7\"", "true", "{}", "[]", "9223372036854775808"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			var record RequestRecord
			observeGatewayResponse(&record, []byte(`{"usage":{"input_tokens_details":{"image_tokens":`+value+`},"output_tokens_details":{"image_tokens":`+value+`}}}`))
			if record.ImageInputTokens != nil || record.ImageOutputTokens != nil || record.ImageInputTokensSource != "" || record.ImageOutputTokensSource != "" {
				t.Fatal("invalid explicit integer retained")
			}
		})
	}
	var repeated RequestRecord
	observeGatewayResponse(&repeated, []byte(`{"usage":{"input_tokens_details":{"image_tokens":0}}}`))
	for i := 0; i < 2; i++ {
		observeGatewayResponse(&repeated, []byte(`{"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":100},"output_tokens_details":{"image_tokens":7}}}}`))
	}
	assertObservedCount(t, "explicit zero survives lower priority", repeated.ImageInputTokens, observedTestInt64(0))
	assertObservedCount(t, "repeated terminal not summed", repeated.ImageOutputTokens, observedTestInt64(7))
}

func TestGatewayPassiveImageHTTPWire(t *testing.T) {
	const response = `{"id":"passive-image-response","status":"completed","model":"synthetic-image","output":[],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":5,"image_tokens":0},"output_tokens_details":{"image_tokens":7}}}`
	const sse = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	for _, test := range []struct {
		name, path, body, wire string
		stream                 bool
	}{
		{"sse", "/v1/responses", `{"model":"synthetic-image","stream":true,"input":"PRIVATE_PROMPT"}`, sse, true},
		{"collected_json", "/v1/responses", `{"model":"synthetic-image","stream":false,"input":"PRIVATE_PROMPT"}`, sse, false},
		{"compact_json", "/v1/responses/compact", `{"model":"synthetic-image","input":[]}`, response, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, test.wire)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", test.path, test.body))
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatal("observer changed dispatch", w.Code, calls.Load())
			}
			if test.stream && w.Body.String() != test.wire || !test.stream && strings.TrimSpace(w.Body.String()) != response {
				t.Fatal("observer changed response bytes", w.Body.String())
			}
			record, ok := p.Metrics.LastFor("test")
			if !ok {
				t.Fatal("request was not recorded")
			}
			assertObservedCount(t, "wire image input", record.ImageInputTokens, observedTestInt64(0))
			assertObservedCount(t, "wire image output", record.ImageOutputTokens, observedTestInt64(7))
			if record.InputTokens != 100 || record.OutputTokens != 20 || record.CachedTokens != 5 || record.tokens() != 120 {
				t.Fatal("image token duplicated in base totals")
			}
			raw, _ := json.Marshal(record)
			if strings.Contains(string(raw), "PRIVATE_PROMPT") {
				t.Fatal("prompt retained")
			}
		})
	}
}

func TestGatewayPassiveImageWebSocketIsolation(t *testing.T) {
	var requests atomic.Int32
	wires := []string{
		`{"type":"response.completed","response":{"id":"image-turn-one","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":4},"output_tokens_details":{"image_tokens":0}}}}}`,
		`{"type":"response.completed","response":{"id":"image-turn-two","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2}}}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for _, wire := range wires {
			if _, _, err := conn.Read(r.Context()); err != nil {
				t.Error(err)
				return
			}
			requests.Add(1)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(wire)); err != nil {
				t.Error(err)
				return
			}
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
	conn, _, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"passive-image-isolation"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for i, wire := range wires {
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"synthetic-image","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		_, got, err := conn.Read(ctx)
		if err != nil || string(got) != wire {
			t.Fatal("WebSocket bytes changed", err)
		}
		waitGatewayWSReleased(t, p)
		record, ok := p.Metrics.LastFor("test")
		if !ok {
			t.Fatal("missing WS record")
		}
		if i == 0 {
			assertObservedCount(t, "ws image input", record.ImageInputTokens, observedTestInt64(4))
			assertObservedCount(t, "ws image zero", record.ImageOutputTokens, observedTestInt64(0))
		} else if record.ImageInputTokens != nil || record.ImageOutputTokens != nil || record.ImageInputTokensSource != "" || record.ImageOutputTokensSource != "" {
			t.Fatal("image observations leaked to later turn")
		}
		if record.tokens() != 12 {
			t.Fatal("WS image observer changed totals")
		}
	}
	if requests.Load() != 2 {
		t.Fatal(fmt.Sprint("observer made extra WS requests: ", requests.Load()))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}
