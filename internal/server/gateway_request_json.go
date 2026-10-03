package server

import (
	"bytes"
	"encoding/json"
)

// Gateway bodies are standalone application/json or WS JSON, never embedded
// HTML. Escaping <, > and & can turn a legal large source/tool context into a
// six-times-larger request that the gateway itself rejects. Preserve standard
// JSON quoting/validation and RawMessage number/history fields, without changing
// the separate canonical cache digest or management API encoding contracts.
func marshalGatewayRequestJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
