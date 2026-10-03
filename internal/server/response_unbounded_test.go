package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

func TestLightweightUnboundedResponseLargeOpaqueOutput(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		count, size  int
		slimTerminal bool
	}{
		{name: "single 33 MiB event with slim terminal restoration", count: 1, size: 33 << 20, slimTerminal: true},
		{name: "two individually valid items beyond aggregate limit", count: 2, size: 17 << 20},
		{name: "more than 8192 distinct completed items", count: 8193, size: 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// UTF-8 and an opaque encrypted value must survive without truncation
			// or lossy string conversion when a terminal omits completed items.
			value := strings.Repeat("糖", scenario.size/3)
			var parts []io.Reader
			for i := 0; i < scenario.count; i++ {
				parts = append(parts,
					strings.NewReader(fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":{\"id\":\"rs_%d\",\"type\":\"reasoning\",\"encrypted_content\":\"", i, i)),
					strings.NewReader(value), strings.NewReader("\",\"unknown\":9007199254740993}}\r\n\r\n"))
			}
			output := "[]"
			if scenario.slimTerminal {
				output = `[{"id":"rs_0","type":"reasoning"}]`
			}
			parts = append(parts, strings.NewReader(`data: {"type":"response.completed","response":{"id":"resp_large","status":"completed","model":"gpt-6-astra","output":`+output+`,"usage":{"input_tokens":19,"output_tokens":7,"input_tokens_details":{"cached_tokens":11}}}}`+"\r\n\r\n"))
			record := &RequestRecord{unboundedPayload: true}
			result, err := collectResponseObserved(io.MultiReader(parts...), nil, record)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Model  string `json:"model"`
				Output []struct {
					ID        string      `json:"id"`
					Encrypted string      `json:"encrypted_content"`
					Unknown   json.Number `json:"unknown"`
				} `json:"output"`
			}
			if err := json.Unmarshal(result, &response); err != nil {
				t.Fatal(err)
			}
			if response.Model != "gpt-6-astra" || len(response.Output) != scenario.count || !record.httpCompleted {
				t.Fatalf("response metadata or item count changed: model=%s count=%d completed=%v", response.Model, len(response.Output), record.httpCompleted)
			}
			want := sha256.Sum256([]byte(value))
			for i, item := range response.Output {
				if item.ID != fmt.Sprintf("rs_%d", i) || item.Unknown.String() != "9007199254740993" || sha256.Sum256([]byte(item.Encrypted)) != want {
					t.Fatalf("output %d changed", i)
				}
			}
			if input, output, cached, ok := usageFromResponse(result); !ok || input != 19 || output != 7 || cached != 11 {
				t.Fatal("large output lost terminal token accounting")
			}
		})
	}
}

func TestLightweightUnboundedMappedStreamLargeToolAndUsage(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"function","name":"inspect","parameters":{"type":"object"}}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Repeat("x", 33<<20)
	source := io.MultiReader(
		strings.NewReader("\xef\xbb\xbfid: event_large\r\nevent: response.output_item.done\r\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_large\",\"call_id\":\"call_large\",\"namespace\":\"functions\",\"name\":\"inspect\",\"arguments\":\""),
		strings.NewReader(arguments),
		strings.NewReader("\"}}\r\n\r\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_large\",\"status\":\"completed\",\"usage\":{\"input_tokens\":19,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":11}}}}\r\n\r\n"))
	stream := newAppServerMappedStream(io.NopCloser(source), adapter, true)
	defer stream.Close()
	record := &RequestRecord{unboundedPayload: true}
	observer := &sseScanner{rec: record}
	result, err := collectResponseObserved(io.TeeReader(stream, observer), nil, record)
	observer.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0].Namespace != "" || response.Output[0].Name != "inspect" || sha256.Sum256([]byte(response.Output[0].Arguments)) != sha256.Sum256([]byte(arguments)) {
		t.Fatal("large tool call was truncated or its client identity was not restored")
	}
	if record.Status != "ok" || record.InputTokens != 19 || record.OutputTokens != 7 || record.CachedTokens != 11 || !record.httpCompleted {
		t.Fatalf("large event disabled terminal accounting: status=%s input=%d output=%d cached=%d completed=%v", record.Status, record.InputTokens, record.OutputTokens, record.CachedTokens, record.httpCompleted)
	}
}

func TestLightweightUnboundedSSEFragmentedFramingAndFailure(t *testing.T) {
	for _, ending := range []string{"\r", "\n", "\r\n"} {
		wire := "\xef\xbb\xbf: 心跳" + ending + ending + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_utf8\",\"output\":[{\"type\":\"message\",\"content\":\"糖\"}]}}" + ending + ending
		result, err := collectResponseObserved(iotest.OneByteReader(strings.NewReader(wire)), nil, &RequestRecord{unboundedPayload: true})
		if err != nil || !bytes.Contains(result, []byte("糖")) {
			t.Fatalf("fragmented %q framing failed: %v", ending, err)
		}
	}
	for _, wire := range []string{
		"data: \xff\n\n",
		"data: {\"type\":\"response.output_item.done\",\"output_index\":-1,\"item\":{}}\n\n",
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"one\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"two\"}}\n\n",
	} {
		if _, err := collectResponseObserved(strings.NewReader(wire), nil, &RequestRecord{unboundedPayload: true}); err == nil {
			t.Fatal("unbounded mode accepted malformed UTF-8, an invalid index or conflicting response IDs")
		}
	}
	failed := `data: {"type":"response.failed","response":{"id":"failed","status":"failed","error":{"message":"stopped"}}}` + "\n\n"
	_, err := collectResponseObserved(strings.NewReader(failed), nil, &RequestRecord{unboundedPayload: true})
	var failure *upstreamResponseError
	if !errors.As(err, &failure) {
		t.Fatalf("failed response framing became a success: %v", err)
	}
}

type unboundedCancelReader struct {
	started chan struct{}
	closed  chan struct{}
	start   sync.Once
	close   sync.Once
}

func (r *unboundedCancelReader) Read([]byte) (int, error) {
	r.start.Do(func() { close(r.started) })
	<-r.closed
	return 0, context.Canceled
}

func (r *unboundedCancelReader) Close() error {
	r.close.Do(func() { close(r.closed) })
	return nil
}

func TestLightweightUnboundedMappedStreamCancellation(t *testing.T) {
	upstream := &unboundedCancelReader{started: make(chan struct{}), closed: make(chan struct{})}
	stream := newAppServerMappedStream(upstream, appServerToolAdapter{}, true)
	finished := make(chan error, 1)
	go func() {
		_, err := collectResponseObserved(stream, nil, &RequestRecord{unboundedPayload: true})
		finished <- err
	}()
	select {
	case <-upstream.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never began reading")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled stream reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing downstream did not release the upstream read and collector")
	}
}
