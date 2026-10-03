package server

import (
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

func outputFixtureEvent(index int64, item string) string {
	return fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
}

func TestCollectedResponseReconstructsOpaqueOutputAndUsage(t *testing.T) {
	items := []string{
		`{"id":"rs_1","type":"reasoning","encrypted_content":"opaque+/=","summary":[{"type":"summary_text","text":"private summary"}],"unknown":{"keep":1234567890123456789}}`,
		`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"echo check\"}","namespace":"functions"}`,
		`{"id":"msg_1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"MARKER_OK","annotations":[]}]}`,
	}
	for _, output := range []string{"", `,"output":null`, `,"output":[]`} {
		t.Run(output, func(t *testing.T) {
			wire := ""
			for i, item := range items {
				wire += outputFixtureEvent(int64(i), item)
			}
			wire += `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1328,"output_tokens":11,"input_tokens_details":{"cached_tokens":1024}},"metadata":{"preserve":true}` + output + "}}\n\n"
			result, err := collectResponse(iotest.OneByteReader(strings.NewReader(wire)))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				ID, Status string
				Output     []json.RawMessage
				Metadata   json.RawMessage
			}
			if err := json.Unmarshal(result, &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != "resp_1" || response.Status != "completed" || len(response.Output) != 3 || string(response.Metadata) != `{"preserve":true}` {
				t.Fatalf("terminal metadata changed: %s", result)
			}
			for i, item := range items {
				assertJSONEqual(t, response.Output[i], []byte(item))
			}
			if input, output, cached, ok := usageFromResponse(result); !ok || input != 1328 || output != 11 || cached != 1024 {
				t.Fatal("real usage was lost during output reconstruction")
			}
		})
	}
}

func TestCollectedResponseTerminalOutputIsAuthoritative(t *testing.T) {
	terminal := `{ "id": "resp_final", "status": "completed", "output": [{"type":"message","content":[{"type":"output_text","text":"authoritative"}]}] }`
	wire := outputFixtureEvent(0, `{"type":"message","content":[{"type":"output_text","text":"older"}]}`) + "data: {\"type\":\"response.completed\",\"response\":" + terminal + "}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil || string(result) != terminal {
		t.Fatalf("nonempty terminal output was modified or duplicated: %s %v", result, err)
	}
}

func TestCollectedResponseSortsSparseIndicesAndReplacesDuplicates(t *testing.T) {
	wire := outputFixtureEvent(9223372036854775807, `{"type":"message","id":"last"}`) +
		outputFixtureEvent(2, `{"type":"message","id":"middle","content":"old"}`) +
		outputFixtureEvent(0, `{"type":"reasoning","id":"first"}`) +
		outputFixtureEvent(2, `{"type":"message","id":"middle"}`) +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_sparse\",\"status\":\"completed\"}}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Output []struct{ ID string } }
	_ = json.Unmarshal(result, &response)
	if len(response.Output) != 3 || response.Output[0].ID != "first" || response.Output[1].ID != "middle" || response.Output[2].ID != "last" {
		t.Fatalf("sparse output order or deduplication: %s", result)
	}
}

func TestCollectedResponseMissingIndicesKeepArrivalOrder(t *testing.T) {
	wire := ""
	for _, id := range []string{"first", "second", "first"} {
		wire += `data: {"type":"response.output_item.done","item":{"type":"message","id":"` + id + `"}}` + "\n\n"
	}
	wire += "data: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_legacy\"}}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(result), `"first"`) > strings.Index(string(result), `"second"`) {
		t.Fatalf("legacy event order changed: %s", result)
	}
	var response struct{ Output []json.RawMessage }
	_ = json.Unmarshal(result, &response)
	if len(response.Output) != 2 {
		t.Fatalf("repeated unindexed item ID duplicated: %s", result)
	}
}

func TestCollectedResponseRejectsMixedIndexCollisionWithoutLosingItems(t *testing.T) {
	for _, first := range []string{
		`{"id":"item_a","type":"reasoning","encrypted_content":"preserve-a"}`,
		`{"type":"reasoning","encrypted_content":"anonymous-preserve"}`,
	} {
		unindexed := `{"type":"response.output_item.done","item":` + first + `}`
		indexed := `{"type":"response.output_item.done","output_index":0,"item":{"id":"item_b","type":"message","content":[]}}`
		wire := "data: " + unindexed + "\n\ndata: " + indexed + "\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mixed\"}}\n\n"
		if result, err := collectResponse(strings.NewReader(wire)); err == nil || result != nil || !strings.Contains(err.Error(), "conflicts with an unindexed output item") {
			t.Fatalf("mixed collision silently accepted: result=%s err=%v", result, err)
		}
		var a responseOutputAccumulator
		if err := a.observe([]byte(unindexed)); err != nil {
			t.Fatal(err)
		}
		beforeBytes := a.bytes
		if err := a.observe([]byte(indexed)); err == nil {
			t.Fatal("expected mixed index conflict")
		}
		if len(a.items) != 1 || string(a.items[0]) != first || a.bytes != beforeBytes {
			t.Fatal("rejected event mutated previously collected output")
		}
	}
}

func TestCollectedResponseAllowsSameItemToGainExplicitIndex(t *testing.T) {
	wire := `data: {"type":"response.output_item.done","item":{"id":"item_a","type":"message","content":[]}}` + "\n\n" +
		outputFixtureEvent(0, `{"id":"item_a","type":"message","content":[{"type":"output_text","text":"updated"}]}`) +
		`data: {"type":"response.output_item.done","item":{"id":"item_b","type":"reasoning","encrypted_content":"keep"}}` + "\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_mixed\"}}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Output []json.RawMessage }
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 2 || !strings.Contains(string(response.Output[0]), "updated") || !strings.Contains(string(response.Output[1]), "keep") {
		t.Fatalf("safe mixed updates changed items: %s", result)
	}
}

func TestCollectedResponseRejectsMalformedOutputEvents(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.output_item.done","output_index":-1,"item":{}}`,
		`{"type":"response.output_item.done","output_index":1.5,"item":{}}`,
		`{"type":"response.output_item.done","output_index":null,"item":{}}`,
		`{"type":"response.output_item.done","output_index":0,"item":null}`,
	} {
		if _, err := collectResponse(strings.NewReader("data: " + event + "\n\n")); err == nil {
			t.Fatalf("invalid output event accepted: %s", event)
		}
	}
}

func TestCollectedResponseBoundsAggregateOutputAndItemCount(t *testing.T) {
	t.Run("aggregate bytes", func(t *testing.T) {
		// Each individual SSE event fits; their accumulated output does not.
		item := `{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", maxBody/2) + `"}`
		wire := io.MultiReader(strings.NewReader(outputFixtureEvent(0, item)), strings.NewReader(outputFixtureEvent(1, item)))
		if result, err := collectResponse(wire); err == nil || result != nil || !strings.Contains(err.Error(), "32 MiB") {
			t.Fatalf("aggregate output limit not enforced: err=%v", err)
		}
	})
	t.Run("tiny items", func(t *testing.T) {
		var a responseOutputAccumulator
		for i := 0; i < maxCollectedResponseItems; i++ {
			payload := fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{}}`, i)
			if err := a.observe([]byte(payload)); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.observe([]byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{}}`, maxCollectedResponseItems))); err == nil {
			t.Fatal("tiny items bypassed count bound")
		}
	})
	t.Run("metadata plus output", func(t *testing.T) {
		var a responseOutputAccumulator
		if err := a.observe([]byte(`{"type":"response.output_item.done","item":{"type":"message","content":"` + strings.Repeat("x", maxBody/2) + `"}}`)); err != nil {
			t.Fatal(err)
		}
		terminal := json.RawMessage(`{"id":"resp_large","metadata":"` + strings.Repeat("x", maxBody/2) + `"}`)
		if _, err := a.complete(terminal); err == nil {
			t.Fatal("merged metadata bypassed final response size bound")
		}
	})
}

func TestCollectedResponsePartialOutputKeepsFailureAndIncomplete(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			terminal := `{"id":"resp_partial","status":"` + status + `","error":{"code":"server_error","message":"upstream stopped"},"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":19,"output_tokens":4,"input_tokens_details":{"cached_tokens":12}}}`
			wire := outputFixtureEvent(0, `{"type":"message","content":[{"type":"output_text","text":"partial"}]}`) + `data: {"type":"response.` + status + `","response":` + terminal + "}\n\n"
			result, err := collectResponse(strings.NewReader(wire))
			if status == "failed" {
				var failure *upstreamResponseError
				if !errors.As(err, &failure) || result != nil {
					t.Fatalf("failure became success: %s %v", result, err)
				}
				result = failure.Response
				if !strings.Contains(string(failure.Payload), "server_error") {
					t.Fatal("failure diagnostic lost")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(result), "partial") || !strings.Contains(string(result), `"status":"`+status+`"`) {
				t.Fatalf("partial output/status lost: %s", result)
			}
			if input, output, cached, ok := usageFromResponse(result); !ok || input != 19 || output != 4 || cached != 12 {
				t.Fatal("failure/incomplete usage lost")
			}
		})
	}
}

func TestCollectedResponseStopsAtTerminalMetadata(t *testing.T) {
	first := outputFixtureEvent(0, `{"type":"message","id":"first"}`) + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_first\"}}\n\n"
	result, err := collectResponse(io.MultiReader(strings.NewReader(first), errorReader{}))
	if err != nil || !strings.Contains(string(result), `"first"`) {
		t.Fatalf("collector read beyond terminal: %s %v", result, err)
	}
}

func TestGatewayNonStreamingCollectsCodexOutputItems(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputFixtureEvent(0, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"GATEWAY_JSON_OK"}]}`))
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_metadata\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1328,\"output_tokens\":11,\"input_tokens_details\":{\"cached_tokens\":1024}}}}\n\n")
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"test","stream":false,"input":"hello"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "GATEWAY_JSON_OK") {
		t.Fatalf("JSON gateway lost Codex output: status=%d body=%s", w.Code, w.Body.String())
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.Status != "ok" || rec.InputTokens != 1328 || rec.CachedTokens != 1024 {
		t.Fatalf("reconstruction changed accounting: %+v", rec)
	}
}
