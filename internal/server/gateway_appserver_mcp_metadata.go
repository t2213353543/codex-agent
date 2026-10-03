package server

import (
	"bytes"
	"encoding/json"
)

func marshalMCPJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := out.Bytes()
	return raw[:len(raw)-1], nil
}

// App Server's MCP executor does not include the CLI-only thread_source field
// in x-codex-turn-metadata. Keep the client-owned MCP envelope and all unknown
// fields, removing only that measured difference when the metadata is present.
func appServerMirrorMCPMetadata(raw []byte) ([]byte, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return raw, false, err
	}
	params, ok := envelope["params"]
	if !ok {
		return raw, false, nil
	}
	var paramsObject map[string]json.RawMessage
	if json.Unmarshal(params, &paramsObject) != nil {
		return raw, false, nil
	}
	meta, ok := paramsObject["_meta"]
	if !ok {
		return raw, false, nil
	}
	var metaObject map[string]json.RawMessage
	if json.Unmarshal(meta, &metaObject) != nil {
		return raw, false, nil
	}
	turnRaw, ok := metaObject["x-codex-turn-metadata"]
	if !ok {
		return raw, false, nil
	}
	var turnText string
	if json.Unmarshal(turnRaw, &turnText) != nil || turnText == "" {
		return raw, false, nil
	}
	var turn map[string]json.RawMessage
	if json.Unmarshal([]byte(turnText), &turn) != nil || turn == nil {
		return raw, false, nil
	}
	if _, exists := turn["thread_source"]; !exists {
		return raw, false, nil
	}
	delete(turn, "thread_source")
	clean, err := marshalMCPJSON(turn)
	if err != nil {
		return nil, false, err
	}
	metaText, err := marshalMCPJSON(string(clean))
	if err != nil {
		return nil, false, err
	}
	metaObject["x-codex-turn-metadata"] = metaText
	paramsObject["_meta"], err = marshalMCPJSON(metaObject)
	if err != nil {
		return nil, false, err
	}
	envelope["params"], err = marshalMCPJSON(paramsObject)
	if err != nil {
		return nil, false, err
	}
	result, err := marshalMCPJSON(envelope)
	if err != nil {
		return nil, false, err
	}
	return result, !bytes.Equal(result, raw), nil
}
