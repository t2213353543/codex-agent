package bps

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func cacheTestInput(t *testing.T, source string, turn string) []json.RawMessage {
	t.Helper()
	prepared, err := Prepare([]byte(source), Options{TaskID: "same-task", TurnID: turn})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input
}

func TestPreparedToolsKeepStablePrefixAcrossTurns(t *testing.T) {
	source := `{"model":"m","instructions":"stable instructions","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"value":{"const":9007199254740993,"type":"integer"}}}}],"input":[{"role":"user","content":"` + strings.Repeat("stable history ", 4000) + `"}]}`
	first := cacheTestInput(t, source, "turn-one")
	second := cacheTestInput(t, source, "turn-two")
	if len(first) != len(second) {
		t.Fatal("model input length changed")
	}
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("same tools/history changed model prefix at item %d", i)
		}
	}
}

func TestPreparedToolsIdentityRetainsCallerIDsAndSchemaPrecision(t *testing.T) {
	tool := `[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"n":{"const":9007199254740993,"type":"integer"}}}}]`
	ordered := `[{"parameters":{"properties":{"n":{"type":"integer","const":9007199254740993}},"type":"object"},"name":"inspect","type":"function"}]`
	get := func(definitions string) map[string]json.RawMessage {
		items := cacheTestInput(t, `{"model":"m","tools":`+definitions+`,"input":[]}`, "turn")
		var item map[string]json.RawMessage
		_ = json.Unmarshal(items[0], &item)
		return item
	}
	first, reordered := get(tool), get(ordered)
	if !bytes.Equal(first["id"], reordered["id"]) {
		t.Fatal("tool object-key serialization changed generated identity")
	}
	if !bytes.Contains(first["tools"], []byte("9007199254740993")) || !bytes.Equal(first["tools"], []byte(tool)) || !bytes.Equal(reordered["tools"], []byte(ordered)) {
		t.Fatal("tool schema bytes/numbers rewritten")
	}
	changed := get(strings.ReplaceAll(tool, "9007199254740993", "9007199254740992"))
	if bytes.Equal(first["id"], changed["id"]) {
		t.Fatal("different integer schemas share identity")
	}
	client := cacheTestInput(t, `{"model":"m","tools":`+tool+`,"input":[{"type":"additional_tools","id":"at_client_owned","role":"developer","tools":`+tool+`}]}`, "turn")
	if len(client) != 2 || !bytes.Contains(client[0], []byte(`"id":"at_client_owned"`)) {
		t.Fatal("caller tool declaration duplicated or identity changed")
	}
}
