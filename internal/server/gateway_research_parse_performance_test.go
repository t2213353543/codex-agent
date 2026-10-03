package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResearchParseProductionWSLargeDoneKeepsRawAndLane(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "parse-large", nil)
	s.client.SetReadLimit(maxBody)
	s.send(t, `{"type":"response.create","model":"fixture","stream_id":"main","input":[]}`)
	s.forwarded(t)
	arguments := strings.Repeat("<>&x", 1<<18)
	done := fmt.Sprintf(`{"type":"response.output_item.done","stream_id":"main","response_id":"parse-main","output_index":0,"item":{"type":"function_call","id":"fc_parse","call_id":"call_parse","name":"inspect","arguments":%q,"encrypted_function_args":["opaque+/="],"future":{"Type":"opaque","large":9007199254740993}},"Type":{"extension":true}}`, arguments)
	s.event(t, done) // Writes through both real WS hops and compares exact bytes.
	if s.p.inFlight("test") != 1 || len(s.p.Metrics.RecentForAccount("test", 10)) != 0 {
		t.Fatal("large nonterminal output prematurely consumed pending work")
	}
	s.event(t, `{"type":"response.completed","stream_id":"main","response":{"id":"parse-main","status":"completed","output":[]}}`)
	if s.p.inFlight("test") != 0 {
		t.Fatal("shared envelope parse stranded the completed lane")
	}
	if rec, ok := s.p.Metrics.LastFor("test"); !ok || rec.Status != "ok" || rec.ResponseID != "parse-main" {
		t.Fatal("large raw event or unknown extension corrupted terminal metadata")
	}
}

func TestResearchParseHTTPNormalEventStillBindsBeforeForeignFailure(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := withGatewayCooldownModel(t.Context(), "fixture")
	ctx = context.WithValue(ctx, gatewayCooldownAccountContextKey{}, account)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	res := &http.Response{Request: req}
	var identity gatewayHTTPCooldownIdentity
	frames := [][]byte{
		[]byte(`{"type":"response.output_text.delta","response_id":"original","delta":"token"}`),
		[]byte(`{"type":"error","response_id":"foreign","status":429,"error":{"code":"rate_limit_exceeded"}}`),
	}
	for _, raw := range frames {
		original := append([]byte(nil), raw...)
		p.observeGatewayCooldownEvent(res, raw, &identity)
		if !bytes.Equal(raw, original) {
			t.Fatal("HTTP observation modified forwarded bytes")
		}
	}
	if !identity.known || p.checkGatewayCooldown(ctx, account) != nil {
		t.Fatal("normal delta stopped binding identity before a foreign rate failure")
	}
	p.observeGatewayCooldownEvent(res, []byte(`{"type":"error","response_id":"original","status":429,"error":{"code":"rate_limit_exceeded"}}`), &identity)
	if p.checkGatewayCooldown(ctx, account) == nil {
		t.Fatal("own structured rate failure lost cooldown")
	}
}

func TestResearchParseInvalidEnvelopeAndUTF8DoNotBecomeUsable(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`null`), []byte(`[]`), []byte(`{"type":`)} {
		if _, err := gatewayWSParseFields(raw); err == nil {
			t.Errorf("invalid event object accepted: %q", raw)
		}
	}
	invalid := []byte("{\"type\":\"response.completed\",\"response\":{\"id\":\"bad\xff\"}}")
	if _, _, err := gatewayWSTerminalResponse(invalid); err == nil {
		t.Fatal("invalid UTF-8 terminal became usable")
	}
	fields, err := gatewayWSParseFields([]byte(`{"type":"error","Type":{"unknown":true},"response":{"id":"r"},"unknown":{"n":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var response json.RawMessage
	if gatewayWSDecodeFields(fields, map[string]any{"type": &kind, "response": &response}) != nil || kind != "error" || string(response) != `{"id":"r"}` {
		t.Fatal("shared decoding lost exact-field semantics")
	}
}

func TestResearchParseObservationViewLeavesSharedEnvelopeUnchanged(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.completed","Type":"error","response":{"id":"r","ID":"shadow","status":"completed","Status":"failed","output":[]},"future":{"opaque":9007199254740993}}`,
		`{"type":"response.failed","response":{"id":"r","Status":"completed","error":{"message":"original","Type":"opaque"}},"Type":"response.completed","param":"keep"}`,
	} {
		payload := []byte(raw)
		fields, err := gatewayWSParseFields(payload)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(fields)
		responseBefore, terminalBefore, errorBefore := gatewayWSTerminalFields(payload, fields)
		view := gatewayWSObservationFields(payload, fields)
		after, _ := json.Marshal(fields)
		responseAfter, terminalAfter, errorAfter := gatewayWSTerminalFields(payload, fields)
		if !bytes.Equal(before, after) || string(payload) != raw {
			t.Fatal("metrics view changed shared envelope or original bytes")
		}
		if bytes.Equal(view, payload) || !bytes.Equal(responseBefore, responseAfter) || terminalBefore != terminalAfter || fmt.Sprint(errorBefore) != fmt.Sprint(errorAfter) {
			t.Fatal("observation projection changed subsequent terminal/failure classification")
		}
		if failure, ok := errorAfter.(*upstreamResponseError); ok && string(failure.Response) != string(fields["response"]) {
			t.Fatal("original failure response was replaced by the metrics projection")
		}
	}
}

func TestResearchParseNonterminalPoisonStatusDoesNotBecomeRateFailure(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	turn := &gatewayWSTurn{rec: RequestRecord{Model: "fixture"}, started: time.Now(), idleTimeout: time.Minute}
	turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: map[string][]*gatewayWSTurn{"": {turn}}, count: 1}
	for _, raw := range []string{
		`{"type":"response.output_text.delta","response_id":"nonterminal","delta":"token","status":429,"error":{"code":"rate_limit_exceeded"},"Type":"error"}`,
		`{"type":"response.output_text.delta","response_id":"nonterminal","delta":"token","status":{"future":true},"error":"not a rate object"}`,
	} {
		if turns.observeFrame(t.Context(), []byte(raw), account) {
			t.Fatal("nonterminal metadata became connection error")
		}
		if turns.count != 1 || turn.responseID != "nonterminal" || p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "fixture"), account) != nil {
			t.Fatal("rate-details fast rejection changed nonterminal attribution/cooldown")
		}
	}
}

// Benchmark the callback chains invoked by gateway.go and the WS upstream
// pump, including identity, cooldown, metrics and terminal classification.
// Network, TLS and model execution are intentionally outside this measurement.
func BenchmarkResearchParseProductionObservers(b *testing.B) {
	for _, scenario := range []struct {
		name string
		ws   bool
		body []byte
	}{
		{"SSETokenDelta", false, []byte(`{"type":"response.output_text.delta","response_id":"resp_bench","item_id":"msg_bench","output_index":0,"content_index":0,"delta":"token"}`)},
		{"WSTokenDelta", true, []byte(`{"type":"response.output_text.delta","response_id":"resp_bench","item_id":"msg_bench","output_index":0,"content_index":0,"delta":"token"}`)},
		{"WSToolDone1MiB", true, []byte(fmt.Sprintf(`{"type":"response.output_item.done","response_id":"resp_bench","output_index":0,"item":{"type":"function_call","id":"fc_bench","call_id":"call_bench","name":"inspect","arguments":%q,"encrypted_function_args":["synthetic-opaque"]}}`, strings.Repeat("x", 1<<20)))},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			p := &Proxy{}
			account := Account{ID: "bench-account", AccountID: "bench-workspace"}
			ctx := context.WithValue(context.Background(), gatewayCooldownAccountContextKey{}, account)
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/responses", nil)
			if err != nil {
				b.Fatal(err)
			}
			response := &http.Response{Request: request}
			var identity gatewayHTTPCooldownIdentity
			record := RequestRecord{Endpoint: "responses", ResponseID: "resp_bench"}
			_, attempts := withGatewayAttemptObservation(ctx)
			observation := gatewayObservation{record: &record, started: time.Now()}
			turn := &gatewayWSTurn{rec: record, started: time.Now(), responseID: "resp_bench", idleTimeout: time.Minute}
			turns := gatewayWSTurns{proxy: p, keyID: "bench", pending: map[string][]*gatewayWSTurn{"": {turn}}, count: 1}
			b.SetBytes(int64(len(scenario.body)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if scenario.ws {
					if turns.observeFrame(ctx, scenario.body, account) {
						b.Fatal("nonterminal event became terminal")
					}
				} else {
					attempts.ObserveReturned(scenario.body)
					observation.event(scenario.body)
					p.observeGatewayCooldownEvent(response, scenario.body, &identity)
				}
			}
			b.StopTimer()
			if scenario.ws && (turns.count != 1 || turn.responseID != "resp_bench") {
				b.Fatal("observer changed pending identity")
			}
		})
	}
}
