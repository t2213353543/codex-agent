package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func appServerCostTestCanonical(thread, turn string) []byte {
	metadata, _ := json.Marshal(map[string]string{"thread_id": thread, "turn_id": turn})
	body, _ := json.Marshal(map[string]any{"client_metadata": map[string]string{"x-codex-turn-metadata": string(metadata)}, "input": []string{"NOT_STORED_PRIVATE_PROMPT"}})
	return body
}

func TestAppServerTurnCostObservationUsesActualSentIDsAndOnlyCompletion(t *testing.T) {
	o := newAppServerCostObservation(appServerCostTestCanonical("client-thread", "client-turn"), nil)
	if o == nil || o.ClientTurn != "client-turn" {
		t.Fatal("client billing identity absent")
	}
	o.Owner = "owner"
	o.BindSent([]byte(`{"generate":false}`), nil)
	o.BindSent(appServerCostTestCanonical("upstream-thread", "upstream-turn"), nil)
	o.ObserveResponse([]byte(`{"type":"response.created","response":{"id":"not-complete"}}`))
	if _, known := o.Mapping(); known {
		t.Fatal("uncompleted response counted")
	}
	for _, id := range []string{"r2", "r1", "r2"} {
		raw, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]string{"id": id}})
		o.ObserveResponse(raw)
	}
	value, known := o.Mapping()
	if !known || value.ThreadID != "upstream-thread" || value.TurnID != "upstream-turn" || !reflect.DeepEqual(value.ResponseIDs, []string{"r1", "r2"}) {
		t.Fatalf("wrong provenance %+v", value)
	}
	ctx := withAppServerCostObservation(context.Background(), o)
	if appServerCostObservationFrom(ctx) != o {
		t.Fatal("request context tracker lost")
	}
}

func TestAppServerTurnCostObservationCanonicalPrecedenceAndMissing(t *testing.T) {
	headers := http.Header{"X-Codex-Turn-Metadata": {`{"thread_id":"header-thread","turn_id":"header-turn"}`}, "x-codex-turn-metadata": {`{"turn_id":"shadow"}`}}
	if o := newAppServerCostObservation(appServerCostTestCanonical("body-thread", "body-turn"), headers); o == nil || o.ClientTurn != "body-turn" {
		t.Fatal("body canonical did not win")
	}
	if o := newAppServerCostObservation([]byte(`{}`), headers); o == nil || o.ClientTurn != "header-turn" {
		t.Fatal("header canonical scalar lost")
	}
	if o := newAppServerCostObservation([]byte(`{"client_metadata":{"thread_id":"flat-thread","turn_id":"flat-turn"}}`), nil); o == nil || o.ClientTurn != "flat-turn" {
		t.Fatal("flat native IDs lost")
	}
	headers.Set("Connection", "X-Codex-Turn-Metadata")
	if newAppServerCostObservation([]byte(`{}`), headers) != nil {
		t.Fatal("hop-only turn identity admitted")
	}
	for _, raw := range []string{`{}`, `{"metadata":{"turn_id":"unsupported-field"}}`, `{"client_metadata":{"x-codex-turn-metadata":"broken","turn_id":"must-not-fallback"}}`, `{"client_metadata":{"turn_id":42}}`} {
		if newAppServerCostObservation([]byte(raw), nil) != nil {
			t.Fatalf("invented billing association %s", raw)
		}
	}
}

func TestAppServerTurnCostObservationFailsClosedOnSentIdentityChange(t *testing.T) {
	o := newAppServerCostObservation(appServerCostTestCanonical("thread", "turn"), nil)
	o.Owner = "owner"
	o.BindSent(appServerCostTestCanonical("sent-thread", "sent-turn"), nil)
	o.ObserveResponse([]byte(`{"type":"response.completed","response":{"id":"r"}}`))
	o.BindSent(appServerCostTestCanonical("other-thread", "sent-turn"), nil)
	if _, known := o.Mapping(); known {
		t.Fatal("mixed upstream thread attributed to client turn")
	}
	var absent *appServerCostObservation
	absent.BindSent(nil, nil)
	absent.ObserveResponse(nil)
	if _, known := absent.Mapping(); known {
		t.Fatal("nil tracker became known")
	}
}

func TestAppServerTurnCostWebSocketPreservesClientThreadBeforeScopeRewrite(t *testing.T) {
	inbound := appServerCostTestCanonical("client-thread", "client-turn")
	sent := appServerCostTestCanonical("scoped-thread", "scoped-turn")
	initial := newAppServerCostObservation(sent, nil)
	initial.Owner = "owner"
	turn := &gatewayWSTurn{rec: RequestRecord{turnCostObservation: initial}}
	turns := gatewayWSTurns{pending: map[string][]*gatewayWSTurn{"": {turn}}}
	turns.observeAttemptStart(inbound, sent, "/v1/responses", nil)
	observed := turn.rec.turnCostObservation
	if observed == nil || observed.ClientThread != "client-thread" || observed.ClientTurn != "client-turn" || observed.ThreadID != "scoped-thread" || observed.TurnID != "scoped-turn" || observed.Owner != "owner" {
		t.Fatalf("client and wire billing identities mixed: %+v", observed)
	}
}
