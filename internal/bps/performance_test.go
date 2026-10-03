package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// These synthetic fixtures isolate gateway CPU/allocation cost; they do not
// measure model generation or network latency. Keep the same fixtures for A/B.
func performanceRequest(history, textBytes, tools int) []byte {
	items := make([]json.RawMessage, history)
	for i := range items {
		items[i] = message("user", strings.Repeat("x", textBytes))
	}
	definitions := make([]json.RawMessage, tools)
	for i := range definitions {
		definitions[i] = raw(map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", i), "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}})
	}
	return raw(map[string]any{"model": "gpt-5.4", "input": items, "tools": definitions, "stream": true, "store": false})
}

func BenchmarkGatewayBPSPrepare(b *testing.B) {
	for _, tc := range []struct {
		name                      string
		history, textBytes, tools int
	}{
		{"Small", 1, 32, 0}, {"History128x1K", 128, 1024, 0}, {"History256x4KTools32", 256, 4096, 32},
	} {
		b.Run(tc.name, func(b *testing.B) {
			input := performanceRequest(tc.history, tc.textBytes, tc.tools)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := Prepare(input, Options{TaskID: "perf_task", TurnID: "perf_turn"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGatewayBPSTransformDelta(b *testing.B) {
	for _, kind := range []string{"response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta"} {
		b.Run(kind, func(b *testing.B) {
			payload := raw(map[string]any{"type": kind, "item_id": "msg_perf", "response_id": "resp_perf", "output_index": 0, "content_index": 0, "delta": "some text 中文 <value> 1234567890"})
			t := NewTransformer(StreamOptions{})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := t.Transform(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func performanceStream(frames int, withEnvelope bool) []byte {
	delta := `{"type":"response.output_text.delta","item_id":"msg_perf","response_id":"resp_test","output_index":0,"content_index":0,"delta":"hello world 中文"}`
	prefix := ""
	if withEnvelope {
		prefix = "event: response.output_text.delta\nid: evt_1\n"
	}
	return []byte(strings.Repeat(prefix+streamTestFrame(delta), frames) + streamTestFrame(streamTestCompleted))
}

func BenchmarkGatewayBPSStream(b *testing.B) {
	for _, tc := range []struct {
		name     string
		frames   int
		envelope bool
	}{
		{"FirstEvent", 1, false}, {"Text256", 256, false}, {"Text256Envelope", 256, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			wire := performanceStream(tc.frames, tc.envelope)
			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := Stream(context.Background(), io.Discard, bytes.NewReader(wire), StreamOptions{Flush: func() error { return nil }}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGatewayBPSFirstFlush(b *testing.B) {
	flushed := errors.New("benchmark first frame flushed")
	wire := performanceStream(1, false)
	options := StreamOptions{Flush: func() error { return flushed }}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Stream(context.Background(), io.Discard, bytes.NewReader(wire), options); err != flushed {
			b.Fatalf("first flush = %v", err)
		}
	}
}

func BenchmarkGatewayBPSStreamLargeTerminal(b *testing.B) {
	item := raw(map[string]any{"type": "message", "id": "msg_perf", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", 1<<20)}}})
	payload := raw(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_perf", "status": "completed", "output": []json.RawMessage{item}}})
	wire := []byte(streamTestFrame(string(payload)))
	b.ReportAllocs()
	b.SetBytes(int64(len(wire)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := Stream(context.Background(), io.Discard, bytes.NewReader(wire), StreamOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPerformanceStringFieldMatchesJSON(t *testing.T) {
	for _, input := range []string{`"plain"`, `"中文"`, `""`, ` "spaced" `, `"escaped\"quote"`, `"back\\slash"`, `"\u0061"`, `"\ud800"`, `null`, `123`, `true`, `[]`, `{}`, `"unclosed`, "\"bad\nstring\"", "\"\xff\""} {
		var want string
		_ = json.Unmarshal([]byte(input), &want)
		if got := stringField(map[string]json.RawMessage{"value": []byte(input)}, "value"); got != want {
			t.Errorf("decode %q = %q, want %q", input, got, want)
		}
	}
}

func TestPerformanceObjectStillRejectsTrailingAndNonObjects(t *testing.T) {
	for _, input := range []string{`{} {}`, `{} trailing`, `null`, `[]`, `"text"`, `true`, `123`} {
		if _, err := object([]byte(input)); err == nil {
			t.Errorf("accepted non-object/trailing JSON %q", input)
		}
	}
	fields, err := object([]byte(" {\"id\":9007199254740993} \n"))
	if err != nil || string(fields["id"]) != "9007199254740993" {
		t.Fatalf("raw integer changed: %s, %v", fields["id"], err)
	}
}

func TestPerformancePrepareOwnsHistoryAndKeepsUnknownFields(t *testing.T) {
	input := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"opaque":{"n":9007199254740993}},{"type":"additional_tools","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}]}]}`)
	request, err := Prepare(input, options())
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(request.Body)
	clear(input)
	if !bytes.Equal(before, request.Body) || !bytes.Contains(request.Body, []byte(`"n":9007199254740993`)) {
		t.Fatalf("history changed: %s", request.Body)
	}
	if len(request.Tools.Declared) != 1 || request.Tools.Declared[0].Name != "patch" {
		t.Fatal("history tool declaration lost")
	}
}

func TestPerformanceTerminalRejectsDuplicateIdentitiesAndWrongStatus(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","id":"same"},{"type":"message","id":"same"}]}}`,
		`{"type":"response.completed","response":{"id":"r","status":123,"output":[]}}`,
		`{"type":"response.completed","response":{"id":"r","status":null,"output":[]}}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed","output":null}}`,
	} {
		transformer := NewTransformer(StreamOptions{})
		out, _, err := transformer.Transform([]byte(payload))
		if err == nil || len(out) != 0 || transformer.Result().Completed {
			t.Fatalf("malformed completion accepted: %s, %v", out, err)
		}
	}
}

func BenchmarkGatewayBPSTerminal(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprintf("Text%d", size), func(b *testing.B) {
			item := raw(map[string]any{"type": "message", "id": "msg_perf", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", size)}}})
			payload := raw(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_perf", "status": "completed", "output": []json.RawMessage{item}}})
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				t := NewTransformer(StreamOptions{})
				if _, _, err := t.Transform(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
