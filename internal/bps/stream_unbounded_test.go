package bps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestUnboundedStreamRetainsAllItemsPastLegacyCount(t *testing.T) {
	items := make([]json.RawMessage, 4097)
	for i := range items {
		items[i] = json.RawMessage(fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","content":[{"type":"output_text","text":"exact"}]}`, i))
	}
	response, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "unbounded", "status": "completed", "output": items}})
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		result, err := Stream(context.Background(), io.Discard, iotest.OneByteReader(strings.NewReader("data: "+string(response)+ending+ending)), StreamOptions{Unbounded: true, MaxEventBytes: 64, MaxHistoryBytes: 64, MaxHistoryItems: 1})
		if err != nil || !result.Completed || !result.HistoryComplete || len(result.Output) != 4097 {
			t.Fatalf("unbounded history/framing failed for %q: n=%d err=%v", ending, len(result.Output), err)
		}
	}
}

func TestUnboundedCompactIgnoresOnlyCapacityNotProtocol(t *testing.T) {
	input := `{"id":"compact","status":"completed","output":[{"type":"compaction","encrypted_content":"` + strings.Repeat("x", 1024) + `"}]}`
	result, err := CompactJSONToSSE(context.Background(), io.Discard, strings.NewReader(input), StreamOptions{Unbounded: true, MaxEventBytes: 10, MaxHistoryBytes: 10, MaxHistoryItems: 1})
	if err != nil || !result.Completed || !result.HistoryComplete || len(result.Output) != 1 {
		t.Fatalf("unbounded compact failed: %v", err)
	}
	for _, raw := range []string{"data: \xff\n\n", `data: {"type":"response.completed","response":null}` + "\n\n"} {
		if _, err := Stream(context.Background(), io.Discard, strings.NewReader(raw), StreamOptions{Unbounded: true}); err == nil {
			t.Fatal("capacity policy removed protocol integrity check")
		}
	}
}
