package bps

import (
	"bytes"
	"testing"
)

func TestPublicErrorsOmitProviderWithoutEditingModelContent(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","message":"POWERPOINT failed","error":{"code":"upstream_error","message":"PowerPoint denied"}}`,
		`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"message":"PowerPoint busy"}}}`,
	} {
		payload, terminal, err := NewTransformer(StreamOptions{}).Transform([]byte(raw))
		if !terminal || err == nil || bytes.Contains(bytes.ToLower(payload), []byte("powerpoint")) {
			t.Fatalf("provider disclosed in terminal error: %s %v", payload, err)
		}
	}
	payload, terminal, err := NewTransformer(StreamOptions{}).Transform([]byte(`{"type":"response.output_text.delta","delta":"A PowerPoint presentation"}`))
	if terminal || err != nil || !bytes.Contains(payload, []byte("A PowerPoint presentation")) {
		t.Fatal("successful model output was edited")
	}
}
