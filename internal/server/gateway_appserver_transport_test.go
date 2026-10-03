package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type appServerTransportFakeWS struct {
	reads   chan []byte
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	writes  [][]byte
	onWrite func(int, []byte) error
}

func newAppServerTransportFakeWS() *appServerTransportFakeWS {
	return &appServerTransportFakeWS{reads: make(chan []byte, 16), done: make(chan struct{})}
}
func (w *appServerTransportFakeWS) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-w.done:
		return 0, nil, io.EOF
	case raw := <-w.reads:
		return websocket.MessageText, raw, nil
	}
}
func (w *appServerTransportFakeWS) Write(ctx context.Context, typ websocket.MessageType, raw []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.mu.Lock()
	w.writes = append(w.writes, append([]byte(nil), raw...))
	count := len(w.writes)
	w.mu.Unlock()
	if w.onWrite != nil {
		return w.onWrite(count, raw)
	}
	return nil
}
func (w *appServerTransportFakeWS) CloseNow() error    { w.once.Do(func() { close(w.done) }); return nil }
func (w *appServerTransportFakeWS) SetReadLimit(int64) {}
func (w *appServerTransportFakeWS) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.writes...)
}
func appServerTransportTestInput() appServerRequestInput {
	in := appServerRequestFixtureInput()
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(in.ModelMetadataRaw, &metadata)
	metadata["use_responses_lite"] = json.RawMessage("true")
	in.ModelMetadataRaw, _ = json.Marshal(metadata)
	return in
}
func appServerTransportTest(t *testing.T, conn *appServerTransportFakeWS) *appServerTransport {
	t.Helper()
	result := newAppServerTransport(&Proxy{})
	result.dial = func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
		return conn, &http.Response{StatusCode: 101, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	t.Cleanup(func() { _ = result.Close() })
	return result
}
func appServerTransportTestRequest(ctx context.Context) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set("Authorization", "Bearer admitted-synthetic-token")
	req.Header.Set("ChatGPT-Account-Id", "synthetic-account")
	req.Header.Set("User-Agent", "codex_cli_rs/synthetic")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	return req
}
func appServerTransportAssertClosed(t *testing.T, w *appServerTransportFakeWS) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("websocket was not released")
	}
}

func TestAppServerTransportWarmupDeltaAndRawSSE(t *testing.T) {
	conn := newAppServerTransportFakeWS()
	events := []string{
		`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"exact-call","arguments":"{\"n\":1}","name":"client_tool"}}`,
		"{\n \"type\": \"response.completed\", \"response\": {\"id\":\"resp-generated\", \"usage\":{\"input_tokens\":42,\"output_tokens\":9},\"output\":[]}\n}",
	}
	conn.onWrite = func(n int, raw []byte) error {
		if n == 1 {
			conn.reads <- []byte(`{"type":"response.metadata","headers":{"X-Codex-Turn-State":"synthetic-turn-state"}}`)
			conn.reads <- []byte(`{"type":"response.completed","response":{"id":"resp-warm","output":[]}}`)
		} else {
			for _, event := range events {
				conn.reads <- []byte(event)
			}
		}
		return nil
	}
	transport := appServerTransportTest(t, conn)
	var seen http.Header
	transport.dial = func(_ context.Context, headers http.Header, _ *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
		seen = headers.Clone()
		return conn, &http.Response{StatusCode: 101, Header: http.Header{"Sec-Websocket-Accept": {"secret-handshake"}, "X-Request-Id": {"request-synthetic"}}}, nil
	}
	in := appServerTransportTestInput()
	in.ExtraHeaders = http.Header{"Authorization": {"Bearer rejected-writer-credential"}}
	response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	var expected strings.Builder
	for _, event := range events {
		for _, line := range strings.Split(event, "\n") {
			expected.WriteString("data: " + line + "\n")
		}
		expected.WriteByte('\n')
	}
	if string(body) != expected.String() {
		t.Fatalf("events changed: %s", body)
	}
	if seen.Get("Authorization") != "Bearer admitted-synthetic-token" || seen.Get("ChatGPT-Account-Id") != "synthetic-account" {
		t.Fatal("admitted credentials changed")
	}
	if seen.Get("Content-Type") != "" || seen.Get("Content-Encoding") != "" || seen.Get("User-Agent") != "codex_cli_rs/synthetic" {
		t.Fatal("invalid handshake headers")
	}
	if seen.Get("x-codex-routing-hint") != "model=fixture-model" {
		t.Fatal("routing hint missing")
	}
	if response.Header.Get("Sec-WebSocket-Accept") != "" || response.Header.Get("x-codex-turn-state") != "synthetic-turn-state" {
		t.Fatal("response header projection")
	}
	writes := conn.snapshot()
	if len(writes) != 2 {
		t.Fatalf("writes=%d", len(writes))
	}
	var warm, next map[string]json.RawMessage
	_ = json.Unmarshal(writes[0], &warm)
	_ = json.Unmarshal(writes[1], &next)
	var prefix, delta []json.RawMessage
	_ = json.Unmarshal(warm["input"], &prefix)
	_ = json.Unmarshal(next["input"], &delta)
	if string(warm["generate"]) != "false" || len(prefix) != 2 || len(delta) != 1 || string(next["previous_response_id"]) != `"resp-warm"` {
		t.Fatalf("warm/delta mismatch: %s %s", writes[0], writes[1])
	}
	var metadata map[string]string
	_ = json.Unmarshal(next["client_metadata"], &metadata)
	if metadata["x-codex-turn-state"] != "synthetic-turn-state" {
		t.Fatal("turn state not sent")
	}
	appServerTransportAssertClosed(t, conn)
}

func TestAppServerTransportHandshakeStatusesAndFallbackMarker(t *testing.T) {
	for _, status := range []int{401, 403, 426, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			transport := newAppServerTransport(&Proxy{})
			calls := 0
			transport.dial = func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
				calls++
				return nil, &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("exact rejection"))}, errNativeWSHandshake
			}
			response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, appServerTransportTestInput())
			if response == nil || response.StatusCode != status || calls != 1 {
				t.Fatal("handshake status not preserved")
			}
			if (status == 403 || status == 426) != errors.Is(err, errAppServerWSHandshakeUnavailable) {
				t.Fatalf("fallback marker: %v", err)
			}
			if status != 403 && status != 426 && err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if string(raw) != "exact rejection" {
				t.Fatal("rejection body changed")
			}
		})
	}
}

func TestAppServerTransportWarmupFailureNeverGenerates(t *testing.T) {
	conn := newAppServerTransportFakeWS()
	failure := []byte(`{"type":"error","status":429,"error":{"message":"synthetic-private-error","code":"rate_limit"}}`)
	conn.onWrite = func(int, []byte) error { conn.reads <- failure; return nil }
	transport := appServerTransportTest(t, conn)
	response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, appServerTransportTestInput())
	var stage *appServerTransportError
	var upstream *appServerTransportUpstreamError
	if response != nil || !errors.As(err, &stage) || stage.GenerationSent || !errors.As(err, &upstream) || !bytes.Equal(upstream.Event, failure) {
		t.Fatalf("lost warmup failure: %v", err)
	}
	if len(conn.snapshot()) != 1 || strings.Contains(err.Error(), "synthetic-private-error") {
		t.Fatal("failure leaked or generated")
	}
	appServerTransportAssertClosed(t, conn)
}

func TestAppServerTransportGenerationFailureIsForwardedNoReplay(t *testing.T) {
	for _, writeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "upstream-error", true: "ambiguous-write"}[writeFailure], func(t *testing.T) {
			conn := newAppServerTransportFakeWS()
			failure := []byte(`{"type":"error","status":500,"error":{"message":"exact generation error"}}`)
			conn.onWrite = func(n int, raw []byte) error {
				if n == 1 {
					conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[]}}`)
					return nil
				}
				if writeFailure {
					return io.ErrClosedPipe
				}
				conn.reads <- failure
				return nil
			}
			transport := appServerTransportTest(t, conn)
			response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, appServerTransportTestInput())
			if response != nil {
				var raw []byte
				raw, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
				if !bytes.Contains(raw, failure) {
					t.Fatal("generation error was swallowed")
				}
			}
			var stage *appServerTransportError
			if writeFailure && (!errors.As(err, &stage) || !stage.GenerationSent) || !writeFailure && err != nil || len(conn.snapshot()) != 2 {
				t.Fatalf("unsafe generation state: %v", err)
			}
			appServerTransportAssertClosed(t, conn)
		})
	}
}

func TestAppServerTransportCancellationAndSlowReaderRelease(t *testing.T) {
	for _, action := range []string{"cancel-prewarm", "cancel-generation", "body-close", "transport-close"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			conn := newAppServerTransportFakeWS()
			warmSent := make(chan struct{})
			conn.onWrite = func(n int, raw []byte) error {
				if n == 1 {
					close(warmSent)
					if action != "cancel-prewarm" {
						conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[]}}`)
					}
				} else {
					conn.reads <- []byte(`{"type":"response.output_text.delta","delta":"slow client"}`)
				}
				return nil
			}
			transport := appServerTransportTest(t, conn)
			if action == "cancel-prewarm" {
				done := make(chan error, 1)
				go func() {
					_, err := transport.RoundTripPrepared(appServerTransportTestRequest(ctx), nil, appServerTransportTestInput())
					done <- err
				}()
				<-warmSent
				cancel()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("cancel ignored")
					}
				case <-time.After(time.Second):
					t.Fatal("warmup stuck")
				}
			} else {
				response, err := transport.RoundTripPrepared(appServerTransportTestRequest(ctx), nil, appServerTransportTestInput())
				if err != nil {
					t.Fatal(err)
				}
				switch action {
				case "cancel-generation":
					cancel()
				case "body-close":
					_ = response.Body.Close()
				case "transport-close":
					_ = transport.Close()
				}
				_ = response.Body.Close()
			}
			appServerTransportAssertClosed(t, conn)
		})
	}
}

func TestAppServerTransportPrefixRulesAndChangedFullRequest(t *testing.T) {
	before := []byte(`{"model":"m","reasoning":{"effort":"high"},"input":[{"type":"message","id":"a","content":"same","internal_chat_message_metadata_passthrough":{"turn_id":"old"}}]}`)
	after := []byte(`{"model":"m","reasoning":{"effort":"high"},"client_metadata":{"turn_id":"new"},"input":[{"content":"same","id":"a","type":"message","internal_chat_message_metadata_passthrough":{"turn_id":"new"}},{"type":"message","content":"next"}]}`)
	delta, ok := appServerInputDelta(before, after, nil)
	if !ok || len(delta) != 1 {
		t.Fatal("metadata-only change blocked valid prefix")
	}
	if _, ok = appServerInputDelta(before, bytes.Replace(after, []byte(`"high"`), []byte(`"low"`), 1), nil); ok {
		t.Fatal("reasoning change reused previous response")
	}
	output, _ := appServerItemHash(json.RawMessage(`{"type":"message","content":"different"}`))
	if _, ok = appServerInputDelta(before, after, [][32]byte{output}); ok {
		t.Fatal("mismatched output accepted")
	}
	conn := newAppServerTransportFakeWS()
	conn.onWrite = func(n int, raw []byte) error {
		if n == 1 {
			conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[{"type":"message","role":"assistant","content":[]} ]}}`)
		} else {
			conn.reads <- []byte(`{"type":"response.completed","response":{"id":"generated","output":[]}}`)
		}
		return nil
	}
	transport := appServerTransportTest(t, conn)
	response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, appServerTransportTestInput())
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var next map[string]json.RawMessage
	_ = json.Unmarshal(conn.snapshot()[1], &next)
	if _, exists := next["previous_response_id"]; exists {
		t.Fatal("unmatched warm output reused previous id")
	}
	var items []json.RawMessage
	_ = json.Unmarshal(next["input"], &items)
	if len(items) != 3 {
		t.Fatalf("full request input=%d", len(items))
	}
}

func TestAppServerTransportExplicitToolChoiceWarmupRemainsAuto(t *testing.T) {
	conn := newAppServerTransportFakeWS()
	conn.onWrite = func(n int, raw []byte) error {
		if n == 1 {
			conn.reads <- []byte(`{"type":"response.completed","response":{"id":"warm","output":[]}}`)
		} else {
			conn.reads <- []byte(`{"type":"response.completed","response":{"id":"generated","output":[]}}`)
		}
		return nil
	}
	transport := appServerTransportTest(t, conn)
	in := appServerTransportTestInput()
	in.ClientControls = map[string]json.RawMessage{"tool_choice": json.RawMessage(`"required"`)}
	response, err := transport.RoundTripPrepared(appServerTransportTestRequest(t.Context()), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var warm, generation map[string]json.RawMessage
	writes := conn.snapshot()
	_ = json.Unmarshal(writes[0], &warm)
	_ = json.Unmarshal(writes[1], &generation)
	if string(warm["tool_choice"]) != `"auto"` || string(generation["tool_choice"]) != `"required"` {
		t.Fatal("warmup or caller tool choice changed")
	}
	if _, exists := generation["previous_response_id"]; exists {
		t.Fatal("changed tool choice incorrectly reused warmup")
	}
	if string(in.ClientControls["tool_choice"]) != `"required"` {
		t.Fatal("caller controls mutated")
	}
}
