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
)

func earlyRows(t *testing.T, p *Proxy, count int) []RequestRecord {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.Metrics.mu.Lock()
		rows := append([]RequestRecord(nil), p.Metrics.recent[:p.Metrics.recentAt]...)
		p.Metrics.mu.Unlock()
		if len(rows) == count {
			return rows
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing records")
	return nil
}
func earlyParam(t *testing.T, raw []byte) string {
	t.Helper()
	var envelope struct{ Error struct{ Param string } }
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatalf("bad error JSON: %s", raw)
	}
	return envelope.Error.Param
}

func TestBPSEarlyValidationPublicParamAndSafeRecord(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unknown=%t", path, unknown), func(t *testing.T) {
				calls := 0
				p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
				option := `{"include_usage":"wrong"}`
				wantParam := "stream_options.include_usage"
				wantStored := wantParam
				if unknown {
					option = `{"PRIVATE_DYNAMIC_SECRET":true}`
					wantParam = "stream_options.PRIVATE_DYNAMIC_SECRET"
					wantStored = "stream_options.unknown"
				}
				out := httptest.NewRecorder()
				p.gatewayBPS(out, proxyRequest("POST", path, `{"model":"gpt-6-astra","stream":true,"input":[],"stream_options":`+option+`}`))
				record := earlyRows(t, p, 1)[0]
				if out.Code != 400 || earlyParam(t, out.Body.Bytes()) != wantParam || record.ErrorParam != wantStored || calls != 0 {
					t.Fatalf("API/persisted parameter boundary mismatch: http=%d response=%q record=%q calls=%d", out.Code, earlyParam(t, out.Body.Bytes()), record.ErrorParam, calls)
				}
				encoded, _ := json.Marshal(record)
				if unknown && strings.Contains(string(encoded), "PRIVATE_DYNAMIC_SECRET") {
					t.Fatal("dynamic key leaked into record")
				}
				if path == "/v1/responses" && (record.RequestType != "http_sse" || !record.Stream) {
					t.Fatal("SSE ingress evidence changed")
				}
				if path == "/v1/responses/compact" && (record.RequestType != "compact" || record.Stream) {
					t.Fatal("compact transport was guessed from stream flag")
				}
				if record.ToolPolicy != nil || record.UsageKnown != nil || len(record.Attempts) != 0 {
					t.Fatal("Prepare rejection fabricated successful policy/usage/attempt")
				}
			})
		}
	}
}

func TestBPSEarlyValidationWebSocketWarmupObservation(t *testing.T) {
	for _, mode := range []string{"warmup_rejected", "ordinary_rejected", "ordinary_absent_rejected", "warmup_success"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "early-"+mode)
			defer conn.CloseNow()
			generate := mode == "ordinary_rejected" || mode == "ordinary_absent_rejected"
			options := map[string]any{}
			if mode != "warmup_success" {
				options["include_usage"] = "wrong"
			}
			payload := map[string]any{"type": "response.create", "stream_id": "early", "model": "gpt-6-astra", "generate": generate, "stream": false, "input": []any{}, "stream_options": options}
			if mode == "ordinary_absent_rejected" {
				delete(payload, "generate")
			}
			raw, _ := json.Marshal(payload)
			bpsGatewayWSSend(t, conn, string(raw))
			terminal := bpsGatewayWSUntilTerminal(t, conn, "early")
			record := earlyRows(t, p, 1)[0]
			if calls.Load() != 0 || record.RequestType != "websocket" || !record.Stream {
				t.Fatalf("WS entry classification/dispatch mismatch: %s stream=%v calls=%d", record.RequestType, record.Stream, calls.Load())
			}
			if mode != "warmup_success" {
				encoded, _ := json.Marshal(terminal)
				if bpsGatewayString(terminal, "type") != "error" || earlyParam(t, encoded) != "stream_options.include_usage" || record.ErrorParam != "stream_options.include_usage" || record.ToolPolicy != nil {
					t.Fatal("WS parameter response/record mismatch")
				}
			} else if bpsGatewayString(terminal, "type") != "response.completed" || record.ErrorParam != "" {
				t.Fatal("warmup success retained rejection")
			}
			want := !generate
			if record.WebSocketWarmup == nil || *record.WebSocketWarmup != want {
				t.Errorf("trusted WS generate flag lost on early path: mode=%s want=%v observed=%v", mode, want, record.WebSocketWarmup)
			}
		})
	}
}

func TestBPSEarlyValidationUnknownWarmupContextStaysUnknown(t *testing.T) {
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("early request dispatched") })
	request := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","input":[],"stream_options":{"include_usage":1}}`)
	request = request.WithContext(context.WithValue(request.Context(), bpsWebSocketKey{}, true))
	p.gatewayBPS(httptest.NewRecorder(), request)
	if record := earlyRows(t, p, 1)[0]; record.WebSocketWarmup != nil {
		t.Fatal("missing trusted generate context fabricated known false")
	}
}

func TestBPSEarlyValidationMalformedGenerateNeverReachesObservation(t *testing.T) {
	p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid WS generate dispatched") })
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "malformed-generate")
	defer conn.CloseNow()
	for _, value := range []string{`null`, `"false"`, `1`} {
		bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"invalid","model":"gpt-6-astra","generate":`+value+`,"input":[]}`)
		event := bpsGatewayWSUntilTerminal(t, conn, "invalid")
		raw, _ := json.Marshal(event)
		if !strings.Contains(string(raw), "invalid_generate") {
			t.Fatalf("wrong ingress rejection: %s", raw)
		}
	}
	p.Metrics.mu.Lock()
	defer p.Metrics.mu.Unlock()
	if p.Metrics.recentAt != 0 {
		t.Fatal("unvalidated generate fabricated request observation")
	}
}

func TestBPSEarlyValidationPriorRequestDoesNotTaintSuccess(t *testing.T) {
	calls := 0
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "success-after-error", "[]"))
	})
	bad := httptest.NewRecorder()
	p.gatewayBPS(bad, proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[],"stream_options":{"include_usage":1}}`))
	good := httptest.NewRecorder()
	p.gatewayBPS(good, proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[]}`))
	rows := earlyRows(t, p, 2)
	if bad.Code != 400 || good.Code != 200 || calls != 1 || rows[0].ErrorParam != "stream_options.include_usage" || rows[1].ErrorParam != "" || rows[1].Status != "ok" || rows[1].RequestType != "http_json" || rows[1].Stream {
		t.Fatalf("record identity leaked across requests: rows=%+v", rows)
	}
	encoded, _ := json.Marshal(rows[1])
	if strings.Contains(string(encoded), "error_param") {
		t.Fatal("success persisted previous error param")
	}
}
