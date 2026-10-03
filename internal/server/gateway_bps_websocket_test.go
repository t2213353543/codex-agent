package server

import (
	"bytes"
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

func bpsGatewayWSSend(t *testing.T, conn *websocket.Conn, raw string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

func bpsGatewayWSReadEvent(t *testing.T, conn *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	var event map[string]json.RawMessage
	if err := json.Unmarshal(gatewayWSRead(t, conn), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func bpsGatewayWSUntilTerminal(t *testing.T, conn *websocket.Conn, streamID string) map[string]json.RawMessage {
	t.Helper()
	for i := 0; i < 50; i++ {
		event := bpsGatewayWSReadEvent(t, conn)
		if bpsGatewayString(event, "stream_id") != streamID {
			t.Fatalf("event crossed streams: %s", event)
		}
		switch bpsGatewayString(event, "type") {
		case "response.completed", "response.failed", "response.cancelled", "error":
			return event
		}
	}
	t.Fatal("stream exceeded fixture event bound")
	return nil
}

func bpsGatewayWSResponseID(t *testing.T, event map[string]json.RawMessage) string {
	t.Helper()
	var response map[string]json.RawMessage
	if json.Unmarshal(event["response"], &response) != nil {
		t.Fatalf("event has no response: %s", event)
	}
	return bpsGatewayString(response, "id")
}

func TestBPSGatewayWebSocketWarmupAndIncrementalConversation(t *testing.T) {
	var calls atomic.Int32
	captured := make(chan []byte, 3)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		captured <- raw
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		for _, name := range []string{"generate", "previous_response_id", "stream_id", "type"} {
			if body[name] != nil {
				t.Errorf("Codex WebSocket control forwarded to BPS: %s", name)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		output := fmt.Sprintf(`[{"id":"msg_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant-turn-%d"}]}]`, n, n)
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("resp_turn_%d", n), output))
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "bps-ws-conversation")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"lane_1","generate":false,"model":"gpt-6-astra","input":[{"role":"user","content":"warmup-context"}]}`)
	warmup := bpsGatewayWSUntilTerminal(t, conn, "lane_1")
	if bpsGatewayString(warmup, "type") != "response.completed" || calls.Load() != 0 {
		t.Fatalf("warmup reached inference or failed: %s; calls=%d", warmup, calls.Load())
	}
	warmID := bpsGatewayWSResponseID(t, warmup)
	if !strings.HasPrefix(warmID, "resp_warmup_") {
		t.Fatalf("warmup ID = %q", warmID)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"lane_1","model":"gpt-6-astra","previous_response_id":"`+warmID+`","input":[{"role":"user","content":"first-generated-question"}]}`)
	first := bpsGatewayWSUntilTerminal(t, conn, "lane_1")
	if bpsGatewayString(first, "type") != "response.completed" {
		t.Fatalf("first generated response = %s", first)
	}
	firstBody := <-captured
	if !bytes.Contains(firstBody, []byte("warmup-context")) || !bytes.Contains(firstBody, []byte("first-generated-question")) {
		t.Fatalf("warmup history not expanded: %s", firstBody)
	}
	firstID := bpsGatewayWSResponseID(t, first)
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"lane_1","model":"gpt-6-astra","previous_response_id":"`+firstID+`","input":[{"role":"user","content":"second-generated-question"}]}`)
	second := bpsGatewayWSUntilTerminal(t, conn, "lane_1")
	if bpsGatewayString(second, "type") != "response.completed" || calls.Load() != 2 {
		t.Fatalf("second generated response = %s; calls=%d", second, calls.Load())
	}
	secondBody := <-captured
	for _, want := range []string{"warmup-context", "first-generated-question", "assistant-turn-1", "second-generated-question"} {
		if !bytes.Contains(secondBody, []byte(want)) {
			t.Errorf("expanded history missing %q: %s", want, secondBody)
		}
	}
}

func TestBPSGatewayWebSocketConcurrentStreamEventsStayIsolated(t *testing.T) {
	arrived := make(chan string, 2)
	release := make(chan struct{})
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lane := "A"
		if bytes.Contains(body, []byte("question-B")) {
			lane = "B"
		}
		arrived <- lane
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"answer-`+lane+`"}`+"\n\n")
		_ = http.NewResponseController(w).Flush()
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_"+lane, "[]"))
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "bps-lanes")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"lane_A","model":"gpt-6-astra","input":[{"role":"user","content":"question-A"}]}`)
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"lane_B","model":"gpt-6-astra","input":[{"role":"user","content":"question-B"}]}`)
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(4 * time.Second):
			close(release)
			t.Fatal("independent stream did not reach upstream")
		}
	}
	close(release)
	completed, deltas := map[string]bool{}, map[string]bool{}
	for len(completed) < 2 {
		event := bpsGatewayWSReadEvent(t, conn)
		streamID := bpsGatewayString(event, "stream_id")
		if streamID != "lane_A" && streamID != "lane_B" {
			t.Fatalf("unscoped event: %s", event)
		}
		lane := strings.TrimPrefix(streamID, "lane_")
		switch bpsGatewayString(event, "type") {
		case "response.output_text.delta":
			if bpsGatewayString(event, "delta") != "answer-"+lane {
				t.Fatalf("delta crossed stream: %s", event)
			}
			deltas[lane] = true
		case "response.completed":
			if bpsGatewayWSResponseID(t, event) != "resp_"+lane {
				t.Fatalf("completion crossed stream: %s", event)
			}
			completed[lane] = true
		case "error", "response.failed":
			t.Fatalf("independent stream failed: %s", event)
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("lost live deltas: %v", deltas)
	}
}

func TestBPSGatewayWebSocketCancelInterruptsActualHTTPUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_cancel","status":"in_progress","output":[]}}`+"\n\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "bps-cancel")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"cancel_lane","model":"gpt-6-astra","input":[]}`)
	created := bpsGatewayWSReadEvent(t, conn)
	if bpsGatewayString(created, "type") != "response.created" {
		t.Fatalf("expected created before cancel: %s", created)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.cancel","stream_id":"cancel_lane","response_id":"resp_cancel"}`)
	terminal := bpsGatewayWSUntilTerminal(t, conn, "cancel_lane")
	if bpsGatewayString(terminal, "type") != "response.cancelled" {
		t.Fatalf("cancel terminal = %s", terminal)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not close the actual upstream HTTP request")
	}
	waitGatewayWSReleased(t, p)
}

func TestBPSGatewayWebSocketNativeFunctionAndCustomToolReturnLoop(t *testing.T) {
	var calls atomic.Int32
	captured := make(chan []byte, 2)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			output := `[{"type":"function_call","id":"fc_inspect","call_id":"call_inspect","name":"inspect","arguments":"{\"path\":\"fixture.txt\"}"},{"type":"custom_tool_call","id":"ct_patch","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}]`
			_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_tools", output))
			return
		}
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_tool_done", `[{"type":"message","id":"msg_done","role":"assistant","content":[{"type":"output_text","text":"TOOL_LOOP_OK"}]}]`))
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "bps-tools")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"tool_lane","model":"gpt-6-astra","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},{"type":"custom","name":"apply_patch","format":{"type":"text"}}]}],"input":[{"role":"user","content":"inspect and patch fixture"}]}`)
	first := bpsGatewayWSUntilTerminal(t, conn, "tool_lane")
	if bpsGatewayString(first, "type") != "response.completed" {
		t.Fatalf("native tool selection failed: %s", first)
	}
	var response struct{ Output []map[string]json.RawMessage }
	if json.Unmarshal(first["response"], &response) != nil || len(response.Output) != 2 {
		t.Fatalf("tool output missing: %s", first)
	}
	for _, item := range response.Output {
		if bpsGatewayString(item, "namespace") != "functions" {
			t.Fatalf("client tool namespace missing: %s", item)
		}
	}
	<-captured
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"tool_lane","model":"gpt-6-astra","previous_response_id":"resp_tools","input":[{"type":"function_call_output","call_id":"call_inspect","output":"fixture contents"},{"type":"custom_tool_call_output","call_id":"call_patch","output":"patch applied"}]}`)
	second := bpsGatewayWSUntilTerminal(t, conn, "tool_lane")
	if bpsGatewayString(second, "type") != "response.completed" || !bytes.Contains(second["response"], []byte("TOOL_LOOP_OK")) {
		t.Fatalf("native tool result continuation failed: %s", second)
	}
	secondBody := <-captured
	var request struct{ Input []map[string]json.RawMessage }
	if json.Unmarshal(secondBody, &request) != nil {
		t.Fatal("invalid expanded upstream request")
	}
	seen := map[string]int{}
	for _, item := range request.Input {
		kind := bpsGatewayString(item, "type")
		if kind == "function_call" || kind == "function_call_output" || kind == "custom_tool_call" || kind == "custom_tool_call_output" {
			seen[kind]++
			wantID := "call_inspect"
			if strings.HasPrefix(kind, "custom_") {
				wantID = "call_patch"
			}
			if bpsGatewayString(item, "call_id") != wantID {
				t.Errorf("tool pairing changed: %s", item)
			}
		}
	}
	for _, kind := range []string{"function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output"} {
		if seen[kind] != 1 {
			t.Errorf("native item %s count = %d; body=%s", kind, seen[kind], secondBody)
		}
	}
}

func TestBPSGatewayWebSocketCompactedStateSurvivesNextTurn(t *testing.T) {
	captured := make(chan []byte, 1)
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/responses/compact" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_compacted","output":[{"type":"compaction","encrypted_content":"opaque-past-state"}]}`)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "resp_after_compact", "[]"))
	})
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "bps-compaction")
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"compact_lane","model":"gpt-6-astra","input":[{"role":"user","content":"old-question-to-be-compressed"},{"type":"compaction_trigger"}]}`)
	compact := bpsGatewayWSUntilTerminal(t, conn, "compact_lane")
	if bpsGatewayString(compact, "type") != "response.completed" || bpsGatewayWSResponseID(t, compact) != "resp_compacted" {
		t.Fatalf("compact response failed: %s", compact)
	}
	bpsGatewayWSSend(t, conn, `{"type":"response.create","stream_id":"compact_lane","model":"gpt-6-astra","previous_response_id":"resp_compacted","input":[{"role":"user","content":"new-question-after-compaction"}]}`)
	next := bpsGatewayWSUntilTerminal(t, conn, "compact_lane")
	if bpsGatewayString(next, "type") != "response.completed" {
		t.Fatalf("post-compaction continuation failed: %s", next)
	}
	body := <-captured
	if !bytes.Contains(body, []byte(`"encrypted_content":"opaque-past-state"`)) || !bytes.Contains(body, []byte("new-question-after-compaction")) || bytes.Contains(body, []byte("old-question-to-be-compressed")) || bytes.Contains(body, []byte(`"type":"compaction_trigger"`)) {
		t.Fatalf("compacted history reconstructed incorrectly: %s", body)
	}
	var parsed struct{ Input []map[string]json.RawMessage }
	_ = json.Unmarshal(body, &parsed)
	for _, item := range parsed.Input {
		if bpsGatewayString(item, "type") == "compaction" && item["id"] != nil {
			t.Fatalf("compaction item acquired a fabricated id: %s", item)
		}
	}
}
