package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppServerConnectionGatewayConfirmsTerminalBeforeReuse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.RuntimeModes = true
			settings, _ := p.Settings.Get()
			settings.InferenceMode = inferenceCodexCLI
			settings.BuiltinPromptEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			settings.Upstream401RecoveryEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Error("unexpected HTTP generation")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"models":[` + appServerCLITestModel + `]}`)), Request: r}, nil
			})}
			p.AppServerCLI = newAppServerCLI(p)
			t.Cleanup(p.AppServerCLI.Close)
			fixture := newAppServerConnectionFixture(t)
			fixture.pool.owner = &Proxy{NativeHTTP: &gatewayNativePool{}}
			fixture.respond = func(socket *appServerTransportFakeWS, n int, _ map[string]json.RawMessage) {
				socket.reads <- []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d","status":"in_progress","output":[]}}`, n))
				socket.reads <- []byte(fmt.Sprintf(`{"type":"response.output_item.done","response_id":"resp_%d","output_index":0,"item":{"type":"message","id":"msg_%d","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`, n, n))
				socket.reads <- []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[],"usage":{"input_tokens":8,"output_tokens":1,"total_tokens":9}}}`, n))
			}
			p.AppServerCLI.transport.connections.close()
			p.AppServerCLI.transport.connections = fixture.pool
			server := httptest.NewServer(p)
			defer server.Close()
			history := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`)}
			for i := 0; i < 2; i++ {
				raw, _ := json.Marshal(map[string]any{"model": "fixture", "instructions": "test", "input": history, "stream": stream})
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer test-client-key")
				req.Header.Set("Session-Id", "gateway-connection")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var output []byte
				if stream {
					output, err = collectResponse(res.Body)
				} else {
					output, err = io.ReadAll(res.Body)
				}
				_ = res.Body.Close()
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("request %d: %d %v %s", i, res.StatusCode, err, output)
				}
				var body struct{ Output []json.RawMessage }
				if json.Unmarshal(output, &body) != nil || len(body.Output) != 1 {
					t.Fatalf("missing output: %s", output)
				}
				history = append(history, body.Output...)
				history = append(history, json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`))
			}
			if fixture.count() != 1 {
				t.Fatalf("handler did not return completed connection: sockets=%d", fixture.count())
			}
			writes := fixture.sockets[0].snapshot()
			if len(writes) != 3 {
				t.Fatalf("expected warmup + 2 generations, got %d", len(writes))
			}
			var last struct {
				Previous string `json:"previous_response_id"`
				Input    []json.RawMessage
			}
			_ = json.Unmarshal(writes[2], &last)
			if last.Previous != "resp_1" || len(last.Input) != 1 {
				t.Fatalf("handler replay did not produce verified delta: %s", writes[2])
			}
		})
	}
}
