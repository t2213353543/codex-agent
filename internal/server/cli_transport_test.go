package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// These headers come from codex-rs 0.154.0's responses client, session
// header builder, metadata compatibility headers, and default HTTP client.
func TestCLITransportRequestHeaders(t *testing.T) {
	source := make(http.Header)
	want := map[string]string{
		"User-Agent": "codex_exec/0.154.0 (Windows 10.0.26100; x86_64) unknown",
		"Originator": "codex_exec", "Version": "0.154.0",
		"Session-Id": "process-session", "Thread-Id": "thread-123",
		"X-Client-Request-Id": "thread-123", "X-Request-Id": "request-456",
		"X-Codex-Installation-Id": "installation-123", "X-Codex-Parent-Thread-Id": "parent-123",
		"X-Codex-Turn-State": "opaque-state", "X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Codex-Routing-Hint": "route", "X-Codex-Window-Id": "window-1",
		"X-OpenAI-Subagent": "collab_spawn", "X-OpenAI-Memgen-Request": "true",
		"X-OpenAI-Internal-Codex-Responses-Lite": "true", "X-OpenAI-Internal-Codex-Residency": "us",
		"X-Oai-Attestation": "opaque-attestation", "X-Responsesapi-Include-Timing-Metrics": "true",
		"Traceparent": "00-00000000000000000000000000000011-0000000000000022-01", "Tracestate": "vendor=value",
	}
	for name, value := range want {
		source.Set(name, value)
	}
	blocked := []string{"Authorization", "Proxy-Authorization", "ChatGPT-Account-Id", "Cookie", "Content-Encoding", "X-OpenAI-Admin", "OpenAI-Organization"}
	for _, name := range blocked {
		source.Set(name, "client-secret")
	}
	source.Add("OpenAI-Beta", "responses=experimental, future-feature=v1")
	source.Add("OpenAI-Beta", "responses_websockets=2026-02-06")
	target := make(http.Header)
	copyRequestHeaders(target, source)
	for name, value := range want {
		if got := target.Get(name); got != value {
			t.Errorf("header %s = %q, want %q", name, got, value)
		}
	}
	for _, name := range blocked {
		if target.Get(name) != "" {
			t.Errorf("sensitive or transport header %s leaked upstream", name)
		}
	}
	if got := target.Values("OpenAI-Beta"); !reflect.DeepEqual(got, []string{"future-feature=v1", "responses_websockets=2026-02-06"}) {
		t.Errorf("negotiated beta headers = %v", got)
	}
	target.Set("X-Codex-Turn-State", "changed")
	if source.Get("X-Codex-Turn-State") != "opaque-state" {
		t.Fatal("copy mutated client headers")
	}
}

func TestCLITransportResponseHeaders(t *testing.T) {
	source := make(http.Header)
	want := map[string]string{
		"Content-Type": "text/event-stream", "Retry-After": "30",
		"X-Request-Id": "upstream-request", "X-Oai-Request-Id": "upstream-oai-request",
		"ETag": `"catalog-revision"`, "X-Models-Etag": `"catalog-revision"`,
		"OpenAI-Model": "server-model", "X-OpenAI-Model": "server-model",
		"X-Reasoning-Included": "true", "X-Codex-Turn-State": "next-turn-state",
		"X-Codex-Safety-Buffering-Enabled": "true", "X-Codex-Primary-Used-Percent": "12",
		"X-Other-Meter-Primary-Used-Percent": "50", "X-Other-Meter-Primary-Window-Minutes": "60",
		"X-Other-Meter-Secondary-Reset-At": "1800000000", "X-Other-Meter-Limit-Name": "Other meter",
		"X-OpenAI-Authorization-Error": "expired_token", "X-Error-Json": `{"code":"expired_token"}`,
	}
	for name, value := range want {
		source.Set(name, value)
	}
	blocked := []string{"Set-Cookie", "Authorization", "ChatGPT-Account-Id", "Content-Length", "Content-Encoding", "Connection", "X-Internal-Secret"}
	for _, name := range blocked {
		source.Set(name, "upstream-secret")
	}
	target := make(http.Header)
	copyResponseHeaders(target, source)
	for name, value := range want {
		if got := target.Get(name); got != value {
			t.Errorf("response header %s = %q, want %q", name, got, value)
		}
	}
	for _, name := range blocked {
		if target.Get(name) != "" {
			t.Errorf("sensitive or transport header %s leaked downstream", name)
		}
	}
}

func TestCLITransportPreservesOriginatorOverride(t *testing.T) {
	source := make(http.Header)
	source.Set("User-Agent", "custom_cli/0.154.0 (Windows 10.0.26100; x86_64) unknown")
	source.Set("Originator", "custom_cli")
	source.Set("Version", "0.154.0")
	target := make(http.Header)
	copyRequestHeaders(target, source)
	for _, name := range []string{"User-Agent", "Originator", "Version"} {
		if target.Get(name) != source.Get(name) {
			t.Errorf("native originator override changed %s: got %q want %q", name, target.Get(name), source.Get(name))
		}
	}
}

func TestCLITransportZstdConversation(t *testing.T) {
	requestBody := []byte(`{"model":"test-model","stream":true,"store":false,"instructions":"follow user","input":[{"type":"reasoning","encrypted_content":"opaque-reasoning"},{"type":"function_call_output","call_id":"call-1","output":"done"}],"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"thread-1","text":{"verbosity":"low"}}`)
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[]}}\n\n"
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "zstd" {
			t.Errorf("native request compression intent was lost")
		}
		if r.Header.Get("Thread-Id") != "thread-1" || r.Header.Get("Session-Id") != "session-1" {
			t.Errorf("native session headers lost: %v", r.Header)
		}
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err != nil {
			t.Error(err)
			return
		}
		defer decoder.Close()
		body, err := decoder.DecodeAll(wire, nil)
		if err != nil {
			t.Error("upstream request body is not valid zstd")
			return
		}
		want := strings.Replace(string(requestBody), `"prompt_cache_key":"thread-1"`, `"prompt_cache_key":"`+gatewayFixtureScopedValue("prompt-cache", "thread-1")+`"`, 1)
		assertJSONEqual(t, body, []byte(want))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Models-Etag", `"revision-1"`)
		w.Header().Set("OpenAI-Model", "server-model")
		w.Header().Set("X-Reasoning-Included", "true")
		w.Header().Set("X-Codex-Turn-State", "next-state")
		_, _ = io.WriteString(w, stream)
	})
	// Verify the transport preserves CLI bytes with optional prompt policy off.
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoder.EncodeAll(requestBody, nil)))
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("Content-Encoding", "zstd")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Session-Id", "session-1")
	r.Header.Set("Thread-Id", "thread-1")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != stream {
		t.Fatalf("zstd conversation: status=%d body=%s", w.Code, w.Body.String())
	}
	for name, want := range map[string]string{"X-Models-Etag": `"revision-1"`, "OpenAI-Model": "server-model", "X-Reasoning-Included": "true", "X-Codex-Turn-State": "next-state"} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("CLI response header %s = %q, want %q", name, got, want)
		}
	}
}

func TestCLITransportCompressedBodyLimits(t *testing.T) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	t.Run("decoded size", func(t *testing.T) {
		compressed := encoder.EncodeAll(bytes.Repeat([]byte("x"), maxBody+1), nil)
		r := httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(compressed))
		r.Header.Set("Content-Encoding", "zstd")
		if _, err := readBody(httptest.NewRecorder(), r); err == nil || !strings.Contains(err.Error(), "32 MiB") {
			t.Fatalf("decompressed size limit error = %v", err)
		}
	})
	t.Run("damaged frame", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader("not a zstd frame"))
		r.Header.Set("Content-Encoding", "zstd")
		if _, err := readBody(httptest.NewRecorder(), r); err == nil || err.Error() != "invalid zstd request body" {
			t.Fatalf("damaged zstd error = %v", err)
		}
	})
	for _, encoding := range []string{"br", "gzip", "zstd, gzip"} {
		t.Run(encoding, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"test-model"}`))
			r.Header.Set("Content-Encoding", encoding)
			if _, err := readBody(httptest.NewRecorder(), r); !errors.Is(err, errUnsupportedContentEncoding) {
				t.Fatalf("unsupported encoding error = %v", err)
			}
		})
	}
}

func TestCLITransportCompactNeverStreams(t *testing.T) {
	body, stream, err := prepareBody([]byte(`{"model":"test-model","stream":true,"store":true,"input":[{"type":"reasoning","encrypted_content":"keep"}]}`), true)
	if err != nil || stream {
		t.Fatalf("compact stream=%v err=%v", stream, err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, found := parsed["stream"]; found {
		t.Fatal("compact body retained stream")
	}
	if _, found := parsed["store"]; found {
		t.Fatal("compact body retained store")
	}
	if string(parsed["input"]) != `[{"type":"reasoning","encrypted_content":"keep"}]` {
		t.Fatalf("compact history changed: %s", parsed["input"])
	}
}

func TestCLITransportErrorBodyPreservesBytesPastDiagnosticLimit(t *testing.T) {
	var output bytes.Buffer
	want := "first" + strings.Repeat("x", 100<<10)
	prefix := copyErrorBody(&output, io.MultiReader(strings.NewReader("first"), strings.NewReader(want[5:])))
	if output.String() != want {
		t.Fatalf("error body lost bytes at diagnostic boundary: got %d want %d", output.Len(), len(want))
	}
	if string(prefix) != want[:64<<10] {
		t.Fatalf("diagnostic prefix length = %d", len(prefix))
	}
}
