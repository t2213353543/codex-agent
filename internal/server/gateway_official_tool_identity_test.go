package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfficialDefaultToolNamespacePreservesDeclaredScope(t *testing.T) {
	thread := &officialThread{id: "thread", turn: "turn", custom: map[string]bool{"\x00read": false, "other\x00read": false}, pending: map[string]json.RawMessage{}}
	global := officialToolAlias("tool", "", "read")
	for _, namespace := range []string{"", "functions"} {
		raw, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call", "id": "fc_fixture", "name": global, "namespace": namespace, "arguments": "{}"})
		out, err := thread.restoreItem(raw)
		if err != nil || !strings.Contains(string(out), `"name":"read"`) || strings.Contains(string(out), `"namespace"`) {
			t.Fatalf("default namespace rejected or leaked: %s %v", out, err)
		}
	}
	for _, test := range []struct{ namespace, name string }{{"other", global}, {"functions", officialToolAlias("tool", "other", "read")}, {"functions", "read"}, {"", officialToolAlias("tool", "", "undeclared")}} {
		if _, _, ok := thread.originalTool(test.namespace, test.name); ok {
			t.Fatalf("undeclared tool identity accepted: %+v", test)
		}
	}
	raw, _ := json.Marshal(map[string]any{"id": "rpc-1", "method": "item/tool/call", "params": map[string]string{"threadId": "thread", "turnId": "turn", "callId": "call", "tool": global, "namespace": "functions"}})
	if err := thread.capturePending(raw); err != nil {
		t.Fatalf("official default namespace request rejected: %v", err)
	}
	if thread.pendingTools["call"] != "\x00read" {
		t.Fatal("pending tool changed scope")
	}
}
