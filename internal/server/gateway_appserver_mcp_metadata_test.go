package server

import (
	"bytes"
	"strings"
	"testing"
)

func TestAppServerMirrorMCPMetadataRemovesOnlyCLIThreadSource(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"tools/call","params":{"_meta":{"x-codex-turn-metadata":"{\"thread_source\":\"user\",\"turn_id\":\"turn\",\"opaque\":9007199254740993}"},"name":"fixture","arguments":{"text":"<>&"}}}`)
	got, changed, err := appServerMirrorMCPMetadata(raw)
	if err != nil || !changed || bytes.Contains(got, []byte("thread_source")) || !bytes.Contains(got, []byte("9007199254740993")) || !bytes.Contains(got, []byte("<>&")) {
		t.Fatalf("MCP metadata projection lost/retained fields: changed=%v err=%v body=%s", changed, err, got)
	}
	untouched := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	copy, changed, err := appServerMirrorMCPMetadata(untouched)
	if err != nil || changed || !bytes.Equal(copy, untouched) {
		t.Fatal("MCP request without measured metadata was rewritten")
	}
	if strings.Contains(string(got), "Bearer") {
		t.Fatal("MCP body acquired authentication")
	}
}
