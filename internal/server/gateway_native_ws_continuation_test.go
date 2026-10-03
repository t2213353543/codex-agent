package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

// The real CLI may add a user suffix after prewarm. This separate native TLS
// fixture covers the legal empty delta without claiming the CLI emitted it.
func TestGatewayNativeActualWebSocketEmptyDeltaAndOpaqueItems(t *testing.T) {
	requests := []string{
		`{"type":"response.create","model":"fixture","generate":false,"prompt_cache_key":"native-empty","instructions":"stable","input":[{"type":"reasoning","encrypted_content":"opaque+/=","unknown":{"exact":9007199254740993}}]}`,
		`{"type":"response.create","model":"fixture","previous_response_id":"native-empty-0","prompt_cache_key":"native-empty","instructions":"stable","input":[],"client_metadata":{"x-codex-turn-state":"opaque-turn-state"}}`,
		`{"type":"response.create","model":"fixture","previous_response_id":"native-empty-1","prompt_cache_key":"native-empty","instructions":"stable","input":[{"type":"function_call_output","call_id":"native-call","output":"result"},{"type":"custom_tool_call_output","call_id":"native-custom","output":[{"type":"input_text","text":"custom-result"}]},{"type":"reasoning","encrypted_content":"opaque-next+/=","unknown":{"exact":9007199254740993,"nil":null}}],"unknown":{"exact":9007199254740993,"nil":null},"client_metadata":{"x-codex-turn-state":"opaque-turn-state"}}`,
	}
	var frames atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for index, expected := range requests {
			// This native transport fixture still compares every byte of the
			// empty delta, encrypted items, unknown integers and metadata. Only
			// the documented cache-key scalar projection is expected to change.
			expected = strings.Replace(expected, `"prompt_cache_key":"native-empty"`,
				`"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "native-empty")+`"`, 1)
			typ, actual, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText || string(actual) != expected {
				t.Error("native transport altered empty delta, opaque history or unknown JSON fields")
			}
			frames.Add(1)
			output := `[]`
			if index == 1 {
				output = `[{"type":"function_call","call_id":"native-call","name":"synthetic","arguments":"{}"},{"type":"custom_tool_call","call_id":"native-custom","name":"synthetic_custom","input":"opaque"}]`
			}
			response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"native-empty-%d","status":"completed","output":%s}}`, index, output)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	})
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "native-empty")
	defer conn.CloseNow()
	for index, request := range requests {
		if err := conn.Write(t.Context(), websocket.MessageText, []byte(request)); err != nil {
			t.Fatal(err)
		}
		if response := gatewayWSRead(t, conn); !strings.Contains(string(response), fmt.Sprintf(`"id":"native-empty-%d"`, index)) {
			t.Fatal("native empty-delta response changed")
		}
		waitGatewayWSReleased(t, p)
		record, ok := p.Metrics.LastFor("test")
		if !ok || record.WebSocketWarmup == nil || *record.WebSocketWarmup != (index == 0) || record.UsageKnown != nil {
			t.Fatal("native continuation lost warmup classification or invented usage")
		}
	}
	if frames.Load() != 3 || guard.calls.Load() != 0 || p.inFlight("test") != 0 {
		t.Fatal("native continuation replayed, fell back or leaked admission")
	}
}
