package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

const auditSSETerminal = `{"type":"response.completed","response":{"id":"resp_audit","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`

func TestAuditSSEInitialBOMAndLineBoundaries(t *testing.T) {
	for _, ending := range []string{"\n", "\r", "\r\n"} {
		for _, prefix := range []string{"", ": comment" + ending, ending} {
			wire := "\ufeff" + prefix + "data: " + auditSSETerminal + ending + ending
			result, err := collectResponse(iotest.OneByteReader(strings.NewReader(wire)))
			if err != nil || !strings.Contains(string(result), "resp_audit") {
				t.Fatalf("BOM/ending %q prefix %q: result=%s err=%v", ending, prefix, result, err)
			}
			d := cacheDiagnosticsForTest(t)
			id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
			o := d.ResponseObserver(id)
			for i := range len(wire) {
				_, _ = o.Write([]byte{wire[i]})
			}
			if err := o.Finish(); err != nil || d.Snapshot().SuccessfulResponses != 1 {
				t.Fatalf("cache observer BOM/ending %q: %v %+v", ending, err, d.Snapshot())
			}
		}
	}
	// Only the first BOM is framing; a later one is part of an unknown field.
	wire := ": first\n\n\ufeffdata: {\"type\":\"error\",\"message\":\"must be ignored\"}\n\ndata: " + auditSSETerminal + "\n\n"
	if _, err := collectResponse(strings.NewReader(wire)); err != nil {
		t.Fatalf("non-initial BOM was incorrectly stripped: %v", err)
	}
}

func TestAuditSSERejectsInvalidUTF8WithoutChangingDiagnosticTransport(t *testing.T) {
	for _, prefix := range []string{
		": invalid \xff comment\n\n",
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"bad\xff\"}}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"bad\xff\"}}\n\n",
	} {
		wire := prefix + "data: " + auditSSETerminal + "\n\n"
		if result, err := collectResponse(iotest.OneByteReader(strings.NewReader(wire))); result != nil || err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("invalid UTF-8 became valid JSON: result=%s err=%v", result, err)
		}
		d := cacheDiagnosticsForTest(t)
		id := observeCacheRequestForTest(t, d, "s", "t", `{}`)
		o := d.ResponseObserver(id)
		if n, err := o.Write([]byte(wire)); n != len(wire) || err != nil {
			t.Fatalf("diagnostic interrupted forwarding: %d %v", n, err)
		}
		if err := o.Finish(); err == nil || d.Snapshot().Responses != 0 || d.Snapshot().PendingRequests != 0 {
			t.Fatalf("invalid UTF-8 counted as success: %v %+v", err, d.Snapshot())
		}
	}
}

func TestAuditSSEMalformedJSONAndCompatibleTerminalFields(t *testing.T) {
	prefix := "data: {invalid JSON}\n\ndata: [DONE]\n\ndata: {\"type\":123}\n\ndata\n\n"
	for _, response := range []string{`{}`, `{"id":"r"}`, `{"id":"r","status":null}`, `{"id":"r","status":7}`, `{"id":"r","status":"vendor_status"}`} {
		wire := prefix + `data: {"type":"response.completed","response":` + response + "}\n\n"
		result, err := collectResponse(strings.NewReader(wire))
		if err != nil || string(result) != response {
			t.Fatalf("compatible metadata changed: result=%s err=%v", result, err)
		}
	}
}

func TestAuditOutputIndexCollisionPreservesDistinctTools(t *testing.T) {
	var a responseOutputAccumulator
	first := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_first","type":"function_call","call_id":"call_first","name":"shell","arguments":"{}"}}`)
	second := bytes.ReplaceAll(first, []byte("first"), []byte("second"))
	if err := a.observe(first); err != nil {
		t.Fatal(err)
	}
	beforeBytes := a.bytes
	if err := a.observe(second); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("distinct tools silently replaced one another: %v", err)
	}
	if len(a.items) != 1 || a.bytes != beforeBytes || !bytes.Contains(a.items[0], []byte("fc_first")) {
		t.Fatal("rejected collision mutated the earlier completed tool")
	}
	updated := bytes.ReplaceAll(first, []byte(`"arguments":"{}"`), []byte(`"arguments":"{\"x\":1}"`))
	if err := a.observe(updated); err != nil || !bytes.Contains(a.items[0], []byte(`\"x\"`)) {
		t.Fatalf("same ID update rejected: %v", err)
	}
}

func TestAuditOutputResponseIDsDoNotMix(t *testing.T) {
	for _, mismatch := range []string{
		`{"type":"response.created","response":{"id":"resp_other"}}`,
		`{"type":"response.output_item.done","response_id":"resp_other","item":{"id":"item_other"}}`,
		`{"type":"response.completed","response":{"id":"resp_other","output":[]}}`,
	} {
		wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\"}}\n\n" + outputFixtureEvent(0, `{"id":"item_first","type":"reasoning","encrypted_content":"private-first"}`) + "data: " + mismatch + "\n\n"
		if result, err := collectResponse(strings.NewReader(wire)); err == nil || result != nil || !strings.Contains(err.Error(), "conflicting response IDs") {
			t.Fatalf("response IDs mixed: %s %v", result, err)
		}
	}
}

func TestAuditOutputToolCallIdentityWithoutItemID(t *testing.T) {
	for _, toolType := range []string{"function_call", "custom_tool_call", "local_shell_call", "tool_search_call"} {
		first := `{"type":"` + toolType + `","call_id":"call_first","name":"shell","arguments":"{}"}`
		second := strings.ReplaceAll(first, "call_first", "call_second")
		var a responseOutputAccumulator
		for _, item := range []string{first, first, second} {
			if err := a.observe([]byte(`{"type":"response.output_item.done","item":` + item + `}`)); err != nil {
				t.Fatal(err)
			}
		}
		if len(a.items) != 2 {
			t.Fatalf("unindexed %s call duplicated: %d", toolType, len(a.items))
		}
		// Explicitly assigning the same slot to its existing call is safe.
		if err := a.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + first + `}`)); err != nil {
			t.Fatal(err)
		}
		if err := a.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + second + `}`)); err == nil {
			t.Fatalf("distinct %s call ID replaced completed slot", toolType)
		}
		if err := a.observe([]byte(`{"type":"response.output_item.done","output_index":2,"item":` + first + `}`)); err == nil {
			t.Fatalf("same %s call ID occupied two slots", toolType)
		}
		if err := a.observe([]byte(`{"type":"response.output_item.done","item":{"type":"function_call_output","call_id":"call_first","output":"ok"}}`)); err != nil || len(a.items) != 3 {
			t.Fatalf("tool output merged with its invocation: %v items=%d", err, len(a.items))
		}
	}
}

func TestAuditOutputTerminalKnownSubsetRetainsDoneItems(t *testing.T) {
	const reasoning = `{"id":"rs_audit","type":"reasoning","encrypted_content":"opaque+/=","unknown":1234567890123456789}`
	const tool = `{"id":"fc_audit","type":"function_call","call_id":"call_audit","name":"shell","arguments":"{}"}`
	const oldMessage = `{"id":"msg_audit","type":"message","content":[{"type":"output_text","text":"old"}]}`
	const finalMessage = `{"id":"msg_audit","type":"message","content":[{"type":"output_text","text":"final"}]}`
	for _, status := range []string{"completed", "done", "incomplete", "failed"} {
		wire := outputFixtureEvent(0, reasoning) + outputFixtureEvent(1, tool) + outputFixtureEvent(2, oldMessage)
		wire += "data: {\"type\":\"response.output_item.added\",\"output_index\":3,\"item\":{\"id\":\"not_done\",\"type\":\"message\"}}\n\n"
		wire += `data: {"type":"response.` + status + `","response":{"id":"resp_audit","status":"` + status + `","output":[` + finalMessage + `],"usage":{"input_tokens":11,"output_tokens":2},"error":{"message":"failed-fixture"}}}` + "\n\n"
		result, err := collectResponse(strings.NewReader(wire))
		if status == "failed" {
			var failure *upstreamResponseError
			if !errors.As(err, &failure) {
				t.Fatalf("failed response lost diagnostic: %v", err)
			}
			result = failure.Response
		} else if err != nil {
			t.Fatal(err)
		}
		var response struct{ Output []json.RawMessage }
		if err := json.Unmarshal(result, &response); err != nil || len(response.Output) != 3 {
			t.Fatalf("partial terminal lost output: %s %v", result, err)
		}
		for i, want := range []string{reasoning, tool, finalMessage} {
			assertJSONEqual(t, response.Output[i], []byte(want))
		}
		if in, out, _, ok := usageFromResponse(result); !ok || in != 11 || out != 2 {
			t.Fatal("partial merge lost usage")
		}
	}
}

func TestAuditOutputUnidentifiedTerminalRemainsAuthoritative(t *testing.T) {
	for _, output := range []string{`[{"id":"new_id","type":"message"}]`, `[{"type":"message"}]`, `[{"id":"known"},{"id":"new_id"}]`} {
		terminal := `{ "id": "resp_audit", "output": ` + output + ` }`
		wire := outputFixtureEvent(0, `{"id":"known","type":"reasoning"}`) + "data: {\"type\":\"response.completed\",\"response\":" + terminal + "}\n\n"
		result, err := collectResponse(strings.NewReader(wire))
		if err != nil || string(result) != terminal {
			t.Fatalf("ambiguous terminal was merged: %s %v", result, err)
		}
	}
	var a responseOutputAccumulator
	_ = a.observe([]byte(`{"type":"response.output_item.done","item":{"id":"known"}}`))
	if _, err := a.complete(json.RawMessage(`{"output":[{"id":"known"},{"id":"known"}]}`)); err == nil {
		t.Fatal("duplicate known terminal IDs were accepted")
	}
}

func TestAuditOutputExactFinalLimitAndSubsetLimit(t *testing.T) {
	var a responseOutputAccumulator
	const item = `{"id":"known","type":"message"}`
	if err := a.observe([]byte(`{"type":"response.output_item.done","item":` + item + `}`)); err != nil {
		t.Fatal(err)
	}
	const base = `{"padding":"","output":[]}`
	padding := strings.Repeat("x", maxBody-len(base)-len(item))
	response := json.RawMessage(`{"padding":"` + padding + `","output":[]}`)
	result, err := a.complete(response)
	if err != nil || len(result) != maxBody {
		t.Fatalf("exactly 32 MiB assembly rejected: size=%d err=%v", len(result), err)
	}
	if _, err := a.complete(json.RawMessage(`{"padding":"x` + padding + `","output":[]}`)); err == nil {
		t.Fatal("32 MiB + 1 assembly accepted")
	}
	var subset responseOutputAccumulator
	largeItem := `{"id":"large","type":"reasoning","encrypted_content":"` + strings.Repeat("x", maxBody/2) + `"}`
	if err := subset.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + largeItem + `}`)); err != nil {
		t.Fatal(err)
	}
	_ = subset.observe([]byte(`{"type":"response.output_item.done","output_index":1,"item":` + item + `}`))
	if _, err := subset.complete(json.RawMessage(`{"output":[{"id":"known","padding":"` + strings.Repeat("x", maxBody/2) + `"}]}`)); err == nil {
		t.Fatal("partial terminal merge bypassed the aggregate limit")
	}
}

func TestAuditOutputReconstructionDoesNotInflateHTMLCharacters(t *testing.T) {
	var a responseOutputAccumulator
	item := `{"id":"msg_code","type":"message","content":"` + strings.Repeat("<>&", maxBody/12) + `"}`
	if err := a.observe([]byte(`{"type":"response.output_item.done","item":` + item + `}`)); err != nil {
		t.Fatal(err)
	}
	result, err := a.complete(json.RawMessage(`{"id":"resp_code"}`))
	if err != nil || len(result) > len(item)+128 || bytes.Contains(result, []byte(`\u003c`)) {
		t.Fatalf("JSON reconstruction inflated HTML characters: bytes=%d err=%v", len(result), err)
	}
}

func TestAuditNonStreamingIncompleteWithoutStatus(t *testing.T) {
	const response = `{"id":"resp_incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":2}}`
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.incomplete","response":`+response+"}\n\n")
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false}`))
	if w.Code != http.StatusOK || w.Body.String() != response {
		t.Fatalf("incomplete body was changed: %d %s", w.Code, w.Body.String())
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.Status != "error" || rec.TerminalStatus != "incomplete" || !strings.Contains(rec.Error, "max_output_tokens") || rec.InputTokens != 9 {
		t.Fatalf("incomplete event became successful: %+v", rec)
	}
}

type auditNoProgressBody struct{ reads int }

func (b *auditNoProgressBody) Read([]byte) (int, error) { b.reads++; return 0, nil }
func (b *auditNoProgressBody) Close() error             { return nil }

func TestAuditSSEReadErrorsRetainCauseAndNoProgressStops(t *testing.T) {
	if _, err := collectResponse(iotest.ErrReader(io.ErrUnexpectedEOF)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error cause was discarded: %v", err)
	}
	body := &auditNoProgressBody{}
	w := httptest.NewRecorder()
	err := writeStreamPipe(w, http.NewResponseController(w), &http.Response{Body: body}, nil, false)
	if !errors.Is(err, io.ErrNoProgress) || body.reads != 100 {
		t.Fatalf("empty reads did not stop: reads=%d err=%v", body.reads, err)
	}
}

func FuzzAuditSSEFragmentedOpaqueResponse(f *testing.F) {
	f.Add("你好\r\n世界", uint8(0), true)
	f.Add("opaque+/=", uint8(1), false)
	f.Add("工具🛠", uint8(2), true)
	f.Fuzz(func(t *testing.T, opaque string, endingIndex uint8, bom bool) {
		if len(opaque) > 4096 {
			t.Skip()
		}
		ending := []string{"\n", "\r", "\r\n"}[endingIndex%3]
		response, err := json.Marshal(map[string]any{"id": "fuzz", "output": []map[string]string{{"type": "reasoning", "encrypted_content": opaque}}})
		if err != nil {
			t.Fatal(err)
		}
		wire := fmt.Sprintf("data: {\"type\":\"response.completed\",%sdata: \"response\":%s}%s%s", ending, response, ending, ending)
		if bom {
			wire = "\ufeff" + wire
		}
		result, err := collectResponse(iotest.OneByteReader(strings.NewReader(wire)))
		if err != nil || !bytes.Equal(result, response) {
			t.Fatalf("fragmented opaque output changed: result=%s err=%v", result, err)
		}
	})
}

func FuzzAuditOutputIdentityState(f *testing.F) {
	f.Add([]byte{0, 1, 1, 0, 0, 2, 2, 0})
	f.Add([]byte{1, 0, 2, 1, 1, 0, 2, 1, 1, 0, 3, 1})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 512 {
			t.Skip()
		}
		var a responseOutputAccumulator
		for i := 0; i+3 < len(operations); i += 4 {
			event := map[string]any{"type": "response.output_item.done"}
			item := map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call_%d", operations[i+2]%5), "arguments": "{}"}
			if operations[i+1]%3 != 0 {
				item["id"] = fmt.Sprintf("item_%d", operations[i+1]%5)
			}
			if operations[i]%3 != 0 {
				event["output_index"] = int(operations[i+3]%8) - 1
			}
			event["item"] = item
			payload, _ := json.Marshal(event)
			_ = a.observe(payload)
			total := 0
			for index, raw := range a.items {
				if index < 0 || !jsonObject(raw) {
					t.Fatal("invalid item entered accumulator")
				}
				total += len(raw) + 1
				var identity responseOutputItemIdentity
				_ = json.Unmarshal(raw, &identity)
				if identity.ID != "" && a.byID[identity.ID] != index {
					t.Fatal("item ID map disagrees with collected output")
				}
				if key := identity.callKey(); key != "" && a.byCall[key] != index {
					t.Fatal("tool ID map disagrees with collected output")
				}
			}
			if total != a.bytes {
				t.Fatalf("aggregate size changed after update/rejection: %d != %d", total, a.bytes)
			}
		}
	})
}
