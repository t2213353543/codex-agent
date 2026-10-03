package server

import (
	"bytes"
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

const bpsStructuredTestText = `{"format":{"type":"json_schema","name":"reply","strict":true,"schema":{"type":"object","properties":{"number":{"type":"integer","const":9007199254740993}},"required":["number"],"additionalProperties":false}}}`

func bpsStructuredUpstreamReply(t *testing.T, w http.ResponseWriter, r *http.Request, responseID string, arguments string) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Input []struct {
			Type  string
			Tools []struct {
				Name   string
				Strict bool
			}
		}
	}
	if json.Unmarshal(body, &request) != nil {
		t.Fatal("invalid synthetic request")
	}
	name := ""
	for _, item := range request.Input {
		for _, tool := range item.Tools {
			if strings.HasPrefix(tool.Name, "emit_structured_") {
				name = tool.Name
			}
		}
	}
	if name == "" || bytes.Contains(body, []byte(`"text":{"format"`)) {
		t.Error("formatter was not adapted to upstream tools")
	}
	item := map[string]any{"type": "function_call", "id": "fc_synthetic_format", "call_id": "call_synthetic_format", "name": name, "arguments": arguments, "status": "completed"}
	response := map[string]any{"id": responseID, "status": "completed", "model": "gpt-6-astra", "output": []any{item}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 0}}}
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
}

func TestBPSStructuredGatewayJSONSSEHistoryAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				bpsStructuredUpstreamReply(t, w, r, fmt.Sprintf("resp_structured_%d", n), ` {"number":9007199254740993} `)
			})
			for turn := 1; turn <= 2; turn++ {
				body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"answer","tool_choice":"none","text":%s}`, stream, bpsStructuredTestText)
				if turn == 2 {
					body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"previous_response_id":"resp_structured_1","input":[{"role":"user","content":"continue"}]}`, stream)
				}
				r := proxyRequest(http.MethodPost, "/v1/responses", body)
				r.Header.Set("Session-Id", "structured-session")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != 200 || strings.Contains(w.Body.String(), "emit_structured_") || strings.Contains(w.Body.String(), "function_call") {
					t.Fatalf("internal formatter leaked or failed: %d %s", w.Code, w.Body)
				}
				var response map[string]json.RawMessage
				if stream {
					events := bpsGatewayEvents(t, w.Body.String())
					_ = json.Unmarshal(events[len(events)-1]["response"], &response)
				} else {
					_ = json.Unmarshal(w.Body.Bytes(), &response)
				}
				if bpsGatewayString(response, "status") != "completed" || !bytes.Contains(response["output"], []byte(`9007199254740993`)) {
					t.Fatal("projected response missing original JSON")
				}
				record, ok := p.Metrics.LastFor("test")
				if !ok || record.ReturnedModel != "gpt-6-astra" || record.InputTokens != 100 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 0 || record.TTFTMs == nil {
					t.Fatalf("actual response observations lost: %+v", record)
				}
				key, _, _, err := p.resolveGatewayKeyContext(r.Context(), bearerToken(r))
				if err != nil {
					t.Fatal(err)
				}
				history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(r.Context(), true), key, fmt.Sprintf("resp_structured_%d", turn))
				if err != nil {
					t.Fatal(err)
				}
				stored, _ := json.Marshal(history)
				if bytes.Contains(stored, []byte("emit_structured_")) || bytes.Contains(stored, []byte("fc_synthetic_format")) {
					t.Fatal("internal formatter leaked into continuation history")
				}
			}
		})
	}
}

func TestBPSStructuredGatewayRejectsInvalidResultBeforePublicOutput(t *testing.T) {
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		bpsStructuredUpstreamReply(t, w, r, "resp_bad_format", `{"number":2}`)
	})
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"answer","text":`+bpsStructuredTestText+`}`)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 502 || strings.Contains(w.Body.String(), "9007199254740993") || strings.Contains(w.Body.String(), `"number":2`) || strings.Contains(w.Body.String(), "emit_structured_") {
		t.Fatalf("invalid format accepted or payload leaked: %d %s", w.Code, w.Body)
	}
	record, ok := p.Metrics.LastFor("test")
	if !ok || record.Status != "error" || record.InputTokens != 100 || record.TTFTMs != nil {
		t.Fatal("invalid structured result observations misleading")
	}
}

func TestBPSStructuredGatewayWebSocketRetiresLane(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		bpsStructuredUpstreamReply(t, w, r, fmt.Sprintf("resp_ws_format_%d", calls.Add(1)), `{"number":9007199254740993}`)
	})
	s := httptest.NewServer(p)
	defer s.Close()
	conn, _ := gatewayWSDial(t, s.URL, "structured-ws-session")
	defer conn.CloseNow()
	for turn := 1; turn <= 2; turn++ {
		request := `{"type":"response.create","stream_id":"format-lane","model":"gpt-6-astra","input":"answer","tool_choice":"none","text":` + bpsStructuredTestText + `}`
		bpsGatewayWSSend(t, conn, request)
		deadline := time.Now().Add(5 * time.Second)
		for {
			event := bpsGatewayWSReadEvent(t, conn)
			wire, _ := json.Marshal(event)
			if bytes.Contains(wire, []byte("emit_structured_")) || bytes.Contains(wire, []byte("function_call")) {
				t.Fatal("internal formatter exposed over websocket")
			}
			kind := bpsGatewayString(event, "type")
			if kind == "error" || time.Now().After(deadline) {
				t.Fatalf("websocket structured response failed: %s", wire)
			}
			if kind == "response.completed" {
				break
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatal("completed structured websocket lane was not reusable")
	}
}

func TestBPSStructuredGatewayRequiredClientToolThenFormattedAnswer(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_client_tool", `[{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{}"}]`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"call_id":"call_lookup"`)) || !bytes.Contains(body, []byte(`"output":"real client result"`)) {
			t.Error("client tool history was not preserved")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		bpsStructuredUpstreamReply(t, w, r, "resp_after_client_tool", `{"number":9007199254740993}`)
	})
	first := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"lookup then answer","tool_choice":"required","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"text":`+bpsStructuredTestText+`}`)
	first.Header.Set("Session-Id", "client-tools-and-format")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, first)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"lookup"`) || strings.Contains(w.Body.String(), "emit_structured_") {
		t.Fatalf("required real client action was hidden or rejected: %d %s", w.Code, w.Body)
	}
	next := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","previous_response_id":"resp_client_tool","tool_choice":"none","input":[{"type":"function_call_output","call_id":"call_lookup","output":"real client result"}]}`)
	next.Header.Set("Session-Id", "client-tools-and-format")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, next)
	if w.Code != 200 || strings.Contains(w.Body.String(), "function_call") || !strings.Contains(w.Body.String(), "9007199254740993") || calls.Load() != 2 {
		t.Fatalf("formatted answer after client result failed: %d %s", w.Code, w.Body)
	}
}
