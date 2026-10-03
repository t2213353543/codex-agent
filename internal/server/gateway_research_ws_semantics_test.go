package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// These request shapes follow CLI 0.155 core/client.rs: warmup generate=false,
// an unchanged logical input becomes previous_response_id + input=[], and a
// later tool result is an append-only delta. stream_id is a gateway extension.
func TestResearchWSSemanticsCanonicalWarmupEmptyDeltaAndLanes(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "research-canonical", func(settings *Settings) {
		settings.PromptCacheKeyEnabled = false
		settings.CompatibilityNormalizationEnabled = false
	})
	requests := []string{
		`{"type":"response.create","model":"fixture","instructions":"stable","stream_id":"main","generate":false,"input":[{"type":"reasoning","id":"rs_prior","encrypted_content":"opaque-prior+/=","summary":[]}],"include":["reasoning.encrypted_content"],"client_metadata":{"session_id":"research-canonical","thread_id":"thread-main","turn_id":"warmup"}}`,
		`{"type":"response.create","model":"fixture","instructions":"stable","stream_id":"main","previous_response_id":"resp_warmup","input":[],"include":["reasoning.encrypted_content"],"client_metadata":{"session_id":"research-canonical","thread_id":"thread-main","turn_id":"first","x-codex-turn-state":"opaque-turn-state"}}`,
		`{"type":"response.create","model":"fixture","instructions":"stable","stream_id":"main","previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_main","output":"saved"},{"type":"reasoning","encrypted_content":"opaque-next+/=","summary":[],"future":{"large":9007199254740993,"empty":null}}],"client_metadata":{"session_id":"research-canonical","thread_id":"thread-main","turn_id":"second"}}`,
	}
	s.send(t, requests[0])
	if got := string(s.forwarded(t)); got != requests[0] {
		t.Fatal("canonical warmup was rewritten")
	}
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_warmup","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 0 {
		t.Fatal("completed warmup retained admission")
	}
	s.send(t, requests[1])
	if got := string(s.forwarded(t)); got != requests[1] {
		t.Fatal("empty delta was expanded, rewritten or lost its previous response ID")
	}
	const side = `{"type":"response.create","model":"fixture","stream_id":"side","input":[],"client_metadata":{"thread_id":"thread-side"}}`
	s.send(t, side)
	if got := string(s.forwarded(t)); got != side {
		t.Fatal("independent lane was rewritten")
	}
	s.event(t, `{"type":"response.output_item.done","stream_id":"main","response_id":"resp_first","output_index":0,"item":{"id":"fc_main","type":"function_call","call_id":"call_main","name":"inspect","arguments":"{}","encrypted_function_args":["opaque-call"],"namespace":"functions"}}`)
	s.event(t, `{"type":"response.output_item.done","stream_id":"main","response_id":"resp_first","output_index":1,"item":{"id":"msg_main","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Inspecting."}]}}`)
	// Full done frames must pass unchanged even when terminal output is slim.
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_first","status":"completed","output":[{"id":"msg_main","type":"message"}]}}`)
	if s.p.inFlight("test") != 1 {
		t.Fatal("main completion consumed the side lane")
	}
	s.send(t, requests[2])
	if got := string(s.forwarded(t)); got != requests[2] {
		t.Fatal("tool-result/opaque history delta was changed")
	}
	// A late old terminal is forwarded, but must not consume the new main turn.
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_first","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 2 {
		t.Fatal("late completion consumed a continuation or another lane")
	}
	s.event(t, `{"type":"response.completed","stream_id":"side","response":{"id":"resp_side","status":"completed","output":[]}}`)
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_second","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 0 || len(s.p.Metrics.RecentForAccount("test", 10)) != 4 {
		t.Fatal("canonical multi-turn lane sequence leaked admission or duplicated records")
	}
}

func TestResearchWSSemanticsTypeShadowCannotSkipNextTurnAuthorization(t *testing.T) {
	for _, shadow := range []string{"", `,"Type":"response.cancel"`, `,"TYPE":"ping"`, `,"Type":{"unknown":true}`} {
		t.Run(fmt.Sprintf("shadow_%q", shadow), func(t *testing.T) {
			var dispatched atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					// JSON member names are case sensitive, as in serde-derived
					// CLI wire fields. Unknown Type/TYPE are not aliases of type.
					var fields map[string]json.RawMessage
					if json.Unmarshal(raw, &fields) != nil || string(fields["type"]) != `"response.create"` {
						t.Error("unexpected upstream request kind")
						return
					}
					index := dispatched.Add(1)
					response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[]}}`, index)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
						return
					}
				}
			})
			enableGatewayWS(t, p, "")
			gateway := httptest.NewServer(p)
			defer gateway.Close()
			conn, _ := gatewayWSDial(t, gateway.URL, "research-type-shadow")
			defer conn.CloseNow()
			first := `{"type":"response.create","model":"fixture","generate":false,"input":[],"prompt_cache_key":"research-type-shadow"}`
			if err := conn.Write(t.Context(), websocket.MessageText, []byte(first)); err != nil {
				t.Fatal(err)
			}
			_ = gatewayWSRead(t, conn)
			waitGatewayWSReleased(t, p)
			if err := p.Store.UpdateAccount("test", func(account *Account) error { account.Enabled = false; return nil }); err != nil {
				t.Fatal(err)
			}
			second := `{"type":"response.create","model":"fixture","previous_response_id":"resp_1","input":[],"prompt_cache_key":"research-type-shadow","client_metadata":{"turn_id":"next"}` + shadow + `}`
			if err := conn.Write(t.Context(), websocket.MessageText, []byte(second)); err != nil {
				t.Fatal(err)
			}
			response := gatewayWSRead(t, conn)
			if !strings.Contains(string(response), `"type":"error"`) || !strings.Contains(string(response), `"status":403`) || dispatched.Load() != 1 {
				t.Fatalf("next create bypassed revocation after warmup: dispatched=%d response=%s", dispatched.Load(), response)
			}
		})
	}
}

func TestResearchWSSemanticsResponseShadowKeepsLaneAndSuccess(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "research-event-shadow", func(settings *Settings) {
		settings.PromptCacheKeyEnabled = false
		settings.CompatibilityNormalizationEnabled = false
	})
	const first = `{"type":"response.create","model":"fixture","stream_id":"main","generate":false,"input":[],"Type":{"extension":true},"STREAM_ID":42,"Generate":true,"Model":{"extension":true}}`
	s.send(t, first)
	if got := string(s.forwarded(t)); got != first {
		t.Fatal("unknown request extensions were rewritten or selected as known protocol fields")
	}
	s.event(t, `{"type":"response.created","stream_id":"main","response":{"id":"resp_shadow_first","status":"in_progress","ID":"other"},"Type":false,"STREAM_ID":{"extension":true}}`)
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_shadow_first","status":"completed","ID":"other","Status":"failed","output":[]},"Type":"error","STREAM_ID":"other"}`)
	if s.p.inFlight("test") != 0 {
		t.Fatal("exactly named completed event did not release its lane")
	}
	record, ok := s.p.Metrics.LastFor("test")
	if !ok || record.Status != "ok" || record.TerminalStatus != "completed" || record.ResponseID != "resp_shadow_first" || record.WebSocketWarmup == nil || !*record.WebSocketWarmup {
		t.Fatalf("shadow extension polluted warmup/success metadata: %+v", record)
	}
	const second = `{"type":"response.create","model":"fixture","stream_id":"main","previous_response_id":"resp_shadow_first","input":[]}`
	s.send(t, second)
	if got := string(s.forwarded(t)); got != second {
		t.Fatal("continuation changed after an extended terminal event")
	}
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"resp_shadow_second","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 0 || len(s.p.Metrics.RecentForAccount("test", 10)) != 2 {
		t.Fatal("case-sensitive two-turn continuation leaked or duplicated work")
	}
}

func TestResearchWSSemanticsConflictingResponseIDsDoNotCompleteEitherLane(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "research-conflicting-events", nil)
	for _, lane := range []string{"main", "side"} {
		s.send(t, fmt.Sprintf(`{"type":"response.create","model":"fixture","stream_id":%q,"input":[]}`, lane))
		s.forwarded(t)
		s.event(t, fmt.Sprintf(`{"type":"response.created","stream_id":%q,"response":{"id":%q}}`, lane, "resp_"+lane))
	}
	s.event(t, `{"type":"response.completed","stream_id":"main","response_id":"resp_side","response":{"id":"resp_main","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 2 || len(s.p.Metrics.RecentForAccount("test", 10)) != 0 {
		t.Fatal("contradictory top/nested response identity completed a lane")
	}
	for _, lane := range []string{"side", "main"} {
		s.event(t, fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":%q,"status":"completed","output":[]}}`, lane, "resp_"+lane))
	}
	if s.p.inFlight("test") != 0 || len(s.p.Metrics.RecentForAccount("test", 10)) != 2 {
		t.Fatal("matching terminals did not finish exactly their own lanes")
	}
}

func TestResearchWSSemanticsExactTerminalRetainsOriginalErrorPayload(t *testing.T) {
	const raw = `{"type":"error","code":"invalid_encrypted_content","message":"synthetic","param":"input[0]","Type":"response.completed","future":{"n":9007199254740993}}`
	_, terminal, err := gatewayWSTerminalResponse([]byte(raw))
	failure, ok := err.(*upstreamResponseError)
	if !terminal || !ok || string(failure.Payload) != raw {
		t.Fatal("exact terminal helper stripped upstream error details or opaque extensions")
	}
}

func TestResearchWSSemanticsLaneShadowCannotStrandEmptyContinuation(t *testing.T) {
	for _, shadow := range []string{"", `,"STREAM_ID":"shadow"`} {
		t.Run(fmt.Sprintf("shadow_%q", shadow), func(t *testing.T) {
			var dispatched atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					var fields map[string]json.RawMessage
					if json.Unmarshal(raw, &fields) != nil {
						t.Error("invalid forwarded JSON")
						return
					}
					index := dispatched.Add(1)
					response := fmt.Sprintf(`{"type":"response.completed","stream_id":%s,"response":{"id":"resp_%d","status":"completed","output":[]}}`, fields["stream_id"], index)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(response)); err != nil {
						return
					}
				}
			})
			enableGatewayWS(t, p, "")
			if err := p.Store.UpdateAccount("test", func(account *Account) error { account.Concurrency = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			gateway := httptest.NewServer(p)
			defer gateway.Close()
			conn, _ := gatewayWSDial(t, gateway.URL, "research-lane-shadow")
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			first := `{"type":"response.create","model":"fixture","stream_id":"main","generate":false,"input":[],"prompt_cache_key":"research-lane-shadow"` + shadow + `}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(first)); err != nil {
				t.Fatal(err)
			}
			if firstResponse := string(gatewayWSRead(t, conn)); !strings.Contains(firstResponse, `"id":"resp_1"`) {
				t.Fatalf("warmup did not complete: %s", firstResponse)
			}
			if active := p.inFlight("test"); active != 0 {
				t.Errorf("completed lowercase lane stranded %d admission slot", active)
			}
			second := `{"type":"response.create","model":"fixture","stream_id":"main","previous_response_id":"resp_1","input":[],"prompt_cache_key":"research-lane-shadow"}`
			if err := conn.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
				t.Fatal(err)
			}
			response := gatewayWSRead(t, conn)
			if !strings.Contains(string(response), `"id":"resp_2"`) || dispatched.Load() != 2 {
				t.Fatalf("valid empty continuation was blocked by mismatched lane: dispatched=%d response=%s", dispatched.Load(), response)
			}
		})
	}
}
