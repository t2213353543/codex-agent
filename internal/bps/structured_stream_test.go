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

func structuredStreamFormat(t *testing.T) *StructuredFormat {
	t.Helper()
	format, err := ParseStructuredFormat(json.RawMessage(`{"format":{"type":"json_schema","name":"result","strict":true,"schema":{"type":"object","properties":{"value":{"type":"string"},"n":{"type":"integer"}},"required":["value"],"additionalProperties":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return format
}

func structuredStreamCall(format *StructuredFormat, id, call, arguments string) json.RawMessage {
	return raw(map[string]any{"type": "function_call", "id": id, "call_id": call, "name": format.ToolName, "arguments": arguments, "status": "completed"})
}

func structuredStreamFrame(value any) string { return streamTestFrame(string(raw(value))) }

func structuredStreamFixture(format *StructuredFormat, arguments string) string {
	item := structuredStreamCall(format, "fc_internal", "call_internal", arguments)
	stub := raw(map[string]any{"type": "function_call", "id": "fc_internal", "call_id": "call_internal", "name": format.ToolName, "arguments": "", "status": "in_progress"})
	reasoning := json.RawMessage(`{"id":"rs_real","type":"reasoning","summary":[],"encrypted_content":"unchanged-upstream-cipher"}`)
	usage := json.RawMessage(`{"input_tokens":9007199254740993,"output_tokens":9,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":0}}`)
	return structuredStreamFrame(map[string]any{"type": "response.created", "sequence_number": 11, "response": map[string]any{"id": "resp_real", "status": "in_progress", "output": []any{}, "tools": []any{format.ToolDefinition(), json.RawMessage(`{"type":"function","name":"customer_tool","parameters":{"type":"object"}}`)}}}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": stub}) +
		structuredStreamFrame(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_internal", "output_index": 1, "delta": arguments[:len(arguments)/2]}) +
		structuredStreamFrame(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_internal", "output_index": 1, "delta": arguments[len(arguments)/2:]}) +
		structuredStreamFrame(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_internal", "output_index": 1, "arguments": arguments}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": item}) +
		structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_real", "model": "gpt-real", "status": "completed", "output": []any{reasoning, item}, "usage": usage, "tools": []any{format.ToolDefinition(), json.RawMessage(`{"type":"function","name":"customer_tool","parameters":{"type":"object"}}`)}, "tool_choice": map[string]any{"type": "function", "name": format.ToolName}}})
}

func TestStreamStructuredInternalToolBecomesExactPublicText(t *testing.T) {
	format := structuredStreamFormat(t)
	arguments := " {\"value\":\"OK\",\"n\":9007199254740993} "
	var output bytes.Buffer
	completeCalls, eventCalls := 0, 0
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(structuredStreamFixture(format, arguments)), StreamOptions{Unbounded: true, OnComplete: func(public StreamResult) error {
		completeCalls++
		if output.Len() != 0 || eventCalls != 0 {
			t.Error("unvalidated content escaped before public completion authorization")
		}
		if bytes.Contains(public.Response, []byte(format.ToolName)) || len(public.Output) != 2 || !bytes.Contains(public.Output[1], []byte(`"type":"message"`)) {
			t.Error("OnComplete received internal function history")
		}
		return nil
	}, OnEvent: func(event []byte) error {
		eventCalls++
		if bytes.Contains(event, []byte(format.ToolName)) {
			t.Error("OnEvent observed internal tool")
		}
		return nil
	}}, format)
	if err != nil || !result.Completed || completeCalls != 1 || result.ResponseID != "resp_real" {
		t.Fatalf("structured projection failed: completed=%v err=%v", result.Completed, err)
	}
	for _, private := range []string{format.ToolName, "fc_internal", "call_internal", "response.function_call_arguments"} {
		if strings.Contains(output.String(), private) {
			t.Fatalf("private tool detail escaped: %s", private)
		}
	}
	if !bytes.Contains(result.Response, []byte("9007199254740993")) || !bytes.Contains(result.Response, []byte(`"cache_write_tokens":0`)) || !bytes.Contains(result.Response, []byte("unchanged-upstream-cipher")) || !bytes.Contains(result.Response, []byte("customer_tool")) {
		t.Fatal("real usage, cache-write zero, customer tool or reasoning state lost")
	}
	events := streamTestEvents(t, output.String())
	var delta strings.Builder
	for index, event := range events {
		var sequence int
		_ = json.Unmarshal(event["sequence_number"], &sequence)
		if sequence != index {
			t.Fatal("projected sequence numbers are not continuous")
		}
		if streamString(event, "type") == "response.output_text.delta" {
			delta.WriteString(streamString(event, "delta"))
		}
	}
	if delta.String() != arguments || eventCalls != len(events) || result.Events != int64(len(events)) {
		t.Fatal("public text bytes/event observations changed")
	}
	first := streamString(mustStreamObject(t, result.Output[1]), "id")
	var second bytes.Buffer
	replayed, err := StreamStructured(context.Background(), &second, strings.NewReader(structuredStreamFixture(format, arguments)), StreamOptions{Unbounded: true}, format)
	if err != nil || streamString(mustStreamObject(t, replayed.Output[1]), "id") != first || !strings.HasPrefix(first, "msg_") {
		t.Fatal("derived public message identity was unstable")
	}
}

func mustStreamObject(t *testing.T, value json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	object, err := streamObject(value)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestStreamStructuredRejectsInvalidOrInconsistentWithoutOutput(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, arguments := range []string{`{"value":7}`, `{"value":"OK","extra":true}`, `{"value":"OK"} {}`, `not JSON`, `[]`} {
		var output bytes.Buffer
		completed := false
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(structuredStreamFixture(format, arguments)), StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { completed = true; return nil }}, format)
		if err == nil || result.Completed || output.Len() != 0 || completed {
			t.Fatal("invalid structured final result escaped validation")
		}
	}
	good := structuredStreamFixture(format, `{"value":"OK"}`)
	for _, change := range []struct{ old, new string }{
		{`"delta":"{\"value"`, `"delta":"{\"wrong"`},
		{`"arguments":"{\"value\":\"OK\"}"`, `"arguments":"{\"value\":\"NO\"}"`},
	} {
		wire := strings.Replace(good, change.old, change.new, 1)
		if wire == good {
			t.Fatal("test mutation did not change stream")
		}
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{Unbounded: true}, format)
		if err == nil || result.Completed || output.Len() != 0 {
			t.Fatal("inconsistent arguments were delivered")
		}
	}
}

func TestStreamStructuredPublicToolsNotSwallowedAndMixedRejected(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, name := range []string{format.ToolName, format.ToolName + "_customer"} {
		customer := raw(map[string]any{"type": "function_call", "id": "fc_customer", "call_id": "call_customer", "name": name, "namespace": "customer", "arguments": "{}", "status": "completed"})
		wire := structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": customer}) + structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_customer", "status": "completed", "output": []any{customer}}})
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		if err != nil || !result.Completed || !bytes.Contains(result.Output[0], []byte("fc_customer")) || !strings.Contains(output.String(), name) {
			t.Fatal("namespaced or prefix-sharing customer tool was swallowed")
		}
		for _, items := range [][]any{
			{structuredStreamCall(format, "fc_one", "call_one", `{"value":"OK"}`), customer},
			{structuredStreamCall(format, "fc_one", "call_one", `{"value":"OK"}`), structuredStreamCall(format, "fc_two", "call_two", `{"value":"OK"}`)},
		} {
			output.Reset()
			wire = structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_mixed", "status": "completed", "output": items}})
			result, err = StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
			if err == nil || result.Completed || output.Len() != 0 {
				t.Fatal("mixed internal/customer or multiple internal calls accepted")
			}
		}
	}
}

func TestStreamStructuredRefusalAndRealFailureRemainReal(t *testing.T) {
	format := structuredStreamFormat(t)
	refusal := json.RawMessage(`{"type":"message","id":"msg_refusal","role":"assistant","content":[{"type":"refusal","refusal":"declined"}]}`)
	var output bytes.Buffer
	wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_refusal", "status": "completed", "output": []any{refusal}, "usage": map[string]any{"input_tokens": 7}}})
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	if err != nil || !result.Completed || !strings.Contains(output.String(), "declined") {
		t.Fatal("genuine refusal was rejected or fabricated")
	}
	internal := structuredStreamCall(format, "fc_private", "call_private", `{"value":"partial"}`)
	wire = structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": internal}) + structuredStreamFrame(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_failed", "status": "failed", "output": []any{internal, refusal}, "tools": []any{format.ToolDefinition()}, "usage": map[string]any{"input_tokens": 99, "input_tokens_details": map[string]any{"cached_tokens": 0, "cache_write_tokens": 80}}, "error": map[string]any{"code": "rate_limit_exceeded", "message": "Limit for " + format.ToolName}}})
	output.Reset()
	completes := 0
	result, err = StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{OnComplete: func(StreamResult) error { completes++; return nil }}, format)
	var terminal *TerminalError
	if !errors.As(err, &terminal) || terminal.Code != "rate_limit_exceeded" || result.Completed || completes != 0 || !bytes.Contains(result.Usage, []byte(`"cache_write_tokens":80`)) {
		t.Fatal("genuine failed terminal/usage changed")
	}
	for _, private := range []string{format.ToolName, "fc_private", "call_private", "partial"} {
		if strings.Contains(output.String(), private) || bytes.Contains(terminal.Payload, []byte(private)) {
			t.Fatal("internal failed call leaked")
		}
	}
	if !strings.Contains(output.String(), "rate_limit_exceeded") || !strings.Contains(output.String(), "declined") {
		t.Fatal("public refusal or real failure was discarded")
	}
}

func TestStreamStructuredOnCompleteCancellationAndIOFailClosed(t *testing.T) {
	format := structuredStreamFormat(t)
	wire := structuredStreamFixture(format, `{"value":"OK"}`)
	sentinel := errors.New("synthetic authorization failure")
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { return sentinel }}, format)
	if !errors.Is(err, sentinel) || result.Completed || output.Len() != 0 {
		t.Fatal("failed completion authorization delivered data")
	}
	result, err = StreamStructured(context.Background(), streamTestErrorWriter{sentinel}, strings.NewReader(wire), StreamOptions{Unbounded: true}, format)
	if !errors.Is(err, sentinel) || result.Completed {
		t.Fatal("write failure returned success")
	}
	partial := streamTestFrame(`{"type":"response.created","response":{"id":"r","output":[],"status":"in_progress"}}`)
	result, err = StreamStructured(context.Background(), &output, io.MultiReader(strings.NewReader(partial), streamTestErrorReader{sentinel}), StreamOptions{}, format)
	if !errors.Is(err, sentinel) || result.Completed || output.Len() != 0 {
		t.Fatal("read failure returned success or leaked buffered events")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { _, err := StreamStructured(ctx, io.Discard, reader, StreamOptions{}, format); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation changed")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation failed to stop blocked reader")
	}
}

type streamTestErrorWriter struct{ err error }

func (w streamTestErrorWriter) Write([]byte) (int, error) { return 0, w.err }

type streamTestErrorReader struct{ err error }

func (r streamTestErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamStructuredNilFormatIsOriginalStream(t *testing.T) {
	wire := ": original comment\n\n" + streamTestFrame(streamTestCompleted)
	var want, actual bytes.Buffer
	before, err := Stream(context.Background(), &want, strings.NewReader(wire), StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := StreamStructured(context.Background(), &actual, strings.NewReader(wire), StreamOptions{}, nil)
	if err != nil || !bytes.Equal(want.Bytes(), actual.Bytes()) || before.Events != after.Events || before.Completed != after.Completed {
		t.Fatal("nil-format path changed normal incremental stream")
	}
}

func TestStreamStructuredFinalTextValidatedWithoutInternalTool(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, text := range []string{`{"value":"OK"}`, `{"value":7}`} {
		message := raw(map[string]any{"type": "message", "id": "msg_plain", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
		wire := structuredStreamFrame(map[string]any{"type": "response.output_text.delta", "item_id": "msg_plain", "output_index": 0, "content_index": 0, "delta": text}) + structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": message}) + structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_plain", "status": "completed", "output": []any{message}}})
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		if strings.Contains(text, `"OK"`) {
			if err != nil || !result.Completed || output.Len() == 0 {
				t.Fatal("valid direct structured text failed")
			}
		} else if err == nil || result.Completed || output.Len() != 0 {
			t.Fatal("invalid direct structured text escaped")
		}
	}
}

func TestStreamStructuredTerminalOnlyAndFailedIndices(t *testing.T) {
	format := structuredStreamFormat(t)
	internal := structuredStreamCall(format, "fc_terminal", "call_terminal", `{"value":"OK"}`)
	wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_terminal", "status": "completed", "output": []any{internal}}})
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	if err != nil || !result.Completed {
		t.Fatal("terminal-only internal call was not projected")
	}
	events := streamTestEvents(t, output.String())
	if len(events) != 7 || streamString(events[0], "type") != "response.output_item.added" || streamString(events[6], "type") != "response.completed" {
		t.Fatal("terminal-only call missed the public message lifecycle")
	}
	for _, remove := range []string{"id", "call_id"} {
		item := mustStreamObject(t, internal)
		delete(item, remove)
		wire = structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_bad_identity", "status": "completed", "output": []any{item}}})
		output.Reset()
		result, err = StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		if err == nil || result.Completed || output.Len() != 0 {
			t.Fatal("internal call without identity accepted")
		}
	}
	public := json.RawMessage(`{"type":"message","id":"msg_refusal","role":"assistant","content":[{"type":"refusal","refusal":"cannot continue"}]}`)
	wire = structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": internal}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": public}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": public}) +
		structuredStreamFrame(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_failed_index", "status": "failed", "output": []any{internal, public}, "error": map[string]string{"code": "server_error", "message": "real failure"}}})
	output.Reset()
	result, err = StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	var terminal *TerminalError
	if !errors.As(err, &terminal) || len(result.Output) != 1 {
		t.Fatal("failure projection lost public item")
	}
	for _, event := range streamTestEvents(t, output.String()) {
		if index, ok := structuredEventIndex(event); ok && index != 0 {
			t.Fatal("removing failed internal item left public output index gap")
		}
	}
}

func TestStreamStructuredBuffersUntilValidatedTerminal(t *testing.T) {
	format := structuredStreamFormat(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	visible := make(chan struct{}, 16)
	done := make(chan error, 1)
	go func() {
		_, err := StreamStructured(ctx, &output, reader, StreamOptions{OnEvent: func([]byte) error { visible <- struct{}{}; return nil }}, format)
		done <- err
	}()
	first := structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_buffered", "call_id": "call_buffered", "name": format.ToolName, "arguments": "", "status": "in_progress"}})
	if _, err := io.WriteString(writer, first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-visible:
		t.Fatal("buffered internal event appeared before terminal")
	default:
	}
	terminal := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_buffered", "status": "completed", "output": []any{structuredStreamCall(format, "fc_buffered", "call_buffered", `{"value":"OK"}`)}}})
	if _, err := io.WriteString(writer, terminal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("validated terminal did not finish")
	}
	if output.Len() == 0 {
		t.Fatal("validated buffered turn was not delivered")
	}
}

func TestStreamStructuredPendingNativeSearchMixRejectedAndReflectionsHidden(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, kind := range []string{"web_search_call", "tool_search_call", "code_interpreter_call"} {
		publicCall := map[string]any{"type": kind, "id": "public_search", "execution": "server", "status": "in_progress"}
		wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_mixed_search", "status": "completed", "output": []any{structuredStreamCall(format, "fc_internal", "call_internal", `{"value":"OK"}`), publicCall}}})
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		if err == nil || result.Completed || output.Len() != 0 {
			t.Fatal("internal format and pending native public call mixed in one turn")
		}
	}
	message := json.RawMessage(`{"type":"message","id":"msg_native","role":"assistant","content":[{"type":"output_text","text":"{\"value\":\"OK\"}"}]}`)
	search := json.RawMessage(`{"type":"web_search_call","id":"ws_public","status":"completed"}`)
	wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_native", "status": "completed", "output": []any{search, message}, "instructions": "Final output uses " + format.ToolName, "input": []any{map[string]any{"type": "additional_tools", "tools": []any{format.ToolDefinition(), map[string]string{"type": "function", "name": "customer_tool"}}}, map[string]any{"type": "message", "role": "developer", "content": []any{map[string]string{"type": "input_text", "text": "Use " + format.ToolName}}}}}})
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	if err != nil || !result.Completed || bytes.Contains(result.Response, []byte(format.ToolName)) || strings.Contains(output.String(), format.ToolName) {
		t.Fatal("native search plus valid JSON failed or reflected internal declaration")
	}
	if !bytes.Contains(result.Response, []byte("customer_tool")) || !bytes.Contains(result.Response, []byte("ws_public")) {
		t.Fatal("reflection scrub removed public declarations or native search")
	}
}

func TestStreamStructuredResponseTextEchoMatchesRequestedFormat(t *testing.T) {
	for _, configuration := range []string{
		`{"verbosity":"low","format":{"type":"json_object"}}`,
		`{"verbosity":"high","format":{"type":"json_schema","name":"echo","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993}},"required":["n"],"additionalProperties":false}}}`,
	} {
		format, err := ParseStructuredFormat(json.RawMessage(configuration))
		if err != nil {
			t.Fatal(err)
		}
		internal := structuredStreamCall(format, "fc_echo", "call_echo", `{"n":9007199254740993}`)
		for _, upstreamHasText := range []bool{true, false} {
			created := map[string]any{"id": "resp_echo", "status": "in_progress", "output": []any{}}
			completed := map[string]any{"id": "resp_echo", "status": "completed", "output": []any{internal}}
			if upstreamHasText {
				created["text"] = map[string]any{"format": map[string]string{"type": "text"}}
				completed["text"] = map[string]any{"format": map[string]string{"type": "text"}}
			}
			wire := structuredStreamFrame(map[string]any{"type": "response.created", "response": created}) + structuredStreamFrame(map[string]any{"type": "response.completed", "response": completed})
			var output bytes.Buffer
			result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
			if err != nil || !result.Completed {
				t.Fatal("valid format echo fixture failed")
			}
			responses := []map[string]json.RawMessage{mustStreamObject(t, result.Response)}
			for _, event := range streamTestEvents(t, output.String()) {
				if event["response"] != nil {
					responses = append(responses, mustStreamObject(t, event["response"]))
				}
			}
			for _, response := range responses {
				text, exists := response["text"]
				if exists != upstreamHasText {
					t.Fatal("projection invented or omitted response text metadata")
				}
				if exists && !equalJSON(text, json.RawMessage(configuration)) {
					t.Fatal("response text format differs from caller schema/verbosity")
				}
				if strings.Contains(configuration, "9007199254740993") && exists && !bytes.Contains(text, []byte("9007199254740993")) {
					t.Fatal("format echo rounded raw schema integer")
				}
				if response["model"] != nil || response["usage"] != nil {
					t.Fatal("projection invented absent model/usage")
				}
			}
		}
	}
}

func TestStreamStructuredRejectsGhostAndContradictoryPublicText(t *testing.T) {
	format := structuredStreamFormat(t)
	message := json.RawMessage(`{"type":"message","id":"msg_final","role":"assistant","content":[{"type":"output_text","text":"{\"value\":\"OK\"}"}]}`)
	refusal := json.RawMessage(`{"type":"message","id":"msg_refusal","role":"assistant","content":[{"type":"refusal","refusal":"declined"}]}`)
	for _, tc := range []struct {
		label, id string
		index     int
		item      json.RawMessage
		delta     string
	}{
		{"wrong_index", "msg_final", 8, message, `{"value":"OK"}`},
		{"ghost_before_refusal", "msg_ghost", 7, refusal, "UNVALIDATED PLAIN TEXT"},
		{"wrong_part_type", "msg_refusal", 0, refusal, "UNVALIDATED PLAIN TEXT"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			wire := structuredStreamFrame(map[string]any{"type": "response.output_text.delta", "item_id": tc.id, "output_index": tc.index, "content_index": 0, "delta": tc.delta}) +
				structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_ghost", "status": "completed", "output": []any{tc.item}}})
			var output bytes.Buffer
			result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
			if err == nil || result.Completed || output.Len() != 0 {
				t.Fatal("public text without matching final identity/content escaped")
			}
		})
	}
}

func TestStreamStructuredEachFinalMessageIsValidJSON(t *testing.T) {
	format := structuredStreamFormat(t)
	message := func(id, text, phase string) json.RawMessage {
		return raw(map[string]any{"type": "message", "id": id, "role": "assistant", "phase": phase, "content": []any{map[string]string{"type": "output_text", "text": text}}})
	}
	call := json.RawMessage(`{"type":"function_call","id":"fc_public","call_id":"call_public","name":"customer","arguments":"{}"}`)
	for _, tc := range []struct {
		name  string
		items []any
		valid bool
	}{
		{"split_json", []any{message("msg_a", `{"value":`, "final_answer"), message("msg_b", `"OK"}`, "final_answer")}, false},
		{"invalid_final_with_tool", []any{message("msg_a", "not json", "final"), call}, false},
		{"invalid_final_answer_with_tool", []any{message("msg_a", "not json", "final_answer"), call}, false},
		{"commentary_with_tool", []any{message("msg_a", "Checking the file now.", "commentary"), call}, true},
		{"valid_messages", []any{message("msg_a", `{"value":"OK"}`, "final_answer"), message("msg_b", `{"value":"ALSO_OK"}`, "final_answer")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_per_message", "status": "completed", "output": tc.items}})
			var output bytes.Buffer
			result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
			if tc.valid {
				if err != nil || !result.Completed {
					t.Fatalf("valid message/tool sequence rejected: %v", err)
				}
			} else if err == nil || result.Completed || output.Len() != 0 {
				t.Fatal("a non-JSON final message was delivered")
			}
		})
	}
}

func TestStreamStructuredLateInternalIdentityNeverLeaksOnFailure(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, kind := range []string{"response.failed", "response.cancelled", "response.incomplete"} {
		wire := structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]string{"type": "function_call", "id": "fc_late", "call_id": "call_late", "arguments": ""}}) +
			structuredStreamFrame(map[string]any{"type": "response.function_call_arguments.done", "item_id": "fc_late", "output_index": 0, "name": format.ToolName, "arguments": "{}"}) +
			structuredStreamFrame(map[string]any{"type": kind, "response": map[string]any{"id": "resp_late", "status": strings.TrimPrefix(kind, "response."), "model": "actual-model", "usage": map[string]int{"input_tokens": 17, "output_tokens": 2}}})
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		var terminal *TerminalError
		if !errors.As(err, &terminal) || result.Completed || !bytes.Contains(result.Usage, []byte("17")) {
			t.Fatal("real failed terminal or usage lost")
		}
		for _, private := range []string{"fc_late", "call_late", format.ToolName, "function_call"} {
			if strings.Contains(output.String(), private) {
				t.Fatal("late-identified internal stub escaped on failure")
			}
		}
	}
}

func TestStreamStructuredUpstreamTerminalObservedBeforeWriteFailure(t *testing.T) {
	format := structuredStreamFormat(t)
	created := structuredStreamFrame(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_upstream", "status": "in_progress", "output": []any{}}})
	failed := structuredStreamFrame(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_upstream", "status": "failed", "output": []any{}, "error": map[string]string{"code": "rate_limit_exceeded", "message": "real rate limit"}, "usage": map[string]int{"input_tokens": 37}}})
	observed, public := 0, 0
	options := StreamOptions{OnUpstreamTerminal: func(event []byte) error {
		observed++
		if !bytes.Contains(event, []byte("rate_limit_exceeded")) || !bytes.Contains(event, []byte("37")) {
			t.Error("upstream terminal changed")
		}
		return nil
	}, OnEvent: func([]byte) error { public++; return nil }}
	result, err := StreamStructured(context.Background(), streamTestErrorWriter{io.ErrClosedPipe}, strings.NewReader(created+failed), options, format)
	if !errors.Is(err, io.ErrClosedPipe) || result.Completed || observed != 1 || public != 1 {
		t.Fatal("already-read real terminal was not observed independently of delivery")
	}
	observed, public = 0, 0
	result, err = Stream(context.Background(), streamTestErrorWriter{io.ErrClosedPipe}, strings.NewReader(failed), options)
	if !errors.Is(err, io.ErrClosedPipe) || result.Completed || observed != 1 {
		t.Fatal("ordinary stream terminal hook did not run exactly once before writing")
	}
}

type structuredCountingContext struct {
	context.Context
	checks atomic.Int32
	limit  int32
}

func (c *structuredCountingContext) Err() error {
	if c.checks.Add(1) >= c.limit {
		return context.Canceled
	}
	return nil
}

func TestStreamStructuredProjectionChecksCancellationInsideLoop(t *testing.T) {
	format := structuredStreamFormat(t)
	message := json.RawMessage(`{"type":"message","id":"msg_text","role":"assistant","content":[{"type":"output_text","text":"{\"value\":\"OK\"}"}]}`)
	result := StreamResult{Completed: true, Output: []json.RawMessage{message}, Response: raw(map[string]any{"id": "resp_cpu", "status": "completed", "output": []any{message}})}
	events := make([]json.RawMessage, 10000)
	for index := range events {
		events[index] = json.RawMessage(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_text","delta":""}`)
	}
	ctx := &structuredCountingContext{Context: context.Background(), limit: 80}
	_, projected, err := projectStructuredTurnContext(ctx, result, events, format, true)
	if !errors.Is(err, context.Canceled) || len(projected) != 0 || ctx.checks.Load() > 100 {
		t.Fatal("CPU projection did not stop at periodic context checks")
	}
}

func TestStreamStructuredErrorNestedArraysHideInternalName(t *testing.T) {
	format := structuredStreamFormat(t)
	detail := map[string]any{"code": "upstream_error", "message": "real failure", "details": []any{map[string]any{"tool_name": format.ToolName, "number": json.RawMessage(`9007199254740993`)}, []any{format.ToolName, map[string]string{"nested": format.ToolName}}}}
	wire := structuredStreamFrame(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_array", "status": "failed", "output": []any{}, "error": detail, "usage": map[string]int{"input_tokens": 17}}})
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || terminal.Code != "upstream_error" {
		t.Fatal("real failure identity was lost")
	}
	for _, raw := range [][]byte{output.Bytes(), result.Response, terminal.Payload} {
		if bytes.Contains(raw, []byte(format.ToolName)) {
			t.Fatal("nested error array exposed internal tool identity")
		}
		if !bytes.Contains(raw, []byte("9007199254740993")) || !bytes.Contains(raw, []byte("real failure")) {
			t.Fatal("error array sanitization changed real diagnostic values")
		}
	}
}

func TestStreamStructuredUnknownInternalEventFailsInsteadOfDisappearing(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, references := range []map[string]any{{"item_id": "fc_internal"}, {"output_index": 0}, {"item": structuredStreamCall(format, "fc_internal", "call_internal", `{"value":"OK"}`)}} {
		unknown := map[string]any{"type": "response.future_semantics", "client_meaning": "must remain meaningful"}
		for key, value := range references {
			unknown[key] = value
		}
		wire := structuredStreamFrame(unknown) + structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_unknown", "status": "completed", "output": []any{structuredStreamCall(format, "fc_internal", "call_internal", `{"value":"OK"}`)}}})
		var output bytes.Buffer
		result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
		if !errors.Is(err, ErrInvalidEvent) || result.Completed || output.Len() != 0 {
			t.Fatal("unknown event targeting internal item was silently dropped")
		}
	}
	message := json.RawMessage(`{"type":"message","id":"msg_public","role":"assistant","content":[{"type":"output_text","text":"{\"value\":\"OK\"}"}]}`)
	wire := structuredStreamFrame(map[string]any{"type": "response.future_semantics", "item_id": "msg_public", "output_index": 0, "public_extension": "preserve"}) +
		structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_public_unknown", "status": "completed", "output": []any{message}}})
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{}, format)
	if err != nil || !result.Completed || !strings.Contains(output.String(), "public_extension") {
		t.Fatal("non-internal unknown public event was discarded")
	}
}

func TestStreamStructuredValidationErrorSeparatesUndeliveredValidationFromIO(t *testing.T) {
	format := structuredStreamFormat(t)
	valid := structuredStreamFixture(format, `{"value":"OK"}`)
	sentinel := errors.New("synthetic commit or I/O boundary")
	for _, tc := range []struct {
		name, wire     string
		options        StreamOptions
		writer         io.Writer
		wantValidation bool
		want           error
	}{
		{"schema", structuredStreamFixture(format, `{"value":7}`), StreamOptions{Unbounded: true}, io.Discard, true, nil},
		{"commit", valid, StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { return sentinel }}, io.Discard, true, sentinel},
		{"commit_canceled", valid, StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { return context.Canceled }}, io.Discard, false, context.Canceled},
		{"commit_deadline", valid, StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { return context.DeadlineExceeded }}, io.Discard, false, context.DeadlineExceeded},
		{"writer", valid, StreamOptions{Unbounded: true}, streamTestErrorWriter{sentinel}, false, sentinel},
		{"flush", valid, StreamOptions{Unbounded: true, Flush: func() error { return sentinel }}, io.Discard, false, sentinel},
		{"public_observer", valid, StreamOptions{Unbounded: true, OnEvent: func([]byte) error { return sentinel }}, io.Discard, false, sentinel},
		{"upstream_observer", valid, StreamOptions{Unbounded: true, OnUpstreamTerminal: func([]byte) error { return sentinel }}, io.Discard, false, sentinel},
		{"incomplete_stream", "", StreamOptions{Unbounded: true}, io.Discard, false, ErrIncompleteStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := StreamStructured(context.Background(), tc.writer, strings.NewReader(tc.wire), tc.options, format)
			var validation *StructuredValidationError
			if err == nil || result.Completed || errors.As(err, &validation) != tc.wantValidation {
				t.Fatalf("validation classification mismatch: %T completed=%v", err, result.Completed)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatal("wrapped error did not preserve original identity")
			}
			if tc.wantValidation && validation.Unwrap() == nil {
				t.Fatal("validation wrapper lost its cause")
			}
		})
	}
	reader := io.MultiReader(strings.NewReader(streamTestFrame(`{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}`)), streamTestErrorReader{sentinel})
	_, err := StreamStructured(context.Background(), io.Discard, reader, StreamOptions{}, format)
	var validation *StructuredValidationError
	if !errors.Is(err, sentinel) || errors.As(err, &validation) {
		t.Fatal("upstream read error was mistaken for validation")
	}
}
