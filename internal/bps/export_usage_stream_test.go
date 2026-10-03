package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExportUsageOnlyTransformsDeliveryNotObservers(t *testing.T) {
	var delivered bytes.Buffer
	var observed, complete, terminal []byte
	response := `{"id":"billing_observer","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":50}}}`
	result, err := Stream(context.Background(), &delivered, strings.NewReader("data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n"), StreamOptions{
		ClientUsageTransform: ExportCacheCreationAsRead,
		OnEvent:              func(raw []byte) error { observed = bytes.Clone(raw); return nil },
		OnComplete:           func(result StreamResult) error { complete = bytes.Clone(result.Response); return nil },
		OnUpstreamTerminal:   func(raw []byte) error { terminal = bytes.Clone(raw); return nil },
	})
	if err != nil || !result.Completed {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{observed, complete, terminal, result.Response} {
		if !bytes.Contains(raw, []byte(`"cached_tokens":40`)) || !bytes.Contains(raw, []byte(`"cache_write_tokens":50`)) {
			t.Fatal("internal accounting was projected")
		}
	}
	if !bytes.Contains(delivered.Bytes(), []byte(`"cached_tokens":90`)) || !bytes.Contains(delivered.Bytes(), []byte(`"cache_write_tokens":0`)) {
		t.Fatal("client billing projection missing")
	}
	var parsed map[string]json.RawMessage
	if json.Unmarshal(result.Response, &parsed) != nil {
		t.Fatal("result corrupted")
	}
}

func TestExportNativeResponseIgnoresNestedResponseExtension(t *testing.T) {
	const original = `{"id":"root","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}},"response":{"id":"extension","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}}}`
	got := ExportResponseCacheCreationAsRead([]byte(original))
	want := strings.Replace(strings.Replace(original, `"cached_tokens":80`, `"cached_tokens":87`, 1), `"cache_write_tokens":7`, `"cache_write_tokens":0`, 1)
	if string(got) != want {
		t.Fatalf("wrong native usage projection: %s", got)
	}
}
