package server

import (
	"context"
	"encoding/json"
	"errors"
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

func TestGatewayObservationHooksHTTPProvenanceAndBytes(t *testing.T) {
	const response = `{"id":"observed-native","model":"returned-model","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"service_tier":"default","reasoning":{"effort":"medium"},"usage":{"input_tokens":11,"output_tokens":3,"total_tokens":14,"codex_rollout_budget_units":0,"input_tokens_details":{"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"usage_metadata":{"amount":"0.125","private":"PRIVATE_METADATA"},"output":[]}`
	const wire = ": keepalive\r\n\r\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"PRIVATE_TOOL_ARGUMENT\"}\r\n\r\ndata: {\"type\":\"response.incomplete\",\"response\":" + response + "}\r\n\r\n"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var requestCount atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				w.Header().Set("Openai-Model", "header-model")
				w.Header().Set("X-Request-Id", "request-observed")
				_, _ = io.WriteString(w, wire)
			})
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"model":"requested-model","stream":%t,"service_tier":"priority","reasoning":{"effort":"high"},"input":[{"type":"compaction_trigger"},{"type":"message","role":"user","content":"PRIVATE_PROMPT"}]}`, stream)
			p.ServeHTTP(w, proxyRequest("POST", "/responses", body))
			want := response
			if stream {
				want = wire
			}
			if w.Code != 200 || w.Body.String() != want || requestCount.Load() != 1 {
				t.Fatal("observation altered response bytes or request count")
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.UpstreamHeaderMs == nil || rec.TTFTMs == nil || *rec.UpstreamHeaderMs > *rec.TTFTMs || rec.FirstOutputKind != "response.function_call_arguments.delta" {
				t.Fatalf("header/output timing hook missing: %+v", rec)
			}
			if rec.RequestedServiceTier != "priority" || rec.RequestedReasoningEffort != "high" || rec.ServiceTier != "default" || rec.ServiceTierSource != "response" || rec.ReasoningEffort != "medium" || rec.ReasoningEffortSource != "response" || rec.UpstreamModel != "returned-model" || rec.UpstreamModelSource != "response" {
				t.Fatal("request and response provenance mixed")
			}
			if (stream && (rec.NativeCompaction == nil || !*rec.NativeCompaction)) || (!stream && rec.NativeCompaction != nil) {
				t.Fatal("native compaction inferred without native streaming request")
			}
			if rec.TerminalStatus != "incomplete" || rec.IncompleteReason != "max_output_tokens" || rec.UsageTotalTokens == nil || *rec.UsageTotalTokens != 14 || rec.RolloutBudgetUnits == nil || *rec.RolloutBudgetUnits != 0 || rec.UsageAmount != "0.125" {
				t.Fatal("terminal metadata hook missing or opaque amount reinterpreted")
			}
			encoded, _ := json.Marshal(rec)
			if strings.Contains(string(encoded), "PRIVATE_") {
				t.Fatal("observation persisted content")
			}
		})
	}
}

func TestGatewayObservationHooksNativeCompactionEndpointScope(t *testing.T) {
	const payload = `{"type":"response.create","stream":true,"input":[{"type":"compaction_trigger"}]}`
	for _, requestType := range []string{"http_sse", "http_json", "websocket"} {
		for _, endpoint := range []string{"responses", "guardian", "guardian-classifier", "models", "responses/compact"} {
			t.Run(requestType+"/"+endpoint, func(t *testing.T) {
				rec := RequestRecord{Endpoint: endpoint}
				observeGatewayRequest(&rec, "", requestType, []byte(payload))
				if endpoint == "responses" {
					if rec.NativeCompaction == nil || !*rec.NativeCompaction {
						t.Fatal("native Responses trigger not observed")
					}
				} else if rec.NativeCompaction != nil {
					t.Fatal("unrelated endpoint mislabeled native compaction")
				}
			})
		}
	}
	for _, endpoint := range []string{"guardian", "guardian-classifier"} {
		t.Run("forwarded/"+endpoint, func(t *testing.T) {
			const wire = "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, wire) })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/"+endpoint, `{"model":"test","stream":true,"input":[{"type":"compaction_trigger"}]}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.NativeCompaction != nil || w.Code != 200 || w.Body.String() != wire {
				t.Fatal("endpoint observation altered forwarding or inferred native compaction")
			}
		})
	}
}

func TestGatewayObservationHooksFinalHeadersAfter401(t *testing.T) {
	for _, path := range []string{"/responses", "/responses/compact", "/models"} {
		for _, recoverOK := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/recover=%t", path, recoverOK), func(t *testing.T) {
				var requests, refreshes atomic.Int32
				const rejected = `{"error":{"message":"original rejection"}}`
				const result = `{"output":[],"models":[]}`
				wire := result
				if path == "/responses" {
					wire = "data: {\"type\":\"response.completed\",\"response\":" + result + "}\n\n"
				}
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/oauth/token" {
						refreshes.Add(1)
						time.Sleep(60 * time.Millisecond)
						if !recoverOK {
							w.WriteHeader(400)
							_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
							return
						}
						_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"recovered-fixture","refresh_token":"rotated-fixture"}`, "test-account", "fixture-user"))
						return
					}
					if requests.Add(1) == 1 {
						w.Header().Set("X-Request-Id", "original-rejection")
						w.WriteHeader(401)
						_, _ = io.WriteString(w, rejected)
						return
					}
					w.Header().Set("X-Request-Id", "final-response")
					_, _ = io.WriteString(w, wire)
				})
				method := "POST"
				if path == "/models" {
					method = "GET"
				}
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(method, path, `{"model":"test","stream":true,"input":[]}`))
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.UpstreamHeaderMs == nil || refreshes.Load() != 1 {
					t.Fatal("missing actual header observation")
				}
				if recoverOK {
					if w.Code != 200 || w.Body.String() != wire || rec.RequestID != "final-response" || *rec.UpstreamHeaderMs < 50 || requests.Load() != 2 || rec.ErrorSource != "" {
						t.Fatalf("successful recovery observation changed: %+v", rec)
					}
				} else if w.Code != 401 || w.Body.String() != rejected || requests.Load() != 1 || rec.RequestID != "original-rejection" || rec.LatencyMs-*rec.UpstreamHeaderMs < 50 || rec.ErrorSource != "upstream" || rec.ErrorPhase != "http_status" {
					t.Fatalf("failed refresh was recorded as later headers or changed original failure: %+v", rec)
				}
			})
		}
	}
}

type observationFaultTransport func(*http.Request) (*http.Response, error)

func (f observationFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewayObservationHooksTimeoutHasNoHeaders(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint("retry=", retry), func(t *testing.T) {
			p := newTestProxy(t, 1)
			var attempts atomic.Int32
			p.Client = &http.Client{Transport: observationFaultTransport(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					defer r.Body.Close()
				}
				if r.URL.Path == "/oauth/token" {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(gatewayKnownOwnerRefreshFixture(`{"access_token":"recovered","refresh_token":"rotated"}`, "test-account", "fixture-user"))), Request: r}, nil
				}
				if attempts.Add(1) == 1 && retry {
					return &http.Response{StatusCode: 401, Header: http.Header{"X-Request-Id": {"discarded-first-attempt"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
				}
				return nil, context.DeadlineExceeded
			})}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", `{"model":"test","stream":true}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || w.Code != 502 || rec.UpstreamHeaderMs != nil || rec.TTFTMs != nil || rec.RequestID != "" || rec.ErrorSource != "transport" || rec.ErrorPhase != "response_headers" {
				t.Fatalf("timeout invented a final response measurement: %+v", rec)
			}
			want := int32(1)
			if retry {
				want = 2
			}
			if attempts.Load() != want {
				t.Fatal("observer changed retry count")
			}
		})
	}
}

func TestGatewayObservationHooksErrorsKeepExistingRecords(t *testing.T) {
	for _, test := range []struct {
		name, request, wire, source, phase string
		status                             int
		disable                            bool
	}{
		{"request", `{"input":[]}`, "", "client", "request_body", 400, false},
		{"settings", `{"model":"test","stream":true}`, "", "gateway", "settings", 403, true},
		{"status", `{"model":"test","stream":true}`, `{"error":{"message":"unavailable"}}`, "upstream", "http_status", 503, false},
		{"event", `{"model":"test","stream":true}`, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"rejected\"}}}\n\n", "upstream", "response_event", 200, false},
		{"eof", `{"model":"test","stream":true}`, "data: {\"type\":\"response.created\"}\n\n", "upstream", "response_body", 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.wire)
			})
			if test.disable {
				s, _ := p.Settings.Get()
				s.HTTPStreamEnabled = false
				if err := p.Settings.Set(s); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("POST", "/responses", test.request))
			page, err := p.Metrics.Logs(LogQuery{Size: 10})
			if err != nil || len(page.Items) != 1 {
				t.Fatal("observer changed record count")
			}
			rec := page.Items[0]
			if w.Code != test.status || rec.ErrorSource != test.source || rec.ErrorPhase != test.phase {
				t.Fatalf("wrong observed error branch: %+v", rec)
			}
			if test.wire != "" && w.Body.String() != test.wire {
				t.Fatal("observer rewrote failed response")
			}
		})
	}
	// Authentication previously produces no usage event; adding a passive
	// observer must not turn rejected keys into billable/admitted requests.
	p := newTestProxy(t, 1)
	w := httptest.NewRecorder()
	r := proxyRequest("POST", "/responses", `{"model":"test"}`)
	r.Header.Set("Authorization", "Bearer invalid-fixture")
	p.ServeHTTP(w, r)
	page, err := p.Metrics.Logs(LogQuery{Size: 10})
	if err != nil || w.Code != 401 || len(page.Items) != 0 {
		t.Fatal("observer changed pre-auth record semantics")
	}
}

func TestGatewayObservationHooksWebSocketFirstHandshakeOnly(t *testing.T) {
	for _, headerSession := range []bool{true, false} {
		t.Run(fmt.Sprint("header-session=", headerSession), func(t *testing.T) {
			const created = `{"type":"response.created","response":{"id":"ws-fixture-%d"}}`
			const output = `{"type":"response.custom_tool_call_input.delta","delta":"PRIVATE_TOOL_ARGUMENT"}`
			const completed = `{"type":"response.completed","response":{"id":"ws-fixture-%d","status":"completed","model":"returned-model","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3},"usage_metadata":{"amount":"0"}}}`
			var handshakes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					time.Sleep(40 * time.Millisecond)
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"ws-recovered","refresh_token":"ws-rotated"}`, "test-account", "fixture-user"))
					return
				}
				if handshakes.Add(1) == 1 {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Openai-Model", "handshake-model")
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for turn := range 2 {
					_, _, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					for _, payload := range []string{fmt.Sprintf(created, turn), output, fmt.Sprintf(completed, turn)} {
						if err := conn.Write(r.Context(), websocket.MessageText, []byte(payload)); err != nil {
							return
						}
					}
				}
				_, _, _ = conn.Read(r.Context())
			})
			t.Cleanup(p.closeGatewayClients)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": {"Bearer test-client-key"}}
			if headerSession {
				headers.Set("Session-Id", "observed-session")
			}
			conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			for turn := range 2 {
				request := `{"type":"response.create","model":"requested-model","prompt_cache_key":"observed-session","service_tier":"priority","reasoning":{"effort":"high"},"input":[{"type":"compaction_trigger"}]}`
				if err := conn.Write(ctx, websocket.MessageText, []byte(request)); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{fmt.Sprintf(created, turn), output, fmt.Sprintf(completed, turn)} {
					_, got, err := conn.Read(ctx)
					if err != nil || string(got) != want {
						t.Fatal("observer changed WebSocket frames")
					}
				}
				waitGatewayWSReleased(t, p)
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.TTFTMs == nil || rec.FirstOutputKind != "response.custom_tool_call_input.delta" || rec.NativeCompaction == nil || !*rec.NativeCompaction || rec.UpstreamModelSource != "response" || rec.ServiceTierSource != "request" || rec.ReasoningEffortSource != "request" || rec.UsageAmount != "0" {
					t.Fatalf("WebSocket metadata missing: %+v", rec)
				}
				if turn == 0 && (rec.UpstreamHeaderMs == nil || *rec.UpstreamHeaderMs < 30) {
					t.Fatal("first turn omitted recovered handshake timing")
				}
				if turn == 1 && rec.UpstreamHeaderMs != nil {
					t.Fatal("later turn reused handshake as a new header measurement")
				}
			}
			if handshakes.Load() != 2 {
				t.Fatal("observer changed WebSocket handshake/recovery count")
			}
		})
	}
}

func TestGatewayObservationHooksTransportErrorPreservesProxyOrigin(t *testing.T) {
	var record RequestRecord
	observeGatewayTransportError(&record, fmt.Errorf("wrapped: %w", errGatewayProxyUnavailable), "response_headers")
	if record.ErrorSource != "gateway" || record.ErrorPhase != "proxy_route" {
		t.Fatal("local proxy guard mislabeled upstream")
	}
	observeGatewayTransportError(&record, errors.New("later failure"), "response_headers")
	if record.ErrorSource != "gateway" || record.ErrorPhase != "proxy_route" {
		t.Fatal("first observed origin was replaced")
	}
}

type observationBrokenBody struct{ sent bool }

func (b *observationBrokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.ErrUnexpectedEOF
	}
	b.sent = true
	return copy(p, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}\n\n"), nil
}

func (*observationBrokenBody) Close() error { return nil }

type observationBrokenWriter struct{ *httptest.ResponseRecorder }

func (w observationBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGatewayObservationHooksStreamingReadAndWriteFailures(t *testing.T) {
	for _, upstreamBroken := range []bool{true, false} {
		t.Run(fmt.Sprint("upstream=", upstreamBroken), func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.Client = &http.Client{Transport: observationFaultTransport(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					_ = r.Body.Close()
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &observationBrokenBody{}, Request: r}, nil
			})}
			w := httptest.NewRecorder()
			var writer http.ResponseWriter = w
			if !upstreamBroken {
				writer = observationBrokenWriter{w}
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				p.ServeHTTP(writer, proxyRequest("POST", "/responses", `{"model":"test","stream":true}`))
			}()
			rec, ok := p.Metrics.LastFor("test")
			wantSource, wantPhase := "client", "response_write"
			if upstreamBroken {
				wantSource, wantPhase = "upstream", "response_body"
			}
			if !ok || rec.UpstreamHeaderMs == nil || rec.ErrorSource != wantSource || rec.ErrorPhase != wantPhase || rec.Status != "aborted" {
				t.Fatalf("stream failure origin missing: %+v", rec)
			}
			if (upstreamBroken && recovered != http.ErrAbortHandler) || (!upstreamBroken && recovered != nil) {
				t.Fatal("observer changed truncation behavior")
			}
		})
	}
}

func TestGatewayObservationHooksWebSocketAbortOrigins(t *testing.T) {
	for _, origin := range []string{"upstream", "client", "event"} {
		t.Run(origin, func(t *testing.T) {
			requestRead := make(chan struct{})
			const created = `{"type":"response.created","response":{"id":"pending-observed"}}`
			const failure = `{"type":"error","error":{"message":"fixture failure"}}`
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
				close(requestRead)
				if origin == "client" {
					_, _, _ = conn.Read(r.Context())
					return
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(created))
				if origin == "event" {
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(failure))
					_, _, _ = conn.Read(r.Context())
				}
			})
			t.Cleanup(p.closeGatewayClients)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"abort-observed"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requestRead:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if origin == "client" {
				_ = conn.CloseNow()
			} else {
				_, got, err := conn.Read(ctx)
				if err != nil || string(got) != created {
					t.Fatal("changed created frame")
				}
				if origin == "event" {
					_, got, err = conn.Read(ctx)
					if err != nil || string(got) != failure {
						t.Fatal("changed unscoped error frame")
					}
				}
				_, _, _ = conn.Read(ctx)
			}
			waitGatewayWSReleased(t, p)
			rec, ok := p.Metrics.LastFor("test")
			wantSource, wantPhase := "upstream", "response_read"
			if origin == "client" {
				wantSource, wantPhase = "client", "request_read"
			}
			if origin == "event" {
				wantSource, wantPhase = "upstream", "response_event"
			}
			if !ok || rec.Status != "aborted" || rec.ErrorSource != wantSource || rec.ErrorPhase != wantPhase || rec.TTFTMs != nil {
				t.Fatalf("pending WebSocket abort origin missing: %+v", rec)
			}
		})
	}
}
