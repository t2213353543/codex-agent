package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfficialEncryptedReasoningReplayPreservesIdentitySemantics(t *testing.T) {
	thread := &officialThread{}
	input := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"exact"}],"encrypted_content":"cipher-a","extra":{"exact":9007199254740993}}`),
		json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"cipher-b"}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_server-original","summary":[],"encrypted_content":"cipher-c"}`),
		json.RawMessage(`{"type":"reasoning","id":"legacy-original","summary":[],"encrypted_content":"cipher-d"}`),
		json.RawMessage(`{"type":"reasoning","summary":[]}`),
	}
	first, err := thread.injectItems(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := thread.injectItems(input)
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]bool{}
	for i, raw := range first {
		if !officialJSONEqual(raw, second[i]) {
			t.Fatal("legacy identity changes on replay")
		}
		if i >= 2 {
			if !officialJSONEqual(raw, input[i]) {
				t.Fatalf("provided identity or unencrypted item was modified: %s", raw)
			}
			continue
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		var id string
		_ = json.Unmarshal(fields["id"], &id)
		if id == "" || strings.Contains(id, "_") || identities[id] {
			t.Fatalf("internal identity would be forwarded or collide: %q", id)
		}
		identities[id] = true
		// This is the official prepare_response_items_for_request rule: an
		// unprefixed local legacy ID is omitted when writing the model request.
		delete(fields, "id")
		wire, _ := json.Marshal(fields)
		if !officialJSONEqual(wire, input[i]) {
			t.Fatalf("wire replay did not preserve missing ID/cipher/summary: %s", wire)
		}
	}
}
