package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"codex-agent/internal/bps"
)

func TestBPSOpaqueOwnerLiveClientSerializationBoundary(t *testing.T) {
	// Synthetic only: no provider, account, saved history or real ciphertext.
	compact := json.RawMessage(`{
  "type": "compaction",
  "id": "cmp_synthetic",
  "encrypted_content": "synthetic<&>\\cipher+exact==",
  "synthetic_large_number": 9007199254740993
}`)
	history := []json.RawMessage{compact, json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply OK"}]}`)}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": history, "tools": []any{}, "tool_choice": "none"})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := bpsOpaqueOwnerClientInput(body, history)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(compact, expected[0]) {
		t.Fatal("synthetic case did not exercise original raw comparison failure")
	}
	if !bpsOpaqueOwnerCompactionCipherMatches(history[:1], expected[:1]) {
		t.Fatal("JSON whitespace and escaping must preserve decoded compact cipher")
	}
	for _, changed := range []json.RawMessage{
		json.RawMessage(`{"type":"compaction","id":"cmp_synthetic","encrypted_content":"WRONG"}`),
		json.RawMessage(`{"type":"compaction","id":"cmp_other","encrypted_content":"synthetic<&>\\cipher+exact=="}`),
		json.RawMessage(`{"type":"reasoning","id":"cmp_synthetic","encrypted_content":"synthetic<&>\\cipher+exact=="}`),
	} {
		if bpsOpaqueOwnerCompactionCipherMatches(history[:1], []json.RawMessage{changed}) {
			t.Fatal("upstream/public cipher, ID and type must not change")
		}
	}
	prepared, err := bps.Prepare(body, bps.Options{TaskID: "task-synthetic", TurnID: "turn-synthetic", SearchDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(prepared.Body, &actual) != nil || len(actual.Input) < len(expected) {
		t.Fatal("prepared input unavailable")
	}
	for i := range expected {
		if !bytes.Equal(actual.Input[i], expected[i]) {
			t.Fatal("actual client input bytes changed")
		}
	}
	clientPrefix, _ := json.Marshal(map[string]any{"input": actual.Input[:len(expected)]})
	if _, err := bpsOpaqueOwnerClientInput(clientPrefix, history); err != nil {
		t.Fatal("decoded original cipher changed")
	}
	if !bytes.Contains(actual.Input[0], []byte("9007199254740993")) {
		t.Fatal("large number precision lost")
	}
	var changed map[string]json.RawMessage
	_ = json.Unmarshal(body, &changed)
	changed["input"] = json.RawMessage(`[{"type":"compaction","id":"cmp_synthetic","encrypted_content":"WRONG"},{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply OK"}]}]`)
	bad, _ := json.Marshal(changed)
	if _, err := bpsOpaqueOwnerClientInput(bad, history); err == nil || err.Error() != "client_cipher_changed" {
		t.Fatal("changed cipher must still fail")
	}
	t.Log("original RawMessage differs after client serialization; serialized input preserved by Prepare; decoded cipher and large integer unchanged; changed cipher rejected; provider_calls=0")
}
