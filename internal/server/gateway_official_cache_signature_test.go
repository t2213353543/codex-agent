package server

import (
	"context"
	"encoding/json"
	"testing"
)

func TestOfficialCacheThreadSurvivesEquivalentToolSchemaSerialization(t *testing.T) {
	original := officialResponseInput{Model: "fixture", Instructions: "same instructions", Tools: []json.RawMessage{json.RawMessage(`{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"n":{"const":9007199254740993,"type":"integer"}}}}`)}}
	next := original
	next.Tools = []json.RawMessage{json.RawMessage(`{"parameters":{"properties":{"n":{"type":"integer","const":9007199254740993}},"type":"object"},"name":"inspect","type":"function"}`)}
	signature, err := officialResponseSignature(original)
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "owner", AccountID: "workspace", AccessToken: "fixture-token"}
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"previous question"}`)}
	thread := &officialThread{rpc: &officialRPC{}, id: "original-thread", signature: signature, credentials: officialCredentialDigest(account), history: history}
	rt := &officialRuntime{threads: map[string]*officialThread{"key\x00" + gatewayAccountRef(account) + "\x00session": thread}}
	items := append(append([]json.RawMessage{}, history...), json.RawMessage(`{"role":"user","content":"followup"}`))
	got, delta, err := rt.responseThread(context.Background(), "key", "session", account, nil, next, items)
	if err != nil {
		t.Fatal(err)
	}
	defer got.mu.Unlock()
	if got != thread || len(delta) != 1 || got.id != "original-thread" {
		t.Fatal("equivalent schema recreated official thread")
	}
	changed := next
	changed.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"n":{"const":9007199254740992,"type":"integer"}}}}`)}
	different, err := officialResponseSignature(changed)
	if err != nil || different == signature {
		t.Fatal("different schema or integer was collapsed")
	}
}
