package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const streamTestCompleted = `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2}}}`

func streamTestFrame(event string) string { return "data: " + event + "\n\n" }

func streamTestEvents(t *testing.T, raw string) []map[string]json.RawMessage {
	t.Helper()
	var events []map[string]json.RawMessage
	for _, frame := range strings.Split(raw, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var value map[string]json.RawMessage
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
					t.Fatal(err)
				}
				events = append(events, value)
			}
		}
	}
	return events
}

func TestStreamFlushesFirstDeltaBeforeTerminalIsAvailable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	var output bytes.Buffer
	flushed := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		_, err := Stream(ctx, &output, reader, StreamOptions{Flush: func() error {
			flushed <- output.String()
			return nil
		}})
		done <- err
	}()
	if _, err := io.WriteString(writer, streamTestFrame(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"糖果21"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-flushed:
		if !strings.Contains(value, "糖果21") || strings.Contains(value, "response.completed") {
			t.Fatalf("first flushed event = %s", value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delta was buffered while waiting for the terminal")
	}
	if _, err := io.WriteString(writer, streamTestFrame(streamTestCompleted)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal did not finish stream")
	}
}

type streamByteReader struct{ text []byte }

func (r *streamByteReader) Read(dst []byte) (int, error) {
	if len(r.text) == 0 {
		return 0, io.EOF
	}
	dst[0], r.text = r.text[0], r.text[1:]
	return 1, nil
}

func TestStreamParsesFragmentedMultilineUTF8CRLFAndComments(t *testing.T) {
	wire := "\xef\xbb\xbf: keep alive\r\n\r\n" +
		"id: event-7\revent: response.output_text.delta\rdata: {\"type\":\r\ndata: \"response.output_text.delta\",\"delta\":\"糖果🍬\",\"item_id\":\"msg_1\"}\r\n\r\n" +
		strings.ReplaceAll(streamTestFrame(streamTestCompleted), "\n", "\r\n")
	var output bytes.Buffer
	flushes := 0
	result, err := Stream(context.Background(), &output, &streamByteReader{text: []byte(wire)}, StreamOptions{Flush: func() error { flushes++; return nil }})
	if err != nil || !result.Completed || result.ResponseID != "resp_test" || flushes != 3 {
		t.Fatalf("result = %+v, flushes = %d, error = %v", result, flushes, err)
	}
	if !strings.Contains(output.String(), ": keep alive\n\n") || !strings.Contains(output.String(), "id: event-7\n") {
		t.Fatalf("envelope lost: %s", output.String())
	}
	events := streamTestEvents(t, output.String())
	if len(events) != 2 || streamString(events[0], "delta") != "糖果🍬" {
		t.Fatalf("events = %s", output.String())
	}
}

func TestStreamCancellationClosesBlockedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := Stream(ctx, io.Discard, reader, StreamOptions{})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked upstream body was not closed")
	}
}

type streamChunkReader struct {
	chunks [][]byte
	reads  atomic.Int64
}

func (r *streamChunkReader) Read(dst []byte) (int, error) {
	r.reads.Add(1)
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(dst, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

type streamBlockedWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w *streamBlockedWriter) Write([]byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, io.ErrClosedPipe
}

func TestStreamBackpressureDoesNotPumpFurtherEvents(t *testing.T) {
	reader := &streamChunkReader{chunks: [][]byte{
		[]byte(streamTestFrame(`{"type":"response.output_text.delta","delta":"first"}`)),
		[]byte(streamTestFrame(streamTestCompleted)),
	}}
	writer := &streamBlockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := Stream(context.Background(), writer, reader, StreamOptions{}); done <- err }()
	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first event was not written")
	}
	if reads := reader.reads.Load(); reads != 1 {
		t.Fatalf("read %d upstream chunks before downstream accepted the first event", reads)
	}
	close(writer.release)
	if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if reads := reader.reads.Load(); reads != 1 {
		t.Fatalf("read after write failure: %d", reads)
	}
}

func TestStreamTruncationAndMalformedTerminalsNeverSucceed(t *testing.T) {
	cases := map[string]string{
		"EOF":           streamTestFrame(`{"type":"response.output_text.delta","delta":"partial"}`),
		"DONE only":     "data: [DONE]\n\n",
		"no delimiter":  "data: " + streamTestCompleted + "\n",
		"broken JSON":   streamTestFrame(`{"type":"response.completed","response":`),
		"missing body":  streamTestFrame(`{"type":"response.completed"}`),
		"missing id":    streamTestFrame(`{"type":"response.completed","response":{"status":"completed","output":[]}}`),
		"missing state": streamTestFrame(`{"type":"response.completed","response":{"id":"r","output":[]}}`),
		"missing array": streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`),
		"null array":    streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":null}}`),
		"false status":  streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"failed","output":[]}}`),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{})
			if err == nil || result.Completed || strings.Contains(output.String(), `"type":"response.completed"`) {
				t.Fatalf("result = %+v, error = %v, output = %s", result, err, output.String())
			}
		})
	}
}

func TestStreamForwardsActualFailedAndCancelledTerminals(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled", "error"} {
		t.Run(kind, func(t *testing.T) {
			wire := streamTestFrame(`{"type":"` + kind + `","error":{"code":"test_failure","message":"private upstream diagnostic"}}`)
			var output bytes.Buffer
			committed := false
			result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{OnComplete: func(StreamResult) error { committed = true; return nil }})
			var terminal *TerminalError
			if !errors.As(err, &terminal) || result.Completed || committed || !strings.Contains(output.String(), kind) {
				t.Fatalf("result = %+v, error = %v, committed = %v", result, err, committed)
			}
			if strings.Contains(err.Error(), "private") || !bytes.Contains(terminal.Payload, []byte("private")) {
				t.Fatal("diagnostic payload handling changed")
			}
		})
	}
}

func TestStreamBoundsEventAndHistorySeparately(t *testing.T) {
	t.Run("event", func(t *testing.T) {
		_, err := Stream(context.Background(), io.Discard, strings.NewReader(streamTestFrame(`{"type":"unknown","blob":"`+strings.Repeat("x", 100)+`"}`)), StreamOptions{MaxEventBytes: 64})
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("multiline event", func(t *testing.T) {
		_, err := Stream(context.Background(), io.Discard, strings.NewReader(strings.Repeat(": comment bytes\n", 20)+"\n"), StreamOptions{MaxEventBytes: 64})
		if !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("history", func(t *testing.T) {
		wire := streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}]}}`)
		var output bytes.Buffer
		result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{MaxHistoryBytes: 8})
		if err != nil || !result.Completed || result.HistoryComplete || result.Output != nil || !strings.Contains(output.String(), "opaque") {
			t.Fatalf("result = %+v, error = %v, wire = %s", result, err, output.String())
		}
	})
}

func TestStreamPreservesUnknownRefusalAndNoIDCompaction(t *testing.T) {
	output := `[{"type":"future_item","vendor":{"array":[1,2,3]}},{"type":"message","id":"msg_1","content":[{"type":"refusal","refusal":"declined"}]},{"type":"compaction","encrypted_content":"opaque"}]`
	wire := streamTestFrame(`{"type":"response.future_event","opaque":{"key":"value"}}`) + streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":`+output+`}}`)
	var dst bytes.Buffer
	result, err := Stream(context.Background(), &dst, strings.NewReader(wire), StreamOptions{})
	if err != nil || !result.Completed || len(result.Output) != 3 {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	item, _ := streamObject(result.Output[2])
	if item["id"] != nil || streamString(item, "encrypted_content") != "opaque" {
		t.Fatalf("compaction changed: %s", result.Output[2])
	}
	if !bytes.Contains(result.Output[1], []byte(`"refusal":"declined"`)) || !strings.Contains(dst.String(), "response.future_event") {
		t.Fatalf("opaque data lost: %s", dst.String())
	}
}

func streamTestToolNormalizer(item map[string]json.RawMessage) error {
	if streamString(item, "name") == "write_file" {
		item["namespace"] = json.RawMessage(`"filesystem"`)
	}
	if streamString(item, "name") == "raw_patch" {
		item["namespace"] = json.RawMessage(`"functions"`)
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		if args, ok := item["arguments"]; ok {
			item["input"] = args
			delete(item, "arguments")
		}
	}
	return nil
}

func TestStreamToolNamespaceAndCustomDeltaIdentityStayConsistent(t *testing.T) {
	for _, name := range []string{"write_file", "raw_patch"} {
		t.Run(name, func(t *testing.T) {
			item := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"` + name + `","arguments":""}`
			wire := streamTestFrame(`{"type":"response.output_item.added","output_index":0,"item":`+item+`}`) +
				"event: response.function_call_arguments.delta\n" + streamTestFrame(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"payload"}`) +
				streamTestFrame(`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"payload"}`) +
				streamTestFrame(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","arguments":"payload"}}`) +
				streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+strings.Replace(item, `"arguments":""`, `"arguments":"payload"`, 1)+`]}}`)
			var output bytes.Buffer
			result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{NormalizeItem: streamTestToolNormalizer})
			if err != nil || !result.Completed {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
			events := streamTestEvents(t, output.String())
			wantNamespace := "filesystem"
			if name == "raw_patch" {
				wantNamespace = "functions"
				if streamString(events[1], "type") != "response.custom_tool_call_input.delta" || events[2]["arguments"] != nil || streamString(events[2], "input") != "payload" {
					t.Fatalf("custom events: %s", output.String())
				}
				if !strings.Contains(output.String(), "event: response.custom_tool_call_input.delta\n") {
					t.Fatalf("custom SSE event name not transformed: %s", output.String())
				}
			}
			for _, index := range []int{1, 2} {
				if streamString(events[index], "namespace") != wantNamespace || streamString(events[index], "call_id") != "call_1" || streamString(events[index], "item_id") != "fc_1" {
					t.Fatalf("identity missing: %s", output.String())
				}
			}
			doneItem, _ := streamObject(events[3]["item"])
			if streamString(doneItem, "namespace") != wantNamespace || streamString(doneItem, "name") != name || streamString(doneItem, "call_id") != "call_1" {
				t.Fatalf("done item identity = %s", events[3]["item"])
			}
			if name == "raw_patch" && (streamString(doneItem, "type") != "custom_tool_call" || streamString(doneItem, "input") != "payload" || doneItem["arguments"] != nil) {
				t.Fatalf("done item reverted to native identity: %s", events[3]["item"])
			}
		})
	}
}

func TestStreamLateToolNameUsesItemIdentityWithoutInventingIDs(t *testing.T) {
	wire := streamTestFrame(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1"}}`) +
		streamTestFrame(`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`) +
		streamTestFrame(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","name":"write_file","arguments":"{}"}}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","id":"fc_1","arguments":"{}"}]}}`)
	var output bytes.Buffer
	result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{NormalizeItem: streamTestToolNormalizer})
	if err != nil {
		t.Fatal(err)
	}
	item, _ := streamObject(result.Output[0])
	if streamString(item, "namespace") != "filesystem" || streamString(item, "name") != "write_file" || streamString(item, "call_id") != "call_1" {
		t.Fatalf("late identity not retained: %s", result.Output[0])
	}
	events := streamTestEvents(t, output.String())
	if events[1]["namespace"] != nil || events[1]["name"] != nil {
		t.Fatalf("identity invented before name arrived: %s", output.String())
	}
}

func TestStreamLearnsLateNameFromArgumentsDone(t *testing.T) {
	wire := streamTestFrame(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1"}}`) +
		streamTestFrame(`{"type":"response.function_call_arguments.done","item_id":"fc_1","name":"write_file","arguments":"{}"}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"function_call","id":"fc_1","arguments":"{}"}]}}`)
	var output bytes.Buffer
	result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{NormalizeItem: streamTestToolNormalizer})
	if err != nil {
		t.Fatal(err)
	}
	events := streamTestEvents(t, output.String())
	if streamString(events[1], "namespace") != "filesystem" || streamString(events[1], "call_id") != "call_1" {
		t.Fatalf("late argument identity not normalized: %s", output.String())
	}
	item, _ := streamObject(result.Output[0])
	if streamString(item, "name") != "write_file" || streamString(item, "namespace") != "filesystem" {
		t.Fatalf("terminal lost late identity: %s", result.Output[0])
	}
}

func TestStreamRejectsChangedToolCallID(t *testing.T) {
	wire := streamTestFrame(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"write_file"}}`) +
		streamTestFrame(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_2","name":"write_file"}}`)
	result, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{NormalizeItem: streamTestToolNormalizer})
	if !errors.Is(err, ErrResponseIdentity) || result.Completed {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestStreamCommitRunsBeforeCompletionAndFailureWithholdsTerminal(t *testing.T) {
	var output bytes.Buffer
	storageFailure := errors.New("history storage unavailable")
	observed := false
	result, err := Stream(context.Background(), &output, strings.NewReader(streamTestFrame(streamTestCompleted)), StreamOptions{
		OnComplete: func(result StreamResult) error {
			observed = true
			if output.Len() != 0 || !result.Completed || len(result.Response) == 0 || result.ResponseID != "resp_test" {
				t.Errorf("commit called too late or without complete response: %+v", result)
			}
			return storageFailure
		},
	})
	if !observed || !errors.Is(err, storageFailure) || result.Completed || output.Len() != 0 {
		t.Fatalf("result = %+v, error = %v, bytes = %d", result, err, output.Len())
	}
}

func TestStreamRejectsResponseIdentityChangesAndInvalidUTF8(t *testing.T) {
	wire := streamTestFrame(`{"type":"response.created","response":{"id":"r1","status":"in_progress","output":[]}}`) + streamTestFrame(streamTestCompleted)
	result, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
	if !errors.Is(err, ErrResponseIdentity) || result.Completed {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	_, err = Stream(context.Background(), io.Discard, bytes.NewReader([]byte("data: {\"type\":\"unknown\",\"text\":\"\xff\"}\n\n")), StreamOptions{})
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("UTF-8 error = %v", err)
	}
}

func TestCompactJSONToSSEPreservesOpaqueCompactionAndFailure(t *testing.T) {
	var output bytes.Buffer
	var order []string
	result, err := CompactJSONToSSE(context.Background(), &output, strings.NewReader(`{"id":"comp_test","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":5}}`), StreamOptions{
		OnComplete: func(StreamResult) error { order = append(order, "commit"); return nil },
		OnEvent: func(raw []byte) error {
			item, _ := streamObject(raw)
			order = append(order, streamString(item, "type"))
			return nil
		},
	})
	if err != nil || !result.Completed || len(result.Output) != 1 || strings.Join(order, ",") != "response.output_item.done,commit,response.completed" {
		t.Fatalf("result = %+v, error = %v, order = %v", result, err, order)
	}
	item, _ := streamObject(result.Output[0])
	if item["id"] != nil || streamString(item, "encrypted_content") != "opaque" {
		t.Fatal("compaction item changed")
	}
	output.Reset()
	result, err = CompactJSONToSSE(context.Background(), &output, strings.NewReader(`{"id":"r","status":"failed","output":[],"error":{"code":"failed"}}`), StreamOptions{})
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || !strings.Contains(output.String(), "response.failed") {
		t.Fatalf("result = %+v, error = %v, wire = %s", result, err, output.String())
	}
}

func TestStreamTerminalMustIncludeEveryPreviouslyCompletedItem(t *testing.T) {
	const tool = `{"type":"function_call","id":"fc1","call_id":"c1","name":"inspect","arguments":"{}"}`
	const compact = `{"type":"compaction","encrypted_content":"opaque-original"}`
	for _, test := range []struct {
		name, done, output string
		valid              bool
	}{
		{"missing tool", `{"type":"response.output_item.done","output_index":0,"item":` + tool + `}`, `[]`, false},
		{"changed arguments", `{"type":"response.output_item.done","item":` + tool + `}`, `[{"type":"function_call","id":"fc1","call_id":"c1","name":"inspect","arguments":"changed"}]`, false},
		{"indexed tool moved", `{"type":"response.output_item.done","output_index":1,"item":` + tool + `}`, `[` + tool + `]`, false},
		{"no-id compact missing", `{"type":"response.output_item.done","output_index":0,"item":` + compact + `}`, `[]`, false},
		{"no-id compact changed", `{"type":"response.output_item.done","output_index":0,"item":` + compact + `}`, `[{"type":"compaction","encrypted_content":"different-state"}]`, false},
		{"tool intact", `{"type":"response.output_item.done","item":` + tool + `}`, `[` + tool + `]`, true},
		{"no-id compact intact", `{"type":"response.output_item.done","output_index":0,"item":` + compact + `}`, `[` + compact + `]`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := streamTestFrame(test.done) + streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":`+test.output+`}}`)
			var out bytes.Buffer
			committed := false
			result, err := Stream(context.Background(), &out, strings.NewReader(wire), StreamOptions{OnComplete: func(StreamResult) error { committed = true; return nil }})
			if test.valid {
				if err != nil || !result.Completed || !committed {
					t.Fatalf("intact terminal rejected: %+v %v", result, err)
				}
			} else if !errors.Is(err, ErrInvalidEvent) || result.Completed || committed || strings.Contains(out.String(), `"type":"response.completed"`) {
				t.Fatalf("incomplete history accepted: %+v %v %s", result, err, out.String())
			}
		})
	}
}

func TestStreamCompletedWithNonNullErrorCannotSucceed(t *testing.T) {
	for _, value := range []string{`{"code":"failed"}`, `"failed"`, `false`, `{}`} {
		for _, topLevel := range []bool{false, true} {
			response := `{"id":"r","status":"completed","output":[]}`
			event := `{"type":"response.completed","error":` + value + `,"response":` + response + `}`
			if !topLevel {
				event = `{"type":"response.completed","response":{"id":"r","status":"completed","output":[],"error":` + value + `}}`
			}
			var out bytes.Buffer
			result, err := Stream(context.Background(), &out, strings.NewReader(streamTestFrame(event)), StreamOptions{})
			if !errors.Is(err, ErrInvalidEvent) || result.Completed || out.Len() != 0 {
				t.Fatalf("contradictory completion accepted: %+v %v %s", result, err, out.String())
			}
		}
	}
}

func TestStreamDoneMismatchDiagnosticReportsOnlyIdentityAndFieldNames(t *testing.T) {
	before := `{"type":"message","id":"msg_diag","status":"completed","content":[{"type":"output_text","text":"PRIVATE_BEFORE"}],"phase":"final_answer"}`
	after := `{"type":"message","id":"msg_diag","status":"completed","content":[{"type":"output_text","text":"PRIVATE_AFTER"}],"extra":"PRIVATE_EXTRA"}`
	wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
	result, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
	if !errors.Is(err, ErrInvalidEvent) || result.Completed {
		t.Fatalf("mismatch accepted: %+v %v", result, err)
	}
	for _, expected := range []string{"item_type=message", "item_id=msg_diag", "output_index=0", "added_fields=[extra]", "removed_fields=[phase]", "changed_fields=[content]"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("diagnostic missing %q: %v", expected, err)
		}
	}
	if strings.Contains(err.Error(), "PRIVATE_") || strings.Contains(err.Error(), "final_answer") {
		t.Fatalf("private value leaked in diagnostic: %v", err)
	}
}

func TestStreamDoneIntegrityIgnoresNestedObjectKeyOrderButRetainsNumbers(t *testing.T) {
	before := `{"type":"message","id":"msg_order","content":[{"type":"output_text","text":"same answer","annotations":[{"type":"url_citation","start_index":1,"end_index":2,"details":{"z":9007199254740993,"a":1.2500e+02}}]}]}`
	after := `{"content":[{"annotations":[{"details":{"a":1.2500e+02,"z":9007199254740993},"end_index":2,"start_index":1,"type":"url_citation"}],"text":"same answer","type":"output_text"}],"id":"msg_order","type":"message"}`
	wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
	var output bytes.Buffer
	result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{})
	if err != nil || !result.Completed {
		t.Fatalf("legitimate nested serialization change rejected: %+v %v", result, err)
	}
	if !bytes.Contains(result.Output[0], []byte("9007199254740993")) || !bytes.Contains(result.Output[0], []byte("1.2500e+02")) {
		t.Fatalf("comparison changed forwarded number literals: %s", result.Output[0])
	}
	changed := strings.Replace(after, "9007199254740993", "9007199254740992", 1)
	wire = streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+changed+`]}}`)
	result, err = Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
	if !errors.Is(err, ErrInvalidEvent) || result.Completed {
		t.Fatalf("real large integer change was not detected: %+v %v", result, err)
	}
}

func TestStreamReasoningEnvelopeKeepsAlreadyDeliveredUpstreamCipher(t *testing.T) {
	const before = `{"type":"reasoning","id":"rs_cipher","summary":[{"type":"summary_text","text":"same reasoning summary"}],"encrypted_content":"upstream-done-cipher+/="}`
	const after = `{"id":"rs_cipher","type":"reasoning","encrypted_content":"upstream-terminal-rewrapped-cipher","summary":[{"text":"same reasoning summary","type":"summary_text"}]}`
	wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
	var output bytes.Buffer
	var committed StreamResult
	result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{OnComplete: func(result StreamResult) error { committed = result; return nil }})
	if err != nil || !result.Completed || !result.HistoryComplete || len(result.Output) != 1 {
		t.Fatalf("valid reasoning re-envelope rejected: %+v %v", result, err)
	}
	for _, raw := range []json.RawMessage{result.Output[0], result.Response, committed.Output[0], committed.Response} {
		if !bytes.Contains(raw, []byte("upstream-done-cipher+/=")) || bytes.Contains(raw, []byte("upstream-terminal-rewrapped-cipher")) {
			t.Fatalf("CLI done cipher and persisted history diverged: %s", raw)
		}
	}
	events := streamTestEvents(t, output.String())
	if len(events) != 2 || !bytes.Contains(events[0]["item"], []byte("upstream-done-cipher+/=")) || !bytes.Contains(events[1]["response"], []byte("upstream-done-cipher+/=")) || strings.Contains(output.String(), "upstream-terminal-rewrapped-cipher") {
		t.Fatalf("delivered cipher differs across done/terminal: %s", output.String())
	}
}

func TestStreamReasoningCipherReconciliationRejectsEveryOtherChange(t *testing.T) {
	const before = `{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":"PRIVATE_DONE_CIPHER"}`
	for _, after := range []string{
		`{"type":"reasoning","id":"rs_other","summary":[],"encrypted_content":"PRIVATE_TERMINAL_CIPHER"}`,
		`{"type":"reasoning","id":"rs_cipher","summary":[{"type":"summary_text","text":"changed"}],"encrypted_content":"PRIVATE_TERMINAL_CIPHER"}`,
		`{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":"PRIVATE_TERMINAL_CIPHER","extra":true}`,
		`{"type":"reasoning","id":"rs_cipher","encrypted_content":"PRIVATE_TERMINAL_CIPHER"}`,
		`{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":""}`,
		`{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":null}`,
		`{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":42}`,
		`{"type":"compaction","id":"rs_cipher","summary":[],"encrypted_content":"PRIVATE_TERMINAL_CIPHER"}`,
	} {
		wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
			streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
		var output bytes.Buffer
		result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{})
		if !errors.Is(err, ErrInvalidEvent) || result.Completed || strings.Contains(output.String(), `"type":"response.completed"`) {
			t.Fatalf("non-envelope change was accepted: %+v %v", result, err)
		}
		if strings.Contains(err.Error(), "PRIVATE_") {
			t.Fatalf("cipher leaked in diagnostic: %v", err)
		}
	}
	for _, invalidDone := range []string{`""`, `null`, `42`} {
		item := `{"type":"reasoning","id":"rs_cipher","summary":[],"encrypted_content":` + invalidDone + `}`
		wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
			streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+before+`]}}`)
		result, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
		if !errors.Is(err, ErrInvalidEvent) || result.Completed {
			t.Fatalf("invalid done cipher was reconciled: %+v %v", result, err)
		}
	}
}

func TestStreamCompletedReasoningCipherStorageSharesBoundedHistoryBudget(t *testing.T) {
	input := json.RawMessage(`{"role":"user","content":"` + strings.Repeat("input", 40) + `"}`)
	makeDone := func(id string) string {
		return streamTestFrame(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"` + id + `","summary":[],"encrypted_content":"` + strings.Repeat("C", 350) + `"}}`)
	}
	result, err := Stream(context.Background(), io.Discard, strings.NewReader(makeDone("rs_one")+makeDone("rs_two")), StreamOptions{
		MaxHistoryBytes: 1024,
		Input:           []json.RawMessage{input},
	})
	if !errors.Is(err, ErrEventTooLarge) || result.Completed {
		t.Fatalf("temporary cipher retention exceeded history budget: %+v %v", result, err)
	}
}

func TestStreamNativeSearchActionKeepsAlreadyDeliveredObject(t *testing.T) {
	const doneAction = `{"type":"search","query":"official docs","sources":[{"type":"url","url":"https://official.example/original","title":"Original source"}]}`
	const terminalAction = `{"type":"search","query":"official docs","sources":[{"type":"url","url":"https://official.example/refreshed","title":"Refreshed source"}]}`
	before := `{"type":"web_search_call","id":"ws_search","status":"completed","action":` + doneAction + `}`
	after := `{"type":"web_search_call","id":"ws_search","status":"completed","action":` + terminalAction + `}`
	wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
		streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
	var output bytes.Buffer
	var committed StreamResult
	result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{OnComplete: func(result StreamResult) error { committed = result; return nil }})
	if err != nil || !result.Completed || !result.HistoryComplete || len(result.Output) != 1 {
		t.Fatalf("native search action metadata refresh rejected: %+v %v", result, err)
	}
	for _, raw := range []json.RawMessage{result.Output[0], committed.Output[0]} {
		item, _ := streamObject(raw)
		if !bytes.Equal(item["action"], []byte(doneAction)) {
			t.Fatalf("history diverged from already delivered search action: %s", raw)
		}
	}
	events := streamTestEvents(t, output.String())
	if len(events) != 2 || !bytes.Contains(events[0]["item"], []byte(doneAction)) || !bytes.Contains(events[1]["response"], []byte(doneAction)) || strings.Contains(output.String(), "refreshed") {
		t.Fatalf("search action changed across done and terminal: %s", output.String())
	}
}

func TestStreamSearchActionReconciliationRejectsOtherChangesOrNonObjects(t *testing.T) {
	const before = `{"type":"web_search_call","id":"ws_search","status":"completed","action":{"type":"search","query":"PRIVATE_QUERY","sources":[]}}`
	for _, after := range []string{
		`{"type":"web_search_call","id":"ws_other","status":"completed","action":{}}`,
		`{"type":"web_search_call","id":"ws_search","status":"in_progress","action":{}}`,
		`{"type":"web_search_call","id":"ws_search","status":"completed","extra":true,"action":{}}`,
		`{"type":"web_search_call","id":"ws_search","action":{}}`,
		`{"type":"web_search_call","id":"ws_search","status":"completed","action":null}`,
		`{"type":"web_search_call","id":"ws_search","status":"completed","action":[]}`,
		`{"type":"web_search_call","id":"ws_search","status":"completed","action":"PRIVATE_STRING"}`,
		`{"type":"function_call","id":"ws_search","status":"completed","action":{}}`,
	} {
		wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+before+`}`) +
			streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+after+`]}}`)
		var output bytes.Buffer
		result, err := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{})
		if !errors.Is(err, ErrInvalidEvent) || result.Completed || strings.Contains(output.String(), `"type":"response.completed"`) {
			t.Fatalf("non-action change was accepted: %+v %v", result, err)
		}
		if strings.Contains(err.Error(), "PRIVATE_") {
			t.Fatalf("search query leaked in diagnostic: %v", err)
		}
	}
	for _, invalidAction := range []string{`""`, `null`, `[]`, `42`} {
		item := `{"type":"web_search_call","id":"ws_search","status":"completed","action":` + invalidAction + `}`
		wire := streamTestFrame(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`) +
			streamTestFrame(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[`+before+`]}}`)
		result, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
		if !errors.Is(err, ErrInvalidEvent) || result.Completed {
			t.Fatalf("non-object done action was reconciled: %+v %v", result, err)
		}
	}
}

func TestStreamSearchActionsShareBoundedHistoryBudget(t *testing.T) {
	input := json.RawMessage(`{"role":"user","content":"` + strings.Repeat("input", 40) + `"}`)
	makeDone := func(id string) string {
		return streamTestFrame(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"` + id + `","status":"completed","action":{"type":"search","query":"` + strings.Repeat("Q", 350) + `"}}}`)
	}
	result, err := Stream(context.Background(), io.Discard, strings.NewReader(makeDone("ws_one")+makeDone("ws_two")), StreamOptions{
		MaxHistoryBytes: 1024,
		Input:           []json.RawMessage{input},
	})
	if !errors.Is(err, ErrEventTooLarge) || result.Completed {
		t.Fatalf("temporary search action retention exceeded history budget: %+v %v", result, err)
	}
}
