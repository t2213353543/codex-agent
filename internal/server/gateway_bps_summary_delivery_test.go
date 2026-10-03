package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const independentSummaryOptions = `"stream_options":{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}`

func independentSummaryWire(id, formatter string) string {
	frame := func(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }
	reason := map[string]any{"type": "reasoning", "id": "rs_summary", "summary": []any{map[string]string{"type": "summary_text", "text": "Full summary <>&"}}, "encrypted_content": "SYNTHETIC_EXACT_CIPHER"}
	answer := map[string]any{"type": "message", "id": "msg_summary", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "ANSWER"}}}
	if formatter != "" {
		answer = map[string]any{"type": "function_call", "id": "fc_format", "call_id": "call_format", "name": formatter, "arguments": `{"number":9007199254740993}`, "status": "completed"}
	}
	wire := frame(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}})
	wire += frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": "rs_summary", "summary": []any{}}})
	wire += frame(map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": "rs_summary", "output_index": 0, "summary_index": 0, "delta": "Full "})
	wire += frame(map[string]any{"type": "response.reasoning_summary_text.done", "item_id": "rs_summary", "output_index": 0, "summary_index": 0, "text": "Full summary <>&"})
	wire += frame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reason})
	if formatter == "" {
		wire += frame(map[string]any{"type": "response.output_text.delta", "item_id": "msg_summary", "output_index": 1, "content_index": 0, "delta": "ANSWER"})
	}
	wire += frame(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": answer})
	wire += frame(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "model": "gpt-6-astra", "output": []any{reason, answer}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 7, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 0}}}})
	return wire
}
func independentSummaryAssertEvents(t *testing.T, wire string) {
	t.Helper()
	events := bpsGatewayEvents(t, wire)
	delta, done, itemDone, terminal := -1, -1, -1, -1
	for i, event := range events {
		switch bpsGatewayString(event, "type") {
		case "response.reasoning_summary_text.delta":
			delta = i
			if bpsGatewayString(event, "delta") != "Full " {
				t.Fatal("summary delta changed")
			}
		case "response.reasoning_summary_text.done":
			done = i
			if bpsGatewayString(event, "text") != "Full summary <>&" || bpsGatewayString(event, "item_id") != "rs_summary" || string(event["summary_index"]) != "0" {
				t.Fatal("summary done identity/text/index changed")
			}
		case "response.output_item.done":
			var item map[string]json.RawMessage
			_ = json.Unmarshal(event["item"], &item)
			if bpsGatewayString(item, "type") == "reasoning" {
				itemDone = i
				if bpsGatewayString(item, "encrypted_content") != "SYNTHETIC_EXACT_CIPHER" {
					t.Fatal("opaque state changed")
				}
			}
		case "response.completed":
			terminal = i
		}
	}
	if delta < 0 || done <= delta || itemDone <= done || terminal <= itemDone {
		t.Fatalf("summary cutoff event order changed %d/%d/%d/%d", delta, done, itemDone, terminal)
	}
}
func TestBPSSequentialCutoffHTTPJSONSSEAndStructured(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, structured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%t", stream, structured), func(t *testing.T) {
				var baseline []byte
				for _, enabled := range []bool{false, true} {
					calls := 0
					p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
						calls++
						body, _ := io.ReadAll(r.Body)
						var request map[string]json.RawMessage
						_ = json.Unmarshal(body, &request)
						if request["stream_options"] != nil {
							t.Error("stream_options leaked")
						}
						name := ""
						var input []struct{ Tools []struct{ Name string } }
						_ = json.Unmarshal(request["input"], &input)
						for _, item := range input {
							for _, tool := range item.Tools {
								if strings.HasPrefix(tool.Name, "emit_structured_") {
									name = tool.Name
								}
							}
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, independentSummaryWire("resp_summary", name))
					})
					extra := ""
					if enabled {
						extra += "," + independentSummaryOptions
					}
					if structured {
						extra += `,"text":` + bpsStructuredTestText
					}
					w := httptest.NewRecorder()
					p.gatewayBPS(w, proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[]%s}`, stream, extra)))
					if w.Code != 200 || calls != 1 {
						t.Fatalf("summary delivery failed: %d", w.Code)
					}
					if !enabled {
						baseline = bytes.Clone(w.Body.Bytes())
					} else if !bytes.Equal(baseline, w.Body.Bytes()) {
						t.Fatal("delivery opt changed downstream event/payload bytes")
					}
					if stream {
						independentSummaryAssertEvents(t, w.Body.String())
					}
					record, _ := p.Metrics.LastFor("test")
					if record.InputTokens != 100 || record.CachedTokens != 80 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 0 || record.ReturnedModel != "gpt-6-astra" {
						t.Fatal("summary opt changed actual usage/model")
					}
				}
			})
		}
	}
}
func TestBPSSequentialCutoffWSWarmupHistoryAndCompact(t *testing.T) {
	var calls atomic.Int32
	captured := make(chan []byte, 3)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		captured <- body
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		if fields["stream_options"] != nil {
			t.Error("local delivery leaked upstream")
		}
		if strings.HasSuffix(r.URL.Path, "/compact") {
			if len(fields) != 3 {
				t.Error("compact schema changed")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"compact_summary","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"SYNTHETIC_COMPACT"}],"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, independentSummaryWire(fmt.Sprintf("resp_summary_%d", calls.Load()), ""))
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "summary-session")
	defer conn.CloseNow()
	bpsGatewayWSSend(t, conn, `{"type":"response.create","generate":false,"model":"gpt-6-astra","input":[{"role":"user","content":"FIRST_CONTEXT"}],`+independentSummaryOptions+`}`)
	warm := bpsGatewayWSUntilTerminal(t, conn, "")
	warmID := bpsGatewayWSResponseID(t, warm)
	if calls.Load() != 0 {
		t.Fatal("warmup generated upstream")
	}
	previous := warmID
	for turn := 1; turn <= 2; turn++ {
		bpsGatewayWSSend(t, conn, fmt.Sprintf(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":%q,"input":[{"role":"user","content":"TURN_%d"}]}`, previous, turn))
		var wire strings.Builder
		for i := 0; i < 20; i++ {
			event := bpsGatewayWSReadEvent(t, conn)
			encoded, _ := json.Marshal(event)
			wire.WriteString("data: " + string(encoded) + "\n\n")
			if bpsGatewayString(event, "type") == "response.completed" {
				previous = bpsGatewayWSResponseID(t, event)
				break
			}
			if bpsGatewayString(event, "type") == "error" {
				t.Fatal("WS summary failed")
			}
		}
		independentSummaryAssertEvents(t, wire.String())
		sent := <-captured
		if !bytes.Contains(sent, []byte("FIRST_CONTEXT")) {
			t.Fatal("warmup history lost")
		}
		if turn == 2 && !bytes.Contains(sent, []byte("SYNTHETIC_EXACT_CIPHER")) {
			t.Fatal("continuation lost prior opaque reasoning")
		}
	}
	request := proxyRequest(http.MethodPost, "/v1/responses/compact", fmt.Sprintf(`{"model":"gpt-6-astra","previous_response_id":%q,"input":[],%s}`, previous, independentSummaryOptions))
	request.Header.Set("Session-Id", "summary-session")
	w := httptest.NewRecorder()
	p.gatewayBPS(w, request)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SYNTHETIC_COMPACT") || calls.Load() != 3 {
		t.Fatalf("compact failed: %d", w.Code)
	}
	sent := <-captured
	if !bytes.Contains(sent, []byte("SYNTHETIC_EXACT_CIPHER")) {
		t.Fatal("compact discarded opaque context")
	}
	history, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", previous)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(history.Request, &body)
	var actual, want any
	_ = json.Unmarshal(body["stream_options"], &actual)
	_ = json.Unmarshal([]byte(`{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}`), &want)
	if !reflect.DeepEqual(actual, want) {
		t.Fatal("delivery control disappeared from incremental history")
	}
}
