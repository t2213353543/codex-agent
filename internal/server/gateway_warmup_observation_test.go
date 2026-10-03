package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func TestGatewayWarmupObservationExplicitAndUnknown(t *testing.T) {
	for _, test := range []struct {
		name, requestType, payload string
		want                       *bool
	}{
		{"warmup", "websocket", `{"type":"response.create","generate":false}`, boolPointerWarmup(true)},
		{"generate", "websocket", `{"type":"response.create","generate":true}`, boolPointerWarmup(false)},
		{"default", "websocket", `{"type":"response.create"}`, boolPointerWarmup(false)},
		{"whitespace_boolean", "websocket", "{\"type\":\"response.create\",\"generate\": \n false\t}", boolPointerWarmup(true)},
		{"null", "websocket", `{"type":"response.create","generate":null}`, nil},
		{"string", "websocket", `{"type":"response.create","generate":"false"}`, nil},
		{"number", "websocket", `{"type":"response.create","generate":0}`, nil},
		{"object", "websocket", `{"type":"response.create","generate":{}}`, nil},
		{"array", "websocket", `{"type":"response.create","generate":[]}`, nil},
		{"other_event", "websocket", `{"type":"response.cancel","generate":false}`, nil},
		{"missing_type", "websocket", `{"generate":false}`, nil},
		{"invalid_type", "websocket", `{"type":false,"generate":false}`, nil},
		{"http_json", "http_json", `{"type":"response.create","generate":false}`, nil},
		{"http_sse", "http_sse", `{"type":"response.create","generate":false}`, nil},
		{"compact", "compact", `{"type":"response.create","generate":false}`, nil},
		{"invalid_json", "websocket", `invalid`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			before := bytes.Clone(payload)
			rec := RequestRecord{WebSocketWarmup: boolPointerWarmup(true), InputTokens: 11, OutputTokens: 7, CachedTokens: 3}
			observeGatewayRequest(&rec, "fixture-client", test.requestType, payload)
			if !bytes.Equal(payload, before) || rec.RequestType != test.requestType || rec.InputTokens != 11 || rec.OutputTokens != 7 || rec.CachedTokens != 3 || rec.TTFTMs != nil {
				t.Fatal("passive warmup observation changed payload, type, tokens or TTFT")
			}
			if (rec.WebSocketWarmup == nil) != (test.want == nil) || rec.WebSocketWarmup != nil && *rec.WebSocketWarmup != *test.want {
				t.Fatal("warmup presence/value does not match explicit WS create semantics")
			}
		})
	}
}

func boolPointerWarmup(value bool) *bool { return &value }

func TestGatewayWarmupObservationJSONRoundTripAndPrivacy(t *testing.T) {
	for _, value := range []*bool{nil, boolPointerWarmup(false), boolPointerWarmup(true)} {
		rec := RequestRecord{RequestType: "websocket", WebSocketWarmup: value}
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("invalid record JSON")
		}
		_, present := object["websocket_warmup"]
		if present != (value != nil) {
			t.Fatal("JSON omitted explicit false or invented old unknown warmup")
		}
		var decoded RequestRecord
		if json.Unmarshal(raw, &decoded) != nil || (decoded.WebSocketWarmup == nil) != (value == nil) || value != nil && *decoded.WebSocketWarmup != *value {
			t.Fatal("warmup did not survive JSON round trip")
		}
	}
	var rec RequestRecord
	payload := []byte(`{"type":"response.create","generate":false,"input":[{"type":"reasoning","encrypted_content":"PRIVATE_ENCRYPTED"},{"type":"function_call_output","call_id":"PRIVATE_ID","output":"PRIVATE_OUTPUT"}],"instructions":"PRIVATE_PROMPT","client_metadata":{"state":"PRIVATE_STATE"}}`)
	observeGatewayRequest(&rec, "", "websocket", payload)
	raw, _ := json.Marshal(rec)
	if bytes.Contains(raw, []byte("PRIVATE_")) {
		t.Fatal("warmup observer retained conversation/tool/state content")
	}
}

func TestGatewayWarmupObservationWSKeepsFramesAndAccounting(t *testing.T) {
	requests := []string{
		`{"type":"response.create","model":"fixture","generate":false,"prompt_cache_key":"warmup-observation","input":[]}`,
		`{"type":"response.create","model":"fixture","previous_response_id":"warmup-0","prompt_cache_key":"warmup-observation","input":[]}`,
		`{"type":"response.create","model":"fixture","generate":true,"previous_response_id":"warmup-1","prompt_cache_key":"warmup-observation","input":[]}`,
		`{"type":"response.create","model":"fixture","generate":null,"previous_response_id":"warmup-2","prompt_cache_key":"warmup-observation","input":[]}`,
	}
	var count atomic.Int32
	var scopedCacheKey string
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for index, expected := range requests {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			wireKey := policyString(t, raw, "prompt_cache_key")
			if index == 0 {
				scopedCacheKey = wireKey
			}
			if len(wireKey) != 36 || wireKey != scopedCacheKey {
				t.Error("warmup/continuation did not retain the account-scoped cache partition")
			}
			// Cache isolation is an intentional gateway transform. Restoring that
			// one scalar must recover every other original request byte.
			quotedKey, _ := json.Marshal(wireKey)
			restored := bytes.Replace(raw, quotedKey, []byte(`"warmup-observation"`), 1)
			if string(restored) != expected {
				t.Error("warmup observation changed WS bytes outside the scoped cache key")
			}
			count.Add(1)
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"warmup-%d","status":"completed","output":[],"usage":{"input_tokens":6,"output_tokens":0}}}`, index)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(terminal)); err != nil {
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "warmup-observation")
	defer conn.CloseNow()
	for index, request := range requests {
		if err := conn.Write(t.Context(), websocket.MessageText, []byte(request)); err != nil {
			t.Fatal(err)
		}
		response := gatewayWSRead(t, conn)
		if !strings.Contains(string(response), fmt.Sprintf(`"id":"warmup-%d"`, index)) {
			t.Fatal("warmup observation changed terminal response")
		}
		waitGatewayWSReleased(t, p)
		rec, ok := p.Metrics.LastFor("test")
		if !ok || rec.RequestType != "websocket" || rec.Status != "ok" || rec.InputTokens != 6 || rec.OutputTokens != 0 || rec.TTFTMs != nil || rec.UsageKnown == nil || !*rec.UsageKnown {
			t.Fatal("warmup observer altered usage/outcome or invented output")
		}
		if index == 3 {
			if rec.WebSocketWarmup != nil {
				t.Fatal("null generate became a known warmup value")
			}
		} else if rec.WebSocketWarmup == nil || *rec.WebSocketWarmup != (index == 0) {
			t.Fatal("actual WS turn lost warmup provenance")
		}
	}
	rows := p.Metrics.RecentForAccount("test", 10)
	var tokens int64
	for _, row := range rows {
		tokens += row.InputTokens + row.OutputTokens
	}
	if len(rows) != 4 || count.Load() != 4 || tokens != 24 || p.inFlight("test") != 0 {
		t.Fatal("warmup metadata changed request cardinality, tokens or admission")
	}
}
